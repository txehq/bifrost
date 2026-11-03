package integrations

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

func imageStreamEvents(t *testing.T, raw bool, chunks ...*schemas.BifrostImageGenerationStreamResponse) []string {
	t.Helper()
	stream := make(chan *schemas.BifrostStreamChunk, len(chunks))
	for _, c := range chunks {
		stream <- &schemas.BifrostStreamChunk{BifrostImageGenerationStreamResponse: c}
	}
	close(stream)
	bifrostCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if raw {
		bifrostCtx.SetValue(schemas.BifrostContextKeySendBackRawResponse, true)
	}
	router := NewGenericRouter(nil, &mockHandlerStore{}, nil, nil, nil, bifrost.NewNoOpLogger())
	ctx := &fasthttp.RequestCtx{}
	router.handleStreaming(ctx, bifrostCtx, RouteConfig{
		Type: RouteConfigTypeOpenAI,
		StreamConfig: &StreamConfig{
			ImageGenerationStreamResponseConverter: func(_ *schemas.BifrostContext, resp *schemas.BifrostImageGenerationStreamResponse) (string, interface{}, error) {
				return string(resp.Type), map[string]any{"type": string(resp.Type), "b64_json": resp.B64JSON}, nil
			},
		},
	}, "", stream, newCancelRecorder().cancel)
	body, err := io.ReadAll(ctx.Response.BodyStream())
	require.NoError(t, err)
	var events []string
	for _, line := range strings.Split(string(body), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok && strings.HasPrefix(data, "{") {
			events = append(events, data)
		}
	}
	return events
}

// A streamed event a caller asked raw responses for carries the upstream's event behind it
// under extra_fields.raw_response; one that did not ask gets the converted event alone.
func TestStreamEventsCarryTheRawResponseWhenAsked(t *testing.T) {
	chunk := func() *schemas.BifrostImageGenerationStreamResponse {
		return &schemas.BifrostImageGenerationStreamResponse{Type: schemas.ImageGenerationEventTypeCompleted, B64JSON: "AAAA",
			ExtraFields: schemas.BifrostResponseExtraFields{RawResponse: json.RawMessage(`[{"candidates":[]},{"usageMetadata":{"candidatesTokenCount":1290}}]`)}}
	}
	asked := imageStreamEvents(t, true, chunk())
	require.Len(t, asked, 1)
	require.Equal(t, "AAAA", gjson.Get(asked[0], "b64_json").Str)
	require.Equal(t, int64(1290), gjson.Get(asked[0], "extra_fields.raw_response.1.usageMetadata.candidatesTokenCount").Int())

	notAsked := imageStreamEvents(t, false, chunk())
	require.Len(t, notAsked, 1)
	require.False(t, gjson.Get(notAsked[0], "extra_fields.raw_response").Exists(), notAsked[0])
}
