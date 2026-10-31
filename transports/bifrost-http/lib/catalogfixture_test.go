package lib

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Config tests exercise catalog initialization and persistence, not the size or
// changing contents of the public feeds, and a boot must not depend on reaching them.
// The default URLs are answered here, a local test server goes through the guarded
// transport as it would in production, and any other host fails closed.
type catalogFixtureTransport struct {
	guarded http.RoundTripper
}

// catalogFixtureHits counts the default-URL requests the fixture has answered.
var catalogFixtureHits atomic.Int64

func (rt catalogFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	switch req.URL.String() {
	case modelcatalog.DefaultPricingURL:
		body = `{"gpt-4o-mini":{"provider":"openai","mode":"chat","input_cost_per_token":0.00000015,"output_cost_per_token":0.0000006}}`
	case modelcatalog.DefaultModelParametersURL:
		body = `{"gpt-4o-mini":{"supports_reasoning":false,"supports_sampling_params":true}}`
	case modelcatalog.DefaultMCPLibraryURL:
		body = `{"servers":[]}`
	default:
		if host := req.URL.Hostname(); host == "localhost" || net.ParseIP(host).IsLoopback() {
			return rt.guarded.RoundTrip(req)
		}
		return nil, fmt.Errorf("catalog fixture: refused a live fetch of %s", req.URL.Host)
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	catalogFixtureHits.Add(1)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// catalogFixtureLookup answers the URL validation's DNS lookup: the catalog host and
// local servers resolve, and any other host fails closed before a fetch starts.
type catalogFixtureLookup struct{}

func (catalogFixtureLookup) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	if host == "localhost" {
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}
	if catalog, err := url.Parse(modelcatalog.DefaultPricingURL); err == nil && host == catalog.Hostname() {
		return []net.IP{net.ParseIP("203.0.113.1")}, nil
	}
	return nil, fmt.Errorf("catalog fixture: refused a live lookup of %s", host)
}

func TestMain(m *testing.M) {
	// Install once before any test or background worker starts. Leave it in
	// place until process exit so surviving workers cannot race a restoration.
	datasheet.SetFetchSeamForTest(func(guarded http.RoundTripper) http.RoundTripper {
		return catalogFixtureTransport{guarded: guarded}
	}, catalogFixtureLookup{})
	os.Exit(m.Run())
}

// TestConfigBootTakesTheCatalogFromTheFixture: every boot here syncs the catalog, so
// a fixture the fetch client bypasses puts the public feeds under every config test,
// which is what ran this package into its 40-minute limit. A boot only succeeds
// offline if that sheet loaded, so a fallback does not pass for it.
func TestConfigBootTakesTheCatalogFromTheFixture(t *testing.T) {
	initTestLogger()
	tempDir := createTempDir(t)
	createConfigFile(t, tempDir, makeConfigDataWithProvidersAndDir(nil, tempDir))

	before := catalogFixtureHits.Load()
	ctx := context.Background()
	config, err := LoadConfig(ctx, tempDir)
	require.NoError(t, err)
	t.Cleanup(func() { config.Close(ctx) })
	require.Greater(t, catalogFixtureHits.Load(), before, "the boot's catalog fetches did not reach the fixture")
	require.NotNil(t, config.ModelCatalog, "the fixture's pricing sheet did not load")
	require.NotNil(t, config.ModelCatalog.GetPricingEntryForModel("gpt-4o-mini", schemas.OpenAI), "the fixture's pricing sheet did not load")
}

// The fixture fails closed: a catalog fetch of any other host is refused before it
// reaches the network, while a local server still works.
func TestCatalogFixtureRefusesLiveHosts(t *testing.T) {
	client := datasheet.NewFetchClient(5 * time.Second)
	for _, target := range []string{"https://getbifrost.ai/other", "https://example.com/pricing.json", "http://203.0.113.10/pricing.json", "http://10.0.0.1/pricing.json"} {
		_, err := client.Get(target)
		require.Error(t, err, target)
		assert.Contains(t, err.Error(), "refused a live fetch", target)
	}
	err := datasheet.ValidateFetchURL("https://example.com/pricing.json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused a live lookup")
}
