package dashscope

import (
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// DefaultBaseURL is DashScope's Singapore (international) site. The vendor sheet leaves the
// host name to be confirmed; operators override it with network_config.base_url.
const DefaultBaseURL = "https://dashscope-intl.aliyuncs.com"

// isWorkspaceOperation reports whether an operation goes to the workspace base. Wan video
// is served from the workspace address; images, speech and transcription from the
// regional base.
func isWorkspaceOperation(requestType schemas.RequestType) bool {
	switch requestType {
	case schemas.VideoGenerationRequest,
		schemas.VideoEditRequest,
		schemas.VideoRetrieveRequest,
		schemas.VideoDownloadRequest,
		schemas.VideoDeleteRequest,
		schemas.VideoListRequest,
		schemas.VideoRemixRequest:
		return true
	}
	return false
}

// setAuthHeader writes DashScope's auth header: the API key as a bearer token.
func setAuthHeader(req *fasthttp.Request, key schemas.Key) {
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
}
