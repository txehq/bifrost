package datasheet

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An over-limit refusal on a row that prices images only per image token (TXE-7492) is charged
// the table's tokens per image at the row's image token rate, not $0.
func TestCalculateCostForEstimatedImageUsage_TokenPricedRows(t *testing.T) {
	tokenRow := func(provider, model string, rate float64) *Store {
		raw, err := json.Marshal(map[string]any{"provider": provider, "mode": "image_generation", "input_cost_per_token": 5e-06, "output_cost_per_image_token": rate})
		require.NoError(t, err)
		return batchStore(t, provider, model, "image_generation", string(raw))
	}
	estimate := func(s *Store, provider schemas.ModelProvider, model string, usage schemas.EstimatedImageUsage) float64 {
		return s.CalculateCostForEstimatedImageUsage(&usage, schemas.RoutingInfo{Provider: provider, Model: model}, schemas.ImageGenerationRequest, nil)
	}

	gptImage2 := tokenRow("openai", "gpt-image-2", 3e-05)
	assert.InDelta(t, 3*7034*3e-05, estimate(gptImage2, schemas.OpenAI, "gpt-image-2", schemas.EstimatedImageUsage{OutputImages: 3, Size: "1024x1024", Quality: "high"}), 1e-12)
	// A client-supplied n large enough to wrap an int product still charges a positive estimate.
	huge := math.MaxInt64 / 1000
	assert.InDelta(t, float64(huge)*7034*3e-05, estimate(gptImage2, schemas.OpenAI, "gpt-image-2", schemas.EstimatedImageUsage{OutputImages: huge, Size: "1024x1024", Quality: "high"}), float64(huge)*1e-09)
	assert.InDelta(t, 200*3e-05, estimate(gptImage2, schemas.OpenAI, "gpt-image-2", schemas.EstimatedImageUsage{OutputImages: 1, Size: "1024x1024", Quality: "low"}), 1e-12)
	assert.InDelta(t, 1767*3e-05, estimate(gptImage2, schemas.OpenAI, "gpt-image-2", schemas.EstimatedImageUsage{OutputImages: 1, Size: "2048x2048", Quality: "medium"}),
		1e-12, "a size OpenAI publishes no figure for takes the dearest at that quality")
	for _, quality := range []string{"", "auto"} {
		assert.InDelta(t, 2*7034*3e-05, estimate(gptImage2, schemas.OpenAI, "gpt-image-2", schemas.EstimatedImageUsage{OutputImages: 2, Quality: quality}), 1e-12, "quality %q", quality)
	}

	// gpt-image-2.5 and 2.6 publish no figure: the floor, gpt-image-2's dearest, at every quality.
	flare := tokenRow("openai", "gpt-image-2.5-flare-2026-09-08", 3e-05)
	for _, quality := range []string{"low", "xhigh", "max", ""} {
		assert.InDelta(t, 7034*3e-05, estimate(flare, schemas.OpenAI, "gpt-image-2.5-flare-2026-09-08", schemas.EstimatedImageUsage{OutputImages: 1, Size: "1024x1024", Quality: quality}), 1e-12, "quality %q", quality)
	}

	azure := tokenRow("azure", "gpt-image-1.5-2025-12-16", 3.2e-05)
	assert.InDelta(t, 6208*3.2e-05, estimate(azure, schemas.Azure, "gpt-image-1.5-2025-12-16", schemas.EstimatedImageUsage{OutputImages: 1, Size: "1536x1024", Quality: "high"}), 1e-12)
	mini := tokenRow("databricks", "databricks-gpt-image-1-mini", 8e-06)
	assert.InDelta(t, 4500*8e-06, estimate(mini, schemas.Databricks, "databricks-gpt-image-1-mini", schemas.EstimatedImageUsage{OutputImages: 1, Size: "1024x1024", Quality: "high"}), 1e-12)

	unknown := tokenRow("openai", "gpt-image-9", 3e-05)
	assert.Zero(t, estimate(unknown, schemas.OpenAI, "gpt-image-9", schemas.EstimatedImageUsage{OutputImages: 1, Quality: "high"}), "a model with no figures still prices to 0")

	// A row with per-image prices keeps them: the token table is never consulted.
	perImage := batchStore(t, "openai", "gpt-image-1.5", "image_generation",
		`{"provider": "openai", "mode": "image_generation", "output_cost_per_image_token": 3.2e-05, "output_cost_per_image_high_quality": 0.133}`)
	assert.InDelta(t, 0.133, estimate(perImage, schemas.OpenAI, "gpt-image-1.5", schemas.EstimatedImageUsage{OutputImages: 1, Size: "1024x1024", Quality: "high"}), 1e-12)

	// An explicit per-image price of 0 is a free image, not a missing price: no token fallback.
	freeImage := batchStore(t, "openai", "gpt-image-1.5", "image_generation",
		`{"provider": "openai", "mode": "image_generation", "output_cost_per_image_token": 3.2e-05, "output_cost_per_image_high_quality": 0}`)
	assert.Zero(t, estimate(freeImage, schemas.OpenAI, "gpt-image-1.5", schemas.EstimatedImageUsage{OutputImages: 1, Size: "1024x1024", Quality: "high"}))
}

// The fallback's per-image field list is read from the struct; it must find the known fields and
// never a token rate.
func TestPerImageOutputPriceFields(t *testing.T) {
	typ := reflect.TypeFor[configstoreTables.TableModelPricing]()
	var names []string
	for _, i := range perImageOutputPriceFields() {
		names = append(names, typ.Field(i).Name)
	}
	assert.Contains(t, names, "OutputCostPerImage")
	assert.Contains(t, names, "OutputCostPerPixel")
	assert.Contains(t, names, "OutputCostPerImageHighQuality")
	assert.Contains(t, names, "OutputCostPerImageAbove1024x1024Pixels")
	assert.NotContains(t, names, "OutputCostPerImageToken")
}

func BenchmarkCalculateCostForEstimatedImageUsage_TokenPricedRow(b *testing.B) {
	s := testStoreWithPricing(map[string]configstoreTables.TableModelPricing{makeKey("gpt-image-2", "openai", "image_generation"): {
		Model: "gpt-image-2", Provider: "openai", Mode: "image_generation", OutputCostPerImageToken: schemas.Ptr(3e-05),
	}})
	usage := &schemas.EstimatedImageUsage{OutputImages: 3, Size: "1024x1024", Quality: "high"}
	routing := schemas.RoutingInfo{Provider: schemas.OpenAI, Model: "gpt-image-2"}
	b.ReportAllocs()
	for b.Loop() {
		s.CalculateCostForEstimatedImageUsage(usage, routing, schemas.ImageGenerationRequest, nil)
	}
}

// upstreamImageTokenRowsSnapshot is testdata/upstream_image_token_rows.json: every row of the
// upstream datasheet with an output_cost_per_image_token price, verbatim.
type upstreamImageTokenRowsSnapshot struct {
	Source  string                     `json:"source"`
	Fetched string                     `json:"fetched"`
	Rows    map[string]json.RawMessage `json:"rows"`
}

const upstreamImageTokenRowsSnapshotPath = "testdata/upstream_image_token_rows.json"

// estimatedImageTokenExemptions are the upstream rows with an image token price whose over-limit
// estimate stays $0, each with its reason.
var estimatedImageTokenExemptions = map[string]string{
	"azure/MAI-Image-2.6":                               "Microsoft publishes no output tokens or price per image for MAI-Image-2.6 (learn.microsoft.com Foundry Models sold by Azure; azure.microsoft.com Foundry Models pricing; microsoft.ai MAI Image 2.6 announcement; read 2026-11-16)",
	"azure/MAI-Image-2.6-Flash":                         "Microsoft publishes no output tokens or price per image for MAI-Image-2.6-Flash (same pages, read 2026-11-16)",
	"databricks/databricks-gemini-3-1-flash-image":      "a chat-mode row: an image request never resolves to it (getBasePricingLocked has no image-to-chat fallback; read 2026-11-16)",
	"databricks/databricks-gemini-3-1-flash-lite-image": "a chat-mode row: an image request never resolves to it (read 2026-11-16)",
	"databricks/databricks-gemini-3-pro-image":          "a chat-mode row: an image request never resolves to it (read 2026-11-16)",
	"vertex_ai/deep-research-pro-preview-12-2025":       "a chat-mode research agent, not an image model: an image request never resolves to it (read 2026-11-16)",
}

// TestEveryTokenPricedImageRowHasAnEstimate is the class check for TXE-7492. It reads the committed
// snapshot of the upstream datasheet's rows that price image output per token, loads them into a
// store as the gateway does, and estimates one refused image on each. Each must price above $0
// (per-image prices, or an entry in estimatedimagetokens.go) or be in estimatedImageTokenExemptions
// with its reason. An exempt row that now prices, or one upstream no longer publishes, must leave
// the list.
//
// Regenerate the snapshot from a datasheet downloaded by hand (tests run offline); the same
// variables regenerate testdata/upstream_cost_keys.json:
//
//	curl -sSL -o /tmp/datasheet.json https://getbifrost.ai/datasheet
//	UPSTREAM_DATASHEET_FILE=/tmp/datasheet.json UPSTREAM_DATASHEET_FETCHED=YYYY-MM-DD go test ./modelcatalog/datasheet/ -run 'TestEveryTokenPricedImageRowHasAnEstimate|TestEveryUpstreamCostKeyIsParsedOrIgnored'
//
// It runs on every pull request in the Go unit tests (offline) job.
func TestEveryTokenPricedImageRowHasAnEstimate(t *testing.T) {
	if source := os.Getenv("UPSTREAM_DATASHEET_FILE"); source != "" {
		writeUpstreamImageTokenRowsSnapshot(t, source)
	}
	raw, err := os.ReadFile(upstreamImageTokenRowsSnapshotPath)
	require.NoError(t, err)
	var snapshot upstreamImageTokenRowsSnapshot
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.NotEmpty(t, snapshot.Rows, "%s lists no rows", upstreamImageTokenRowsSnapshotPath)

	entries := make(map[string]Entry, len(snapshot.Rows))
	for key, row := range snapshot.Rows {
		var entry Entry
		require.NoError(t, json.Unmarshal(row, &entry), key)
		entries[key] = entry
	}
	s := testStoreWithPricing(nil)
	s.applyPricingData(entries)

	for _, key := range sortedKeys(entries) {
		row := convertEntryToTablePricing(key, entries[key])
		cost := s.CalculateCostForEstimatedImageUsage(&schemas.EstimatedImageUsage{OutputImages: 1},
			schemas.RoutingInfo{Provider: schemas.ModelProvider(row.Provider), Model: row.Model}, schemas.ImageGenerationRequest, nil)
		reason, exempt := estimatedImageTokenExemptions[key]
		switch {
		case exempt && cost > 0:
			t.Errorf("%s is exempt (%q) but its over-limit estimate now prices %g: remove the exemption", key, reason, cost)
		case !exempt && cost <= 0:
			t.Errorf("upstream prices %s's images only per image token, and an over-limit refusal on it charges the budget $0. Add its output tokens per image, with the vendor doc and the date read, to estimatedimagetokens.go, or exempt it in estimatedImageTokenExemptions with the reason", key)
		case exempt && strings.TrimSpace(reason) == "":
			t.Errorf("%s is exempt without a reason", key)
		}
	}
	for _, key := range sortedKeys(estimatedImageTokenExemptions) {
		if _, published := entries[key]; !published {
			t.Errorf("%s is in estimatedImageTokenExemptions, but upstream no longer publishes it with an image token price: remove the exemption", key)
		}
	}
}

func writeUpstreamImageTokenRowsSnapshot(t *testing.T, source string) {
	t.Helper()
	raw, err := os.ReadFile(source)
	require.NoError(t, err)
	var datasheet map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &datasheet))
	rows := map[string]json.RawMessage{}
	for _, key := range sortedKeys(datasheet) {
		var fields struct {
			OutputCostPerImageToken *float64 `json:"output_cost_per_image_token"`
		}
		if json.Unmarshal(datasheet[key], &fields) == nil && fields.OutputCostPerImageToken != nil && *fields.OutputCostPerImageToken > 0 {
			rows[key] = datasheet[key]
		}
	}
	out, err := json.MarshalIndent(upstreamImageTokenRowsSnapshot{
		Source:  "https://getbifrost.ai/datasheet",
		Fetched: os.Getenv("UPSTREAM_DATASHEET_FETCHED"),
		Rows:    rows,
	}, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Clean(upstreamImageTokenRowsSnapshotPath), append(out, '\n'), 0o644))
	t.Logf("wrote %d upstream image token rows to %s", len(rows), upstreamImageTokenRowsSnapshotPath)
}
