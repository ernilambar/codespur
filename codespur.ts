#!/usr/bin/env bun
/**
 * Codespur — AI-powered local PR reviewer.
 *
 * Zero-dependency: uses only native Bun/web APIs (Bun.spawnSync, fetch,
 * util.parseArgs). No node_modules required.
 *
 * Pure helpers are exported for unit testing; the CLI only runs when this file
 * is executed directly (import.meta.main), so importing it in tests is safe.
 */

import { parseArgs } from "util";
import pkg from "./package.json" with { type: "json" };

export const VERSION: string = (pkg as { version?: string }).version ?? "0.0.0";

// Tunables that guard the "no context overload" promise.
export const MAX_DIFF_CHARS = 24_000; // ~6k tokens of diff per file
const DEFAULT_IDLE_SECONDS = 120;
const DEFAULT_JOBS = 3;

export const SEVERITIES = ["none", "low", "medium", "high", "critical"] as const;
export type Severity = (typeof SEVERITIES)[number];
export const rankOf = (s: string) => SEVERITIES.indexOf(s as Severity);

// ── ANSI styling (no dependencies) ──────────────────────────────────────────
const isTTY = process.stdout.isTTY;
const paint = (code: string, s: string) => (isTTY ? `\x1b[${code}m${s}\x1b[0m` : s);
const bold = (s: string) => paint("1", s);
const dim = (s: string) => paint("2", s);
const red = (s: string) => paint("31", s);
const green = (s: string) => paint("32", s);
const yellow = (s: string) => paint("33", s);
const cyan = (s: string) => paint("36", s);
const gray = (s: string) => paint("90", s);

// ── Backend config (env-driven → "backend agnostic") ────────────────────────
// baseUrl / model are validated + assigned inside the CLI entry block below.
const API_KEY = process.env.CODESPUR_API_KEY;
let baseUrl = "";
let model = "";

// ── Runtime config (assigned from CLI args inside the import.meta.main block) ─
let base = "main";
let custom: string | undefined;
let outPath: string | undefined;
let jobs = DEFAULT_JOBS;
let idleMs = DEFAULT_IDLE_SECONDS * 1000;
let stagedFlag = false;
let workingFlag = false;

// ── Mutable run state ─────────────────────────────────────────────────────────
let interrupted = false;
let fatalBackend = false;
const userAbort = new AbortController();

// ── Pure helpers (exported, unit-tested) ─────────────────────────────────────

const SKIP_FILENAMES = new Set([
  "bun.lockb", "package-lock.json", "yarn.lock", "pnpm-lock.yaml",
  "npm-shrinkwrap.json", "cargo.lock", "composer.lock", "gemfile.lock",
  "poetry.lock", "pdm.lock", "flake.lock", "go.sum",
]);
const SKIP_EXTENSIONS = new Set([
  ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".svg", ".tiff",
  ".woff", ".woff2", ".ttf", ".otf", ".eot",
  ".mp3", ".mp4", ".wav", ".mov", ".avi", ".webm", ".flac", ".ogg",
  ".zip", ".tar", ".gz", ".tgz", ".rar", ".7z", ".bz2", ".xz",
  ".exe", ".dll", ".so", ".dylib", ".bin", ".o", ".a", ".wasm",
  ".class", ".jar", ".pyc", ".node",
  ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".map",
  ".snap",
]);

/** True for lockfiles, assets, and known binary/compiled extensions. */
export function isNoise(file: string): boolean {
  const name = file.toLowerCase().split("/").pop() ?? "";
  if (SKIP_FILENAMES.has(name)) return true;
  if (name.endsWith(".lock")) return true;
  if (name.endsWith(".min.js") || name.endsWith(".min.css")) return true;
  if (name.endsWith(".d.ts")) return true;
  if (!name.includes(".")) return false;
  return SKIP_EXTENSIONS.has(name.slice(name.lastIndexOf(".")));
}

/** Parse `git diff --numstat` output; binaries show as `-\t-\t<path>`. */
export function parseNumstatBinaries(numstatOut: string): Set<string> {
  const set = new Set<string>();
  for (const line of numstatOut.split("\n")) {
    const parts = line.split("\t");
    if (parts.length >= 3 && parts[0] === "-" && parts[1] === "-") {
      set.add(parts.slice(2).join("\t").trim());
    }
  }
  return set;
}

/** Split an oversized diff by hunk and truncate it to the context budget. */
export function budgetDiff(diff: string): {
  text: string; truncated: boolean; shown: number; total: number;
} {
  if (diff.length <= MAX_DIFF_CHARS) return { text: diff, truncated: false, shown: 0, total: 0 };

  const lines = diff.split("\n");
  const firstHunk = lines.findIndex((l) => l.startsWith("@@"));
  if (firstHunk === -1) {
    return {
      text: diff.slice(0, MAX_DIFF_CHARS) + "\n… [diff truncated to fit context budget]",
      truncated: true, shown: 0, total: 0,
    };
  }

  const header = lines.slice(0, firstHunk).join("\n");
  const hunks: string[] = [];
  let cur: string[] = [];
  for (let i = firstHunk; i < lines.length; i++) {
    if (lines[i].startsWith("@@") && cur.length) { hunks.push(cur.join("\n")); cur = []; }
    cur.push(lines[i]);
  }
  if (cur.length) hunks.push(cur.join("\n"));

  let out = header;
  let shown = 0;
  for (const h of hunks) {
    if (shown > 0 && out.length + h.length + 1 > MAX_DIFF_CHARS) break;
    out += "\n" + h;
    shown++;
    if (out.length > MAX_DIFF_CHARS) break;
  }
  const truncated = shown < hunks.length;
  if (truncated) {
    out += `\n\n… [diff truncated: ${shown}/${hunks.length} hunks shown to fit context budget]`;
  }
  return { text: out, truncated, shown, total: hunks.length };
}

/** Extract the last `SEVERITY: <level>` tag from model output. */
export function parseSeverity(text: string): Severity {
  const m = [...text.matchAll(/SEVERITY:\s*(none|low|medium|high|critical)/gi)];
  return m.length ? (m[m.length - 1][1].toLowerCase() as Severity) : "none";
}

export function sevColor(s: Severity): string {
  if (s === "critical" || s === "high") return red(s);
  if (s === "medium") return yellow(s);
  if (s === "low") return cyan(s);
  return gray(s);
}

// ── git + diff sources ────────────────────────────────────────────────────────
function git(args: string[]): { ok: boolean; out: string; err: string } {
  const proc = Bun.spawnSync(["git", ...args]);
  return {
    ok: proc.exitCode === 0,
    out: proc.stdout.toString(),
    err: proc.stderr.toString().trim(),
  };
}

function die(msg: string): never {
  process.stderr.write(red("✖ ") + msg + "\n");
  process.exit(1);
}
function dieEarly(msg: string): never {
  process.stderr.write(red("✖ ") + msg + "\n");
  process.exit(2);
}

type Source = {
  label: string;
  needsBase: boolean;
  nameArgs: string[];
  numstatArgs: string[];
  fileArgs: (file: string) => string[];
};

function buildSource(): Source {
  if (stagedFlag && workingFlag) dieEarly("Use only one of --staged / --working.");
  if (stagedFlag) {
    return {
      label: "staged changes", needsBase: false,
      nameArgs: ["--cached", "--name-only"],
      numstatArgs: ["--cached", "--numstat"],
      fileArgs: (f) => ["--cached", "--", f],
    };
  }
  if (workingFlag) {
    return {
      label: "uncommitted changes", needsBase: false,
      nameArgs: ["HEAD", "--name-only"],
      numstatArgs: ["HEAD", "--numstat"],
      fileArgs: (f) => ["HEAD", "--", f],
    };
  }
  const range = `${base}...HEAD`;
  return {
    label: `${base}...HEAD`, needsBase: true,
    nameArgs: ["--name-only", range],
    numstatArgs: ["--numstat", range],
    fileArgs: (f) => [range, "--", f],
  };
}

function binarySet(src: Source): Set<string> {
  const r = git(["diff", ...src.numstatArgs]);
  return r.ok ? parseNumstatBinaries(r.out) : new Set<string>();
}

// ── Task model + streaming display engine ────────────────────────────────────
type Task = {
  index: number;
  file: string;
  diff: string;
  truncated: boolean;
  shown: number;
  total: number;
  buffer: string;
  fullText: string;
  live: boolean;
  done: boolean;
  error: Error | null;
  severity: Severity;
  resolveDone: () => void;
  donePromise: Promise<void>;
};

let allTasks: Task[] = [];

function emit(task: Task, text: string): void {
  if (!text) return;
  task.fullText += text;
  if (task.live) process.stdout.write(text);
  else task.buffer += text;
}

function messagesFor(task: Task) {
  const system =
    "Review a single-file git diff for a pull request.\n\n" +
    "Report only:\n" +
    "- Bugs: incorrect logic, off-by-one, null/undefined risk, race conditions\n" +
    "- Security: injection, auth bypass, secret exposure, unsafe input\n" +
    "- Performance: quadratic loops, N+1, blocking I/O on hot paths\n" +
    "- Readability: only when it obscures correctness\n\n" +
    "Rules:\n" +
    "- Cite specific lines from the diff.\n" +
    "- Do NOT summarize the diff or restate what the code does.\n" +
    "- Do NOT speculate about code outside the diff (callers, other files, framework internals).\n" +
    "- Use short bullets. No praise, no preamble.\n\n" +
    "If nothing is wrong, respond with exactly \"No issues found.\" and stop.\n" +
    "Otherwise, end with exactly one line:\n" +
    "SEVERITY: <low|medium|high|critical>\n\n" +
    "Rubric (used for the Summary column):\n" +
    "- low: nit; no effect on correctness\n" +
    "- medium: real bug or bad pattern, contained blast radius\n" +
    "- high: likely production bug, data-loss risk, or security flaw\n" +
    "- critical: severe security issue, guaranteed data loss, or RCE" +
    (custom
      ? `\n\n<extra_instructions>\n${custom}\n</extra_instructions>\n` +
        "Instructions above override defaults where they conflict."
      : "");
  const user = `File: ${task.file}\n\n\`\`\`diff\n${task.diff}\n\`\`\``;
  return [
    { role: "system", content: system },
    { role: "user", content: user },
  ];
}

async function review(task: Task): Promise<void> {
  if (fatalBackend) throw new Error("backend unreachable (skipped)");

  const reqAbort = new AbortController();
  const onUser = () => reqAbort.abort();
  userAbort.signal.addEventListener("abort", onUser);

  let idledOut = false;
  let idle: ReturnType<typeof setTimeout> | undefined;
  const resetIdle = () => {
    clearTimeout(idle);
    idle = setTimeout(() => { idledOut = true; reqAbort.abort(); }, idleMs);
  };

  try {
    resetIdle();
    let res: Response;
    try {
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (API_KEY) headers.Authorization = `Bearer ${API_KEY}`;
      res = await fetch(`${baseUrl}/chat/completions`, {
        method: "POST",
        headers,
        body: JSON.stringify({ model, stream: true, messages: messagesFor(task) }),
        signal: reqAbort.signal,
      });
    } catch (e) {
      if (idledOut) throw new Error(`no response for ${idleMs / 1000}s (timed out)`);
      if (userAbort.signal.aborted) throw new Error("cancelled");
      fatalBackend = true;
      throw new Error(`could not reach backend at ${baseUrl} (${(e as Error).message})`);
    }

    if (!res.ok) {
      const body = await res.text().catch(() => "");
      throw new Error(`backend returned HTTP ${res.status} ${body.slice(0, 300)}`.trim());
    }

    const ctype = res.headers.get("content-type") ?? "";
    if (!ctype.includes("event-stream")) {
      const bodyText = await res.text();
      let content = "";
      try {
        const j = JSON.parse(bodyText);
        content = j?.choices?.[0]?.message?.content ?? j?.choices?.[0]?.text ?? "";
      } catch { content = bodyText; }
      emit(task, content || dim("(empty response)"));
      return;
    }

    if (!res.body) throw new Error("empty response body");
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      resetIdle();
      buf += decoder.decode(value, { stream: true });
      const lines = buf.split("\n");
      buf = lines.pop() ?? "";
      for (const raw of lines) {
        const line = raw.trim();
        if (!line.startsWith("data:")) continue;
        const payload = line.slice(5).trim();
        if (payload === "[DONE]") return;
        try {
          const json = JSON.parse(payload);
          emit(task, json.choices?.[0]?.delta?.content ?? "");
        } catch { /* partial/keep-alive */ }
      }
    }
  } catch (e) {
    if (idledOut) throw new Error(`no response for ${idleMs / 1000}s (timed out)`);
    if (userAbort.signal.aborted) throw new Error("cancelled");
    throw e as Error;
  } finally {
    clearTimeout(idle);
    userAbort.signal.removeEventListener("abort", onUser);
  }
}

function startPool(tasks: Task[]): void {
  let idx = 0;
  const runner = async () => {
    while (!interrupted) {
      const cur = idx++;
      if (cur >= tasks.length) return;
      const task = tasks[cur];
      try {
        await review(task);
      } catch (e) {
        if (!task.error) task.error = e as Error;
      } finally {
        task.done = true;
        task.resolveDone();
      }
    }
  };
  for (let k = 0; k < Math.min(jobs, tasks.length); k++) void runner();
}

async function main() {
  if (!git(["rev-parse", "--is-inside-work-tree"]).ok) die("Not inside a git repository.");

  const src = buildSource();
  if (src.needsBase && !git(["rev-parse", "--verify", base]).ok) {
    die(`Base branch "${base}" not found. Pass one with -b <branch>.`);
  }

  const listed = git(["diff", ...src.nameArgs]);
  if (!listed.ok) die(`git diff failed: ${listed.err}`);
  const allFiles = listed.out.split("\n").map((s) => s.trim()).filter(Boolean);

  if (allFiles.length === 0) {
    process.stdout.write(yellow(`No changes for ${src.label}. Nothing to review.\n`));
    return;
  }

  const binaries = binarySet(src);
  let skippedNoise = 0;
  let skippedBinary = 0;

  const tasks: Task[] = [];
  for (const file of allFiles) {
    if (binaries.has(file)) { skippedBinary++; continue; }
    if (isNoise(file)) { skippedNoise++; continue; }

    const raw = git(["diff", ...src.fileArgs(file)]).out;
    if (raw.includes("Binary files ") || raw.includes("GIT binary patch")) {
      skippedBinary++;
      continue;
    }
    const diffBody = raw.trim();
    if (!diffBody) continue;

    const { text, truncated, shown, total } = budgetDiff(diffBody);
    const task: Task = {
      index: tasks.length + 1,
      file, diff: text, truncated, shown, total,
      buffer: "", fullText: "", live: false, done: false,
      error: null, severity: "none",
      resolveDone: () => {}, donePromise: Promise.resolve(),
    };
    task.donePromise = new Promise<void>((res) => { task.resolveDone = res; });
    tasks.push(task);
  }
  allTasks = tasks;

  const skipParts: string[] = [];
  if (skippedNoise) skipParts.push(`${skippedNoise} noise`);
  if (skippedBinary) skipParts.push(`${skippedBinary} binary`);
  process.stdout.write(
    "\n" + bold(cyan("◆ Codespur")) + dim(` v${VERSION} — local PR review`) + "\n" +
    gray(`  source ${src.label}`) + "\n" +
    gray(`  model  ${model}`) + "\n" +
    gray(`  engine ${baseUrl}`) + "\n" +
    gray(`  jobs   ${jobs}`) + "\n" +
    gray(`  files  ${tasks.length} to review` +
      (skipParts.length ? `, skipped ${skipParts.join(", ")}` : "")) + "\n"
  );

  if (tasks.length === 0) {
    process.stdout.write(yellow("\nNo reviewable code files after filtering.\n"));
    return;
  }

  startPool(tasks);

  for (const t of tasks) {
    if (interrupted) break;

    const flags = t.truncated ? yellow(` (diff truncated ${t.shown}/${t.total} hunks)`) : "";
    process.stdout.write(
      "\n" + bold(`─ [${t.index}/${tasks.length}] `) + bold(green(t.file)) + flags + "\n" +
      gray("─".repeat(Math.min(60, t.file.length + 14))) + "\n"
    );

    if (t.buffer) { process.stdout.write(t.buffer); t.buffer = ""; }
    t.live = true;

    await t.donePromise;
    t.live = false;
    if (t.buffer) { process.stdout.write(t.buffer); t.buffer = ""; }

    if (t.error) {
      process.stdout.write(red(`✖ ${t.error.message}`) + "\n");
    } else {
      t.severity = parseSeverity(t.fullText);
      process.stdout.write("\n");
    }
  }

  await finish(tasks, src);
}

async function finish(tasks: Task[], src: Source) {
  const anyError = tasks.some((t) => t.error);

  process.stdout.write("\n" + bold("Summary") + "\n");
  const width = Math.min(48, Math.max(...tasks.map((t) => t.file.length), 4));
  for (const t of tasks) {
    const label = t.file.padEnd(width);
    const val = t.error ? red("error") : sevColor(t.severity);
    process.stdout.write(gray("  " + label) + "  " + val + "\n");
  }

  if (outPath) {
    const lines = [
      `# Codespur review`, ``,
      `- Source: \`${src.label}\``,
      `- Model: \`${model}\``,
      `- Generated: ${new Date().toISOString()}`, ``, `---`, ``,
    ];
    for (const t of tasks) {
      lines.push(`## ${t.file}`, ``);
      const meta = t.truncated ? `  _(diff truncated ${t.shown}/${t.total} hunks)_` : "";
      lines.push(`**Severity:** ${t.error ? "error" : t.severity}${meta}`, ``);
      lines.push(t.error ? `> ${t.error.message}` : (t.fullText.trim() || "_(no output)_"), ``);
    }
    try {
      await Bun.write(outPath, lines.join("\n"));
      process.stdout.write("\n" + gray(`report written to ${outPath}`) + "\n");
    } catch (e) {
      process.stderr.write(red(`✖ could not write report: ${(e as Error).message}`) + "\n");
    }
  }

  if (interrupted) {
    process.stdout.write("\n" + yellow("✖ Interrupted.") + "\n\n");
    process.exit(130);
  }
  if (anyError) {
    process.stdout.write("\n" + yellow("✔ Done, with request errors.") + "\n\n");
    process.exit(1);
  }
  process.stdout.write("\n" + bold(green("✔ Review complete.")) + "\n\n");
}

// ── CLI entrypoint (only when executed directly) ──────────────────────────────
const HELP = `${bold("codespur")} v${VERSION} — AI-powered local PR reviewer

${bold("USAGE")}
  codespur [options]

${bold("OPTIONS")}
  -b, --base <branch>    Base branch to diff against        (default: main)
  -c, --custom <text>    Extra review instructions
  -j, --jobs <n>         Files reviewed concurrently         (default: ${DEFAULT_JOBS})
  -o, --out <file>       Also write a markdown report
      --staged           Review staged changes (git diff --cached)
      --working          Review modified tracked files (excludes untracked)
      --timeout <secs>   Abort a request after N idle seconds (default: ${DEFAULT_IDLE_SECONDS})
  -h, --help             Show this help
  -v, --version          Print version and exit

${bold("ENVIRONMENT")}
  CODESPUR_BASE_URL   OpenAI-compatible endpoint  (required, e.g. https://api.openai.com/v1)
  CODESPUR_MODEL      Model name                  (required, e.g. gpt-4o-mini)
  CODESPUR_API_KEY    API key (optional for local backends)

${bold("EXAMPLES")}
  codespur                              Review current branch vs main
  codespur -b develop -j 4              Diff vs develop, 4 files at a time
  codespur --staged                     Review staged changes before committing
  codespur -o review.md -c "security"   Save a report, security-focused

${bold("EXIT CODES")}
  0  review completed
  1  one or more requests failed (backend unreachable, HTTP error, timeout)
  2  invalid CLI arguments
  130 interrupted (Ctrl-C)
`;

if (import.meta.main) {
  const { values } = parseArgs({
    args: Bun.argv.slice(2),
    options: {
      base: { type: "string", short: "b", default: "main" },
      custom: { type: "string", short: "c" },
      jobs: { type: "string", short: "j", default: String(DEFAULT_JOBS) },
      out: { type: "string", short: "o" },
      staged: { type: "boolean", default: false },
      working: { type: "boolean", default: false },
      timeout: { type: "string", default: String(DEFAULT_IDLE_SECONDS) },
      help: { type: "boolean", short: "h", default: false },
      version: { type: "boolean", short: "v", default: false },
    },
    allowPositionals: true,
  });

  if (values.version) {
    process.stdout.write(`codespur ${VERSION}\n`);
    process.exit(0);
  }
  if (values.help) {
    process.stdout.write(HELP + "\n");
    process.exit(0);
  }

  const envBase = process.env.CODESPUR_BASE_URL?.replace(/\/+$/, "");
  const envModel = process.env.CODESPUR_MODEL;
  if (!envBase) {
    dieEarly("CODESPUR_BASE_URL not set. Point it at an OpenAI-compatible endpoint (e.g. https://api.openai.com/v1).");
  }
  if (!envModel) {
    dieEarly("CODESPUR_MODEL not set. Specify a model name (e.g. gpt-4o-mini).");
  }
  baseUrl = envBase;
  model = envModel;

  base = values.base!;
  custom = values.custom;
  outPath = values.out;
  jobs = Math.max(1, parseInt(values.jobs ?? "", 10) || DEFAULT_JOBS);
  idleMs = Math.max(1, parseInt(values.timeout ?? "", 10) || DEFAULT_IDLE_SECONDS) * 1000;
  stagedFlag = Boolean(values.staged);
  workingFlag = Boolean(values.working);

  process.on("SIGINT", () => {
    if (interrupted) process.exit(130);
    interrupted = true;
    userAbort.abort();
    for (const t of allTasks) {
      if (!t.done) { t.error = new Error("cancelled"); t.done = true; t.resolveDone(); }
    }
    process.stderr.write("\n" + yellow("Interrupted — cancelling in-flight requests…") + "\n");
  });

  main().catch((e) => die((e as Error).message));
}
