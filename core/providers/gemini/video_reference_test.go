package gemini

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// A video's reference image reaches the model in each form it comes: a data URL inline, a
// Cloud Storage object as gcsUri, and a web link as fileData, each with its MIME type.
func TestVideoReferenceImageForms(t *testing.T) {
	convert := func(ref string) *VideoImageData {
		t.Helper()
		req, err := ToGeminiVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
			Model: "veo-3.1-generate-001",
			Input: &schemas.VideoGenerationInput{Prompt: "the cat wakes up", InputReference: schemas.Ptr(ref)},
		})
		require.NoError(t, err)
		require.Len(t, req.Instances, 1)
		require.NotNil(t, req.Instances[0].Image)
		return req.Instances[0].Image
	}

	inline := convert("data:image/jpeg;base64,/9j/4AAQ")
	require.Equal(t, "/9j/4AAQ", *inline.BytesBase64Encoded)
	require.Equal(t, "image/jpeg", *inline.MimeType)
	require.Nil(t, inline.FileData)
	require.Nil(t, inline.GCSURI)

	gcs := convert("gs://refs/cat.png")
	require.Equal(t, "gs://refs/cat.png", *gcs.GCSURI)
	require.Nil(t, gcs.BytesBase64Encoded)
	require.Nil(t, gcs.FileData)

	link := convert("https://example.com/cat.png")
	require.NotNil(t, link.FileData)
	require.Equal(t, "https://example.com/cat.png", link.FileData.FileURI)
	require.Equal(t, *link.MimeType, link.FileData.MIMEType)
	require.Nil(t, link.BytesBase64Encoded)

	_, err := ToGeminiVideoGenerationRequest(&schemas.BifrostVideoGenerationRequest{
		Model: "veo-3.1-generate-001",
		Input: &schemas.VideoGenerationInput{Prompt: "x", InputReference: schemas.Ptr("ftp://example.com/cat.png")},
	})
	require.Error(t, err)
}
