package huggingface_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/providers/huggingface"

	"github.com/capsohq/bifrost/core/schemas"
)

func TestHuggingface(t *testing.T) {
	t.Parallel()
	if os.Getenv("HUGGING_FACE_API_KEY") == "" {
		t.Skip("Skipping HuggingFace tests because HUGGING_FACE_API_KEY is not set")
	}

	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	testConfig := llmtests.ComprehensiveTestConfig{
		Provider:             schemas.HuggingFace,
		ChatModel:            "together/meta-llama/Llama-3.3-70B-Instruct",
		VisionModel:          "novita/meta-llama/Llama-4-Scout-17B-16E-Instruct",
		EmbeddingModel:       "sambanova/intfloat/e5-mistral-7b-instruct",
		TranscriptionModel:   "fal-ai/openai/whisper-large-v3",
		SpeechSynthesisModel: "fal-ai/hexgrad/Kokoro-82M",
		SpeechSynthesisFallbacks: []schemas.Fallback{
			{Provider: schemas.HuggingFace, Model: "fal-ai/ResembleAI/chatterbox"},
		},
		ReasoningModel:       "groq/openai/gpt-oss-120b",
		ImageGenerationModel: "fal-ai/fal-ai/flux/dev",
		ImageEditModel:       "fal-ai/fal-ai/flux-2/edit",
		Scenarios: llmtests.TestScenarios{
			TextCompletion:        false,
			TextCompletionStream:  false,
			SimpleChat:            true,
			CompletionStream:      true,
			MultiTurnConversation: true,
			ToolCalls:             true,
			ToolCallsStreaming:    true,
			MultipleToolCalls:     false,
			End2EndToolCalling:    true,
			AutomaticFunctionCall: true,
			ImageURL:              true,
			ImageBase64:           true,
			MultipleImages:        true,
			CompleteEnd2End:       true,
			Embedding:             false,
			Transcription:         false,
			TranscriptionStream:   false,
			SpeechSynthesis:       false,
			SpeechSynthesisStream: false,
			Reasoning:             true,
			ListModels:            true,
			BatchCreate:           false,
			BatchList:             false,
			BatchRetrieve:         false,
			BatchCancel:           false,
			BatchResults:          false,
			FileUpload:            false,
			FileList:              false,
			FileRetrieve:          false,
			FileDelete:            false,
			FileContent:           false,
			FileBatchInput:        false,
			ImageGeneration:       false,
			ImageGenerationStream: false,
			ImageEdit:             false,
			ImageEditStream:       false,
		},
	}

	t.Run("HuggingFaceTests", func(t *testing.T) {
		llmtests.RunAllComprehensiveTests(t, client, ctx, testConfig)
	})
}

func TestUnmarshalHuggingFaceEmbeddingResponsePreservesPrecision(t *testing.T) {
	const want = 0.12345678901234568

	resp, err := huggingface.UnmarshalHuggingFaceEmbeddingResponse([]byte(`[[0.12345678901234568]]`), "test-model")
	if err != nil {
		t.Fatalf("UnmarshalHuggingFaceEmbeddingResponse failed: %v", err)
	}

	if resp == nil || len(resp.Data) != 1 {
		t.Fatalf("expected single embedding response, got %#v", resp)
	}
	if len(resp.Data[0].Embedding.EmbeddingArray) != 1 {
		t.Fatalf("expected single embedding value, got %#v", resp.Data[0].Embedding.EmbeddingArray)
	}

	got := resp.Data[0].Embedding.EmbeddingArray[0]
	if got != want {
		t.Fatalf("expected %0.18f, got %0.18f", want, got)
	}

	if got == float64(float32(want)) {
		t.Fatalf("expected preserved precision, got float32-rounded value %0.18f", got)
	}
}

func TestUnmarshalHuggingFaceEmbeddingResponse1DPreservesPrecision(t *testing.T) {
	const want = 0.12345678901234568

	resp, err := huggingface.UnmarshalHuggingFaceEmbeddingResponse([]byte(`[0.12345678901234568]`), "test-model")
	if err != nil {
		t.Fatalf("UnmarshalHuggingFaceEmbeddingResponse failed: %v", err)
	}

	if resp == nil || len(resp.Data) != 1 {
		t.Fatalf("expected single embedding response, got %#v", resp)
	}
	if len(resp.Data[0].Embedding.EmbeddingArray) != 1 {
		t.Fatalf("expected single embedding value, got %#v", resp.Data[0].Embedding.EmbeddingArray)
	}

	got := resp.Data[0].Embedding.EmbeddingArray[0]
	if got != want {
		t.Fatalf("expected %0.18f, got %0.18f", want, got)
	}

	if got == float64(float32(want)) {
		t.Fatalf("expected preserved precision, got float32-rounded value %0.18f", got)
	}
}

// Hugging Face's chat request has no service_tier field, so a tier other than "default"
// cannot be delivered: it is refused with 400 caller_unsupported_parameter before any
// upstream call, non-streamed and streamed, instead of being left out (contract C4).
// "default" is the tier the vendor serves anyway and goes through.
func TestChatCompletion_RefusesAServiceTierHuggingFaceCannotTake(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["service_tier"]; has {
			t.Errorf("service_tier reached Hugging Face: %v", body)
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	provider := huggingface.NewHuggingFaceProvider(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
	}, bifrost.NewDefaultLogger(schemas.LogLevelError))
	key := schemas.Key{Value: schemas.SecretVar{Val: "test-token"}, Models: []string{"*"}}
	request := func(tier schemas.BifrostServiceTier) *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: schemas.HuggingFace,
			Model:    "groq/openai/gpt-oss-120b",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}},
			Params:   &schemas.ChatParameters{ServiceTier: &tier},
		}
	}
	newCtx := func() *schemas.BifrostContext {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		ctx.SetValue(schemas.BifrostContextKeyTracer, &schemas.NoOpTracer{})
		return ctx
	}
	send := map[string]func(schemas.BifrostServiceTier) *schemas.BifrostError{
		"chat": func(tier schemas.BifrostServiceTier) *schemas.BifrostError {
			_, bifrostErr := provider.ChatCompletion(newCtx(), key, request(tier))
			return bifrostErr
		},
		"chat stream": func(tier schemas.BifrostServiceTier) *schemas.BifrostError {
			postHook := func(_ *schemas.BifrostContext, result *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				return result, err
			}
			chunks, bifrostErr := provider.ChatCompletionStream(newCtx(), postHook, nil, key, request(tier))
			if bifrostErr != nil {
				return bifrostErr
			}
			for range chunks {
			}
			return nil
		},
	}
	for name, sendTier := range send {
		t.Run(name, func(t *testing.T) {
			before := calls
			refused := sendTier(schemas.BifrostServiceTierPriority)
			if refused == nil {
				t.Fatal("a tier Hugging Face cannot take must be refused")
			}
			if calls != before {
				t.Fatalf("%d upstream calls for a refused tier, want 0", calls-before)
			}
			if refused.StatusCode == nil || *refused.StatusCode != 400 ||
				refused.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter ||
				refused.Error == nil || refused.Error.Param != "service_tier" {
				t.Fatalf("refusal = %+v (error %+v), want 400 caller_unsupported_parameter with param service_tier", refused, refused.Error)
			}
			if bifrostErr := sendTier(schemas.BifrostServiceTierDefault); bifrostErr != nil {
				t.Fatalf("the default tier failed: %+v", bifrostErr.Error)
			}
			if calls != before+1 {
				t.Fatalf("%d upstream calls for the default tier, want 1", calls-before)
			}
		})
	}
}
