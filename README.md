# Coblink

AI-powered PR reviewer.

## Install

Download the binary for your platform from [Releases](https://github.com/ernilambar/coblink/releases) and put it on your `PATH`.

## Usage

```bash
coblink                              # review current branch vs main
coblink -b develop -j 4              # diff vs develop, 4 files concurrently
coblink --staged                     # review staged changes before committing
coblink -o review.md -c "security"   # save a report, security-focused
```

| Flag | Long | Description | Default |
|------|------|-------------|---------|
| `-b` | `--base` | Base branch to diff against | `main` |
| `-c` | `--custom` | Extra reviewer instructions | — |
| `-j` | `--jobs` | Files reviewed concurrently | `3` |
| `-o` | `--out` | Write a markdown report | — |
|  | `--staged` | Review staged changes | — |
|  | `--working` | Review modified tracked files (excludes untracked; `git add` first to include new files) | — |
|  | `--timeout` | Abort after N idle seconds | `120` |
| `-h` | `--help` | Show help | — |
| `-v` | `--version` | Print version | — |

> [!IMPORTANT]
> `--custom` is injected verbatim into the reviewer's system prompt. **Never wire it to untrusted input** (e.g. a PR title or commit message in CI) — a hostile string can override review instructions, exfiltrate diff content, or downgrade severity. Treat it as operator-only.

> [!NOTE]
> Coblink is **advisory**. Severity is an LLM opinion, not a gate — do not use it to block merges automatically. Pipe the output to a human reviewer or a report.

## Backend

Set these env vars to point at any OpenAI-compatible backend:

```bash
export COBLINK_BASE_URL="http://localhost:1234/v1"
export COBLINK_MODEL="qwen2.5-coder"
export COBLINK_API_KEY="sk-..."        # optional for local backends
```

| Backend | `COBLINK_BASE_URL` |
|---------|--------------------|
| LM Studio | `http://localhost:1234/v1` |
| Ollama | `http://localhost:11434/v1` |
| OpenAI | `https://api.openai.com/v1` |
| DeepSeek | `https://api.deepseek.com/v1` |

## Contributing

Single-file source: `coblink.ts`. Uses only `Bun.spawnSync`, `fetch`, and `util.parseArgs` — no external dependencies. Requires [Bun](https://bun.sh).

```bash
bun test             # run tests
bun run build        # → ./coblink for current platform
bun run build:all    # → dist/ binaries for mac arm64
```

## License

[MIT](http://opensource.org/licenses/MIT) © 2026 [Nilambar Sharma](https://www.nilambar.net)
