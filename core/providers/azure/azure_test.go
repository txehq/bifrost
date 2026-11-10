package azure_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/internal/llmtests"

	"github.com/capsohq/bifrost/core/providers/azure"
	"github.com/capsohq/bifrost/core/providers/typesafe"
	"github.com/capsohq/bifrost/core/schemas"
)

func TestAzure(t *testing.T) {
	t.Parallel()

	if strings.TrimSpace(os.Getenv("AZURE_API_KEY")) == "" {
		t.Skip("Skipping Azure tests because AZURE_API_KEY is not set")
	}
	if strings.TrimSpace(os.Getenv("AZURE_ENDPOINT")) == "" {
		t.Skip("Skipping Azure tests because AZURE_ENDPOINT is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:           schemas.Azure,
		ChatModel:          "gpt-4o",
		PromptCachingModel: "gpt-4o",
		VisionModel:        "gpt-4o",
		ChatAudioModel:     "gpt-4o-mini-audio-preview",
		Fallbacks: []schemas.Fallback{
			{Provider: schemas.Azure, Model: "gpt-4o"},
		},
		TextModel:               "", // Azure doesn't support text completion in newer models
		EmbeddingModel:          "text-embedding-ada-002",
		ReasoningModel:          "claude-opus-4-5",
		WebSearchChatModel:      "claude-opus-4-5",
		SpeechSynthesisModel:    "gpt-4o-mini-tts",
		TranscriptionModel:      "gpt-4o-transcribe",
		ImageGenerationModel:    "gpt-image-2",
		ImageEditModel:          "gpt-image-2",
		VideoGenerationModel:    "sora-2",
		PassthroughModel:        "gpt-4o",
		ExternalCompactionModel: "gpt-4o",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:               false, // Not supported
			SimpleChat:                   true,
			CompletionStream:             true,
			MultiTurnConversation:        true,
			ToolCalls:                    true,
			ToolCallsStreaming:           true,
			MultipleToolCalls:            true,
			MultipleToolCallsStreaming:   true,
			End2EndToolCalling:           true,
			AutomaticFunctionCall:        true,
			ImageURL:                     true,
			ImageBase64:                  true,
			MultipleImages:               true,
			CompleteEnd2End:              true,
			Embedding:                    true,
			ListModels:                   true,
			Reasoning:                    true,
			ChatAudio:                    false,
			Transcription:                false, // Disabled for azure because of 3 calls/minute quota
			TranscriptionStream:          false, // Not properly supported yet by Azure
			SpeechSynthesis:              false, // Disabled for azure because of 3 calls/minute quota
			SpeechSynthesisStream:        false, // Disabled for azure because of 3 calls/minute quota
			StructuredOutputs:            true,  // Structured outputs with nullable enum support
			PromptCaching:                true,
			ImageGeneration:              false, // Skipped for Azure
			ImageGenerationStream:        false, // Skipped for Azure
			ImageEdit:                    false, // Model not deployed on Azure endpoint
			ImageEditStream:              false, // Model not deployed on Azure endpoint
			ImageVariation:               false, // Not supported by Azure
			VideoGeneration:              false, // disabled for now because of long running operations
			VideoDownload:                false,
			VideoRetrieve:                false,
			VideoRemix:                   false,
			VideoList:                    false,
			VideoDelete:                  false,
			InterleavedThinking:          true,
			PassthroughAPI:               true,
			EagerInputStreaming:          true, // fine-grained-tool-streaming-2025-05-14 (Beta on Azure Foundry)
			ServerToolsViaOpenAIEndpoint: true, // web_search / web_fetch / code_execution on Azure per Table 20
			ContainerCreate:              true,
			ContainerList:                false, // not supported (hangs with 0 bytes)
			ContainerRetrieve:            true,
			ContainerDelete:              true,
			ContainerFileCreate:          true,
			ContainerFileList:            true,
			ContainerFileRetrieve:        true,
			ContainerFileContent:         true,
			ContainerFileDelete:          true,
			ExternalCompaction:           true,
		},
		DisableParallelFor: []string{"Transcription"}, // Azure Whisper has 3 calls/minute quota
	}

	t.Run("AzureTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

// routingTestLogger is a no-op logger for provider construction in routing tests.
type routingTestLogger struct{}

func (routingTestLogger) Debug(msg string, args ...any)                     {}
func (routingTestLogger) Info(msg string, args ...any)                      {}
func (routingTestLogger) Warn(msg string, args ...any)                      {}
func (routingTestLogger) Error(msg string, args ...any)                     {}
func (routingTestLogger) Fatal(msg string, args ...any)                     {}
func (routingTestLogger) SetLevel(level schemas.LogLevel)                   {}
func (routingTestLogger) SetOutputType(outputType schemas.LoggerOutputType) {}
func (routingTestLogger) LogHTTPRequest(level schemas.LogLevel, msg string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

// azureRoutingUpstream is a fake Azure resource that records which OpenAI-compatible
// path each request hit and answers both shapes: a chat completion truncated by
// max_tokens (finish_reason "length"), and a completed Responses object.
type azureRoutingUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newAzureRoutingUpstream(t *testing.T) *azureRoutingUpstream {
	t.Helper()
	u := &azureRoutingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		var body strings.Builder
		if r.Body != nil {
			buf := make([]byte, 1<<16)
			n, _ := r.Body.Read(buf)
			body.Write(buf[:n])
		}
		switch r.URL.Path {
		case "/openai/v1/chat/completions":
			if strings.Contains(body.String(), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"FW-GLM-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"1\n2\n3"},"finish_reason":null}]}` + "\n\n" +
					`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"FW-GLM-5.2","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":16,"total_tokens":26}}` + "\n\n" +
					"data: [DONE]\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"FW-GLM-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"1\n2\n3"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":16,"total_tokens":26}}`))
		case "/openai/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4o","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(u.Server.Close)
	return u
}

func (u *azureRoutingUpstream) hitPaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

// TestAzureResponsesHonoursDatasheetSupportedEndpoints pins #6782: Azure Foundry
// deployments of Fireworks models (FW-GLM-5.2 and friends) hard-cap output at 4096
// tokens on /openai/v1/responses while /openai/v1/chat/completions honours
// max_tokens. Microsoft lists every Fireworks catalog model as a "Chat completions"
// model with Responses only via the Foundry Projects API. When the model's datasheet
// row lists supported_endpoints without /v1/responses, the azure provider must serve
// Responses (and ResponsesStream) through chat completions, mirroring the Bedrock
// Mantle gate. A row that lists /v1/responses, or no row at all, keeps today's route.
func TestAzureResponsesHonoursDatasheetSupportedEndpoints(t *testing.T) {
	// The capability resolver is process-global, so this test must not run in
	// parallel with anything that also installs one.
	schemas.SetCapabilityResolver(func(provider schemas.ModelProvider, model string) *schemas.ModelCapabilities {
		if provider != schemas.Azure {
			return nil
		}
		switch model {
		case "FW-GLM-5.2":
			return &schemas.ModelCapabilities{SupportedEndpoints: []string{"/v1/chat/completions"}}
		case "gpt-4o":
			return &schemas.ModelCapabilities{SupportedEndpoints: []string{"/v1/chat/completions", "/v1/batch", "/v1/responses"}}
		}
		return nil
	})
	t.Cleanup(func() { schemas.SetCapabilityResolver(nil) })

	newProvider := func(t *testing.T) *azure.AzureProvider {
		t.Helper()
		provider, err := azure.NewAzureProvider(&schemas.ProviderConfig{
			NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 10},
		}, routingTestLogger{})
		if err != nil {
			t.Fatalf("NewAzureProvider: %v", err)
		}
		return provider
	}
	newKey := func(endpoint string) schemas.Key {
		return schemas.Key{
			Value:          *schemas.NewSecretVar("test-key"),
			Models:         []string{"*"},
			AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar(endpoint)},
		}
	}
	newRequest := func(model string) *schemas.BifrostResponsesRequest {
		return &schemas.BifrostResponsesRequest{
			Provider: schemas.Azure,
			Model:    model,
			Input: []schemas.ResponsesMessage{{
				Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
				Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("Count from 1 to 3000, one number per line.")},
			}},
			Params: &schemas.ResponsesParameters{MaxOutputTokens: schemas.Ptr(8000)},
		}
	}
	passthroughPostHook := func(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return resp, err
	}
	assertOnlyPath := func(t *testing.T, upstream *azureRoutingUpstream, want string) {
		t.Helper()
		paths := upstream.hitPaths()
		if len(paths) != 1 || paths[0] != want {
			t.Fatalf("upstream paths = %v, want exactly [%s]", paths, want)
		}
	}

	t.Run("ChatOnlyRowRoutesResponsesToChatCompletions", func(t *testing.T) {
		upstream := newAzureRoutingUpstream(t)
		ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		resp, bifrostErr := newProvider(t).Responses(ctx, newKey(upstream.URL), newRequest("FW-GLM-5.2"))
		if bifrostErr != nil {
			t.Fatalf("Responses: %+v", bifrostErr)
		}
		assertOnlyPath(t, upstream, "/openai/v1/chat/completions")
		if resp.Status == nil || *resp.Status != schemas.ResponsesResponseStatusIncomplete {
			t.Errorf("status = %v, want incomplete (finish_reason length must survive the conversion)", resp.Status)
		}
		if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != schemas.ResponsesResponseIncompleteReasonMaxOutputTokens {
			t.Errorf("incomplete_details = %+v, want reason max_output_tokens", resp.IncompleteDetails)
		}
	})

	t.Run("ChatOnlyRowRoutesResponsesStreamToChatCompletions", func(t *testing.T) {
		upstream := newAzureRoutingUpstream(t)
		ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		stream, bifrostErr := newProvider(t).ResponsesStream(ctx, passthroughPostHook, nil, newKey(upstream.URL), newRequest("FW-GLM-5.2"))
		if bifrostErr != nil {
			t.Fatalf("ResponsesStream: %+v", bifrostErr)
		}
		var last *schemas.BifrostStreamChunk
		var streamErr *schemas.BifrostError
		timeout := time.NewTimer(20 * time.Second)
		defer timeout.Stop()
	drain:
		for {
			select {
			case chunk, ok := <-stream:
				if !ok {
					break drain
				}
				if chunk != nil && chunk.BifrostError != nil && streamErr == nil {
					streamErr = chunk.BifrostError
				}
				if chunk != nil && chunk.BifrostResponsesStreamResponse != nil {
					last = chunk
				}
			case <-timeout.C:
				t.Fatal("timed out waiting for the provider stream to close")
			}
		}
		// Route first: a wrong path is the defect; whatever the fake answered there is noise.
		assertOnlyPath(t, upstream, "/openai/v1/chat/completions")
		if streamErr != nil {
			t.Fatalf("stream error: %+v", streamErr)
		}
		if last == nil {
			t.Fatal("no Responses stream events were re-assembled from the chat chunks")
		}
		if got := last.BifrostResponsesStreamResponse.Type; got != schemas.ResponsesStreamResponseTypeIncomplete {
			t.Errorf("terminal event = %q, want %q", got, schemas.ResponsesStreamResponseTypeIncomplete)
		}
	})

	t.Run("RowWithResponsesEndpointKeepsResponsesRoute", func(t *testing.T) {
		upstream := newAzureRoutingUpstream(t)
		ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if _, bifrostErr := newProvider(t).Responses(ctx, newKey(upstream.URL), newRequest("gpt-4o")); bifrostErr != nil {
			t.Fatalf("Responses: %+v", bifrostErr)
		}
		assertOnlyPath(t, upstream, "/openai/v1/responses")
	})

	t.Run("UnknownModelKeepsResponsesRoute", func(t *testing.T) {
		upstream := newAzureRoutingUpstream(t)
		ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if _, bifrostErr := newProvider(t).Responses(ctx, newKey(upstream.URL), newRequest("my-custom-deployment")); bifrostErr != nil {
			t.Fatalf("Responses: %+v", bifrostErr)
		}
		assertOnlyPath(t, upstream, "/openai/v1/responses")
	})

	t.Run("AliasModelNameResolvesTheDatasheetRow", func(t *testing.T) {
		// The reporter's deployment is named azure-glm-5.2; the key alias maps it to
		// the catalog ID, which is also how pricing finds the row.
		upstream := newAzureRoutingUpstream(t)
		ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx.SetValue(schemas.BifrostContextKeyResolvedAlias, &schemas.ResolvedAlias{
			Config: &schemas.AliasConfig{ModelID: "azure-glm-5.2", ModelName: schemas.Ptr("FW-GLM-5.2")},
		})

		if _, bifrostErr := newProvider(t).Responses(ctx, newKey(upstream.URL), newRequest("azure-glm-5.2")); bifrostErr != nil {
			t.Fatalf("Responses: %+v", bifrostErr)
		}
		assertOnlyPath(t, upstream, "/openai/v1/chat/completions")
	})
}

// Microsoft-Decision-1 bodies and errors below are as captured from a live Foundry
// deployment named "Decision-1".
const azureDecisionResponseBody = `{"model":"microsoft-decision-1","answers":{"team":{"choice":"engineering","confidence":0.99,"probabilities":{"billing":0.001,"engineering":0.998,"support":0.001},"type":"choice"}},"usage":{"input_tokens":64,"output_tokens":1}}`

func newAzureDecisionRequest(input schemas.DecisionInput) *schemas.BifrostDecisionRequest {
	return &schemas.BifrostDecisionRequest{
		Provider: schemas.Azure,
		Model:    "Decision-1",
		Input:    input,
		Questions: []schemas.DecisionQuestion{{
			Type:         schemas.DecisionTypeChoice,
			Name:         schemas.Ptr("team"),
			Instructions: schemas.NewDecisionText("Which team should handle this ticket?"),
			Choices: []schemas.DecisionChoice{
				{Value: schemas.DecisionScalar{Str: schemas.Ptr("billing")}, Description: schemas.NewDecisionText("Charges, invoices, and refunds")},
				{Value: schemas.DecisionScalar{Str: schemas.Ptr("engineering")}, Description: schemas.NewDecisionText("Bugs, errors, and outages")},
				{Value: schemas.DecisionScalar{Str: schemas.Ptr("support")}, Description: schemas.NewDecisionText("How-to and account questions")},
			},
		}},
	}
}

func newAzureDecisionProvider(t *testing.T) *azure.AzureProvider {
	t.Helper()
	provider, err := azure.NewAzureProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 10},
	}, routingTestLogger{})
	if err != nil {
		t.Fatalf("NewAzureProvider: %v", err)
	}
	return provider
}

func newAzureDecisionKey(endpoint string) schemas.Key {
	return schemas.Key{
		Value:          *schemas.NewSecretVar("test-key"),
		Models:         []string{"*"},
		AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar(endpoint)},
	}
}

// TestAzureDecisionSendsTypesafeBodyToSystemOne pins the native call: the body is exactly
// Typesafe's systemone request with the deployment name as the model, it goes to the
// resource's /providers/microsoft/v1/systemone with no api-version, and authenticates with
// the api-key header, or Bearer when a token is on the context.
func TestAzureDecisionSendsTypesafeBodyToSystemOne(t *testing.T) {
	request := newAzureDecisionRequest(schemas.DecisionInput{Text: schemas.Ptr("The API returns 500 on every call.")})
	native, err := typesafe.ToTypesafeDecisionRequest(request)
	if err != nil {
		t.Fatalf("ToTypesafeDecisionRequest: %v", err)
	}
	wantBody, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}

	for name, token := range map[string]string{"api-key": "", "bearer": "entra-token"} {
		t.Run(name, func(t *testing.T) {
			type capturedRequest struct {
				path, query, apiKey, auth string
				body                      []byte
			}
			// The handler runs on the server's goroutine, so the request is handed over on a channel.
			captured := make(chan capturedRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captured <- capturedRequest{path: r.URL.Path, query: r.URL.RawQuery, apiKey: r.Header.Get("api-key"), auth: r.Header.Get("Authorization"), body: body}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, azureDecisionResponseBody)
			}))
			defer server.Close()

			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if token != "" {
				ctx.SetValue(azure.AzureAuthorizationTokenKey, token)
			}
			resp, bifrostErr := newAzureDecisionProvider(t).Decision(ctx, newAzureDecisionKey(server.URL), request)
			if bifrostErr != nil {
				t.Fatalf("Decision: %+v", bifrostErr.Error)
			}
			sent := <-captured
			gotPath, gotQuery, gotAPIKey, gotAuth, gotBody := sent.path, sent.query, sent.apiKey, sent.auth, sent.body

			if gotPath != "/providers/microsoft/v1/systemone" || gotQuery != "" {
				t.Errorf("request = %s?%s, want /providers/microsoft/v1/systemone with no query", gotPath, gotQuery)
			}
			if token == "" && (gotAPIKey != "test-key" || gotAuth != "") {
				t.Errorf("auth = api-key %q, Authorization %q, want api-key test-key only", gotAPIKey, gotAuth)
			}
			if token != "" && (gotAuth != "Bearer "+token || gotAPIKey != "") {
				t.Errorf("auth = api-key %q, Authorization %q, want Bearer %s only", gotAPIKey, gotAuth, token)
			}
			var got, want any
			if err := json.Unmarshal(gotBody, &got); err != nil {
				t.Fatalf("body is not JSON: %s", gotBody)
			}
			_ = json.Unmarshal(wantBody, &want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("body =\n%s\nwant Typesafe's\n%s", gotBody, wantBody)
			}

			if len(resp.Answers) != 1 || resp.Answers[0].Choice == nil || resp.Answers[0].Choice.Str == nil || *resp.Answers[0].Choice.Str != "engineering" {
				t.Errorf("answers = %+v, want team=engineering", resp.Answers)
			}
			if resp.Usage == nil || resp.Usage.PromptTokens != 64 || resp.Usage.CompletionTokens != 1 {
				t.Errorf("usage = %+v, want 64 input and 1 output tokens", resp.Usage)
			}
		})
	}
}

// TestAzureDecisionRejectsImageInputLocally pins that an image never reaches
// Microsoft-Decision-1: the model is text-only and answers an image part read as JSON
// text with a 200, so the request is a local 400 instead.
func TestAzureDecisionRejectsImageInputLocally(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer server.Close()

	request := newAzureDecisionRequest(schemas.DecisionInput{Messages: []schemas.DecisionInputMessage{{
		Role: "user",
		Content: schemas.DecisionInputContent{Parts: []schemas.DecisionInputPart{
			{Type: schemas.DecisionInputPartTypeImage, ImageURL: schemas.Ptr("data:image/png;base64,AAAA")},
		}},
	}}})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := newAzureDecisionProvider(t).Decision(ctx, newAzureDecisionKey(server.URL), request)
	if bifrostErr == nil {
		t.Fatal("expected an error for an image input")
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %v, want 400", bifrostErr.StatusCode)
	}
	if bifrostErr.Error == nil || !strings.Contains(bifrostErr.Error.Message, "this provider cannot read") {
		t.Errorf("message = %+v, want the provider-neutral image rejection", bifrostErr.Error)
	}
	if hit {
		t.Error("the request reached Azure")
	}
}

// TestAzureDecisionForwardsAssets pins that images sent by reference reach Azure as
// Typesafe's assets rather than being refused locally, and that Azure's own verdict
// comes back: Microsoft-Decision-1 rejects the field today.
func TestAzureDecisionForwardsAssets(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"unrecognized_request_argument","message":"Unrecognized request argument supplied: assets"}}`)
	}))
	defer server.Close()

	request := newAzureDecisionRequest(schemas.DecisionInput{Messages: []schemas.DecisionInputMessage{{
		Role: "user",
		Content: schemas.DecisionInputContent{Parts: []schemas.DecisionInputPart{
			{Type: schemas.DecisionInputPartTypeText, Text: schemas.NewDecisionText("Is r-a a red square?")},
			{Type: schemas.DecisionInputPartTypeImage, ID: schemas.Ptr("r-a"), Data: []byte("\x89PNG\r\n\x1a\n")},
		}},
	}}})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := newAzureDecisionProvider(t).Decision(ctx, newAzureDecisionKey(server.URL), request)
	if body := <-bodies; !strings.Contains(string(body), `"assets":{"r-a":{"type":"image","format":"png"`) {
		t.Errorf("body sent to Azure = %s, want the asset", body)
	}
	if bifrostErr == nil || bifrostErr.Error == nil || bifrostErr.Error.Message != "Unrecognized request argument supplied: assets" {
		t.Errorf("error = %+v, want Azure's own message", bifrostErr)
	}
}

// TestAzureDecisionPassesAzureErrorsThrough pins that Azure's own errors reach the caller
// with their status and message, and that the reply's headers stay on the context.
func TestAzureDecisionPassesAzureErrorsThrough(t *testing.T) {
	cases := map[string]struct {
		status      int
		body        string
		wantMessage string
	}{
		"deployment not found": {
			status:      http.StatusNotFound,
			body:        `{"error":{"code":"DeploymentNotFound","message":"The API deployment no-such-deployment does not exist.","details":"The API deployment no-such-deployment does not exist."}}`,
			wantMessage: "The API deployment no-such-deployment does not exist.",
		},
		"unrecognized argument": {
			status:      http.StatusBadRequest,
			body:        `{"error":{"code":"unrecognized_request_argument","message":"Unrecognized request argument supplied: foo","details":"Unrecognized request argument supplied: foo"}}`,
			wantMessage: "Unrecognized request argument supplied: foo",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("apim-request-id", "req-"+name)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := newAzureDecisionProvider(t).Decision(ctx, newAzureDecisionKey(server.URL), newAzureDecisionRequest(schemas.DecisionInput{Text: schemas.Ptr("hi")}))
			if bifrostErr == nil {
				t.Fatal("expected an error")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Errorf("status = %v, want %d", bifrostErr.StatusCode, tc.status)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message != tc.wantMessage {
				t.Errorf("message = %+v, want %q", bifrostErr.Error, tc.wantMessage)
			}
			headers, _ := ctx.Value(schemas.BifrostContextKeyProviderResponseHeaders).(map[string]string)
			found := false
			for k, v := range headers {
				if strings.EqualFold(k, "apim-request-id") && v == "req-"+name {
					found = true
				}
			}
			if !found {
				t.Errorf("context headers = %v, want apim-request-id from the error reply", headers)
			}
		})
	}
}

// azureBatchVendor is a stand-in for the Azure OpenAI files and batches API.
type azureBatchVendor struct {
	*httptest.Server
	mu          sync.Mutex
	calls       []string
	uploads     []string
	uploadForms []map[string]string
	creates     []string
}

func newAzureBatchVendor(t *testing.T, batch string, files map[string]string) *azureBatchVendor {
	t.Helper()
	v := &azureBatchVendor{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.calls = append(v.calls, r.Method+" "+r.URL.Path)
		v.mu.Unlock()
		if r.Header.Get("api-key") != "test-key" {
			t.Errorf("%s %s: api-key = %q, want the key's", r.Method, r.URL.Path, r.Header.Get("api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/openai/v1/files":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("upload form: %v", err)
			}
			fields := map[string]string{}
			for name, values := range r.MultipartForm.Value {
				fields[name] = values[0]
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("upload file part: %v", err)
			}
			content, _ := io.ReadAll(file)
			v.mu.Lock()
			v.uploads = append(v.uploads, string(content))
			v.uploadForms = append(v.uploadForms, fields)
			v.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"file-in","object":"file","bytes":1,"created_at":1,"filename":"batch_requests.jsonl","purpose":"batch"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/openai/v1/batches":
			body, _ := io.ReadAll(r.Body)
			v.mu.Lock()
			v.creates = append(v.creates, string(body))
			v.mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"batch_az1","object":"batch","endpoint":"/chat/completions","input_file_id":"file-in","completion_window":"24h","status":"validating","created_at":1711471533}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/openai/v1/batches/"):
			_, _ = io.WriteString(w, batch)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/openai/v1/files/"), "/content")
			content, ok := files[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"file not found","code":"NotFound"}}`)
				return
			}
			_, _ = io.WriteString(w, content)
		default:
			t.Errorf("unexpected upstream call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

// Azure reads the deployment from each line's body.model. Inline lines carry the caller's
// model name, so each is rewritten to the deployment the key maps that model to, and the
// rest of the line reaches Azure unchanged; output_expires_after and metadata reach the
// batch create (contract C4, C8).
func TestAzureBatchCreateInlineRewritesModelsToDeployments(t *testing.T) {
	vendor := newAzureBatchVendor(t, "", nil)
	key := newAzureDecisionKey(vendor.URL)
	key.Aliases = schemas.KeyAliases{
		"gpt-4o-mini": {ModelID: "prod-gpt-4o-mini"},
		"gpt-4o":      {ModelID: "prod-gpt-4o"},
	}

	lines := []string{
		`{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"max_completion_tokens":64,"temperature":0.2,"response_format":{"type":"json_object"}}}`,
		`{"custom_id":"line-2","method":"POST","url":"/v1/chat/completions","body":{"model":"azure/gpt-4o","messages":[{"role":"user","content":"Bye"}],"seed":7}}`,
		`{"custom_id":"line-3","method":"POST","url":"/v1/chat/completions","body":{"model":"already-a-deployment","messages":[{"role":"user","content":"Hi"}]}}`,
	}
	var requests []schemas.BatchRequestItem
	for _, line := range lines {
		var item schemas.BatchRequestItem
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, item)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := newAzureDecisionProvider(t).BatchCreate(ctx, key, &schemas.BifrostBatchCreateRequest{
		Provider:           schemas.Azure,
		Model:              schemas.Ptr("prod-gpt-4o-mini"),
		Endpoint:           schemas.BatchEndpointChatCompletions,
		CompletionWindow:   "24h",
		Metadata:           map[string]string{"tokun_batch_id": "batch_tk_1"},
		OutputExpiresAfter: &schemas.BatchExpiresAfter{Anchor: "created_at", Seconds: 86400},
		Requests:           requests,
	})
	if bifrostErr != nil {
		t.Fatalf("BatchCreate: %+v", bifrostErr.Error)
	}
	if resp.ID != "batch_az1" {
		t.Errorf("id = %q, want the vendor's batch_az1", resp.ID)
	}
	if want := []string{"POST /openai/v1/files", "POST /openai/v1/batches"}; !reflect.DeepEqual(vendor.calls, want) {
		t.Errorf("upstream calls = %v, want %v", vendor.calls, want)
	}

	wantLines := []string{
		strings.Replace(lines[0], `"model":"gpt-4o-mini"`, `"model":"prod-gpt-4o-mini"`, 1),
		strings.Replace(lines[1], `"model":"azure/gpt-4o"`, `"model":"prod-gpt-4o"`, 1),
		lines[2],
	}
	uploaded := strings.Split(strings.TrimSpace(vendor.uploads[0]), "\n")
	if len(uploaded) != len(wantLines) {
		t.Fatalf("uploaded %d lines, want %d: %s", len(uploaded), len(wantLines), vendor.uploads[0])
	}
	for i := range wantLines {
		var got, want any
		if err := json.Unmarshal([]byte(uploaded[i]), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(wantLines[i]), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("line %d = %s, want %s", i+1, uploaded[i], wantLines[i])
		}
	}
	if model := requests[0].Body["model"]; model != "gpt-4o-mini" {
		t.Errorf("the caller's request was modified: body.model = %v", model)
	}

	var create map[string]any
	if err := json.Unmarshal([]byte(vendor.creates[0]), &create); err != nil {
		t.Fatal(err)
	}
	wantCreate := map[string]any{
		"input_file_id":        "file-in",
		"endpoint":             "/v1/chat/completions",
		"completion_window":    "24h",
		"metadata":             map[string]any{"tokun_batch_id": "batch_tk_1"},
		"output_expires_after": map[string]any{"anchor": "created_at", "seconds": float64(86400)},
	}
	if !reflect.DeepEqual(create, wantCreate) {
		t.Errorf("batch create body = %s, want %v", vendor.creates[0], wantCreate)
	}
}

// A file upload's expiry reaches Azure in the files API's bracketed form fields.
func TestAzureFileUploadSendsExpiresAfter(t *testing.T) {
	vendor := newAzureBatchVendor(t, "", nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, bifrostErr := newAzureDecisionProvider(t).FileUpload(ctx, newAzureDecisionKey(vendor.URL), &schemas.BifrostFileUploadRequest{
		Provider:     schemas.Azure,
		File:         []byte("{}\n"),
		Filename:     "in.jsonl",
		Purpose:      schemas.FilePurposeBatch,
		ExpiresAfter: &schemas.FileExpiresAfter{Anchor: "created_at", Seconds: 1209600},
	})
	if bifrostErr != nil {
		t.Fatalf("FileUpload: %+v", bifrostErr.Error)
	}
	form := vendor.uploadForms[0]
	if form["expires_after[anchor]"] != "created_at" || form["expires_after[seconds]"] != "1209600" || form["purpose"] != "batch" {
		t.Errorf("upload form = %v, want expires_after[anchor]=created_at, expires_after[seconds]=1209600, purpose=batch", form)
	}
}

// Azure results read the error file as well as the output file, with the line status, the
// vendor's line id, the unified usage and the batch's endpoint, and page by cursor.
func TestAzureBatchResultsMergeTheErrorFileAndPage(t *testing.T) {
	const batch = `{"id":"batch_az1","object":"batch","endpoint":"/chat/completions","status":"completed","output_file_id":"file-out","error_file_id":"file-err","created_at":1711471533}`
	output := `{"id":"batch_req_1","custom_id":"line-1","response":{"status_code":200,"request_id":"r1","body":{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop","content_filter_results":{"hate":{"filtered":false,"severity":"safe"}}}],"usage":{"prompt_tokens":40,"completion_tokens":3,"total_tokens":43,"prompt_tokens_details":{"cached_tokens":32}}}},"error":null}` + "\n" +
		`{"id":"batch_req_2","custom_id":"line-2","response":{"status_code":200,"request_id":"r2","body":{"id":"chatcmpl-2","object":"chat.completion","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}},"error":null}` + "\n"
	errorFile := `{"id":"batch_req_3","custom_id":"line-3","response":{"status_code":400,"request_id":"r3","body":{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy.","type":null,"param":"prompt","code":"content_filter"}}},"error":null}` + "\n" +
		`{"id":"batch_req_4","custom_id":"line-4","response":null,"error":{"code":"batch_expired","message":"This request could not be executed before the completion window expired."}}` + "\n"
	vendor := newAzureBatchVendor(t, batch, map[string]string{"file-out": output, "file-err": errorFile})
	provider := newAzureDecisionProvider(t)
	keys := []schemas.Key{newAzureDecisionKey(vendor.URL)}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	resp, bifrostErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Azure, BatchID: "batch_az1"})
	if bifrostErr != nil {
		t.Fatalf("BatchResults: %+v", bifrostErr.Error)
	}
	if resp.Endpoint != "/chat/completions" {
		t.Errorf("Endpoint = %q, want the batch's own endpoint", resp.Endpoint)
	}
	var got []string
	for _, item := range resp.Results {
		got = append(got, item.ID+" "+item.CustomID+" "+string(item.Status))
	}
	want := []string{"batch_req_1 line-1 succeeded", "batch_req_2 line-2 succeeded", "batch_req_3 line-3 failed", "batch_req_4 line-4 expired"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if code := resp.Results[2].Error.Code; code != "content_filter" {
		t.Errorf("failed line error code = %q, want the vendor's content_filter", code)
	}
	if code := resp.Results[3].Error.Code; code != "batch_expired" {
		t.Errorf("expired line error code = %q, want batch_expired", code)
	}
	choice := resp.Results[0].Response.Body["choices"].([]any)[0].(map[string]any)
	if _, ok := choice["content_filter_results"]; !ok {
		t.Errorf("Azure's content_filter_results was dropped from the line body: %v", choice)
	}
	if want := []string{"GET /openai/v1/batches/batch_az1", "GET /openai/v1/files/file-out/content", "GET /openai/v1/files/file-err/content"}; !reflect.DeepEqual(vendor.calls, want) {
		t.Errorf("upstream calls = %v, want %v", vendor.calls, want)
	}

	var paged []string
	var after *string
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("paging did not terminate")
		}
		pageResp, pageErr := provider.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: schemas.Azure, BatchID: "batch_az1", Limit: 3, After: after})
		if pageErr != nil {
			t.Fatalf("BatchResults page %d: %+v", page, pageErr.Error)
		}
		for _, item := range pageResp.Results {
			paged = append(paged, item.ID+" "+item.CustomID+" "+string(item.Status))
		}
		if !pageResp.HasMore {
			break
		}
		after = pageResp.NextCursor
	}
	if !reflect.DeepEqual(paged, want) {
		t.Errorf("paged lines = %v, want %v", paged, want)
	}
}

// TXE-6528 (contract C4): Azure image generation runs OpenAI's images API through the shared
// handlers, so a unified field that API has no field for is refused here too, on the sync and the
// streaming route: 400 caller_unsupported_parameter naming the field, and the request never
// reaches Azure. A request carrying only fields the API takes still makes its one call.
func TestAzureImageGenerationRejectsUndeliverableFields(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	}))
	defer server.Close()
	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(server.URL)
	request := func(params schemas.ImageGenerationParameters) *schemas.BifrostImageGenerationRequest {
		return &schemas.BifrostImageGenerationRequest{
			Provider: schemas.Azure,
			Model:    "gpt-image-1.5",
			Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
			Params:   &params,
		}
	}
	checkRefused := func(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
		t.Helper()
		if bifrostErr == nil {
			t.Fatalf("want a 400 naming %s", param)
		}
		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %v, want 400", bifrostErr.StatusCode)
		}
		if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerUnsupportedParameter {
			t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerUnsupportedParameter)
		}
		if bifrostErr.Error == nil || bifrostErr.Error.Param != param {
			t.Fatalf("error.param = %+v, want %q", bifrostErr.Error, param)
		}
	}
	for _, tc := range []struct {
		param  string
		params schemas.ImageGenerationParameters
	}{
		{"aspect_ratio", schemas.ImageGenerationParameters{AspectRatio: schemas.Ptr("16:9")}},
		{"input_images", schemas.ImageGenerationParameters{InputImages: []string{"https://example.com/a.png"}}},
		{"resolution", schemas.ImageGenerationParameters{Resolution: "2K"}},
		{"type", schemas.ImageGenerationParameters{Type: schemas.Ptr("vectorize")}},
	} {
		t.Run(tc.param, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.ImageGeneration(ctx, key, request(tc.params))
			checkRefused(t, bifrostErr, tc.param)
		})
		t.Run(tc.param+" stream", func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			stream, bifrostErr := provider.ImageGenerationStream(ctx, nil, nil, key, request(tc.params))
			if stream != nil {
				t.Fatal("a stream was opened for a refused request")
			}
			checkRefused(t, bifrostErr, tc.param)
		})
	}
	mu.Lock()
	if len(paths) != 0 {
		t.Fatalf("refused requests reached Azure: %v", paths)
	}
	mu.Unlock()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := provider.ImageGeneration(ctx, key, request(schemas.ImageGenerationParameters{N: schemas.Ptr(1), Size: schemas.Ptr("1024x1024")})); bifrostErr != nil {
		t.Fatalf("ImageGeneration with deliverable fields: %+v", bifrostErr.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/openai/v1/images/generations" {
		t.Fatalf("upstream calls = %v, want one to /openai/v1/images/generations", paths)
	}
}

// TestAzureVideoListForwardsFiltersAndPagesToTheEnd is contract C7 / BF15 for Azure Sora 2,
// whose list Tokun matches lost submissions against (recovery method 4): after, limit and order
// and any further filter reach /openai/v1/videos unchanged with the named key's api-key, and
// following last_id walks a recorded two-page response to its end.
func TestAzureVideoListForwardsFiltersAndPagesToTheEnd(t *testing.T) {
	var mu sync.Mutex
	var queries, apiKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/videos" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query().Encode())
		apiKeys = append(apiKeys, r.Header.Get("api-key"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after") == "video_b" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video_c","object":"video","model":"sora-2","status":"failed","created_at":1762700200}],"first_id":"video_c","last_id":"video_c","has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video_a","object":"video","model":"sora-2","status":"completed","created_at":1762700000},{"id":"video_b","object":"video","model":"sora-2","status":"in_progress","created_at":1762700100}],"first_id":"video_a","last_id":"video_b","has_more":true}`))
	}))
	defer srv.Close()

	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(srv.URL)
	request := &schemas.BifrostVideoListRequest{
		Provider: schemas.Azure,
		Limit:    schemas.Ptr(2),
		Order:    schemas.Ptr("desc"),
		Filters:  map[string][]string{"status": {"completed"}},
	}
	var ids []string
	for page := 1; ; page++ {
		if page > 3 {
			t.Fatal("paging did not end after the recorded two pages")
		}
		resp, bifrostErr := provider.VideoList(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key, request)
		if bifrostErr != nil {
			t.Fatalf("page %d: %v", page, bifrostErr.Error)
		}
		for _, video := range resp.Data {
			ids = append(ids, video.ID)
		}
		if resp.HasMore == nil || !*resp.HasMore {
			break
		}
		request.After = resp.LastID
	}

	if got := strings.Join(ids, ","); got != "video_a:azure,video_b:azure,video_c:azure" {
		t.Fatalf("ids = %s", got)
	}
	wantQueries := []string{"limit=2&order=desc&status=completed", "after=video_b&limit=2&order=desc&status=completed"}
	if !reflect.DeepEqual(queries, wantQueries) {
		t.Fatalf("vendor queries = %q, want %q", queries, wantQueries)
	}
	if !reflect.DeepEqual(apiKeys, []string{"test-key", "test-key"}) {
		t.Fatalf("api-key headers = %q", apiKeys)
	}
}

// TestAzureVideoDownloadStreamsOnRequest is contract C9 / BF20 for Azure Sora 2, whose file can
// only be fetched with the resource's key: one call to /openai/v1/videos/{id}/content with that
// key, and the body handed on as a stream.
func TestAzureVideoDownloadStreamsOnRequest(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("api-key"))
		mu.Unlock()
		if r.URL.Path == "/openai/v1/videos/video_gone/content" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Video not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("SORA-MP4"))
	}))
	defer srv.Close()

	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(srv.URL)
	resp, bifrostErr := provider.VideoDownload(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key,
		&schemas.BifrostVideoDownloadRequest{Provider: schemas.Azure, ID: "video_a:azure", Stream: true})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload: %v", bifrostErr.Error)
	}
	if resp.Body == nil || resp.Content != nil || resp.ContentType != "video/mp4" || resp.ContentLength != int64(len("SORA-MP4")) {
		t.Fatalf("streamed response = %+v", resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.Body.Close() != nil || string(body) != "SORA-MP4" {
		t.Fatalf("body = %q, %v", body, err)
	}

	_, bifrostErr = provider.VideoDownload(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), key,
		&schemas.BifrostVideoDownloadRequest{Provider: schemas.Azure, ID: "video_gone:azure", Stream: true})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusNotFound || bifrostErr.Error.Message != "Video not found" {
		t.Fatalf("want Azure's 404, got %+v", bifrostErr)
	}

	want := []string{"GET /openai/v1/videos/video_a/content test-key", "GET /openai/v1/videos/video_gone/content test-key"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("upstream calls = %q, want %q", calls, want)
	}
}

// azureVideoCall is one request the fake Azure resource received.
type azureVideoCall struct {
	method, path, apiKey string
	form                 map[string][]string
}

// newAzureVideoServer is a fake Azure resource that records each request (multipart form fields
// included) and answers with respond.
func newAzureVideoServer(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []azureVideoCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []azureVideoCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := azureVideoCall{method: r.Method, path: r.URL.Path, apiKey: r.Header.Get("api-key")}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart form: %v", err)
			} else {
				call.form = r.MultipartForm.Value
			}
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, func() []azureVideoCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]azureVideoCall(nil), calls...)
	}
}

func azureVideoQueued(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"id":"video_1","object":"video","status":"queued","created_at":1,"model":"sora-2","seconds":"4","size":"720x1280"}`)
}

func newAzureVideoRequest(input *schemas.VideoGenerationInput, params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	return &schemas.BifrostVideoGenerationRequest{Provider: schemas.Azure, Model: "sora-2", Input: input, Params: params}
}

func newAzureVideoContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

func checkAzureVideoFieldRefused(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
	t.Helper()
	if bifrostErr == nil {
		t.Fatalf("want a 400 naming %s", param)
	}
	if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %v, want 400: %+v", bifrostErr.StatusCode, bifrostErr.Error)
	}
	if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerUnsupportedParameter {
		t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerUnsupportedParameter)
	}
	if bifrostErr.Error == nil || bifrostErr.Error.Param != param {
		t.Fatalf("error.param = %+v, want %q", bifrostErr.Error, param)
	}
}

// TestAzureVideoGenerationSizes pins contract C4 rule three: a size Azure does not have is refused
// with no upstream call instead of being rewritten to 720x1280, and each of the four it has
// reaches the vendor unchanged.
func TestAzureVideoGenerationSizes(t *testing.T) {
	server, calls := newAzureVideoServer(t, azureVideoQueued)
	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(server.URL)
	input := &schemas.VideoGenerationInput{Prompt: "a fox"}

	_, bifrostErr := provider.VideoGeneration(newAzureVideoContext(), key, newAzureVideoRequest(input, &schemas.VideoGenerationParameters{Size: "1920x1080"}))
	checkAzureVideoFieldRefused(t, bifrostErr, "size")
	if got := calls(); len(got) != 0 {
		t.Fatalf("a refused size reached the vendor: %+v", got)
	}

	sizes := []string{"720x1280", "1280x720", "1024x1792", "1792x1024"}
	for _, size := range sizes {
		if _, bifrostErr := provider.VideoGeneration(newAzureVideoContext(), key, newAzureVideoRequest(input, &schemas.VideoGenerationParameters{Size: size})); bifrostErr != nil {
			t.Fatalf("size %s: %+v", size, bifrostErr.Error)
		}
	}
	got := calls()
	if len(got) != len(sizes) {
		t.Fatalf("calls = %d, want %d", len(got), len(sizes))
	}
	for i, size := range sizes {
		if got[i].path != "/openai/v1/videos" {
			t.Errorf("path = %q", got[i].path)
		}
		if values := got[i].form["size"]; len(values) != 1 || values[0] != size {
			t.Errorf("form size = %v, want [%s]", values, size)
		}
	}
}

// TestAzureVideoGenerationRefusesURLInputReference pins C4: Azure takes input_reference only as
// bytes and Bifrost does not download, so a link is refused with no upstream call.
func TestAzureVideoGenerationRefusesURLInputReference(t *testing.T) {
	server, calls := newAzureVideoServer(t, azureVideoQueued)
	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(server.URL)
	for _, link := range []string{"http://example.com/a.png", "https://example.com/a.png"} {
		input := &schemas.VideoGenerationInput{Prompt: "a fox", InputReference: schemas.Ptr(link)}
		_, bifrostErr := provider.VideoGeneration(newAzureVideoContext(), key, newAzureVideoRequest(input, nil))
		checkAzureVideoFieldRefused(t, bifrostErr, "input_reference")
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("a refused input_reference reached the vendor: %+v", got)
	}
}

// TestAzureVideoGenerationWritesExtraParamsAsFormFields pins C4 agreement two on the multipart
// body: with x-bf-passthrough-extra-params each extra field is one form field, and a typed field
// is never written twice.
func TestAzureVideoGenerationWritesExtraParamsAsFormFields(t *testing.T) {
	server, calls := newAzureVideoServer(t, azureVideoQueued)
	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(server.URL)
	input := &schemas.VideoGenerationInput{Prompt: "a fox"}
	params := func() *schemas.VideoGenerationParameters {
		return &schemas.VideoGenerationParameters{
			Size:        "1280x720",
			ExtraParams: map[string]any{"vendor_hint": "high", "vendor_count": 2, "prompt": "not this", "size": "1x1"},
		}
	}

	ctx := newAzureVideoContext()
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	if _, bifrostErr := provider.VideoGeneration(ctx, key, newAzureVideoRequest(input, params())); bifrostErr != nil {
		t.Fatalf("create: %+v", bifrostErr.Error)
	}
	form := calls()[0].form
	for field, want := range map[string]string{"vendor_hint": "high", "vendor_count": "2", "prompt": "a fox", "size": "1280x720"} {
		if values := form[field]; len(values) != 1 || values[0] != want {
			t.Errorf("form %s = %v, want [%s]", field, values, want)
		}
	}

	if _, bifrostErr := provider.VideoGeneration(newAzureVideoContext(), key, newAzureVideoRequest(input, params())); bifrostErr != nil {
		t.Fatalf("create without passthrough: %+v", bifrostErr.Error)
	}
	if values, ok := calls()[1].form["vendor_hint"]; ok {
		t.Errorf("extra field sent without x-bf-passthrough-extra-params: %v", values)
	}
}

// TestAzureVideoRetrieveFillsTheTaskObject pins C5, C7 and C9 on a query: the vendor's raw status,
// its seconds as usage at a completed task only, and an artifact marked as fetched through Bifrost.
func TestAzureVideoRetrieveFillsTheTaskObject(t *testing.T) {
	var body string
	server, _ := newAzureVideoServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(server.URL)
	retrieve := func(t *testing.T, vendorBody string) *schemas.BifrostVideoGenerationResponse {
		t.Helper()
		body = vendorBody
		resp, bifrostErr := provider.VideoRetrieve(newAzureVideoContext(), key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Azure, ID: "video_1:azure"})
		if bifrostErr != nil {
			t.Fatalf("retrieve: %+v", bifrostErr.Error)
		}
		return resp
	}

	t.Run("completed", func(t *testing.T) {
		resp := retrieve(t, `{"id":"video_1","object":"video","status":"completed","created_at":1,"completed_at":2,"expires_at":90000,"model":"sora-2","seconds":"8","size":"1280x720"}`)
		if resp.Status != schemas.VideoStatusCompleted || resp.ProviderStatus != "completed" {
			t.Fatalf("status/provider_status = %q/%q", resp.Status, resp.ProviderStatus)
		}
		if resp.Usage == nil || resp.Usage.OutputSeconds == nil || *resp.Usage.OutputSeconds != 8 {
			t.Fatalf("usage = %+v, want output_seconds 8", resp.Usage)
		}
		if len(resp.Videos) != 1 || resp.Videos[0].Type != schemas.VideoOutputTypeContent || resp.Videos[0].URL != nil || resp.Videos[0].ContentType != "video/mp4" {
			t.Fatalf("videos = %+v, want one content entry without url", resp.Videos)
		}
		if resp.ExpiresAt == nil || *resp.ExpiresAt != 90000 {
			t.Fatalf("expires_at = %v, want the vendor's value", resp.ExpiresAt)
		}
	})

	t.Run("not finished", func(t *testing.T) {
		resp := retrieve(t, `{"id":"video_1","object":"video","status":"in_progress","created_at":1,"model":"sora-2","seconds":"8","size":"1280x720"}`)
		if resp.ProviderStatus != "in_progress" || resp.Usage != nil || len(resp.Videos) != 0 {
			t.Fatalf("provider_status/usage/videos = %q/%+v/%+v", resp.ProviderStatus, resp.Usage, resp.Videos)
		}
	})

	t.Run("failed reports no output", func(t *testing.T) {
		resp := retrieve(t, `{"id":"video_1","object":"video","status":"failed","created_at":1,"model":"sora-2","seconds":"8","size":"1280x720","error":{"code":"internal_error","message":"boom"}}`)
		if resp.Status != schemas.VideoStatusFailed || resp.ProviderStatus != "failed" || resp.Usage != nil || len(resp.Videos) != 0 {
			t.Fatalf("status/provider_status/usage/videos = %q/%q/%+v/%+v", resp.Status, resp.ProviderStatus, resp.Usage, resp.Videos)
		}
		if resp.Error == nil || resp.Error.Code != "internal_error" {
			t.Fatalf("error = %+v, want the vendor's code", resp.Error)
		}
	})

	t.Run("a status the vendor adds later is not terminal", func(t *testing.T) {
		resp := retrieve(t, `{"id":"video_1","object":"video","status":"preprocessing","created_at":1,"model":"sora-2"}`)
		if resp.Status != schemas.VideoStatusInProgress || resp.ProviderStatus != "preprocessing" {
			t.Fatalf("status/provider_status = %q/%q", resp.Status, resp.ProviderStatus)
		}
	})
}

// TestAzureVideoCreateReportsProviderStatus: create and query return the same task object (C7).
func TestAzureVideoCreateReportsProviderStatus(t *testing.T) {
	server, _ := newAzureVideoServer(t, azureVideoQueued)
	provider := newAzureDecisionProvider(t)
	resp, bifrostErr := provider.VideoGeneration(newAzureVideoContext(), newAzureDecisionKey(server.URL), newAzureVideoRequest(&schemas.VideoGenerationInput{Prompt: "a fox"}, nil))
	if bifrostErr != nil {
		t.Fatalf("create: %+v", bifrostErr.Error)
	}
	if resp.ID != "video_1:azure" || resp.Status != schemas.VideoStatusQueued || resp.ProviderStatus != "queued" {
		t.Fatalf("id/status/provider_status = %q/%q/%q", resp.ID, resp.Status, resp.ProviderStatus)
	}
}

// TestAzureVideoDeclaresOnlyWhatItDelivers pins C4 rule three and C7: Sora 2 on Azure has seconds
// and size and nothing else from the unified set, so every other priced field and every task
// control field is refused before a key is chosen, while seed and negative_prompt stay exempt.
func TestAzureVideoDeclaresOnlyWhatItDelivers(t *testing.T) {
	provider := newAzureDecisionProvider(t)
	check := func(params schemas.VideoGenerationParameters) *schemas.BifrostError {
		request := &schemas.BifrostRequest{
			RequestType:            schemas.VideoGenerationRequest,
			VideoGenerationRequest: newAzureVideoRequest(&schemas.VideoGenerationInput{Prompt: "a fox"}, &params),
		}
		if bifrostErr := schemas.CheckDeclaredUnifiedFields(provider, request); bifrostErr != nil {
			return bifrostErr
		}
		return schemas.CheckTaskControlFields(provider, request)
	}
	for _, tc := range []struct {
		param  string
		params schemas.VideoGenerationParameters
	}{
		{"aspect_ratio", schemas.VideoGenerationParameters{AspectRatio: "16:9"}},
		{"audio", schemas.VideoGenerationParameters{Audio: schemas.Ptr(true)}},
		{"last_frame", schemas.VideoGenerationParameters{LastFrame: "data:image/png;base64,aGk="}},
		{"output_format", schemas.VideoGenerationParameters{OutputFormat: schemas.Ptr("webm")}},
		{"reference_images", schemas.VideoGenerationParameters{ReferenceImages: schemas.VideoReferenceImages{"https://example.com/a.png"}}},
		{"resolution", schemas.VideoGenerationParameters{Resolution: "1080p"}},
		{"callback_url", schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://example.com/hook")}}},
		{"expires_after_seconds", schemas.VideoGenerationParameters{TaskControlParameters: schemas.TaskControlParameters{ExpiresAfterSeconds: schemas.Ptr(600)}}},
	} {
		checkAzureVideoFieldRefused(t, check(tc.params), tc.param)
	}
	if bifrostErr := check(schemas.VideoGenerationParameters{
		Seconds:        schemas.Ptr("8"),
		Size:           "1280x720",
		Seed:           schemas.Ptr(7),
		NegativePrompt: schemas.Ptr("no text"),
	}); bifrostErr != nil {
		t.Fatalf("a field Azure takes or an exempt one was refused: %+v", bifrostErr.Error)
	}
}

// TestAzureHasNoVideoCancel pins C7: Azure has no cancel API and its DELETE removes the stored
// video, so the provider must not implement schemas.VideoCanceller; core then answers 400
// provider_cancel_unsupported without an upstream call.
func TestAzureHasNoVideoCancel(t *testing.T) {
	provider := newAzureDecisionProvider(t)
	if _, ok := any(provider).(schemas.VideoCanceller); ok {
		t.Fatal("azure implements VideoCanceller; a cancel must never reach Azure's DELETE")
	}
	bifrostErr := schemas.CheckVideoCancelSupported(provider, &schemas.BifrostRequest{RequestType: schemas.VideoCancelRequest})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
		t.Fatalf("cancel = %+v, want 400 provider_cancel_unsupported", bifrostErr)
	}
}

// TXE-6984: Azure's image edit runs on the same OpenAI handler, so an edit to Azure carrying a
// unified field OpenAI's edit API has no field for is refused the same way, sync and streaming,
// before any upstream call; an edit with fields it takes makes its one call.
func TestAzureImageEditRejectsUndeliverableFields(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	}))
	defer server.Close()
	provider := newAzureDecisionProvider(t)
	key := newAzureDecisionKey(server.URL)
	request := func(params schemas.ImageEditParameters) *schemas.BifrostImageEditRequest {
		return &schemas.BifrostImageEditRequest{
			Provider: schemas.Azure,
			Model:    "gpt-image-1.5",
			Input:    &schemas.ImageEditInput{Prompt: "add a hat", Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake")}}},
			Params:   &params,
		}
	}
	checkRefused := func(t *testing.T, bifrostErr *schemas.BifrostError, param string) {
		t.Helper()
		if bifrostErr == nil {
			t.Fatalf("want a 400 naming %s", param)
		}
		if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %v, want 400", bifrostErr.StatusCode)
		}
		if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerUnsupportedParameter {
			t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerUnsupportedParameter)
		}
		if bifrostErr.Error == nil || bifrostErr.Error.Param != param {
			t.Fatalf("error.param = %+v, want %q", bifrostErr.Error, param)
		}
	}
	for _, tc := range []struct {
		param  string
		params schemas.ImageEditParameters
	}{
		{"aspect_ratio", schemas.ImageEditParameters{AspectRatio: schemas.Ptr("16:9")}},
		{"resolution", schemas.ImageEditParameters{Resolution: "2K"}},
		{"target_megapixels", schemas.ImageEditParameters{TargetMegapixels: schemas.Ptr(4)}},
		{"type", schemas.ImageEditParameters{Type: schemas.Ptr("upscale")}},
		{"upscale_factor", schemas.ImageEditParameters{UpscaleFactor: schemas.Ptr(2)}},
	} {
		t.Run(tc.param, func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := provider.ImageEdit(ctx, key, request(tc.params))
			checkRefused(t, bifrostErr, tc.param)
		})
		t.Run(tc.param+" stream", func(t *testing.T) {
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			stream, bifrostErr := provider.ImageEditStream(ctx, nil, nil, key, request(tc.params))
			if stream != nil {
				t.Fatal("a stream was opened for a refused request")
			}
			checkRefused(t, bifrostErr, tc.param)
		})
	}
	mu.Lock()
	if len(paths) != 0 {
		t.Fatalf("refused requests reached Azure: %v", paths)
	}
	mu.Unlock()

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := provider.ImageEdit(ctx, key, request(schemas.ImageEditParameters{N: schemas.Ptr(1), Size: schemas.Ptr("1024x1024")})); bifrostErr != nil {
		t.Fatalf("ImageEdit with deliverable fields: %+v", bifrostErr.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/openai/v1/images/edits" {
		t.Fatalf("upstream calls = %v, want one to /openai/v1/images/edits", paths)
	}
}

// TXE-6984: Azure's image streams run on the same OpenAI handlers, so the completed frame of a
// streamed Azure generation or edit carries usage.output_images too: 1 for the one finished image,
// with two previews in the stream, next to the token usage Azure reported.
func TestAzureImageStreamReportsOutputImages(t *testing.T) {
	for _, kind := range []string{"image_generation", "image_edit"} {
		t.Run(kind, func(t *testing.T) {
			sse := "data: " + `{"type":"` + kind + `.partial_image","b64_json":"cDA=","partial_image_index":0,"sequence_number":1}` + "\n\n" +
				"data: " + `{"type":"` + kind + `.partial_image","b64_json":"cDE=","partial_image_index":1,"sequence_number":2}` + "\n\n" +
				"data: " + `{"type":"` + kind + `.completed","b64_json":"aGk=","sequence_number":3,"usage":{"input_tokens":12,"output_tokens":272,"total_tokens":284}}` + "\n\n"
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sse)
			}))
			defer server.Close()
			provider := newAzureDecisionProvider(t)
			key := newAzureDecisionKey(server.URL)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			postHook := func(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return resp, err
			}
			var stream chan *schemas.BifrostStreamChunk
			var bifrostErr *schemas.BifrostError
			wantPath := "/openai/v1/images/generations"
			if kind == "image_generation" {
				stream, bifrostErr = provider.ImageGenerationStream(ctx, postHook, nil, key, &schemas.BifrostImageGenerationRequest{
					Provider: schemas.Azure,
					Model:    "gpt-image-1.5",
					Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
					Params:   &schemas.ImageGenerationParameters{PartialImages: schemas.Ptr(2)},
				})
			} else {
				wantPath = "/openai/v1/images/edits"
				stream, bifrostErr = provider.ImageEditStream(ctx, postHook, nil, key, &schemas.BifrostImageEditRequest{
					Provider: schemas.Azure,
					Model:    "gpt-image-1.5",
					Input:    &schemas.ImageEditInput{Prompt: "add a hat", Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake")}}},
					Params:   &schemas.ImageEditParameters{PartialImages: schemas.Ptr(2)},
				})
			}
			if bifrostErr != nil {
				t.Fatalf("stream setup: %+v", bifrostErr.Error)
			}
			var frames []*schemas.BifrostImageGenerationStreamResponse
			timeout := time.After(20 * time.Second)
		collect:
			for {
				select {
				case chunk, ok := <-stream:
					if !ok {
						break collect
					}
					if chunk == nil {
						continue
					}
					if chunk.BifrostError != nil {
						t.Fatalf("stream error: %+v", chunk.BifrostError.Error)
					}
					frames = append(frames, chunk.BifrostImageGenerationStreamResponse)
				case <-timeout:
					t.Fatal("the stream did not close")
				}
			}
			if len(frames) != 3 {
				t.Fatalf("frames = %d, want 3 (two previews, completed)", len(frames))
			}
			for i, frame := range frames[:2] {
				if frame == nil || frame.Usage != nil {
					t.Fatalf("preview %d = %+v, want a preview frame with no usage", i, frame)
				}
			}
			final := frames[2]
			if final == nil || final.Usage == nil || final.Usage.OutputImages == nil || *final.Usage.OutputImages != 1 {
				t.Fatalf("completed frame usage = %+v, want output_images 1", final)
			}
			if final.Usage.InputTokens != 12 || final.Usage.OutputTokens != 272 || final.Usage.TotalTokens != 284 {
				t.Fatalf("usage = %+v, want Azure's 12/272/284 kept", final.Usage)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(paths) != 1 || paths[0] != wantPath {
				t.Fatalf("upstream calls = %v, want one to %s", paths, wantPath)
			}
		})
	}
}

// First-generation sora on Azure (TXE-6533): the video generation jobs API.

// azureSoraJobQueued is the create response sample from the Azure video generation quickstart.
const azureSoraJobQueued = `{
    "object": "video.generation.job",
    "id": "task_01jwcet0eje35tc5jy54yjax5q",
    "status": "queued",
    "created_at": 1748469875,
    "finished_at": null,
    "expires_at": null,
    "generations": [],
    "prompt": "A cat playing piano in a jazz bar.",
    "model": "sora",
    "n_variants": 1,
    "n_seconds": 5,
    "height": 480,
    "width": 480,
    "failure_reason": null
}`

// azureSoraJobWithStatus is a job in the reference's VideoGenerationJob shape; a succeeded one
// carries one generation in the reference's VideoGeneration shape.
func azureSoraJobWithStatus(status string) string {
	generations, finished, expires, failure := `[]`, `null`, `null`, `null`
	switch status {
	case "succeeded":
		generations = `[{"object":"video.generation","id":"gen_01jwcf0a","job_id":"task_01jwcet0eje35tc5jy54yjax5q","created_at":1748469990,"width":480,"height":480,"n_seconds":5,"prompt":"A cat playing piano in a jazz bar."}]`
		finished, expires = `1748470000`, `1748556400`
	case "failed":
		finished, failure = `1748470000`, `"output_moderation"`
	}
	return `{"object":"video.generation.job","id":"task_01jwcet0eje35tc5jy54yjax5q","status":"` + status +
		`","created_at":1748469875,"finished_at":` + finished + `,"expires_at":` + expires +
		`,"generations":` + generations + `,"prompt":"A cat playing piano in a jazz bar.","model":"sora","n_variants":1,"n_seconds":5,"height":480,"width":480,"failure_reason":` + failure + `}`
}

type azureSoraJobsCall struct {
	method, path, query, apiKey string
	body                        []byte
}

// azureSoraJobsServer answers every jobs API call with respond and records each call.
func azureSoraJobsServer(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, func() []azureSoraJobsCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []azureSoraJobsCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, azureSoraJobsCall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("api-key"), body})
		mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, func() []azureSoraJobsCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]azureSoraJobsCall(nil), calls...)
	}
}

func newAzureSoraJobsRequest(params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	return &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Azure,
		Model:    "sora",
		Input:    &schemas.VideoGenerationInput{Prompt: "A cat playing piano in a jazz bar."},
		Params:   params,
	}
}

// TestAzureSoraJobsCreateSendsJobsBody pins the create: one POST to the jobs API with
// n_seconds, width and height from seconds and size, and a task object whose id names the job
// and ends ":azure".
func TestAzureSoraJobsCreateSendsJobsBody(t *testing.T) {
	server, calls := azureSoraJobsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, azureSoraJobQueued)
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := newAzureDecisionProvider(t).VideoGeneration(ctx, newAzureDecisionKey(server.URL),
		newAzureSoraJobsRequest(&schemas.VideoGenerationParameters{Seconds: schemas.Ptr("5"), Size: "480x480"}))
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration: %+v", bifrostErr.Error)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(got))
	}
	if got[0].method != http.MethodPost || got[0].path != "/openai/v1/video/generations/jobs" || got[0].query != "api-version=preview" || got[0].apiKey != "test-key" {
		t.Fatalf("call = %s %s?%s key %q, want POST /openai/v1/video/generations/jobs?api-version=preview with the key", got[0].method, got[0].path, got[0].query, got[0].apiKey)
	}
	var sent, want map[string]any
	if err := json.Unmarshal(got[0].body, &sent); err != nil {
		t.Fatalf("body is not JSON: %s", got[0].body)
	}
	_ = json.Unmarshal([]byte(`{"prompt":"A cat playing piano in a jazz bar.","model":"sora","width":480,"height":480,"n_seconds":5}`), &want)
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("body = %s, want the quickstart's jobs body", got[0].body)
	}
	if resp.ID != "sora1.task_01jwcet0eje35tc5jy54yjax5q:azure" {
		t.Errorf("id = %q, want sora1.task_01jwcet0eje35tc5jy54yjax5q:azure", resp.ID)
	}
	if resp.Status != schemas.VideoStatusQueued || resp.ProviderStatus != "queued" {
		t.Errorf("status = %q/%q, want queued/queued", resp.Status, resp.ProviderStatus)
	}
	if resp.CreatedAt != 1748469875 || resp.Size != "480x480" || resp.Seconds == nil || *resp.Seconds != "5" {
		t.Errorf("task = created %d size %q seconds %v, want the job's", resp.CreatedAt, resp.Size, resp.Seconds)
	}
	if resp.ExpiresAt != nil || len(resp.Videos) != 0 || resp.Usage != nil {
		t.Errorf("queued task carries expires_at %v, videos %v, usage %+v; want none", resp.ExpiresAt, resp.Videos, resp.Usage)
	}
}

// TestAzureSoraJobsQueryMapsEveryStatus pins the status table: each job status the Azure
// quickstart names maps to a unified status, with the raw one in provider_status.
func TestAzureSoraJobsQueryMapsEveryStatus(t *testing.T) {
	for raw, want := range map[string]schemas.VideoStatus{
		"queued":        schemas.VideoStatusQueued,
		"preprocessing": schemas.VideoStatusInProgress,
		"running":       schemas.VideoStatusInProgress,
		"processing":    schemas.VideoStatusInProgress,
		"succeeded":     schemas.VideoStatusCompleted,
		"failed":        schemas.VideoStatusFailed,
		"cancelled":     schemas.VideoStatusCancelled,
		"some_new_word": schemas.VideoStatusInProgress,
	} {
		t.Run(raw, func(t *testing.T) {
			server, calls := azureSoraJobsServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, azureSoraJobWithStatus(raw))
			})
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			resp, bifrostErr := newAzureDecisionProvider(t).VideoRetrieve(ctx, newAzureDecisionKey(server.URL),
				&schemas.BifrostVideoRetrieveRequest{Provider: schemas.Azure, ID: "sora1.task_01jwcet0eje35tc5jy54yjax5q:azure"})
			if bifrostErr != nil {
				t.Fatalf("VideoRetrieve: %+v", bifrostErr.Error)
			}
			if c := calls(); len(c) != 1 || c[0].method != http.MethodGet || c[0].path != "/openai/v1/video/generations/jobs/task_01jwcet0eje35tc5jy54yjax5q" {
				t.Fatalf("calls = %+v, want one GET of the job", c)
			}
			if resp.Status != want || resp.ProviderStatus != raw {
				t.Errorf("status = %q/%q, want %q/%q", resp.Status, resp.ProviderStatus, want, raw)
			}
			switch raw {
			case "succeeded":
				if len(resp.Videos) != 1 || resp.Videos[0].Type != schemas.VideoOutputTypeContent || resp.Videos[0].URL != nil || resp.Videos[0].ID != "gen_01jwcf0a" || resp.Videos[0].ContentType != "video/mp4" {
					t.Errorf("videos = %+v, want one content entry for gen_01jwcf0a", resp.Videos)
				}
				if resp.Usage == nil || resp.Usage.OutputSeconds == nil || *resp.Usage.OutputSeconds != 5 {
					t.Errorf("usage = %+v, want output_seconds 5 from the generation", resp.Usage)
				}
				if resp.CompletedAt == nil || *resp.CompletedAt != 1748470000 || resp.ExpiresAt == nil || *resp.ExpiresAt != 1748556400 {
					t.Errorf("completed_at %v expires_at %v, want the job's finished_at and expires_at", resp.CompletedAt, resp.ExpiresAt)
				}
			case "failed":
				if resp.Error == nil || resp.Error.Code != "output_moderation" {
					t.Errorf("error = %+v, want the job's failure_reason as the code", resp.Error)
				}
				if len(resp.Videos) != 0 || resp.Usage != nil {
					t.Errorf("failed task carries videos %v usage %+v", resp.Videos, resp.Usage)
				}
			}
		})
	}
}

// TestAzureSoraJobsDownloadStreamsTheGenerationWithTheKey pins the artifact: the job is
// queried, then the generation's video is fetched with the same key and returned as is.
func TestAzureSoraJobsDownloadStreamsTheGenerationWithTheKey(t *testing.T) {
	server, calls := azureSoraJobsServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/v1/video/generations/jobs/task_01jwcet0eje35tc5jy54yjax5q":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, azureSoraJobWithStatus("succeeded"))
		case "/openai/v1/video/generations/gen_01jwcf0a/content/video":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = io.WriteString(w, "mp4-bytes")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, bifrostErr := newAzureDecisionProvider(t).VideoDownload(ctx, newAzureDecisionKey(server.URL),
		&schemas.BifrostVideoDownloadRequest{Provider: schemas.Azure, ID: "sora1.task_01jwcet0eje35tc5jy54yjax5q:azure"})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload: %+v", bifrostErr.Error)
	}
	if string(resp.Content) != "mp4-bytes" || resp.ContentType != "video/mp4" {
		t.Errorf("content = %q (%s), want the generation's mp4", resp.Content, resp.ContentType)
	}
	got := calls()
	if len(got) != 2 || got[1].path != "/openai/v1/video/generations/gen_01jwcf0a/content/video" || got[1].query != "api-version=preview" {
		t.Fatalf("calls = %+v, want the job query then the generation's content", got)
	}
	for _, c := range got {
		if c.apiKey != "test-key" {
			t.Errorf("%s sent key %q, want the creating key", c.path, c.apiKey)
		}
	}
}

// TestAzureSoraJobsRefusesUndeliverableFields pins contract C4 convention three on the jobs
// API: a priced field it cannot take, or a value it cannot be sent as, is a 400
// caller_unsupported_parameter naming it, and nothing reaches Azure.
func TestAzureSoraJobsRefusesUndeliverableFields(t *testing.T) {
	server, calls := azureSoraJobsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, azureSoraJobQueued)
	})
	for _, tc := range []struct {
		param  string
		params schemas.VideoGenerationParameters
		input  *string
	}{
		{"aspect_ratio", schemas.VideoGenerationParameters{AspectRatio: "16:9"}, nil},
		{"resolution", schemas.VideoGenerationParameters{Resolution: "1080p"}, nil},
		{"last_frame", schemas.VideoGenerationParameters{LastFrame: "https://example.com/a.png"}, nil},
		{"reference_images", schemas.VideoGenerationParameters{ReferenceImages: []string{"https://example.com/a.png"}}, nil},
		{"audio", schemas.VideoGenerationParameters{Audio: schemas.Ptr(true)}, nil},
		{"output_format", schemas.VideoGenerationParameters{OutputFormat: schemas.Ptr("webm")}, nil},
		{"seconds", schemas.VideoGenerationParameters{Seconds: schemas.Ptr("2.5")}, nil},
		{"size", schemas.VideoGenerationParameters{Size: "1080p"}, nil},
		{"n", schemas.VideoGenerationParameters{ExtraParams: map[string]any{"n": float64(2)}}, nil},
		{"n_variants", schemas.VideoGenerationParameters{ExtraParams: map[string]any{"n_variants": float64(3)}}, nil},
		{"input_reference", schemas.VideoGenerationParameters{}, schemas.Ptr("https://example.com/a.png")},
	} {
		t.Run(tc.param, func(t *testing.T) {
			request := newAzureSoraJobsRequest(&tc.params)
			request.Input.InputReference = tc.input
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := newAzureDecisionProvider(t).VideoGeneration(ctx, newAzureDecisionKey(server.URL), request)
			if bifrostErr == nil {
				t.Fatalf("want a 400 naming %s", tc.param)
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %v, want 400", bifrostErr.StatusCode)
			}
			if got := bifrostErr.ResolveErrorType(); got != schemas.ErrorTypeCallerUnsupportedParameter {
				t.Fatalf("error_type = %q, want %q", got, schemas.ErrorTypeCallerUnsupportedParameter)
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Param != tc.param {
				t.Fatalf("error.param = %+v, want %q", bifrostErr.Error, tc.param)
			}
		})
	}
	if c := calls(); len(c) != 0 {
		t.Fatalf("refused requests reached Azure: %+v", c)
	}

	// One variant, audio off and mp4 are deliverable as asked.
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, bifrostErr := newAzureDecisionProvider(t).VideoGeneration(ctx, newAzureDecisionKey(server.URL), newAzureSoraJobsRequest(&schemas.VideoGenerationParameters{
		Audio: schemas.Ptr(false), OutputFormat: schemas.Ptr("mp4"), ExtraParams: map[string]any{"n_variants": float64(1)},
	})); bifrostErr != nil {
		t.Fatalf("VideoGeneration with deliverable fields: %+v", bifrostErr.Error)
	}
}

// TestAzureSoraJobsErrorsKeepTheVendorStatus pins contract C6 on the jobs API: the vendor's
// status stays, and a ResponsibleAIPolicyViolation is caller_content_filtered.
func TestAzureSoraJobsErrorsKeepTheVendorStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		body      string
		errorType schemas.ErrorType
	}{
		"content filtered": {400, `{"error":{"code":"content_policy_violation_custom","message":"Your request was rejected by the safety system.","inner_error":{"code":"ResponsibleAIPolicyViolation"}}}`, schemas.ErrorTypeCallerContentFiltered},
		"unwrapped body":   {400, `{"code":"invalid_size","message":"Unsupported resolution","inner_error":{"code":"ResponsibleAIPolicyViolation"}}`, schemas.ErrorTypeCallerContentFiltered},
		"rate limited":     {429, `{"error":{"code":"429","message":"Too Many Requests"}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := azureSoraJobsServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			_, bifrostErr := newAzureDecisionProvider(t).VideoGeneration(ctx, newAzureDecisionKey(server.URL), newAzureSoraJobsRequest(nil))
			if bifrostErr == nil {
				t.Fatal("want the vendor's error")
			}
			if bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != tc.status {
				t.Fatalf("status = %v, want %d", bifrostErr.StatusCode, tc.status)
			}
			if tc.errorType != "" && bifrostErr.ExtraFields.ErrorType != tc.errorType {
				t.Fatalf("error_type = %q, want %q", bifrostErr.ExtraFields.ErrorType, tc.errorType)
			}
			if tc.errorType == "" && bifrostErr.ExtraFields.ErrorType == schemas.ErrorTypeCallerContentFiltered {
				t.Fatal("a rate limit was marked content filtered")
			}
			if bifrostErr.Error == nil || bifrostErr.Error.Message == "" || strings.HasPrefix(bifrostErr.Error.Message, "provider API error") {
				t.Fatalf("message = %+v, want the vendor's", bifrostErr.Error)
			}
		})
	}
}

// TestAzureSoraJobsCancelIsUnsupported pins that the jobs API, which documents no cancel,
// gets provider_cancel_unsupported from core with no upstream call.
func TestAzureSoraJobsCancelIsUnsupported(t *testing.T) {
	provider := newAzureDecisionProvider(t)
	bifrostErr := schemas.CheckVideoCancelSupported(provider, &schemas.BifrostRequest{
		RequestType:        schemas.VideoCancelRequest,
		VideoCancelRequest: &schemas.BifrostVideoCancelRequest{Provider: schemas.Azure, ID: "sora1.task_01jwcet0eje35tc5jy54yjax5q:azure"},
	})
	if bifrostErr == nil || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
		t.Fatalf("cancel = %+v, want provider_cancel_unsupported", bifrostErr)
	}
}

// TestAzureSoraJobsLeaveSora2OnVideos pins the selection: only first-generation sora goes to
// the jobs API; sora-2 and its ids stay on /openai/v1/videos.
func TestAzureSoraJobsLeaveSora2OnVideos(t *testing.T) {
	server, calls := azureSoraJobsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"video_68f0","object":"video","status":"queued","model":"sora-2","created_at":1}`)
	})
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	provider := newAzureDecisionProvider(t)
	request := newAzureSoraJobsRequest(nil)
	request.Model = "sora-2"
	if _, bifrostErr := provider.VideoGeneration(ctx, newAzureDecisionKey(server.URL), request); bifrostErr != nil {
		t.Fatalf("VideoGeneration sora-2: %+v", bifrostErr.Error)
	}
	if _, bifrostErr := provider.VideoRetrieve(ctx, newAzureDecisionKey(server.URL), &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Azure, ID: "video_68f0:azure"}); bifrostErr != nil {
		t.Fatalf("VideoRetrieve sora-2: %+v", bifrostErr.Error)
	}
	got := calls()
	if len(got) != 2 || got[0].path != "/openai/v1/videos" || got[1].path != "/openai/v1/videos/video_68f0" {
		t.Fatalf("calls = %+v, want sora-2 on /openai/v1/videos", got)
	}
}
