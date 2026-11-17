package datasheet

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/capsohq/bifrost/core/network"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

const (
	urlFetchMaxRetries = 3                // retries after first attempt (4 attempts total)
	urlFetchMaxBackoff = 10 * time.Second // cap for exponential backoff (steps start at 1s)
)

// Total budget for fetching a datasheet across all retries; vars so tests can shorten them.
var (
	pricingFetchTimeout = DefaultPricingTimeout
	paramsFetchTimeout  = DefaultModelParametersTimeout
)

// SyncFromURL fetches the upstream pricing datasheet, persists it to the DB
// (when configStore != nil), and refreshes the in-memory cache + derived
// datasheet view. On URL failure it falls back to existing DB records when
// any exist, otherwise propagates the error.
//
// The composer owns the distributed lock, the ticker, and the after-sync
// gossip hook — none of that lives here. SyncFromURL is the pure
// "URL → DB → memory" step.
func (s *Store) SyncFromURL(ctx context.Context) error {
	// Only the fetch is bounded so a hung URL cannot starve the DB fallback below.
	fetchCtx, cancel := context.WithTimeout(ctx, pricingFetchTimeout)
	pricingData, err := withRetries(fetchCtx, urlFetchMaxRetries, urlFetchMaxBackoff, func() (map[string]Entry, error) {
		return s.loadPricingFromURL(fetchCtx)
	})
	cancel()
	if err != nil {
		// URL failed — fall back to existing DB records when we have them.
		if s.configStore != nil {
			records, dbErr := s.configStore.GetModelPrices(ctx)
			if dbErr != nil {
				return fmt.Errorf("failed to load pricing data from URL (%v) and failed to get pricing records: %w", err, dbErr)
			}
			if len(records) > 0 {
				if s.logger != nil {
					s.logger.Warn("failed to fetch pricing from URL, falling back to existing database records: %v", err)
				}
				return nil
			}
		}
		return fmt.Errorf("failed to load pricing data from URL and no existing data available: %w", err)
	}

	if s.configStore != nil {
		// Dedup by (model, provider, mode) before the write: the batched upsert
		// below issues multi-row ON CONFLICT statements, and PostgreSQL rejects a
		// statement whose VALUES list hits the same conflict target twice.
		records := make([]configstoreTables.TableModelPricing, 0, len(pricingData))
		seen := make(map[string]struct{}, len(pricingData))
		for modelKey, entry := range pricingData {
			pricing := convertEntryToTablePricing(modelKey, entry)
			key := makeKey(pricing.Model, pricing.Provider, pricing.Mode)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			records = append(records, pricing)
		}
		if err := s.configStore.UpsertModelPricesBatch(ctx, records); err != nil {
			return fmt.Errorf("failed to sync pricing data to database: %w", err)
		}

		if err := s.LoadFromDB(ctx); err != nil {
			return fmt.Errorf("failed to reload pricing cache: %w", err)
		}
	} else {
		// No config store — apply the parsed data directly to in-memory state.
		s.applyPricingData(pricingData)
	}

	if s.logger != nil {
		s.logger.Debug("successfully synced %d pricing records", len(pricingData))
	}
	return nil
}

// LoadFromDB reloads the in-memory pricing cache + datasheet view from the
// config store. Used by the composer at bootstrap and as the gossip
// ReloadFromDB handler on non-leader pods.
func (s *Store) LoadFromDB(ctx context.Context) error {
	if s.configStore == nil {
		return nil
	}
	records, err := s.configStore.GetModelPrices(ctx)
	if err != nil {
		return fmt.Errorf("failed to load pricing from database: %w", err)
	}

	s.mu.Lock()
	s.pricingData = make(map[string]configstoreTables.TableModelPricing, len(records))
	for _, pricing := range records {
		key := makeKey(pricing.Model, pricing.Provider, pricing.Mode)
		s.pricingData[key] = pricing
	}
	s.rebuildDatasheetViewUnsafe()
	s.mu.Unlock()

	if s.logger != nil {
		s.logger.Debug("loaded %d pricing records from database into memory", len(records))
	}
	return nil
}

// LoadFromURLIntoMemory loads pricing from the URL directly into memory
// (no DB). Used when the composer was built without a config store.
func (s *Store) LoadFromURLIntoMemory(ctx context.Context) error {
	pricingData, err := withRetries(ctx, urlFetchMaxRetries, urlFetchMaxBackoff, func() (map[string]Entry, error) {
		return s.loadPricingFromURL(ctx)
	})
	if err != nil {
		return fmt.Errorf("failed to load pricing data from URL: %w", err)
	}
	s.applyPricingData(pricingData)
	return nil
}

// applyPricingData replaces the in-memory pricing cache + datasheet view
// from a freshly-parsed URL payload. Used by the LoadFromURLIntoMemory
// path and the no-configstore SyncFromURL fallback.
func (s *Store) applyPricingData(pricingData map[string]Entry) {
	s.mu.Lock()
	s.pricingData = make(map[string]configstoreTables.TableModelPricing, len(pricingData))
	for modelKey, entry := range pricingData {
		pricing := convertEntryToTablePricing(modelKey, entry)
		key := makeKey(pricing.Model, pricing.Provider, pricing.Mode)
		s.pricingData[key] = pricing
	}
	s.rebuildDatasheetViewUnsafe()
	s.mu.Unlock()
}

// FilePathFromURL resolves a parsed file:// URL to a filesystem path,
// supporting both absolute and relative references. Go's url.Parse scatters a
// relative path across different fields depending on its form, so we reassemble
// it here:
//   - file:./x.json or file:x.json -> Opaque ("./x.json")
//   - file://./x.json              -> Host (".") + Path ("/x.json") = "./x.json"
//   - file:///abs/x.json           -> Path ("/abs/x.json")
//   - file://localhost/abs/x.json  -> Path ("/abs/x.json")
//
// Relative paths resolve against the process working directory, matching how the
// sqlite config store treats a relative "path" value.
//
// Exported because every air-gapped source in the catalog resolves file:// URLs
// the same way: the pricing datasheet, the model parameters datasheet, and the
// MCP library catalog (modelcatalog.fetchMCPLibrary).
func FilePathFromURL(parsed *url.URL) string {
	if parsed.Opaque != "" {
		return parsed.Opaque
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return parsed.Host + parsed.Path
	}
	return parsed.Path
}

// loadPricingFromURL fetches and parses the pricing datasheet at the
// configured URL. Honors ctx for cancellation.
func (s *Store) loadPricingFromURL(ctx context.Context) (map[string]Entry, error) {
	s.syncCfgMu.RLock()
	rawURL := s.url
	s.syncCfgMu.RUnlock()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pricing URL: %w", err)
	}

	var data []byte

	if parsed.Scheme == "file" {
		data, err = os.ReadFile(FilePathFromURL(parsed))
		if err != nil {
			return nil, fmt.Errorf("failed to read pricing file: %w", err)
		}
	} else {
		if err := ValidateFetchURL(rawURL); err != nil {
			return nil, fmt.Errorf("pricing URL validation failed: %w", err)
		}
		// ValidateExternalURL screens the configured host once; the guarded
		// client holds every connection and redirect hop to the same policy.
		client := NewFetchClient(DefaultPricingTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL(), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to download pricing data: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to download pricing data: HTTP %d", resp.StatusCode)
		}
		data, err = network.ReadAllBounded(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read pricing data response: %w", err)
		}
	}
	var pricingData map[string]Entry
	if err := json.Unmarshal(data, &pricingData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pricing data: %w", err)
	}
	if corrected := applyUpstreamRateCorrections(pricingData); len(corrected) > 0 && s.logger != nil {
		s.logger.Debug("corrected %d upstream datasheet rates the fork has evidence are wrong: %v", len(corrected), corrected)
	}
	for _, line := range applyUpstreamPromotions(pricingData, promotionClock()) {
		if s.logger != nil {
			s.logger.Info("%s", line)
		}
	}
	if s.logger != nil {
		s.logger.Debug("successfully downloaded and parsed %d pricing records", len(pricingData))
	}

	return pricingData, nil
}

// upstreamRateCorrection replaces one rate the upstream datasheet publishes and the fork has
// evidence is wrong. It applies only while the row still publishes exactly the value it corrects,
// so a row upstream has since fixed, or a file:// datasheet with a rate of its own, is never
// rewritten. TestUpstreamRateCorrectionsStillMatchUpstream fails once the upstream snapshot stops
// publishing that value, so the correction is removed instead of left stale.
type upstreamRateCorrection struct {
	row       string                 // datasheet key, "provider/model"
	key       string                 // the cost key's JSON name, which field must return
	field     func(*Entry) **float64 // the Entry field the key decodes into
	upstream  float64                // the value upstream publishes, which this replaces
	corrected float64
	evidence  string // the source and the date it was read
}

// upstreamRateCorrections is every fork correction to an upstream datasheet rate.
var upstreamRateCorrections = []upstreamRateCorrection{
	{
		row: "databricks/databricks-gemini-3-pro", key: "input_cost_per_token",
		field:    func(e *Entry) **float64 { return &e.InputCostPerToken },
		upstream: 2e-06, corrected: 2.49998e-06,
		evidence: "TXE-7688. The row's input_dbu_cost_per_token is 3.5714e-05, $2.49998e-06 at Databricks' $0.07 per DBU. Every other Databricks Gemini row in the datasheet bills that list rate, including databricks-gemini-3-1-pro, which has the same DBU rates and publishes 2.49998e-06. The 2e-06 here is 0.8x, the 20% promotional price the other rows do not carry. https://www.databricks.com/product/pricing/proprietary-foundation-model-serving, read 18 Nov 2026, lists Gemini 3.1 Pro at 35.714 DBU per 1M input tokens and no longer lists Gemini 3 Pro.",
	},
	{
		row: "databricks/databricks-gemini-3-pro", key: "output_cost_per_token",
		field:    func(e *Entry) **float64 { return &e.OutputCostPerToken },
		upstream: 1.2e-05, corrected: 1.500002e-05,
		evidence: "TXE-7688. The row's output_dbu_cost_per_token is 0.000214286, $1.500002e-05 at $0.07 per DBU, which databricks-gemini-3-1-pro publishes. The 1.2e-05 here is the same 0.8x promotional price. Same page and read date as the input rate: Gemini 3.1 Pro at 214.286 DBU per 1M output tokens.",
	},
}

// applyUpstreamRateCorrections rewrites, in pricingData, each rate in upstreamRateCorrections whose
// row still publishes the value it corrects, and returns "row key" for each one it rewrote.
func applyUpstreamRateCorrections(pricingData map[string]Entry) []string {
	var corrected []string
	for _, c := range upstreamRateCorrections {
		entry, ok := pricingData[c.row]
		if !ok {
			continue
		}
		rate := c.field(&entry)
		if *rate == nil || **rate != c.upstream {
			continue
		}
		value := c.corrected
		*rate = &value
		pricingData[c.row] = entry
		corrected = append(corrected, c.row+" "+c.key)
	}
	return corrected
}

// upstreamPromotion is a vendor promotion on rates the upstream datasheet publishes (TXE-7763). A
// promotion ends on a date the datasheet does not carry, so a row that publishes the promotional
// rate would keep billing it after the end. Every rate of the promotion is recorded with its
// promotional and standard values. From lastDay (UTC, the promotion's last day, checked at each
// parse) the standard value replaces the promotional one, while the row still publishes exactly
// the promotional value. The datasheet syncs every 24 hours, so switching on the last day means
// the rate never underbills after the end; the cost is overbilling within that last day.
// A rate whose upstream value is already the standard one (a list-billed row) is left as it is.
type upstreamPromotion struct {
	name    string
	lastDay string // YYYY-MM-DD, UTC: the last day the promotional rate applies
	source  string // where the promotion and its end date are published
	read    string // YYYY-MM-DD the source was read
	// rates maps row to cost key to the rate's promotional and standard values. A standard value
	// of 0 means it is not known yet: TestEveryPromotionHasItsStandardRateBeforeItEnds fails from
	// promotionWarningDays before lastDay until it is recorded.
	rates map[string]map[string]promotionalRate
}

type promotionalRate struct {
	promotional float64
	standard    float64
}

// promotionWarningDays is how long before a promotion's last day a rate with no standard value
// recorded fails TestEveryPromotionHasItsStandardRateBeforeItEnds.
const promotionWarningDays = 14

// promotionClock is the clock applyUpstreamPromotions reads at each parse; tests replace it.
var promotionClock = func() time.Time { return time.Now().UTC() }

const databricksFoundationModelPricing = "https://www.databricks.com/product/pricing/proprietary-foundation-model-serving"

// upstreamPromotions is every known vendor promotion on a rate the fork bills. The values come from
// the datasheet fetched 18 Nov 2026 (testdata/upstream_promotion_rows.json); which rates are
// promotional, and the standard rate, from the source.
var upstreamPromotions = []upstreamPromotion{
	{
		name: "Databricks Gemini 4 Argon 50% promotion", lastDay: "2026-11-30", source: databricksFoundationModelPricing, read: "2026-11-18",
		// "The Gemini 4 Argon DBU rates shown here reflect a 50% promotion in effect through November 30, 2026.
		// Afterward, the standard rates, twice those shown, will take effect." The datasheet publishes the rates shown.
		rates: map[string]map[string]promotionalRate{
			"databricks/databricks-gemini-4-argon": {"cache_creation_input_token_cost": {2.49998e-06, 4.99996e-06}, "cache_creation_input_token_cost_priority": {4.50002e-06, 9.00004e-06}, "cache_read_input_token_cost": {1.2502e-07, 2.5004e-07}, "cache_read_input_token_cost_priority": {2.2498e-07, 4.4996e-07}, "input_cost_per_token": {2.49998e-06, 4.99996e-06}, "input_cost_per_token_priority": {4.50002e-06, 9.00004e-06}, "output_cost_per_token": {1.249997e-05, 2.49999e-05}, "output_cost_per_token_priority": {2.250003e-05, 4.50001e-05}},
			"databricks/system.ai.gemini-4-argon":  {"cache_read_input_token_cost": {1.2502e-07, 2.5004e-07}, "cache_read_input_token_cost_priority": {2.2498e-07, 4.4996e-07}, "input_cost_per_token": {2.49998e-06, 4.99996e-06}, "input_cost_per_token_priority": {4.50002e-06, 9.00004e-06}, "output_cost_per_token": {1.249997e-05, 2.49999e-05}, "output_cost_per_token_priority": {2.250003e-05, 4.50001e-05}},
		},
	},
	{
		name: "Databricks Gemini 3.6, 3.7 and 3.8 Flash 50% promotion", lastDay: "2026-12-31", source: databricksFoundationModelPricing, read: "2026-11-18",
		// "The Gemini 3.6 Flash, 3.7 Flash, and 3.8 Flash DBU rates shown here reflect a 50% promotion in effect through
		// December 31, 2026. Afterward, the standard rates, twice those shown, will take effect."
		rates: map[string]map[string]promotionalRate{
			"databricks/databricks-gemini-3-6-flash": {"cache_creation_input_token_cost": {7.4998e-07, 1.49996e-06}, "cache_creation_input_token_cost_priority": {1.35002e-06, 2.70004e-06}, "cache_read_input_token_cost": {7.497e-08, 1.4994e-07}, "cache_read_input_token_cost_priority": {1.3503e-07, 2.7006e-07}, "input_cost_per_token": {7.4998e-07, 1.49996e-06}, "input_cost_per_token_priority": {1.35002e-06, 2.70004e-06}, "output_cost_per_token": {3.74997e-06, 7.49994e-06}, "output_cost_per_token_priority": {6.75003e-06, 1.35001e-05}},
			"databricks/databricks-gemini-3-7-flash": {"cache_creation_input_token_cost": {7.4998e-07, 1.49996e-06}, "cache_creation_input_token_cost_priority": {1.35002e-06, 2.70004e-06}, "cache_read_input_token_cost": {7.497e-08, 1.4994e-07}, "cache_read_input_token_cost_priority": {1.3503e-07, 2.7006e-07}, "input_cost_per_token": {7.4998e-07, 1.49996e-06}, "input_cost_per_token_priority": {1.35002e-06, 2.70004e-06}, "output_cost_per_token": {3.74997e-06, 7.49994e-06}, "output_cost_per_token_priority": {6.75003e-06, 1.35001e-05}},
			"databricks/databricks-gemini-3-8-flash": {"cache_creation_input_token_cost": {7.4998e-07, 1.49996e-06}, "cache_creation_input_token_cost_priority": {1.35002e-06, 2.70004e-06}, "cache_read_input_token_cost": {7.497e-08, 1.4994e-07}, "cache_read_input_token_cost_priority": {1.3503e-07, 2.7006e-07}, "input_cost_per_token": {7.4998e-07, 1.49996e-06}, "input_cost_per_token_priority": {1.35002e-06, 2.70004e-06}, "output_cost_per_token": {3.74997e-06, 7.49994e-06}, "output_cost_per_token_priority": {6.75003e-06, 1.35001e-05}},
			"databricks/system.ai.gemini-3-6-flash":  {"cache_read_input_token_cost": {7.497e-08, 1.4994e-07}, "cache_read_input_token_cost_priority": {1.3503e-07, 2.7006e-07}, "input_cost_per_token": {7.4998e-07, 1.49996e-06}, "input_cost_per_token_priority": {1.35002e-06, 2.70004e-06}, "output_cost_per_token": {3.74997e-06, 7.49994e-06}, "output_cost_per_token_priority": {6.75003e-06, 1.35001e-05}},
			"databricks/system.ai.gemini-3-7-flash":  {"cache_read_input_token_cost": {7.497e-08, 1.4994e-07}, "cache_read_input_token_cost_priority": {1.3503e-07, 2.7006e-07}, "input_cost_per_token": {7.4998e-07, 1.49996e-06}, "input_cost_per_token_priority": {1.35002e-06, 2.70004e-06}, "output_cost_per_token": {3.74997e-06, 7.49994e-06}, "output_cost_per_token_priority": {6.75003e-06, 1.35001e-05}},
			"databricks/system.ai.gemini-3-8-flash":  {"cache_read_input_token_cost": {7.497e-08, 1.4994e-07}, "cache_read_input_token_cost_priority": {1.3503e-07, 2.7006e-07}, "input_cost_per_token": {7.4998e-07, 1.49996e-06}, "input_cost_per_token_priority": {1.35002e-06, 2.70004e-06}, "output_cost_per_token": {3.74997e-06, 7.49994e-06}, "output_cost_per_token_priority": {6.75003e-06, 1.35001e-05}},
		},
	},
	{
		name: "Databricks GPT-5.6 Sol promotional pricing", lastDay: "2026-11-21", source: databricksFoundationModelPricing, read: "2026-11-18",
		// "The GPT-5.6 Sol rates shown here reflect OpenAI's promotional pricing, in effect through November 21, 2026.
		// Afterward, the standard GPT-5.6 Sol rates will take effect; input, cache, and Batch rates will be 25% higher
		// than those shown, while output rates will be 50% higher." databricks-gpt-5-6-sol already publishes the standard
		// short-context base rates (the standard value below); its priority and long-context rates are the promotional ones.
		rates: map[string]map[string]promotionalRate{
			"databricks/databricks-gpt-5-6-sol": {"cache_creation_input_token_cost": {5.00002e-06, 6.25002e-06}, "cache_creation_input_token_cost_above_272k_tokens": {9.99999e-06, 1.25e-05}, "cache_creation_input_token_cost_above_272k_tokens_priority": {1.999998e-05, 2.5e-05}, "cache_creation_input_token_cost_priority": {9.99999e-06, 1.25e-05}, "cache_read_input_token_cost": {4.00008e-07, 5.0001e-07}, "cache_read_input_token_cost_above_272k_tokens": {8.0003e-07, 1.00004e-06}, "cache_read_input_token_cost_above_272k_tokens_priority": {1.59999e-06, 1.99999e-06}, "cache_read_input_token_cost_priority": {8.0003e-07, 1.00004e-06}, "input_cost_per_token": {4.00002e-06, 5.00003e-06}, "input_cost_per_token_above_272k_tokens": {8.00002e-06, 1e-05}, "input_cost_per_token_above_272k_tokens_priority": {1.599997e-05, 2e-05}, "input_cost_per_token_priority": {8.00002e-06, 1e-05}, "output_cost_per_token": {2e-05, 2.999997e-05}, "output_cost_per_token_above_272k_tokens": {3e-05, 4.5e-05}, "output_cost_per_token_above_272k_tokens_priority": {6.000001e-05, 9e-05}, "output_cost_per_token_priority": {4.000003e-05, 6e-05}},
			"databricks/system.ai.gpt-5-6-sol":  {"cache_creation_input_token_cost": {5.00003e-06, 6.25004e-06}, "cache_read_input_token_cost": {3.9998e-07, 4.99975e-07}, "cache_read_input_token_cost_priority": {8.0003e-07, 1.00004e-06}, "input_cost_per_token": {4.00001e-06, 5.00001e-06}, "input_cost_per_token_priority": {8.00002e-06, 1e-05}, "output_cost_per_token": {1.999998e-05, 3e-05}, "output_cost_per_token_priority": {4.000003e-05, 6e-05}},
		},
	},
}

// listBilledPromotion is a vendor promotion the fork does not apply: upstream publishes the list rate
// on its rows, so the fork bills above the promotional price until the promotion ends, and nothing
// changes when it does. Each row is recorded with the list input rate it publishes, so a row that
// starts publishing the promotional rate fails TestUpstreamPromotionsMatchTheSnapshot and is moved
// into upstreamPromotions.
type listBilledPromotion struct {
	name    string
	lastDay string
	source  string
	read    string
	rows    map[string]float64 // row -> the list input_cost_per_token it publishes
}

var listBilledPromotions = []listBilledPromotion{
	{
		name: "Databricks Gemini 20% promotion", lastDay: "2027-01-31", source: databricksFoundationModelPricing, read: "2026-11-18",
		// "The DBU rates for these Gemini models do not include a promotional discount of 20% (promotional pricing is
		// 20% lower than shown). The promotion expires on Jan 31, 2027." databricks-gemini-3-pro bills the list rate
		// through upstreamRateCorrections (TXE-7688) and Databricks no longer lists it.
		rows: map[string]float64{
			"databricks/databricks-gemini-2-5-flash-lite": 1.2502e-07, "databricks/databricks-gemini-3-1-flash-image": 6.2503e-07,
			"databricks/databricks-gemini-3-1-flash-lite": 3.1248e-07, "databricks/databricks-gemini-3-1-flash-lite-image": 3.1248e-07,
			"databricks/databricks-gemini-3-1-pro": 2.49998e-06, "databricks/databricks-gemini-3-5-flash": 1.87502e-06,
			"databricks/databricks-gemini-3-5-flash-lite": 3.7499e-07, "databricks/databricks-gemini-3-flash": 6.2503e-07,
			"databricks/databricks-gemini-3-pro-image": 2.49998e-06, "databricks/system.ai.gemini-3-1-flash-image": 6.2503e-07,
			"databricks/system.ai.gemini-3-1-flash-lite": 3.1248e-07, "databricks/system.ai.gemini-3-1-pro": 2.49998e-06,
			"databricks/system.ai.gemini-3-5-flash": 1.87502e-06, "databricks/system.ai.gemini-3-5-flash-lite": 3.7499e-07,
			"databricks/system.ai.gemini-3-flash": 6.2503e-07, "databricks/system.ai.gemini-3-pro-image": 2.49998e-06,
		},
	},
	{
		name: "Databricks Grok 20% promotion", lastDay: "2027-01-31", source: databricksFoundationModelPricing, read: "2026-11-18",
		// "The DBU rates for these Grok models do not include a promotional discount of 20%. The promotion expires on Jan 31, 2027."
		rows: map[string]float64{
			"databricks/databricks-grok-4-6": 2.49998e-06, "databricks/databricks-grok-4-7": 2.49998e-06,
			"databricks/system.ai.grok-4-6": 2.49998e-06, "databricks/system.ai.grok-4-7": 2.49998e-06,
		},
	},
}

// applyUpstreamPromotions switches, in pricingData, the rates of every promotion whose last day is
// on or before now (UTC) from the promotional value to the standard one, where the row still
// publishes exactly the promotional value and a standard value is recorded. It returns one log line
// for each promotion it switched, naming the rows and rates, so a price change is never silent.
func applyUpstreamPromotions(pricingData map[string]Entry, now time.Time) []string {
	return applyPromotions(pricingData, now, upstreamPromotions)
}

func applyPromotions(pricingData map[string]Entry, now time.Time, promotions []upstreamPromotion) []string {
	today := now.UTC().Format(time.DateOnly)
	var lines []string
	for _, p := range promotions {
		if today < p.lastDay {
			continue
		}
		var switched []string
		for _, row := range sortedStringKeys(p.rates) {
			entry, ok := pricingData[row]
			if !ok {
				continue
			}
			changed := false
			for _, key := range sortedStringKeys(p.rates[row]) {
				r := p.rates[row][key]
				rate := entryRateField(&entry, key)
				if rate == nil || *rate == nil || **rate != r.promotional || r.standard == 0 {
					continue
				}
				value := r.standard
				*rate = &value
				changed = true
				switched = append(switched, fmt.Sprintf("%s %s %g->%g", row, key, r.promotional, r.standard))
			}
			if changed {
				pricingData[row] = entry
			}
		}
		if len(switched) > 0 {
			lines = append(lines, fmt.Sprintf("datasheet: %s ended %s (%s, read %s): billing %d rates at the standard rate: %s", p.name, p.lastDay, p.source, p.read, len(switched), strings.Join(switched, ", ")))
		}
	}
	return lines
}

// entryRateFields maps each *float64 cost field's JSON name to its index path in Entry, through the
// embedded Options.
var entryRateFields = func() map[string][]int {
	fields := map[string][]int{}
	var walk func(t reflect.Type, prefix []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue // upstreamRateAliases holds the older key names, read through their canonical fields
			}
			index := append(append([]int(nil), prefix...), i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type, index)
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" && f.Type == reflect.TypeOf((*float64)(nil)) {
				fields[name] = index
			}
		}
	}
	walk(reflect.TypeOf(Entry{}), nil)
	return fields
}()

// entryRateField returns the Entry field the cost key decodes into, or nil when no field does.
func entryRateField(entry *Entry, key string) **float64 {
	index, ok := entryRateFields[key]
	if !ok {
		return nil
	}
	return reflect.ValueOf(entry).Elem().FieldByIndex(index).Addr().Interface().(**float64)
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
