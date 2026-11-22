package schemas

import (
	"reflect"
	"strings"
	"testing"
)

// cacheControlStep is one step on the way from a request to a *CacheControl: a struct field, or
// the first element of a slice.
type cacheControlStep struct {
	name  string
	field []int // nil for a slice element
}

// cacheControlPaths lists every path from root to a *CacheControl field, through struct fields
// (embedded ones included), pointers and slices. A type already on the path is not entered again,
// so a recursive type ends the path there.
func cacheControlPaths(root reflect.Type) [][]cacheControlStep {
	ccType := reflect.TypeOf((*CacheControl)(nil))
	var paths [][]cacheControlStep
	var walk func(t reflect.Type, path []cacheControlStep, onPath map[reflect.Type]bool)
	walk = func(t reflect.Type, path []cacheControlStep, onPath map[reflect.Type]bool) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Slice:
			walk(t.Elem(), append(path[:len(path):len(path)], cacheControlStep{name: "[0]"}), onPath)
			return
		case reflect.Struct:
		default:
			return
		}
		if onPath[t] {
			return
		}
		onPath[t] = true
		defer delete(onPath, t)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			step := append(path[:len(path):len(path)], cacheControlStep{name: f.Name, field: f.Index})
			if f.Type == ccType {
				paths = append(paths, step)
				continue
			}
			walk(f.Type, step, onPath)
		}
	}
	walk(root, nil, map[reflect.Type]bool{})
	return paths
}

// requestWithCacheControlAt builds a request of type root whose only cache_control is the one at
// path, with ttl.
func requestWithCacheControlAt(root reflect.Type, path []cacheControlStep, ttl string) reflect.Value {
	req := reflect.New(root)
	v := req.Elem()
	for _, step := range path {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		if step.field == nil {
			if v.Len() == 0 {
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			}
			v = v.Index(0)
			continue
		}
		v = v.FieldByIndex(step.field)
	}
	v.Set(reflect.ValueOf(&CacheControl{Type: CacheControlTypeEphemeral, TTL: &ttl}))
	return req
}

func pathName(path []cacheControlStep) string {
	var b strings.Builder
	for _, s := range path {
		if s.field != nil {
			b.WriteString(".")
		}
		b.WriteString(s.name)
	}
	return b.String()
}

// TestRequestedCacheControlTTLReadsEveryCacheControl: RequestedCacheControlTTL decides whether
// the request path reads cache_control.ttl from the body it sends, so a cache_control it does
// not walk is a 1h write priced at the base rate (fork, TXE-8086). The test finds every
// *CacheControl a chat or Responses request can carry, sets only that one, and fails when it is
// not read. A new CacheControl field fails here until the walk reads it.
func TestRequestedCacheControlTTLReadsEveryCacheControl(t *testing.T) {
	for _, c := range []struct {
		root reflect.Type
		wrap func(reflect.Value) *BifrostRequest
	}{
		{reflect.TypeOf(BifrostChatRequest{}), func(v reflect.Value) *BifrostRequest {
			return &BifrostRequest{ChatRequest: v.Interface().(*BifrostChatRequest)}
		}},
		{reflect.TypeOf(BifrostResponsesRequest{}), func(v reflect.Value) *BifrostRequest {
			return &BifrostRequest{ResponsesRequest: v.Interface().(*BifrostResponsesRequest)}
		}},
	} {
		paths := cacheControlPaths(c.root)
		if len(paths) == 0 {
			t.Fatalf("%s: found no cache_control", c.root.Name())
		}
		for _, path := range paths {
			name := c.root.Name() + pathName(path)
			req := c.wrap(requestWithCacheControlAt(c.root, path, "1h"))
			if got := req.RequestedCacheControlTTL(); got != "1h" {
				t.Errorf("%s with cache_control.ttl \"1h\": RequestedCacheControlTTL() = %q; read it there", name, got)
			}
		}
	}
}

// A request whose blocks carry different TTLs is reported by the vendor as one write figure, so
// it is priced at the longest TTL any block asked for: never below the tier the vendor may have
// billed (fork, TXE-8086). An unparseable TTL ranks below any duration.
func TestRequestedCacheControlTTLIsTheLongestBlockTTL(t *testing.T) {
	ttl := func(s string) *CacheControl { return &CacheControl{Type: CacheControlTypeEphemeral, TTL: &s} }
	mixed := &BifrostRequest{ChatRequest: &BifrostChatRequest{
		Input: []ChatMessage{
			{Content: &ChatMessageContent{ContentBlocks: []ChatContentBlock{{CacheControl: ttl("1h")}}}},
			{Content: &ChatMessageContent{ContentBlocks: []ChatContentBlock{{CacheControl: ttl("5m")}, {CacheControl: &CacheControl{Type: CacheControlTypeEphemeral}}}}},
		},
	}}
	if got := mixed.RequestedCacheControlTTL(); got != "1h" {
		t.Errorf("1h and 5m blocks: got %q, want 1h", got)
	}
	untimed := &BifrostRequest{ChatRequest: &BifrostChatRequest{Params: &ChatParameters{CacheControl: &CacheControl{Type: CacheControlTypeEphemeral}}}}
	if got := untimed.RequestedCacheControlTTL(); got != "" {
		t.Errorf("cache_control without a ttl: got %q, want \"\"", got)
	}
	if got := (*BifrostRequest)(nil).RequestedCacheControlTTL(); got != "" {
		t.Errorf("nil request: got %q", got)
	}
	for _, c := range []struct{ a, b, want string }{
		{"", "", ""}, {"1h", "", "1h"}, {"", "5m", "5m"}, {"5m", "1h", "1h"}, {"1h", "5m", "1h"},
		{"forever", "5m", "5m"}, {"5m", "forever", "5m"}, {"forever", "", "forever"}, {"12h", "1h", "12h"},
	} {
		if got := LongerCacheTTL(c.a, c.b); got != c.want {
			t.Errorf("LongerCacheTTL(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestRequestedCacheControlTTLKeyIsStable(t *testing.T) {
	if BifrostContextKeyRequestedCacheControlTTL != BifrostContextKey("bifrost-requested-cache-control-ttl") {
		t.Fatalf("BifrostContextKeyRequestedCacheControlTTL = %q", BifrostContextKeyRequestedCacheControlTTL)
	}
}
