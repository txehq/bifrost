package bifrost

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/network"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// StopKeyWalk stops at the response-limit refusal, however a provider built it, and at nothing
// else: a key's own failure (a 403, a network error) still moves the walk to the next key.
func TestStopKeyWalk(t *testing.T) {
	tooLarge := &network.ResponseBodyTooLargeError{Limit: 1 << 20}
	for name, tc := range map[string]struct {
		err  *schemas.BifrostError
		stop bool
	}{
		"no error yet":          {nil, false},
		"the refusal":           {providerUtils.NewResponseBodyTooLargeError(tooLarge), true},
		"a wrapped read error":  {providerUtils.NewBifrostOperationError("error reading S3 GET response", tooLarge), true},
		"a vendor 403":          {providerUtils.NewProviderAPIError("S3 GET failed: AccessDenied", nil, 403, nil, nil), false},
		"a network error":       {providerUtils.NewBifrostOperationError(schemas.ErrProviderDoRequest, errors.New("dial tcp: connection refused")), false},
		"a configuration error": {providerUtils.NewConfigurationError("endpoint not set"), false},
	} {
		if got := providerUtils.StopKeyWalk(tc.err); got != tc.stop {
			t.Errorf("%s: StopKeyWalk = %t, want %t", name, got, tc.stop)
		}
	}
}

// BenchmarkStopKeyWalk is what TXE-7301 adds to each iteration of a key walk: a nil check before
// the first key, and an errors.Is after a key failed.
func BenchmarkStopKeyWalk(b *testing.B) {
	failed := providerUtils.NewProviderAPIError("S3 GET failed: AccessDenied", nil, 403, nil, nil)
	b.Run("first_key", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = providerUtils.StopKeyWalk(nil)
		}
	})
	b.Run("after_a_failed_key", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = providerUtils.StopKeyWalk(failed)
		}
	})
}

// TXE-7301: no provider walks on to another key after an answer past the response limit
// (client.max_response_body_size_mb). The next key would download the same object again, up to
// the limit, and a later key's error would replace the bifrost_internal refusal the caller is
// owed. Every loop over keys that records a per-key error in lastErr must open with
//
//	if providerUtils.StopKeyWalk(lastErr) {
//		break
//	}
//
// so a loop added later without it fails here, by file and line.
func TestEveryKeyWalkStopsAtAResponseLimitRefusal(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir("providers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "utils" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			loop, ok := n.(*ast.RangeStmt)
			if !ok || !rangesOverKeys(loop.X) || !assignsLastErr(loop.Body) {
				return true
			}
			checked++
			if !opensWithStopKeyWalk(loop.Body) {
				t.Errorf("%s: a loop over keys records lastErr but does not open with `if providerUtils.StopKeyWalk(lastErr) { break }`", fset.Position(loop.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan providers: %v", err)
	}
	if checked < 50 {
		t.Fatalf("found %d key-walk loops; the scan no longer sees them", checked)
	}
}

func rangesOverKeys(x ast.Expr) bool {
	ident, ok := x.(*ast.Ident)
	return ok && strings.HasSuffix(strings.ToLower(ident.Name), "keys")
}

func assignsLastErr(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == "lastErr" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func opensWithStopKeyWalk(body *ast.BlockStmt) bool {
	if len(body.List) == 0 {
		return false
	}
	guard, ok := body.List[0].(*ast.IfStmt)
	if !ok || guard.Init != nil || guard.Else != nil || len(guard.Body.List) != 1 {
		return false
	}
	call, ok := guard.Cond.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "StopKeyWalk" {
		return false
	}
	if arg, ok := call.Args[0].(*ast.Ident); !ok || arg.Name != "lastErr" {
		return false
	}
	branch, ok := guard.Body.List[0].(*ast.BranchStmt)
	return ok && branch.Tok == token.BREAK && branch.Label == nil
}
