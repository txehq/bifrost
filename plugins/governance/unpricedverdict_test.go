package governance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnpricedVerdictComesFromTheCostCalculation (TXE-8016) fails when governance decides an answer
// was unpriced from its $0 total instead of from the cost calculation that charged it. A $0 total is
// also what a free model, zero flex rates or a free fallback produce; only the calculation can tell
// those from an answer that met no rate (modelcatalog.AnswerPriceStatus).
//
// Across the package's non-test files:
//   - noteUnpricedAnswer takes no float64: it is handed a verdict, never a cost to judge.
//   - every call passes as its verdict either usagelessAnswerVerdict(...) (an answer with no usage at
//     all, judged by its row) or a variable every assignment of which is AnswerPriceStatus(...),
//     modelcatalog.PricePriced or modelcatalog.PriceUnpriced, with at least one AnswerPriceStatus.
//   - servedPriceStatus, which reads a whole row, is called only from usagelessAnswerVerdict.
func TestUnpricedVerdictComesFromTheCostCalculation(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	calls := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "noteUnpricedAnswer" {
				for _, field := range fn.Type.Params.List {
					if ident, ok := field.Type.(*ast.Ident); ok {
						require.NotEqual(t, "float64", ident.Name, "noteUnpricedAnswer takes a cost to judge; hand it a verdict (AnswerPriceStatus)")
					}
				}
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch calledName(call) {
				case "servedPriceStatus":
					require.Equal(t, "usagelessAnswerVerdict", fn.Name.Name,
						"%s calls servedPriceStatus (a whole row's status) at %s; judge an answer by AnswerPriceStatus", fn.Name.Name, fset.Position(call.Pos()))
				case "noteUnpricedAnswer":
					calls++
					require.GreaterOrEqual(t, len(call.Args), 5, fset.Position(call.Pos()).String())
					verdict := call.Args[4]
					if inner, ok := verdict.(*ast.CallExpr); ok && calledName(inner) == "usagelessAnswerVerdict" {
						return true
					}
					ident, ok := verdict.(*ast.Ident)
					require.True(t, ok, "noteUnpricedAnswer at %s is handed a verdict that is neither AnswerPriceStatus nor usagelessAnswerVerdict", fset.Position(call.Pos()))
					fromCalculation := false
					ast.Inspect(fn.Body, func(node ast.Node) bool {
						assign, ok := node.(*ast.AssignStmt)
						if !ok {
							return true
						}
						for i, lhs := range assign.Lhs {
							if target, ok := lhs.(*ast.Ident); !ok || target.Name != ident.Name || i >= len(assign.Rhs) {
								continue
							}
							switch rhs := assign.Rhs[i].(type) {
							case *ast.CallExpr:
								require.Equal(t, "AnswerPriceStatus", calledName(rhs), "the verdict at %s is assigned from %s", fset.Position(assign.Pos()), calledName(rhs))
								fromCalculation = true
							case *ast.SelectorExpr:
								// PricePriced for an answer that cost something; PriceUnpriced, failing closed, when
								// no catalog can read the answer. Never PriceFree from outside the calculation.
								require.Contains(t, []string{"PricePriced", "PriceUnpriced"}, rhs.Sel.Name, "the verdict at %s is the constant %s", fset.Position(assign.Pos()), rhs.Sel.Name)
							default:
								t.Fatalf("the verdict at %s is assigned from neither AnswerPriceStatus nor PricePriced", fset.Position(assign.Pos()))
							}
						}
						return true
					})
					require.True(t, fromCalculation, "the verdict noteUnpricedAnswer gets at %s is never read from AnswerPriceStatus", fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	require.Positive(t, calls, "noteUnpricedAnswer has no callers; the check reads nothing")
}

// calledName is the called function's or method's name.
func calledName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}
