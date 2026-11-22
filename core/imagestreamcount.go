package bifrost

import schemas "github.com/capsohq/bifrost/core/schemas"

// decideImageStreamCountForAttempt refuses this attempt when it streams an image generation or
// edit that asks for more than one image and the attempt's provider does not declare that its
// stream relays and counts every image (schemas.ImageStreamCounter; fork, TXE-8077). It runs
// before any upstream call, on every attempt, so a fallback to a provider that counts is still
// taken.
func decideImageStreamCountForAttempt(provider schemas.Provider, requestType schemas.RequestType, req *schemas.BifrostRequest) *schemas.BifrostError {
	if !schemas.IsImageStreamRequestType(requestType) {
		return nil
	}
	count, param := schemas.RequestedStreamImageCount(req)
	if count <= 1 {
		return nil
	}
	if counter, ok := provider.(schemas.ImageStreamCounter); ok && counter.StreamsEveryImage(requestType) {
		return nil
	}
	return schemas.NewImageStreamCountError(provider.GetProviderKey(), param, count)
}
