package datasheet

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/framework/configstore/tables"
)

// TestSyncFallsBackToDBWhenURLHangs guards BF-2486: a pricing URL that never
// answers used to burn the whole sync deadline, so the DB fallback ran on an
// expired ctx and force sync failed with "failed to get pricing records:
// context deadline exceeded" even though the DB had data.
func TestSyncFallsBackToDBWhenURLHangs(t *testing.T) {
	origPricing, origParams := pricingFetchTimeout, paramsFetchTimeout
	pricingFetchTimeout, paramsFetchTimeout = 500*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { pricingFetchTimeout, paramsFetchTimeout = origPricing, origParams })

	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	ctx := context.Background()
	cs, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, logger)
	if err != nil {
		t.Fatalf("failed to create config store: %v", err)
	}
	if err := cs.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{{Model: "gpt-4o", Provider: "openai", Mode: "chat"}}); err != nil {
		t.Fatalf("failed to seed pricing: %v", err)
	}
	if err := cs.UpsertModelParametersBatch(ctx, []tables.TableModelParameters{{Model: "gpt-4o", Data: "{}"}}); err != nil {
		t.Fatalf("failed to seed model parameters: %v", err)
	}

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	store := New(cs, logger, Config{URL: hang.URL, ModelParametersURL: hang.URL})

	// Mirrors a caller-level deadline like force sync's; the fetch must not consume all of it.
	syncCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := store.SyncFromURL(syncCtx); err != nil {
		t.Fatalf("SyncFromURL should fall back to DB records, got: %v", err)
	}
	if err := store.SyncModelParamsFromURL(syncCtx); err != nil {
		t.Fatalf("SyncModelParamsFromURL should fall back to DB records, got: %v", err)
	}
}

// TestSyncErrorKeepsURLCause checks that a hung URL with an empty DB reports
// the failing request, not just "context deadline exceeded".
func TestSyncErrorKeepsURLCause(t *testing.T) {
	orig := pricingFetchTimeout
	pricingFetchTimeout = 500 * time.Millisecond
	t.Cleanup(func() { pricingFetchTimeout = orig })

	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	store := New(nil, bifrost.NewDefaultLogger(schemas.LogLevelError), Config{URL: hang.URL})

	err := store.SyncFromURL(context.Background())
	if err == nil {
		t.Fatal("expected an error for a hung pricing URL with no DB data")
	}
	if !strings.Contains(err.Error(), "failed to download pricing data") {
		t.Fatalf("error should carry the URL failure, got: %v", err)
	}
}

// TestLoadFromURLRefusesRedirectToLinkLocal: the entry URL passes
// ValidateExternalURL (it is a reachable loopback server), but its 302 points
// at the cloud metadata address. The redirect hop must be refused and the
// error must say so; the datasheet must never be read from there.
func TestLoadFromURLRefusesRedirectToLinkLocal(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer redirect.Close()
	store := New(nil, bifrost.NewDefaultLogger(schemas.LogLevelError), Config{URL: redirect.URL, ModelParametersURL: redirect.URL})

	_, err := store.loadPricingFromURL(context.Background())
	if err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("pricing: expected link-local refusal, got %v", err)
	}
	_, err = store.loadModelParametersFromURL(context.Background())
	if err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("model parameters: expected link-local refusal, got %v", err)
	}
}

// Outside tests a catalog fetch is the guarded client, policy on every hop.
func TestNewFetchClientIsTheGuardedClient(t *testing.T) {
	guarded := network.NewPrivateNetworkHTTPClient(time.Second)
	client := NewFetchClient(time.Second)
	if reflect.TypeOf(client.Transport) != reflect.TypeOf(guarded.Transport) {
		t.Fatalf("transport = %T, want the guarded %T", client.Transport, guarded.Transport)
	}
	if client.CheckRedirect == nil {
		t.Fatal("the fetch client lost its redirect policy")
	}
	if _, err := client.Get("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Fatal("the fetch client reached a metadata address")
	}
}

type recordingTransport struct{ hits atomic.Int32 }

func (rt *recordingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	rt.hits.Add(1)
	return nil, errors.New("recorded")
}

type fixedLookup struct{ hits atomic.Int32 }

func (l *fixedLookup) LookupIP(context.Context, string, string) ([]net.IP, error) {
	l.hits.Add(1)
	return []net.IP{net.ParseIP("203.0.113.10")}, nil
}

// The seam covers both places a fetch touches the network, applies to clients built
// after it is set, keeps the redirect policy and validation, and restores cleanly.
func TestSetFetchSeamForTestWrapsAndRestores(t *testing.T) {
	recorder, lookup := &recordingTransport{}, &fixedLookup{}
	restore := SetFetchSeamForTest(func(http.RoundTripper) http.RoundTripper { return recorder }, lookup)
	if err := ValidateFetchURL("https://catalog.example/feed"); err != nil || lookup.hits.Load() != 1 {
		t.Fatalf("validation did not use the seam's resolver: err=%v lookups=%d", err, lookup.hits.Load())
	}
	if err := ValidateFetchURL("ftp://catalog.example/feed"); err == nil {
		t.Fatal("the seam skipped the URL policy")
	}
	client := NewFetchClient(time.Second)
	if client.CheckRedirect == nil {
		t.Fatal("the seam dropped the redirect policy")
	}
	if _, err := client.Get("https://example.com/"); err == nil || recorder.hits.Load() != 1 {
		t.Fatalf("the wrapped transport was not used: err=%v hits=%d", err, recorder.hits.Load())
	}
	restore()
	if activeFetchTestSeam.Load() != nil {
		t.Fatal("restore left the seam installed")
	}
	if _, ok := NewFetchClient(time.Second).Transport.(*recordingTransport); ok {
		t.Fatal("a client built after restore still uses the test transport")
	}
}

// A nil wrap clears the seam rather than panicking on the next fetch.
func TestSetFetchSeamForTestNilClearsTheSeam(t *testing.T) {
	restore := SetFetchSeamForTest(nil, nil)
	t.Cleanup(restore)
	if activeFetchTestSeam.Load() != nil {
		t.Fatal("a nil wrap left the seam installed")
	}
	if client := NewFetchClient(time.Second); client.CheckRedirect == nil {
		t.Fatal("the fetch client lost its redirect policy")
	}
}
