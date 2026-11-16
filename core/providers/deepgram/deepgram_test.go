package deepgram_test

import (
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/internal/llmtests"
	"github.com/capsohq/bifrost/core/livetest"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// TestDeepgram is the live check (make test-core PROVIDER=deepgram): one Nova-3
// transcription of Deepgram's public sample by URL. It needs DEEPGRAM_API_KEY and is
// skipped without it.
func TestDeepgram(t *testing.T) {
	livetest.Require(t, "DEEPGRAM_API_KEY")
	client, ctx, cancel, err := llmtests.SetupTest()
	if err != nil {
		t.Fatalf("Error initializing test setup: %v", err)
	}
	defer cancel()
	defer client.Shutdown()

	bifrostCtx := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	resp, bifrostErr := client.TranscriptionRequest(bifrostCtx, &schemas.BifrostTranscriptionRequest{
		Provider: schemas.Deepgram,
		Model:    "nova-3",
		Input:    &schemas.TranscriptionInput{},
		Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{
			schemas.TranscriptionURLParam: "https://dpgr.am/spacewalk.wav",
			"smart_format":                "true",
		}},
	})
	if bifrostErr != nil {
		t.Fatalf("live transcription failed: %s", bifrostErr.GetErrorString())
	}
	if strings.TrimSpace(resp.Text) == "" {
		t.Fatal("live transcription returned no text")
	}
	if resp.Usage == nil || resp.Usage.Type != "duration" || resp.Usage.Seconds == nil || *resp.Usage.Seconds <= 0 {
		t.Fatalf("usage = %+v, want a positive duration", resp.Usage)
	}
}
