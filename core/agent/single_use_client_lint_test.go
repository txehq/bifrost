package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// singleUseClientsWithoutEarlyClose returns "file:line: name" for every client a function
// builds with clientForContext and calls Do on, without a `defer <name>.CloseIdleConnections()`
// before the first Do. clientForContext clones the transport, so nothing else ever closes
// that pool. A Do that returns an error returns before any later defer is registered,
// and credentialTransport turns a refused POST into exactly that error.
func singleUseClientsWithoutEarlyClose(fset *token.FileSet, file *ast.File) []string {
	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		clients := map[string]token.Pos{} // name -> where it was built
		firstDo := map[string]token.Pos{}
		earlyClose := map[string]token.Pos{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range node.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok {
						continue
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "clientForContext" && i < len(node.Lhs) {
						if id, ok := node.Lhs[0].(*ast.Ident); ok {
							clients[id.Name] = node.Pos()
						}
					}
				}
			case *ast.DeferStmt:
				if name, method := selectorCall(node.Call); method == "CloseIdleConnections" {
					if _, seen := earlyClose[name]; !seen {
						earlyClose[name] = node.Pos()
					}
				}
			case *ast.CallExpr:
				if name, method := selectorCall(node); method == "Do" {
					if _, seen := firstDo[name]; !seen {
						firstDo[name] = node.Pos()
					}
				}
			}
			return true
		})
		for name, built := range clients {
			do, calls := firstDo[name]
			if !calls {
				continue // a client that is stored, not called here, has another owner
			}
			if closed, ok := earlyClose[name]; !ok || closed > do {
				pos := fset.Position(built)
				found = append(found, filepath.Base(pos.Filename)+":"+strconv.Itoa(pos.Line)+": "+name)
			}
		}
	}
	return found
}

func selectorCall(call *ast.CallExpr) (receiver, method string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", sel.Sel.Name
	}
	return id.Name, sel.Sel.Name
}

// TestSingleUseClientsCloseBeforeDo holds every single-use client in this package to
// closing its pool on every return path (TXE-7743).
func TestSingleUseClientsCloseBeforeDo(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var found []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		require.NoError(t, err)
		found = append(found, singleUseClientsWithoutEarlyClose(fset, file)...)
	}
	require.Empty(t, found, "write `defer <client>.CloseIdleConnections()` right after clientForContext, before Do")
}

// TestSingleUseClientLintReportsALateClose proves the check above can fail, on the
// order main had before TXE-7743 and on a client that never closes.
func TestSingleUseClientLintReportsALateClose(t *testing.T) {
	src := `package agent
func late(m *Manager) {
	client, _ := m.clientForContext(nil, nil, nil, false)
	resp, err := client.Do(nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	defer client.CloseIdleConnections()
}
func never(m *Manager) {
	client, _ := m.clientForContext(nil, nil, nil, false)
	_, _ = client.Do(nil)
}
func early(m *Manager) {
	client, _ := m.clientForContext(nil, nil, nil, false)
	defer client.CloseIdleConnections()
	_, _ = client.Do(nil)
}
func stored(m *Manager) {
	client, _ := m.clientForContext(nil, nil, nil, false)
	m.httpClient = client
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x.go", src, 0)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"x.go:3: client", "x.go:12: client"}, singleUseClientsWithoutEarlyClose(fset, file))
}
