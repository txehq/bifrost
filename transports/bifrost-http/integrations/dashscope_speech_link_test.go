package integrations

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// TXE-6433: a speech answer that is a link to the audio (DashScope Qwen3-TTS) reaches a caller
// on a route whose response converter writes only audio bytes. GenAI's generateContent would
// encode the empty Audio as inline data and answer 200 with no audio; the router answers the
// link as JSON instead, with the vendor's url, expires_at and usage, as /v1/audio/speech does.
func TestGenAISpeechRouteAnswersDashScopeAudioLink(t *testing.T) {
	const audioLink = "https://dashscope-result-sgp.oss-ap-southeast-1.aliyuncs.com/1d/a.wav?Expires=1766113409&Signature=x"
	var sent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"5c63c65c","output":{"finish_reason":"stop","audio":{"data":"","url":"` + audioLink +
			`","id":"audio_5c63c65c","expires_at":1766113409}},"usage":{"input_tokens":0,"output_tokens":0,"characters":5}}`))
	}))
	defer upstream.Close()

	account := &nativeMessagesTestAccount{
		configs: map[schemas.ModelProvider]*schemas.ProviderConfig{
			schemas.DashScope: {NetworkConfig: schemas.NetworkConfig{BaseURL: upstream.URL, AllowPrivateNetwork: true}},
		},
		keys: map[schemas.ModelProvider][]schemas.Key{
			schemas.DashScope: {{ID: "dashscope-key", Value: schemas.SecretVar{Val: "dashscope-secret"}, Models: schemas.WhiteList{"*"}, Weight: 1}},
		},
	}
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: account, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("init Bifrost: %v", err)
	}
	defer client.Shutdown()

	route := findGenAIRouteForTest(t, CreateGenAIRouteConfigs("/genai"), "/genai/v1beta/models/{model:*}", "POST")
	handler := NewGenericRouter(client, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger()).createHandler(route)
	httpCtx := &fasthttp.RequestCtx{}
	httpCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	httpCtx.Request.SetRequestURI("/genai/v1beta/models/dashscope/qwen3-tts-flash:generateContent")
	httpCtx.SetUserValue("model", "dashscope/qwen3-tts-flash:generateContent")
	httpCtx.Request.Header.SetContentType("application/json")
	httpCtx.Request.SetBody([]byte(`{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],` +
		`"generationConfig":{"responseModalities":["AUDIO"],"speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"Cherry"}}}}}`))
	handler(httpCtx)

	if httpCtx.Response.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("status = %d, body = %s", httpCtx.Response.StatusCode(), httpCtx.Response.Body())
	}
	if !strings.Contains(sent, `"voice":"Cherry"`) || !strings.Contains(sent, `"text":"Hello"`) {
		t.Fatalf("vendor body = %s, want the text and voice", sent)
	}
	var body struct {
		URL       string         `json:"url"`
		ExpiresAt int64          `json:"expires_at"`
		Usage     map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(httpCtx.Response.Body(), &body); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, httpCtx.Response.Body())
	}
	if body.URL != audioLink || body.ExpiresAt != 1766113409 {
		t.Fatalf("url, expires_at = %q, %d; want the vendor's link and expiry: %s", body.URL, body.ExpiresAt, httpCtx.Response.Body())
	}
	if body.Usage["billed_characters"] != float64(5) {
		t.Fatalf("usage = %v, want billed_characters 5", body.Usage)
	}
	if got := string(httpCtx.Response.Header.Peek("x-bifrost-usage")); !strings.Contains(got, `"billed_characters":5`) {
		t.Fatalf("x-bifrost-usage = %q, want billed_characters 5", got)
	}
}
