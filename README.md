# Coblink

AI-powered local PR reviewer built on native Bun. It walks your diff **one file
at a time**, streams reviews from any OpenAI-compatible LLM into your terminal,
and ships as a single self-contained binary.

## Install / Run

Requires [Bun](https://bun.sh). No `node_modules` — uses only `Bun.spawnSync`,
`fetch`, and `util.parseArgs`.

```bash
bun run coblink.ts        # run directly
bun link && coblink       # or install as a global command
```

## Usage

```bash
coblink                              # review current branch vs main
coblink -b develop -j 4              # diff vs develop, 4 files concurrently
coblink --staged --fail-on high      # gate a commit on staged changes
coblink -o review.md -c "security"   # save a report, security-focused
```

| Flag | Long | Description | Default |
|------|------|-------------|---------|
| `-b` | `--base` | Base branch to diff against | `main` |
| `-c` | `--custom` | Extra reviewer instructions | — |
| `-j` | `--jobs` | Files reviewed concurrently | `3` |
| `-o` | `--out` | Also write a markdown report | — |
|  | `--staged` | Review staged changes (`git diff --cached`) | — |
|  | `--working` | Review all uncommitted changes (`git diff HEAD`) | — |
|  | `--fail-on` | Exit non-zero at/above severity (`none…critical`/`off`) | `off` |
|  | `--timeout` | Abort a request after N idle seconds | `120` |
| `-h` | `--help` | Show help | — |
| `-v` | `--version` | Print version | — |

Default mode reviews the committed diff `git diff <base>...HEAD` (what a PR shows).

## Concurrency + ordered streaming

With `-j > 1`, up to N file reviews are fetched in the background while output is
still printed in order: the current file streams live and the rest buffer until
their turn. Big speedup on cloud backends; local single-slot backends simply
queue with no downside.

## Severity + CI gating

Each review ends with a `SEVERITY: <level>` line. Coblink prints a summary table
and, with `--fail-on <level>`, exits non-zero when the worst severity meets the
threshold — usable as a `pre-push` hook or CI gate.

Exit codes: `0` clean · `1` fail-on hit or request error · `2` bad usage · `130` interrupted (Ctrl+C).

## Robustness

- **Binary detection** via `git diff --numstat` (`-  -` rows) plus a per-file
  "Binary files differ" guard — catches binaries even without a known extension.
- **Idle timeout** (`--timeout`) aborts a hung request; the timer resets on every
  streamed chunk so long, healthy streams aren't cut off.
- **Ctrl+C** cancels all in-flight requests cleanly (second press force-quits).
- **Non-streaming fallback** for backends that ignore `stream:true`.

## Backend (env-driven, backend agnostic)

```bash
export COBLINK_BASE_URL="http://localhost:1234/v1"   # endpoint
export COBLINK_MODEL="qwen2.5-coder"                 # model name
export COBLINK_API_KEY="sk-..."                       # optional for local
```

| Backend | `COBLINK_BASE_URL` |
|---------|--------------------|
| LM Studio | `http://localhost:1234/v1` |
| Ollama | `http://localhost:11434/v1` |
| OpenAI | `https://api.openai.com/v1` |
| DeepSeek | `https://api.deepseek.com/v1` |

`OPENAI_BASE_URL` / `OPENAI_API_KEY` / `OPENAI_MODEL` are accepted as fallbacks.

## Noise filtering

Lockfiles (`*.lock`, `bun.lockb`, `package-lock.json`, `go.sum`, …), assets
(`.png`, `.svg`, fonts, media), archives, and compiled binaries are skipped
automatically.

## Build single binaries (Bun bundler)

```bash
bun run build        # → ./coblink for the current platform
bun run build:all    # → dist/ binaries for mac (arm64/x64)
```

## Copyright and License

This project is licensed under the [MIT](http://opensource.org/licenses/MIT).

2026 &copy; [Nilambar Sharma](https://www.nilambar.net).
