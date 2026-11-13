package logstore

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The class check for TXE-7348 (and TXE-7315 before it). A HybridLogStore write that
// offloads a payload returns before its upload runs: a worker Puts the object, deletes a
// replaced copy and only then commits has_object. A test that reads after such a write
// without waiting for the upload passes on a fast machine and fails on a loaded CI
// runner. TestHybrid_CreateAndFindMCPToolLog waited on objStore.Len() == 1, which holds
// from the Put, and read has_object=false in shard 9.
//
// The rule, for every function in this package's _test.go files:
//   - a call to an upload-enqueuing method (uploadWriters, derived from the non-test
//     source, so a new write path is covered without editing a list) on a hybrid leaves
//     that hybrid pending;
//   - waitFor*(t, <that hybrid>, ...) or a non-deferred <hybrid>.Close clears it;
//   - any other call on a hybrid, on a store passed to or returned with one (the inner
//     LogStore, the object store), or a call that takes one of them as an argument,
//     while a hybrid is pending, is reported.
// Every waitFor* helper that takes a *HybridLogStore must call waitForUploads, which
// waits for outstandingUploads() == 0 whatever else it checks, so no wait can return
// mid-upload. A read that is meant to run before the upload settles (the test pins the
// in-flight state itself) carries `// upload-wait-ok: <reason>` on its line or the line
// above.

const uploadWaitOKMarker = "upload-wait-ok:"

var uploadWaitOK = regexp.MustCompile(regexp.QuoteMeta(uploadWaitOKMarker) + `\s*\S`)

// uploadSink is the method every upload goes through.
const uploadSink = "enqueueReplacingUpload"

// hybridConstructor matches the functions that build a hybrid store in tests.
var hybridConstructor = regexp.MustCompile(`^new(Test|Parquet)?\w*Hybrid\w*$`)

// notAStore names the constructor arguments that are not a store the hybrid reads.
var notAStore = map[string]bool{"t": true, "tb": true, "b": true, "ctx": true, "nil": true}

// uploadWriters returns the *HybridLogStore methods that reach uploadSink, from the
// package's non-test .go files in dir (or from src when it is not nil).
func uploadWriters(dir string, src map[string]string) (map[string]bool, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	if src != nil {
		for name, s := range src {
			f, err := parser.ParseFile(fset, name, s, 0)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
	} else {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, dir+"/"+e.Name(), nil, 0)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
	}
	calls := map[string]map[string]bool{} // method -> methods it calls on its receiver
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Body == nil || len(fd.Recv.List) != 1 || len(fd.Recv.List[0].Names) != 1 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "HybridLogStore" {
				continue
			}
			recv := fd.Recv.List[0].Names[0].Name
			callees := map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
						callees[sel.Sel.Name] = true
					}
				}
				return true
			})
			calls[fd.Name.Name] = callees
		}
	}
	writers := map[string]bool{uploadSink: true}
	for changed := true; changed; {
		changed = false
		for m, callees := range calls {
			if writers[m] {
				continue
			}
			for c := range callees {
				if writers[c] {
					writers[m] = true
					changed = true
					break
				}
			}
		}
	}
	if _, ok := calls[uploadSink]; !ok {
		return nil, fmt.Errorf("no (*HybridLogStore).%s in %s: update uploadSink", uploadSink, dir)
	}
	return writers, nil
}

// unwaitedHybridReads parses one test file (src, or the file at path when src is empty)
// and returns "path:line: what" for each read that follows an upload-enqueuing write with
// no wait between them, and for each waitFor* helper that takes a hybrid but does not go
// through waitForUploads.
func unwaitedHybridReads(path, src string, writers map[string]bool) ([]string, error) {
	fset := token.NewFileSet()
	var source any
	if src != "" {
		source = src
	}
	file, err := parser.ParseFile(fset, path, source, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	waived := map[int]bool{}
	for _, group := range file.Comments {
		for _, c := range group.List {
			if uploadWaitOK.MatchString(c.Text) {
				line := fset.Position(c.Slash).Line
				waived[line] = true
				waived[line+1] = true
			}
		}
	}
	var found []string
	report := func(pos token.Pos, what string) {
		p := fset.Position(pos)
		if !waived[p.Line] {
			found = append(found, fmt.Sprintf("%s:%d: %s", path, p.Line, what))
		}
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if strings.HasPrefix(fd.Name.Name, "waitFor") && fd.Name.Name != "waitForUploads" && takesHybrid(fd) {
			calls := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && strings.HasPrefix(id.Name, "waitFor") {
						calls = true
					}
				}
				return true
			})
			if !calls {
				report(fd.Pos(), fd.Name.Name+" takes a hybrid but does not wait through waitForUploads")
			}
		}
		lintHybridFunc(fd.Body, writers, report)
	}
	return found, nil
}

func takesHybrid(fd *ast.FuncDecl) bool {
	for _, field := range fd.Type.Params.List {
		if star, ok := field.Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "HybridLogStore" {
				return true
			}
		}
	}
	return false
}

type hybridEvent struct {
	pos    token.Pos
	kind   int // eventWrite, eventWait, eventRead
	hybrid string
	what   string
}

const (
	eventWrite = iota
	eventWait
	eventRead
)

func lintHybridFunc(body *ast.BlockStmt, writers map[string]bool, report func(token.Pos, string)) {
	hybrids := map[string]bool{} // first result of a hybrid constructor
	stores := map[string]bool{}  // every other value given to or returned by one
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !hybridConstructor.MatchString(id.Name) {
			return true
		}
		for i, lhs := range as.Lhs {
			if v, ok := lhs.(*ast.Ident); ok && v.Name != "_" {
				if i == 0 {
					hybrids[v.Name] = true
				} else {
					stores[v.Name] = true
				}
			}
		}
		for _, arg := range call.Args {
			if v, ok := arg.(*ast.Ident); ok && !notAStore[v.Name] {
				stores[v.Name] = true
			}
		}
		return true
	})
	if len(hybrids) == 0 {
		return
	}
	tracked := func(e ast.Expr) (string, bool) {
		v, ok := e.(*ast.Ident)
		if !ok {
			return "", false
		}
		return v.Name, hybrids[v.Name] || stores[v.Name]
	}
	var events []hybridEvent
	var skip func(n ast.Node) bool
	skip = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeferStmt:
			return false // runs at return, after every read
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && strings.HasPrefix(id.Name, "waitFor") {
				for _, arg := range x.Args {
					if name, ok := arg.(*ast.Ident); ok && hybrids[name.Name] {
						events = append(events, hybridEvent{pos: x.Pos(), kind: eventWait, hybrid: name.Name})
					}
				}
				return false // the reads inside the wait's condition are the wait
			}
			if id, ok := x.Fun.(*ast.Ident); ok && hybridConstructor.MatchString(id.Name) {
				return true
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if name, ok := tracked(sel.X); ok {
					method := sel.Sel.Name
					switch {
					case hybrids[name] && writers[method]:
						events = append(events, hybridEvent{pos: x.Pos(), kind: eventWrite, hybrid: name})
					case hybrids[name] && method == "Close":
						events = append(events, hybridEvent{pos: x.Pos(), kind: eventWait, hybrid: name})
					case strings.HasPrefix(method, "wait"):
						// A fake store's own synchronization (gate.waitEntered), not a read.
					default:
						events = append(events, hybridEvent{pos: x.Pos(), kind: eventRead, what: name + "." + method})
					}
					return true
				}
			}
			if id, ok := x.Fun.(*ast.Ident); ok {
				for _, arg := range x.Args {
					if name, ok := tracked(arg); ok {
						events = append(events, hybridEvent{pos: x.Pos(), kind: eventRead, what: id.Name + "(" + name + ")"})
						break
					}
				}
			}
		}
		return true
	}
	ast.Inspect(body, skip)
	sort.SliceStable(events, func(i, j int) bool { return events[i].pos < events[j].pos })
	pending := map[string]bool{}
	for _, ev := range events {
		switch ev.kind {
		case eventWrite:
			pending[ev.hybrid] = true
		case eventWait:
			delete(pending, ev.hybrid)
		case eventRead:
			if len(pending) > 0 {
				names := make([]string, 0, len(pending))
				for h := range pending {
					names = append(names, h)
				}
				sort.Strings(names)
				report(ev.pos, fmt.Sprintf("%s reads before %s's upload is waited for", ev.what, strings.Join(names, ", ")))
			}
		}
	}
}

// TestHybridReadsWaitForUploads is the class check (TXE-7348): no test in this package
// reads a hybrid store, its inner store or its object store after an upload-enqueuing
// write without waitFor*(t, hybrid, ...) or Close in between.
func TestHybridReadsWaitForUploads(t *testing.T) {
	writers, err := uploadWriters(".", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"CreateIfNotExists", "BatchCreateIfNotExists", "CreateMCPToolLog", "BatchUpdate"} {
		if !writers[m] {
			t.Fatalf("uploadWriters missed %s; the derivation from %s is broken", m, uploadSink)
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		found, err := unwaitedHybridReads(e.Name(), "", writers)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, found...)
	}
	if len(all) > 0 {
		t.Errorf("%d read(s) after a HybridLogStore write with no wait for its upload (TXE-7348). Call "+
			"waitForOffload / waitForMCPOffload / waitForUploads(t, hybrid, cond) / waitForUploadsSettled(t, hybrid) "+
			"first; they all wait for outstandingUploads() == 0. A read that pins the in-flight state needs "+
			"`// %s <reason>` on its line or the line above:\n  %s", len(all), uploadWaitOKMarker, strings.Join(all, "\n  "))
	}
}

// TestHybridUploadWaitLintFixture checks the lint on a fixture.
func TestHybridUploadWaitLintFixture(t *testing.T) {
	store := `package fixture

type HybridLogStore struct{}

func (h *HybridLogStore) enqueueReplacingUpload() {}
func (h *HybridLogStore) enqueueRawUpload()       { h.enqueueReplacingUpload() }
func (h *HybridLogStore) CreateMCPToolLog()       { h.enqueueRawUpload() }
func (h *HybridLogStore) FindMCPToolLog()         {}
`
	writers, err := uploadWriters("", map[string]string{"store.go": store})
	if err != nil {
		t.Fatal(err)
	}
	if !writers["CreateMCPToolLog"] || writers["FindMCPToolLog"] {
		t.Fatalf("uploadWriters() = %v, want CreateMCPToolLog and not FindMCPToolLog", writers)
	}
	src := `package fixture

func TestBad(t *testing.T) {
	hybrid, inner, objStore := newTestHybrid(t)
	defer hybrid.Close(ctx)
	hybrid.CreateMCPToolLog(ctx, entry)
	testtime.Eventually(t, d, "put", func() bool { return objStore.Len() == 1 })
	inner.FindMCPToolLog(ctx, "id")
	assertRow(t, inner)
	hybrid.FindMCPToolLog(ctx, "id") // upload-wait-ok: pins the in-flight read
}

func TestGood(t *testing.T) {
	hybrid, inner, objStore := newTestHybrid(t)
	other := newHybridLogStore(inner, objStore)
	inner.FindMCPToolLog(ctx, "before any write")
	hybrid.CreateMCPToolLog(ctx, entry)
	waitForMCPOffload(t, hybrid, inner, "id")
	inner.FindMCPToolLog(ctx, "id")
	other.CreateMCPToolLog(ctx, entry)
	waitForUploads(t, hybrid, func() bool { return objStore.Len() == 1 })
	objStore.Len()
	other.Close(ctx)
	objStore.Len()
}

func waitForSomething(t *testing.T, h *HybridLogStore) {
	testtime.Eventually(t, d, "x", func() bool { return true })
}
`
	got, err := unwaitedHybridReads("fixture_test.go", src, writers)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		// A raw Eventually on an object-store condition is the TXE-7348 bug: not a wait.
		"fixture_test.go:7: objStore.Len reads before hybrid's upload is waited for",
		"fixture_test.go:8: inner.FindMCPToolLog reads before hybrid's upload is waited for",
		"fixture_test.go:9: assertRow(inner) reads before hybrid's upload is waited for",
		"fixture_test.go:22: objStore.Len reads before other's upload is waited for",
		"fixture_test.go:27: waitForSomething takes a hybrid but does not wait through waitForUploads",
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unwaitedHybridReads() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
