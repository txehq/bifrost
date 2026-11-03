package utils

import (
	"net/url"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// The route reads the raw path segment and decodes it once; the provider decodes the ID
// part once more. Each upstream ID must come back unchanged through that chain, however
// many colons, slashes or percent signs it carries.
func TestProviderScopedVideoIDRoundTrip(t *testing.T) {
	upstreamIDs := []string{
		"vid_123",
		"projects/p/locations/us-central1/publishers/google/models/veo-3.1/operations/7281",
		"task:with:colons",
		"literal%41percent",
		"mixed/a:b%2Fc?d#e",
	}
	providers := []schemas.ModelProvider{schemas.OpenAI, schemas.Vertex, "vertex-us"}
	for _, id := range upstreamIDs {
		for _, provider := range providers {
			scoped := AddVideoIDProviderSuffix(id, provider)
			// A client puts the scoped ID into the URL path as one escaped segment.
			segment := url.PathEscape(scoped)
			decoded, err := url.PathUnescape(segment)
			if err != nil {
				t.Fatalf("%q: route decode: %v", id, err)
			}
			gotProvider, rawID, ok := SplitProviderScopedVideoID(decoded)
			if !ok {
				t.Fatalf("%q: %q did not split", id, decoded)
			}
			if gotProvider != provider {
				t.Fatalf("%q: provider %q, want %q", id, gotProvider, provider)
			}
			if got := StripVideoIDProviderSuffix(rawID, provider); got != id {
				t.Fatalf("upstream ID %q came back as %q", id, got)
			}
		}
	}
}

func TestSplitProviderScopedVideoIDRejectsMissingParts(t *testing.T) {
	for _, id := range []string{"", "no-provider", ":openai", "vid_1:"} {
		if _, _, ok := SplitProviderScopedVideoID(id); ok {
			t.Fatalf("%q split, want rejection", id)
		}
	}
}
