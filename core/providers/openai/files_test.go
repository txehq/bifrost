package openai

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertRequestsToJSONLRequiresCustomID(t *testing.T) {
	for _, customID := range []string{"", "   "} {
		_, err := ConvertRequestsToJSONL([]schemas.BatchRequestItem{{
			CustomID: customID,
			Method:   "POST",
			URL:      string(schemas.BatchEndpointChatCompletions),
			Body:     map[string]interface{}{"model": "gpt-4o-mini"},
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "batch request item 0: custom_id is required")
	}
}

func TestConvertRequestsToJSONLPreservesCustomID(t *testing.T) {
	data, err := ConvertRequestsToJSONL([]schemas.BatchRequestItem{{
		CustomID: "request-1",
		Method:   "POST",
		URL:      string(schemas.BatchEndpointEmbeddings),
		Body:     map[string]interface{}{"model": "text-embedding-3-small", "input": "hello"},
	}})
	require.NoError(t, err)

	line := strings.TrimSpace(string(data))
	var decoded schemas.BatchRequestItem
	require.NoError(t, sonic.UnmarshalString(line, &decoded))
	assert.Equal(t, "request-1", decoded.CustomID)
}

// A file upload's expiry reaches OpenAI in the files API's bracketed form fields, and the
// expiry the vendor sets comes back on the upload and retrieve responses (contract C4).
func TestFileUploadSendsExpiresAfterAndReturnsExpiresAt(t *testing.T) {
	vendor := newBatchVendor(t, "", nil)
	provider, ctx, keys := newBatchTestProvider(vendor)

	_, bifrostErr := provider.FileUpload(ctx, keys[0], &schemas.BifrostFileUploadRequest{
		Provider:     schemas.OpenAI,
		File:         []byte("{}\n"),
		Filename:     "in.jsonl",
		Purpose:      schemas.FilePurposeBatch,
		ExpiresAfter: &schemas.FileExpiresAfter{Anchor: "created_at", Seconds: 1209600},
	})
	require.Nil(t, bifrostErr)
	require.Len(t, vendor.uploadForms, 1)
	assert.Equal(t, map[string]string{"purpose": "batch", "expires_after[anchor]": "created_at", "expires_after[seconds]": "1209600"}, vendor.uploadForms[0])

	var file OpenAIFileResponse
	require.NoError(t, sonic.UnmarshalString(`{"id":"file-1","object":"file","bytes":3,"created_at":1711471533,"expires_at":1712681133,"filename":"in.jsonl","purpose":"batch"}`, &file))
	uploaded := file.ToBifrostFileUploadResponse(0, false, false, nil, nil)
	require.NotNil(t, uploaded.ExpiresAt)
	assert.EqualValues(t, 1712681133, *uploaded.ExpiresAt)
	retrieved := file.ToBifrostFileRetrieveResponse(schemas.OpenAI, 0, false, false, nil, nil)
	require.NotNil(t, retrieved.ExpiresAt)
	assert.EqualValues(t, 1712681133, *retrieved.ExpiresAt)

	// A file with no expiry reports none.
	require.NoError(t, sonic.UnmarshalString(`{"id":"file-2","object":"file","bytes":3,"created_at":1711471533,"filename":"in.jsonl","purpose":"batch"}`, &file))
	file.ExpiresAt = nil
	assert.Nil(t, file.ToBifrostFileUploadResponse(0, false, false, nil, nil).ExpiresAt)
}
