package openai

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// multipartFields reads a multipart body into its parts in order: name and value for a field,
// name and filename for a file.
type multipartPart struct {
	name, value, filename string
}

func readMultipartParts(t *testing.T, contentType string, body []byte) []multipartPart {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("ParseMediaType(%q): %v", contentType, err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var parts []multipartPart
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("NextPart(): %v", err)
		}
		data, _ := io.ReadAll(part)
		p := multipartPart{name: part.FormName(), filename: part.FileName()}
		if p.filename == "" {
			p.value = string(data)
		}
		parts = append(parts, p)
	}
}

func imageEditWithExtras() *schemas.BifrostImageEditRequest {
	return &schemas.BifrostImageEditRequest{
		Provider: schemas.OpenAI,
		Model:    "gpt-image-1",
		Input: &schemas.ImageEditInput{
			Prompt: "add a hat",
			Images: []schemas.ImageInput{{Image: []byte("\x89PNG\r\n\x1a\nfake")}},
		},
		Params: &schemas.ImageEditParameters{
			Size:       schemas.Ptr("1024x1024"),
			Resolution: schemas.Ptr("2K"),
			ExtraParams: map[string]interface{}{
				"resolution":  "2K",                                   // a unified field is still an extra until a provider converts it
				"watermark":   false,                                  // a JSON bool
				"strength":    0.35,                                   // a JSON number
				"style_ref":   map[string]interface{}{"b": 2, "a": 1}, // a JSON object
				"camera":      "fixed",                                // a multipart string, written verbatim
				"size":        "256x256",                              // collides with a typed field: the typed value wins
				"ignored_nil": nil,
			},
		},
	}
}

// TXE-6518 (contract C4 BF5): with x-bf-passthrough-extra-params: true, the multipart image edit
// sent upstream carries every extra param as a form field, keys sorted, before the file parts.
// Without the header it carries none, as before.
func TestImageEditMultipartPassthroughExtraParams(t *testing.T) {
	for _, tc := range []struct {
		name        string
		passthrough bool
		want        []multipartPart
	}{
		{
			name:        "header set",
			passthrough: true,
			want: []multipartPart{
				{name: "model", value: "gpt-image-1"},
				{name: "prompt", value: "add a hat"},
				{name: "size", value: "1024x1024"},
				{name: "camera", value: "fixed"},
				{name: "resolution", value: "2K"},
				{name: "strength", value: "0.35"},
				{name: "style_ref", value: `{"a":1,"b":2}`},
				{name: "watermark", value: "false"},
				{name: "image[]", filename: "image0.png"},
			},
		},
		{
			name:        "header absent",
			passthrough: false,
			want: []multipartPart{
				{name: "model", value: "gpt-image-1"},
				{name: "prompt", value: "add a hat"},
				{name: "size", value: "1024x1024"},
				{name: "image[]", filename: "image0.png"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotType string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/images/edits" {
					t.Errorf("path = %s, want /v1/images/edits", r.URL.Path)
				}
				gotType = r.Header.Get("Content-Type")
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGk="}]}`))
			}))
			defer server.Close()

			provider := NewOpenAIProvider(&schemas.ProviderConfig{
				NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
			}, testNoopLogger{})
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			if tc.passthrough {
				ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
			}

			_, bifrostErr := provider.ImageEdit(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, imageEditWithExtras())
			if bifrostErr != nil {
				t.Fatalf("ImageEdit: %+v", bifrostErr.Error)
			}
			got := readMultipartParts(t, gotType, gotBody)
			if len(got) != len(tc.want) {
				t.Fatalf("parts = %+v\nwant  %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("part %d = %+v, want %+v\nall parts %+v", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// The OpenAI images API has no resolution field. Until an OpenAI-shaped provider converts the
// unified tier, the JSON body is the one sent before the field existed: no typed resolution, the
// raw extra param (sent only with the passthrough header) unchanged.
func TestToOpenAIImageGenerationRequestDropsUnifiedResolution(t *testing.T) {
	params := &schemas.ImageGenerationParameters{
		Resolution:  schemas.Ptr("2K"),
		ExtraParams: map[string]interface{}{"resolution": "2K"},
	}
	req := ToOpenAIImageGenerationRequest(&schemas.BifrostImageGenerationRequest{
		Provider: schemas.XAI,
		Model:    "grok-imagine-image",
		Input:    &schemas.ImageGenerationInput{Prompt: "a fox"},
		Params:   params,
	})
	body, err := schemas.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte("resolution")) {
		t.Fatalf("typed resolution leaked into the OpenAI body: %s", body)
	}
	if req.ExtraParams["resolution"] != "2K" {
		t.Fatalf("extra resolution = %v, want it kept for the passthrough channel", req.ExtraParams["resolution"])
	}
	if params.Resolution == nil {
		t.Fatalf("the caller's request was mutated")
	}
}
