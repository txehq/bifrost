package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bytedance/sonic"
	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/openai"
	"github.com/capsohq/bifrost/core/providers/xai"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

// buildFileUploadCtx builds a fasthttp request context carrying a multipart
// file upload body with the given extra form fields.
func buildFileUploadCtx(t *testing.T, fields map[string]string) *fasthttp.RequestCtx {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "test.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("fake-png-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("purpose", "vision"); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(body.Bytes())
	return ctx
}

func TestParseOpenAIFileUploadMultipartRequest_ExpiresAfter(t *testing.T) {
	tests := []struct {
		name    string
		fields  map[string]string
		want    *schemas.FileExpiresAfter
		wantErr bool
	}{
		{
			name: "both fields present",
			fields: map[string]string{
				"expires_after[anchor]":  "created_at",
				"expires_after[seconds]": "3600",
			},
			want: &schemas.FileExpiresAfter{Anchor: "created_at", Seconds: 3600},
		},
		{
			name:   "neither field present",
			fields: nil,
			want:   nil,
		},
		{
			// Partial input is passed through; the upstream provider rejects it.
			name: "anchor only",
			fields: map[string]string{
				"expires_after[anchor]": "created_at",
			},
			want: &schemas.FileExpiresAfter{Anchor: "created_at", Seconds: 0},
		},
		{
			name: "seconds only",
			fields: map[string]string{
				"expires_after[seconds]": "3600",
			},
			want: &schemas.FileExpiresAfter{Anchor: "", Seconds: 3600},
		},
		{
			// Out-of-range values are passed through; the upstream provider rejects them.
			name: "out-of-range seconds",
			fields: map[string]string{
				"expires_after[anchor]":  "created_at",
				"expires_after[seconds]": "100",
			},
			want: &schemas.FileExpiresAfter{Anchor: "created_at", Seconds: 100},
		},
		{
			name: "non-numeric seconds",
			fields: map[string]string{
				"expires_after[anchor]":  "created_at",
				"expires_after[seconds]": "not-a-number",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := buildFileUploadCtx(t, tt.fields)
			uploadReq := &schemas.BifrostFileUploadRequest{}
			err := parseOpenAIFileUploadMultipartRequest(ctx, uploadReq)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if uploadReq.Purpose != schemas.FilePurpose("vision") {
				t.Errorf("purpose = %q, want %q", uploadReq.Purpose, "vision")
			}
			if tt.want == nil {
				if uploadReq.ExpiresAfter != nil {
					t.Errorf("ExpiresAfter = %+v, want nil", uploadReq.ExpiresAfter)
				}
				return
			}
			if uploadReq.ExpiresAfter == nil {
				t.Fatal("ExpiresAfter is nil, expected it to be populated")
			}
			if *uploadReq.ExpiresAfter != *tt.want {
				t.Errorf("ExpiresAfter = %+v, want %+v", *uploadReq.ExpiresAfter, *tt.want)
			}
		})
	}
}

// buildTranscriptionMultipartCtx builds a fasthttp request context carrying
// a multipart transcription upload body with the given extra form fields.
// Fields with multiple values (e.g. repeated "timestamp_granularities[]")
// are written once per slice element, preserving order.
func buildTranscriptionMultipartCtx(t *testing.T, fields map[string][]string) *fasthttp.RequestCtx {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-4o-transcribe-diarize"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "sample.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("fake-audio-bytes")); err != nil {
		t.Fatal(err)
	}
	for k, values := range fields {
		for _, v := range values {
			if err := writer.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(body.Bytes())
	return ctx
}

func TestParseTranscriptionMultipartRequest_ExtraParamsPassthrough(t *testing.T) {
	ctx := buildTranscriptionMultipartCtx(t, map[string][]string{
		"diarize":            {"true"},      // ElevenLabs-specific: JSON-decodable (bool)
		"num_speakers":       {"2"},         // ElevenLabs-specific: JSON-decodable (number)
		"unstructured_extra": {"not-json{"}, // falls back to raw string on decode failure
		"chunking_strategy":  {"auto"},      // has its own dedicated handling; must not also appear via the generic passthrough
	})

	req := &openai.OpenAITranscriptionRequest{}
	if err := parseTranscriptionMultipartRequest(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.ExtraParams["diarize"] != true {
		t.Errorf("ExtraParams[diarize] = %#v, want true (bool)", req.ExtraParams["diarize"])
	}
	if req.ExtraParams["num_speakers"] != float64(2) {
		t.Errorf("ExtraParams[num_speakers] = %#v, want float64(2)", req.ExtraParams["num_speakers"])
	}
	if req.ExtraParams["unstructured_extra"] != "not-json{" {
		t.Errorf("ExtraParams[unstructured_extra] = %#v, want raw string fallback", req.ExtraParams["unstructured_extra"])
	}

	// chunking_strategy must be set exactly once, via its own dedicated
	// handling above the generic passthrough loop, not duplicated.
	if req.ExtraParams["chunking_strategy"] != "auto" {
		t.Errorf("ExtraParams[chunking_strategy] = %#v, want %q", req.ExtraParams["chunking_strategy"], "auto")
	}
}

func TestParseTranscriptionMultipartRequest_TypedFieldsNotShadowedByExtraParams(t *testing.T) {
	ctx := buildTranscriptionMultipartCtx(t, map[string][]string{
		"temperature":               {"0.3"},
		"timestamp_granularities[]": {"word", "segment"},
	})

	req := &openai.OpenAITranscriptionRequest{}
	if err := parseTranscriptionMultipartRequest(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.Temperature == nil || *req.Temperature != 0.3 {
		t.Fatalf("Temperature = %v, want *0.3", req.Temperature)
	}
	if len(req.TimestampGranularities) != 2 || req.TimestampGranularities[0] != "word" || req.TimestampGranularities[1] != "segment" {
		t.Fatalf("TimestampGranularities = %v, want [word segment]", req.TimestampGranularities)
	}
	// These are known/typed fields now, so they must not also leak into
	// ExtraParams via the generic passthrough loop.
	if _, ok := req.ExtraParams["temperature"]; ok {
		t.Errorf("temperature leaked into ExtraParams: %#v", req.ExtraParams["temperature"])
	}
	if _, ok := req.ExtraParams["timestamp_granularities[]"]; ok {
		t.Errorf("timestamp_granularities[] leaked into ExtraParams: %#v", req.ExtraParams["timestamp_granularities[]"])
	}
}

// TestParseTranscriptionMultipartRequest_PreservesFilename covers issue #5670:
// the multipart part's filename must reach the parsed request, otherwise the
// provider re-derives it from magic bytes and mislabels non-sniffable
// containers (m4a/mp4/webm) as audio.mp3, which OpenAI rejects.
func TestParseTranscriptionMultipartRequest_PreservesFilename(t *testing.T) {
	ctx := buildTranscriptionMultipartCtx(t, nil)

	req := &openai.OpenAITranscriptionRequest{}
	if err := parseTranscriptionMultipartRequest(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.Filename != "sample.mp3" {
		t.Errorf("Filename = %q, want %q (multipart part filename dropped)", req.Filename, "sample.mp3")
	}
}

// A repeated form field (include[]=a&include[]=b) used to keep only its first value on the
// /openai transcription route (TXE-6530).
func TestParseTranscriptionMultipartRequest_RepeatedExtraFieldKeepsEveryValue(t *testing.T) {
	ctx := buildTranscriptionMultipartCtx(t, map[string][]string{
		"include[]":    {"logprobs", "segments"},
		"num_speakers": {"2"},
	})

	req := &openai.OpenAITranscriptionRequest{}
	if err := parseTranscriptionMultipartRequest(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := req.ExtraParams["include[]"].([]string)
	if len(got) != 2 || got[0] != "logprobs" || got[1] != "segments" {
		t.Errorf("ExtraParams[include[]] = %#v, want [logprobs segments]", req.ExtraParams["include[]"])
	}
	if req.ExtraParams["num_speakers"] != float64(2) {
		t.Errorf("ExtraParams[num_speakers] = %#v, want the single value decoded as before", req.ExtraParams["num_speakers"])
	}
}

// findOpenAIRoute returns the POST route registered at path under the given
// prefix, or nil when none is.
func findOpenAIRoute(prefix, path string) *RouteConfig {
	for _, route := range CreateOpenAIRouteConfigs(prefix, &mockHandlerStore{}) {
		if route.Path == path && route.Method == "POST" {
			rc := route
			return &rc
		}
	}
	return nil
}

// TestOpenAIDecisionsRouteRegisteredOnEveryPath pins that OpenAI's decisions
// route is served at both SDK paths, under /openai and under every SDK prefix
// that reuses the OpenAI routes, and that it attaches no large-payload hook,
// which would leave the questions unparsed.
func TestOpenAIDecisionsRouteRegisteredOnEveryPath(t *testing.T) {
	for _, prefix := range []string{"/openai", "/langchain", "/litellm", "/pydanticai"} {
		for _, path := range []string{"/v1/decisions", "/decisions"} {
			route := findOpenAIRoute(prefix, prefix+path)
			if route == nil {
				t.Errorf("POST %s%s not registered", prefix, path)
				continue
			}
			if got := route.GetHTTPRequestType(nil); got != schemas.DecisionRequest {
				t.Errorf("%s%s request type = %s, want %s", prefix, path, got, schemas.DecisionRequest)
			}
			if route.PreCallback != nil {
				t.Errorf("%s%s must not register a large-payload hook", prefix, path)
			}
		}
	}
}

// TestOpenAIDecisionsRouteConvertsRequest pins the route's request handling:
// an SDK body becomes the normalized request, questions keep their order and
// an unnamed question stays unnamed, a model without a prefix is OpenAI's, an
// unknown future field is marked to reach the provider wire, and fallbacks are
// kept for routing. The payload is synthetic.
func TestOpenAIDecisionsRouteConvertsRequest(t *testing.T) {
	route := findOpenAIRoute("/openai", "/openai/v1/decisions")
	if route == nil {
		t.Fatal("decisions route not registered")
	}

	incoming := route.GetRequestTypeInstance(context.Background())
	body := []byte(`{
		"model": "gpt-6-luna",
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Inspect the product in this photo."}, {"type": "input_image", "image_url": "https://example.com/product.png", "detail": "high"}]}],
		"questions": [
			{"type": "predicate", "name": "visible_damage", "instructions": "Is the product damaged?"},
			{"type": "score", "instructions": "How severe is the damage?", "levels": [{"label": "Cosmetic"}, {"label": "Blocked"}]}
		],
		"fallbacks": ["typesafe/jev-1.13.0"],
		"future_option": {"mode": "strict"}
	}`)
	if err := parseJSONRequestBody(body, incoming); err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	converted, err := route.RequestConverter(ctx, incoming)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	decision := converted.DecisionRequest
	if decision.Provider != schemas.OpenAI || decision.Model != "gpt-6-luna" {
		t.Errorf("got provider %q model %q, want openai gpt-6-luna", decision.Provider, decision.Model)
	}
	if decision.Input.NonTextPartType() != schemas.DecisionInputPartTypeImage {
		t.Error("input messages, with the image, were not kept")
	}
	if image := decision.Input.Messages[0].Content.Parts[1]; image.Detail == nil || *image.Detail != "high" {
		t.Errorf("the image detail level was not kept: %+v", image)
	}
	if len(decision.Questions) != 2 || *decision.Questions[0].Name != "visible_damage" || decision.Questions[1].Name != nil {
		t.Errorf("question order or the unnamed question was not kept: %+v", decision.Questions)
	}
	if _, ok := decision.ExtraParams["future_option"]; !ok {
		t.Error("unknown field was dropped instead of carried to the provider")
	}
	if passthrough, _ := ctx.Value(schemas.BifrostContextKeyPassthroughExtraParams).(bool); !passthrough {
		t.Error("unknown fields present but passthrough not requested; they would be dropped on the wire")
	}

	router := &GenericRouter{}
	if err := router.extractAndParseFallbacks(ctx, incoming, converted); err != nil {
		t.Fatalf("fallbacks: %v", err)
	}
	want := schemas.Fallback{Provider: schemas.Typesafe, Model: "jev-1.13.0"}
	if len(decision.Fallbacks) != 1 || decision.Fallbacks[0] != want {
		t.Errorf("fallbacks = %+v, want [%+v]", decision.Fallbacks, want)
	}
}

// TestOpenAIDecisionsRouteRejectsOtherShapes pins that the route speaks only
// OpenAI's body: a Typesafe-shaped body (questions keyed by name) does not
// decode, and a body without input or questions, with Typesafe's criteria on a
// question, or with a stray state next to input is a 400, so a stray state can
// never reach a Typesafe model as its state.
func TestOpenAIDecisionsRouteRejectsOtherShapes(t *testing.T) {
	route := findOpenAIRoute("/openai", "/openai/v1/decisions")
	if route == nil {
		t.Fatal("decisions route not registered")
	}
	typesafeShaped := route.GetRequestTypeInstance(context.Background())
	if err := parseJSONRequestBody([]byte(`{"model":"typesafe/jev-1.13.0","state":"s","questions":{"q":{"type":"noul"}}}`), typesafeShaped); err == nil {
		t.Error("a Typesafe-shaped body must not decode on the OpenAI route")
	}
	for _, body := range []string{
		`{"model":"gpt-6-luna","questions":[{"type":"predicate","instructions":"Q?"}]}`,
		`{"model":"gpt-6-luna","input":"text","questions":[]}`,
		`{"model":"typesafe/jev-1.13.0","input":"text","questions":[{"type":"predicate","instructions":"Q?","criteria":{"true":"yes"}}]}`,
		`{"model":"typesafe/jev-1.13.0","state":"stale","input":"text","questions":[{"type":"predicate","instructions":"Q?"}]}`,
	} {
		incoming := route.GetRequestTypeInstance(context.Background())
		if err := parseJSONRequestBody([]byte(body), incoming); err != nil {
			t.Fatalf("parse %s: %v", body, err)
		}
		if _, err := route.RequestConverter(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), incoming); err == nil {
			t.Errorf("expected rejection for %s", body)
		}
	}
}

// TestOpenAIDecisionsRouteResponses pins the route's response handling: the
// normalized response is rendered in OpenAI's shape whichever provider served
// it, so a change a plugin made is what the client receives, and the upstream
// body is relayed only when raw responses were asked for, including for a
// custom OpenAI-based provider. Rendered answers carry name null when their
// question had none, usage keeps OpenAI's names and token details, Bifrost's
// extra_fields come along as on the other OpenAI routes, and Laya's fields are
// kept. Errors pass through as on the other OpenAI routes. The
// payloads are synthetic.
func TestOpenAIDecisionsRouteResponses(t *testing.T) {
	route := findOpenAIRoute("/openai", "/openai/v1/decisions")
	if route == nil {
		t.Fatal("decisions route not registered")
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	render := func(ctx *schemas.BifrostContext, resp *schemas.BifrostDecisionResponse) map[string]any {
		t.Helper()
		out, err := route.DecisionResponseConverter(ctx, resp)
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var wire map[string]any
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return wire
	}

	// OpenAI answered 0.99; a plugin changed the normalized answer to 0.01.
	served := &schemas.BifrostDecisionResponse{
		ID:      "dec_1",
		Model:   "gpt-6-luna",
		Answers: []schemas.DecisionAnswer{{Type: schemas.DecisionTypePredicate, Name: schemas.Ptr("q"), Probability: schemas.Ptr(0.01)}},
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 120, TotalTokens: 120,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 30}},
	}
	served.ExtraFields.Provider = schemas.OpenAI
	wire := render(ctx, served)
	if _, hasID := wire["id"]; hasID {
		t.Errorf("OpenAI's Decision object has no id, got %v", wire["id"])
	}
	if extra, _ := wire["extra_fields"].(map[string]any); extra["provider"] != "openai" {
		t.Errorf("extra_fields must come along as on the other OpenAI routes, got %v", wire["extra_fields"])
	}
	if got := wire["answers"].([]any)[0].(map[string]any)["probability"]; got != 0.01 {
		t.Errorf("the normalized answer must reach the client, got probability %v", got)
	}
	details, _ := wire["usage"].(map[string]any)["input_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 30.0 {
		t.Errorf("cached input tokens must survive: %v", wire["usage"])
	}

	raw := json.RawMessage(`{"id":"dec_1","model":"gpt-6-luna","answers":[{"type":"predicate","name":"q","probability":0.99}],"future_field":true}`)
	served.ExtraFields.RawResponse = raw
	if out, err := route.DecisionResponseConverter(ctx, served); err != nil || string(out.(json.RawMessage)) != string(raw) {
		t.Errorf("raw response not relayed when asked for: %#v (%v)", out, err)
	}
	customCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	customCtx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.OpenAI)
	served.ExtraFields.Provider = "my-openai"
	if out, err := route.DecisionResponseConverter(customCtx, served); err != nil || string(out.(json.RawMessage)) != string(raw) {
		t.Errorf("custom OpenAI provider's raw response not relayed when asked for: %#v (%v)", out, err)
	}

	stateTokens, answerConfidence := 13, 0.97
	laya := &schemas.BifrostDecisionResponse{
		Model: "laya-rl-agent",
		Answers: []schemas.DecisionAnswer{
			{Type: schemas.DecisionTypeChoice, Name: schemas.Ptr("category"), Choice: &schemas.DecisionScalar{Str: schemas.Ptr("billing")}, AnswerConfidence: &answerConfidence},
			{Type: schemas.DecisionTypePredicate, Probability: schemas.Ptr(0.2)},
		},
		Usage:   &schemas.BifrostLLMUsage{PromptTokens: 39, TotalTokens: 39, StateTokens: &stateTokens},
		Routing: json.RawMessage(`{"model":"english"}`),
	}
	laya.ExtraFields.Provider = "laya"
	wire = render(ctx, laya)
	wantUsage := map[string]any{"input_tokens": 39.0, "output_tokens": 0.0, "total_tokens": 39.0, "state_tokens": 13.0}
	if got := wire["usage"]; !jsonEqual(got, wantUsage) {
		t.Errorf("usage = %v, want %v", got, wantUsage)
	}
	answers := wire["answers"].([]any)
	if len(answers) != 2 || answers[0].(map[string]any)["answer_confidence"] != 0.97 {
		t.Errorf("answers must keep their order and Laya's fields: %v", answers)
	}
	if name, named := answers[1].(map[string]any)["name"]; !named || name != nil {
		t.Errorf("an unnamed question's answer carries name null, as OpenAI's do: %v", answers[1])
	}
	if !jsonEqual(wire["routing"], map[string]any{"model": "english"}) {
		t.Errorf("routing = %v", wire["routing"])
	}

	bifrostErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "Invalid value for 'questions': expected an array."}}
	if got := route.ErrorConverter(ctx, bifrostErr); got != bifrostErr {
		t.Errorf("error must pass through as on the other OpenAI routes, got %#v", got)
	}
}

// jsonEqual reports whether two decoded JSON values are equal.
func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// deepgramTranscriptionTestAccount configures the Deepgram provider at a local upstream, so
// the OpenAI drop-in transcription route runs end to end without a live key.
type deepgramTranscriptionTestAccount struct {
	baseURL string
}

func (a deepgramTranscriptionTestAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Deepgram}, nil
}

func (a deepgramTranscriptionTestAccount) GetKeysForProvider(_ context.Context, p schemas.ModelProvider) ([]schemas.Key, error) {
	if p != schemas.Deepgram {
		return nil, nil
	}
	return []schemas.Key{{ID: "deepgram-test-key", Name: "deepgram-test-key", Value: *schemas.NewSecretVar("dg-test"), Models: schemas.WhiteList{"*"}, Weight: 1.0}}, nil
}

func (a deepgramTranscriptionTestAccount) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if p != schemas.Deepgram {
		return nil, fmt.Errorf("unsupported provider %s", p)
	}
	nc := schemas.DefaultNetworkConfig
	nc.BaseURL = a.baseURL
	return &schemas.ProviderConfig{NetworkConfig: nc, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// The OpenAI drop-in route /openai/v1/audio/transcriptions reaches Deepgram too, by the
// deepgram/ model prefix: a file upload is one POST /v1/listen with the audio as the body and
// the non-OpenAI form fields as query parameters, and the answer carries Deepgram's billable
// duration as usage (contract C4, C5). A unified field Deepgram does not take (temperature on
// this route) is 400 caller_unsupported_parameter with no upstream call. The route requires a
// file; a URL-only request is not taken there.
func TestOpenAITranscriptionRouteReachesDeepgram(t *testing.T) {
	type upstreamCall struct {
		path, query, contentType, auth, body string
	}
	var mu sync.Mutex
	var calls []upstreamCall
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, upstreamCall{path: r.URL.Path, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"), body: string(body)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"metadata":{"request_id":"a847f427-4ad5-4d67-9b95-db801e58251c","duration":25.933313,"channels":1},"results":{"channels":[{"alternatives":[{"transcript":"Yeah. As as much as, it's worth celebrating."}]}]}}`)
	}))
	defer upstream.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: deepgramTranscriptionTestAccount{baseURL: upstream.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	post := func(fields [][2]string) *fasthttp.RequestCtx {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, f := range fields {
			require.NoError(t, writer.WriteField(f[0], f[1]))
		}
		part, err := writer.CreateFormFile("file", "sample.wav")
		require.NoError(t, err)
		_, err = part.Write([]byte("RIFF\x24\x00\x00\x00WAVEfmt fake-wav"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fasthttp.MethodPost)
		ctx.Request.SetRequestURI("/openai/v1/audio/transcriptions")
		ctx.Request.Header.SetContentType(writer.FormDataContentType())
		ctx.Request.SetBody(body.Bytes())
		r.Handler(ctx)
		return ctx
	}

	ctx := post([][2]string{
		{"model", "deepgram/nova-3"},
		{"language", "en"},
		{"smart_format", "true"},
		{"keyterm", "Bifrost gateway"},
		{"keyterm", "Tokun"},
	})
	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
	require.Equal(t, "Yeah. As as much as, it's worth celebrating.", gjson.GetBytes(ctx.Response.Body(), "text").String())
	usage := gjson.GetBytes(ctx.Response.Body(), "usage")
	require.Equal(t, "duration", usage.Get("type").String(), "body: %s", ctx.Response.Body())
	require.Equal(t, 25.933313, usage.Get("seconds").Float(), "body: %s", ctx.Response.Body())
	mu.Lock()
	require.Len(t, calls, 1)
	call := calls[0]
	mu.Unlock()
	require.Equal(t, "/v1/listen", call.path)
	require.Equal(t, "Token dg-test", call.auth)
	require.Equal(t, "audio/wav", call.contentType)
	require.Equal(t, "RIFF\x24\x00\x00\x00WAVEfmt fake-wav", call.body)
	require.Equal(t, "keyterm=Bifrost+gateway&keyterm=Tokun&language=en&model=nova-3&smart_format=true", call.query)

	ctx = post([][2]string{{"model", "deepgram/nova-3"}, {"temperature", "0.2"}})
	require.Equal(t, fasthttp.StatusBadRequest, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
	require.Equal(t, "caller_unsupported_parameter", gjson.GetBytes(ctx.Response.Body(), "extra_fields.error_type").String(), "body: %s", ctx.Response.Body())
	require.Equal(t, "temperature", gjson.GetBytes(ctx.Response.Body(), "error.param").String(), "body: %s", ctx.Response.Body())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 1, "a refused request must make no upstream call")
}

// speechTestAccount configures OpenAI and Azure providers whose endpoints point at a local
// upstream, so speech routes run end to end without a live key. The Azure-style deployments
// route always resolves to Azure, the plain speech route to the provider in the model prefix.
type speechTestAccount struct {
	baseURL string
}

func (a speechTestAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI, schemas.Azure}, nil
}

func (a speechTestAccount) GetKeysForProvider(_ context.Context, p schemas.ModelProvider) ([]schemas.Key, error) {
	key := schemas.Key{
		ID:     "speech-test-key-" + string(p),
		Value:  *schemas.NewSecretVar("sk-test-operator-key"),
		Models: schemas.WhiteList{"*"},
		Weight: 1.0,
	}
	switch p {
	case schemas.OpenAI:
	case schemas.Azure:
		key.AzureKeyConfig = &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar(a.baseURL)}
	default:
		return nil, nil
	}
	return []schemas.Key{key}, nil
}

func (a speechTestAccount) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if p != schemas.OpenAI && p != schemas.Azure {
		return nil, fmt.Errorf("unsupported provider %s", p)
	}
	nc := schemas.DefaultNetworkConfig
	if p == schemas.OpenAI {
		nc.BaseURL = a.baseURL
	}
	return &schemas.ProviderConfig{
		NetworkConfig:            nc,
		ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize,
	}, nil
}

// TestOpenAISpeechRoutesReturnRawAudio pins that every OpenAI-integration speech route
// answers with the audio bytes, as the OpenAI and Azure OpenAI SDKs expect. The Azure-style
// /openai/deployments/{deployment}/audio/speech route used to serialize the whole
// BifrostSpeechResponse as JSON ({"audio":"<base64>"}) instead.
func TestOpenAISpeechRoutesReturnRawAudio(t *testing.T) {
	audio := []byte("ID3\x04\x00fake-mp3-frames")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audio)
	}))
	defer upstream.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: speechTestAccount{baseURL: upstream.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	for _, uri := range []string{
		"/openai/v1/audio/speech",
		"/openai/openai/deployments/gpt-4o-mini-tts/audio/speech?api-version=2025-04-01-preview",
	} {
		t.Run(uri, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod(fasthttp.MethodPost)
			ctx.Request.SetRequestURI(uri)
			ctx.Request.Header.SetContentType("application/json")
			ctx.Request.SetBodyString(`{"model":"openai/gpt-4o-mini-tts","input":"Hello from the harness.","voice":"alloy"}`)

			r.Handler(&ctx)

			require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
			require.Contains(t, string(ctx.Response.Header.ContentType()), "audio/", "body: %.200s", ctx.Response.Body())
			require.Equal(t, audio, ctx.Response.Body())
		})
	}
}

// The /openai speech routes wrote every audio body as audio/mpeg named speech.mp3 and carried no
// usage. It now shares /v1/audio/speech's writer: the vendor's Content-Type, a matching filename
// and the unified usage in x-bifrost-usage (TXE-6530).
func TestOpenAISpeechRouteLabelsAudioAndCarriesUsage(t *testing.T) {
	audio := []byte("RIFF\x24\x00\x00\x00WAVEfmt ")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(audio)
	}))
	defer upstream.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: speechTestAccount{baseURL: upstream.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	// Both routes reach the router's raw-audio branch: the OpenAI SDK path and the Azure OpenAI
	// SDK deployment path (served by the azure provider).
	for _, uri := range []string{
		"/openai/v1/audio/speech",
		"/openai/openai/deployments/gpt-4o-mini-tts/audio/speech?api-version=2025-04-01-preview",
	} {
		t.Run(uri, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod(fasthttp.MethodPost)
			ctx.Request.SetRequestURI(uri)
			ctx.Request.Header.SetContentType("application/json")
			ctx.Request.SetBodyString(`{"model":"openai/gpt-4o-mini-tts","input":"Hello","voice":"alloy","response_format":"wav"}`)

			r.Handler(&ctx)

			require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
			require.Equal(t, "audio/wav", string(ctx.Response.Header.ContentType()))
			require.Equal(t, "attachment; filename=speech.wav", string(ctx.Response.Header.Peek("Content-Disposition")))
			require.Equal(t, audio, ctx.Response.Body())
			require.JSONEq(t, `{"input_chars":5,"input_tokens":0,"output_tokens":0,"total_tokens":0}`, string(ctx.Response.Header.Peek(lib.HeaderBifrostUsage)))
		})
	}
}

// groqTranscriptionTestAccount configures the Groq provider at a local upstream, so the
// OpenAI drop-in transcription route runs end to end without a live key.
type groqTranscriptionTestAccount struct {
	baseURL string
}

func (a groqTranscriptionTestAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Groq}, nil
}

func (a groqTranscriptionTestAccount) GetKeysForProvider(_ context.Context, p schemas.ModelProvider) ([]schemas.Key, error) {
	if p != schemas.Groq {
		return nil, nil
	}
	return []schemas.Key{{ID: "groq-test-key", Name: "groq-test-key", Value: *schemas.NewSecretVar("gsk-test"), Models: schemas.WhiteList{"*"}, Weight: 1.0}}, nil
}

func (a groqTranscriptionTestAccount) GetConfigForProvider(p schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	if p != schemas.Groq {
		return nil, fmt.Errorf("unsupported provider %s", p)
	}
	nc := schemas.DefaultNetworkConfig
	nc.BaseURL = a.baseURL
	return &schemas.ProviderConfig{NetworkConfig: nc, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// The OpenAI drop-in route /openai/v1/audio/transcriptions reaches the duration-usage backfill
// too (contract C5): a Groq file upload whose verbose_json carries duration and no usage comes
// back with usage{type: duration, seconds}, the vendor's number unchanged. The route still
// requires a file; a URL-only request is not taken there.
func TestOpenAITranscriptionRouteReportsGroqDurationAsUsage(t *testing.T) {
	var calls int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"task":"transcribe","language":"English","duration":12.4,"text":"Your transcribed text appears here.","segments":[],"x_groq":{"id":"req_1"}}`)
	}))
	defer upstream.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: groqTranscriptionTestAccount{baseURL: upstream.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "groq/whisper-large-v3-turbo"))
	require.NoError(t, writer.WriteField("response_format", "verbose_json"))
	part, err := writer.CreateFormFile("file", "sample.mp3")
	require.NoError(t, err)
	_, err = part.Write([]byte("ID3fake-mp3"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	ctx.Request.SetRequestURI("/openai/v1/audio/transcriptions")
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(body.Bytes())
	r.Handler(&ctx)

	require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body: %s", ctx.Response.Body())
	usage := gjson.GetBytes(ctx.Response.Body(), "usage")
	require.Equal(t, "duration", usage.Get("type").String(), "body: %s", ctx.Response.Body())
	require.Equal(t, 12.4, usage.Get("seconds").Float(), "body: %s", ctx.Response.Body())
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls)
}

// azureDataSourcesJSON is an Azure "On Your Data" data_sources value, the
// extension the Azure OpenAI SDK sends at the top level of a chat body.
const azureDataSourcesJSON = `[{"type":"azure_search","parameters":{"endpoint":"https://unreachable.search.windows.net","index_name":"idx","authentication":{"type":"api_key","key":"k"}}}]`

// chatUpstream records the last chat body the provider sent and answers it,
// as SSE when the body asked to stream.
type chatUpstream struct {
	mu   sync.Mutex
	path string
	body []byte
}

func (u *chatUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.path, u.body = r.URL.Path, body
	u.mu.Unlock()
	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}

func (u *chatUpstream) last() (string, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.body
}

// TestAzureDeploymentChatForwardsUnknownTopLevelFields pins that the Azure
// OpenAI SDK route /openai/deployments/{deployment}/chat/completions forwards
// top-level fields Bifrost does not model, such as On Your Data data_sources,
// to the Azure deployment, unary and streaming. They used to be dropped at
// decode, so Azure answered 200 for a body it should have rejected. The plain
// chat routes and a provider-prefixed deployment path keep dropping them.
func TestAzureDeploymentChatForwardsUnknownTopLevelFields(t *testing.T) {
	upstream := &chatUpstream{}
	server := httptest.NewServer(upstream)
	defer server.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: speechTestAccount{baseURL: server.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	const deploymentURI = "/openai/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21"
	cases := []struct {
		name        string
		uri         string
		body        string
		headers     map[string]string
		wantPath    string
		wantFields  map[string]string
		wantMissing []string
	}{
		{
			name:       "azure deployment unary",
			uri:        deploymentURI,
			body:       `{"messages":[{"role":"user","content":"hi"}],"temperature":0.2,"data_sources":` + azureDataSourcesJSON + `,"x_future_field":{"a":1}}`,
			wantPath:   "/openai/v1/chat/completions",
			wantFields: map[string]string{"data_sources": azureDataSourcesJSON, "x_future_field": `{"a":1}`, "temperature": "0.2"},
		},
		{
			name:       "azure deployment stream",
			uri:        deploymentURI,
			body:       `{"messages":[{"role":"user","content":"hi"}],"stream":true,"data_sources":` + azureDataSourcesJSON + `}`,
			wantPath:   "/openai/v1/chat/completions",
			wantFields: map[string]string{"data_sources": azureDataSourcesJSON, "stream": "true"},
		},
		{
			name:        "azure deployment nested extra_params wins on conflict",
			uri:         deploymentURI,
			body:        `{"messages":[{"role":"user","content":"hi"}],"data_sources":` + azureDataSourcesJSON + `,"x_dup":"top","extra_params":{"x_dup":"nested"}}`,
			headers:     map[string]string{"x-bf-passthrough-extra-params": "true"},
			wantPath:    "/openai/v1/chat/completions",
			wantFields:  map[string]string{"data_sources": azureDataSourcesJSON, "x_dup": `"nested"`},
			wantMissing: []string{"extra_params"},
		},
		{
			name:        "azure deployment without unknown fields",
			uri:         deploymentURI,
			body:        `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","fallbacks":["openai/gpt-4o"]}`,
			wantPath:    "/openai/v1/chat/completions",
			wantMissing: []string{"fallbacks", "extra_params"},
		},
		{
			name:        "plain chat route keeps dropping unknown fields",
			uri:         "/openai/v1/chat/completions",
			body:        `{"model":"azure/gpt-4o","messages":[{"role":"user","content":"hi"}],"data_sources":` + azureDataSourcesJSON + `}`,
			wantPath:    "/openai/v1/chat/completions",
			wantMissing: []string{"data_sources"},
		},
		{
			name:        "provider-prefixed deployment path does not forward Azure fields",
			uri:         "/openai/openai/deployments/openai/gpt-4o/chat/completions?api-version=2024-10-21",
			body:        `{"messages":[{"role":"user","content":"hi"}],"data_sources":` + azureDataSourcesJSON + `}`,
			wantPath:    "/v1/chat/completions",
			wantMissing: []string{"data_sources"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod(fasthttp.MethodPost)
			ctx.Request.SetRequestURI(tc.uri)
			ctx.Request.Header.SetContentType("application/json")
			for k, v := range tc.headers {
				ctx.Request.Header.Set(k, v)
			}
			ctx.Request.SetBodyString(tc.body)

			r.Handler(&ctx)
			respBody := string(ctx.Response.Body())
			require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode(), "body: %s", respBody)

			path, sent := upstream.last()
			require.Equal(t, tc.wantPath, path, "upstream body: %s", sent)
			for field, want := range tc.wantFields {
				got := gjson.GetBytes(sent, field)
				require.True(t, got.Exists(), "%s missing from the provider body: %s", field, sent)
				require.JSONEq(t, want, got.Raw, "%s in provider body: %s", field, sent)
			}
			for _, field := range tc.wantMissing {
				require.False(t, gjson.GetBytes(sent, field).Exists(), "%s must not reach the provider: %s", field, sent)
			}
		})
	}
}

// BenchmarkAzureEndpointPreHookChat measures the Azure deployments pre-hook on
// a chat body with no unknown fields (the common case) and with data_sources.
func BenchmarkAzureEndpointPreHookChat(b *testing.B) {
	hook := AzureEndpointPreHook(&mockHandlerStore{})
	bodies := map[string]string{
		"known-only":   `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"temperature":0.2,"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`,
		"data-sources": `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"temperature":0.2,"max_tokens":64,"data_sources":` + azureDataSourcesJSON + `}`,
	}
	for _, name := range []string{"known-only", "data-sources"} {
		body := []byte(bodies[name])
		b.Run(name, func(b *testing.B) {
			var ctx fasthttp.RequestCtx
			ctx.Request.SetBody(body)
			ctx.SetUserValue("deploymentPath", "gpt-4o/chat/completions")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				req := &openai.OpenAIChatRequest{}
				if err := hook(&ctx, bctx, req); err != nil {
					b.Fatal(err)
				}
				bctx.Cancel()
			}
		})
	}
}

// A request Bifrost converted to another API (chat served through Responses, text
// through chat, Responses through chat) carries that API's raw upstream bytes. The
// OpenAI raw passthrough must not echo them: a chat client reading
// {"type":"response.created",...} on a chat stream fails its schema check. Every
// chat, text and Responses converter on every OpenAI route is checked, unary and
// streaming, plus the unconverted control that must still pass raw bytes through.
func TestOpenAIRawPassthroughSkipsConvertedRequests(t *testing.T) {
	foreignRaw := `{"type":"response.created","response":{"id":"resp_1","object":"response"}}`
	extra := func(converted schemas.RequestType) schemas.BifrostResponseExtraFields {
		return schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI, RawResponse: foreignRaw, ConvertedRequestType: converted}
	}
	checked := 0
	for _, prefix := range []string{"", "/openai"} {
		for _, route := range CreateOpenAIRouteConfigs(prefix, &mockHandlerStore{}) {
			for _, converted := range []schemas.RequestType{schemas.ResponsesRequest, ""} {
				name := route.Path + "/converted=" + string(converted)
				wantRaw := converted == ""
				check := func(kind string, got interface{}) {
					t.Helper()
					checked++
					if wantRaw {
						require.Equal(t, foreignRaw, got, "%s %s: unconverted response must pass raw bytes through", name, kind)
					} else {
						require.NotEqual(t, foreignRaw, got, "%s %s: converted response echoed the other API's raw bytes", name, kind)
					}
				}
				if route.ChatResponseConverter != nil {
					got, err := route.ChatResponseConverter(nil, &schemas.BifrostChatResponse{ExtraFields: extra(converted)})
					require.NoError(t, err)
					check("chat", got)
				}
				if route.TextResponseConverter != nil {
					got, err := route.TextResponseConverter(nil, &schemas.BifrostTextCompletionResponse{ExtraFields: extra(converted)})
					require.NoError(t, err)
					check("text", got)
				}
				if route.ResponsesResponseConverter != nil {
					got, err := route.ResponsesResponseConverter(nil, &schemas.BifrostResponsesResponse{ExtraFields: extra(converted)})
					require.NoError(t, err)
					check("responses", got)
				}
				if sc := route.StreamConfig; sc != nil {
					if sc.ChatStreamResponseConverter != nil {
						_, got, err := sc.ChatStreamResponseConverter(nil, &schemas.BifrostChatResponse{ExtraFields: extra(converted)})
						require.NoError(t, err)
						check("chat stream", got)
					}
					if sc.TextStreamResponseConverter != nil {
						_, got, err := sc.TextStreamResponseConverter(nil, &schemas.BifrostTextCompletionResponse{ExtraFields: extra(converted)})
						require.NoError(t, err)
						check("text stream", got)
					}
					if sc.ResponsesStreamResponseConverter != nil {
						_, got, err := sc.ResponsesStreamResponseConverter(nil, &schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeCreated, ExtraFields: extra(converted)})
						require.NoError(t, err)
						check("responses stream", got)
					}
				}
			}
		}
	}
	require.Positive(t, checked, "no OpenAI chat/text/responses converters found")
}

// A response the provider rewrote after decoding (custom tools restored from the
// function tools they were sent as) no longer matches its raw upstream bytes, so
// the raw passthrough must relay the typed response instead, on every OpenAI
// route, unary and streaming.
func TestOpenAIRawPassthroughSkipsRewrittenResponses(t *testing.T) {
	raw := `{"output":[{"type":"function_call","name":"apply_patch"}]}`
	extra := schemas.BifrostResponseExtraFields{Provider: schemas.OpenAI, RawResponse: raw, RawResponseIsNotWire: true}
	checked := 0
	for _, prefix := range []string{"", "/openai"} {
		for _, route := range CreateOpenAIRouteConfigs(prefix, &mockHandlerStore{}) {
			if route.ResponsesResponseConverter != nil {
				got, err := route.ResponsesResponseConverter(nil, &schemas.BifrostResponsesResponse{ExtraFields: extra})
				require.NoError(t, err)
				require.NotEqual(t, raw, got, "%s: rewritten response relayed its raw bytes", route.Path)
				checked++
			}
			if sc := route.StreamConfig; sc != nil && sc.ResponsesStreamResponseConverter != nil {
				_, got, err := sc.ResponsesStreamResponseConverter(nil, &schemas.BifrostResponsesStreamResponse{ExtraFields: extra})
				require.NoError(t, err)
				require.NotEqual(t, raw, got, "%s stream: rewritten event relayed its raw bytes", route.Path)
				checked++
			}
		}
	}
	require.NotZero(t, checked)
}

// TXE-6531 (contract C5): OpenAI's 30m/12h cache-write split, decoded from the vendor body,
// reaches clients of the OpenAI drop-in routes (/openai/v1/chat/completions,
// /openai/v1/responses and the unprefixed SDK routes) both as the unified tiers and in
// OpenAI's own cache_write_tokens_details shape, with and without the float-cost wire wrapper.
func TestOpenAIRoutesCarryCacheWriteTiers(t *testing.T) {
	chatBody := `{"id":"chatcmpl-1","object":"chat.completion","created":1762000000,"model":"gpt-5.6",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":40000,"completion_tokens":1,"total_tokens":40001,"prompt_tokens_details":{"cached_tokens":1024,` +
		`"cache_write_tokens":38000,"cache_write_tokens_details":{"ttl_30m_tokens":30000,"ttl_12h_tokens":8000}}}}`
	respBody := `{"id":"resp_1","object":"response","created_at":1762000000,"status":"completed","model":"gpt-5.6","output":[],` +
		`"usage":{"input_tokens":40000,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":38000,` +
		`"cache_write_tokens_details":{"ttl_30m_tokens":30000,"ttl_12h_tokens":8000}},` +
		`"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":40001}}`
	assertTiers := func(t *testing.T, where string, got interface{}) {
		t.Helper()
		js, err := sonic.Marshal(got)
		require.NoError(t, err, where)
		s := string(js)
		for _, want := range []string{
			`"cached_write_tokens_30m":30000`,
			`"cached_write_tokens_12h":8000`,
			`"cache_write_tokens_details":{"ttl_30m_tokens":30000,"ttl_12h_tokens":8000}`,
		} {
			require.Contains(t, s, want, "%s: %s", where, s)
		}
	}

	checked := 0
	for _, prefix := range []string{"", "/openai"} {
		for _, route := range CreateOpenAIRouteConfigs(prefix, &mockHandlerStore{}) {
			for _, withCost := range []bool{false, true} {
				where := route.Path + map[bool]string{false: " (no cost)", true: " (cost wrapper)"}[withCost]
				if route.ChatResponseConverter != nil {
					var resp schemas.BifrostChatResponse
					require.NoError(t, schemas.Unmarshal([]byte(chatBody), &resp))
					resp.ExtraFields.Provider = schemas.OpenAI
					if withCost {
						resp.Usage.Cost = &schemas.BifrostCost{TotalCost: 0.5}
					}
					got, err := route.ChatResponseConverter(nil, &resp)
					require.NoError(t, err)
					assertTiers(t, where+" chat", got)
					checked++
				}
				if route.ResponsesResponseConverter != nil {
					var resp schemas.BifrostResponsesResponse
					require.NoError(t, schemas.Unmarshal([]byte(respBody), &resp))
					resp.ExtraFields.Provider = schemas.OpenAI
					if withCost {
						resp.Usage.Cost = &schemas.BifrostCost{TotalCost: 0.5}
					}
					got, err := route.ResponsesResponseConverter(nil, &resp)
					require.NoError(t, err)
					assertTiers(t, where+" responses", got)
					checked++
				}
			}
		}
	}
	require.Positive(t, checked, "no OpenAI chat/responses converters found")
}

// TXE-6606 (contract C4): /openai/v1/images/generations and /openai/v1/images/edits with an xai/
// model reach the same provider methods as /v1/images/*. These tests drive each route's own
// parser and converter into the xAI provider and pin what reaches xAI's wire.

var openAIXAIPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)

type openAIXAIEgress struct {
	mu     sync.Mutex
	paths  []string
	ctypes []string
	bodies []map[string]any
}

func (e *openAIXAIEgress) provider(t *testing.T) *xai.XAIProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.paths = append(e.paths, r.URL.Path)
		e.ctypes = append(e.ctypes, r.Header.Get("Content-Type"))
		e.bodies = append(e.bodies, body)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1762761600,"data":[{"url":"https://imgen.x.ai/a.jpg"}],"usage":{"cost_in_usd_ticks":400000000}}`))
	}))
	t.Cleanup(server.Close)
	provider, err := xai.NewXAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	return provider
}

func (e *openAIXAIEgress) only(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Len(t, e.paths, 1, "exactly one upstream call")
	return e.paths[0], e.ctypes[0], e.bodies[0]
}

func openAIImageRoute(t *testing.T, path string) RouteConfig {
	t.Helper()
	for _, route := range CreateOpenAIRouteConfigs("/openai", nil) {
		if route.Path == path {
			return route
		}
	}
	t.Fatalf("route %s not registered", path)
	return RouteConfig{}
}

var openAIXAIKey = schemas.Key{Value: *schemas.NewSecretVar("xai-test")}

func TestOpenAIRouteXAIImageGenerationIngressToEgress(t *testing.T) {
	route := openAIImageRoute(t, "/openai/v1/images/generations")
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := route.GetRequestTypeInstance(bifrostCtx)
	require.NoError(t, parseJSONRequestBody([]byte(
		`{"model":"xai/grok-imagine-image-2.0","prompt":"a lighthouse","n":2,"quality":"medium","resolution":"2K","aspect_ratio":"16:9","response_format":"url"}`), req))
	converted, err := route.RequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, converted.ImageGenerationRequest)
	require.Equal(t, schemas.XAI, converted.ImageGenerationRequest.Provider)

	egress := &openAIXAIEgress{}
	response, bifrostErr := egress.provider(t).ImageGeneration(bifrostCtx, openAIXAIKey, converted.ImageGenerationRequest)
	require.Nil(t, bifrostErr)
	require.Len(t, response.Data, 1)

	path, ctype, body := egress.only(t)
	require.Equal(t, "/v1/images/generations", path)
	require.Equal(t, "application/json", ctype)
	require.Equal(t, "grok-imagine-image-2.0", body["model"])
	require.Equal(t, "medium", body["quality"])
	require.Equal(t, "2k", body["resolution"])
	require.Equal(t, "16:9", body["aspect_ratio"])
	require.Equal(t, "url", body["response_format"])
	require.EqualValues(t, 2, body["n"])
}

func TestOpenAIRouteXAIImageEditIngressToEgress(t *testing.T) {
	route := openAIImageRoute(t, "/openai/v1/images/edits")

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for k, v := range map[string]string{
		"model":        "xai/grok-imagine-image-2.0",
		"prompt":       "make it night",
		"quality":      "low",
		"resolution":   "1K",
		"aspect_ratio": "4:3",
		"n":            "1",
	} {
		require.NoError(t, writer.WriteField(k, v))
	}
	part, err := writer.CreateFormFile("image", "day.png")
	require.NoError(t, err)
	_, err = part.Write(openAIXAIPNG)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(http.MethodPost)
	httpCtx.Request.SetRequestURI("/openai/v1/images/edits")
	httpCtx.Request.Header.SetContentType(writer.FormDataContentType())
	httpCtx.Request.SetBody(form.Bytes())

	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := route.GetRequestTypeInstance(bifrostCtx)
	require.NotNil(t, route.RequestParser, "the edit route parses multipart")
	require.NoError(t, route.RequestParser(httpCtx, req))
	converted, err := route.RequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, converted.ImageEditRequest)
	require.Equal(t, schemas.XAI, converted.ImageEditRequest.Provider)

	egress := &openAIXAIEgress{}
	_, bifrostErr := egress.provider(t).ImageEdit(bifrostCtx, openAIXAIKey, converted.ImageEditRequest)
	require.Nil(t, bifrostErr)

	path, ctype, body := egress.only(t)
	require.Equal(t, "/v1/images/edits", path)
	require.Equal(t, "application/json", ctype, "xAI's edit takes JSON, never multipart")
	require.Equal(t, map[string]any{
		"url":  "data:image/png;base64," + base64.StdEncoding.EncodeToString(openAIXAIPNG),
		"type": "image_url",
	}, body["image"])
	require.Equal(t, "low", body["quality"])
	require.Equal(t, "1k", body["resolution"], "the route must not drop the price-affecting tier")
	require.Equal(t, "4:3", body["aspect_ratio"])
	require.EqualValues(t, 1, body["n"])
}

// TestOpenAIVideoContentRouteStreams is contract C9 / BF20 on the OpenAI-SDK routes: the three
// /videos/{id}/content paths forward the file as a body stream, read from the vendor after the
// handler has returned, on one upstream call. They used to hold the whole file before answering.
func TestOpenAIVideoContentRouteStreams(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("FIRST-HALF|"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("SECOND-HALF"))
	}))
	defer upstream.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: speechTestAccount{baseURL: upstream.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	for _, uri := range []string{
		"/openai/v1/videos/video_a:openai/content",
		"/openai/videos/video_a:openai/content?variant=thumbnail",
		"/openai/openai/videos/video_a:openai/content",
	} {
		t.Run(uri, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.SetMethod(fasthttp.MethodGet)
			ctx.Request.SetRequestURI(uri)

			r.Handler(&ctx)

			// Response.Body() would read the stream to its end, so nothing here may call it.
			require.Equal(t, fasthttp.StatusOK, ctx.Response.StatusCode())
			require.True(t, ctx.Response.IsBodyStream(), "the file must be a body stream, not bytes held by the router")
			require.Equal(t, "video/mp4", string(ctx.Response.Header.ContentType()))
			body, err := io.ReadAll(ctx.Response.BodyStream())
			require.NoError(t, err, "the request context must outlive the handler for the stream to be read")
			require.Equal(t, "FIRST-HALF|SECOND-HALF", string(body))
			require.NoError(t, ctx.Response.CloseBodyStream())
		})
	}
	require.Equal(t, []string{
		"GET /v1/videos/video_a/content?variant=video",
		"GET /v1/videos/video_a/content?variant=thumbnail",
		"GET /v1/videos/video_a/content?variant=video",
	}, calls)
}

// TXE-6984 (contract C4, C5): /openai/v1/images/edits and /openai/v1/images/generations reach the
// OpenAI provider through their own parser and converters. These tests drive each route's parser
// and request converter into the provider and its stream converter back out, and pin that an
// edit carrying a unified field OpenAI's edit API has no field for is refused before any
// upstream call, and that the completed frame the route writes carries usage.output_images.

type openAIImageUpstream struct {
	mu    sync.Mutex
	paths []string
}

func (u *openAIImageUpstream) provider(t *testing.T, sse string) *openai.OpenAIProvider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		if sse != "" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGk="}]}`))
	}))
	t.Cleanup(server.Close)
	return openai.NewOpenAIProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 30},
	}, bifrost.NewNoOpLogger())
}

func (u *openAIImageUpstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

// openAIRouteImageEdit parses a multipart edit on the /openai/v1/images/edits route and returns
// the Bifrost request its converter builds.
func openAIRouteImageEdit(t *testing.T, bifrostCtx *schemas.BifrostContext, fields map[string]string) *schemas.BifrostImageEditRequest {
	t.Helper()
	route := openAIImageRoute(t, "/openai/v1/images/edits")
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	part, err := writer.CreateFormFile("image", "day.png")
	require.NoError(t, err)
	_, err = part.Write(openAIXAIPNG)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(http.MethodPost)
	httpCtx.Request.SetRequestURI("/openai/v1/images/edits")
	httpCtx.Request.Header.SetContentType(writer.FormDataContentType())
	httpCtx.Request.SetBody(form.Bytes())

	req := route.GetRequestTypeInstance(bifrostCtx)
	require.NoError(t, route.RequestParser(httpCtx, req))
	converted, err := route.RequestConverter(bifrostCtx, req)
	require.NoError(t, err)
	require.NotNil(t, converted.ImageEditRequest)
	require.Equal(t, schemas.OpenAI, converted.ImageEditRequest.Provider)
	return converted.ImageEditRequest
}

func openAIImagePostHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return resp, err
}

var openAIImageKey = schemas.Key{Value: *schemas.NewSecretVar("openai-test")}

func TestOpenAIRouteImageEditRefusesUndeliverableFields(t *testing.T) {
	for param, value := range map[string]string{
		"aspect_ratio":      "16:9",
		"resolution":        "2K",
		"target_megapixels": "4",
		"type":              "upscale",
		"upscale_factor":    "2",
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s stream=%v", param, stream), func(t *testing.T) {
				bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				fields := map[string]string{"model": "openai/gpt-image-1.5", "prompt": "add a hat", param: value}
				if stream {
					fields["stream"] = "true"
				}
				request := openAIRouteImageEdit(t, bifrostCtx, fields)

				upstream := &openAIImageUpstream{}
				provider := upstream.provider(t, "")
				var bifrostErr *schemas.BifrostError
				if stream {
					_, bifrostErr = provider.ImageEditStream(bifrostCtx, openAIImagePostHook, nil, openAIImageKey, request)
				} else {
					_, bifrostErr = provider.ImageEdit(bifrostCtx, openAIImageKey, request)
				}
				require.NotNil(t, bifrostErr, "the edit must be refused")
				require.NotNil(t, bifrostErr.StatusCode)
				require.Equal(t, http.StatusBadRequest, *bifrostErr.StatusCode)
				require.Equal(t, schemas.ErrorTypeCallerUnsupportedParameter, bifrostErr.ResolveErrorType())
				require.NotNil(t, bifrostErr.Error)
				require.NotNil(t, bifrostErr.Error.Param)
				require.Equal(t, param, bifrostErr.Error.Param)
				require.Empty(t, upstream.calls(), "no upstream call for a refused edit")

				// The route writes the error as it is: status, error_type and param reach the client.
				route := openAIImageRoute(t, "/openai/v1/images/edits")
				wire, err := sonic.Marshal(route.ErrorConverter(bifrostCtx, bifrostErr))
				require.NoError(t, err)
				require.Equal(t, param, gjson.GetBytes(wire, "error.param").String(), string(wire))
				require.Equal(t, "caller_unsupported_parameter", gjson.GetBytes(wire, "extra_fields.error_type").String(), string(wire))
			})
		}
	}
}

func TestOpenAIRouteImageStreamReportsOutputImages(t *testing.T) {
	for _, kind := range []string{"image_generation", "image_edit"} {
		t.Run(kind, func(t *testing.T) {
			sse := "data: " + `{"type":"` + kind + `.partial_image","b64_json":"cDA=","partial_image_index":0,"sequence_number":1}` + "\n\n" +
				"data: " + `{"type":"` + kind + `.partial_image","b64_json":"cDE=","partial_image_index":1,"sequence_number":2}` + "\n\n" +
				"data: " + `{"type":"` + kind + `.completed","sequence_number":3,"b64_json":"aGk=","usage":{"input_tokens":12,"output_tokens":272,"total_tokens":284}}` + "\n\n"
			upstream := &openAIImageUpstream{}
			provider := upstream.provider(t, sse)
			bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			var route RouteConfig
			var stream chan *schemas.BifrostStreamChunk
			var bifrostErr *schemas.BifrostError
			wantPath := "/v1/images/generations"
			if kind == "image_generation" {
				route = openAIImageRoute(t, "/openai/v1/images/generations")
				req := route.GetRequestTypeInstance(bifrostCtx)
				require.NoError(t, parseJSONRequestBody([]byte(
					`{"model":"openai/gpt-image-1.5","prompt":"a fox","stream":true,"partial_images":2}`), req))
				converted, err := route.RequestConverter(bifrostCtx, req)
				require.NoError(t, err)
				require.NotNil(t, converted.ImageGenerationRequest)
				require.Equal(t, schemas.OpenAI, converted.ImageGenerationRequest.Provider)
				stream, bifrostErr = provider.ImageGenerationStream(bifrostCtx, openAIImagePostHook, nil, openAIImageKey, converted.ImageGenerationRequest)
			} else {
				route = openAIImageRoute(t, "/openai/v1/images/edits")
				wantPath = "/v1/images/edits"
				request := openAIRouteImageEdit(t, bifrostCtx, map[string]string{
					"model": "openai/gpt-image-1.5", "prompt": "add a hat", "stream": "true", "partial_images": "2",
				})
				stream, bifrostErr = provider.ImageEditStream(bifrostCtx, openAIImagePostHook, nil, openAIImageKey, request)
			}
			require.Nil(t, bifrostErr)
			require.NotNil(t, route.StreamConfig)

			var frames [][]byte
			for chunk := range stream {
				require.NotNil(t, chunk)
				require.Nil(t, chunk.BifrostError)
				require.NotNil(t, chunk.BifrostImageGenerationStreamResponse)
				_, payload, err := route.StreamConfig.ImageGenerationStreamResponseConverter(bifrostCtx, chunk.BifrostImageGenerationStreamResponse)
				require.NoError(t, err)
				wire, err := sonic.Marshal(payload)
				require.NoError(t, err)
				frames = append(frames, wire)
			}
			require.Len(t, frames, 3, "two previews and the completed frame")
			for _, preview := range frames[:2] {
				require.False(t, gjson.GetBytes(preview, "usage").Exists(), string(preview))
			}
			final := frames[2]
			require.Equal(t, kind+".completed", gjson.GetBytes(final, "type").String(), string(final))
			require.True(t, gjson.GetBytes(final, "usage.output_images").Exists(), string(final))
			require.EqualValues(t, 1, gjson.GetBytes(final, "usage.output_images").Int(), string(final))
			require.EqualValues(t, 284, gjson.GetBytes(final, "usage.total_tokens").Int(), string(final))
			require.Equal(t, []string{wantPath}, upstream.calls())
		})
	}
}

// bytePlusVideoUpstream records each body that reaches BytePlus's task create and answers it.
type bytePlusVideoUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (u *bytePlusVideoUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.URL.Path != "/api/v3/contents/generations/tasks" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	u.mu.Lock()
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"cgt-7126"}`)
}

func (u *bytePlusVideoUpstream) take() []map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	bodies := u.bodies
	u.bodies = nil
	return bodies
}

type bytePlusVideoAccount struct{ url string }

func (a bytePlusVideoAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.BytePlus}, nil
}

func (a bytePlusVideoAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "bp-key", Name: "bp-key", Value: *schemas.NewSecretVar("placeholder"), Models: schemas.WhiteList{"*"}, Weight: 1.0}}, nil
}

func (a bytePlusVideoAccount) GetConfigForProvider(_ schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	nc := schemas.DefaultNetworkConfig
	nc.BaseURL = a.url
	nc.MaxRetries = 0
	nc.DefaultRequestTimeoutInSeconds = 5
	nc.AllowPrivateNetwork = true
	return &schemas.ProviderConfig{NetworkConfig: nc, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}, nil
}

// TestOpenAIVideoMultipartDeliversEveryField sends each video create field as a multipart
// /openai/v1/videos request to BytePlus (fork, TXE-7126, contract C4). Each one either reaches
// BytePlus's body in BytePlus's own field, or is refused with a 400 naming it before any call.
// Before, the multipart parser kept only seconds, size, negative_prompt, seed and video_uri, and
// dropped every other field without an error.
func TestOpenAIVideoMultipartDeliversEveryField(t *testing.T) {
	upstream := &bytePlusVideoUpstream{}
	server := httptest.NewServer(upstream)
	defer server.Close()

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: bytePlusVideoAccount{url: server.URL},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	require.NoError(t, err)
	defer client.Shutdown()

	r := router.New()
	identity := func(next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }
	NewOpenAIRouter(client, &mockHandlerStore{}, nil, &testLogger{}).RegisterRoutes(r, identity)

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	type file struct{ field, name string }
	post := func(t *testing.T, passthrough bool, values [][2]string, files []file) (int, map[string]any) {
		t.Helper()
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		require.NoError(t, writer.WriteField("model", "byteplus/dreamina-seedance-2-5-260628"))
		require.NoError(t, writer.WriteField("prompt", "a paper boat"))
		for _, v := range values {
			require.NoError(t, writer.WriteField(v[0], v[1]))
		}
		for _, f := range files {
			part, err := writer.CreateFormFile(f.field, f.name)
			require.NoError(t, err)
			_, err = part.Write(png)
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod(fasthttp.MethodPost)
		ctx.Request.SetRequestURI("/openai/v1/videos")
		if passthrough {
			ctx.Request.Header.Set("x-bf-passthrough-extra-params", "true")
		}
		ctx.Request.Header.SetContentType(writer.FormDataContentType())
		ctx.Request.SetBody(form.Bytes())
		r.Handler(&ctx)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(ctx.Response.Body(), &resp), "response: %s", ctx.Response.Body())
		return ctx.Response.StatusCode(), resp
	}
	contentRoles := func(body map[string]any) map[string]string {
		roles := map[string]string{}
		items, _ := body["content"].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			role, _ := m["role"].(string)
			if image, ok := m["image_url"].(map[string]any); ok {
				roles[role], _ = image["url"].(string)
			}
			if video, ok := m["video_url"].(map[string]any); ok {
				roles[role], _ = video["url"].(string)
			}
		}
		return roles
	}

	delivered := []struct {
		name        string
		passthrough bool
		values      [][2]string
		files       []file
		check       func(t *testing.T, body map[string]any)
	}{
		{name: "seconds", values: [][2]string{{"seconds", "5"}}, check: func(t *testing.T, b map[string]any) { assert.EqualValues(t, 5, b["duration"]) }},
		{name: "size", values: [][2]string{{"size", "1280x720"}}, check: func(t *testing.T, b map[string]any) {
			assert.Equal(t, "720p", b["resolution"])
			assert.Equal(t, "16:9", b["ratio"])
		}},
		{name: "resolution and aspect_ratio", values: [][2]string{{"resolution", "1080p"}, {"aspect_ratio", "9:16"}}, check: func(t *testing.T, b map[string]any) {
			assert.Equal(t, "1080p", b["resolution"])
			assert.Equal(t, "9:16", b["ratio"])
		}},
		{name: "seed", values: [][2]string{{"seed", "7"}}, check: func(t *testing.T, b map[string]any) { assert.EqualValues(t, 7, b["seed"]) }},
		{name: "audio", values: [][2]string{{"audio", "true"}}, check: func(t *testing.T, b map[string]any) { assert.Equal(t, true, b["generate_audio"]) }},
		{name: "output_format", values: [][2]string{{"output_format", "mp4"}}, check: func(t *testing.T, b map[string]any) { assert.Equal(t, "mp4", b["output_format"]) }},
		{name: "callback_url", values: [][2]string{{"callback_url", "https://hooks.example.test/7126"}}, check: func(t *testing.T, b map[string]any) {
			assert.Equal(t, "https://hooks.example.test/7126", b["callback_url"])
		}},
		{name: "expires_after_seconds", values: [][2]string{{"expires_after_seconds", "7200"}}, check: func(t *testing.T, b map[string]any) {
			assert.EqualValues(t, 7200, b["execution_expires_after"])
		}},
		{name: "input_reference file", files: []file{{"input_reference", "first.png"}}, check: func(t *testing.T, b map[string]any) {
			assert.Contains(t, contentRoles(b)["first_frame"], "data:image/png;base64,")
		}},
		{name: "last_frame as a URL", values: [][2]string{{"last_frame", "https://img.example.test/last.png"}}, check: func(t *testing.T, b map[string]any) {
			assert.Equal(t, "https://img.example.test/last.png", contentRoles(b)["last_frame"])
		}},
		{name: "last_frame as a file", files: []file{{"last_frame", "last.png"}}, check: func(t *testing.T, b map[string]any) {
			assert.Contains(t, contentRoles(b)["last_frame"], "data:image/png;base64,")
		}},
		{name: "reference_images as URLs and a file", values: [][2]string{{"reference_images[]", "https://img.example.test/a.png"}, {"reference_images[]", "https://img.example.test/b.png"}}, files: []file{{"reference_images[]", "c.png"}}, check: func(t *testing.T, b map[string]any) {
			var refs []string
			for _, item := range b["content"].([]any) {
				m := item.(map[string]any)
				if m["role"] == "reference_image" {
					refs = append(refs, m["image_url"].(map[string]any)["url"].(string))
				}
			}
			require.Len(t, refs, 3, "content: %v", b["content"])
			assert.Equal(t, "https://img.example.test/a.png", refs[0])
			assert.Equal(t, "https://img.example.test/b.png", refs[1])
			assert.Contains(t, refs[2], "data:image/png;base64,")
		}},
		{name: "video_uri", values: [][2]string{{"video_uri", "https://media.example.test/source.mp4"}}, check: func(t *testing.T, b map[string]any) {
			assert.Equal(t, "https://media.example.test/source.mp4", contentRoles(b)["reference_video"])
		}},
		// Contract C4 exempts negative_prompt (schemas.UnpricedUnifiedFields): Ark has no field for
		// it, so it is parsed and accepted, and BytePlus may ignore it.
		{name: "negative_prompt, which C4 lets BytePlus ignore", values: [][2]string{{"negative_prompt", "blur"}}, check: func(t *testing.T, b map[string]any) {
			assert.NotContains(t, b, "negative_prompt")
		}},
		{name: "a vendor field under passthrough keeps its JSON type", passthrough: true, values: [][2]string{{"watermark", "false"}, {"camera_fixed", "true"}, {"service_tier", "flex"}}, check: func(t *testing.T, b map[string]any) {
			assert.Equal(t, false, b["watermark"])
			assert.Equal(t, true, b["camera_fixed"])
			assert.Equal(t, "flex", b["service_tier"])
		}},
	}
	for _, tc := range delivered {
		t.Run("delivers "+tc.name, func(t *testing.T) {
			status, resp := post(t, tc.passthrough, tc.values, tc.files)
			require.Equal(t, http.StatusOK, status, "response: %v", resp)
			bodies := upstream.take()
			require.Len(t, bodies, 1)
			tc.check(t, bodies[0])
		})
	}

	refused := []struct {
		name, param string
		values      [][2]string
		files       []file
	}{
		{name: "client_reference_id", param: "client_reference_id", values: [][2]string{{"client_reference_id", "mine-1"}}},
		{name: "output_storage_uri", param: "output_storage_uri", values: [][2]string{{"output_storage_uri", "s3://bucket/out"}}},
		{name: "upscale_factor", param: "upscale_factor", values: [][2]string{{"upscale_factor", "2"}}},
		{name: "target_megapixels", param: "target_megapixels", values: [][2]string{{"target_megapixels", "4"}}},
		{name: "a seed that is not a number", param: "seed", values: [][2]string{{"seed", "seven"}}},
		{name: "audio that is not a boolean", param: "audio", values: [][2]string{{"audio", "loud"}}},
		{name: "a vendor field without passthrough", param: "watermark", values: [][2]string{{"watermark", "false"}}},
		{name: "a file field the create does not read", param: "mask", files: []file{{"mask", "mask.png"}}},
		{name: "last_frame as both a URL and a file", param: "last_frame", values: [][2]string{{"last_frame", "https://img.example.test/last.png"}}, files: []file{{"last_frame", "last.png"}}},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			status, resp := post(t, false, tc.values, tc.files)
			require.Equal(t, http.StatusBadRequest, status, "response: %v", resp)
			errField, _ := resp["error"].(map[string]any)
			assert.Equal(t, tc.param, errField["param"], "response: %v", resp)
			extra, _ := resp["extra_fields"].(map[string]any)
			assert.Equal(t, string(schemas.ErrorTypeCallerUnsupportedParameter), extra["error_type"], "response: %v", resp)
			assert.Empty(t, upstream.take(), "a refused request reached BytePlus")
		})
	}
}

// BenchmarkOpenAIVideoMultipartParse is the multipart /openai/v1/videos parser on its own (fork,
// TXE-7126): an ordinary SDK create (model, prompt, seconds, size and an input_reference file),
// and one that also carries unified fields, task control fields and vendor fields under
// x-bf-passthrough-extra-params. Each iteration parses a fresh request, as the router does.
func BenchmarkOpenAIVideoMultipartParse(b *testing.B) {
	build := func(values [][2]string) ([]byte, string) {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		for _, v := range values {
			_ = writer.WriteField(v[0], v[1])
		}
		part, _ := writer.CreateFormFile("input_reference", "first.png")
		_, _ = part.Write(bytes.Repeat([]byte("\x89PNG\r\n\x1a\n"), 512))
		_ = writer.Close()
		return form.Bytes(), writer.FormDataContentType()
	}
	ordinary := [][2]string{{"model", "openai/sora-2"}, {"prompt", "a paper boat"}, {"seconds", "8"}, {"size", "1280x720"}}
	cases := []struct {
		name        string
		values      [][2]string
		passthrough bool
	}{
		{name: "ordinary", values: ordinary},
		{name: "unified, control and vendor fields", passthrough: true, values: append(append([][2]string{}, ordinary...),
			[2]string{"aspect_ratio", "16:9"}, [2]string{"resolution", "720p"}, [2]string{"last_frame", "https://img.example.test/last.png"},
			[2]string{"callback_url", "https://hooks.example.test/v"}, [2]string{"expires_after_seconds", "7200"},
			[2]string{"watermark", "false"}, [2]string{"service_tier", "flex"})},
	}
	for _, c := range cases {
		body, contentType := build(c.values)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var ctx fasthttp.RequestCtx
				ctx.Request.Header.SetMethod(fasthttp.MethodPost)
				ctx.Request.Header.SetContentType(contentType)
				if c.passthrough {
					ctx.Request.Header.Set("x-bf-passthrough-extra-params", "true")
				}
				ctx.Request.SetBody(body)
				var req openai.OpenAIVideoGenerationRequest
				if err := parseOpenAIVideoGenerationMultipartRequest(&ctx, &req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
