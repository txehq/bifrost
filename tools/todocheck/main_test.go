package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Fixtures are single-line interpreted strings: a placeholder written as a real
// comment line here would be read by todocheck itself.
const (
	goodGo   = "package a\n\nfunc f() bool {\n\t// TODO(tokun-B2): 按其余六种检查方式判断候选是否满足请求\n\t// 阶段：B1 放入；B2-A1「九种检查方式」完成时删除这两行和占位\n\treturn true\n}\n"
	goodStub = "// TODO(tokun-B2): 补上\n// 阶段：B1 放入；B2-A1 完成时删除\n"
)

func check(name, src string, closed ...int) ([]string, map[int]int) {
	set := map[int]bool{}
	for _, n := range closed {
		set[n] = true
	}
	counts := map[int]int{}
	forms := commentForms(name, []byte(src))
	if forms == nil {
		return nil, counts
	}
	var got []string
	for _, p := range checkFile(name, []byte(src), forms, set, "tokun/BATCHES", counts) {
		got = append(got, fmt.Sprintf("%d: %s", p.line, p.msg))
	}
	return got, counts
}

func TestWellFormedPlaceholdersPass(t *testing.T) {
	cases := []struct {
		name, file, src string
		batch           int
	}{
		{"go, indented in a function", "a/x.go", goodGo, 2},
		{"go, B0", "a/x.go", "// TODO(tokun-B0): 补上\n// 阶段：B0 放入；B0-4 完成时删除\nvar x = 0\n", 0},
		{"go block comment", "a/x.go", "/* TODO(tokun-B2): 补上 */\n/* 阶段：B1 放入；B2-A1 完成时删除 */\n", 2},
		{"yaml", "k/x.yaml", "spec:\n  # TODO(tokun-B1): 补上\n  # 阶段：B0 放入；B1-A1 完成时删除\n  replicas: 1\n", 1},
		{"extensionless script with a shebang", "ops/local/tokun-e2e/merge-check", "#!/usr/bin/env bash\n# TODO(tokun-B1): 补上\n# 阶段：B0 放入；B1-A1 完成时删除\ntrue\n", 1},
		{"BUILD file", "a/BUILD", "# TODO(tokun-B1): 补上\n# 阶段：B0 放入；B1-A1 完成时删除\n", 1},
		{"sql", "tokun/migrations/x.sql", "-- TODO(tokun-B1): 补上\n-- 阶段：B0 放入；B1-A1 完成时删除\nSELECT 1;\n", 1},
		{"tsx line comment", "web/x.tsx", goodStub, 2},
		{"tsx inside JSX", "web/x.tsx", "return (\n  <div>\n    {/* TODO(tokun-B5): 补上 */}\n    {/* 阶段：B1 放入；B5-1 完成时删除 */}\n  </div>\n)\n", 5},
		{"CRLF line endings", "a/x.go", "// TODO(tokun-B2): 补上\r\n// 阶段：B1 放入；B2-A1 完成时删除\r\nvar x = 0\r\n", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, counts := check(c.file, c.src)
			if len(got) != 0 {
				t.Fatalf("problems on a well-formed placeholder: %q", got)
			}
			if want := map[int]int{c.batch: 1}; !reflect.DeepEqual(counts, want) {
				t.Fatalf("counts = %v, want %v", counts, want)
			}
		})
	}
}

func TestMalformedPlaceholdersAreReported(t *testing.T) {
	cases := []struct {
		name, file, src string
		line            int
		want            string
	}{
		{"no stage line", "a/x.go", "// TODO(tokun-B2): 补上\nvar x = 0\n", 2, "must be its stage line"},
		{"stage line with an ASCII colon", "a/x.go", "// TODO(tokun-B2): 补上\n// 阶段:B1 放入\n", 2, "must be its stage line"},
		{"empty stage line", "a/x.go", "// TODO(tokun-B2): 补上\n// 阶段：\n", 2, "must be its stage line"},
		{"stage line in another comment form", "a/x.go", "// TODO(tokun-B2): 补上\n/* 阶段：B1 放入 */\n", 2, "in the same comment form"},
		{"stage line without its space", "a/x.go", "// TODO(tokun-B2): 补上\n//阶段：B1 放入\n", 2, "must be its stage line"},
		{"TODO on the last line", "a/x.go", "var x = 0\n// TODO(tokun-B2): 补上", 2, "is the last line"},
		{"lower-case todo", "a/x.go", "// todo(tokun-B2): 补上\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"lower-case batch", "a/x.go", "// TODO(tokun-b2): 补上\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"batch without B", "a/x.go", "// TODO(tokun-2): 补上\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"leading zero", "a/x.go", "// TODO(tokun-B02): 补上\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"no colon", "a/x.go", "// TODO(tokun-B2) 补上\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"nothing to add", "a/x.go", "// TODO(tokun-B2):\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"no space after the token", "a/x.go", "//TODO(tokun-B2): 补上\n// 阶段：B1 放入\n", 1, "malformed placeholder TODO"},
		{"spaced marker", "a/x.go", "// TODO ( tokun-B2 ): 补上\n", 1, "malformed placeholder TODO"},
		{"trailing comment", "a/x.go", "\treturn true // TODO(tokun-B2): 补上\n", 1, "malformed placeholder TODO"},
		{"inside a block comment's body", "a/x.go", "/*\n * TODO(tokun-B2): 补上\n */\n", 2, "malformed placeholder TODO"},
		{"hash form in yaml shows its token", "k/x.yaml", "key: v # TODO(tokun-B1): 补上\n", 1, "# TODO(tokun-B<n>)"},
		{"stage line on its own", "a/x.go", "var x = 0\n// 阶段：B1 放入；B2-A1 完成时删除\n", 2, "no placeholder TODO directly above it"},
		{"stage line under a blank line", "a/x.go", "// TODO(tokun-B2): 补上\n\n// 阶段：B1 放入\n", 2, "must be its stage line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := check(c.file, c.src)
			if len(got) == 0 {
				t.Fatalf("no problem reported; want line %d %q", c.line, c.want)
			}
			if !strings.HasPrefix(got[0], fmt.Sprintf("%d: ", c.line)) || !strings.Contains(got[0], c.want) {
				t.Fatalf("got %q; want line %d containing %q", got, c.line, c.want)
			}
		})
	}
}

func TestOneMistakeIsOneProblem(t *testing.T) {
	// A stage line in the wrong form is reported as that, not also as a stage
	// line with no TODO above it.
	got, _ := check("a/x.go", "// TODO(tokun-B2): 补上\n/* 阶段：B1 放入 */\n")
	if len(got) != 1 {
		t.Fatalf("got %q, want exactly one problem", got)
	}
}

func TestClosedBatchKeepsNoPlaceholder(t *testing.T) {
	got, _ := check("a/x.go", goodGo, 1, 2)
	if len(got) != 1 || !strings.HasPrefix(got[0], "4: B2 is closed in tokun/BATCHES") {
		t.Fatalf("got %q, want B2 reported closed on line 4", got)
	}
	if got, _ := check("a/x.go", goodGo, 1, 3); len(got) != 0 {
		t.Fatalf("a placeholder of an open batch was reported: %q", got)
	}
}

func TestDataAndMentionsAreNotPlaceholders(t *testing.T) {
	cases := []struct{ name, file, src string }{
		{"string literal", "a/x.go", "var s = \"TODO(tokun-B2): 补上\"\n"},
		{"string literal with an escaped quote", "a/x.go", "var s = \"a \\\" // TODO(tokun-b2)\"\n"},
		{"raw string on one line", "a/x.go", "var re = regexp.MustCompile(`TODO\\(tokun-B`)\n"},
		{"python string", "a/x.py", "CASE = \"# TODO(tokun-b2): x\"\n"},
		{"mention in backticks", "a/x.go", "// The `TODO(tokun-B<n>)` marker names the batch that removes it.\n"},
		{"stage prefix in a string", "a/x.go", "t.Fatal(\"3.9 没有重新挂起 tokun 的 Flux 阶段：3.5\")\n"},
		{"stage prefix mentioned in a comment", "a/x.go", "// The second line starts with `阶段：`.\n"},
		{"after a closed block comment, code", "a/x.ts", "/* note */ const s = 'TODO(tokun-b2)';\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := check(c.file, c.src); len(got) != 0 {
				t.Fatalf("got %q, want nothing", got)
			}
		})
	}
}

func TestCommentForms(t *testing.T) {
	cases := []struct {
		name, src string
		want      []form
	}{
		{"a/x.go", "", slashForms},
		{"web/x.tsx", "", jsxForms},
		{"x.css", "", cssForms},
		{"a/x.sql", "", sqlForms},
		{"k/x.yml", "", hashForms},
		{"ops/local/tokun-e2e/ports.env", "", hashForms},
		{"a/BUILD", "", hashForms},
		{"a/.gitignore", "", hashForms},
		{"ops/local/tokun-e2e/up", "#!/usr/bin/env bash\n", hashForms},
		{"tokun/BATCHES", "B1\n", nil},
		{"AGENTS.md", "", nil},
		{"a/x.json", "", nil},
		{"web/index.html", "", nil},
		{"a/build.gradle.kts", "", nil},
	}
	for _, c := range cases {
		if got := commentForms(c.name, []byte(c.src)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("commentForms(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseBatches(t *testing.T) {
	cases := []struct {
		name, src string
		closed    map[int]bool
		problems  []string
	}{
		{"empty", "", map[int]bool{}, nil},
		{"one per line", "B0\nB1\nB3\n", map[int]bool{0: true, 1: true, 3: true}, nil},
		{"no final newline", "B1", map[int]bool{1: true}, nil},
		{"blank line", "B1\n\nB2\n", map[int]bool{1: true, 2: true}, []string{"2: \"\" is not a closed batch"}},
		{"lower case", "b1\n", map[int]bool{}, []string{"1: \"b1\" is not a closed batch"}},
		{"leading zero", "B01\n", map[int]bool{}, []string{"1: \"B01\" is not a closed batch"}},
		{"surrounding space", " B1\n", map[int]bool{}, []string{"1: \" B1\" is not a closed batch"}},
		{"listed twice", "B1\nB1\n", map[int]bool{1: true}, []string{"2: B1 is listed twice"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			closed, problems := parseBatches("tokun/BATCHES", []byte(c.src))
			if !reflect.DeepEqual(closed, c.closed) {
				t.Errorf("closed = %v, want %v", closed, c.closed)
			}
			var got []string
			for _, p := range problems {
				if p.path != "tokun/BATCHES" {
					t.Errorf("problem path %q", p.path)
				}
				got = append(got, fmt.Sprintf("%d: %s", p.line, p.msg))
			}
			if len(got) != len(c.problems) {
				t.Fatalf("problems = %q, want %q", got, c.problems)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], c.problems[i]) {
					t.Errorf("problem %d = %q, want prefix %q", i, got[i], c.problems[i])
				}
			}
		})
	}
}

// repo makes a git repository holding files; nothing is committed, so every
// file is untracked unless staged.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	for name, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runIn(root, batches string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(root, batches, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunPassesAndSummarises(t *testing.T) {
	dir := repo(t, map[string]string{
		"tokun/BATCHES":           "B1\n",
		"tokun/internal/a/a.go":   goodGo,
		"tokun/internal/a/b.go":   "package a\n\n" + goodStub + "var b = 0\n",
		"ops/local/tokun-e2e/run": "#!/usr/bin/env bash\n# TODO(tokun-B3): 补上\n# 阶段：B1 放入；B3-1 完成时删除\ntrue\n",
	})
	code, stdout, stderr := runIn(dir, "tokun/BATCHES")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if want := "todocheck: OK, 3 placeholder(s) (B2 2, B3 1); closed batches: B1\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunReportsEveryProblemWithItsPosition(t *testing.T) {
	dir := repo(t, map[string]string{
		"tokun/BATCHES":           "B2\nB2\n",
		"tokun/internal/a/a.go":   goodGo,
		"tokun/internal/a/bad.go": "package a\n\n// TODO(tokun-B3): 补上\nvar b = 0\n",
		// Ignored and documentation files are not read.
		".gitignore":            "ignored.go\n",
		"ignored.go":            "// todo(tokun-B9) ignored\n",
		"AGENTS.md":             "```go\n// TODO(tokun-B2): 例子\n```\n",
		"tokun/internal/a/x.go": "package a\n",
	})
	code, stdout, stderr := runIn(dir, "tokun/BATCHES")
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q stderr:\n%s", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	want := []string{
		"tokun/BATCHES:2: B2 is listed twice",
		"tokun/internal/a/a.go:4: B2 is closed in tokun/BATCHES",
		"tokun/internal/a/bad.go:4: the line under the placeholder TODO must be its stage line",
		"todocheck: 3 problem(s)",
	}
	if len(lines) != len(want) {
		t.Fatalf("stderr:\n%s\nwant %d lines", stderr, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], want[i])
		}
	}
}

func TestRunReportsMarkersInFilesOfUnknownSyntax(t *testing.T) {
	dir := repo(t, map[string]string{
		"tokun/BATCHES":      "",
		"web/index.html":     "<html>\n<!-- TODO(tokun-B2): 补上 -->\n<!-- 阶段：B1 放入；B2-A1 完成时删除 -->\n</html>\n",
		"a/build.gradle.kts": "// TODO(tokun-B3): 补上\n// 阶段：B1 放入；B3-1 完成时删除\n",
		"a/x.lua":            "local x = 1\n-- todo(tokun-b2) misspelt\n",
		"a/notes":            "plain text\nTODO(tokun-B4): 补上\n",
		// Markdown documents are not read, and a stage prefix alone is not a marker.
		"docs/x.mdx":        "```go\n// TODO(tokun-B2): 例子\n```\n",
		"docs/README.MD":    "TODO(tokun-B2)\n",
		"web/messages.json": "{\"stage\": \"阶段：一\"}\n",
	})
	code, stdout, stderr := runIn(dir, "tokun/BATCHES")
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q stderr:\n%s", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	want := []string{
		"a/build.gradle.kts:1: a placeholder marker in a .kts file, whose comment syntax todocheck does not know",
		"a/notes:2: a placeholder marker in notes, a file with no extension and no shebang, whose comment syntax todocheck does not know",
		"a/x.lua:2: a placeholder marker in a .lua file, whose comment syntax todocheck does not know",
		"web/index.html:2: a placeholder marker in a .html file, whose comment syntax todocheck does not know",
		"todocheck: 4 problem(s)",
	}
	if len(lines) != len(want) {
		t.Fatalf("stderr:\n%s\nwant %d lines", stderr, len(want))
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], want[i])
		}
	}
}

func TestRunReadsStagedTrackedFilesAndSkipsDeletedOnes(t *testing.T) {
	dir := repo(t, map[string]string{
		"tokun/BATCHES": "",
		"a.go":          "// TODO(tokun-B2) no colon\n",
		"gone.go":       "// TODO(tokun-B2) no colon\n",
	})
	git(t, dir, "add", "a.go", "gone.go")
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.go", filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runIn(dir, "tokun/BATCHES")
	if code != 1 || !strings.HasPrefix(stderr, "a.go:1: malformed placeholder TODO") || strings.Contains(stderr, "gone.go") || strings.Contains(stderr, "link.go") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestRunCannotRun(t *testing.T) {
	dir := repo(t, map[string]string{"a.go": "package a\n"})
	for _, c := range []struct{ name, root, batches, want string }{
		{"no -batches", dir, "", "-batches is required"},
		{"no batches file", dir, "tokun/BATCHES", "the batches file"},
		{"not a checkout", t.TempDir(), "BATCHES", "git ls-files"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "not a checkout" {
				if err := os.WriteFile(filepath.Join(c.root, "BATCHES"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code, _, stderr := runIn(c.root, c.batches)
			if code != 2 || !strings.Contains(stderr, c.want) {
				t.Fatalf("exit %d, stderr %q; want 2 and %q", code, stderr, c.want)
			}
		})
	}
}
