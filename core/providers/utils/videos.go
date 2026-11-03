package utils

import (
	"net/url"
	"strings"

	schemas "github.com/capsohq/bifrost/core/schemas"
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
