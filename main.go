package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const VERSION = "1.0.3"

const MaxDiffChars = 24_000
const defaultIdleSeconds = 120
const defaultJobs = 3

var severities = []string{"none", "low", "medium", "high", "critical"}

func rankOf(s string) int {
	for i, v := range severities {
		if v == s {
			return i
		}
	}
	return -1
}

var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}()

func paint(code, s string) string {
	if !isTTY {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func bold(s string) string   { return paint("1", s) }
func dim(s string) string    { return paint("2", s) }
func red(s string) string    { return paint("31", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
func cyan(s string) string   { return paint("36", s) }
func gray(s string) string   { return paint("90", s) }

func sevColor(s string) string {
	switch s {
	case "critical", "high":
		return red(s)
	case "medium":
		return yellow(s)
	case "low":
		return cyan(s)
	default:
		return gray(s)
	}
}

var skipFilenames = map[string]bool{
	"bun.lockb": true, "package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"npm-shrinkwrap.json": true, "cargo.lock": true, "composer.lock": true, "gemfile.lock": true,
	"poetry.lock": true, "pdm.lock": true, "flake.lock": true, "go.sum": true,
}

var skipExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
	".ico": true, ".svg": true, ".tiff": true, ".woff": true, ".woff2": true, ".ttf": true,
	".otf": true, ".eot": true, ".mp3": true, ".mp4": true, ".wav": true, ".mov": true,
	".avi": true, ".webm": true, ".flac": true, ".ogg": true, ".zip": true, ".tar": true,
	".gz": true, ".tgz": true, ".rar": true, ".7z": true, ".bz2": true, ".xz": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".bin": true, ".o": true,
	".a": true, ".wasm": true, ".class": true, ".jar": true, ".pyc": true, ".node": true,
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true,
	".pptx": true, ".map": true, ".snap": true,
}

func isNoise(file string) bool {
	lower := strings.ToLower(file)
	slash := strings.LastIndex(lower, "/")
	name := lower
	if slash >= 0 {
		name = lower[slash+1:]
	}
	if skipFilenames[name] {
		return true
	}
	if strings.HasSuffix(name, ".lock") {
		return true
	}
	if strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".min.css") {
		return true
	}
	if strings.HasSuffix(name, ".d.ts") {
		return true
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return false
	}
	return skipExtensions[name[dot:]]
}

func parseNumstatBinaries(out string) map[string]bool {
	set := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 && parts[0] == "-" && parts[1] == "-" {
			set[strings.TrimSpace(strings.Join(parts[2:], "\t"))] = true
		}
	}
	return set
}

type budgetResult struct {
	text      string
	truncated bool
	shown     int
	total     int
}

func budgetDiff(diff string) budgetResult {
	if len(diff) <= MaxDiffChars {
		return budgetResult{text: diff}
	}

	lines := strings.Split(diff, "\n")
	firstHunk := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "@@") {
			firstHunk = i
			break
		}
	}
	if firstHunk == -1 {
		return budgetResult{
			text:      diff[:MaxDiffChars] + "\n… [diff truncated to fit context budget]",
			truncated: true,
		}
	}

	header := strings.Join(lines[:firstHunk], "\n")
	var hunks []string
	var cur []string
	for i := firstHunk; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "@@") && len(cur) > 0 {
			hunks = append(hunks, strings.Join(cur, "\n"))
			cur = cur[:0]
		}
		cur = append(cur, lines[i])
	}
	if len(cur) > 0 {
		hunks = append(hunks, strings.Join(cur, "\n"))
	}

	out := header
	shown := 0
	for _, h := range hunks {
		if shown > 0 && len(out)+len(h)+1 > MaxDiffChars {
			break
		}
		out += "\n" + h
		shown++
		if len(out) > MaxDiffChars {
			break
		}
	}
	truncated := shown < len(hunks)
	if truncated {
		out += fmt.Sprintf("\n\n… [diff truncated: %d/%d hunks shown to fit context budget]", shown, len(hunks))
	}
	return budgetResult{text: out, truncated: truncated, shown: shown, total: len(hunks)}
}

var severityRe = regexp.MustCompile(`(?i)SEVERITY:\s*(none|low|medium|high|critical)`)

func parseSeverity(text string) string {
	m := severityRe.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return "none"
	}
	return strings.ToLower(m[len(m)-1][1])
}

type gitResult struct {
	ok  bool
	out string
	err string
}

func gitRun(args ...string) gitResult {
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return gitResult{
		ok:  err == nil,
		out: stdout.String(),
		err: strings.TrimSpace(stderr.String()),
	}
}

func die(msg string) {
	fmt.Fprint(os.Stderr, red("✖ ")+msg+"\n")
	os.Exit(1)
}
func dieEarly(msg string) {
	fmt.Fprint(os.Stderr, red("✖ ")+msg+"\n")
	os.Exit(2)
}

type source struct {
	label       string
	needsBase   bool
	nameArgs    []string
	numstatArgs []string
	fileArgs    func(string) []string
}

type task struct {
	index     int
	file      string
	diff      string
	truncated bool
	shown     int
	total     int

	mu       sync.Mutex
	buffer   strings.Builder
	fullText strings.Builder
	live     bool
	done     bool
	err      error
	severity string

	doneCh chan struct{}
}

type runtime struct {
	baseUrl   string
	model     string
	apiKey    string
	base      string
	custom    string
	outPath   string
	jobs      int
	idleMs    int
	staged    bool
	working   bool
	diffFile  string
	tasks     []*task
	tasksMu   sync.Mutex
	interrupt chan struct{}

	fatalMu       sync.Mutex
	fatalBackend  bool
	userAbort     context.Context
	userAbortStop context.CancelFunc
}

func (r *runtime) isFatal() bool {
	r.fatalMu.Lock()
	defer r.fatalMu.Unlock()
	return r.fatalBackend
}

func (r *runtime) setFatal() {
	r.fatalMu.Lock()
	r.fatalBackend = true
	r.fatalMu.Unlock()
}

func (r *runtime) buildSource() *source {
	if r.staged && r.working {
		dieEarly("Use only one of --staged / --working.")
	}
	if r.staged {
		return &source{
			label:       "staged changes",
			needsBase:   false,
			nameArgs:    []string{"--cached", "--name-only"},
			numstatArgs: []string{"--cached", "--numstat"},
			fileArgs:    func(f string) []string { return []string{"--cached", "--", f} },
		}
	}
	if r.working {
		return &source{
			label:       "uncommitted changes",
			needsBase:   false,
			nameArgs:    []string{"HEAD", "--name-only"},
			numstatArgs: []string{"HEAD", "--numstat"},
			fileArgs:    func(f string) []string { return []string{"HEAD", "--", f} },
		}
	}
	rng := r.base + "...HEAD"
	return &source{
		label:       rng,
		needsBase:   true,
		nameArgs:    []string{"--name-only", rng},
		numstatArgs: []string{"--numstat", rng},
		fileArgs:    func(f string) []string { return []string{rng, "--", f} },
	}
}

func binarySet(src *source) map[string]bool {
	r := gitRun(append([]string{"diff"}, src.numstatArgs...)...)
	if !r.ok {
		return map[string]bool{}
	}
	return parseNumstatBinaries(r.out)
}

type fileDiff struct {
	name string
	text string
}

// fileNameFromLines extracts the canonical filename from a per-file diff chunk.
// Uses "+++ b/<path>" (handles renames correctly), falls back to "--- a/<path>"
// for deleted files, then the diff --git header for binaries.
func fileNameFromLines(lines []string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, "+++ b/") {
			return strings.TrimPrefix(line, "+++ b/")
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "--- a/") {
			return strings.TrimPrefix(line, "--- a/")
		}
	}
	if len(lines) > 0 {
		header := strings.TrimPrefix(lines[0], "diff --git ")
		if idx := strings.LastIndex(header, " b/"); idx >= 0 {
			return header[idx+3:]
		}
	}
	return ""
}

// splitDiffByFile splits a git diff into per-file chunks keyed by filename.
func splitDiffByFile(raw string) []fileDiff {
	lines := strings.Split(raw, "\n")
	var chunks [][]string
	var cur []string
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			if len(cur) > 0 {
				chunks = append(chunks, cur)
			}
			cur = []string{line}
		} else {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	var result []fileDiff
	for _, chunk := range chunks {
		name := fileNameFromLines(chunk)
		if name == "" {
			continue
		}
		result = append(result, fileDiff{name: name, text: strings.Join(chunk, "\n")})
	}
	return result
}

func (t *task) emit(text string) {
	if text == "" {
		return
	}
	t.mu.Lock()
	t.fullText.WriteString(text)
	if t.live {
		t.mu.Unlock()
		os.Stdout.WriteString(text)
		return
	}
	t.buffer.WriteString(text)
	t.mu.Unlock()
}

func (r *runtime) messagesFor(t *task) []map[string]string {
	system := "Review a single-file git diff for a pull request.\n\n" +
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
		"- critical: severe security issue, guaranteed data loss, or RCE"
	if r.custom != "" {
		system += "\n\n<extra_instructions>\n" + r.custom + "\n</extra_instructions>\n" +
			"Instructions above override defaults where they conflict."
	}
	user := fmt.Sprintf("File: %s\n\n```diff\n%s\n```", t.file, t.diff)
	return []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
}

func (r *runtime) review(ctx context.Context, t *task) error {
	if r.isFatal() {
		return errors.New("backend unreachable (skipped)")
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-r.userAbort.Done():
			cancel()
		case <-reqCtx.Done():
		}
	}()

	var idledOut atomicBool
	idleDur := time.Duration(r.idleMs) * time.Millisecond
	idleTimer := time.AfterFunc(idleDur, func() {
		idledOut.set(true)
		cancel()
	})
	defer idleTimer.Stop()
	resetIdle := func() { idleTimer.Reset(idleDur) }

	body, _ := json.Marshal(map[string]any{
		"model":    r.model,
		"stream":   true,
		"messages": r.messagesFor(t),
	})

	req, err := http.NewRequestWithContext(reqCtx, "POST", r.baseUrl+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

	client := &http.Client{Timeout: 0}
	res, err := client.Do(req)
	if err != nil {
		if idledOut.get() {
			return fmt.Errorf("no response for %ds (timed out)", r.idleMs/1000)
		}
		if r.userAbort.Err() != nil {
			return errors.New("cancelled")
		}
		r.setFatal()
		return fmt.Errorf("could not reach backend at %s (%s)", r.baseUrl, err.Error())
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		msg := strings.TrimSpace(fmt.Sprintf("backend returned HTTP %d %s", res.StatusCode, string(buf)))
		return errors.New(msg)
	}

	ctype := res.Header.Get("Content-Type")
	if !strings.Contains(ctype, "event-stream") {
		buf, _ := io.ReadAll(res.Body)
		content := ""
		var j sseChunk
		if json.Unmarshal(buf, &j) == nil && len(j.Choices) > 0 {
			if j.Choices[0].Message.Content != "" {
				content = j.Choices[0].Message.Content
			} else if j.Choices[0].Text != "" {
				content = j.Choices[0].Text
			}
		}
		if content == "" {
			content = string(buf)
		}
		if content == "" {
			t.emit(dim("(empty response)"))
		} else {
			t.emit(content)
		}
		return nil
	}

	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		resetIdle()
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			return nil
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			t.emit(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		if idledOut.get() {
			return fmt.Errorf("no response for %ds (timed out)", r.idleMs/1000)
		}
		if r.userAbort.Err() != nil {
			return errors.New("cancelled")
		}
		return err
	}
	return nil
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) get() bool  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func (r *runtime) startPool(ctx context.Context, tasks []*task) {
	var idx atomicInt
	jobs := r.jobs
	if jobs > len(tasks) {
		jobs = len(tasks)
	}
	for k := 0; k < jobs; k++ {
		go func() {
			for {
				select {
				case <-r.interrupt:
					return
				default:
				}
				cur := idx.incAndGet() - 1
				if cur >= len(tasks) {
					return
				}
				t := tasks[cur]
				err := r.review(ctx, t)
				t.mu.Lock()
				if err != nil && t.err == nil {
					t.err = err
				}
				t.done = true
				t.mu.Unlock()
				close(t.doneCh)
			}
		}()
	}
}

type atomicInt struct {
	mu sync.Mutex
	v  int
}

func (a *atomicInt) incAndGet() int {
	a.mu.Lock()
	a.v++
	v := a.v
	a.mu.Unlock()
	return v
}

func (r *runtime) mainLoop() {
	if !gitRun("rev-parse", "--is-inside-work-tree").ok {
		die("Not inside a git repository.")
	}

	src := r.buildSource()
	if src.needsBase && !gitRun("rev-parse", "--verify", r.base).ok {
		die(fmt.Sprintf("Base branch %q not found. Pass one with -b <branch>.", r.base))
	}

	listed := gitRun(append([]string{"diff"}, src.nameArgs...)...)
	if !listed.ok {
		die("git diff failed: " + listed.err)
	}

	var allFiles []string
	for _, s := range strings.Split(listed.out, "\n") {
		s = strings.TrimSpace(s)
		if s != "" {
			allFiles = append(allFiles, s)
		}
	}

	if len(allFiles) == 0 {
		fmt.Fprint(os.Stdout, yellow(fmt.Sprintf("No changes for %s. Nothing to review.\n", src.label)))
		return
	}

	binaries := binarySet(src)
	skippedNoise := 0
	skippedBinary := 0

	var tasks []*task
	for _, file := range allFiles {
		if binaries[file] {
			skippedBinary++
			continue
		}
		if isNoise(file) {
			skippedNoise++
			continue
		}
		raw := gitRun(append([]string{"diff"}, src.fileArgs(file)...)...).out
		if strings.Contains(raw, "Binary files ") || strings.Contains(raw, "GIT binary patch") {
			skippedBinary++
			continue
		}
		diffBody := strings.TrimSpace(raw)
		if diffBody == "" {
			continue
		}
		res := budgetDiff(diffBody)
		t := &task{
			index:     len(tasks) + 1,
			file:      file,
			diff:      res.text,
			truncated: res.truncated,
			shown:     res.shown,
			total:     res.total,
			severity:  "none",
			doneCh:    make(chan struct{}),
		}
		tasks = append(tasks, t)
	}
	r.runReview(tasks, src.label, skippedNoise, skippedBinary)
}

func (r *runtime) runReview(tasks []*task, sourceLabel string, skippedNoise, skippedBinary int) {
	r.tasksMu.Lock()
	r.tasks = tasks
	r.tasksMu.Unlock()

	var skipParts []string
	if skippedNoise > 0 {
		skipParts = append(skipParts, fmt.Sprintf("%d noise", skippedNoise))
	}
	if skippedBinary > 0 {
		skipParts = append(skipParts, fmt.Sprintf("%d binary", skippedBinary))
	}
	skipSuffix := ""
	if len(skipParts) > 0 {
		skipSuffix = fmt.Sprintf(", skipped %s", strings.Join(skipParts, ", "))
	}
	fmt.Fprint(os.Stdout,
		"\n"+bold(cyan("◆ Codespur"))+dim(" v"+VERSION+" — local PR review")+"\n"+
			gray(fmt.Sprintf("  source %s", sourceLabel))+"\n"+
			gray(fmt.Sprintf("  model  %s", r.model))+"\n"+
			gray(fmt.Sprintf("  engine %s", r.baseUrl))+"\n"+
			gray(fmt.Sprintf("  jobs   %d", r.jobs))+"\n"+
			gray(fmt.Sprintf("  files  %d to review%s", len(tasks), skipSuffix))+"\n")

	if len(tasks) == 0 {
		fmt.Fprint(os.Stdout, yellow("\nNo reviewable code files after filtering.\n"))
		return
	}

	ctx := context.Background()
	r.startPool(ctx, tasks)

	for _, t := range tasks {
		select {
		case <-r.interrupt:
			r.finish(tasks, sourceLabel, true)
			return
		default:
		}

		flags := ""
		if t.truncated {
			flags = yellow(fmt.Sprintf(" (diff truncated %d/%d hunks)", t.shown, t.total))
		}
		barLen := len(t.file) + 14
		if barLen > 60 {
			barLen = 60
		}
		fmt.Fprint(os.Stdout,
			"\n"+bold(fmt.Sprintf("─ [%d/%d] ", t.index, len(tasks)))+bold(green(t.file))+flags+"\n"+
				gray(strings.Repeat("─", barLen))+"\n")

		t.mu.Lock()
		if t.buffer.Len() > 0 {
			os.Stdout.WriteString(t.buffer.String())
			t.buffer.Reset()
		}
		t.live = true
		t.mu.Unlock()

		<-t.doneCh
		t.mu.Lock()
		t.live = false
		if t.buffer.Len() > 0 {
			os.Stdout.WriteString(t.buffer.String())
			t.buffer.Reset()
		}
		terr := t.err
		full := t.fullText.String()
		t.mu.Unlock()

		if terr != nil {
			fmt.Fprint(os.Stdout, red("✖ "+terr.Error())+"\n")
		} else {
			t.severity = parseSeverity(full)
			fmt.Fprint(os.Stdout, "\n")
		}
	}

	r.finish(tasks, sourceLabel, false)
}

func fetchRemoteDiff(rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "codespur/"+VERSION)

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects not supported — use the direct raw diff URL")
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}

func (r *runtime) diffFileLoop(diffPath string) {
	var data []byte
	var err error
	isURL := strings.HasPrefix(diffPath, "https://") || strings.HasPrefix(diffPath, "http://")
	if isURL {
		data, err = fetchRemoteDiff(diffPath)
		if err != nil {
			die("cannot fetch remote diff: " + err.Error())
		}
	} else {
		data, err = os.ReadFile(diffPath)
		if err != nil {
			die("cannot read diff file: " + err.Error())
		}
	}

	raw := strings.TrimSpace(string(data))
	if raw != "" && !strings.Contains(raw, "diff --git ") {
		die("file does not appear to be a git diff (no \"diff --git\" header found)")
	}

	fileDiffs := splitDiffByFile(raw)
	if len(fileDiffs) == 0 {
		fmt.Fprint(os.Stdout, yellow("No files found in diff. Nothing to review.\n"))
		return
	}

	skippedNoise := 0
	skippedBinary := 0
	var tasks []*task

	for _, fd := range fileDiffs {
		if strings.Contains(fd.text, "Binary files ") || strings.Contains(fd.text, "GIT binary patch") {
			skippedBinary++
			continue
		}
		if isNoise(fd.name) {
			skippedNoise++
			continue
		}
		diffBody := strings.TrimSpace(fd.text)
		if diffBody == "" {
			continue
		}
		res := budgetDiff(diffBody)
		t := &task{
			index:     len(tasks) + 1,
			file:      fd.name,
			diff:      res.text,
			truncated: res.truncated,
			shown:     res.shown,
			total:     res.total,
			severity:  "none",
			doneCh:    make(chan struct{}),
		}
		tasks = append(tasks, t)
	}

	r.runReview(tasks, filepath.Base(diffPath), skippedNoise, skippedBinary)
}

func (r *runtime) finish(tasks []*task, sourceLabel string, interrupted bool) {
	anyError := false
	for _, t := range tasks {
		if t.err != nil {
			anyError = true
			break
		}
	}

	fmt.Fprint(os.Stdout, "\n"+bold("Summary")+"\n")
	width := 4
	for _, t := range tasks {
		if len(t.file) > width {
			width = len(t.file)
		}
	}
	if width > 48 {
		width = 48
	}
	for _, t := range tasks {
		label := t.file
		if len(label) < width {
			label += strings.Repeat(" ", width-len(label))
		}
		val := sevColor(t.severity)
		if t.err != nil {
			val = red("error")
		}
		fmt.Fprint(os.Stdout, gray("  "+label)+"  "+val+"\n")
	}

	if r.outPath != "" {
		var lines []string
		lines = append(lines,
			"# Codespur review", "",
			fmt.Sprintf("- Source: `%s`", sourceLabel),
			fmt.Sprintf("- Model: `%s`", r.model),
			fmt.Sprintf("- Generated: %s", time.Now().UTC().Format(time.RFC3339)),
			"", "---", "",
		)
		for _, t := range tasks {
			lines = append(lines, "## "+t.file, "")
			meta := ""
			if t.truncated {
				meta = fmt.Sprintf("  _(diff truncated %d/%d hunks)_", t.shown, t.total)
			}
			sev := t.severity
			if t.err != nil {
				sev = "error"
			}
			lines = append(lines, fmt.Sprintf("**Severity:** %s%s", sev, meta), "")
			body := strings.TrimSpace(t.fullText.String())
			if t.err != nil {
				body = "> " + t.err.Error()
			}
			if body == "" {
				body = "_(no output)_"
			}
			lines = append(lines, body, "")
		}
		if err := os.WriteFile(r.outPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
			fmt.Fprint(os.Stderr, red("✖ could not write report: "+err.Error())+"\n")
		} else {
			fmt.Fprint(os.Stdout, "\n"+gray("report written to "+r.outPath)+"\n")
		}
	}

	if interrupted {
		fmt.Fprint(os.Stdout, "\n"+yellow("✖ Interrupted.")+"\n\n")
		os.Exit(130)
	}
	if anyError {
		fmt.Fprint(os.Stdout, "\n"+yellow("✔ Done, with request errors.")+"\n\n")
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, "\n"+bold(green("✔ Review complete."))+"\n\n")
}

const helpTemplate = "%s v%s — AI-powered local PR reviewer\n\n" +
	"%s\n" +
	"  codespur [options]\n\n" +
	"%s\n" +
	"  -b, --base <branch>    Base branch to diff against        (default: main)\n" +
	"  -c, --custom <text>    Extra review instructions\n" +
	"  -j, --jobs <n>         Files reviewed concurrently         (default: %d)\n" +
	"  -o, --out <file>       Also write a markdown report\n" +
	"      --staged           Review staged changes (git diff --cached)\n" +
	"      --working          Review modified tracked files (excludes untracked)\n" +
	"  -f, --diff-file <path|url> Review a saved diff file or direct https:// diff URL\n" +
	"      --timeout <secs>   Abort a request after N idle seconds (default: %d)\n" +
	"  -h, --help             Show this help\n" +
	"  -v, --version          Print version and exit\n\n" +
	"%s\n" +
	"  CODESPUR_BASE_URL   OpenAI-compatible endpoint  (required, e.g. https://api.openai.com/v1)\n" +
	"  CODESPUR_MODEL      Model name                  (required, e.g. gpt-4o-mini)\n" +
	"  CODESPUR_API_KEY    API key (optional for local backends)\n\n" +
	"%s\n" +
	"  codespur                              Review current branch vs main\n" +
	"  codespur -b develop -j 4              Diff vs develop, 4 files at a time\n" +
	"  codespur --staged                     Review staged changes before committing\n" +
	"  codespur -o review.md -c \"security\"   Save a report, security-focused\n\n" +
	"%s\n" +
	"  0  review completed\n" +
	"  1  one or more requests failed (backend unreachable, HTTP error, timeout)\n" +
	"  2  invalid CLI arguments\n" +
	"  130 interrupted (Ctrl-C)\n"

func helpText() string {
	return fmt.Sprintf(helpTemplate,
		bold("codespur"), VERSION,
		bold("USAGE"),
		bold("OPTIONS"),
		defaultJobs, defaultIdleSeconds,
		bold("ENVIRONMENT"),
		bold("EXAMPLES"),
		bold("EXIT CODES"),
	)
}

func main() {
	r := &runtime{
		interrupt: make(chan struct{}),
	}
	r.userAbort, r.userAbortStop = context.WithCancel(context.Background())

	fs := flag.NewFlagSet("codespur", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var base string
	var custom string
	var jobsStr string
	var out string
	var staged bool
	var working bool
	var timeoutStr string
	var help bool
	var version bool

	fs.StringVar(&base, "base", "main", "")
	fs.StringVar(&base, "b", "main", "")
	fs.StringVar(&custom, "custom", "", "")
	fs.StringVar(&custom, "c", "", "")
	fs.StringVar(&jobsStr, "jobs", fmt.Sprintf("%d", defaultJobs), "")
	fs.StringVar(&jobsStr, "j", fmt.Sprintf("%d", defaultJobs), "")
	fs.StringVar(&out, "out", "", "")
	fs.StringVar(&out, "o", "", "")
	fs.BoolVar(&staged, "staged", false, "")
	fs.BoolVar(&working, "working", false, "")
	var diffFile string
	fs.StringVar(&diffFile, "diff-file", "", "")
	fs.StringVar(&diffFile, "f", "", "")
	fs.StringVar(&timeoutStr, "timeout", fmt.Sprintf("%d", defaultIdleSeconds), "")
	fs.BoolVar(&help, "help", false, "")
	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&version, "version", false, "")
	fs.BoolVar(&version, "v", false, "")

	args := normalizeArgs(os.Args[1:])
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, red("✖ ")+err.Error()+"\n")
		os.Exit(2)
	}

	if version {
		fmt.Fprintf(os.Stdout, "codespur %s\n", VERSION)
		os.Exit(0)
	}
	if help {
		fmt.Fprint(os.Stdout, helpText()+"\n")
		os.Exit(0)
	}

	envBase := strings.TrimRight(os.Getenv("CODESPUR_BASE_URL"), "/")
	envModel := os.Getenv("CODESPUR_MODEL")
	if envBase == "" {
		dieEarly("CODESPUR_BASE_URL not set. Point it at an OpenAI-compatible endpoint (e.g. https://api.openai.com/v1).")
	}
	if envModel == "" {
		dieEarly("CODESPUR_MODEL not set. Specify a model name (e.g. gpt-4o-mini).")
	}
	r.baseUrl = envBase
	r.model = envModel
	r.apiKey = os.Getenv("CODESPUR_API_KEY")
	r.base = base
	r.custom = custom
	r.outPath = out
	r.staged = staged
	r.working = working
	r.diffFile = diffFile

	jobsInt := defaultJobs
	if n, err := parsePositiveInt(jobsStr); err == nil {
		jobsInt = n
	}
	r.jobs = jobsInt

	idleSec := defaultIdleSeconds
	if n, err := parsePositiveInt(timeoutStr); err == nil {
		idleSec = n
	}
	r.idleMs = idleSec * 1000

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGINT)
	go func() {
		for range sigCh {
			select {
			case <-r.interrupt:
				os.Exit(130)
			default:
			}
			close(r.interrupt)
			r.userAbortStop()
			r.tasksMu.Lock()
			tasks := r.tasks
			r.tasksMu.Unlock()
			for _, t := range tasks {
				t.mu.Lock()
				if !t.done {
					t.err = errors.New("cancelled")
					t.done = true
					t.mu.Unlock()
					select {
					case <-t.doneCh:
					default:
						close(t.doneCh)
					}
					continue
				}
				t.mu.Unlock()
			}
			fmt.Fprint(os.Stderr, "\n"+yellow("Interrupted — cancelling in-flight requests…")+"\n")
			return
		}
	}()

	defer func() {
		if rec := recover(); rec != nil {
			die(fmt.Sprintf("%v", rec))
		}
	}()

	if r.diffFile != "" {
		if r.staged || r.working {
			dieEarly("--diff-file cannot be used with --staged or --working.")
		}
		r.diffFileLoop(r.diffFile)
	} else {
		r.mainLoop()
	}
}

func parsePositiveInt(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	if n < 1 {
		return 1, nil
	}
	return n, nil
}

// normalizeArgs converts GNU-style --long-flag into Go's -long-flag form so
// stdlib `flag` accepts both single- and double-dash prefixes uniformly.
func normalizeArgs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			out = append(out, a[1:])
			continue
		}
		out = append(out, a)
	}
	return out
}
