package bedrock

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func offlineEgressChatRequest() *schemas.BifrostChatRequest {
	return &schemas.BifrostChatRequest{
		Provider: schemas.Bedrock,
		Model:    testConverseStreamModel,
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")},
		}},
	}
}

func offlineEgressKey(endpoints *schemas.BedrockEndpoints) schemas.Key {
	return schemas.Key{BedrockKeyConfig: &schemas.BedrockKeyConfig{
		AccessKey: *schemas.NewSecretVar("AKIAIOSFODNN7EXAMPLE"),
		SecretKey: *schemas.NewSecretVar("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"),
		Region:    schemas.NewSecretVar("us-east-1"),
		Endpoints: endpoints,
	}}
}

func bifrostErrorText(err *schemas.BifrostError) string {
	if err == nil || err.Error == nil {
		return fmt.Sprint(err)
	}
	text := err.Error.Message
	if err.Error.Error != nil {
		text += " | " + err.Error.Error.Error()
	}
	return text
}

// TestBedrockOfflineRunRefusesTheRegionHost reproduces TXE-7083: a Bedrock provider pointed
// at a fixture with network_config.base_url still dials bedrock-runtime.<region>.amazonaws.com,
// because Bedrock builds its host from the region. In an offline run the guard refuses the
// connection before it is attempted and the error names the host, so nothing reaches AWS. The
// name still resolves (DNS is allowed), so the test skips where DNS is unavailable.
func TestBedrockOfflineRunRefusesTheRegionHost(t *testing.T) {
	if _, err := net.DefaultResolver.LookupHost(context.Background(), "bedrock-runtime.us-east-1.amazonaws.com"); err != nil {
		t.Skipf("no DNS for the AWS host here: %v", err)
	}
	defer network.SetOfflineEgressGuard(true)()
	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL:                        "http://127.0.0.1:1", // ignored by Bedrock: the incident
		DefaultRequestTimeoutInSeconds: 5,
	}}
	config.CheckAndSetDefaults()
	provider, err := NewBedrockProvider(config, noopLogger{})
	require.NoError(t, err)

	_, bifrostErr := provider.ChatCompletion(testBedrockCtx(), offlineEgressKey(nil), offlineEgressChatRequest())
	require.NotNil(t, bifrostErr, "an offline run must not reach AWS")
	text := bifrostErrorText(bifrostErr)
	assert.Contains(t, text, network.OfflineEgressMarker)
	assert.Contains(t, text, "bedrock-runtime.us-east-1.amazonaws.com")
}

// TestBedrockOfflineRunReachesAFixtureThroughTheEndpointOverride is the documented way for a
// fixture to cover Bedrock: bedrock_key_config.endpoints names a loopback TLS listener, and
// network_config.insecure_skip_verify accepts its self-signed certificate. The guard allows it.
func TestBedrockOfflineRunReachesAFixtureThroughTheEndpointOverride(t *testing.T) {
	defer network.SetOfflineEgressGuard(true)()
	var path string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello from the fixture"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":4,"totalTokens":7}}`)
	}))
	defer server.Close()
	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		InsecureSkipVerify:             true,
		DefaultRequestTimeoutInSeconds: 5,
	}}
	config.CheckAndSetDefaults()
	provider, err := NewBedrockProvider(config, noopLogger{})
	require.NoError(t, err)

	host := strings.TrimPrefix(server.URL, "https://")
	response, bifrostErr := provider.ChatCompletion(testBedrockCtx(),
		offlineEgressKey(&schemas.BedrockEndpoints{Runtime: schemas.NewSecretVar(host)}), offlineEgressChatRequest())
	require.Nil(t, bifrostErr, bifrostErrorText(bifrostErr))
	require.NotEmpty(t, response.Choices)
	assert.Equal(t, "hello from the fixture", *response.Choices[0].Message.Content.ContentStr)
	assert.Equal(t, "/model/"+testConverseStreamModel+"/converse", path)
}
