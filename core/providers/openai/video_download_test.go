package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// A video larger than the stream threshold stays on the upstream connection: the response
// carries no content, and the reader left in the context yields the upstream bytes exactly.
// A video within the threshold is returned in memory as before.
func TestVideoDownloadStreamsAboveThreshold(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		streamed bool
	}{
		{"small", 4 << 10, false},
		{"large", 3 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			video := bytes.Repeat([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}, tc.size/8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/videos/vid_1/content" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(video)
			}))
			defer server.Close()

			provider := NewOpenAIProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL}}, testNoopLogger{})
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(schemas.BifrostContextKeyLargeResponseThreshold, providerUtils.VideoDownloadStreamThreshold)
			resp, bifrostErr := provider.VideoDownload(ctx, schemas.Key{Value: *schemas.NewSecretVar("test-key")}, &schemas.BifrostVideoDownloadRequest{
				Provider: schemas.OpenAI,
				ID:       "vid_1:openai",
			})
			if bifrostErr != nil {
				t.Fatalf("download: %+v", bifrostErr.Error)
			}
			if resp.ContentType != "video/mp4" {
				t.Fatalf("content type %q", resp.ContentType)
			}
			isLarge, _ := ctx.Value(schemas.BifrostContextKeyLargeResponseMode).(bool)
			if isLarge != tc.streamed {
				t.Fatalf("streamed = %v, want %v", isLarge, tc.streamed)
			}
			got := resp.Content
			if tc.streamed {
				if len(resp.Content) != 0 {
					t.Fatalf("streamed download also held %d bytes in memory", len(resp.Content))
				}
				reader, ok := ctx.Value(schemas.BifrostContextKeyLargeResponseReader).(io.ReadCloser)
				if !ok {
					t.Fatal("no large response reader in context")
				}
				var err error
				got, err = io.ReadAll(reader)
				if err != nil {
					t.Fatalf("read stream: %v", err)
				}
				if err := reader.Close(); err != nil {
					t.Fatalf("close stream: %v", err)
				}
			}
			if !bytes.Equal(got, video) {
				t.Fatalf("got %d bytes, want the %d upstream bytes", len(got), len(video))
			}
		})
	}
}
