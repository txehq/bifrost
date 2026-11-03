// Package main is todocheck, the gate on New Tokun's placeholders.
//
// New Tokun leaves a piece it has not built yet as a placeholder: a comment line
// with the `TODO(tokun-B<n>)` marker, where B<n> is the batch that removes it, a
// second comment line starting with the stage prefix (`阶段：`) that says which
// batch put it there and which plan item removes it, and the stand-in code under
// both. The convention and an example are in AGENTS.md §New Tokun.
//
// Two things are checked, in every file git tracks or would track:
//
//   - every placeholder TODO is a comment line of its own in the marker's exact
//     spelling, and the line under it is the stage line, written with the same
//     comment token;
//   - no placeholder remains for a batch the batches file lists as closed.
//
// A misspelt marker (another case, a missing dash or colon, a trailing comment)
// is reported, never skipped, and so is a stage line with no TODO above it.
// A Markdown document (.md, .mdx) is not read: a marker there is an example in
// prose. Any other file whose comment syntax this program does not know is
// reported on every line that carries the marker, in any spelling, so no
// placeholder escapes both checks by being in a file type nobody listed.
//
// The same program lives in txe (cmd/tools/todocheck, the source) and in the
// Bifrost fork (tools/todocheck, a copy); change both together.
//
//	go run ./cmd/tools/todocheck -batches tokun/BATCHES                          (txe)
//	cd tools/todocheck && GOWORK=off go run . -root ../.. -batches BATCHES      (Bifrost fork)
//
// It reads a line as a comment the way txe's .github/scripts/check-english.py
// does: by the file's comment syntax, skipping strings on the code side. A marker
// inside a string literal is data, and one quoted in backticks inside a comment
// is a mention. It does not see a marker inside a Python docstring or a
// multi-line raw string.
//
// Exit 0: every placeholder is well formed and none belongs to a closed batch.
// Exit 1: problems, one `path:line: message` each. Exit 2: it could not run.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const stagePrefix = "阶段："

var (
	// Any spelling of the marker, so a misspelt placeholder is reported rather
	// than passing unseen.
	markerRe = regexp.MustCompile(`(?i)todo\s*\(\s*tokun`)
	todoRe   = regexp.MustCompile(`^TODO\(tokun-B(0|[1-9][0-9]*)\): \S`)
	batchRe  = regexp.MustCompile(`^B(0|[1-9][0-9]*)$`)
	quotedRe = regexp.MustCompile("`[^`]*`")
)

// form is one way a file type writes a comment: a token that runs to the end of
// the line, or an opening and a closing token.
type form struct{ open, close string }

var (
	slash = form{open: "//"}
	block = form{open: "/*", close: "*/"}
	jsx   = form{open: "{/*", close: "*/}"}
	hash  = form{open: "#"}
	dash  = form{open: "--"}

	slashForms = []form{slash, block}
	jsxForms   = []form{slash, jsx, block}
	cssForms   = []form{block}
	hashForms  = []form{hash}
	sqlForms   = []form{dash, block}
)

// formsByExt follows check-english.py's table, so both tools read the same text
// as comments.
var formsByExt = map[string][]form{
	".go": slashForms, ".ts": slashForms, ".js": slashForms, ".mjs": slashForms, ".cjs": slashForms,
	".java": slashForms, ".kt": slashForms, ".swift": slashForms, ".rs": slashForms, ".c": slashForms,
	".h": slashForms, ".cc": slashForms, ".cpp": slashForms, ".proto": slashForms, ".scss": slashForms,
	".tsx": jsxForms, ".jsx": jsxForms,
	".css": cssForms,
	".py":  hashForms, ".sh": hashForms, ".bash": hashForms, ".zsh": hashForms, ".yaml": hashForms,
	".yml": hashForms, ".toml": hashForms, ".bzl": hashForms, ".bazel": hashForms, ".rb": hashForms,
	".tf": hashForms, ".ini": hashForms, ".conf": hashForms, ".cfg": hashForms, ".env": hashForms,
	".sql": sqlForms,
}

var hashNames = map[string]bool{
	"BUILD": true, "WORKSPACE": true, "Makefile": true, "Dockerfile": true, "Caddyfile": true,
	".gitignore": true, ".bazelrc": true, "CODEOWNERS": true,
}

// markdownExts are the documents that are not read.
var markdownExts = map[string]bool{".md": true, ".mdx": true}

func isMarkdown(name string) bool {
	return markdownExts[strings.ToLower(path.Ext(name))]
}

// commentForms is how the file at name writes comments: by its name, its
// extension, or a shebang on a file without one. Nil means a syntax this
// program does not know.
func commentForms(name string, data []byte) []form {
	base := path.Base(name)
	if hashNames[base] {
		return hashForms
	}
	ext := path.Ext(base)
	if forms, ok := formsByExt[ext]; ok {
		return forms
	}
	if ext == "" && bytes.HasPrefix(data, []byte("#!")) {
		return hashForms
	}
	return nil
}

func hasBlock(forms []form) bool {
	for _, f := range forms {
		if f.close != "" {
			return true
		}
	}
	return false
}

func opensAt(line string, i int, forms []form) (form, bool) {
	for _, f := range forms {
		if strings.HasPrefix(line[i:], f.open) {
			return f, true
		}
	}
	return form{}, false
}

// commentText is the comment part of one line, with every backtick-quoted span
// removed. Quoted strings on the code side are skipped; a closed block comment
// gives way to code again; a line starting with `*` continues a block comment.
func commentText(line string, forms []form) string {
	var parts []string
	i := 0
	trimmed := strings.TrimLeft(line, " \t")
	if hasBlock(forms) && strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "*/") {
		start := len(line) - len(trimmed) + 1
		end := strings.Index(line[start:], "*/")
		if end < 0 {
			return quotedRe.ReplaceAllString(line[start:], "")
		}
		parts = append(parts, line[start:start+end])
		i = start + end + 2
	}
	var quote byte
	for i < len(line) {
		c := line[i]
		if quote != 0 {
			if c == '\\' {
				i += 2
				continue
			}
			if c == quote {
				quote = 0
			}
			i++
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote = c
			i++
			continue
		}
		f, ok := opensAt(line, i, forms)
		if !ok {
			i++
			continue
		}
		body := line[i+len(f.open):]
		end := -1
		if f.close != "" {
			end = strings.Index(body, f.close)
		}
		if end < 0 {
			parts = append(parts, body)
			break
		}
		parts = append(parts, body[:end])
		i += len(f.open) + end + len(f.close)
	}
	return quotedRe.ReplaceAllString(strings.Join(parts, " "), "")
}

// lineComment is the text of line when the whole line is one comment in form f,
// with one space after the opening token and, for a closing one, one before it.
func lineComment(line string, f form) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, f.open+" ") {
		return "", false
	}
	body := t[len(f.open)+1:]
	if f.close != "" {
		if !strings.HasSuffix(body, " "+f.close) {
			return "", false
		}
		body = body[:len(body)-len(f.close)-1]
	}
	return body, true
}

type problem struct {
	path string
	line int
	msg  string
}

// parseBatches reads the batches file: one closed batch per line, `B<n>`, and
// nothing else. An empty file closes no batch.
func parseBatches(name string, data []byte) (map[int]bool, []problem) {
	closed := map[int]bool{}
	var problems []problem
	text := string(data)
	if text == "" {
		return closed, nil
	}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		m := batchRe.FindStringSubmatch(line)
		if m == nil {
			problems = append(problems, problem{name, i + 1, fmt.Sprintf("%q is not a closed batch; write one per line, like B1", line)})
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			problems = append(problems, problem{name, i + 1, fmt.Sprintf("%q: %v", line, err)})
			continue
		}
		if closed[n] {
			problems = append(problems, problem{name, i + 1, fmt.Sprintf("B%d is listed twice", n)})
			continue
		}
		closed[n] = true
	}
	return closed, problems
}

// checkFile reports every malformed placeholder in one file and every
// placeholder of a closed batch, and counts the well-formed ones per batch.
func checkFile(name string, data []byte, forms []form, closed map[int]bool, batchesFile string, counts map[int]int) []problem {
	if !markerRe.Match(data) && !bytes.Contains(data, []byte(stagePrefix)) {
		return nil
	}
	var problems []problem
	report := func(i int, format string, args ...any) {
		problems = append(problems, problem{name, i + 1, fmt.Sprintf(format, args...)})
	}
	lines := strings.Split(string(data), "\n")
	todoAbove := -1
	for i, line := range lines {
		text := strings.TrimSpace(commentText(line, forms))
		if strings.HasPrefix(text, stagePrefix) {
			if todoAbove != i-1 {
				report(i, "a stage line with no placeholder TODO directly above it; the two lines are removed together")
			}
			continue
		}
		if !markerRe.MatchString(text) {
			continue
		}
		var (
			f    form
			body string
			ok   bool
		)
		for _, f = range forms {
			if body, ok = lineComment(line, f); ok {
				break
			}
		}
		m := todoRe.FindStringSubmatch(body)
		if !ok || m == nil {
			report(i, "malformed placeholder TODO; write it as a comment line of its own: %s TODO(tokun-B<n>): <what to add>", forms[0].open)
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			report(i, "batch %s: %v", m[1], err)
			continue
		}
		todoAbove = i
		counts[n]++
		if closed[n] {
			report(i, "B%d is closed in %s, so this placeholder must be gone: build the piece, then delete the TODO, its stage line and the stand-in code", n, batchesFile)
		}
		if i+1 == len(lines) {
			report(i, "the placeholder TODO is the last line; the stage line goes under it: %s %s<batch it was put in; the plan item that removes it>", f.open, stagePrefix)
			continue
		}
		stage, ok := lineComment(lines[i+1], f)
		if !ok || !strings.HasPrefix(stage, stagePrefix) || strings.TrimSpace(strings.TrimPrefix(stage, stagePrefix)) == "" {
			report(i+1, "the line under the placeholder TODO must be its stage line, in the same comment form: %s %s<batch it was put in; the plan item that removes it>", f.open, stagePrefix)
		}
	}
	return problems
}

// unknownSyntax reports every line of a file whose comment syntax is not known
// that carries the marker in any spelling: such a placeholder could be checked
// neither for its stage line nor against the closed batches.
func unknownSyntax(name string, data []byte) []problem {
	if !markerRe.Match(data) {
		return nil
	}
	kind := "a " + path.Ext(name) + " file,"
	if path.Ext(name) == "" {
		kind = path.Base(name) + ", a file with no extension and no shebang,"
	}
	var problems []problem
	for i, line := range strings.Split(string(data), "\n") {
		if markerRe.MatchString(line) {
			problems = append(problems, problem{name, i + 1, fmt.Sprintf(
				"a placeholder marker in %s whose comment syntax todocheck does not know, so the placeholder cannot be checked; "+
					"put it in a file whose comments todocheck reads, or add the file type to formsByExt in todocheck "+
					"(txe's cmd/tools/todocheck and the Bifrost fork's tools/todocheck) and to txe's .github/scripts/check-english.py", kind)})
		}
	}
	return problems
}

// trackedFiles is every file git tracks under root, and every untracked file it
// does not ignore, so a placeholder is checked before it is committed.
func trackedFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git ls-files: %w: %s", err, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("git ls-files listed nothing under %s; not a checkout?", root)
	}
	sort.Strings(files)
	return files, nil
}

func batchList(batches map[int]int) string {
	keys := make([]int, 0, len(batches))
	for k := range batches {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("B%d %d", k, batches[k]))
	}
	return strings.Join(parts, ", ")
}

func closedList(closed map[int]bool) string {
	if len(closed) == 0 {
		return "none"
	}
	keys := make([]int, 0, len(closed))
	for k := range closed {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("B%d", k))
	}
	return strings.Join(parts, ", ")
}

func run(root, batchesFile string, stdout, stderr io.Writer) int {
	if batchesFile == "" {
		fmt.Fprintln(stderr, "todocheck: -batches is required (txe: tokun/BATCHES; the Bifrost fork: BATCHES)")
		return 2
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(batchesFile)))
	if err != nil {
		fmt.Fprintf(stderr, "todocheck: the batches file: %v\n", err)
		return 2
	}
	closed, problems := parseBatches(batchesFile, raw)
	files, err := trackedFiles(root)
	if err != nil {
		fmt.Fprintf(stderr, "todocheck: %v\n", err)
		return 2
	}
	counts := map[int]int{}
	for _, name := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			// Deleted in the worktree and still in the index: nothing to read.
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "todocheck: %v\n", err)
			return 2
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			fmt.Fprintf(stderr, "todocheck: %v\n", err)
			return 2
		}
		if isMarkdown(name) {
			continue
		}
		forms := commentForms(name, data)
		if forms == nil {
			problems = append(problems, unknownSyntax(name, data)...)
			continue
		}
		problems = append(problems, checkFile(name, data, forms, closed, batchesFile, counts)...)
	}
	sort.SliceStable(problems, func(a, b int) bool {
		if problems[a].path != problems[b].path {
			return problems[a].path < problems[b].path
		}
		return problems[a].line < problems[b].line
	})
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(stderr, "%s:%d: %s\n", p.path, p.line, p.msg)
		}
		fmt.Fprintf(stderr, "todocheck: %d problem(s); the convention is in AGENTS.md §New Tokun\n", len(problems))
		return 1
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		fmt.Fprintf(stdout, "todocheck: OK, no placeholders; closed batches: %s\n", closedList(closed))
		return 0
	}
	fmt.Fprintf(stdout, "todocheck: OK, %d placeholder(s) (%s); closed batches: %s\n", total, batchList(counts), closedList(closed))
	return 0
}

func main() {
	root := flag.String("root", ".", "repository root; every file git tracks or would track under it is read")
	batches := flag.String("batches", "", "the closed-batches file, relative to -root (txe: tokun/BATCHES, the Bifrost fork: BATCHES)")
	flag.Parse()
	os.Exit(run(*root, *batches, os.Stdout, os.Stderr))
}
