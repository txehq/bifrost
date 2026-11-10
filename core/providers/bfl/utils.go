package bfl

import (
	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// DefaultBaseURL is BFL's global endpoint, which routes to the nearest region.
// Operators pin a region with network_config.base_url.
const DefaultBaseURL = "https://api.bfl.ai"

// authHeader is the header BFL reads the API key from.
const authHeader = "x-key"

// setAuthHeader writes BFL's auth header. An Authorization header is never sent: BFL
// does not read one, and leaving it out keeps the key out of headers BFL might log.
func setAuthHeader(req *fasthttp.Request, key schemas.Key) {
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set(authHeader, value)
	}
}
