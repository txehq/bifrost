package dashscope

import (
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// DefaultBaseURL is DashScope's Singapore (international) site. The vendor sheet leaves the
// host name to be confirmed; operators override it with network_config.base_url.
const DefaultBaseURL = "https://dashscope-intl.aliyuncs.com"

// isWorkspaceOperation reports whether an operation goes to the workspace base. Wan video
// and Qwen3-ASR transcription are served from the workspace address (the Qwen3-ASR API
// reference names {WorkspaceId}.ap-southeast-1.maas.aliyuncs.com for Singapore); images
// and Qwen3-TTS speech from the regional base (the Qwen3-TTS reference names
// dashscope-intl.aliyuncs.com). Which Singapore host each serves is not settled
// (TXE-6433); both bases are configurable.
func isWorkspaceOperation(requestType schemas.RequestType) bool {
	switch requestType {
	case schemas.VideoGenerationRequest,
		schemas.VideoEditRequest,
		schemas.VideoRetrieveRequest,
		schemas.VideoCancelRequest,
		schemas.VideoDownloadRequest,
		schemas.VideoDeleteRequest,
		schemas.VideoListRequest,
		schemas.VideoRemixRequest,
		schemas.TranscriptionRequest:
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
