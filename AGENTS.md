# AGENTS.md

Instructions for AI coding agents working in this repository.

## Overview

Codespur is a command-line tool that reviews a git diff (the current branch vs a base
branch, staged changes, a saved diff file, or a diff URL) by sending each changed file to
an OpenAI-compatible chat completions backend and printing a per-file review plus a
cross-file consistency pass. It is a Go 1.24 program that uses only the standard library
and ships as a single static binary.

## Setup

Requires Go >= 1.24 and `git` on `PATH`.

```bash
git clone https://github.com/ernilambar/codespur.git
cd codespur
go mod download
```

The tool needs a backend configured at runtime (not required to build or test):

```bash
export CODESPUR_BASE_URL="http://localhost:1234/v1"   # required
export CODESPUR_MODEL="qwen2.5-coder"                 # required
export CODESPUR_API_KEY="sk-..."                      # optional for local backends
```

## Commands

```bash
# build (matches the release workflow)
go build -trimpath -ldflags="-s -w" -o codespur .

# test
go test ./...

# lint (this is the only linter CI runs)
go vet ./...

# format check / format fix
gofmt -l .
gofmt -w .

# typecheck (the compiler is the type checker for Go)
go build ./...
```

## Conventions

- **Standard library only.** `go.mod` declares no dependencies. Do not add third-party
  modules; use `net/http`, `encoding/json`, and the rest of stdlib.
- **Single package, single entry point.** All production code lives in `main.go`
  (`package main`); tests live in `main_test.go`. Do not split into subpackages without a
  deliberate, stated reason.
- **Exit codes are a public contract.** `0` = review completed, `1` = one or more requests
  failed, `2` = invalid CLI arguments, `130` = interrupted (Ctrl-C). Use `die()` for exit 1
  and `dieEarly()` for exit 2; never call `os.Exit` with other codes ad hoc.
- **Never print secrets.** `CODESPUR_API_KEY` may only be reported as `set` / `not set`
  (see `runStatus`); it must never appear in output, logs, or reports.
- **Prompt-injection boundary.** `--custom` text is injected verbatim into the system
  prompt and must never be wired to untrusted input. `--issue-file` content goes into the
  user message inside an `<issue_context>` block and is treated strictly as reference data
  — the reviewer is told never to follow instructions found inside it. Preserve this split.
- **Severity is an LLM opinion, not a gate.** Per-file reviews emit exactly one trailing
  `SEVERITY: <low|medium|high|critical>` line only when an issue is found; clean reviews say
  exactly `No issues found.` Do not turn severity into a pass/fail exit code.
- **Releases are tag-driven.** Bump the `VERSION` constant, add a `CHANGELOG.md` entry
  under a dated heading, and tag with a `v` prefix (e.g. `v1.0.6`) — unprefixed tags do not
  trigger the release workflow.

## Quality gate

Run these and confirm every one exits 0 before declaring a task complete:

```bash
gofmt -l .                                                  # must print nothing
go vet ./...                                                # must exit 0
go test ./...                                               # must exit 0
go build -trimpath -ldflags="-s -w" -o codespur .           # must exit 0
./codespur --version                                        # must print "codespur <version>"
```

`main_test.go` builds the binary in `TestMain` and drives it end-to-end against a mock
OpenAI-compatible server, so a passing `go test ./...` also covers the CLI contract.
