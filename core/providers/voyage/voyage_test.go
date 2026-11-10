package voyage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/capsohq/bifrost/core/internal/llmtests"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/providers/voyage"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func request(model string) *schemas.BifrostEmbeddingRequest {
	return &schemas.BifrostEmbeddingRequest{Provider: schemas.Voyage, Model: model, Input: []schemas.EmbeddingInputItem{
		{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("hello")}}},
		{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("world")}}},
	}}
}

// The response shape follows https://docs.voyageai.com/reference/embeddings-api.
// Vectors are shortened synthetic fixture values, not recorded paid requests.
func TestVoyageBatch(t *testing.T) {
	for _, model := range []string{"voyage-4", "voyage-4-large", "voyage-4-lite"} {
		for _, status := range []int{200, 400, 401, 429, 500, 503, 0} {
			t.Run(fmt.Sprintf("%s/%d", model, status), func(t *testing.T) {
				var calls atomic.Int32
				wireCh := make(chan map[string]interface{}, 1)
				server := &httptest.Server{Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer test-key" {
						w.WriteHeader(404)
						return
					}
					var wire map[string]interface{}
					_ = json.NewDecoder(r.Body).Decode(&wire)
					wireCh <- wire
					w.Header().Set("X-Voyage-Fixture", "present")
					if status == 0 {
						_, _ = w.Write([]byte(`{"object":"list","data":[],"usage":{"total_tokens":7}}`))
						return
					}
					w.WriteHeader(status)
					if status != 200 {
						_, _ = w.Write([]byte(`{"detail":"fixture error"}`))
						return
					}
					_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]},{"object":"embedding","index":1,"embedding":[0.3,0.4]}],"usage":{"total_tokens":7}}`))
				})}}
				var err error
				for attempt := 0; attempt < 100; attempt++ {
					server.Listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 20000+rand.IntN(40001)))
					if err == nil {
						break
					}
				}
				require.NoError(t, err)
				server.Start()
				defer server.Close()
				p, err := voyage.NewVoyageProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, AllowPrivateNetwork: true}, SendBackRawRequest: true, SendBackRawResponse: true}, bifrost.NewNoOpLogger())
				require.NoError(t, err)
				req := request(model)
				req.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(256), EncodingFormat: schemas.Ptr("float"), TaskType: schemas.Ptr("document"), AutoTruncate: schemas.Ptr(false), ExtraParams: map[string]interface{}{"vendor_option": true}}
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
				resp, bfErr := p.Embedding(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, req)
				require.EqualValues(t, 1, calls.Load())
				wire := <-wireCh
				require.Equal(t, []interface{}{"hello", "world"}, wire["input"])
				require.Equal(t, float64(256), wire["output_dimension"])
				require.Equal(t, "float", wire["output_dtype"])
				require.Equal(t, "document", wire["input_type"])
				require.Equal(t, false, wire["truncation"])
				require.Equal(t, true, wire["vendor_option"])
				require.NotContains(t, wire, "dimensions")
				if status == 200 {
					require.Nil(t, bfErr)
					require.Len(t, resp.Data, 2)
					require.Equal(t, model, resp.Model)
					require.Equal(t, 7, resp.Usage.PromptTokens)
					require.Equal(t, 7, resp.Usage.TotalTokens)
					require.NotNil(t, resp.ExtraFields.RawResponse)
				} else if status == 0 {
					require.Nil(t, resp)
					require.NotNil(t, bfErr)
					require.Equal(t, 502, *bfErr.StatusCode)
					require.EqualValues(t, "provider_empty_result", bfErr.ExtraFields.ErrorType)
				} else {
					require.Nil(t, resp)
					require.NotNil(t, bfErr)
					require.Equal(t, status, *bfErr.StatusCode)
					require.Equal(t, "fixture error", bfErr.Error.Message)
					wantType := map[int]schemas.ErrorType{400: schemas.ErrorTypeCallerInvalidRequest, 401: schemas.ErrorTypeProviderAuthFailed, 429: schemas.ErrorTypeProviderRateLimited, 500: schemas.ErrorTypeProviderServerError, 503: schemas.ErrorTypeProviderOverloaded}[status]
					require.Equal(t, wantType, bfErr.ExtraFields.ErrorType)
					require.NotNil(t, bfErr.ExtraFields.RawResponse)
				}
			})
		}
	}
}

func TestVoyageParameters(t *testing.T) {
	for _, encoding := range []string{"float", "int8", "uint8", "binary", "ubinary", "base64"} {
		req := request("voyage-4")
		req.Params = &schemas.EmbeddingParameters{EncodingFormat: &encoding}
		got, err := voyage.ToVoyageEmbeddingRequest(req)
		require.Nil(t, err)
		if encoding == "base64" {
			require.Equal(t, encoding, *got.EncodingFormat)
			require.Nil(t, got.OutputDType)
		} else {
			require.Equal(t, encoding, *got.OutputDType)
			require.Nil(t, got.EncodingFormat)
		}
	}
	cases := []struct {
		name   string
		mutate func(*schemas.BifrostEmbeddingRequest)
		param  string
	}{
		{"title", func(r *schemas.BifrostEmbeddingRequest) { r.Params.Title = schemas.Ptr("title") }, "title"},
		{"instructions", func(r *schemas.BifrostEmbeddingRequest) { r.Params.Instructions = schemas.Ptr("instructions") }, "instructions"},
		{"sparse", func(r *schemas.BifrostEmbeddingRequest) {
			r.Params.SparseEmbedding = map[string]interface{}{"enabled": true}
		}, "sparse_embedding"},
		{"dimension", func(r *schemas.BifrostEmbeddingRequest) { r.Params.Dimensions = schemas.Ptr(13) }, "dimensions"},
		{"encoding", func(r *schemas.BifrostEmbeddingRequest) { r.Params.EncodingFormat = schemas.Ptr("float16") }, "encoding_format"},
		{"task", func(r *schemas.BifrostEmbeddingRequest) { r.Params.TaskType = schemas.Ptr("classification") }, "task_type"},
		{"itemparams", func(r *schemas.BifrostEmbeddingRequest) { r.Input[1].Params = &schemas.EmbeddingParameters{} }, "input"},
		{"media", func(r *schemas.BifrostEmbeddingRequest) {
			r.Input[1].Content[0].Type = schemas.EmbeddingContentPartTypeImage
		}, "input"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := request("voyage-4")
			r.Params = &schemas.EmbeddingParameters{}
			c.mutate(r)
			out, err := voyage.ToVoyageEmbeddingRequest(r)
			require.Nil(t, out)
			require.NotNil(t, err)
			require.Equal(t, 400, *err.StatusCode)
			require.Equal(t, c.param, err.Error.Param)
			require.EqualValues(t, "caller_unsupported_parameter", err.ExtraFields.ErrorType)
		})
	}
}

func TestVoyageMissingUsage(t *testing.T) {
	r := (&voyage.VoyageEmbeddingResponse{}).ToBifrostEmbeddingResponse("voyage-4")
	require.Nil(t, r.Usage)
}

func BenchmarkVoyageEmbeddingConversion(b *testing.B) {
	r := request("voyage-4")
	r.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(256), EncodingFormat: schemas.Ptr("float"), TaskType: schemas.Ptr("document")}
	b.ReportAllocs()
	for b.Loop() {
		v, err := voyage.ToVoyageEmbeddingRequest(r)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := providerUtils.MarshalProviderRequest(v); err != nil {
			b.Fatal(err)
		}
	}
}

func TestVoyage(t *testing.T) {
	if strings.TrimSpace(os.Getenv("VOYAGE_API_KEY")) == "" {
		t.Skip("VOYAGE_API_KEY is not set")
	}
	client, ctx, cancel, err := llmtests.SetupTest()
	require.NoError(t, err)
	defer cancel()
	defer client.Shutdown()
	llmtests.RunAllComprehensiveTests(t, client, ctx, llmtests.ComprehensiveTestConfig{Provider: schemas.Voyage, EmbeddingModel: "voyage-4-lite", Scenarios: llmtests.TestScenarios{Embedding: true}})
}

// Both HTTP embedding routes use OpenAIEmbeddingRequest at ingress.
func TestVoyageOpenAIIngress(t *testing.T) {
	var ingress openai.OpenAIEmbeddingRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"voyage/voyage-4","input":["hello","world"],"dimensions":256,"task_type":"query","encoding_format":"float"}`), &ingress))
	r := ingress.ToBifrostEmbeddingRequest(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline))
	require.Equal(t, schemas.Voyage, r.Provider)
	wire, err := voyage.ToVoyageEmbeddingRequest(r)
	require.Nil(t, err)
	require.Equal(t, []string{"hello", "world"}, wire.Input)
	require.Equal(t, "query", *wire.InputType)
	require.Equal(t, 256, *wire.OutputDimension)
}

func TestVoyageRefusesBeforeHTTP(t *testing.T) {
	p, err := voyage.NewVoyageProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "http://voyage.invalid"}}, bifrost.NewNoOpLogger())
	require.NoError(t, err)
	r := request("voyage-4")
	r.Params = &schemas.EmbeddingParameters{Title: schemas.Ptr("unsupported")}
	resp, bfErr := p.Embedding(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), schemas.Key{}, r)
	require.Nil(t, resp)
	require.NotNil(t, bfErr)
	require.Equal(t, 400, *bfErr.StatusCode)
	require.Equal(t, "title", bfErr.Error.Param)
}

// This is the published response example, with the display-only "..." entries
// removed from both vectors: https://docs.voyageai.com/reference/embeddings-api.md.
func TestVoyageDocumentedResponse(t *testing.T) {
	const sample = `{"object":"list","data":[{"object":"embedding","embedding":[-0.016709786,0.026996311,-0.027496673,-0.012125067],"index":0},{"object":"embedding","embedding":[0.003613521,0.026428301,-0.009491397,-0.028471239],"index":1}],"model":"voyage-4-large","usage":{"total_tokens":8}}`
	var vendor voyage.VoyageEmbeddingResponse
	require.NoError(t, json.Unmarshal([]byte(sample), &vendor))
	got := vendor.ToBifrostEmbeddingResponse("voyage-4-large")
	require.Len(t, got.Data, 2)
	require.Equal(t, 8, got.Usage.PromptTokens)
	require.Equal(t, 8, got.Usage.TotalTokens)
	require.Equal(t, []float64{-0.016709786, 0.026996311, -0.027496673, -0.012125067}, got.Data[0].Embedding.EmbeddingArray)
}

func TestVoyageExtraParamsCannotReplaceAuthorizedFields(t *testing.T) {
	for _, field := range []string{"model", "input", "output_dimension", "output_dtype", "encoding_format", "input_type", "truncation"} {
		t.Run(field, func(t *testing.T) {
			r := request("voyage-4-lite")
			r.Params = &schemas.EmbeddingParameters{Dimensions: schemas.Ptr(256), EncodingFormat: schemas.Ptr("float"), TaskType: schemas.Ptr("query"), AutoTruncate: schemas.Ptr(false), ExtraParams: map[string]interface{}{field: "replacement"}}
			got, err := voyage.ToVoyageEmbeddingRequest(r)
			require.Nil(t, got)
			require.NotNil(t, err)
			require.Equal(t, 400, *err.StatusCode)
			require.Equal(t, "extra_params."+field, err.Error.Param)
		})
	}
	for _, field := range []string{"model", "input"} {
		var ingress openai.OpenAIEmbeddingRequest
		payload := fmt.Sprintf(`{"model":"voyage/voyage-4-lite","input":"hello","extra_params":{%q:"voyage-4-large"}}`, field)
		require.NoError(t, json.Unmarshal([]byte(payload), &ingress))
		// The HTTP router promotes the explicit extra_params object before
		// invoking the OpenAI converter; exercise that normalized ingress.
		ingress.SetExtraParams(map[string]interface{}{field: "voyage-4-large"})
		r := ingress.ToBifrostEmbeddingRequest(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline))
		_, err := voyage.ToVoyageEmbeddingRequest(r)
		require.NotNil(t, err)
	}
}
