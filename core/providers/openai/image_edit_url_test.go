package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// An edit whose input image is a URL goes upstream as the JSON form of POST /v1/images/edits
// with images[].image_url, as the OpenAI reference documents; an edit with image bytes keeps
// the multipart form.
func TestImageEditSendsURLInputsAsJSON(t *testing.T) {
	var contentType string
	var payload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		payload = nil
		if strings.HasPrefix(contentType, "application/json") {
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("upstream body is not JSON: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGk="}],"usage":{"input_tokens":10,"output_tokens":272,"total_tokens":282}}`))
	}))
	defer server.Close()

	provider := NewOpenAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL}}, testNoopLogger{})
	key := schemas.Key{Value: *schemas.NewSecretVar("test-key")}
	edit := func(images []schemas.ImageInput) {
		t.Helper()
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		_, bifrostErr := provider.ImageEdit(ctx, key, &schemas.BifrostImageEditRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-image-2",
			Input:    &schemas.ImageEditInput{Prompt: "add a hat", Images: images},
			Params:   &schemas.ImageEditParameters{N: schemas.Ptr(2), Size: schemas.Ptr("1024x1024"), Quality: schemas.Ptr("low")},
		})
		if bifrostErr != nil {
			t.Fatalf("edit: %+v", bifrostErr.Error)
		}
	}

	png := []byte("\x89PNG\r\n\x1a\n0000")
	edit([]schemas.ImageInput{{URL: "https://example.com/cat.png"}, {Image: png}})
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("URL input sent as %q", contentType)
	}
	images, _ := payload["images"].([]interface{})
	if len(images) != 2 {
		t.Fatalf("images = %v", payload["images"])
	}
	if got := images[0].(map[string]interface{})["image_url"]; got != "https://example.com/cat.png" {
		t.Fatalf("images[0].image_url = %v", got)
	}
	if got, _ := images[1].(map[string]interface{})["image_url"].(string); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("images[1].image_url = %q", got)
	}
	if payload["model"] != "gpt-image-2" || payload["prompt"] != "add a hat" || payload["n"] != float64(2) || payload["size"] != "1024x1024" || payload["quality"] != "low" {
		t.Fatalf("payload = %v", payload)
	}

	edit([]schemas.ImageInput{{Image: png}})
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("byte input sent as %q", contentType)
	}
}
