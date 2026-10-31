package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/typesafe"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// The raw systemone body is kept before post-hooks run, so it is relayed only while it
// still carries the processed answers; once a post-hook changes an answer the client gets
// the processed response instead.
func TestTypesafeDecisionConverterRelaysRawOnlyWhileAnswersMatch(t *testing.T) {
	var convert DecisionResponseConverter
	for _, route := range CreateTypesafeRouteConfigs("/typesafe") {
		if route.DecisionResponseConverter != nil {
			convert = route.DecisionResponseConverter
		}
	}
	if convert == nil {
		t.Fatal("no Typesafe decision route")
	}
	raw := json.RawMessage(`{"model":"jev-1.13.0","answers":{"is_spam":{"type":"noul","noul":0.9}}}`)
	response := func(value float64) *schemas.BifrostDecisionResponse {
		return &schemas.BifrostDecisionResponse{
			Model:   "jev-1.13.0",
			Answers: []schemas.DecisionAnswer{{Type: schemas.DecisionTypePredicate, Name: schemas.Ptr("is_spam"), Probability: schemas.Ptr(value)}},
			ExtraFields: schemas.BifrostResponseExtraFields{
				Provider:    schemas.Typesafe,
				RawResponse: raw,
			},
		}
	}

	got, err := convert(nil, response(0.9))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, isRaw := got.(json.RawMessage); !isRaw {
		t.Fatalf("unchanged answers: got %T, want the raw body", got)
	}

	got, err = convert(nil, response(0.1))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, isRaw := got.(json.RawMessage); isRaw {
		t.Fatal("a post-hook's changed answer was dropped in favour of the raw body")
	}
	body, _ := json.Marshal(got)
	if want := `"noul":0.1`; !json.Valid(body) || !strings.Contains(string(body), want) {
		t.Fatalf("converted body %s does not carry %s", body, want)
	}
}

// TestTypesafeSystemOneRouteRendersPostHookAnswerChanges pins that a retained native body is
// relayed only while it still says what the processed response says: an LLM post-hook that
// changes a normalized answer must reach the client, so the route renders the processed
// answers instead of the stale native body.
func TestTypesafeSystemOneRouteRendersPostHookAnswerChanges(t *testing.T) {
	var route *RouteConfig
	for _, r := range CreateTypesafeRouteConfigs("/typesafe") {
		if r.Path == "/typesafe/v1/systemone" {
			rc := r
			route = &rc
		}
	}
	if route == nil {
		t.Fatal("systemone route not registered")
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	native := `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.99}},"usage":{"input_tokens":3,"output_tokens":0}}`
	resp := &schemas.BifrostDecisionResponse{
		Model:          "jev-1.13.0",
		Answers:        []schemas.DecisionAnswer{{Type: schemas.DecisionTypePredicate, Name: schemas.Ptr("q"), Probability: schemas.Ptr(0.01)}},
		NativeResponse: json.RawMessage(native),
	}
	resp.ExtraFields.Provider = schemas.Typesafe

	out, err := route.DecisionResponseConverter(ctx, resp)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got, ok := out.(json.RawMessage); ok {
		t.Fatalf("a post-hook's answer change was overridden by the retained native body: %s", got)
	}
	rendered, ok := out.(*typesafe.TypesafeDecisionResponse)
	if !ok {
		t.Fatalf("expected a rendered native response, got %T", out)
	}
	if answer := rendered.Answers["q"]; answer.Noul == nil || *answer.Noul != 0.01 {
		t.Errorf("rendered answer = %+v, want the post-hook's 0.01", answer)
	}
}

// typesafeRouteAccount points the Typesafe provider at a test upstream.
type typesafeRouteAccount struct{ baseURL string }

func (typesafeRouteAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Typesafe}, nil
}

func (typesafeRouteAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "k1", Name: "k1", Value: *schemas.NewSecretVar("sk-test"), Models: schemas.WhiteList{"*"}, Weight: 1}}, nil
}

func (a typesafeRouteAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	cfg := &schemas.ProviderConfig{NetworkConfig: schemas.DefaultNetworkConfig, ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize}
	cfg.NetworkConfig.BaseURL = a.baseURL
	return cfg, nil
}

// typesafeAnswerHook is an LLM post-hook that rewrites the first answer when set.
type typesafeAnswerHook struct{ probability *float64 }

func (typesafeAnswerHook) GetName() string { return "typesafe-answer-hook" }
func (typesafeAnswerHook) Cleanup() error  { return nil }
func (typesafeAnswerHook) PreRequestHook(*schemas.BifrostContext, *schemas.BifrostRequest) error {
	return nil
}
func (typesafeAnswerHook) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}
func (h typesafeAnswerHook) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if h.probability != nil && resp != nil && resp.DecisionResponse != nil {
		resp.DecisionResponse.Answers[0].Probability = h.probability
	}
	return resp, bifrostErr, nil
}

// TestTypesafeSystemOneRouteThroughTheProviderAndHooks runs the real provider against a test
// upstream with a real post-hook, so the body the converter judges is the one the provider
// kept: unchanged answers relay it byte for byte, a hook's change is rendered with none of it,
// and an upstream refusal keeps its native error body.
func TestTypesafeSystemOneRouteThroughTheProviderAndHooks(t *testing.T) {
	const native = `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.9,"rationale":"stale-native-only"}},"usage":{"input_tokens":3,"output_tokens":0,"billed":true}}`
	const nativeErr = `{"detail":[{"type":"missing","loc":["body","state"],"msg":"Field required"}]}`
	var refuse atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if refuse.Load() {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(nativeErr))
			return
		}
		_, _ = w.Write([]byte(native))
	}))
	defer upstream.Close()

	var route *RouteConfig
	for _, r := range CreateTypesafeRouteConfigs("/typesafe") {
		if r.Path == "/typesafe/v1/systemone" {
			rc := r
			route = &rc
		}
	}
	if route == nil {
		t.Fatal("systemone route not registered")
	}
	call := func(t *testing.T, hook typesafeAnswerHook) (*schemas.BifrostContext, *schemas.BifrostDecisionResponse, *schemas.BifrostError) {
		t.Helper()
		client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
			Account:    typesafeRouteAccount{baseURL: upstream.URL},
			LLMPlugins: []schemas.LLMPlugin{hook},
			Logger:     bifrost.NewDefaultLogger(schemas.LogLevelError),
		})
		if err != nil {
			t.Fatalf("init bifrost: %v", err)
		}
		t.Cleanup(client.Shutdown)
		incoming := route.GetRequestTypeInstance(context.Background())
		body := []byte(`{"model":"jev-1.13.0","state":{"text":"hello"},"questions":{"q":{"type":"noul","instructions":"Is it spam?"}}}`)
		if err := parseJSONRequestBody(body, incoming); err != nil {
			t.Fatalf("parse: %v", err)
		}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		converted, err := route.RequestConverter(ctx, incoming)
		if err != nil {
			t.Fatalf("convert request: %v", err)
		}
		resp, bifrostErr := client.DecisionRequest(ctx, converted.DecisionRequest)
		return ctx, resp, bifrostErr
	}

	t.Run("unchanged answers relay the body as sent", func(t *testing.T) {
		ctx, resp, bifrostErr := call(t, typesafeAnswerHook{})
		if bifrostErr != nil {
			t.Fatalf("decision: %+v", bifrostErr.Error)
		}
		out, err := route.DecisionResponseConverter(ctx, resp)
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if got, ok := out.(json.RawMessage); !ok || string(got) != native {
			t.Fatalf("got %#v, want the upstream body verbatim", out)
		}
	})
	t.Run("a hook's change is rendered without the stale body", func(t *testing.T) {
		ctx, resp, bifrostErr := call(t, typesafeAnswerHook{probability: schemas.Ptr(0.1)})
		if bifrostErr != nil {
			t.Fatalf("decision: %+v", bifrostErr.Error)
		}
		out, err := route.DecisionResponseConverter(ctx, resp)
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		body, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(body), "stale-native-only") || strings.Contains(string(body), `"noul":0.9`) {
			t.Fatalf("the stale native body reached the client: %s", body)
		}
		if !strings.Contains(string(body), `"noul":0.1`) {
			t.Fatalf("the hook's answer is missing: %s", body)
		}
	})
	t.Run("an upstream refusal keeps its native error body", func(t *testing.T) {
		refuse.Store(true)
		defer refuse.Store(false)
		ctx, _, bifrostErr := call(t, typesafeAnswerHook{probability: schemas.Ptr(0.1)})
		if bifrostErr == nil {
			t.Fatal("want the upstream refusal")
		}
		if got, ok := route.ErrorConverter(ctx, bifrostErr).(json.RawMessage); !ok || string(got) != nativeErr {
			t.Fatalf("got %#v, want the upstream error body verbatim", route.ErrorConverter(ctx, bifrostErr))
		}
	})
}

// TestSDKFidelityTypesafeSystemOneRouteNullStateAndExtensions pins #7599 on the
// native /typesafe/v1/systemone route: an SDK-valid {"state": null} converts
// instead of failing with "failed to convert request to Bifrost format", and
// additional native request properties (classifier.dev "images") are marked to
// reach the provider wire so an image request never degrades to text-only.
func TestSDKFidelityTypesafeSystemOneRouteNullStateAndExtensions(t *testing.T) {
	var route *RouteConfig
	for i := range CreateTypesafeRouteConfigs("/typesafe") {
		if r := CreateTypesafeRouteConfigs("/typesafe")[i]; r.Path == "/typesafe/v1/systemone" {
			route = &r
			break
		}
	}
	if route == nil {
		t.Fatal("systemone route not registered")
	}

	incoming := route.GetRequestTypeInstance(context.Background())
	body := []byte(`{"model":"jev-1.13.0","state":null,"images":["data:image/png;base64,iVBORw0KGgo="],"questions":{"q":{"type":"noul","instructions":"Does the image contain text?"}}}`)
	if err := parseJSONRequestBody(body, incoming); err != nil {
		t.Fatalf("parse: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	converted, err := route.RequestConverter(ctx, incoming)
	if err != nil {
		t.Fatalf("null state is SDK-valid and must convert, got %v", err)
	}
	if converted == nil || converted.DecisionRequest == nil {
		t.Fatal("decision request not produced")
	}
	if !converted.DecisionRequest.Input.IsEmpty() {
		t.Errorf("null state must stay null, got %#v", converted.DecisionRequest.Input)
	}
	if passthrough, _ := ctx.Value(schemas.BifrostContextKeyPassthroughExtraParams).(bool); !passthrough {
		t.Error("native extensions present but passthrough not requested; images would be dropped on the wire")
	}
}

// TestSDKFidelityTypesafeSystemOneRouteRelaysNativeBodies pins that the native
// route hands the endpoint's own success and error bodies back verbatim when
// the provider kept them, and rebuilds the native shape otherwise.
func TestSDKFidelityTypesafeSystemOneRouteRelaysNativeBodies(t *testing.T) {
	var route *RouteConfig
	for _, r := range CreateTypesafeRouteConfigs("/typesafe") {
		if r.Path == "/typesafe/v1/systemone" {
			rc := r
			route = &rc
		}
	}
	if route == nil {
		t.Fatal("systemone route not registered")
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	native := `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.25,"rationale":"extra"}},"usage":{"input_tokens":3,"output_tokens":0,"billed":true}}`
	resp := &schemas.BifrostDecisionResponse{
		Model:          "jev-1.13.0",
		Answers:        []schemas.DecisionAnswer{{Type: schemas.DecisionTypePredicate, Name: schemas.Ptr("q"), Probability: schemas.Ptr(0.25)}},
		NativeResponse: json.RawMessage(native),
	}
	resp.ExtraFields.Provider = schemas.Typesafe
	out, err := route.DecisionResponseConverter(ctx, resp)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got, ok := out.(json.RawMessage); !ok || string(got) != native {
		t.Errorf("native success body not relayed verbatim: %#v", out)
	}

	resp.ExtraFields.Provider = schemas.OpenAI // emulated elsewhere: rebuilt shape
	out, err = route.DecisionResponseConverter(ctx, resp)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, ok := out.(json.RawMessage); ok {
		t.Error("non-typesafe responses must be rebuilt, not relayed")
	}

	// A custom provider backed by Typesafe reports its own name; the base
	// provider type on the context is what marks the body as native.
	customCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	customCtx.SetValue(schemas.BifrostContextKeyBaseProviderType, schemas.Typesafe)
	resp.ExtraFields.Provider = "my-typesafe"
	out, err = route.DecisionResponseConverter(customCtx, resp)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got, ok := out.(json.RawMessage); !ok || string(got) != native {
		t.Errorf("custom typesafe provider's native body not relayed verbatim: %#v", out)
	}

	nativeErr := `{"detail":[{"type":"missing","loc":["body","state"],"msg":"Field required"}]}`
	bifrostErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "state: Field required"}}
	bifrostErr.ExtraFields.NativeErrorResponse = json.RawMessage(nativeErr)
	if got, ok := route.ErrorConverter(ctx, bifrostErr).(json.RawMessage); !ok || string(got) != nativeErr {
		t.Errorf("native error body not relayed verbatim: %#v", route.ErrorConverter(ctx, bifrostErr))
	}
	local := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "state is required"}}
	if _, ok := route.ErrorConverter(ctx, local).(json.RawMessage); ok {
		t.Error("errors Bifrost raised itself must use the rebuilt envelope")
	}
}

// systemOneMultipart builds a multipart systemone body from parts in order: each
// part is a name, an optional filename (a file part), an optional content type,
// and its data.
func systemOneMultipart(t *testing.T, parts ...[4]string) (*fasthttp.RequestCtx, *typesafe.TypesafeDecisionRequest) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, p := range parts {
		header := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name=%q`, p[0])
		if p[1] != "" {
			disposition += fmt.Sprintf(`; filename=%q`, p[1])
		}
		header.Set("Content-Disposition", disposition)
		if p[2] != "" {
			header.Set("Content-Type", p[2])
		}
		w, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		_, _ = w.Write([]byte(p[3]))
	}
	_ = writer.Close()
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetContentType(writer.FormDataContentType())
	ctx.Request.SetBody(buf.Bytes())
	return ctx, &typesafe.TypesafeDecisionRequest{}
}

// TestTypesafeSystemOneMultipartParser pins the systemone route's multipart body: the
// JSON is read from the "request" part, a plain field or a file part, in any position,
// and each file part's bytes become the asset that names it, keeping its name.
func TestTypesafeSystemOneMultipartParser(t *testing.T) {
	request := `{"model":"jev-latest","state":{"door":"r-door","note":"r-note"},"assets":{"r-door":{"type":"image","part":"door"},"r-note":{"format":"png","data_base64":"Tk9URQ=="}},"questions":{"q":{"type":"noul","instructions":"Is someone at the door?"}}}`
	for name, parts := range map[string][][4]string{
		"request field first":   {{"request", "", "application/json", request}, {"door", "door.jpg", "image/jpeg", "JPEG"}},
		"request as a file":     {{"request", "request.json", "application/json", request}, {"door", "door.jpg", "image/jpeg", "JPEG"}},
		"request not first":     {{"door", "door.jpg", "image/jpeg", "JPEG"}, {"request", "", "application/json", request}},
		"part without its type": {{"request", "", "", request}, {"door", "door.jpg", "", "JPEG"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, native := systemOneMultipart(t, parts...)
			if err := parseTypesafeSystemOneRequest(ctx, native); err != nil {
				t.Fatalf("parse: %v", err)
			}
			shared, err := native.ToBifrostDecisionRequest(nil)
			if err != nil {
				t.Fatalf("to shared: %v", err)
			}
			door := shared.Input.Messages[0].Content.Parts[1]
			if *door.ID != "r-door" || door.Name == nil || *door.Name != "door" || string(door.Data) != "JPEG" {
				t.Errorf("door part = %+v", door)
			}
			if note := shared.Input.Messages[0].Content.Parts[2]; note.Name != nil || string(note.Data) != "NOTE" {
				t.Errorf("base64 asset in a multipart request changed: %+v", note)
			}
		})
	}
}

// TestTypesafeSystemOneMultipartParserRejections pins the 400s for a multipart body
// the request cannot be built from; Typesafe rejects each of them too.
func TestTypesafeSystemOneMultipartParserRejections(t *testing.T) {
	request := func(assets string) string {
		return `{"model":"jev-latest","state":"r-a","assets":` + assets + `,"questions":{"q":{"type":"noul","instructions":"x"}}}`
	}
	one := request(`{"r-a":{"part":"a"}}`)
	for name, parts := range map[string][][4]string{
		"no request part":      {{"a", "a.png", "image/png", "PNG"}},
		"duplicate part":       {{"request", "", "", one}, {"a", "a.png", "image/png", "PNG"}, {"a", "b.png", "image/png", "PNG"}},
		"missing part":         {{"request", "", "", request(`{"r-a":{"part":"gone"}}`)}},
		"unused part":          {{"request", "", "", one}, {"a", "a.png", "image/png", "PNG"}, {"b", "b.png", "image/png", "PNG"}},
		"stray form field":     {{"request", "", "", one}, {"a", "a.png", "image/png", "PNG"}, {"note", "", "", "hello"}},
		"two assets, one part": {{"request", "", "", request(`{"r-a":{"part":"a"},"r-b":{"part":"a"}}`)}, {"a", "a.png", "image/png", "PNG"}},
		"part named request":   {{"request", "", "", request(`{"r-a":{"part":"request"}}`)}},
		"part without a name":  {{"request", "", "", one}, {"a", "a.png", "image/png", "PNG"}, {"", "b.png", "image/png", "PNG"}},
	} {
		ctx, native := systemOneMultipart(t, parts...)
		if err := parseTypesafeSystemOneRequest(ctx, native); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestTypesafeSystemOneParserJSON pins that a JSON body still decodes as before.
func TestTypesafeSystemOneParserJSON(t *testing.T) {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetContentType("application/json")
	ctx.Request.SetBody([]byte(`{"model":"jev-latest","state":"text","questions":{"q":{"type":"noul","instructions":"x"}}}`))
	native := &typesafe.TypesafeDecisionRequest{}
	if err := parseTypesafeSystemOneRequest(ctx, native); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if native.State != "text" || native.Model != "jev-latest" || len(native.Questions) != 1 {
		t.Errorf("decoded = %+v", native)
	}
}
