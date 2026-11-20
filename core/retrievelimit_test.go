package bifrost

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
)

// TXE-8031: every retrieve request type is read under the retrieve limit, and no other is. The
// expectation comes from each constant's Go name (FooRetrieveRequest), while the classification
// (RequestType.IsRetrieve) reads the wire value (foo_retrieve), so a retrieve type added later
// whose value is named otherwise fails here until it is read under the limit. A streamed retrieve
// is relayed as it arrives and is not held to it. The mark follows the request, not the context:
// a retrieve's mark left on a shared context is cleared by the next request that is not one.
func TestEveryRetrieveRequestTypeIsReadUnderTheRetrieveLimit(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "schemas/bifrost.go", nil, 0)
	if err != nil {
		t.Fatalf("parse schemas/bifrost.go: %v", err)
	}
	type declared struct {
		name  string
		value schemas.RequestType
	}
	var types []declared
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if ident, ok := spec.Type.(*ast.Ident); !ok || ident.Name != "RequestType" {
			return true
		}
		for i, value := range spec.Values {
			if lit, ok := value.(*ast.BasicLit); ok && i < len(spec.Names) {
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				types = append(types, declared{spec.Names[i].Name, schemas.RequestType(v)})
			}
		}
		return true
	})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	marked := func() bool {
		v, _ := ctx.Value(schemas.BifrostContextKeyRetrieveRequest).(bool)
		return v
	}
	retrieves := 0
	for _, rt := range types {
		want := strings.Contains(rt.name, "Retrieve") && !strings.Contains(rt.name, "Stream")
		if want {
			retrieves++
		}
		// From a context carrying the opposite mark, so a request that leaves it alone fails too.
		ctx.SetValue(schemas.BifrostContextKeyRetrieveRequest, !want)
		markRetrieve(ctx, rt.value)
		if got := marked(); got != want {
			t.Errorf("%s (%q): marked as a retrieve = %v, want %v", rt.name, rt.value, got, want)
		}
	}
	// The scan must see the retrieves this change was made for, or it is reading the wrong thing.
	if retrieves < 9 {
		t.Fatalf("the scan of schemas/bifrost.go found %d retrieve types in %d", retrieves, len(types))
	}
}

// TXE-8031, through the request worker: a batch retrieve whose job is past the retrieve limit is
// refused naming that limit, while a batch list of the same size, which is not a retrieve, is read.
// Before, both were read under the whole response limit.
func TestBatchRetrieveThroughBifrostIsReadUnderTheRetrieveLimit(t *testing.T) {
	network.SetMaxResponseBodySizeMB(4) // retrieve limit 1 MiB
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })

	results := strings.Repeat(`{"response":{"candidates":[{"content":{"parts":[{"text":"lorem ipsum"}]}}]}},`, (2<<20)/80)
	results = strings.TrimSuffix(results, ",")
	job := `{"name":"batches/big","metadata":{"name":"batches/big","state":"BATCH_STATE_SUCCEEDED","createTime":"2026-11-01T00:00:00Z","output":{"inlinedResponses":{"inlinedResponses":[` + results + `]}}},"done":true}`
	var retrieves, lists atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/batches/big") {
			retrieves.Add(1)
			_, _ = w.Write([]byte(job))
			return
		}
		lists.Add(1)
		_, _ = fmt.Fprintf(w, `{"operations":[%s]}`, job)
	}))
	defer ts.Close()

	account := NewMockAccount()
	account.AddProviderWithBaseURL(schemas.Gemini, 1, 4, ts.URL+"/v1beta")
	account.keys[schemas.Gemini][0].UseForBatchAPI = schemas.Ptr(true)
	client, initErr := Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: NewNoOpLogger()})
	if initErr != nil {
		t.Fatalf("init: %v", initErr)
	}
	defer client.Shutdown()

	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, bifrostErr := client.BatchRetrieveRequest(ctx, &schemas.BifrostBatchRetrieveRequest{Provider: schemas.Gemini, BatchID: "big"})
	if bifrostErr == nil || bifrostErr.Error == nil {
		t.Fatalf("a %d-byte retrieve was read under a 1 MiB retrieve limit", len(job))
	}
	if !strings.Contains(bifrostErr.Error.Message, "limit 1 MiB, for a retrieve") {
		t.Errorf("the refusal does not name the retrieve limit: %s", bifrostErr.Error.Message)
	}
	if got := retrieves.Load(); got != 1 {
		t.Errorf("the job was requested %d times, want once", got)
	}

	listCtx, listCancel := schemas.NewBifrostContextWithTimeout(context.Background(), 30*time.Second)
	defer listCancel()
	listed, bifrostErr := client.BatchListRequest(listCtx, &schemas.BifrostBatchListRequest{Provider: schemas.Gemini})
	if bifrostErr != nil {
		t.Fatalf("a batch list (not a retrieve) of the same size was refused: %+v", bifrostErr.Error)
	}
	if len(listed.Data) != 1 {
		t.Errorf("listed %d batches, want 1", len(listed.Data))
	}
}

// BenchmarkMarkRetrieve measures what the request worker pays per request to stamp the context:
// a chat request on a fresh context reads once and writes nothing.
func BenchmarkMarkRetrieve(b *testing.B) {
	for name, rt := range map[string]schemas.RequestType{
		"chat":           schemas.ChatCompletionRequest,
		"batch retrieve": schemas.BatchRetrieveRequest,
	} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				markRetrieve(ctx, rt)
			}
		})
	}
}
