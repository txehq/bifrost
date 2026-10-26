package schemas

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fork's flat multimodal list (Volcengine's wire shape) maps onto upstream's
// content parts one entry at a time; the transport parses the list itself
// (handlers/inference_embedding_test.go).
func TestMultiModalEmbeddingInput_ToEmbeddingContentPart(t *testing.T) {
	text := "a photo of a cat"
	cases := []struct {
		name  string
		input MultiModalEmbeddingInput
		check func(t *testing.T, part EmbeddingContentPart)
	}{
		{"text", MultiModalEmbeddingInput{Type: MultiModalEmbeddingText, Text: &text}, func(t *testing.T, part EmbeddingContentPart) {
			if part.Type != EmbeddingContentPartTypeText || part.Text == nil || *part.Text != text {
				t.Fatalf("got %+v, want the text part", part)
			}
		}},
		{"image_url", MultiModalEmbeddingInput{Type: MultiModalEmbeddingImageURL, ImageURL: &MultiModalEmbeddingMediaURL{URL: "https://example.com/cat.jpg"}}, func(t *testing.T, part EmbeddingContentPart) {
			if part.Type != EmbeddingContentPartTypeImage || part.Image == nil || part.Image.URL == nil || *part.Image.URL != "https://example.com/cat.jpg" {
				t.Fatalf("got %+v, want the image part", part)
			}
		}},
		{"video_url", MultiModalEmbeddingInput{Type: MultiModalEmbeddingVideoURL, VideoURL: &MultiModalEmbeddingMediaURL{URL: "https://example.com/cat.mp4"}}, func(t *testing.T, part EmbeddingContentPart) {
			if part.Type != EmbeddingContentPartTypeVideo || part.Video == nil || part.Video.URL == nil || *part.Video.URL != "https://example.com/cat.mp4" {
				t.Fatalf("got %+v, want the video part", part)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			part, err := tc.input.ToEmbeddingContentPart()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := part.Validate(); err != nil {
				t.Fatalf("converted part does not validate: %v", err)
			}
			tc.check(t, part)
		})
	}
}

func TestMultiModalEmbeddingInput_ToEmbeddingContentPartRejectsBadEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input MultiModalEmbeddingInput
		want  string
	}{
		{"image without url", MultiModalEmbeddingInput{Type: MultiModalEmbeddingImageURL}, "requires image_url"},
		{"video without url", MultiModalEmbeddingInput{Type: MultiModalEmbeddingVideoURL}, "requires video_url"},
		{"unknown type", MultiModalEmbeddingInput{Type: "audio_url"}, "unsupported multimodal embedding input type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.input.ToEmbeddingContentPart()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestMultiModalEmbeddingInput_Types(t *testing.T) {
	if MultiModalEmbeddingText != "text" {
		t.Fatalf("expected 'text', got '%s'", MultiModalEmbeddingText)
	}
	if MultiModalEmbeddingImageURL != "image_url" {
		t.Fatalf("expected 'image_url', got '%s'", MultiModalEmbeddingImageURL)
	}
	if MultiModalEmbeddingVideoURL != "video_url" {
		t.Fatalf("expected 'video_url', got '%s'", MultiModalEmbeddingVideoURL)
	}
}

// Clone must not share the provider-specific sparse_embedding map: providers read and
// occasionally write params per item.
func TestEmbeddingParameters_CloneCopiesSparseEmbedding(t *testing.T) {
	instructions := "Instruction: retrieve"
	p := &EmbeddingParameters{Instructions: &instructions, SparseEmbedding: map[string]interface{}{"type": "enabled"}}
	clone := p.Clone()
	clone.SparseEmbedding["type"] = "disabled"
	if p.SparseEmbedding["type"] != "enabled" {
		t.Fatalf("mutating the clone changed the original: %v", p.SparseEmbedding)
	}
	if clone.Instructions == nil || *clone.Instructions != instructions {
		t.Fatalf("clone lost instructions: %v", clone.Instructions)
	}
}

func TestEmbeddingData_SparseEmbeddingRoundTrip(t *testing.T) {
	data := []byte(`{"index":0,"object":"embedding","embedding":[0.1,0.2],"sparse_embedding":[{"index":1,"value":0.5}]}`)
	var d EmbeddingData
	if err := Unmarshal(data, &d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.SparseEmbedding) != 1 || d.SparseEmbedding[0].Index != 1 || d.SparseEmbedding[0].Value != 0.5 {
		t.Fatalf("sparse embedding not decoded: %+v", d.SparseEmbedding)
	}
	out, err := Marshal(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), `"sparse_embedding":[{"index":1,"value":0.5}]`) {
		t.Fatalf("sparse embedding not encoded: %s", out)
	}
}

func TestEmbeddingInputValidateRejectsEmpty(t *testing.T) {
	err := ValidateEmbeddingInput(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty")
}

func TestEmbeddingContentPartValidateRejectsMultipleModalities(t *testing.T) {
	text := "bad"
	part := EmbeddingContentPart{
		Type:  EmbeddingContentPartTypeImage,
		Text:  &text,
		Image: &EmbeddingMediaPart{URL: Ptr("https://example.com/img.png")},
	}

	err := part.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exactly one modality")
}
