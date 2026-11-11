package bifrost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// Threshold-crossing tests for the large-payload eligibility rule (TXE-7082), through
// Bifrost's dispatch and the real providers against a local fixture. The large-payload hook
// and the response threshold are set on the context the way a transport hook sets them.

// largePayloadUpstream records what a provider sent to its vendor.
type largePayloadUpstream struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func (u *largePayloadUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func (u *largePayloadUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return ""
	}
	return u.bodies[len(u.bodies)-1]
}

// largePayloadClient starts one fixture per provider, each answering every request with
// that provider's response, and a Bifrost client configured to reach them.
func largePayloadClient(t *testing.T, responses map[schemas.ModelProvider]string) (*Bifrost, map[schemas.ModelProvider]*largePayloadUpstream) {
	t.Helper()
	account := NewMockAccount()
	upstreams := map[schemas.ModelProvider]*largePayloadUpstream{}
	for provider, response := range responses {
		upstream := &largePayloadUpstream{}
		upstreams[provider] = upstream
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			upstream.mu.Lock()
			upstream.bodies = append(upstream.bodies, string(body))
			upstream.paths = append(upstream.paths, r.URL.Path)
			upstream.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		}))
		t.Cleanup(server.Close)
		account.AddProviderWithBaseURL(provider, 1, 4, server.URL)
		account.configs[provider].NetworkConfig.AllowPrivateNetwork = true
		account.configs[provider].NetworkConfig.MaxRetries = 0
		account.keys[provider][0].Models = schemas.WhiteList{"*"}
	}
	client, err := Init(context.Background(), schemas.BifrostConfig{Account: account, Logger: NewNoOpLogger()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(client.Shutdown)
	return client, upstreams
}

// stageLargeRequest puts a request body on the context as the large-payload hook does: the
// body is a stream, nothing of it is parsed, and only routing metadata is known.
func stageLargeRequest(ctx *schemas.BifrostContext, body, model string) {
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadMode, true)
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadReader, io.Reader(strings.NewReader(body)))
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadContentLength, len(body))
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadContentType, "application/json")
	ctx.SetValue(schemas.BifrostContextKeyLargePayloadMetadata, &schemas.LargePayloadMetadata{Model: model})
}

func routeCtx(wire string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if wire != "" {
		ctx.SetValue(schemas.BifrostContextKeyIntegrationType, wire)
	}
	return ctx
}

// cohereEmbedResponse is Cohere's /v2/embed answer: the tokens Cohere bills are under
// meta.billed_units, not in a usage object, and floats pads the body past a threshold.
func cohereEmbedResponse(floats int) string {
	values := make([]string, floats)
	for i := range values {
		values[i] = "0.125"
	}
	return `{"id":"emb_1","embeddings":{"float":[[` + strings.Join(values, ",") + `]]},"texts":["hello"],"meta":{"api_version":{"version":"2"},"billed_units":{"input_tokens":7}}}`
}

const (
	openAIChatResponse      = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	anthropicMessageReponse = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`
)

// A converted response is the same below and above the response threshold: the caller gets
// unified usage from Cohere's billed units, and the vendor gets the mapped native field. On
// the code before TXE-7082 the response above the threshold was Cohere's own body streamed
// raw, with no usage on the response.
func TestLargeResponseAboveThresholdIsStillConverted(t *testing.T) {
	text := "hello"
	request := func() *schemas.BifrostEmbeddingRequest {
		return &schemas.BifrostEmbeddingRequest{
			Provider: schemas.Cohere,
			Model:    "embed-v4.0",
			Params:   &schemas.EmbeddingParameters{TaskType: schemas.Ptr("search_query")},
			Input:    []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}}}},
		}
	}
	type observed struct {
		promptTokens int
		dimensions   int
		inputType    string
	}
	observe := func(t *testing.T, threshold int64) observed {
		t.Helper()
		client, upstreams := largePayloadClient(t, map[schemas.ModelProvider]string{schemas.Cohere: cohereEmbedResponse(4000)})
		ctx := routeCtx("")
		if threshold > 0 {
			ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, threshold)
		}
		response, bifrostErr := client.EmbeddingRequest(ctx, request())
		if bifrostErr != nil {
			t.Fatalf("embedding failed: %s", bifrostErr.Error.Message)
		}
		if ctx.Value(schemas.BifrostContextKeyLargeResponseMode) != nil {
			t.Error("the response was handed to the transport as a raw stream")
		}
		var wire struct {
			InputType string `json:"input_type"`
		}
		if err := json.Unmarshal([]byte(upstreams[schemas.Cohere].lastBody()), &wire); err != nil {
			t.Fatalf("upstream body is not JSON: %v", err)
		}
		got := observed{inputType: wire.InputType}
		if response.Usage != nil {
			got.promptTokens = response.Usage.PromptTokens
		}
		if len(response.Data) == 1 {
			got.dimensions = len(response.Data[0].Embedding.EmbeddingArray)
		}
		return got
	}

	below := observe(t, 0)
	above := observe(t, 1024)
	want := observed{promptTokens: 7, dimensions: 4000, inputType: "search_query"}
	if below != want {
		t.Errorf("below the threshold: got %+v, want %+v", below, want)
	}
	if above != want {
		t.Errorf("above the threshold: got %+v, want %+v (the same as below)", above, want)
	}
}

// A staged large request reaches the vendor unparsed only where the caller's wire is the
// vendor's. Everywhere else it is refused with 413 before anything is written upstream. On
// the code before TXE-7082 the converting rows were streamed too: Mistral received the
// OpenAI body as written, and Cohere a unified embeddings body with no input_type.
func TestStagedLargeRequestIsForwardedOnlyWhereNothingIsConverted(t *testing.T) {
	const (
		embeddings = "embeddings"
		chat       = "chat"
		responses  = "responses"
		speech     = "speech"
	)
	for _, tt := range []struct {
		name      string
		wire      string
		provider  schemas.ModelProvider
		model     string
		operation string
		guardrail bool
		wantRaw   bool
	}{
		{"cohere embeddings on the neutral route", "", schemas.Cohere, "embed-v4.0", embeddings, false, false},
		{"cohere embeddings on the OpenAI route", "openai", schemas.Cohere, "embed-v4.0", embeddings, false, false},
		{"cohere embeddings on the Cohere route", "cohere", schemas.Cohere, "embed-v4.0", embeddings, false, true},
		{"mistral chat on the OpenAI route", "openai", schemas.Mistral, "mistral-large-latest", chat, false, false},
		{"elevenlabs speech on the OpenAI route", "openai", schemas.Elevenlabs, "eleven_v3", speech, false, false},
		{"openai chat on the OpenAI route", "openai", schemas.OpenAI, "gpt-5", chat, false, true},
		{"openai chat on the neutral route", "", schemas.OpenAI, "gpt-5", chat, false, false},
		{"anthropic messages on the Anthropic route", "anthropic", schemas.Anthropic, "claude-sonnet-4-5", responses, false, true},
		{"anthropic messages with a guardrail that rewrites the body", "anthropic", schemas.Anthropic, "claude-sonnet-4-5", responses, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, upstreams := largePayloadClient(t, map[schemas.ModelProvider]string{
				schemas.Cohere:     cohereEmbedResponse(2),
				schemas.Mistral:    openAIChatResponse,
				schemas.OpenAI:     openAIChatResponse,
				schemas.Elevenlabs: `{}`,
				schemas.Anthropic:  anthropicMessageReponse,
			})
			callerModel := string(tt.provider) + "/" + tt.model
			callerBody := fmt.Sprintf(`{"model":%q,"caller_field":"kept as written","payload":%q}`, callerModel, strings.Repeat("x", 4096))
			ctx := routeCtx(tt.wire)
			stageLargeRequest(ctx, callerBody, callerModel)
			if tt.guardrail {
				schemas.RequireBodyConversion(ctx, "the redaction guardrail rewrites message text")
			}

			var bifrostErr *schemas.BifrostError
			switch tt.operation {
			case embeddings:
				_, bifrostErr = client.EmbeddingRequest(ctx, &schemas.BifrostEmbeddingRequest{Provider: tt.provider, Model: tt.model})
			case chat:
				_, bifrostErr = client.ChatCompletionRequest(ctx, &schemas.BifrostChatRequest{Provider: tt.provider, Model: tt.model})
			case responses:
				_, bifrostErr = client.ResponsesRequest(ctx, &schemas.BifrostResponsesRequest{Provider: tt.provider, Model: tt.model})
			case speech:
				_, bifrostErr = client.SpeechRequest(ctx, &schemas.BifrostSpeechRequest{Provider: tt.provider, Model: tt.model})
			}

			upstream := upstreams[tt.provider]
			if !tt.wantRaw {
				if bifrostErr == nil {
					t.Fatalf("the request was served; the vendor received %q", upstream.lastBody())
				}
				if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusRequestEntityTooLarge {
					t.Errorf("status = %v, want 413: %s", bifrostErr.StatusCode, bifrostErr.Error.Message)
				}
				if bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
					t.Errorf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, schemas.ErrorTypeCallerInvalidRequest)
				}
				if tt.guardrail && !strings.Contains(bifrostErr.Error.Message, "the redaction guardrail rewrites message text") {
					t.Errorf("the refusal does not give the plugin's reason: %s", bifrostErr.Error.Message)
				}
				if upstream.calls() != 0 {
					t.Errorf("the vendor was called %d times with %q; the refusal must come before any upstream write", upstream.calls(), upstream.lastBody())
				}
				return
			}
			if bifrostErr != nil {
				t.Fatalf("a native request was refused: %s", bifrostErr.Error.Message)
			}
			if upstream.calls() != 1 {
				t.Fatalf("upstream calls = %d, want 1", upstream.calls())
			}
			// The vendor gets the caller's bytes; only the provider prefix leaves the model.
			if want := strings.Replace(callerBody, callerModel, tt.model, 1); upstream.lastBody() != want {
				t.Errorf("upstream body differs from the caller's:\n got: %.200s\nwant: %.200s", upstream.lastBody(), want)
			}
		})
	}
}

// Eligibility is decided per attempt. A staged request refused for a converting primary has
// not been read, so a fallback whose vendor takes the caller's wire still receives it whole.
func TestStagedLargeRequestFallsBackToAProviderThatTakesItUnchanged(t *testing.T) {
	client, upstreams := largePayloadClient(t, map[schemas.ModelProvider]string{
		schemas.Mistral: openAIChatResponse,
		schemas.OpenAI:  openAIChatResponse,
	})
	callerBody := `{"model":"mistral/mistral-large-latest","messages":[{"role":"user","content":"hello"}]}`
	ctx := routeCtx("openai")
	stageLargeRequest(ctx, callerBody, "mistral/mistral-large-latest")

	_, bifrostErr := client.ChatCompletionRequest(ctx, &schemas.BifrostChatRequest{
		Provider:  schemas.Mistral,
		Model:     "mistral-large-latest",
		Fallbacks: []schemas.Fallback{{Provider: schemas.OpenAI, Model: "gpt-5"}},
	})
	if bifrostErr != nil {
		t.Fatalf("the fallback was not served: %s", bifrostErr.Error.Message)
	}
	if calls := upstreams[schemas.Mistral].calls(); calls != 0 {
		t.Errorf("mistral was called %d times with an unconverted body", calls)
	}
	if calls := upstreams[schemas.OpenAI].calls(); calls != 1 {
		t.Fatalf("openai calls = %d, want 1", calls)
	}
	if got := upstreams[schemas.OpenAI].lastBody(); !strings.Contains(got, `"messages":[{"role":"user","content":"hello"}]`) {
		t.Errorf("openai did not receive the caller's body: %s", got)
	}
}

// With neither large-payload mode on, which is every request of a gateway with no hook
// installed, the per-attempt decision does nothing and leaves nothing on the context.
func TestDecideLargePayloadForAttemptIsInertWithoutAMode(t *testing.T) {
	ctx := routeCtx("openai")
	if refusal := decideLargePayloadForAttempt(ctx, schemas.Mistral, schemas.Mistral, schemas.ChatCompletionRequest, "mistral-large-latest"); refusal != nil {
		t.Fatalf("refused a request with no large-payload mode on: %s", refusal.Error.Message)
	}
	if ctx.Value(schemas.BifrostContextKeyLargePayloadEligibility) != nil {
		t.Error("a decision was stamped with no large-payload mode on")
	}
}

func BenchmarkDecideLargePayloadForAttempt(b *testing.B) {
	b.Run("no large-payload mode", func(b *testing.B) {
		ctx := routeCtx("openai")
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = decideLargePayloadForAttempt(ctx, schemas.OpenAI, schemas.OpenAI, schemas.ChatCompletionRequest, "gpt-5")
		}
	})
	b.Run("response threshold set", func(b *testing.B) {
		ctx := routeCtx("openai")
		ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, int64(10<<20))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = decideLargePayloadForAttempt(ctx, schemas.OpenAI, schemas.OpenAI, schemas.ChatCompletionRequest, "gpt-5")
		}
	})
}
