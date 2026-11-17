package utils

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckBatchLineModel(t *testing.T) {
	t.Run("a line may name the batch's model in another spelling", func(t *testing.T) {
		for _, tc := range []struct {
			provider    schemas.ModelProvider
			line, batch string
		}{
			{schemas.Gemini, "gemini-2.5-flash", "publishers/google/models/gemini-2.5-flash"},
			{schemas.Gemini, "models/gemini-2.5-flash", "gemini-2.5-flash"},
			{schemas.Gemini, "gemini/gemini-2.5-flash", "gemini-2.5-flash"},
			{schemas.Gemini, "publishers/google/models/gemini-2.5-flash", "gemini-2.5-flash"},
			{schemas.Anthropic, "anthropic/claude-haiku-4-5", "claude-haiku-4-5"},
		} {
			item := schemas.BatchRequestItem{CustomID: "l", Body: map[string]interface{}{"model": tc.line}}
			assert.NoError(t, CheckBatchLineModel(item, tc.provider, tc.batch, nil), "%s vs %s", tc.line, tc.batch)
		}
	})
	t.Run("an alias the key serves as the batch's model is the batch's model", func(t *testing.T) {
		aliased := schemas.BatchRequestItem{CustomID: "l", Body: map[string]interface{}{"model": "fast"}}
		resolve := func(model string) string {
			if model == "fast" {
				return "gemini-2.5-flash"
			}
			return model
		}
		assert.NoError(t, CheckBatchLineModel(aliased, schemas.Gemini, "publishers/google/models/gemini-2.5-flash", resolve))
		assert.Error(t, CheckBatchLineModel(aliased, schemas.Gemini, "gemini-2.5-pro", resolve))
	})
	t.Run("another model is a line error naming body.model", func(t *testing.T) {
		item := schemas.BatchRequestItem{CustomID: "l", Body: map[string]interface{}{"model": "claude-opus-4-5"}}
		err := CheckBatchLineModel(item, schemas.Anthropic, "claude-haiku-4-5", nil)
		var lineErr *schemas.BatchLineError
		require.True(t, errors.As(err, &lineErr))
		assert.Equal(t, "body.model", lineErr.Param)
	})
}

func TestNewBatchLineBadRequestError(t *testing.T) {
	bifrostErr := NewBatchLineBadRequestError(3, &schemas.BatchLineError{CustomID: "c", Param: "url", Message: "bad url"})
	require.NotNil(t, bifrostErr.StatusCode)
	assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
	assert.Equal(t, "requests[3].url", bifrostErr.Error.Param)
	assert.Contains(t, bifrostErr.Error.Message, "batch request item 3")

	other := NewBatchLineBadRequestError(1, errors.New("boom"))
	assert.Nil(t, other.StatusCode, "an error that is not the caller's is not a 400")
}

// BenchmarkNewBatchLineBadRequestError measures the 400 built for a refused batch line, for
// both classifications: a field the batch cannot take and a malformed line.
func BenchmarkNewBatchLineBadRequestError(b *testing.B) {
	for name, unsupported := range map[string]bool{"unsupported_parameter": true, "invalid_request": false} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				lineErr := &schemas.BatchLineError{CustomID: "line-1", Param: "url", Unsupported: unsupported, Message: "url /v1/embeddings cannot run in a generateContent batch"}
				if bifrostErr := NewBatchLineBadRequestError(1, lineErr); bifrostErr.StatusCode == nil {
					b.Fatal("no status code")
				}
			}
		})
	}
}

// A batch at its provider's limit passes; one line over is a 400 caller_invalid_request on
// requests that names the limit; a provider with no limit fails closed (TXE-7696).
func TestCheckBatchLineLimit(t *testing.T) {
	for provider, limit := range batchLineLimits {
		require.Nil(t, CheckBatchLineLimit(provider, limit), "%s at its limit", provider)
		require.Nil(t, CheckBatchLineLimit(provider, 0), "%s with no inline lines", provider)

		bifrostErr := CheckBatchLineLimit(provider, limit+1)
		require.NotNil(t, bifrostErr, "%s one line over its limit", provider)
		require.NotNil(t, bifrostErr.StatusCode)
		assert.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
		assert.Equal(t, schemas.ErrorTypeCallerInvalidRequest, bifrostErr.ExtraFields.ErrorType)
		assert.Equal(t, "requests", bifrostErr.Error.Param)
		assert.Contains(t, bifrostErr.Error.Message, fmt.Sprintf("at most %d requests", limit))
		assert.Contains(t, bifrostErr.Error.Message, fmt.Sprintf("has %d", limit+1))
	}

	bifrostErr := CheckBatchLineLimit(schemas.ModelProvider("no-such-provider"), 1)
	require.NotNil(t, bifrostErr)
	assert.NotEqual(t, schemas.ErrorTypeCallerInvalidRequest, bifrostErr.ExtraFields.ErrorType, "a missing limit is the gateway's fault, not the caller's")
}

// Every provider's BatchCreate that reads its request calls CheckBatchLineLimit, naming a
// provider that has a limit, before it reads anything else from the request (a nil check
// aside). A new provider's BatchCreate, or a line conversion moved above the check, fails
// here, so no batch path converts lines the vendor would refuse (TXE-7696).
func TestEveryBatchCreateChecksTheLineLimitFirst(t *testing.T) {
	providerValues := modelProviderConstants(t)
	files, err := filepath.Glob(filepath.Join("..", "*", "*.go"))
	require.NoError(t, err)

	checked := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "BatchCreate" || fn.Body == nil {
				continue
			}
			where := fmt.Sprintf("%s: %s", path, fn.Name.Name)
			params := fn.Type.Params.List
			last := params[len(params)-1]
			if len(last.Names) == 0 || last.Names[0].Name == "_" {
				continue // a BatchCreate that never reads its request converts nothing
			}
			problem := batchCreateLimitProblem(fn, last.Names[0].Name, providerValues)
			if problem != "" {
				t.Errorf("%s: %s", where, problem)
				continue
			}
			checked[path] = true
		}
	}
	// The seven providers with a batch create today; the walk finding none would pass vacuously.
	require.GreaterOrEqual(t, len(checked), 7, "found BatchCreate in %v", checked)
}

// batchCreateLimitProblem says what is wrong with fn's use of CheckBatchLineLimit, or "".
func batchCreateLimitProblem(fn *ast.FuncDecl, request string, providerValues map[string]schemas.ModelProvider) string {
	var check *ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || check != nil {
			return check == nil
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckBatchLineLimit" {
			check = call
		}
		return true
	})
	if check == nil {
		return "reads its request but never calls providerUtils.CheckBatchLineLimit"
	}
	if len(check.Args) != 2 {
		return "CheckBatchLineLimit takes the provider and len(request.Requests)"
	}
	sel, ok := check.Args[0].(*ast.SelectorExpr)
	if !ok {
		return "CheckBatchLineLimit's provider is not a schemas constant"
	}
	provider, ok := providerValues[sel.Sel.Name]
	if !ok {
		return fmt.Sprintf("schemas.%s is not a ModelProvider constant", sel.Sel.Name)
	}
	if _, ok := BatchLineLimit(provider); !ok {
		return fmt.Sprintf("no batch line limit is set for %s in batchLineLimits", provider)
	}
	if want := "len(" + request + ".Requests)"; exprString(check.Args[1]) != want {
		return fmt.Sprintf("CheckBatchLineLimit counts %s, not %s", exprString(check.Args[1]), want)
	}

	// Nothing before the check may read the request, except a nil comparison.
	nilChecked := map[*ast.Ident]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if bin, ok := n.(*ast.BinaryExpr); ok && (bin.Op == token.EQL || bin.Op == token.NEQ) {
			for _, pair := range [][2]ast.Expr{{bin.X, bin.Y}, {bin.Y, bin.X}} {
				if id, ok := pair[0].(*ast.Ident); ok && id.Name == request {
					if other, ok := pair[1].(*ast.Ident); ok && other.Name == "nil" {
						nilChecked[id] = true
					}
				}
			}
		}
		return true
	})
	early := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && id.Name == request && id.Pos() < check.Pos() && !nilChecked[id] {
			early++
		}
		return true
	})
	if early > 0 {
		return fmt.Sprintf("reads %s before CheckBatchLineLimit (%d place(s)): the limit must come first", request, early)
	}
	return ""
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		args := make([]string, len(e.Args))
		for i, arg := range e.Args {
			args[i] = exprString(arg)
		}
		return exprString(e.Fun) + "(" + strings.Join(args, ", ") + ")"
	}
	return fmt.Sprintf("%T", expr)
}

// modelProviderConstants maps each schemas ModelProvider constant's name to its value.
func modelProviderConstants(t *testing.T) map[string]schemas.ModelProvider {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "schemas", "*.go"))
	require.NoError(t, err)
	out := map[string]schemas.ModelProvider{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, path)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				typ, ok := vs.Type.(*ast.Ident)
				if !ok || typ.Name != "ModelProvider" {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
							value, err := strconv.Unquote(lit.Value)
							require.NoError(t, err)
							out[name.Name] = schemas.ModelProvider(value)
						}
					}
				}
			}
		}
	}
	require.Contains(t, out, "Anthropic", "no ModelProvider constants parsed from core/schemas")
	return out
}
