package utils

import (
	"net/url"
	"strings"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// StripVideoIDProviderSuffix removes ":<provider>" from a video ID if present.
func StripVideoIDProviderSuffix(videoID string, provider schemas.ModelProvider) string {
	suffix := ":" + string(provider)
	stripped := strings.TrimSuffix(videoID, suffix)
	// URL decode the ID to restore original characters (e.g., %2F -> /)
	if decoded, err := url.PathUnescape(stripped); err == nil {
		return decoded
	}
	return stripped
}

// VideoDownloadStreamThreshold is the size above which a video download is streamed from
// the upstream to the client instead of being read into memory. Bifrost.VideoDownloadRequest
// sets it when the caller has not chosen a threshold.
const VideoDownloadStreamThreshold int64 = 1 << 20

// FinishVideoDownload reads the body of a successful video download response. When the
// body is larger than the threshold in ctx, it is left on the wire: content is nil, the
// reader in ctx owns resp and the transport streams it, and owned is false so the caller
// must not release resp. Otherwise content is an owned copy and owned is true.
func FinishVideoDownload(ctx *schemas.BifrostContext, resp *fasthttp.Response, defaultContentType string, logger schemas.Logger) (content []byte, contentType string, owned bool, bifrostErr *schemas.BifrostError) {
	contentType = string(resp.Header.ContentType())
	if contentType == "" {
		contentType = defaultContentType
	}
	body, isLarge, bifrostErr := FinalizeResponseWithLargeDetection(ctx, resp, logger)
	if bifrostErr != nil {
		return nil, "", true, bifrostErr
	}
	if isLarge {
		ctx.SetValue(schemas.BifrostContextKeyLargeResponseContentType, contentType)
		return nil, contentType, false, nil
	}
	return append([]byte(nil), body...), contentType, true, nil
}

// SplitProviderScopedVideoID splits a provider-scoped video ID "<escaped id>:<provider>"
// at its last colon. The provider name never contains a colon, while the escaped ID can
// (url.PathEscape keeps ":"), so the last colon is the only unambiguous separator. The ID
// part is returned still escaped: the provider's StripVideoIDProviderSuffix decodes it once,
// matching the single url.PathEscape in AddVideoIDProviderSuffix.
func SplitProviderScopedVideoID(videoID string) (schemas.ModelProvider, string, bool) {
	i := strings.LastIndexByte(videoID, ':')
	if i <= 0 || i == len(videoID)-1 {
		return "", "", false
	}
	return schemas.ModelProvider(videoID[i+1:]), videoID[:i], true
}

// AddVideoIDProviderSuffix ensures a video ID is scoped as "<id>:<provider>".
func AddVideoIDProviderSuffix(videoID string, provider schemas.ModelProvider) string {
	if videoID == "" {
		return videoID
	}
	suffix := ":" + string(provider)
	if strings.HasSuffix(videoID, suffix) {
		return videoID
	}
	// URL-encode the video ID to make it safe for URL paths
	// This converts / to %2F and other special characters
	escapedVideoID := url.PathEscape(videoID)
	return escapedVideoID + suffix
}
