package datasheet

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// dbuToUSD pairs each Databricks DBU cost key with the USD key that states the same rate. The fork
// prices Databricks rows from the USD keys and leaves the DBU keys unparsed (TXE-7359), which is
// safe only while every DBU rate has its USD counterpart at the same price.
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

// dbuUSDDisagreements are the rates whose USD key disagrees with DBU x usdPerDBU in the snapshot,
// keyed "row|dbu key", each with what was found. The fork bills the USD key, so each is a
// candidate misprice until it is resolved (TXE-7688); a new disagreement fails the test, and one
// that agrees again must leave the list.
var dbuUSDDisagreements = map[string]string{
	"databricks/databricks-gemini-3-pro|input_dbu_cost_per_token":                "35.714 DBU per 1M ($2.50 at $0.07, Databricks' current list rate) against 2e-06 USD ($2.00): the USD rate carries the 20% Gemini promotion Databricks ended on 30 Jun 2026 (TXE-7688)",
	"databricks/databricks-gemini-3-pro|output_dbu_cost_per_token":               "214.286 DBU per 1M ($15.00 at $0.07) against 1.2e-05 USD ($12.00): the same ended 20% promotion (TXE-7688)",
	"databricks/databricks-gemini-3-6-flash|cache_read_input_token_dbu_cost":     "2.679e-06 DBU (1.875e-07 USD at $0.07) against 7.497e-08 USD, 0.4x: no published source explains the gap (TXE-7688)",
	"databricks/databricks-gemini-3-6-flash|cache_creation_input_token_dbu_cost": "2.6786e-05 DBU (1.875e-06 USD at $0.07) against 7.4998e-07 USD, 0.4x: no published source explains the gap (TXE-7688)",
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
			agrees := math.Abs(*usd-want) <= 0.01*math.Max(math.Abs(want), math.Abs(*usd))
			reason, known := dbuUSDDisagreements[id]
			switch {
			case !agrees && !known:
				t.Errorf("%s: %s is %g, but %s is %g DBU, %g USD at $%g/DBU. The fork bills the USD rate; find which is right and record it in dbuUSDDisagreements with the evidence, or fix the row", row, usdKey, *usd, dbuKey, *dbu, want, usdPerDBU)
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
