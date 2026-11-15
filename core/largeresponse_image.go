package bifrost

import "github.com/capsohq/bifrost/core/schemas"

// backfillImageResponse backfills an image generation, edit or variation response. When the
// provider streamed the vendor's answer to the caller unread (large-response mode), the response
// is marked unread first, so no image provider's empty placeholder reaches the post-hooks as
// output_images 0 (fork, TXE-7017). Every image provider returns through here, so a provider
// added later is covered without a change of its own.
func backfillImageResponse(ctx *schemas.BifrostContext, response *schemas.BifrostImageGenerationResponse, req *schemas.BifrostRequest) {
	if ctx != nil {
		if large, _ := ctx.Value(schemas.BifrostContextKeyLargeResponseMode).(bool); large {
			response.MarkLargeResponseUnread(req)
		}
	}
	response.BackfillParams(req)
}
