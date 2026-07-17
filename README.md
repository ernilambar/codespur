# Codespur

AI-powered PR reviewer.

## Install / Upgrade

**macOS** — prebuilt binary (replace `arm64` with `amd64` for Intel Macs):

```bash
curl -fL -o codespur https://github.com/ernilambar/codespur/releases/latest/download/codespur-darwin-arm64
chmod +x codespur
sudo mv codespur /usr/local/bin/
sudo xattr -d com.apple.quarantine /usr/local/bin/codespur 2>/dev/null || true
codespur --version
```

**Other platforms** — build from source (requires [Go](https://go.dev) ≥ 1.24):

```bash
git clone https://github.com/ernilambar/codespur.git
cd codespur && go build -trimpath -ldflags="-s -w" -o codespur .
sudo mv codespur /usr/local/bin/
```

To upgrade from source, replace `git clone …` with `git pull` inside the existing repo directory.

## Configure

Point at any OpenAI-compatible backend:

```bash
export CODESPUR_BASE_URL="http://localhost:1234/v1"
export CODESPUR_MODEL="qwen2.5-coder"
export CODESPUR_API_KEY="sk-..."        # optional for local backends
```

| Backend | `CODESPUR_BASE_URL` |
|---------|--------------------|
| LM Studio | `http://localhost:1234/v1` |
| Ollama | `http://localhost:11434/v1` |
| OpenAI | `https://api.openai.com/v1` |
| DeepSeek | `https://api.deepseek.com/v1` |
| GitHub Models | `https://models.github.ai/inference` |

## Usage

```bash
codespur                              # review current branch vs main
codespur -b develop -j 4              # diff vs develop, 4 files concurrently
codespur --staged                     # review staged changes before committing
codespur -f pr.diff                   # review a downloaded diff file
codespur -f https://patch-diff.githubusercontent.com/raw/org/repo/pull/123.diff  # review a remote diff
codespur -o review.md -c "security"   # save a report, security-focused
```

| Flag | Long | Description | Default |
|------|------|-------------|---------|
| `-b` | `--base` | Base branch to diff against | `main` |
| `-c` | `--custom` | Extra reviewer instructions | — |
| `-j` | `--jobs` | Files reviewed concurrently | `3` |
| `-f` | `--diff-file` | Review a saved diff file or direct `https://` diff URL | — |
| `-o` | `--out` | Write a markdown report | — |
|  | `--staged` | Review staged changes | — |
|  | `--working` | Review modified tracked files (excludes untracked; `git add` first to include new files) | — |
|  | `--timeout` | Abort after N idle seconds | `120` |
| `-h` | `--help` | Show help | — |
| `-v` | `--version` | Print version | — |

> [!IMPORTANT]
> `--custom` is injected verbatim into the reviewer's system prompt. **Never wire it to untrusted input** (e.g. a PR title or commit message in CI) — a hostile string can override review instructions, exfiltrate diff content, or downgrade severity. Treat it as operator-only.

> [!NOTE]
> Codespur is **advisory**. Severity is an LLM opinion, not a gate — do not use it to block merges automatically. Pipe the output to a human reviewer or a report.

## Contributing

Requires [Go](https://go.dev) ≥ 1.24.

```bash
go test ./...        # run tests
go build .           # → ./codespur for current platform
GOOS=linux GOARCH=amd64 go build -o dist/codespur-linux-amd64 .   # cross-compile
```

## Release

Tags must be prefixed with `v` (e.g. `v1.0.1`). The release workflow triggers on `v*` tags only — an unprefixed tag like `1.0.1` will not build or publish binaries.

```bash
git tag v1.0.1
git push origin v1.0.1
```

## License

[MIT](http://opensource.org/licenses/MIT) © 2026 [Nilambar Sharma](https://www.nilambar.net)
