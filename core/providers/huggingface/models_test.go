package huggingface

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// TestModelHubFollowsTheConfiguredBaseURL (fork, TXE-7117): with the default base URL the hub
// API is asked at huggingface.co, as before. With a base URL the operator set, the model
// lookup and the model list go there too, so a provider pointed at a gateway or a fixture
// reaches no HuggingFace host by a route nobody configured.
func TestModelHubFollowsTheConfiguredBaseURL(t *testing.T) {
	newProvider := func(baseURL string) *HuggingFaceProvider {
		return NewHuggingFaceProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: baseURL, DefaultRequestTimeoutInSeconds: 5}}, nil)
	}
	for _, baseURL := range []string{"", defaultInferenceBaseURL, defaultInferenceBaseURL + "/"} {
		provider := newProvider(baseURL)
		assert.Equal(t, "https://huggingface.co/api/models/org/model?expand%5B%5D=inferenceProviderMapping", provider.buildModelInferenceProviderURL("org/model"), "base %q", baseURL)
		assert.Contains(t, provider.buildModelHubURL(&schemas.BifrostListModelsRequest{}, falAI), "https://huggingface.co/api/models?", "base %q", baseURL)
	}
	// A provider built without the constructor keeps the old host.
	assert.Contains(t, (&HuggingFaceProvider{}).buildModelInferenceProviderURL("org/model"), "https://huggingface.co/api/models/org/model?")

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"_id":"1","id":"org/model","pipeline_tag":"text-to-speech","inferenceProviderMapping":{"fal-ai":{"status":"live","providerId":"fal-ai/org-model","task":"text-to-speech"}}}`))
	}))
	defer server.Close()
	provider := newProvider(server.URL + "/pp/huggingface/")
	assert.Equal(t, server.URL+"/pp/huggingface/api/models/org/model?expand%5B%5D=inferenceProviderMapping", provider.buildModelInferenceProviderURL("org/model"))
	assert.Contains(t, provider.buildModelHubURL(&schemas.BifrostListModelsRequest{}, falAI), server.URL+"/pp/huggingface/api/models?")

	modelID, bifrostErr := provider.getValidatedProviderModelID(context.Background(), falAI, "org/model", "text-to-speech", schemas.SpeechRequest)
	require.Nil(t, bifrostErr)
	assert.Equal(t, "fal-ai/org-model", modelID)
	assert.Equal(t, []string{"/pp/huggingface/api/models/org/model"}, paths, "the lookup must go to the configured base URL")
}

// BenchmarkModelHubURL is the cost of building the two hub URLs (TXE-7117): the model lookup
// before an uncached inference request and the model list. "constant" is the expression the
// builder used before, with the hub host as a constant; "provider" is the builder now, which
// reads the host the constructor chose.
func BenchmarkModelHubURL(b *testing.B) {
	provider := NewHuggingFaceProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 5}}, nil)
	request := &schemas.BifrostListModelsRequest{}
	modelName := strings.Repeat("org/model", 1) // a variable, as a request's model name is
	b.Run("lookup/constant", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			values := url.Values{}
			values.Set("expand[]", "pipeline_tag")
			values.Set("expand[]", "inferenceProviderMapping")
			_ = fmt.Sprintf("%s/api/models/%s?%s", modelHubBaseURL, modelName, values.Encode())
		}
	})
	b.Run("lookup/provider", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = provider.buildModelInferenceProviderURL(modelName)
		}
	})
	b.Run("list/provider", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = provider.buildModelHubURL(request, falAI)
		}
	})
}

// TestNoHuggingFaceRequestGoesToAFixedHost keeps TXE-7117 true of the whole package: the
// base_url table (core/providers/base_url_support.json) says HuggingFace honours base_url, and
// that holds only while no request is built from a host constant. The hub constant may be
// read where the provider chooses its hub base and nowhere else; a new https:// host constant
// in the package fails here until it is routed through configuration or recorded in the table.
func TestNoHuggingFaceRequestGoesToAFixedHost(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	hostConstant := regexp.MustCompile(`"https?://[^"\s]+"`)
	allowedHosts := map[string]bool{`"https://router.huggingface.co"`: true, `"https://huggingface.co"`: true}
	hubUses := map[string]int{}
	hostLiterals := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, line := range strings.Split(string(source), "\n") {
			// A comment is a whole line or follows the code after a space; "//" inside a
			// URL literal is neither.
			code := line
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			} else if at := strings.Index(line, " // "); at >= 0 {
				code = line[:at]
			}
			for _, literal := range hostConstant.FindAllString(code, -1) {
				hostLiterals++
				assert.True(t, allowedHosts[literal], "%s builds a request from the fixed host %s; take it from configuration (as hubBaseURL does) or record it in base_url_support.json", file, literal)
			}
			if strings.Contains(code, "modelHubBaseURL") && !strings.Contains(code, "modelHubBaseURL         =") {
				hubUses[file]++
			}
		}
	}
	assert.Equal(t, 2, hostLiterals, "the scan must see the router and hub constants and nothing else; it is not reading the sources if it sees fewer")
	assert.Equal(t, map[string]int{"huggingface.go": 1, "utils.go": 1}, hubUses, "modelHubBaseURL is read once by the constructor and once by hubBase; every request asks provider.hubBase()")
}

// Regression tests for https://github.com/capsohq/bifrost/issues/4215.
//
// Allowlist entries selected from a previous ListModels response carry an
// inference-provider segment (e.g. "featherless-ai/org/model"). The backfill
// re-wrap used to blindly prepend the current inference provider, producing
// IDs like "huggingface/cohere/featherless-ai/org/model" that duplicate the
// provider segment and fail request routing.
func TestToBifrostListModelsResponse_AllowlistWithInferenceProviderSegment(t *testing.T) {
	t.Parallel()

	allowlist := schemas.WhiteList{"featherless-ai/deepseek-ai/DeepSeek-V4-Pro"}

	t.Run("matching provider emits single correctly-prefixed entry", func(t *testing.T) {
		t.Parallel()
		response := &HuggingFaceListModelsResponse{
			Models: []HuggingFaceModel{
				{
					ID:          "abc123",
					ModelID:     "deepseek-ai/DeepSeek-V4-Pro",
					PipelineTag: "conversational",
				},
			},
		}

		result := response.ToBifrostListModelsResponse(schemas.HuggingFace, featherlessAI, allowlist, nil, nil, false)
		require.NotNil(t, result)
		require.Len(t, result.Data, 1)
		assert.Equal(t, "huggingface/featherless-ai/deepseek-ai/DeepSeek-V4-Pro", result.Data[0].ID)
	})

	t.Run("other providers do not duplicate the entry", func(t *testing.T) {
		t.Parallel()
		response := &HuggingFaceListModelsResponse{
			Models: []HuggingFaceModel{
				{
					ID:          "def456",
					ModelID:     "CohereLabs/aya-vision-32b",
					PipelineTag: "conversational",
				},
			},
		}

		result := response.ToBifrostListModelsResponse(schemas.HuggingFace, cohere, allowlist, nil, nil, false)
		require.NotNil(t, result)
		assert.Empty(t, result.Data, "an allowlist entry pinned to featherless-ai must not be backfilled under cohere")
	})
}

func TestToBifrostListModelsResponse_AllowlistWithBasetenProviderSegment(t *testing.T) {
	t.Parallel()

	provider := baseten
	assert.Contains(t, INFERENCE_PROVIDERS, provider)

	response := &HuggingFaceListModelsResponse{Models: nil}
	allowlist := schemas.WhiteList{"baseten/zai-org/GLM-5.3-Flash"}

	result := response.ToBifrostListModelsResponse(schemas.HuggingFace, provider, allowlist, nil, nil, false)
	require.NotNil(t, result)
	require.Len(t, result.Data, 1)
	assert.Equal(t, "huggingface/baseten/zai-org/GLM-5.3-Flash", result.Data[0].ID)
}

func TestListModelsByKey_DiscoversBasetenModels(t *testing.T) {
	t.Parallel()

	var requested atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/models", r.URL.Path)
		if r.URL.Query().Get("inference_provider") == string(baseten) {
			requested.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`[{"_id":"glm-5.3-flash","modelId":"zai-org/GLM-5.3-Flash","pipeline_tag":"conversational"}]`))
			assert.NoError(t, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`[]`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	provider := &HuggingFaceProvider{
		client: &fasthttp.Client{
			Dial: func(string) (net.Conn, error) {
				return net.Dial("tcp", server.Listener.Addr().String())
			},
			TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Test server certificate.
		},
	}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	key := schemas.Key{Value: *schemas.NewSecretVar(""), Models: schemas.WhiteList{"*"}}

	response, bifrostErr := provider.listModelsByKey(ctx, key, &schemas.BifrostListModelsRequest{})

	require.Nil(t, bifrostErr)
	require.True(t, requested.Load(), "model discovery must query the Baseten inference provider")
	require.NotNil(t, response)
	require.Len(t, response.Data, 1)
	assert.Equal(t, "huggingface/baseten/zai-org/GLM-5.3-Flash", response.Data[0].ID)
}

// Allowlist entries prefixed with the "auto" policy must not be re-prefixed
// with an inference provider: "auto" is owned by no provider pass (the listing
// loop iterates INFERENCE_PROVIDERS, which excludes it). To keep the entry from
// being dropped entirely, it is emitted exactly once during the canonical first
// pass and skipped in every other pass, so it surfaces in the listing without
// being duplicated once per provider. The "auto/" prefix is preserved, so it
// stays routable: splitIntoModelProvider recognizes "auto" as a valid policy.
func TestToBifrostListModelsResponse_AllowlistWithAutoPolicySegment(t *testing.T) {
	t.Parallel()

	allowlist := schemas.WhiteList{"auto/deepseek-ai/DeepSeek-V4-Pro"}

	t.Run("canonical first pass emits the auto-policy entry exactly once", func(t *testing.T) {
		t.Parallel()
		response := &HuggingFaceListModelsResponse{Models: nil}
		result := response.ToBifrostListModelsResponse(schemas.HuggingFace, INFERENCE_PROVIDERS[0], allowlist, nil, nil, false)
		require.NotNil(t, result)
		require.Len(t, result.Data, 1)
		assert.Equal(t, "huggingface/auto/deepseek-ai/DeepSeek-V4-Pro", result.Data[0].ID,
			"the auto-policy entry keeps its prefix and is not re-wrapped with an inference provider")
	})

	t.Run("non-canonical passes do not duplicate the auto-policy entry", func(t *testing.T) {
		t.Parallel()
		require.Greater(t, len(INFERENCE_PROVIDERS), 1)
		nonCanonicalProvider := INFERENCE_PROVIDERS[1]

		response := &HuggingFaceListModelsResponse{Models: nil}
		result := response.ToBifrostListModelsResponse(schemas.HuggingFace, nonCanonicalProvider, allowlist, nil, nil, false)
		require.NotNil(t, result)
		assert.Empty(t, result.Data, "a non-canonical pass must not re-emit the auto-policy entry")
	})
}

// Entries without an inference-provider segment keep the existing backfill
// behavior: the current inference provider is prepended.
func TestToBifrostListModelsResponse_BackfillWithoutInferenceProviderSegment(t *testing.T) {
	t.Parallel()

	response := &HuggingFaceListModelsResponse{Models: nil}
	allowlist := schemas.WhiteList{"deepseek-ai/DeepSeek-V4-Pro"}

	result := response.ToBifrostListModelsResponse(schemas.HuggingFace, featherlessAI, allowlist, nil, nil, false)
	require.NotNil(t, result)
	require.Len(t, result.Data, 1)
	assert.Equal(t, "huggingface/featherless-ai/deepseek-ai/DeepSeek-V4-Pro", result.Data[0].ID)
	// No matching entry in response.Models, so there is nothing to enrich from.
	assert.Nil(t, result.Data[0].HuggingFaceID)
	assert.Empty(t, result.Data[0].SupportedMethods)
}

// A backfilled entry whose model is actually present in response.Models (e.g.
// its provider-prefixed allowlist entry didn't string-match model.ModelID
// during the main filter pass) should still surface HuggingFaceID and
// SupportedMethods, not just ID/Name, since the data is available right there
// in the same response.
func TestToBifrostListModelsResponse_BackfillEnrichesHuggingFaceIDAndSupportedMethods(t *testing.T) {
	t.Parallel()

	allowlist := schemas.WhiteList{"featherless-ai/deepseek-ai/DeepSeek-V4-Pro"}
	response := &HuggingFaceListModelsResponse{
		Models: []HuggingFaceModel{
			{
				ID:          "abc123",
				ModelID:     "deepseek-ai/DeepSeek-V4-Pro",
				PipelineTag: "conversational",
			},
		},
	}

	result := response.ToBifrostListModelsResponse(schemas.HuggingFace, featherlessAI, allowlist, nil, nil, false)
	require.NotNil(t, result)
	require.Len(t, result.Data, 1)

	got := result.Data[0]
	assert.Equal(t, "huggingface/featherless-ai/deepseek-ai/DeepSeek-V4-Pro", got.ID)
	require.NotNil(t, got.HuggingFaceID)
	assert.Equal(t, "abc123", *got.HuggingFaceID)
	assert.ElementsMatch(t, []string{
		string(schemas.ChatCompletionRequest),
		string(schemas.ChatCompletionStreamRequest),
		string(schemas.ResponsesRequest),
		string(schemas.ResponsesStreamRequest),
	}, got.SupportedMethods)
}

// If the backfilled model is present in response.Models but has no
// recognizable pipeline tag/tags, HuggingFaceID is still recovered but
// SupportedMethods stays unset rather than being forced to an empty slice.
func TestToBifrostListModelsResponse_BackfillEnrichesHuggingFaceIDOnlyWhenMethodsUnknown(t *testing.T) {
	t.Parallel()

	allowlist := schemas.WhiteList{"deepseek-ai/DeepSeek-V4-Pro"}
	response := &HuggingFaceListModelsResponse{
		Models: []HuggingFaceModel{
			{
				ID:          "abc123",
				ModelID:     "deepseek-ai/DeepSeek-V4-Pro",
				PipelineTag: "some-unrecognized-pipeline",
			},
		},
	}

	result := response.ToBifrostListModelsResponse(schemas.HuggingFace, featherlessAI, allowlist, nil, nil, false)
	require.NotNil(t, result)
	require.Len(t, result.Data, 1)

	got := result.Data[0]
	require.NotNil(t, got.HuggingFaceID)
	assert.Equal(t, "abc123", *got.HuggingFaceID)
	assert.Nil(t, got.SupportedMethods)
}
