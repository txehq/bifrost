package streaming

import (
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// imageChunk builds one accumulated image stream chunk the way processImageStreamingResponse
// does: ImageIndex from the event's image index, and FinishReason set on the stream's last event.
func imageChunk(eventType schemas.ImageEventType, imageIndex int, url, b64 string, final bool) *ImageStreamChunk {
	chunk := &ImageStreamChunk{
		ImageIndex: imageIndex,
		Delta:      &schemas.BifrostImageGenerationStreamResponse{Type: eventType, Index: imageIndex, URL: url, B64JSON: b64},
	}
	if final {
		chunk.FinishReason = bifrost.Ptr("completed")
	}
	return chunk
}

// previewChunk is a progressive preview of an image still being generated, as OpenAI sends.
func previewChunk(imageIndex, partialImageIndex int, b64 string) *ImageStreamChunk {
	chunk := imageChunk(schemas.ImageGenerationEventTypePartial, imageIndex, "", b64, false)
	chunk.Delta.PartialImageIndex = bifrost.Ptr(partialImageIndex)
	return chunk
}

// A stream of one image keeps exactly the completed image: its previews are not images.
func TestBuildCompleteImage_SingleImageKeepsOnlyTheCompletedImage(t *testing.T) {
	a := &Accumulator{}
	got := a.buildCompleteImageFromImageStreamChunks([]*ImageStreamChunk{
		previewChunk(0, 0, "preview-0"),
		previewChunk(0, 1, "preview-1"),
		previewChunk(0, 2, "preview-2"),
		imageChunk(schemas.ImageGenerationEventTypeCompleted, 0, "", "final", true),
	})
	require.NotNil(t, got)
	assert.Equal(t, []schemas.ImageData{{B64JSON: "final", Index: 0}}, got.Data)
}

// BytePlus Seedream sends every finished image as its own event and ends on a completed event
// that repeats only the last one. The accumulated response must keep every image, once each.
func TestBuildCompleteImage_KeepsEveryImageOfAMultiImageStream(t *testing.T) {
	a := &Accumulator{}
	for name, chunks := range map[string][]*ImageStreamChunk{
		"urls": {
			imageChunk(schemas.ImageGenerationEventTypePartial, 0, "https://cdn.example/0.jpeg", "", false),
			imageChunk(schemas.ImageGenerationEventTypePartial, 2, "https://cdn.example/2.jpeg", "", false),
			imageChunk(schemas.ImageGenerationEventTypeCompleted, 2, "https://cdn.example/2.jpeg", "", true),
		},
		"edit events": {
			imageChunk(schemas.ImageEditEventTypePartial, 0, "https://cdn.example/0.jpeg", "", false),
			imageChunk(schemas.ImageEditEventTypePartial, 2, "https://cdn.example/2.jpeg", "", false),
			imageChunk(schemas.ImageEditEventTypeCompleted, 2, "https://cdn.example/2.jpeg", "", true),
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := a.buildCompleteImageFromImageStreamChunks(chunks)
			require.NotNil(t, got)
			assert.Equal(t, []schemas.ImageData{
				{URL: "https://cdn.example/0.jpeg", Index: 0},
				{URL: "https://cdn.example/2.jpeg", Index: 2},
			}, got.Data)
		})
	}

	got := a.buildCompleteImageFromImageStreamChunks([]*ImageStreamChunk{
		imageChunk(schemas.ImageGenerationEventTypePartial, 1, "", "image-1", false),
		imageChunk(schemas.ImageGenerationEventTypePartial, 0, "", "image-0", false),
		imageChunk(schemas.ImageGenerationEventTypeCompleted, 0, "", "image-0", true),
	})
	require.NotNil(t, got)
	assert.Equal(t, []schemas.ImageData{{B64JSON: "image-0", Index: 0}, {B64JSON: "image-1", Index: 1}}, got.Data, "inline images, sorted by image index")
}

// fal-ai resends every image on each progress event, so the last event of an index is its image.
func TestBuildCompleteImage_LastWholeImagePerIndexWins(t *testing.T) {
	a := &Accumulator{}
	got := a.buildCompleteImageFromImageStreamChunks([]*ImageStreamChunk{
		imageChunk(schemas.ImageGenerationEventTypePartial, 0, "", "0-early", false),
		imageChunk(schemas.ImageGenerationEventTypePartial, 1, "", "1-early", false),
		imageChunk(schemas.ImageGenerationEventTypePartial, 0, "", "0-late", false),
		imageChunk(schemas.ImageGenerationEventTypePartial, 1, "", "1-late", false),
		imageChunk(schemas.ImageGenerationEventTypeCompleted, 1, "", "1-late", true),
	})
	require.NotNil(t, got)
	assert.Equal(t, []schemas.ImageData{{B64JSON: "0-late", Index: 0}, {B64JSON: "1-late", Index: 1}}, got.Data)
}

// A completed event is the image of its index: neither a preview nor a later whole-image event
// of the same index replaces it, and a preview of an index that never completed is not an image.
func TestBuildCompleteImage_CompletedEventsAndPreviewsAtOtherIndexes(t *testing.T) {
	a := &Accumulator{}
	got := a.buildCompleteImageFromImageStreamChunks([]*ImageStreamChunk{
		previewChunk(0, 0, "0-preview"),
		imageChunk(schemas.ImageGenerationEventTypeCompleted, 0, "", "0-final", false),
		previewChunk(1, 0, "1-preview"),
		imageChunk(schemas.ImageGenerationEventTypeCompleted, 1, "", "1-final", false),
		imageChunk(schemas.ImageGenerationEventTypePartial, 1, "", "1-stray", false),
		previewChunk(2, 0, "2-preview"),
	})
	require.NotNil(t, got)
	assert.Equal(t, []schemas.ImageData{{B64JSON: "0-final", Index: 0}, {B64JSON: "1-final", Index: 1}}, got.Data)
}

// BenchmarkBuildCompleteImageFromImageStreamChunks is the build of the logged response at the end
// of an image stream: the common one-image stream (previews, then the completed image), and a
// stream of three images that each arrive as their own event.
func BenchmarkBuildCompleteImageFromImageStreamChunks(b *testing.B) {
	a := &Accumulator{}
	for _, tc := range []struct {
		name   string
		chunks []*ImageStreamChunk
	}{
		{
			name: "one_image_with_previews",
			chunks: []*ImageStreamChunk{
				previewChunk(0, 0, "preview-0"),
				previewChunk(0, 1, "preview-1"),
				previewChunk(0, 2, "preview-2"),
				imageChunk(schemas.ImageGenerationEventTypeCompleted, 0, "", "final", true),
			},
		},
		{
			name: "three_images",
			chunks: []*ImageStreamChunk{
				imageChunk(schemas.ImageGenerationEventTypePartial, 0, "https://cdn.example/0.jpeg", "", false),
				imageChunk(schemas.ImageGenerationEventTypePartial, 1, "https://cdn.example/1.jpeg", "", false),
				imageChunk(schemas.ImageGenerationEventTypePartial, 2, "https://cdn.example/2.jpeg", "", false),
				imageChunk(schemas.ImageGenerationEventTypeCompleted, 2, "https://cdn.example/2.jpeg", "", true),
			},
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if got := a.buildCompleteImageFromImageStreamChunks(tc.chunks); got == nil || len(got.Data) == 0 {
					b.Fatal("no images")
				}
			}
		})
	}
}
