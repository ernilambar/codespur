import { test, expect, describe, beforeAll, afterAll } from "bun:test";
import {
  isNoise, budgetDiff, parseSeverity, parseNumstatBinaries,
  rankOf, sevColor, SEVERITIES, MAX_DIFF_CHARS,
} from "./coblink.ts";
import { mkdtempSync, rmSync, writeFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";

// ─────────────────────────── unit: noise filter ────────────────────────────
describe("isNoise", () => {
  test("skips lockfiles and asset/binary extensions", () => {
    for (const f of ["bun.lockb", "yarn.lock", "go.sum", "custom.lock",
      "logo.png", "src/icon.svg", "fonts/x.woff2", "a.min.js", "dist/app.min.css",
      "build/out.wasm", "vendor.tar.gz"]) {
      expect(isNoise(f)).toBe(true);
    }
  });
  test("keeps real source files", () => {
    for (const f of ["app.js", "src/utils.ts", "README.md", "Dockerfile",
      "lib/main.py", "server.go", "styles.css"]) {
      expect(isNoise(f)).toBe(false);
    }
  });
});

// ──────────────────────── unit: numstat binary parse ────────────────────────
describe("parseNumstatBinaries", () => {
  test("returns only files marked binary (- -)", () => {
    const out = "2\t1\tapp.js\n-\t-\tassets/blob.dat\n1\t0\tutils.ts\n-\t-\timg.raw\n";
    expect(parseNumstatBinaries(out)).toEqual(new Set(["assets/blob.dat", "img.raw"]));
  });
  test("empty input → empty set", () => {
    expect(parseNumstatBinaries("").size).toBe(0);
  });
});

// ──────────────────────────── unit: severity ────────────────────────────────
describe("parseSeverity", () => {
  test("reads the tag", () => {
    expect(parseSeverity("looks fine\nSEVERITY: high")).toBe("high");
  });
  test("case-insensitive", () => {
    expect(parseSeverity("severity: Medium")).toBe("medium");
  });
  test("takes the last tag when several appear", () => {
    expect(parseSeverity("SEVERITY: low\nmore\nSEVERITY: critical")).toBe("critical");
  });
  test("defaults to none when absent", () => {
    expect(parseSeverity("no tag here")).toBe("none");
  });
});

describe("rankOf / SEVERITIES", () => {
  test("ordered none < low < medium < high < critical", () => {
    expect(rankOf("none")).toBeLessThan(rankOf("low"));
    expect(rankOf("high")).toBeLessThan(rankOf("critical"));
    expect(SEVERITIES).toHaveLength(5);
  });
  test("unknown level → -1", () => {
    expect(rankOf("bogus")).toBe(-1);
  });
  test("sevColor returns a string for every level", () => {
    for (const s of SEVERITIES) expect(typeof sevColor(s)).toBe("string");
  });
});

// ──────────────────────────── unit: budgeting ───────────────────────────────
describe("budgetDiff", () => {
  test("small diffs pass through untouched", () => {
    const d = "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n";
    const r = budgetDiff(d);
    expect(r.truncated).toBe(false);
    expect(r.text).toBe(d);
  });
  test("large multi-hunk diffs truncate on hunk boundaries", () => {
    const header = "diff --git a/big b/big\n--- a/big\n+++ b/big\n";
    let hunks = "";
    const total = 80;
    for (let i = 0; i < total; i++) {
      hunks += `@@ -${i} +${i} @@\n+` + "x".repeat(500) + "\n";
    }
    const big = header + hunks;
    expect(big.length).toBeGreaterThan(MAX_DIFF_CHARS);
    const r = budgetDiff(big);
    expect(r.truncated).toBe(true);
    expect(r.shown).toBeGreaterThan(0);
    expect(r.shown).toBeLessThan(r.total);
    expect(r.total).toBe(total);
    expect(r.text).toContain("diff truncated:");
    expect(r.text.length).toBeLessThanOrEqual(MAX_DIFF_CHARS + 200);
  });
  test("oversized diff with no hunks still truncates", () => {
    const r = budgetDiff("Z".repeat(MAX_DIFF_CHARS + 5000));
    expect(r.truncated).toBe(true);
    expect(r.text).toContain("truncated");
  });
});

// ─────────────────────────── integration: CLI ───────────────────────────────
const CLI = join(import.meta.dir, "coblink.ts");

let server: ReturnType<typeof Bun.serve>;
let baseUrl = "";
let repo = "";

function mockFetch(req: Request): Response | Promise<Response> {
  return (async () => {
    const url = new URL(req.url);
    if (!url.pathname.endsWith("/chat/completions")) return new Response("nf", { status: 404 });
    const body: any = await req.json();
    const file = body.messages?.[1]?.content?.match(/File: (.*)/)?.[1] ?? "unknown";
    const sev = file.includes("app") ? "high" : "none";
    const text = `Review of ${file}: check edges. SEVERITY: ${sev}`;
    const words = text.split(" ");
    const stream = new ReadableStream({
      start(c) {
        const enc = new TextEncoder();
        for (const w of words) {
          c.enqueue(enc.encode(`data: ${JSON.stringify({ choices: [{ delta: { content: w + " " } }] })}\n\n`));
        }
        c.enqueue(enc.encode("data: [DONE]\n\n"));
        c.close();
      },
    });
    return new Response(stream, { headers: { "Content-Type": "text/event-stream" } });
  })();
}

function g(args: string[]) {
  const p = Bun.spawnSync(["git", ...args], { cwd: repo });
  if (p.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${p.stderr.toString()}`);
}

async function runCli(args: string[]) {
  const proc = Bun.spawn(["bun", CLI, ...args], {
    cwd: repo,
    env: { ...process.env, COBLINK_BASE_URL: baseUrl, COBLINK_MODEL: "mock" },
    stdout: "pipe",
    stderr: "pipe",
  });
  const out = await new Response(proc.stdout).text();
  const err = await new Response(proc.stderr).text();
  const code = await proc.exited;
  return { out, err, code };
}

beforeAll(() => {
  server = Bun.serve({ port: 0, fetch: mockFetch });
  baseUrl = `http://localhost:${server.port}/v1`;

  repo = mkdtempSync(join(tmpdir(), "coblink-test-"));
  g(["init", "-q", "-b", "main"]);
  g(["config", "user.email", "t@t.com"]);
  g(["config", "user.name", "test"]);
  writeFileSync(join(repo, "app.js"), "function add(a,b){return a+b;}\n");
  g(["add", "-A"]);
  g(["commit", "-qm", "init"]);

  g(["checkout", "-q", "-b", "feature"]);
  writeFileSync(join(repo, "app.js"), "function add(a,b){return a-b;}\n");
  writeFileSync(join(repo, "utils.ts"), "export const x = 1;\n");
  writeFileSync(join(repo, "bun.lockb"), "lockdata\n"); // noise
  g(["add", "-A"]);
  g(["commit", "-qm", "feature"]);
});

afterAll(() => {
  server?.stop(true);
  if (repo) rmSync(repo, { recursive: true, force: true });
});

describe("CLI integration", () => {
  test("--version prints version", async () => {
    const { out, code } = await runCli(["--version"]);
    expect(code).toBe(0);
    expect(out).toMatch(/^coblink \d+\.\d+\.\d+/);
  });

  test("reviews code files, skips noise, exits 0 by default", async () => {
    const { out, code } = await runCli(["-b", "main", "-j", "2"]);
    expect(code).toBe(0);
    expect(out).toContain("app.js");
    expect(out).toContain("utils.ts");
    expect(out).toContain("skipped 1 noise");
    expect(out).toContain("Summary");
    expect(out).toContain("Review complete");
  });

  test("--fail-on high exits 1 when a high-severity file is found", async () => {
    const { out, code } = await runCli(["-b", "main", "--fail-on", "high"]);
    expect(code).toBe(1);
    expect(out.toLowerCase()).toContain("fail-on");
  });

  test("--fail-on low still passes when nothing reaches the threshold isn't the case here → medium", async () => {
    // app.js is high, so medium threshold should also fail (exit 1)
    const { code } = await runCli(["-b", "main", "--fail-on", "medium"]);
    expect(code).toBe(1);
  });

  test("bad --fail-on value exits 2", async () => {
    const { code, err } = await runCli(["--fail-on", "nonsense"]);
    expect(code).toBe(2);
    expect(err).toContain("fail-on");
  });

  test("--staged + --working conflict exits 2", async () => {
    const { code, err } = await runCli(["--staged", "--working"]);
    expect(code).toBe(2);
    expect(err).toContain("only one");
  });

  test("unknown base branch exits 1", async () => {
    const { code, err } = await runCli(["-b", "does-not-exist"]);
    expect(code).toBe(1);
    expect(err).toContain("not found");
  });
});
