package elevenlabs

import (
	"bytes"
	"mime/multipart"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TXE-7018: a transcription whose extra params name the model, or spell model_id or file
// another way, is refused; the exact model_id and file are still Bifrost's and skipped.
func TestWriteTranscriptionExtraFieldsRefusesAModel(t *testing.T) {
	for _, param := range []string{"model", "Model", "modelId", "MODEL_ID", "File"} {
		var body bytes.Buffer
		bifrostErr := writeTranscriptionExtraFields(multipart.NewWriter(&body), &ElevenlabsTranscriptionRequest{
			ExtraParams: map[string]interface{}{param: "scribe_v2", "keyterms": "Ada"},
		})
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 ||
			bifrostErr.Error.Param != param || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerInvalidRequest {
			t.Errorf("extra param %q: want a 400 caller_invalid_request naming it, got %+v", param, bifrostErr)
		}
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if bifrostErr := writeTranscriptionExtraFields(writer, &ElevenlabsTranscriptionRequest{
		ExtraParams: map[string]interface{}{"model_id": "other", "file": "other", "keyterms": "Ada"},
	}); bifrostErr != nil {
		t.Fatalf("unexpected refusal: %+v", bifrostErr)
	}
	_ = writer.Close()
	if bytes.Contains(body.Bytes(), []byte("other")) || !bytes.Contains(body.Bytes(), []byte("Ada")) {
		t.Fatalf("form = %s", body.String())
	}
}
