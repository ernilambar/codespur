package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────────────── unit: isNoise ───────────────────────

func TestIsNoise_skipsLockAndBinaryExts(t *testing.T) {
	for _, f := range []string{
		"bun.lockb", "yarn.lock", "go.sum", "custom.lock",
		"logo.png", "src/icon.svg", "fonts/x.woff2", "a.min.js",
		"dist/app.min.css", "build/out.wasm", "vendor.tar.gz",
	} {
		if !isNoise(f) {
			t.Errorf("expected noise: %s", f)
		}
	}
}

func TestIsNoise_keepsSource(t *testing.T) {
	for _, f := range []string{
		"app.js", "src/utils.ts", "README.md", "Dockerfile",
		"lib/main.py", "server.go", "styles.css",
	} {
		if isNoise(f) {
			t.Errorf("expected NOT noise: %s", f)
		}
	}
}

// ─────────────────── unit: parseNumstatBinaries ───────────────────

func TestParseNumstatBinaries(t *testing.T) {
	out := "2\t1\tapp.js\n-\t-\tassets/blob.dat\n1\t0\tutils.ts\n-\t-\timg.raw\n"
	got := parseNumstatBinaries(out)
	if len(got) != 2 || !got["assets/blob.dat"] || !got["img.raw"] {
		t.Errorf("unexpected result: %#v", got)
	}
}

func TestParseNumstatBinaries_empty(t *testing.T) {
	if len(parseNumstatBinaries("")) != 0 {
		t.Errorf("expected empty set")
	}
}

// ────────────────────── unit: parseSeverity ──────────────────────

func TestParseSeverity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"looks fine\nSEVERITY: high", "high"},
		{"severity: Medium", "medium"},
		{"SEVERITY: low\nmore\nSEVERITY: critical", "critical"},
		{"no tag here", "none"},
	}
	for _, c := range cases {
		if got := parseSeverity(c.in); got != c.want {
			t.Errorf("parseSeverity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ────────────────── unit: rankOf / severities / sevColor ──────────────────

func TestRankOfAndSeverities(t *testing.T) {
	if rankOf("none") >= rankOf("low") {
		t.Errorf("none should rank below low")
	}
	if rankOf("high") >= rankOf("critical") {
		t.Errorf("high should rank below critical")
	}
	if len(severities) != 5 {
		t.Errorf("expected 5 severities, got %d", len(severities))
	}
	if rankOf("bogus") != -1 {
		t.Errorf("expected -1 for unknown")
	}
	for _, s := range severities {
		if sevColor(s) == "" {
			t.Errorf("sevColor(%q) empty", s)
		}
	}
}

// ─────────────────────── unit: budgetDiff ───────────────────────

func TestBudgetDiff_small(t *testing.T) {
	d := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n"
	r := budgetDiff(d)
	if r.truncated || r.text != d {
		t.Errorf("small diff must pass through untouched")
	}
}

func TestBudgetDiff_largeMultiHunk(t *testing.T) {
	header := "diff --git a/big b/big\n--- a/big\n+++ b/big\n"
	var b strings.Builder
	total := 80
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "@@ -%d +%d @@\n+%s\n", i, i, strings.Repeat("x", 500))
	}
	big := header + b.String()
	if len(big) <= MaxDiffChars {
		t.Fatal("test setup wrong: big diff too small")
	}
	r := budgetDiff(big)
	if !r.truncated {
		t.Errorf("expected truncated")
	}
	if r.shown <= 0 || r.shown >= r.total {
		t.Errorf("shown %d out of %d", r.shown, r.total)
	}
	if r.total != total {
		t.Errorf("total mismatch")
	}
	if !strings.Contains(r.text, "diff truncated:") {
		t.Errorf("missing truncation marker")
	}
	if len(r.text) > MaxDiffChars+200 {
		t.Errorf("truncated diff too long: %d", len(r.text))
	}
}

func TestBudgetDiff_noHunksStillTruncates(t *testing.T) {
	r := budgetDiff(strings.Repeat("Z", MaxDiffChars+5000))
	if !r.truncated || !strings.Contains(r.text, "truncated") {
		t.Errorf("expected truncation for hunkless oversized diff")
	}
}

// ─────────────────── unit: splitDiffByFile ───────────────────

func TestSplitDiffByFile_simple(t *testing.T) {
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/bar.ts b/bar.ts\n--- a/bar.ts\n+++ b/bar.ts\n@@ -1 +1 @@\n-a\n+b\n"
	files := splitDiffByFile(raw)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0].name != "foo.go" {
		t.Errorf("files[0].name = %q, want foo.go", files[0].name)
	}
	if files[1].name != "bar.ts" {
		t.Errorf("files[1].name = %q, want bar.ts", files[1].name)
	}
}

func TestSplitDiffByFile_rename(t *testing.T) {
	raw := "diff --git a/old.go b/new.go\nrename from old.go\nrename to new.go\n--- a/old.go\n+++ b/new.go\n@@ -1 +1 @@\n-x\n+y\n"
	files := splitDiffByFile(raw)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].name != "new.go" {
		t.Errorf("files[0].name = %q, want new.go", files[0].name)
	}
}

func TestSplitDiffByFile_deleted(t *testing.T) {
	raw := "diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n"
	files := splitDiffByFile(raw)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files[0].name != "gone.go" {
		t.Errorf("files[0].name = %q, want gone.go", files[0].name)
	}
}

func TestSplitDiffByFile_empty(t *testing.T) {
	if len(splitDiffByFile("")) != 0 {
		t.Errorf("expected empty result for empty input")
	}
}

// ────────────────────── integration: CLI ──────────────────────

func mockHandler(w http.ResponseWriter, req *http.Request) {
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		http.Error(w, "nf", http.StatusNotFound)
		return
	}
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file := "unknown"
	if len(body.Messages) > 1 {
		for _, line := range strings.Split(body.Messages[1].Content, "\n") {
			if strings.HasPrefix(line, "File: ") {
				file = strings.TrimPrefix(line, "File: ")
				break
			}
		}
	}
	sev := "none"
	if strings.Contains(file, "app") {
		sev = "high"
	}
	text := fmt.Sprintf("Review of %s: check edges. SEVERITY: %s", file, sev)
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, word := range strings.Split(text, " ") {
		chunk := map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{"content": word + " "}}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

var (
	testBinary string
	testServer *httptest.Server
	testRepo   string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "codespur-testbin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	testBinary = filepath.Join(tmp, "codespur-test")

	build := exec.Command("go", "build", "-o", testBinary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}

	testServer = httptest.NewServer(http.HandlerFunc(mockHandler))

	testRepo, err = os.MkdirTemp("", "codespur-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp repo:", err)
		os.Exit(1)
	}
	seedRepo(testRepo)

	code := m.Run()

	testServer.Close()
	os.RemoveAll(tmp)
	os.RemoveAll(testRepo)
	os.Exit(code)
}

func gitInRepo(t testing.TB, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = testRepo
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(b))
	}
}

func gitInDir(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %v\n%s", args, err, string(out))
	}
	return nil
}

func seedRepo(dir string) {
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	must(gitInDir(dir, "init", "-q", "-b", "main"))
	must(gitInDir(dir, "config", "user.email", "t@t.com"))
	must(gitInDir(dir, "config", "user.name", "test"))
	must(os.WriteFile(filepath.Join(dir, "app.js"), []byte("function add(a,b){return a+b;}\n"), 0644))
	must(gitInDir(dir, "add", "-A"))
	must(gitInDir(dir, "commit", "-qm", "init"))
	must(gitInDir(dir, "checkout", "-q", "-b", "feature"))
	must(os.WriteFile(filepath.Join(dir, "app.js"), []byte("function add(a,b){return a-b;}\n"), 0644))
	must(os.WriteFile(filepath.Join(dir, "utils.ts"), []byte("export const x = 1;\n"), 0644))
	must(os.WriteFile(filepath.Join(dir, "bun.lockb"), []byte("lockdata\n"), 0644))
	must(gitInDir(dir, "add", "-A"))
	must(gitInDir(dir, "commit", "-qm", "feature"))
}

func runCli(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(testBinary, args...)
	cmd.Dir = testRepo
	cmd.Env = append(os.Environ(),
		"CODESPUR_BASE_URL="+testServer.URL+"/v1",
		"CODESPUR_MODEL=mock",
	)
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
		}
	}
	return so.String(), se.String(), code
}

func TestCLI_version(t *testing.T) {
	out, _, code := runCli(t, "--version")
	if code != 0 {
		t.Errorf("exit code %d", code)
	}
	if !strings.HasPrefix(out, "codespur ") {
		t.Errorf("unexpected version output: %q", out)
	}
}

func TestCLI_reviewsFilesSkipsNoise(t *testing.T) {
	out, _, code := runCli(t, "-b", "main", "-j", "2")
	if code != 0 {
		t.Errorf("exit code %d, stderr contained... check test output", code)
	}
	for _, want := range []string{"app.js", "utils.ts", "skipped 1 noise", "Summary", "Review complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- out ---\n%s", want, out)
		}
	}
}

func TestCLI_stagedWorkingConflict(t *testing.T) {
	_, err, code := runCli(t, "--staged", "--working")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(err, "only one") {
		t.Errorf("stderr missing 'only one': %q", err)
	}
}

func TestCLI_unknownBaseBranch(t *testing.T) {
	_, err, code := runCli(t, "-b", "does-not-exist")
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
	if !strings.Contains(err, "not found") {
		t.Errorf("stderr missing 'not found': %q", err)
	}
}

func TestCLI_diffFile(t *testing.T) {
	cmd := exec.Command("git", "diff", "main...HEAD")
	cmd.Dir = testRepo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("git diff:", err)
	}
	f, err := os.CreateTemp("", "codespur-*.diff")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(out); err != nil {
		t.Fatal(err)
	}
	f.Close()

	stdout, _, code := runCli(t, "--diff-file", f.Name())
	if code != 0 {
		t.Errorf("exit code %d\n--- out ---\n%s", code, stdout)
	}
	for _, want := range []string{"app.js", "utils.ts", "Summary", "Review complete"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q\n--- out ---\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "bun.lockb") {
		t.Errorf("noise file bun.lockb should be skipped")
	}
	if strings.Contains(stdout, "skipped 1 noise") {
		// good — noise filter still applied from diff file path
	}
}

func TestCLI_diffFileNotADiff(t *testing.T) {
	f, err := os.CreateTemp("", "codespur-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("this is not a diff file\n")
	f.Close()
	defer os.Remove(f.Name())

	_, stderr, code := runCli(t, "--diff-file", f.Name())
	if code != 1 {
		t.Errorf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr, "diff --git") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
}

func TestCLI_diffFileStagedConflict(t *testing.T) {
	f, err := os.CreateTemp("", "codespur-*.diff")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	_, stderr, code := runCli(t, "--diff-file", f.Name(), "--staged")
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "--diff-file") {
		t.Errorf("stderr missing --diff-file notice: %q", stderr)
	}
}

func TestCLI_missingBaseUrlEnv(t *testing.T) {
	cmd := exec.Command(testBinary)
	cmd.Dir = testRepo
	env := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CODESPUR_BASE_URL=") || strings.HasPrefix(e, "CODESPUR_MODEL=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	var se strings.Builder
	cmd.Stderr = &se
	cmd.Stdout = io.Discard
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(se.String(), "CODESPUR_BASE_URL") {
		t.Errorf("stderr missing CODESPUR_BASE_URL notice: %q", se.String())
	}
}
