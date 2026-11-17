package datasheet

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/capsohq/bifrost/core/network"
	"net/http"
	"net/url"
	"os"
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
