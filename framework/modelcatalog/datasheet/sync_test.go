package datasheet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	"github.com/stretchr/testify/require"
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
	// ProviderSpecificKeys is every key of upstream's provider_specific_entry objects, each with
	// one model that carries it (fork, TXE-7409). MiniMax's free input images are published only
	// there, and were dropped at sync.
	ProviderSpecificKeys map[string]string `json:"provider_specific_keys"`
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

	// The same check for the keys of provider_specific_entry (TXE-7409): each is read into an
	// Options field (a json tag on providerSpecificEntry) or ignored with its reason.
	if len(snapshot.ProviderSpecificKeys) == 0 {
		t.Fatalf("%s lists no provider_specific_entry keys", upstreamCostKeysSnapshotPath)
	}
	parsedSpecific := jsonTags(reflect.TypeOf(providerSpecificEntry{}))
	for _, key := range sortedKeys(snapshot.ProviderSpecificKeys) {
		reason, ignored := ignoredUpstreamProviderSpecificKeys[key]
		switch {
		case parsedSpecific[key] && ignored:
			t.Errorf("provider_specific_entry.%s is parsed now but still in ignoredUpstreamProviderSpecificKeys (%q): remove the entry", key, reason)
		case !parsedSpecific[key] && !ignored:
			t.Errorf("upstream publishes provider_specific_entry.%s (for example on %s), which the fork neither parses nor ignores: a price there is dropped at sync. Read it in Entry.UnmarshalJSON through providerSpecificEntry, or add it to ignoredUpstreamProviderSpecificKeys with the ticket or reason", key, snapshot.ProviderSpecificKeys[key])
		case ignored && strings.TrimSpace(reason) == "":
			t.Errorf("provider_specific_entry.%s is in ignoredUpstreamProviderSpecificKeys without a reason", key)
		}
	}
	for _, key := range sortedKeys(ignoredUpstreamProviderSpecificKeys) {
		if _, published := snapshot.ProviderSpecificKeys[key]; !published {
			t.Errorf("provider_specific_entry.%s is in ignoredUpstreamProviderSpecificKeys, but upstream no longer publishes it: remove the entry", key)
		}
	}
	for key := range parsedSpecific {
		if _, published := snapshot.ProviderSpecificKeys[key]; !published {
			t.Errorf("providerSpecificEntry reads provider_specific_entry.%s, which upstream no longer publishes", key)
		}
	}
}

// jsonTags is the json tag names of a struct type's fields.
func jsonTags(typ reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// ignoredUpstreamProviderSpecificKeys are the provider_specific_entry keys the fork does not read,
// each with its reason or the ticket that decides it (TXE-7409).
var ignoredUpstreamProviderSpecificKeys = map[string]string{
	"realtime_ga_only":                "not a price: marks Azure realtime models served only on the GA API",
	"bedrock_invocation_schema":       "not a price: the request schema of a Bedrock Titan embedding model",
	"dashscope_video_reference_input": "not a price: how a DashScope video model takes its reference image",
	"volcengine_video_frames":         "not a price: the frame size of each Volcengine resolution and aspect ratio",
	"transcription_json_or_text_only": "not a price: the response formats a transcription model returns",
	"transcription_usage_duration":    "not a price: that a transcription model reports its usage as a duration",
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
	specific := map[string]string{}
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
		var entry map[string]json.RawMessage
		if json.Unmarshal(fields["provider_specific_entry"], &entry) == nil {
			for field := range entry {
				if _, seen := specific[field]; !seen {
					specific[field] = model
				}
			}
		}
	}
	out, err := json.MarshalIndent(upstreamCostKeysSnapshot{
		Source:               "https://getbifrost.ai/datasheet",
		Fetched:              os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Keys:                 keys,
		ProviderSpecificKeys: specific,
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
// dbuRestatesUSD is the reason the Databricks DBU keys are not parsed (TXE-7359).
const dbuRestatesUSD = "TXE-7359: deliberately ignored: every Databricks row that publishes a DBU rate publishes the same price in USD, which the fork prices from"

var ignoredUpstreamCostKeys = map[string]string{
	// Batch tiers.
	"annotation_cost_per_page_batches":                     "TXE-7354: deliberately ignored: the fork has no Mistral batch API, so an OCR batch cannot be created and these rates price nothing",
	"cache_read_input_image_token_cost_batches":            "TXE-7358: cached image tokens",
	"input_cost_per_audio_token_above_200k_tokens_batches": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"ocr_cost_per_page_batches":                            "TXE-7354: deliberately ignored: the fork has no Mistral batch API, so an OCR batch cannot be created and these rates price nothing",
	// Flex, priority, fast, ultrafast, decisions and balanced tiers.
	"cache_read_input_token_cost_balanced":                  "TXE-7353: deliberately ignored: upstream publishes _balanced rates only for sail/*, which is not a fork provider, so no request the fork serves can reach them",
	"input_cost_per_audio_token_above_200k_tokens_flex":     "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"input_cost_per_audio_token_above_200k_tokens_priority": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"input_cost_per_token_balanced":                         "TXE-7353: deliberately ignored: upstream publishes _balanced rates only for sail/*, which is not a fork provider, so no request the fork serves can reach them",
	"output_cost_per_token_balanced":                        "TXE-7353: deliberately ignored: upstream publishes _balanced rates only for sail/*, which is not a fork provider, so no request the fork serves can reach them",
	// 12-hour cache write.
	// Databricks DBU costs: deliberately ignored (TXE-7359). Every one of the 83 Databricks rows that
	// carries a DBU rate also carries the same price in USD (input_cost_per_token and its siblings),
	// which the fork already prices from: databricks-claude-fable-5's 0.00014286 DBU/token at
	// $0.07/DBU is its 1e-05 USD/token. The DBU keys restate the price in another unit.
	"cache_creation_input_token_dbu_cost": dbuRestatesUSD,
	"cache_read_input_token_dbu_cost":     dbuRestatesUSD,
	"input_dbu_cost_per_token":            dbuRestatesUSD,
	"output_dbu_cost_per_token":           dbuRestatesUSD,
	// Long-context thresholds the fork has no tier for.
	"input_cost_per_audio_token_above_200k_tokens": "TXE-7355: deliberately ignored: every row publishing a 200k audio tier repeats its non-200k audio rate, except gemini-2.5-flash standard (3e-07 against Google flat 1e-06), an upstream error that would underbill",
	"input_cost_per_character_above_128k_tokens":   "TXE-7355: deliberately ignored: published only for two free Gemini models (gemma-3-27b-it, learnlm-1.5-pro-experimental), priced 0",
	"output_cost_per_character_above_128k_tokens":  "TXE-7355: deliberately ignored: published only for two free Gemini models (gemma-3-27b-it, learnlm-1.5-pro-experimental), priced 0",
	// Everything else.
	"citation_cost_per_token":              "TXE-7358: deliberately ignored: only perplexity/sonar-deep-research publishes it, and Perplexity reports the request's own cost (citation_tokens_cost included), which the fork bills as the provider-computed cost",
	"cost_per_second":                      "TXE-7358: deliberately ignored: all 28 rows are Bedrock provisioned-throughput commitments, billed per model-unit hour whether or not they serve traffic, not per request",
	"explicit_cache_read_input_token_cost": "TXE-7358: deliberately ignored: published only for Qwen explicit-cache reads on dashscope, qwencloud, qwen_ai_platform and streamlake rows; DashScope chat is unsupported and the other three match no fork provider",
	"ocr_cost_per_credit":                  "TXE-7358: deliberately ignored: only reducto rows publish it, and the fork has no Reducto provider",
	"output_cost_per_billable_unit":        "TXE-7358: deliberately ignored: only fal_ai nano-banana-2.1 rows publish it, and the fork has no fal provider",
	"output_cost_per_character":            "TXE-7358: deliberately ignored: the two Gemini rows are 0, and medlm-large and medlm-medium are PaLM-era Vertex models served on the :predict text API, which the fork's Vertex chat does not call",
	"output_cost_per_reasoning_token":      "TXE-7358: deliberately ignored: equal to output_cost_per_token on every reachable row; it differs only on DashScope, qwencloud and qwen_ai_platform Qwen rows (no fork chat route), perplexity/sonar-deep-research (provider-computed cost), cerebras/qwen-3-235b-a22b-instruct-2507 (0; an instruct model that emits no reasoning tokens), and claude-opus-4-8 and openai/gpt-5.4-pro, whose values (5e-07 and 0) are upstream errors: both vendors bill reasoning as output tokens",
	"output_cost_per_unit":                 "TXE-7358: deliberately ignored: only replicate/tencent/hunyuan-3d-3.1 publishes it, in mode 3d, which no fork request type resolves",
}

// dbuToUSD pairs each Databricks DBU cost key with the USD key that states the same rate. The fork
// prices Databricks rows from the USD keys and leaves the DBU keys unparsed (dbuRestatesUSD,
// TXE-7359), which is safe only while every DBU rate has its USD counterpart at the same price.
var dbuToUSD = map[string]string{
	"input_dbu_cost_per_token":            "input_cost_per_token",
	"output_dbu_cost_per_token":           "output_cost_per_token",
	"cache_read_input_token_dbu_cost":     "cache_read_input_token_cost",
	"cache_creation_input_token_dbu_cost": "cache_creation_input_token_cost",
}

// usdPerDBU is the list price the datasheet's Databricks rows imply: 226 of the 230 DBU rates in
// the 17 Nov 2026 datasheet equal their USD rate at $0.07 per DBU (databricks-claude-fable-5's
// 0.00014286 DBU per input token is its 1e-05 USD), Databricks' published pay-per-token rate.
const usdPerDBU = 0.07

// dbuUSDDisagreements are the rates whose USD key, after upstreamRateCorrections, disagrees with
// DBU x usdPerDBU in the snapshot, keyed "row|dbu key", each with what was found. The fork bills
// the USD key. A new disagreement fails the test, and one that agrees again must leave the list.
// A USD rate that is wrong is corrected in upstreamRateCorrections, and then agrees.
var dbuUSDDisagreements = map[string]string{
	"databricks/databricks-gemini-3-6-flash|cache_read_input_token_dbu_cost":     "the DBU key is wrong and the USD key is right (TXE-7688). https://www.databricks.com/product/pricing/proprietary-foundation-model-serving, read 18 Nov 2026, lists Gemini 3.6 Flash cache reads at 1.071 DBU per 1M, 7.497e-08 USD at $0.07, which is the USD key. The DBU key's 2.679e-06 is Gemini 3.5 Flash's cache-read rate",
	"databricks/databricks-gemini-3-6-flash|cache_creation_input_token_dbu_cost": "the DBU key is wrong and the USD key is right (TXE-7688). The USD key is 1.0x the row's input rate, as on every other Databricks Gemini row. The DBU key's 2.6786e-05 is Gemini 3.5 Flash's input rate (26.786 DBU per 1M on the same page, read 18 Nov 2026)",
}

// correctedUSD is the USD rate the fork bills for row's usdKey when upstream publishes usd: the
// correction's value while upstreamRateCorrections applies to it, otherwise usd.
func correctedUSD(row, usdKey string, usd float64) float64 {
	for _, c := range upstreamRateCorrections {
		if c.row == row && c.key == usdKey && c.upstream == usd {
			return c.corrected
		}
	}
	return usd
}

// upstreamDBURowsSnapshot is testdata/upstream_dbu_rows.json: for every upstream row that carries a
// DBU cost key, its DBU keys and their USD counterparts, verbatim.
type upstreamDBURowsSnapshot struct {
	Source  string                         `json:"source"`
	Fetched string                         `json:"fetched"`
	Rows    map[string]map[string]*float64 `json:"rows"`
}

const upstreamDBURowsSnapshotPath = "testdata/upstream_dbu_rows.json"

// TestEveryDBURateHasItsUSDRate is the guard for TXE-7359's decision to leave the DBU keys
// unparsed (TXE-7687). It reads the committed snapshot of the upstream rows that carry a DBU cost
// key and fails when a DBU rate has no USD counterpart, which the fork would bill at $0, or when
// the two disagree at usdPerDBU and the rate is not in dbuUSDDisagreements.
//
// Regenerate the snapshot from a datasheet downloaded by hand (tests run offline); the same
// variables regenerate the other upstream snapshots:
//
//	curl -sSL -o /tmp/datasheet.json https://getbifrost.ai/datasheet
//	UPSTREAM_DATASHEET_FILE=/tmp/datasheet.json UPSTREAM_DATASHEET_FETCHED=YYYY-MM-DD go test ./modelcatalog/datasheet/ -run 'TestEveryDBURateHasItsUSDRate|TestEveryTokenPricedImageRowHasAnEstimate|TestEveryUpstreamCostKeyIsParsedOrIgnored'
//
// It runs on every pull request in the Go unit tests (offline) job.
func TestEveryDBURateHasItsUSDRate(t *testing.T) {
	if source := os.Getenv("UPSTREAM_DATASHEET_FILE"); source != "" {
		writeUpstreamDBURowsSnapshot(t, source)
	}
	raw, err := os.ReadFile(upstreamDBURowsSnapshotPath)
	require.NoError(t, err)
	var snapshot upstreamDBURowsSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.NotEmpty(t, snapshot.Rows, "%s lists no rows", upstreamDBURowsSnapshotPath)

	disagreeing := map[string]bool{}
	for _, row := range sortedKeys(snapshot.Rows) {
		fields := snapshot.Rows[row]
		for _, dbuKey := range sortedKeys(dbuToUSD) {
			dbu, hasDBU := fields[dbuKey]
			if !hasDBU || dbu == nil {
				continue
			}
			usdKey := dbuToUSD[dbuKey]
			usd, hasUSD := fields[usdKey]
			if !hasUSD || usd == nil {
				t.Errorf("%s publishes %s (%g DBU) without %s: the fork prices Databricks rows from the USD key, so this rate bills $0. Parse the DBU key through the add-pricing-field path, or fix the row", row, dbuKey, *dbu, usdKey)
				continue
			}
			id := row + "|" + dbuKey
			want := *dbu * usdPerDBU
			billed := correctedUSD(row, usdKey, *usd)
			agrees := dbuAgrees(*dbu, billed)
			reason, known := dbuUSDDisagreements[id]
			switch {
			case !agrees && !known:
				t.Errorf("%s: the fork bills %s at %g (upstream publishes %g), but %s is %g DBU, %g USD at $%g/DBU. Find which is right: a wrong USD rate goes in upstreamRateCorrections, a wrong DBU rate in dbuUSDDisagreements, each with the evidence", row, usdKey, billed, *usd, dbuKey, *dbu, want, usdPerDBU)
			case !agrees && strings.TrimSpace(reason) == "":
				t.Errorf("%s is in dbuUSDDisagreements without a reason", id)
			case agrees && known:
				t.Errorf("%s is in dbuUSDDisagreements (%q), but its USD rate now agrees with its DBU rate: remove the entry", id, reason)
			}
			if !agrees {
				disagreeing[id] = true
			}
		}
	}
	for _, id := range sortedKeys(dbuUSDDisagreements) {
		if !disagreeing[id] {
			if _, published := snapshot.Rows[strings.SplitN(id, "|", 2)[0]]; !published {
				t.Errorf("%s is in dbuUSDDisagreements, but upstream no longer publishes the row: remove the entry", id)
			}
		}
	}
}

// dbuAgrees reports whether a USD rate equals a DBU rate at usdPerDBU, within 1% (the datasheet
// rounds DBU rates to five significant figures).
func dbuAgrees(dbu, usd float64) bool {
	want := dbu * usdPerDBU
	return math.Abs(usd-want) <= 0.01*math.Max(math.Abs(want), math.Abs(usd))
}

// TestUpstreamRateCorrectionsStillMatchUpstream is the check that takes a correction out once
// upstream fixes its row (TXE-7688). It reads the same committed snapshot as
// TestEveryDBURateHasItsUSDRate and fails when a correction's row is gone or no longer publishes
// the value the correction replaces: the correction then does nothing at runtime, so it must be
// removed. It also fails when a correction's key does not name the field it rewrites, when it has
// no evidence, or, for a Databricks row with a DBU rate, when the corrected value disagrees with it.
func TestUpstreamRateCorrectionsStillMatchUpstream(t *testing.T) {
	raw, err := os.ReadFile(upstreamDBURowsSnapshotPath)
	require.NoError(t, err)
	var snapshot upstreamDBURowsSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	usdToDBU := map[string]string{}
	for dbuKey, usdKey := range dbuToUSD {
		usdToDBU[usdKey] = dbuKey
	}

	seen := map[string]bool{}
	for _, c := range upstreamRateCorrections {
		id := c.row + " " + c.key
		if seen[id] {
			t.Errorf("%s is corrected twice in upstreamRateCorrections", id)
		}
		seen[id] = true
		if strings.TrimSpace(c.evidence) == "" {
			t.Errorf("%s has no evidence in upstreamRateCorrections", id)
		}

		// The key must be the JSON name of the field the correction rewrites.
		var entry Entry
		value := c.upstream
		*c.field(&entry) = &value
		encoded, err := json.Marshal(entry)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(encoded, &fields))
		if _, ok := fields[c.key]; !ok {
			t.Errorf("%s: field does not decode %s (the Entry it sets encodes as %s)", id, c.key, encoded)
		}

		row, published := snapshot.Rows[c.row]
		if !published {
			t.Errorf("%s: upstream no longer publishes %s (snapshot fetched %s): remove the correction", id, c.row, snapshot.Fetched)
			continue
		}
		got, has := row[c.key]
		switch {
		case !has || got == nil:
			t.Errorf("%s: upstream no longer publishes %s on the row (snapshot fetched %s): remove the correction", id, c.key, snapshot.Fetched)
		case *got != c.upstream:
			t.Errorf("%s: upstream now publishes %g, not the %g this corrects (snapshot fetched %s), so the correction no longer applies: remove it, and check %g against its evidence", id, *got, c.upstream, snapshot.Fetched, *got)
		}
		if dbu := row[usdToDBU[c.key]]; dbu != nil && !dbuAgrees(*dbu, c.corrected) {
			t.Errorf("%s: the corrected %g disagrees with the row's %s of %g DBU (%g USD at $%g/DBU)", id, c.corrected, usdToDBU[c.key], *dbu, *dbu*usdPerDBU, usdPerDBU)
		}
	}
}

// TestUpstreamRateCorrectionsApplyOnlyToTheValueTheyCorrect loads a datasheet the way a sync does
// and reads the rates back from the store (TXE-7688). The two databricks-gemini-3-pro rates bill at
// the corrected list price while upstream publishes the promotional value. A row that publishes
// any other value, as upstream will once it fixes the row or as a hand-written file:// datasheet
// may, is billed at that value. The row's other rates, and other rows, are untouched.
func TestUpstreamRateCorrectionsApplyOnlyToTheValueTheyCorrect(t *testing.T) {
	logger := bifrost.NewDefaultLogger(schemas.LogLevelWarn)
	load := func(t *testing.T, rows map[string]any) *Store {
		t.Helper()
		raw, err := json.Marshal(rows)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "pricing.json")
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		store := New(nil, logger, Config{URL: "file://" + path})
		require.NoError(t, store.LoadFromURLIntoMemory(context.Background()))
		return store
	}
	// databricks/databricks-gemini-3-pro as the datasheet published it on 17 Nov 2026.
	upstreamRow := func(input, output float64) map[string]any {
		return map[string]any{
			"provider": "databricks", "mode": "chat",
			"input_cost_per_token": input, "output_cost_per_token": output,
			"input_dbu_cost_per_token": 3.5714e-05, "output_dbu_cost_per_token": 0.000214286,
			"cache_read_input_token_cost": 2.4997e-07, "cache_creation_input_token_cost": 2.49998e-06,
		}
	}
	rate := func(t *testing.T, store *Store, model string, provider schemas.ModelProvider) (input, output, cacheRead float64) {
		t.Helper()
		row := store.Get(model, provider, schemas.ChatCompletionRequest)
		require.NotNil(t, row, "%s/%s has no chat pricing row", provider, model)
		require.NotNil(t, row.InputCostPerToken)
		require.NotNil(t, row.OutputCostPerToken)
		require.NotNil(t, row.CacheReadInputTokenCost)
		return *row.InputCostPerToken, *row.OutputCostPerToken, *row.CacheReadInputTokenCost
	}

	t.Run("the upstream promotional rates are corrected to the list rates", func(t *testing.T) {
		store := load(t, map[string]any{
			"databricks/databricks-gemini-3-pro": upstreamRow(2e-06, 1.2e-05),
			// Another provider's row with the same model name and the same rates is not Databricks'.
			"vertex/databricks-gemini-3-pro": map[string]any{"provider": "vertex", "mode": "chat", "input_cost_per_token": 2e-06, "output_cost_per_token": 1.2e-05, "cache_read_input_token_cost": 2e-07},
		})
		input, output, cacheRead := rate(t, store, "databricks-gemini-3-pro", schemas.Databricks)
		require.Equal(t, 2.49998e-06, input)
		require.Equal(t, 1.500002e-05, output)
		require.Equal(t, 2.4997e-07, cacheRead, "the row's cache rate is not corrected")
		input, output, _ = rate(t, store, "databricks-gemini-3-pro", schemas.Vertex)
		require.Equal(t, 2e-06, input)
		require.Equal(t, 1.2e-05, output)
	})

	t.Run("a row that publishes any other value keeps it", func(t *testing.T) {
		store := load(t, map[string]any{"databricks/databricks-gemini-3-pro": upstreamRow(2.5e-06, 1.3e-05)})
		input, output, _ := rate(t, store, "databricks-gemini-3-pro", schemas.Databricks)
		require.Equal(t, 2.5e-06, input)
		require.Equal(t, 1.3e-05, output)
	})
}

// upstreamPromotionRowsSnapshot is testdata/upstream_promotion_rows.json: every cost key (DBU keys
// aside) of every row upstreamPromotions and listBilledPromotions record, verbatim.
type upstreamPromotionRowsSnapshot struct {
	Source  string                        `json:"source"`
	Fetched string                        `json:"fetched"`
	Rows    map[string]map[string]float64 `json:"rows"`
}

const upstreamPromotionRowsSnapshotPath = "testdata/upstream_promotion_rows.json"

func readUpstreamPromotionRowsSnapshot(t *testing.T) upstreamPromotionRowsSnapshot {
	t.Helper()
	if source := os.Getenv("UPSTREAM_DATASHEET_FILE"); source != "" {
		writeUpstreamPromotionRowsSnapshot(t, source)
	}
	raw, err := os.ReadFile(upstreamPromotionRowsSnapshotPath)
	require.NoError(t, err)
	var snapshot upstreamPromotionRowsSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.NotEmpty(t, snapshot.Rows, "%s lists no rows", upstreamPromotionRowsSnapshotPath)
	return snapshot
}

func writeUpstreamPromotionRowsSnapshot(t *testing.T, source string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	var datasheet map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &datasheet))
	wanted := map[string]bool{}
	for _, p := range upstreamPromotions {
		for row := range p.rates {
			wanted[row] = true
		}
	}
	for _, p := range listBilledPromotions {
		for row := range p.rows {
			wanted[row] = true
		}
	}
	rows := map[string]map[string]float64{}
	for row := range wanted {
		var fields map[string]any
		if datasheet[row] == nil || json.Unmarshal(datasheet[row], &fields) != nil {
			continue
		}
		kept := map[string]float64{}
		for key, value := range fields {
			if v, ok := value.(float64); ok && strings.Contains(key, "cost") && !strings.Contains(key, "dbu") {
				kept[key] = v
			}
		}
		rows[row] = kept
	}
	out, err := json.MarshalIndent(upstreamPromotionRowsSnapshot{
		Source:  "https://getbifrost.ai/datasheet",
		Fetched: os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Rows:    rows,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Clean(upstreamPromotionRowsSnapshotPath), append(out, '\n'), 0o644))
	t.Logf("wrote %d upstream promotion rows to %s", len(rows), upstreamPromotionRowsSnapshotPath)
}

// snapshotEntries decodes snapshot rows into datasheet entries, as a sync would.
func snapshotEntries(t *testing.T, rows map[string]map[string]float64) map[string]Entry {
	t.Helper()
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	var entries map[string]Entry
	require.NoError(t, json.Unmarshal(raw, &entries))
	return entries
}

// promotionsStillBilledAfterTheyEnd returns every recorded rate the fork would still bill at its
// promotional value on the day after its promotion ends, for each promotion ending within
// promotionWarningDays of now (or already ended). It runs the real switch over the snapshot rows,
// so a rate with no standard value, or a switch that does not apply, is reported before the first
// day the rate would underbill.
func promotionsStillBilledAfterTheyEnd(t *testing.T, rows map[string]map[string]float64, promotions []upstreamPromotion, now time.Time) []string {
	t.Helper()
	var due []string
	horizon := now.UTC().AddDate(0, 0, promotionWarningDays).Format(time.DateOnly)
	for _, p := range promotions {
		if p.lastDay > horizon {
			continue
		}
		last, err := time.Parse(time.DateOnly, p.lastDay)
		require.NoError(t, err, "%s: lastDay %q", p.name, p.lastDay)
		entries := snapshotEntries(t, rows)
		applyPromotions(entries, last.AddDate(0, 0, 1), []upstreamPromotion{p})
		for _, row := range sortedKeys(p.rates) {
			entry, ok := entries[row]
			if !ok {
				continue
			}
			for _, key := range sortedKeys(p.rates[row]) {
				rate := entryRateField(&entry, key)
				if rate != nil && *rate != nil && **rate == p.rates[row][key].promotional {
					due = append(due, fmt.Sprintf("%s: %s %s still bills the promotional %g after %s ends on %s", p.name, row, key, **rate, p.name, p.lastDay))
				}
			}
		}
	}
	return due
}

// TestEveryPromotionHasItsStandardRateBeforeItEnds is TXE-7763's guard: it fails from
// promotionWarningDays before a promotion's last day when the fork would still bill one of its
// rates at the promotional value once the promotion ends.
func TestEveryPromotionHasItsStandardRateBeforeItEnds(t *testing.T) {
	snapshot := readUpstreamPromotionRowsSnapshot(t)
	for _, line := range promotionsStillBilledAfterTheyEnd(t, snapshot.Rows, upstreamPromotions, promotionClock()) {
		t.Error(line + ": record its standard rate in upstreamPromotions")
	}

	// The same check, with a promotion whose standard rate is not recorded, at dates around its end.
	row := map[string]map[string]float64{"databricks/databricks-gemini-4-argon": {"input_cost_per_token": 2.49998e-06}}
	unknown := []upstreamPromotion{{
		name: "a promotion with no standard rate", lastDay: "2026-11-30", source: "test", read: "2026-11-18",
		rates: map[string]map[string]promotionalRate{"databricks/databricks-gemini-4-argon": {"input_cost_per_token": {promotional: 2.49998e-06}}},
	}}
	at := func(day string) time.Time {
		d, err := time.Parse(time.DateOnly, day)
		require.NoError(t, err)
		return d
	}
	require.Empty(t, promotionsStillBilledAfterTheyEnd(t, row, unknown, at("2026-11-15")), "15 days before the last day is outside the warning window")
	require.Len(t, promotionsStillBilledAfterTheyEnd(t, row, unknown, at("2026-11-16")), 1, "14 days before the last day is inside the warning window")
	require.Len(t, promotionsStillBilledAfterTheyEnd(t, row, unknown, at("2026-12-15")), 1, "a past end date with the promotional rate still billed fails")
	known := []upstreamPromotion{unknown[0]}
	known[0].rates = map[string]map[string]promotionalRate{"databricks/databricks-gemini-4-argon": {"input_cost_per_token": {2.49998e-06, 4.99996e-06}}}
	require.Empty(t, promotionsStillBilledAfterTheyEnd(t, row, known, at("2026-12-15")), "a promotion with its standard rate recorded is switched")
}

// TestUpstreamPromotionsMatchTheSnapshot keeps the record honest (TXE-7763). Every recorded row must
// be in the snapshot; every cost key on a promotion row must be recorded, and must publish either
// its promotional or its standard value (anything else means upstream changed the row and the
// record must be re-read against its source). A list-billed row must still publish its list rate.
func TestUpstreamPromotionsMatchTheSnapshot(t *testing.T) {
	snapshot := readUpstreamPromotionRowsSnapshot(t)
	corrected := map[string]bool{}
	for _, c := range upstreamRateCorrections {
		corrected[c.row] = true
	}
	recordedIn := map[string]string{}
	claim := func(row, name string) {
		if other, ok := recordedIn[row]; ok {
			t.Errorf("%s is recorded in both %q and %q", row, other, name)
		}
		recordedIn[row] = name
		if corrected[row] {
			t.Errorf("%s is in %q and in upstreamRateCorrections: record it in one place", row, name)
		}
	}
	checkHeader := func(name, lastDay, source, read string) {
		for _, day := range []string{lastDay, read} {
			if _, err := time.Parse(time.DateOnly, day); err != nil {
				t.Errorf("%s: %q is not a YYYY-MM-DD date", name, day)
			}
		}
		if strings.TrimSpace(source) == "" {
			t.Errorf("%s has no source", name)
		}
	}

	for _, p := range upstreamPromotions {
		checkHeader(p.name, p.lastDay, p.source, p.read)
		require.NotEmpty(t, p.rates, "%s records no rates", p.name)
		ended := promotionClock().Format(time.DateOnly) > p.lastDay
		allStandard := true
		for _, row := range sortedKeys(p.rates) {
			claim(row, p.name)
			published, ok := snapshot.Rows[row]
			if !ok {
				t.Errorf("%s: upstream no longer publishes %s (snapshot fetched %s): re-read %s and update the record", p.name, row, snapshot.Fetched, p.source)
				continue
			}
			for _, key := range sortedKeys(published) {
				if _, recorded := p.rates[row][key]; !recorded {
					t.Errorf("%s: %s publishes %s (%g), which the promotion does not record: record its promotional and standard values", p.name, row, key, published[key])
				}
			}
			for _, key := range sortedKeys(p.rates[row]) {
				r := p.rates[row][key]
				if entryRateField(&Entry{}, key) == nil {
					t.Errorf("%s: %s %s is not a cost field the fork parses", p.name, row, key)
				}
				if r.standard != 0 && r.standard == r.promotional {
					t.Errorf("%s: %s %s records the same promotional and standard value %g", p.name, row, key, r.standard)
				}
				value, has := published[key]
				switch {
				case !has:
					t.Errorf("%s: %s no longer publishes %s: remove it from the record", p.name, row, key)
				case value == r.promotional:
					allStandard = false
				case r.standard != 0 && value == r.standard:
				default:
					t.Errorf("%s: %s %s is %g upstream, neither the recorded promotional %g nor the standard %g: re-read %s", p.name, row, key, value, r.promotional, r.standard, p.source)
					allStandard = false
				}
			}
		}
		if ended && allStandard {
			t.Errorf("%s ended on %s and upstream publishes the standard rate on every row: remove the record", p.name, p.lastDay)
		}
	}

	for _, p := range listBilledPromotions {
		checkHeader(p.name, p.lastDay, p.source, p.read)
		require.NotEmpty(t, p.rows, "%s records no rows", p.name)
		for _, row := range sortedKeys(p.rows) {
			claim(row, p.name)
			value, ok := snapshot.Rows[row]["input_cost_per_token"]
			switch {
			case !ok:
				t.Errorf("%s: upstream no longer publishes %s input_cost_per_token: re-read %s", p.name, row, p.source)
			case value != p.rows[row]:
				t.Errorf("%s: %s input_cost_per_token is %g upstream, not the list rate %g: if it is the promotional rate, move the row into upstreamPromotions", p.name, row, value, p.rows[row])
			}
		}
	}
}

// TestUpstreamPromotionsSwitchOnTheirLastDay loads a datasheet the way a sync does, with the clock
// set either side of a promotion's last day (TXE-7763). The rates bill the promotional value until
// the day before, and the standard value from the last day (UTC), and the switch names the rates it
// changed. A row that publishes any other value, and a list-billed row, are left as they are.
func TestUpstreamPromotionsSwitchOnTheirLastDay(t *testing.T) {
	logger := bifrost.NewDefaultLogger(schemas.LogLevelWarn)
	datasheet := map[string]any{
		"databricks/databricks-gemini-4-argon": map[string]any{"provider": "databricks", "mode": "chat", "input_cost_per_token": 2.49998e-06, "output_cost_per_token": 1.249997e-05, "cache_read_input_token_cost": 1.2502e-07, "input_cost_per_token_priority": 4.50002e-06},
		"databricks/system.ai.gpt-5-6-sol":     map[string]any{"provider": "databricks", "mode": "chat", "input_cost_per_token": 4.00001e-06, "output_cost_per_token": 1.999998e-05, "cache_read_input_token_cost": 3.9998e-07},
		// Upstream has already moved this row to a rate that is neither recorded value.
		"databricks/system.ai.gemini-4-argon":  map[string]any{"provider": "databricks", "mode": "chat", "input_cost_per_token": 6e-06, "output_cost_per_token": 3e-05, "cache_read_input_token_cost": 6e-07},
		"databricks/databricks-gemini-3-1-pro": map[string]any{"provider": "databricks", "mode": "chat", "input_cost_per_token": 2.49998e-06, "output_cost_per_token": 1.500002e-05, "cache_read_input_token_cost": 2.4997e-07},
	}
	raw, err := json.Marshal(datasheet)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "pricing.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	loadAt := func(t *testing.T, when string) *Store {
		t.Helper()
		now, err := time.Parse(time.RFC3339, when)
		require.NoError(t, err)
		original := promotionClock
		promotionClock = func() time.Time { return now }
		t.Cleanup(func() { promotionClock = original })
		store := New(nil, logger, Config{URL: "file://" + path})
		require.NoError(t, store.LoadFromURLIntoMemory(context.Background()))
		return store
	}
	rates := func(t *testing.T, store *Store, model string) (input, output, cacheRead float64) {
		t.Helper()
		row := store.Get(model, schemas.Databricks, schemas.ChatCompletionRequest)
		require.NotNil(t, row, "databricks/%s has no chat pricing row", model)
		return *row.InputCostPerToken, *row.OutputCostPerToken, *row.CacheReadInputTokenCost
	}

	t.Run("the day before Argon's last day, in UTC, bills the promotional rate", func(t *testing.T) {
		store := loadAt(t, "2026-11-30T07:59:59+08:00") // 23:59:59 UTC on 29 Nov
		input, output, cacheRead := rates(t, store, "databricks-gemini-4-argon")
		require.Equal(t, []float64{2.49998e-06, 1.249997e-05, 1.2502e-07}, []float64{input, output, cacheRead})
		input, _, _ = rates(t, store, "system.ai.gpt-5-6-sol")
		require.Equal(t, 5.00001e-06, input, "Sol's last day, 21 Nov, is before this clock, so Sol is already switched")
	})

	t.Run("from Argon's last day it bills the standard rate", func(t *testing.T) {
		store := loadAt(t, "2026-11-30T00:00:00Z")
		input, output, cacheRead := rates(t, store, "databricks-gemini-4-argon")
		require.Equal(t, []float64{4.99996e-06, 2.49999e-05, 2.5004e-07}, []float64{input, output, cacheRead})
		priority := store.Get("databricks-gemini-4-argon", schemas.Databricks, schemas.ChatCompletionRequest).InputCostPerTokenPriority
		require.Equal(t, 9.00004e-06, *priority)
		input, output, cacheRead = rates(t, store, "system.ai.gpt-5-6-sol")
		require.Equal(t, []float64{5.00001e-06, 3e-05, 4.99975e-07}, []float64{input, output, cacheRead})
		input, _, _ = rates(t, store, "system.ai.gemini-4-argon")
		require.Equal(t, 6e-06, input, "a row upstream has moved off the promotional value keeps its own rate")
		input, _, _ = rates(t, store, "databricks-gemini-3-1-pro")
		require.Equal(t, 2.49998e-06, input, "a list-billed row is not switched")
	})

	t.Run("the switch names each promotion and rate it changed", func(t *testing.T) {
		entries := snapshotEntries(t, map[string]map[string]float64{
			"databricks/databricks-gemini-4-argon": {"input_cost_per_token": 2.49998e-06},
			"databricks/system.ai.gpt-5-6-sol":     {"output_cost_per_token": 1.999998e-05},
		})
		now, err := time.Parse(time.RFC3339, "2026-11-30T12:00:00Z")
		require.NoError(t, err)
		lines := applyUpstreamPromotions(entries, now)
		require.Len(t, lines, 2, "one line per promotion switched: %v", lines)
		require.Contains(t, lines[0], "Databricks Gemini 4 Argon 50% promotion ended 2026-11-30")
		require.Contains(t, lines[0], "databricks/databricks-gemini-4-argon input_cost_per_token 2.49998e-06->4.99996e-06")
		require.Contains(t, lines[1], "Databricks GPT-5.6 Sol promotional pricing ended 2026-11-21")
		require.Contains(t, lines[1], "databricks/system.ai.gpt-5-6-sol output_cost_per_token 1.999998e-05->3e-05")
		require.Empty(t, applyUpstreamPromotions(snapshotEntries(t, map[string]map[string]float64{"databricks/databricks-gemini-4-argon": {"input_cost_per_token": 2.49998e-06}}), now.AddDate(0, 0, -1)), "no line while no promotion has ended")
	})
}

func writeUpstreamDBURowsSnapshot(t *testing.T, source string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	var datasheet map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &datasheet))
	rows := map[string]map[string]*float64{}
	for _, key := range sortedKeys(datasheet) {
		var fields map[string]any
		if json.Unmarshal(datasheet[key], &fields) != nil {
			continue
		}
		kept := map[string]*float64{}
		for dbuKey, usdKey := range dbuToUSD {
			if v, ok := fields[dbuKey].(float64); ok {
				kept[dbuKey] = &v
				if u, ok := fields[usdKey].(float64); ok {
					kept[usdKey] = &u
				} else {
					kept[usdKey] = nil
				}
			}
		}
		if len(kept) > 0 {
			rows[key] = kept
		}
	}
	out, err := json.MarshalIndent(upstreamDBURowsSnapshot{
		Source:  "https://getbifrost.ai/datasheet",
		Fetched: os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Rows:    rows,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Clean(upstreamDBURowsSnapshotPath), append(out, '\n'), 0o644))
	t.Logf("wrote %d upstream DBU rows to %s", len(rows), upstreamDBURowsSnapshotPath)
}
