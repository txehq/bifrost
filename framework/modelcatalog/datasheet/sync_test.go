package datasheet

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capsohq/bifrost/framework/internal/testtime"

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
	pricingFetchTimeout, paramsFetchTimeout = testtime.Scale(500*time.Millisecond), testtime.Scale(500*time.Millisecond)
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
	syncCtx, cancel := context.WithTimeout(ctx, testtime.Scale(3*time.Second))
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
	pricingFetchTimeout = testtime.Scale(500 * time.Millisecond)
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
	guarded := network.NewPrivateNetworkHTTPClient(testtime.Scale(time.Second))
	client := NewFetchClient(testtime.Scale(time.Second))
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
	client := NewFetchClient(testtime.Scale(time.Second))
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
	if _, ok := NewFetchClient(testtime.Scale(time.Second)).Transport.(*recordingTransport); ok {
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
	if client := NewFetchClient(testtime.Scale(time.Second)); client.CheckRedirect == nil {
		t.Fatal("the fetch client lost its redirect policy")
	}
}

// upstreamCostKeysSnapshot is testdata/upstream_cost_keys.json: every cost key the upstream
// datasheet publishes, each with one model that carries it.
type upstreamCostKeysSnapshot struct {
	Source  string            `json:"source"`
	Fetched string            `json:"fetched"`
	Keys    map[string]string `json:"keys"`
}

const upstreamCostKeysSnapshotPath = "testdata/upstream_cost_keys.json"

// TestEveryUpstreamCostKeyIsParsedOrIgnored is the class check for datasheet keys the fork drops
// (fork, TXE-7306). The fork reads a datasheet key only through a json tag on Entry. A key with
// no tag is dropped at sync without a word, and every request in its class is priced at a
// fallback rate or not at all. That is how the video audio rates, published upstream, never
// priced a job (TXE-7306), and how Veo 4K billed at the 720p rate (TXE-7349).
//
// The test reads the committed snapshot of upstream's cost keys. Each one must be parsed (a json
// tag on Entry) or listed in ignoredUpstreamCostKeys with the ticket or reason. A parsed key, or a
// key upstream no longer publishes, must leave the list.
//
// Regenerate the snapshot after upstream adds keys, from a datasheet downloaded by hand
// (tests run offline):
//
//	curl -sSL -o /tmp/datasheet.json https://getbifrost.ai/datasheet
//	UPSTREAM_DATASHEET_FILE=/tmp/datasheet.json go test ./modelcatalog/datasheet/ -run TestEveryUpstreamCostKeyIsParsedOrIgnored
//
// It runs on every pull request in the Go unit tests (offline) job.
func TestEveryUpstreamCostKeyIsParsedOrIgnored(t *testing.T) {
	if source := os.Getenv("UPSTREAM_DATASHEET_FILE"); source != "" {
		writeUpstreamCostKeysSnapshot(t, source)
	}
	raw, err := os.ReadFile(upstreamCostKeysSnapshotPath)
	if err != nil {
		t.Fatalf("read %s: %v", upstreamCostKeysSnapshotPath, err)
	}
	var snapshot upstreamCostKeysSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode %s: %v", upstreamCostKeysSnapshotPath, err)
	}
	if len(snapshot.Keys) == 0 {
		t.Fatalf("%s lists no keys", upstreamCostKeysSnapshotPath)
	}

	parsed := entryJSONKeys()
	for _, key := range sortedKeys(snapshot.Keys) {
		reason, ignored := ignoredUpstreamCostKeys[key]
		switch {
		case parsed[key] && ignored:
			t.Errorf("%s is parsed now (a json tag on Entry) but still in ignoredUpstreamCostKeys (%q): remove the entry", key, reason)
		case !parsed[key] && !ignored:
			t.Errorf("upstream publishes %s (for example on %s), which the fork neither parses nor ignores: it is dropped at sync and never prices a request. Parse it through the add-pricing-field path, or add it to ignoredUpstreamCostKeys with the ticket or reason", key, snapshot.Keys[key])
		case ignored && strings.TrimSpace(reason) == "":
			t.Errorf("%s is in ignoredUpstreamCostKeys without a reason", key)
		}
	}
	for _, key := range sortedKeys(ignoredUpstreamCostKeys) {
		if _, published := snapshot.Keys[key]; !published {
			t.Errorf("%s is in ignoredUpstreamCostKeys, but upstream no longer publishes it: remove the entry", key)
		}
	}
}

// entryJSONKeys is every datasheet key Entry decodes: the json tags of Entry and of the structs it
// embeds (Options).
func entryJSONKeys() map[string]bool {
	keys := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				walk(field.Type)
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				keys[name] = true
			}
		}
	}
	walk(reflect.TypeOf(Entry{}))
	for _, key := range entryUnmarshalOnlyKeys {
		keys[key] = true
	}
	return keys
}

// entryUnmarshalOnlyKeys are upstream keys Entry.UnmarshalJSON decodes into other fields, so they
// have no json tag of their own. guardrail_cost_per_unit (TXE-7365) is a map spread into the
// GuardrailCostPer* fields; TestEntryUnmarshal_GuardrailCostPerUnitSpreadsIntoUnitRates pins it.
var entryUnmarshalOnlyKeys = []string{"guardrail_cost_per_unit"}

// writeUpstreamCostKeysSnapshot rewrites the snapshot from a downloaded datasheet: every key whose
// name holds "cost", with the alphabetically first model that carries it.
func writeUpstreamCostKeysSnapshot(t *testing.T, source string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	var datasheet map[string]json.RawMessage
	if err := json.Unmarshal(raw, &datasheet); err != nil {
		t.Fatalf("decode %s: %v", source, err)
	}
	keys := map[string]string{}
	for _, model := range sortedKeys(datasheet) {
		var fields map[string]json.RawMessage
		if json.Unmarshal(datasheet[model], &fields) != nil {
			continue
		}
		for field := range fields {
			if _, seen := keys[field]; !seen && strings.Contains(field, "cost") {
				keys[field] = model
			}
		}
	}
	out, err := json.MarshalIndent(upstreamCostKeysSnapshot{
		Source:  "https://getbifrost.ai/datasheet",
		Fetched: os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Keys:    keys,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(upstreamCostKeysSnapshotPath), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d upstream cost keys to %s", len(keys), upstreamCostKeysSnapshotPath)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ignoredUpstreamCostKeys are the upstream cost keys the fork does not parse yet, each with the
// ticket that decides it. TXE-7350 owns every key here that names it: each family is parsed or
// recorded as deliberately ignored with its reason, and its entries leave this list.
var ignoredUpstreamCostKeys = map[string]string{
	// Batch tiers.
	"annotation_cost_per_page_batches":                     "TXE-7354: deliberately ignored: the fork has no Mistral batch API, so an OCR batch cannot be created and these rates price nothing",
	"cache_read_input_image_token_cost_batches":            "TXE-7358: cached image tokens",
	"input_cost_per_audio_token_above_200k_tokens_batches": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"ocr_cost_per_page_batches":                            "TXE-7354: deliberately ignored: the fork has no Mistral batch API, so an OCR batch cannot be created and these rates price nothing",
	// Flex, priority, fast, ultrafast, decisions and balanced tiers.
	"cache_read_input_token_cost_balanced":                  "TXE-7353: deliberately ignored: upstream publishes _balanced rates only for sail/*, which is not a fork provider, so no request the fork serves can reach them",
	"input_cost_per_audio_token_above_200k_tokens_flex": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"input_cost_per_audio_token_above_200k_tokens_priority": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"input_cost_per_token_balanced":                         "TXE-7353: deliberately ignored: upstream publishes _balanced rates only for sail/*, which is not a fork provider, so no request the fork serves can reach them",
	"output_cost_per_token_balanced":                        "TXE-7353: deliberately ignored: upstream publishes _balanced rates only for sail/*, which is not a fork provider, so no request the fork serves can reach them",
	// 12-hour cache write.
	// Databricks DBU costs.
	"cache_creation_input_token_dbu_cost": "TXE-7350: Databricks DBU",
	"cache_read_input_token_dbu_cost":     "TXE-7350: Databricks DBU",
	"input_dbu_cost_per_token":            "TXE-7350: Databricks DBU",
	"output_dbu_cost_per_token":           "TXE-7350: Databricks DBU",
	// Long-context thresholds the fork has no tier for.
	"input_cost_per_audio_token_above_200k_tokens": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"input_cost_per_character_above_128k_tokens": "TXE-7355: deliberately ignored: published only for two free Gemini models (gemma-3-27b-it, learnlm-1.5-pro-experimental), priced 0",
	"output_cost_per_character_above_128k_tokens": "TXE-7355: deliberately ignored: published only for two free Gemini models (gemma-3-27b-it, learnlm-1.5-pro-experimental), priced 0",
	// Everything else.
	"citation_cost_per_token":              "TXE-7350: other",
	"cost_per_second":                      "TXE-7350: other",
	"explicit_cache_read_input_token_cost": "TXE-7350: other",
	"input_cost_per_request":               "TXE-7350: other",
	"input_cost_per_token_cache_hit":       "TXE-7350: other",
	"ocr_cost_per_credit":                  "TXE-7350: other",
	"output_cost_per_billable_unit":        "TXE-7350: other",
	"output_cost_per_character":            "TXE-7350: other",
	"output_cost_per_reasoning_token":      "TXE-7350: other",
	"output_cost_per_unit":                 "TXE-7350: other",
}
