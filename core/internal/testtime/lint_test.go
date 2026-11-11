package testtime

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// threshold is the shortest literal timing a test may write without Scale. A timeout of
// two seconds or more has room for a loaded runner; one under it does not (TXE-7128: a
// 300 ms idle timeout and a 100 ms lease each failed a PR that changed no Go code).
const threshold = 2 * time.Second

// lintRoots are the module trees whose tests run in the Go unit tests job.
var lintRoots = []string{"core", "framework", "transports", "plugins"}

// timingName matches the names that say a duration bounds how long something may take or
// last: a variable, constant, struct field or parameter so named, or a function so named
// that takes the duration.
var timingName = regexp.MustCompile(`(?i)(timeout|ttl|deadline|expir|idle|lease)`)

// waiver lets one line keep a literal, with a reason: `// literal-timing: <reason>` on the
// line or the line above. Use it for a duration that must not grow, such as a timeout a
// test needs to fire before a fixed upstream delay that is not scaled.
var waiver = regexp.MustCompile(`literal-timing:\s*\S`)

// eventuallyFuncs take (condition, waitFor, tick): waitFor is a timeout.
var eventuallyFuncs = map[string]bool{
	"Eventually": true, "Eventuallyf": true, "EventuallyWithT": true, "EventuallyWithTf": true,
	"Never": true, "Neverf": true,
}

var timeUnits = map[string]float64{
	"Nanosecond": float64(time.Nanosecond), "Microsecond": float64(time.Microsecond),
	"Millisecond": float64(time.Millisecond), "Second": float64(time.Second),
	"Minute": float64(time.Minute), "Hour": float64(time.Hour),
}

type finding struct {
	pos  token.Position
	what string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.pos.Filename, f.pos.Line, f.what)
}

// value is the result of evaluating an expression as a constant.
type value struct {
	n   float64 // nanoseconds when dur, else a plain number
	dur bool
}

// pkgInfo holds what one directory's files declare, for resolving names across files.
type pkgInfo struct {
	consts map[string]ast.Expr   // package-level const and var with a value
	params map[string][][]string // function or method name -> parameter names, by position
}

// fileLint walks one file.
type fileLint struct {
	fset     *token.FileSet
	pkg      *pkgInfo
	timePkg  string
	ctxPkg   string
	testify  map[string]bool // local names of testify's assert and require
	waived   map[int]bool    // lines a waiver covers
	scopes   []map[string]ast.Expr
	findings []finding
	seen     map[token.Pos]bool
}

func TestNoLiteralShortTimingsInTests(t *testing.T) {
	root := repoRoot(t)
	var all []finding
	for _, r := range lintRoots {
		dirs, err := testDirs(filepath.Join(root, r))
		if err != nil {
			t.Fatal(err)
		}
		for _, dir := range dirs {
			fs, err := lintDir(dir, func(name string) bool { return strings.HasSuffix(name, "_test.go") })
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, fs...)
		}
	}
	if len(all) == 0 {
		return
	}
	var b strings.Builder
	for _, f := range all {
		rel, _ := filepath.Rel(root, f.pos.Filename)
		fmt.Fprintf(&b, "\n  %s:%d: %s", rel, f.pos.Line, f.what)
	}
	t.Errorf("%d literal timing(s) under %s in tests; write testtime.Scale(<duration>) from <module>/internal/testtime "+
		"(or `// literal-timing: <reason>` where the duration must not grow):%s", len(all), threshold, b.String())
}

// TestLintFixtures checks the lint against testdata/lint: every line ending in `// want`
// is reported and no other line is.
func TestLintFixtures(t *testing.T) {
	dir := filepath.Join("testdata", "lint")
	got, err := lintDir(dir, func(name string) bool { return strings.HasSuffix(name, ".go") })
	if err != nil {
		t.Fatal(err)
	}
	gotLines := map[string]bool{}
	for _, f := range got {
		gotLines[fmt.Sprintf("%s:%d", filepath.Base(f.pos.Filename), f.pos.Line)] = true
	}
	wantLines := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasSuffix(strings.TrimSpace(line), "// want") {
				wantLines[fmt.Sprintf("%s:%d", e.Name(), i+1)] = true
			}
		}
	}
	if len(wantLines) == 0 {
		t.Fatal("no `// want` lines in testdata/lint")
	}
	for l := range wantLines {
		if !gotLines[l] {
			t.Errorf("%s: not reported", l)
		}
	}
	for _, f := range got {
		l := fmt.Sprintf("%s:%d", filepath.Base(f.pos.Filename), f.pos.Line)
		if !wantLines[l] {
			t.Errorf("%s: reported but not marked `// want`: %s", l, f.what)
		}
	}
}

// TestCopiesAreIdentical: every module's internal/testtime/testtime.go is this file, so
// the factor means the same everywhere.
func TestCopiesAreIdentical(t *testing.T) {
	root := repoRoot(t)
	canonical, err := os.ReadFile("testtime.go")
	if err != nil {
		t.Fatal(err)
	}
	copies := 0
	for _, r := range lintRoots {
		err := filepath.WalkDir(filepath.Join(root, r), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			if d.IsDir() || filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "core", "internal", "testtime", "testtime.go")) {
				return nil
			}
			if !strings.HasSuffix(filepath.ToSlash(path), "/internal/testtime/testtime.go") {
				return nil
			}
			copies++
			got, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, canonical) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s differs from core/internal/testtime/testtime.go; copy it unchanged", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d copies checked", copies)
}

func TestParse(t *testing.T) {
	for raw, want := range map[string]float64{"": 1, "1": 1, "3": 3, "2.5": 2.5} {
		got, err := parse(raw)
		if err != nil || got != want {
			t.Errorf("parse(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"0", "0.5", "-2", "x", "NaN", "Inf", "3s"} {
		if _, err := parse(raw); err == nil {
			t.Errorf("parse(%q) accepted", raw)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.work above the working directory")
		}
		dir = parent
	}
}

func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" || (strings.HasPrefix(name, ".") && name != ".")
}

// testDirs lists the directories under root that hold a _test.go file.
func testDirs(root string) ([]string, error) {
	set := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			set[filepath.Dir(path)] = true
		}
		return nil
	})
	dirs := make([]string, 0, len(set))
	for d := range set {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, err
}

// lintDir reports the literal short timings in the files of dir that lint selects. Every
// .go file in dir is read for the names it declares, so a constant declared in one file and
// used in another resolves.
func lintDir(dir string, lint func(name string) bool) ([]finding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	pkg := &pkgInfo{consts: map[string]ast.Expr{}, params: map[string][][]string{}}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		collect(pkg, f)
		if lint(e.Name()) {
			files = append(files, f)
		}
	}
	var out []finding
	for _, f := range files {
		out = append(out, lintFile(fset, pkg, f)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pos.Filename != out[j].pos.Filename {
			return out[i].pos.Filename < out[j].pos.Filename
		}
		return out[i].pos.Line < out[j].pos.Line
	})
	return out, nil
}

func collect(pkg *pkgInfo, f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.CONST && d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs := spec.(*ast.ValueSpec)
				if len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, n := range vs.Names {
					pkg.consts[n.Name] = vs.Values[i]
				}
			}
		case *ast.FuncDecl:
			pkg.params[d.Name.Name] = append(pkg.params[d.Name.Name], paramNames(d.Type))
		}
	}
}

func paramNames(ft *ast.FuncType) []string {
	var names []string
	for _, field := range ft.Params.List {
		if len(field.Names) == 0 {
			names = append(names, "")
		}
		for _, n := range field.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

func lintFile(fset *token.FileSet, pkg *pkgInfo, f *ast.File) []finding {
	l := &fileLint{fset: fset, pkg: pkg, testify: map[string]bool{}, waived: map[int]bool{}, seen: map[token.Pos]bool{}}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch path {
		case "time":
			l.timePkg = name
		case "context":
			l.ctxPkg = name
		case "github.com/stretchr/testify/assert", "github.com/stretchr/testify/require":
			l.testify[name] = true
		}
	}
	if l.timePkg == "" {
		return nil // no time import: no duration literal to find
	}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if waiver.MatchString(c.Text) {
				line := fset.Position(c.Pos()).Line
				l.waived[line] = true
				l.waived[line+1] = true
			}
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			l.walk(d)
		case *ast.FuncDecl:
			if d.Body != nil {
				l.scopes = append(l.scopes, localDefs(d.Body))
				l.walk(d.Body)
				l.scopes = l.scopes[:len(l.scopes)-1]
			}
		}
	}
	return l.findings
}

// localDefs maps each name defined exactly once in body (x := e, var x = e, const x = e)
// and never assigned again to its expression. A name assigned twice is not a constant.
func localDefs(body *ast.BlockStmt) map[string]ast.Expr {
	defs := map[string]ast.Expr{}
	count := map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				count[id.Name]++
				if s.Tok == token.DEFINE && len(s.Lhs) == len(s.Rhs) {
					defs[id.Name] = s.Rhs[i]
				}
			}
		case *ast.ValueSpec:
			for i, id := range s.Names {
				count[id.Name]++
				if len(s.Names) == len(s.Values) {
					defs[id.Name] = s.Values[i]
				}
			}
		case *ast.IncDecStmt:
			if id, ok := s.X.(*ast.Ident); ok {
				count[id.Name] += 2
			}
		}
		return true
	})
	for name, c := range count {
		if c != 1 {
			delete(defs, name)
		}
	}
	return defs
}

func (l *fileLint) walk(root ast.Node) {
	ast.Inspect(root, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			l.call(x)
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && timingName.MatchString(id.Name) {
				l.check(x.Value, "field "+id.Name)
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, lhs := range x.Lhs {
					if name := exprName(lhs); name != "" && timingName.MatchString(name) {
						l.check(x.Rhs[i], name)
					}
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i, id := range x.Names {
					if timingName.MatchString(id.Name) {
						l.check(x.Values[i], id.Name)
					}
				}
			}
		}
		return true
	})
}

func (l *fileLint) call(c *ast.CallExpr) {
	var recv ast.Expr
	name := ""
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		recv, name = fn.X, fn.Sel.Name
	default:
		return
	}
	pkgName := ""
	if id, ok := recv.(*ast.Ident); ok {
		pkgName = id.Name
	}
	switch {
	case pkgName == l.timePkg && (name == "After" || name == "NewTimer" || name == "AfterFunc"):
		l.checkArg(c, 0, "time."+name)
		return
	case pkgName != "" && pkgName == l.ctxPkg && name == "WithTimeout":
		l.checkArg(c, 1, "context.WithTimeout")
		return
	case eventuallyFuncs[name]:
		idx := 1
		if l.testify[pkgName] {
			idx = 2
		}
		l.checkArg(c, idx, name+" waitFor")
		return
	case name == "Add" && isTimeNow(recv, l.timePkg):
		l.checkArg(c, 0, "time.Now().Add")
		return
	case timingName.MatchString(name):
		for i := range c.Args {
			l.checkArg(c, i, "argument of "+name)
		}
		return
	}
	for _, params := range l.pkg.params[name] {
		for i := range c.Args {
			p := ""
			if i < len(params) {
				p = params[i]
			} else if len(params) > 0 {
				p = params[len(params)-1]
			}
			if p != "" && timingName.MatchString(p) {
				l.checkArg(c, i, fmt.Sprintf("parameter %s of %s", p, name))
			}
		}
	}
}

func isTimeNow(e ast.Expr, timePkg string) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == timePkg && sel.Sel.Name == "Now"
}

func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

func (l *fileLint) checkArg(c *ast.CallExpr, i int, what string) {
	if i < len(c.Args) {
		l.check(c.Args[i], what)
	}
}

// check reports e when it is a constant duration above zero and under the threshold.
func (l *fileLint) check(e ast.Expr, what string) {
	v, ok := l.eval(e, 0)
	if !ok || !v.dur || v.n <= 0 || v.n >= float64(threshold) {
		return
	}
	pos := l.fset.Position(e.Pos())
	if l.waived[pos.Line] || l.seen[e.Pos()] {
		return
	}
	l.seen[e.Pos()] = true
	l.findings = append(l.findings, finding{pos: pos, what: fmt.Sprintf("%s is %s", what, time.Duration(v.n))})
}

func (l *fileLint) lookup(name string) (ast.Expr, bool) {
	for i := len(l.scopes) - 1; i >= 0; i-- {
		if e, ok := l.scopes[i][name]; ok {
			return e, true
		}
	}
	e, ok := l.pkg.consts[name]
	return e, ok
}

func (l *fileLint) eval(e ast.Expr, depth int) (value, bool) {
	if depth > 20 {
		return value{}, false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.INT && x.Kind != token.FLOAT {
			return value{}, false
		}
		n, err := strconv.ParseFloat(strings.ReplaceAll(x.Value, "_", ""), 64)
		if err != nil {
			i, err := strconv.ParseInt(strings.ReplaceAll(x.Value, "_", ""), 0, 64)
			if err != nil {
				return value{}, false
			}
			n = float64(i)
		}
		return value{n: n}, true
	case *ast.Ident:
		def, ok := l.lookup(x.Name)
		if !ok {
			return value{}, false
		}
		return l.eval(def, depth+1)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == l.timePkg {
			if u, ok := timeUnits[x.Sel.Name]; ok {
				return value{n: u, dur: true}, true
			}
		}
		return value{}, false
	case *ast.ParenExpr:
		return l.eval(x.X, depth+1)
	case *ast.UnaryExpr:
		v, ok := l.eval(x.X, depth+1)
		if !ok {
			return value{}, false
		}
		switch x.Op {
		case token.SUB:
			v.n = -v.n
			return v, true
		case token.ADD:
			return v, true
		}
		return value{}, false
	case *ast.BinaryExpr:
		a, ok := l.eval(x.X, depth+1)
		if !ok {
			return value{}, false
		}
		b, ok := l.eval(x.Y, depth+1)
		if !ok {
			return value{}, false
		}
		switch x.Op {
		case token.MUL:
			return value{n: a.n * b.n, dur: a.dur || b.dur}, true
		case token.QUO:
			if b.n == 0 {
				return value{}, false
			}
			return value{n: a.n / b.n, dur: a.dur && !b.dur}, true
		case token.ADD:
			return value{n: a.n + b.n, dur: a.dur || b.dur}, true
		case token.SUB:
			return value{n: a.n - b.n, dur: a.dur || b.dur}, true
		}
		return value{}, false
	case *ast.CallExpr:
		// time.Duration(n) converts; any other call is not a constant.
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && len(x.Args) == 1 {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == l.timePkg && sel.Sel.Name == "Duration" {
				v, ok := l.eval(x.Args[0], depth+1)
				if !ok {
					return value{}, false
				}
				v.dur = true
				return v, true
			}
		}
		return value{}, false
	}
	return value{}, false
}
