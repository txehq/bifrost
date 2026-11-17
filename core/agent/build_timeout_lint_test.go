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

// buildTimeoutSetter is the only function in this package's tests allowed to assign
// Manager.buildTimeout.
const buildTimeoutSetter = "withExpiringBuilds"

// buildTimeoutAssignments returns "file:line" for every assignment to a buildTimeout field
// outside buildTimeoutSetter in the given Go source.
func buildTimeoutAssignments(fset *token.FileSet, file *ast.File) []string {
	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == buildTimeoutSetter && fn.Recv == nil {
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "buildTimeout" {
					pos := fset.Position(assign.Pos())
					found = append(found, filepath.Base(pos.Filename)+":"+strconv.Itoa(pos.Line))
				}
			}
			return true
		})
	}
	return found
}

// TestBuildTimeoutIsSetOnlyThroughWithExpiringBuilds keeps a short build timeout scoped
// to the call that is meant to time out (TXE-7743). A test that set m.buildTimeout to
// 20 ms for its whole body also ran its success path under that deadline, and failed
// whenever a loaded -race run took longer than 20 ms to fetch a card.
func TestBuildTimeoutIsSetOnlyThroughWithExpiringBuilds(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var found []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		require.NoError(t, err)
		found = append(found, buildTimeoutAssignments(fset, file)...)
	}
	require.Empty(t, found, "assign Manager.buildTimeout only through %s, which restores the production timeout when the expiring call returns", buildTimeoutSetter)
}

// TestBuildTimeoutLintReportsADirectAssignment proves the check above can fail.
func TestBuildTimeoutLintReportsADirectAssignment(t *testing.T) {
	src := `package agent
func withExpiringBuilds(m *Manager, fn func()) { m.buildTimeout = 1; fn() }
func TestX(m *Manager) {
	m.buildTimeout = 1
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "x_test.go", src, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"x_test.go:4"}, buildTimeoutAssignments(fset, file))
}
