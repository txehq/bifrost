package datasheet

import (
	"net/http"
	"sync/atomic"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/network"
)

// fetchTestSeam stands in for the network a catalog fetch touches. A fetch resolves
// its host while validating the URL, then dials through the guarded client, so both
// have to be replaced for a test to stay off the network.
type fetchTestSeam struct {
	wrap     func(http.RoundTripper) http.RoundTripper
	resolver network.IPLookuper
}

// activeFetchTestSeam is nil outside tests.
var activeFetchTestSeam atomic.Pointer[fetchTestSeam]

// ValidateFetchURL applies the external URL policy a catalog fetch needs, which
// resolves the host.
func ValidateFetchURL(rawURL string) error {
	if seam := activeFetchTestSeam.Load(); seam != nil {
		return bifrost.ValidateExternalURLWithResolver(rawURL, true, seam.resolver)
	}
	return bifrost.ValidateExternalURL(rawURL, true)
}

// NewFetchClient returns the guarded client every catalog fetch uses: the private
// network policy on each connection and redirect hop.
func NewFetchClient(timeout time.Duration) *http.Client {
	client := network.NewPrivateNetworkHTTPClient(timeout)
	if seam := activeFetchTestSeam.Load(); seam != nil {
		client.Transport = seam.wrap(client.Transport)
	}
	return client
}

// SetFetchSeamForTest replaces the resolution and wraps the transport of every
// catalog fetch from now on, and returns the restore. The guarded client owns its
// transport, so replacing http.DefaultTransport reaches neither. Call it before any
// test or background worker starts. A nil wrap clears the seam. Tests only.
func SetFetchSeamForTest(wrap func(http.RoundTripper) http.RoundTripper, resolver network.IPLookuper) (restore func()) {
	var seam *fetchTestSeam
	if wrap != nil {
		seam = &fetchTestSeam{wrap: wrap, resolver: resolver}
	}
	previous := activeFetchTestSeam.Swap(seam)
	return func() { activeFetchTestSeam.Store(previous) }
}
