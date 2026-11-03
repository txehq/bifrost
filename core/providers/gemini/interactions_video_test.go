package gemini

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// omniServer answers the Interactions API as https://aistudio.google.com/docs/omni shows it:
// a background interaction in progress until it has been read polls times, then completed
// with its video inline, or failed.
type omniServer struct {
	mu       sync.Mutex
	requests []string
	bodies   [][]byte
	headers  []http.Header
	polls    int
	fail     bool
	video    []byte
}

func (s *omniServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.bodies = append(s.bodies, body)
	s.headers = append(s.headers, r.Header.Clone())
	reads := 0
	for _, req := range s.requests {
		if strings.HasPrefix(req, "GET /v1beta/interactions/") {
			reads++
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1beta/interactions":
		fmt.Fprint(w, `{"id":"v1_abc123","status":"in_progress","object":"interaction","model":"gemini-omni-1.1-flash","created":"2026-11-04T10:00:00Z"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/v1beta/interactions/v1_abc123":
		switch {
		case reads < s.polls:
			fmt.Fprint(w, `{"id":"v1_abc123","status":"in_progress","object":"interaction","model":"gemini-omni-1.1-flash"}`)
		case s.fail:
			fmt.Fprint(w, `{"id":"v1_abc123","status":"failed","object":"interaction","model":"gemini-omni-1.1-flash"}`)
		default:
			fmt.Fprintf(w, `{"steps":[{"type":"user_input","content":[{"type":"text","text":"a marble"}]},
				{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":%q}]}],
				"id":"v1_abc123","status":"completed","model":"gemini-omni-1.1-flash","object":"interaction"}`, base64.StdEncoding.EncodeToString(s.video))
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1beta/interactions/v1_abc123/cancel",
		r.Method == http.MethodDelete && r.URL.Path == "/v1beta/interactions/v1_abc123":
		fmt.Fprint(w, `{}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`)
	}
}

func newOmniProvider(t *testing.T, s *omniServer) *GeminiProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	return NewGeminiProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: srv.URL + "/v1beta", DefaultRequestTimeoutInSeconds: 10}}, testNoopLogger{})
}

func omniCtx(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	t.Cleanup(ctx.Cancel)
	return ctx
}

var omniKey = schemas.Key{Value: *schemas.NewSecretVar("k")}

// A Gemini Omni video is a background interaction: created with its reference image and prompt
// as content items and its size as aspect ratio and resolution, read until it completes, and
// downloaded from its inline data. Its state never carries the video's bytes.
func TestOmniVideoLifecycle(t *testing.T) {
	s := &omniServer{polls: 2, video: []byte("\x00\x00\x00\x18ftypmp42 omni")}
	provider := newOmniProvider(t, s)
	ctx := omniCtx(t)

	created, bifrostErr := provider.VideoGeneration(ctx, omniKey, &schemas.BifrostVideoGenerationRequest{
		Model:  "gemini-omni-1.1-flash",
		Input:  &schemas.VideoGenerationInput{Prompt: "turn this into realistic footage", InputReference: schemas.Ptr("data:image/jpeg;base64,/9j/4AAQ")},
		Params: &schemas.VideoGenerationParameters{Size: "720x1280", Seconds: schemas.Ptr("8")},
	})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.VideoStatusInProgress, created.Status)
	require.Equal(t, "interactions%2Fv1_abc123:gemini", created.ID)

	body := s.bodies[0]
	require.Equal(t, "gemini-omni-1.1-flash", gjson.GetBytes(body, "model").Str)
	require.True(t, gjson.GetBytes(body, "background").Bool())
	require.Equal(t, "image", gjson.GetBytes(body, "input.0.type").Str)
	require.Equal(t, "/9j/4AAQ", gjson.GetBytes(body, "input.0.data").Str)
	require.Equal(t, "image/jpeg", gjson.GetBytes(body, "input.0.mime_type").Str)
	require.Equal(t, "text", gjson.GetBytes(body, "input.1.type").Str)
	require.Equal(t, "video", gjson.GetBytes(body, "response_format.type").Str)
	require.Equal(t, "9:16", gjson.GetBytes(body, "response_format.aspect_ratio").Str)
	require.Equal(t, "720p", gjson.GetBytes(body, "response_format.resolution").Str)
	require.False(t, gjson.GetBytes(body, "response_format.duration").Exists(), "Omni documents no duration")
	require.Equal(t, "k", s.headers[0].Get("x-goog-api-key"))
	require.Equal(t, interactionsAPIRevision, s.headers[0].Get("Api-Revision"))

	running, bifrostErr := provider.VideoRetrieve(ctx, omniKey, &schemas.BifrostVideoRetrieveRequest{ID: created.ID})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.VideoStatusInProgress, running.Status)

	done, bifrostErr := provider.VideoRetrieve(ctx, omniKey, &schemas.BifrostVideoRetrieveRequest{ID: created.ID})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.VideoStatusCompleted, done.Status)
	require.Equal(t, created.ID, done.ID)
	for _, v := range done.Videos {
		require.Nil(t, v.Base64Data, "a status read carries no video bytes")
	}

	download, bifrostErr := provider.VideoDownload(ctx, omniKey, &schemas.BifrostVideoDownloadRequest{ID: created.ID})
	require.Nil(t, bifrostErr)
	require.Equal(t, s.video, download.Content)
	require.Equal(t, "video/mp4", download.ContentType)

	deleted, bifrostErr := provider.VideoDelete(ctx, omniKey, &schemas.BifrostVideoDeleteRequest{ID: created.ID})
	require.Nil(t, bifrostErr)
	require.True(t, deleted.Deleted)
	require.Equal(t, "DELETE /v1beta/interactions/v1_abc123", s.requests[len(s.requests)-1], "a finished interaction is deleted")
}

// A running interaction is cancelled, not deleted; a failed one is a failed video.
func TestOmniVideoCancelAndFailure(t *testing.T) {
	s := &omniServer{polls: 100}
	provider := newOmniProvider(t, s)
	ctx := omniCtx(t)
	created, bifrostErr := provider.VideoGeneration(ctx, omniKey, &schemas.BifrostVideoGenerationRequest{
		Model: "gemini-omni-1.1-flash", Input: &schemas.VideoGenerationInput{Prompt: "a marble"}})
	require.Nil(t, bifrostErr)
	require.Equal(t, "a marble", gjson.GetBytes(s.bodies[0], "input").Str, "a prompt alone is the input string")
	_, bifrostErr = provider.VideoDelete(ctx, omniKey, &schemas.BifrostVideoDeleteRequest{ID: created.ID})
	require.Nil(t, bifrostErr)
	require.Equal(t, "POST /v1beta/interactions/v1_abc123/cancel", s.requests[len(s.requests)-1])

	failing := &omniServer{fail: true}
	provider = newOmniProvider(t, failing)
	created, bifrostErr = provider.VideoGeneration(ctx, omniKey, &schemas.BifrostVideoGenerationRequest{
		Model: "gemini-omni-1.1-flash", Input: &schemas.VideoGenerationInput{Prompt: "a marble"}})
	require.Nil(t, bifrostErr)
	failed, bifrostErr := provider.VideoRetrieve(ctx, omniKey, &schemas.BifrostVideoRetrieveRequest{ID: created.ID})
	require.Nil(t, bifrostErr)
	require.Equal(t, schemas.VideoStatusFailed, failed.Status)
	require.NotNil(t, failed.Error)
	_, bifrostErr = provider.VideoDownload(ctx, omniKey, &schemas.BifrostVideoDownloadRequest{ID: created.ID})
	require.NotNil(t, bifrostErr)
}

// Omni produces 16:9 and 9:16 at 360p, 720p, 1080p and 4k; any other size is refused before
// the request, not sent to be misread.
func TestOmniVideoSize(t *testing.T) {
	for size, want := range map[string][2]string{"1280x720": {"16:9", "720p"}, "1080x1920": {"9:16", "1080p"}, "640x360": {"16:9", "360p"}, "3840x2160": {"16:9", "4k"}} {
		ratio, resolution, err := interactionVideoSize(size)
		require.NoError(t, err, size)
		require.Equal(t, want, [2]string{ratio, resolution}, size)
	}
	for _, size := range []string{"1024x1024", "1280x721", "1920x1088", "big"} {
		_, _, err := interactionVideoSize(size)
		require.Error(t, err, size)
	}
	require.True(t, IsInteractionsVideoModel("gemini-omni-1.1-flash"))
	require.True(t, IsInteractionsVideoModel("google/gemini-omni-1.1-flash"))
	require.False(t, IsInteractionsVideoModel("veo-3.1-generate-001"))
}
