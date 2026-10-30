package otel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/agent"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/tracing"
	"github.com/stretchr/testify/require"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func makeSpan(id, parentID, name string, kind schemas.SpanKind) *schemas.Span {
	return &schemas.Span{
		SpanID:    id,
		ParentID:  parentID,
		Name:      name,
		Kind:      kind,
		StartTime: time.Now(),
		EndTime:   time.Now(),
	}
}

// TestConvertTraceToResourceSpan_PluginSpanFilter exercises the OTEL converter's end-to-end
// filtering behavior (the parts unique to this package; the filter/reparent logic itself is
// covered by core/schemas/span_filter_test.go). It asserts that filtered plugin spans are
// dropped from the exported ResourceSpan and that an exported child whose direct parent was
// filtered is re-parented to the nearest exported ancestor.
func TestConvertTraceToResourceSpan_PluginSpanFilter(t *testing.T) {
	p := &OtelPlugin{pluginSpanFilter: &PluginSpanFilter{
		Mode:    PluginSpanFilterModeExclude,
		Plugins: []string{"logging"},
	}}

	// Span tree: root (internal) -> logging.prehook (filtered) -> governance.prehook (kept).
	root := makeSpan("aaaa", "", "request", schemas.SpanKindInternal)
	trace := &schemas.Trace{
		TraceID:  "00000000000000000000000000000001",
		RootSpan: root,
		Spans: []*schemas.Span{
			root,
			makeSpan("bbbb", "aaaa", "plugin.logging.prehook", schemas.SpanKindPlugin),
			makeSpan("cccc", "bbbb", "plugin.governance.prehook", schemas.SpanKindPlugin),
		},
	}

	rs := p.convertTraceToResourceSpan("svc", trace, nil, false, false, false, false)
	spans := rs.ScopeSpans[0].Spans

	// The filtered logging span is dropped; root + governance remain.
	if len(spans) != 2 {
		t.Fatalf("expected 2 exported spans (logging dropped), got %d", len(spans))
	}
	byID := make(map[string]*Span, len(spans))
	for _, s := range spans {
		byID[string(s.SpanId)] = s
	}
	if _, ok := byID[string(hexToBytes("bbbb", 8))]; ok {
		t.Error("filtered logging span should not be exported")
	}
	gov, ok := byID[string(hexToBytes("cccc", 8))]
	if !ok {
		t.Fatal("governance span should be exported")
	}
	// governance's direct parent (logging) was filtered, so its parent must be rewritten to
	// the nearest exported ancestor (root), not left dangling at the dropped logging span.
	if !bytes.Equal(gov.ParentSpanId, hexToBytes("aaaa", 8)) {
		t.Errorf("governance ParentSpanId = %x, want %x (reparented to root)", gov.ParentSpanId, hexToBytes("aaaa", 8))
	}
}

// TestConvertTraceToResourceSpan_VirtualKeyContentLoggingOff asserts that a request whose virtual
// key turned content logging off (AttrBifrostContentLoggingDisabled on the root span) is exported
// without content on every span and without raw payloads, exactly as the profile's own
// disable_content_logging would do, while the profile flag itself stays untouched for other traces.
// The key can only tighten: the attribute is never false, and a profile that already disables
// content stays disabled whatever the key says.
func TestConvertTraceToResourceSpan_VirtualKeyContentLoggingOff(t *testing.T) {
	p := &OtelPlugin{pluginSpanFilter: &PluginSpanFilter{}}

	makeTrace := func(keyDisabled bool) *schemas.Trace {
		root := makeSpan("aaaa", "", "request", schemas.SpanKindInternal)
		root.Attributes = map[string]any{
			schemas.AttrInputMessages:  "hello",
			schemas.AttrOutputMessages: "hi there",
			schemas.AttrRequestModel:   "gpt-4o-mini",
		}
		if keyDisabled {
			root.Attributes[schemas.AttrBifrostContentLoggingDisabled] = true
		}
		child := makeSpan("bbbb", "aaaa", "chat", schemas.SpanKindLLMCall)
		child.Attributes = map[string]any{
			schemas.AttrInputMessages:  `[{"role":"user","content":"hello"}]`,
			schemas.AttrOutputMessages: `[{"role":"assistant","content":"hi there"}]`,
			// Text-completion suffix is caller-supplied prompt text; stop sequences are a parameter.
			schemas.AttrSuffix:        "and they lived happily",
			schemas.AttrStopSequences: []string{"END"},
		}
		child.LLM = &schemas.LLMSpanData{RawRequest: `{"raw":"request"}`, RawResponse: `{"raw":"response"}`}
		return &schemas.Trace{
			TraceID:  "00000000000000000000000000000003",
			RootSpan: root,
			Spans:    []*schemas.Span{root, child},
		}
	}
	childSpan := func(spans []*Span) *Span {
		for _, s := range spans {
			if bytes.Equal(s.SpanId, hexToBytes("bbbb", 8)) {
				return s
			}
		}
		return nil
	}

	// Profile exports content and raw payloads; the key turned content off: nothing content-shaped
	// leaves, on the root or the child, and the raw payloads stay home too.
	off := p.convertTraceToResourceSpan("svc", makeTrace(true), nil, false, true, false, false)
	root := findRoot(off.ScopeSpans[0].Spans)
	child := childSpan(off.ScopeSpans[0].Spans)
	if got := attrString(root, schemas.AttrInputMessages); got != "" {
		t.Errorf("root input content = %q, want empty when the key disabled content", got)
	}
	if got := attrString(child, schemas.AttrInputMessages); got != "" {
		t.Errorf("child input content = %q, want empty when the key disabled content", got)
	}
	if got := attrString(child, schemas.AttrOutputMessages); got != "" {
		t.Errorf("child output content = %q, want empty when the key disabled content", got)
	}
	if got := attrString(child, schemas.AttrBifrostRawRequest); got != "" {
		t.Errorf("raw request = %q, want empty when the key disabled content", got)
	}
	if got := attrString(child, schemas.AttrBifrostRawResponse); got != "" {
		t.Errorf("raw response = %q, want empty when the key disabled content", got)
	}
	if got := attrString(child, schemas.AttrSuffix); got != "" {
		t.Errorf("suffix = %q, want empty when the key disabled content (it is prompt text)", got)
	}
	if !hasAttr(child, schemas.AttrStopSequences) {
		t.Error("stop sequences are a request parameter and must still export when the key disabled content")
	}
	if got := attrString(root, schemas.AttrRequestModel); got != "gpt-4o-mini" {
		t.Errorf("root request model = %q, want retained (not content)", got)
	}

	// Same profile, a trace whose key said nothing: content and raw payloads export as configured.
	on := p.convertTraceToResourceSpan("svc", makeTrace(false), nil, false, true, false, false)
	child = childSpan(on.ScopeSpans[0].Spans)
	if got := attrString(child, schemas.AttrInputMessages); got == "" {
		t.Error("without the key attribute, child content should export as the profile allows")
	}
	if got := attrString(child, schemas.AttrBifrostRawRequest); got == "" {
		t.Error("without the key attribute, raw payloads should export as the profile allows")
	}
	if got := attrString(child, schemas.AttrSuffix); got == "" {
		t.Error("without the key attribute, the suffix should export as the profile allows")
	}

	// A profile that disables content stays disabled with or without the key attribute.
	for _, keyDisabled := range []bool{true, false} {
		strict := p.convertTraceToResourceSpan("svc", makeTrace(keyDisabled), nil, true, true, false, false)
		if got := attrString(childSpan(strict.ScopeSpans[0].Spans), schemas.AttrInputMessages); got != "" {
			t.Errorf("profile disable_content_logging with keyDisabled=%v: child content = %q, want empty", keyDisabled, got)
		}
	}
}

// TestConvertTraceToResourceSpan_DisableRootSpanContent asserts that the disableRootSpanContent
// flag drops content attributes from the root span only, leaving child (llm.call) spans with
// their full input/output content intact. This is the storage-saving knob that stops the
// framework's input/output duplication onto the root span from reaching the collector (BF-1512).
func TestConvertTraceToResourceSpan_DisableRootSpanContent(t *testing.T) {
	p := &OtelPlugin{pluginSpanFilter: &PluginSpanFilter{}}

	makeContentTrace := func() *schemas.Trace {
		root := makeSpan("aaaa", "", "request", schemas.SpanKindInternal)
		root.Attributes = map[string]any{
			schemas.AttrInputMessages:  "hello",
			schemas.AttrOutputMessages: "hi there",
			schemas.AttrRequestModel:   "gpt-4o-mini",
		}
		child := makeSpan("bbbb", "aaaa", "chat", schemas.SpanKindLLMCall)
		child.Attributes = map[string]any{
			schemas.AttrInputMessages:  `[{"role":"user","content":"hello"}]`,
			schemas.AttrOutputMessages: `[{"role":"assistant","content":"hi there"}]`,
		}
		return &schemas.Trace{
			TraceID:  "00000000000000000000000000000002",
			RootSpan: root,
			Spans:    []*schemas.Span{root, child},
		}
	}

	childSpan := func(spans []*Span) *Span {
		for _, s := range spans {
			if bytes.Equal(s.SpanId, hexToBytes("bbbb", 8)) {
				return s
			}
		}
		return nil
	}

	// Flag off: root keeps its content (current default behavior).
	off := p.convertTraceToResourceSpan("svc", makeContentTrace(), nil, false, false, false, false)
	if root := findRoot(off.ScopeSpans[0].Spans); attrString(root, schemas.AttrInputMessages) == "" {
		t.Error("with flag off, root span should retain input content")
	}

	// Flag on: root content dropped, request model retained, child content untouched.
	on := p.convertTraceToResourceSpan("svc", makeContentTrace(), nil, false, false, false, true)
	root := findRoot(on.ScopeSpans[0].Spans)
	if got := attrString(root, schemas.AttrInputMessages); got != "" {
		t.Errorf("root input content = %q, want empty when disableRootSpanContent is set", got)
	}
	if got := attrString(root, schemas.AttrOutputMessages); got != "" {
		t.Errorf("root output content = %q, want empty when disableRootSpanContent is set", got)
	}
	if got := attrString(root, schemas.AttrRequestModel); got != "gpt-4o-mini" {
		t.Errorf("root model = %q, want non-content metadata preserved", got)
	}
	child := childSpan(on.ScopeSpans[0].Spans)
	if attrString(child, schemas.AttrInputMessages) == "" {
		t.Error("child llm.call span should retain full input content")
	}
	if attrString(child, schemas.AttrOutputMessages) == "" {
		t.Error("child llm.call span should retain full output content")
	}
}

// TestSessionDerivedIDsAreDeterministicAndValid asserts the session-derived trace and parent
// span IDs are stable across calls and have the byte widths OTEL requires (16-byte trace,
// 8-byte span).
func TestSessionDerivedIDsAreDeterministicAndValid(t *testing.T) {
	const sess = "session-abc-123"
	if got := sessionTraceID(sess); got != sessionTraceID(sess) {
		t.Fatal("sessionTraceID is not deterministic")
	}
	if got := sessionParentSpanID(sess); got != sessionParentSpanID(sess) {
		t.Fatal("sessionParentSpanID is not deterministic")
	}
	if sessionTraceID(sess) == sessionTraceID("other-session") {
		t.Error("distinct sessions produced the same trace ID")
	}
	if n := len(sessionTraceID(sess)); n != 32 { // 16 bytes in hex
		t.Errorf("session trace ID hex length = %d, want 32", n)
	}
	if n := len(sessionParentSpanID(sess)); n != 16 { // 8 bytes in hex
		t.Errorf("session parent span ID hex length = %d, want 16", n)
	}
}

// attrString returns the string value of the named attribute on a span, or "" if absent.
// hasAttr reports whether the span carries key at all, whatever its value type.
func hasAttr(s *Span, key string) bool {
	if s == nil {
		return false
	}
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return true
		}
	}
	return false
}

func attrString(s *Span, key string) string {
	for _, kv := range s.Attributes {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

// findRoot returns the exported root span (SpanID "aaaa") from a converted span slice.
func findRoot(spans []*Span) *Span {
	for _, s := range spans {
		if bytes.Equal(s.SpanId, hexToBytes("aaaa", 8)) {
			return s
		}
	}
	return nil
}

// makeSessionTrace builds a single-request trace (root + one LLM child) optionally carrying an
// x-bf-session-id attribute and an inbound traceparent parent on the root span.
func makeSessionTrace(traceID, sessionID, rootParentID string) *schemas.Trace {
	// The HTTP request span is the trace root in production (never an internal span).
	root := makeSpan("aaaa", rootParentID, "request", schemas.SpanKindHTTPRequest)
	child := makeSpan("bbbb", "aaaa", "chat", schemas.SpanKindLLMCall)
	tr := &schemas.Trace{
		TraceID:  traceID,
		RootSpan: root,
		Spans:    []*schemas.Span{root, child},
	}
	if sessionID != "" {
		tr.Attributes = map[string]any{"x-bf-session-id": sessionID}
	}
	return tr
}

// TestSessionGroupingOverridesTraceID asserts that, with grouping enabled and a session ID
// present (and no inbound traceparent), every span adopts the session-derived trace ID, two
// requests in the same session share that trace ID, and each root span is parented to the
// synthetic session span so they render as siblings.
func TestSessionGroupingOverridesTraceID(t *testing.T) {
	p := &OtelPlugin{}
	const sess = "user-42"
	wantTrace := hexToBytes(sessionTraceID(sess), 16)
	wantParent := hexToBytes(sessionParentSpanID(sess), 8)

	for _, original := range []string{"00000000000000000000000000000001", "00000000000000000000000000000002"} {
		rs := p.convertTraceToResourceSpan("svc", makeSessionTrace(original, sess, ""), nil, false, false, true, false)
		spans := rs.ScopeSpans[0].Spans
		if len(spans) != 2 {
			t.Fatalf("expected 2 spans, got %d", len(spans))
		}
		for _, s := range spans {
			if !bytes.Equal(s.TraceId, wantTrace) {
				t.Errorf("span %q TraceId = %x, want session trace %x", s.Name, s.TraceId, wantTrace)
			}
		}
		root := findRoot(spans)
		if root == nil {
			t.Fatal("root span not found")
		}
		if !bytes.Equal(root.ParentSpanId, wantParent) {
			t.Errorf("root ParentSpanId = %x, want session parent %x", root.ParentSpanId, wantParent)
		}
		if got := attrString(root, "session.id"); got != sess {
			t.Errorf("root session.id = %q, want %q", got, sess)
		}
	}
}

// TestSessionGroupingTraceparentWins asserts that an inbound W3C traceparent (root span has a
// ParentID) takes precedence: the request keeps its original trace ID and root parent even when
// grouping is enabled and a session ID is present.
func TestSessionGroupingTraceparentWins(t *testing.T) {
	p := &OtelPlugin{}
	const original = "0123456789abcdef0123456789abcdef"
	const inboundParent = "fedcba9876543210"
	rs := p.convertTraceToResourceSpan("svc", makeSessionTrace(original, "user-42", inboundParent), nil, false, false, true, false)
	spans := rs.ScopeSpans[0].Spans

	wantTrace := hexToBytes(original, 16)
	for _, s := range spans {
		if !bytes.Equal(s.TraceId, wantTrace) {
			t.Errorf("span %q TraceId = %x, want original %x (traceparent wins)", s.Name, s.TraceId, wantTrace)
		}
	}
	root := findRoot(spans)
	if root == nil {
		t.Fatal("root span not found")
	}
	if !bytes.Equal(root.ParentSpanId, hexToBytes(inboundParent, 8)) {
		t.Errorf("root ParentSpanId = %x, want inbound traceparent %x", root.ParentSpanId, hexToBytes(inboundParent, 8))
	}
	// session.id is still tagged even though grouping was skipped (traceparent wins).
	if got := attrString(root, "session.id"); got != "user-42" {
		t.Errorf("root session.id = %q, want %q", got, "user-42")
	}
}

// TestSessionGroupingDisabled asserts that with grouping off the original trace ID is kept and
// the root span has no parent, but the session ID is still tagged on the root as session.id.
func TestSessionGroupingDisabled(t *testing.T) {
	p := &OtelPlugin{}
	const original = "00000000000000000000000000000009"
	rs := p.convertTraceToResourceSpan("svc", makeSessionTrace(original, "user-42", ""), nil, false, false, false, false)
	spans := rs.ScopeSpans[0].Spans

	wantTrace := hexToBytes(original, 16)
	for _, s := range spans {
		if !bytes.Equal(s.TraceId, wantTrace) {
			t.Errorf("span %q TraceId = %x, want original %x", s.Name, s.TraceId, wantTrace)
		}
	}
	root := findRoot(spans)
	if root == nil {
		t.Fatal("root span not found")
	}
	if root.ParentSpanId != nil {
		t.Errorf("root ParentSpanId = %x, want nil (grouping disabled)", root.ParentSpanId)
	}
	if got := attrString(root, "session.id"); got != "user-42" {
		t.Errorf("root session.id = %q, want %q (always tagged)", got, "user-42")
	}
}

// TestNoSessionIDNoTag asserts that when no x-bf-session-id is present the root span carries no
// session.id attribute.
func TestNoSessionIDNoTag(t *testing.T) {
	p := &OtelPlugin{}
	const original = "0000000000000000000000000000000a"
	rs := p.convertTraceToResourceSpan("svc", makeSessionTrace(original, "", ""), nil, false, false, true, false)
	root := findRoot(rs.ScopeSpans[0].Spans)
	if root == nil {
		t.Fatal("root span not found")
	}
	if got := attrString(root, "session.id"); got != "" {
		t.Errorf("root session.id = %q, want empty (no session header)", got)
	}
	// With no session ID, grouping cannot apply: trace ID stays original.
	if !bytes.Equal(root.TraceId, hexToBytes(original, 16)) {
		t.Errorf("root TraceId = %x, want original %x", root.TraceId, hexToBytes(original, 16))
	}
}

// a2aExportTrace passes a stream through the actual gateway gate and framework
// tracer. Raw-policy conversion uses a synthetic capture already tested at the gate.
func a2aExportTrace(t *testing.T) *schemas.Trace {
	t.Helper()
	return a2aGateTrace(t, func(_ *schemas.BifrostA2ARequest, observe func(*schemas.BifrostA2AEvent)) (*schemas.BifrostA2AResponse, error) {
		body := `{"taskId":"created-task","artifact":{"parts":[{"text":"synthetic output"}]}}`
		observe(&schemas.BifrostA2AEvent{EventType: schemas.A2AEventTypeArtifactUpdate, TaskID: "created-task", ContextID: "created-context", Body: &body})
		return &schemas.BifrostA2AResponse{}, nil
	}, true)
}

// a2aGateTrace runs op through the actual gateway gate and framework tracer and
// returns the ended trace. wantOK is whether op succeeds at the gate.
func a2aGateTrace(t *testing.T, op agent.A2AOpFunc, wantOK bool) *schemas.Trace {
	t.Helper()
	log := bifrost.NewDefaultLogger(schemas.LogLevelError)
	tracer := tracing.NewTracer(tracing.NewTraceStore(time.Minute, log), nil, log)
	t.Cleanup(tracer.Stop)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	id := tracer.CreateTrace("")
	ctx.SetValue(schemas.BifrostContextKeyTraceID, id)
	ctx.SetValue(schemas.BifrostContextKeyTracer, tracer)
	_, root := tracer.StartSpan(ctx, "request", schemas.SpanKindHTTPRequest)
	rootSpan := tracer.SpanFromHandle(root)
	ctx.SetValue(schemas.BifrostContextKeySpanID, rootSpan.SpanID)
	request := `{"message":{"role":"user","parts":[{"text":"synthetic input"}]}}`
	_, err := (&agent.Manager{}).RunWithPluginPipeline(ctx, &schemas.BifrostA2ARequest{RequestType: schemas.A2ARequestTypeSendStreamingMessage, AgentName: "fixture", RequestBody: &request}, op)
	require.Equal(t, wantOK, err == nil)
	tracer.EndSpan(root, schemas.SpanStatusOk, "")
	trace := tracer.EndTrace(id)
	require.NotNil(t, trace)
	t.Cleanup(func() { tracer.ReleaseTrace(trace) })
	trace.Attributes = map[string]any{schemas.TraceAttrSessionID: "synthetic-session"}
	for _, span := range trace.Spans {
		if span.Kind == schemas.SpanKindA2AOperation {
			span.SetA2ARawPayloads(span.SpanID, &schemas.A2ASpanData{RawRequest: request, RawResponse: `{"text":"synthetic raw output"}`})
		}
	}
	return trace
}

// TestA2AExportContentPolicy pins structured content, raw opt-in, root-only
// suppression, virtual-key policy, exclusions, and event content independently.
func TestA2AExportContentPolicy(t *testing.T) {
	trace := a2aExportTrace(t)
	p := &OtelPlugin{}
	for _, tc := range []struct {
		name                    string
		disabled, raw, root, vk bool
		excluded                []string
	}{
		{name: "structured"}, {name: "raw", raw: true}, {name: "disabled", disabled: true, raw: true},
		{name: "root suppressed", raw: true, root: true}, {name: "virtual key suppressed", raw: true, vk: true},
		{name: "raw request excluded", raw: true, excluded: []string{schemas.AttrBifrostRawRequest}},
		{name: "structured excluded", raw: true, excluded: []string{schemas.AttrInputMessages, schemas.AttrOutputMessages}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace.RootSpan.SetAttribute(schemas.AttrBifrostContentLoggingDisabled, tc.vk)
			filtered := schemas.StripTraceAttributes(trace, tc.excluded)
			rs := p.convertTraceToResourceSpan("fixture", filtered, nil, tc.disabled, tc.raw, false, tc.root)
			stripExcludedFromResourceSpan(rs, tc.excluded)
			for _, span := range rs.ScopeSpans[0].Spans {
				attrs := kvMap(span.Attributes)
				root := span.Name == "request"
				wantContent := !tc.disabled && !tc.vk && !(tc.root && root)
				_, input := attrs[schemas.AttrInputMessages]
				require.Equal(t, wantContent && !slices.Contains(tc.excluded, schemas.AttrInputMessages), input)
				_, output := attrs[schemas.AttrOutputMessages]
				require.Equal(t, wantContent && !slices.Contains(tc.excluded, schemas.AttrOutputMessages), output)
				_, rawRequest := attrs[schemas.AttrBifrostRawRequest]
				_, rawResponse := attrs[schemas.AttrBifrostRawResponse]
				require.Equal(t, tc.raw && wantContent && !root && !slices.Contains(tc.excluded, schemas.AttrBifrostRawRequest), rawRequest)
				require.Equal(t, tc.raw && wantContent && !root, rawResponse)
				if !root {
					require.Equal(t, "created-task", attrs[schemas.AttrA2ATaskID].GetStringValue())
					require.Len(t, span.Events, 1)
					_, eventContent := kvMap(span.Events[0].Attributes)[schemas.AttrOutputMessages]
					require.Equal(t, wantContent && !slices.Contains(tc.excluded, schemas.AttrOutputMessages), eventContent)
				}
			}
		})
	}
}

// a2aGRPCCollector captures real protobuf export requests from the gRPC client.
type a2aGRPCCollector struct {
	collectorpb.UnimplementedTraceServiceServer
	requests chan *collectorpb.ExportTraceServiceRequest
}

// Export records the wire request before acknowledging collection.
func (c *a2aGRPCCollector) Export(_ context.Context, req *collectorpb.ExportTraceServiceRequest) (*collectorpb.ExportTraceServiceResponse, error) {
	c.requests <- req
	return &collectorpb.ExportTraceServiceResponse{}, nil
}

// TestA2AExportWireProfiles checks actual HTTP and gRPC protobufs after Inject
// fans out to profiles with different content policies and exclusions.
func TestA2AExportWireProfiles(t *testing.T) {
	logger = bifrost.NewDefaultLogger(schemas.LogLevelError)
	httpRequests := make(chan *collectorpb.ExportTraceServiceRequest, 8)
	httpCollector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		req := &collectorpb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(data, req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		httpRequests <- req
		w.WriteHeader(http.StatusOK)
	}))
	defer httpCollector.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcCollector := &a2aGRPCCollector{requests: make(chan *collectorpb.ExportTraceServiceRequest, 8)}
	server := grpc.NewServer()
	collectorpb.RegisterTraceServiceServer(server, grpcCollector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	httpClient, err := NewOtelClientHTTP(httpCollector.URL, nil, "", true, time.Second)
	require.NoError(t, err)
	defer httpClient.Close()
	grpcClient, err := NewOtelClientGRPC(listener.Addr().String(), nil, "", true)
	require.NoError(t, err)
	defer grpcClient.Close()
	p := &OtelPlugin{targets: []*otelTarget{
		{client: httpClient, serviceName: "structured", exportTimeout: time.Second},
		{client: httpClient, serviceName: "raw", exportRawPayloads: true, exportTimeout: time.Second},
		{client: grpcClient, serviceName: "disabled", disableContentLogging: true, exportRawPayloads: true, exportTimeout: time.Second},
		{client: grpcClient, serviceName: "root suppressed", disableRootSpanContent: true, exportRawPayloads: true, exportTimeout: time.Second, excludedAttributes: []string{schemas.AttrBifrostRawRequest}},
	}}
	trace := a2aExportTrace(t)
	for _, vkDisabled := range []bool{false, true} {
		trace.RootSpan.SetAttribute(schemas.AttrBifrostContentLoggingDisabled, vkDisabled)
		// A denylist cannot remove the virtual-key policy before it is enforced.
		p.targets[0].excludedAttributes = []string{schemas.AttrBifrostContentLoggingDisabled}
		require.NoError(t, p.Inject(context.Background(), trace))
		for i := 0; i < 4; i++ {
			var request *collectorpb.ExportTraceServiceRequest
			select {
			case request = <-httpRequests:
			case request = <-grpcCollector.requests:
			case <-time.After(3 * time.Second):
				t.Fatal("missing protobuf export")
			}
			require.Len(t, request.ResourceSpans, 1)
			rs := request.ResourceSpans[0]
			service := kvMap(rs.Resource.Attributes)["service.name"].GetStringValue()
			for _, span := range rs.ScopeSpans[0].Spans {
				attrs := kvMap(span.Attributes)
				root := span.Name == "request"
				content := !vkDisabled && service != "disabled" && !(service == "root suppressed" && root)
				_, hasContent := attrs[schemas.AttrOutputMessages]
				require.Equal(t, content, hasContent, service)
				_, raw := attrs[schemas.AttrBifrostRawResponse]
				require.Equal(t, content && !root && service != "structured", raw, service)
				_, rawRequest := attrs[schemas.AttrBifrostRawRequest]
				require.Equal(t, content && !root && service == "raw", rawRequest, service)
			}
		}
	}
}

// TestA2AExportStatusMessageFollowsContentPolicy pins, on the HTTP and gRPC wire,
// that an A2A operation's error text leaves only when content does: it is the
// upstream agent's error, which can echo the caller's input. The error status,
// error type and task identity are kept either way, and so is a non-A2A span's
// status text.
func TestA2AExportStatusMessageFollowsContentPolicy(t *testing.T) {
	const marker = "SYNTHETIC-A2A-STATUS-MARKER"
	logger = bifrost.NewDefaultLogger(schemas.LogLevelError)
	requests := make(chan *collectorpb.ExportTraceServiceRequest, 8)
	httpCollector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable", 400)
			return
		}
		req := &collectorpb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(data, req); err != nil {
			http.Error(w, "undecodable", 400)
			return
		}
		requests <- req
		w.WriteHeader(http.StatusOK)
	}))
	defer httpCollector.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcCollector := &a2aGRPCCollector{requests: requests}
	server := grpc.NewServer()
	collectorpb.RegisterTraceServiceServer(server, grpcCollector)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	httpClient, err := NewOtelClientHTTP(httpCollector.URL, nil, "", true, time.Second)
	require.NoError(t, err)
	defer httpClient.Close()
	grpcClient, err := NewOtelClientGRPC(listener.Addr().String(), nil, "", true)
	require.NoError(t, err)
	defer grpcClient.Close()
	p := &OtelPlugin{targets: []*otelTarget{
		{client: httpClient, serviceName: "content", exportTimeout: time.Second},
		{client: grpcClient, serviceName: "disabled", disableContentLogging: true, exportTimeout: time.Second},
	}}

	trace := a2aGateTrace(t, func(*schemas.BifrostA2ARequest, func(*schemas.BifrostA2AEvent)) (*schemas.BifrostA2AResponse, error) {
		return nil, errors.New("agent rejected: " + marker)
	}, false)
	trace.RootSpan.Status, trace.RootSpan.StatusMsg = schemas.SpanStatusError, "upstream unavailable"
	for _, vkDisabled := range []bool{false, true} {
		trace.RootSpan.SetAttribute(schemas.AttrBifrostContentLoggingDisabled, vkDisabled)
		require.NoError(t, p.Inject(context.Background(), trace))
		for range p.targets {
			var request *collectorpb.ExportTraceServiceRequest
			select {
			case request = <-requests:
			case <-time.After(3 * time.Second):
				t.Fatal("missing protobuf export")
			}
			rs := request.ResourceSpans[0]
			service := kvMap(rs.Resource.Attributes)["service.name"].GetStringValue()
			content := !vkDisabled && service != "disabled"
			sawOp := false
			for _, span := range rs.ScopeSpans[0].Spans {
				if span.Name == "request" {
					require.Equal(t, "upstream unavailable", span.Status.GetMessage(), "non-A2A status text is not withheld (%s)", service)
					continue
				}
				sawOp = true
				require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, span.Status.GetCode(), service)
				require.Equal(t, content, strings.Contains(span.Status.GetMessage(), marker), "status text exported=%v, want %v (%s, vk disabled=%v)", !content, content, service, vkDisabled)
				require.NotEmpty(t, kvMap(span.Attributes)[schemas.AttrErrorTypeSpec].GetStringValue(), "error classification kept (%s)", service)
			}
			require.True(t, sawOp, "A2A operation span missing (%s)", service)
		}
	}
}

// BenchmarkConvertSpanToOTELSpanStatus records per-span conversion cost for an
// errored A2A operation and an errored LLM call, with content on and off.
func BenchmarkConvertSpanToOTELSpanStatus(b *testing.B) {
	for _, kind := range []schemas.SpanKind{schemas.SpanKindA2AOperation, schemas.SpanKindLLMCall} {
		span := makeSpan("0102030405060708", "", "op", kind)
		span.Status, span.StatusMsg = schemas.SpanStatusError, "synthetic failure"
		span.Attributes = map[string]any{schemas.AttrErrorTypeSpec: "synthetic", schemas.AttrInputMessages: "synthetic input"}
		for _, disabled := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/content_disabled=%v", kind, disabled), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = convertSpanToOTELSpan("0102030405060708090a0b0c0d0e0f10", span, disabled, false)
				}
			})
		}
	}
}
