package datasheet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

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
	return keys
}

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
	// Video resolution bands under output_cost_per_second_<band>: parsed by TXE-7349.
	"output_cost_per_second_1080p": "TXE-7349: video resolution band",
	"output_cost_per_second_2k":    "TXE-7349: video resolution band",
	"output_cost_per_second_480p":  "TXE-7349: video resolution band",
	"output_cost_per_second_4k":    "TXE-7349: video resolution band",
	"output_cost_per_second_720p":  "TXE-7349: video resolution band",
	"output_cost_per_second_768p":  "TXE-7349: video resolution band",
	// Batch tiers.
	"annotation_cost_per_page_batches":                                     "TXE-7350: batch tier",
	"cache_creation_input_token_cost_above_100k_tokens_batches":            "TXE-7350: batch tier",
	"cache_creation_input_token_cost_above_12hr_above_272k_tokens_batches": "TXE-7350: batch tier",
	"cache_creation_input_token_cost_above_12hr_batches":                   "TXE-7350: batch tier",
	"cache_creation_input_token_cost_above_272k_tokens_batches":            "TXE-7350: batch tier",
	"cache_creation_input_token_cost_batches":                              "TXE-7350: batch tier",
	"cache_creation_input_token_cost_batches_1h":                           "TXE-7350: batch tier",
	"cache_creation_input_token_cost_batches_above_272k_tokens":            "TXE-7350: batch tier",
	"cache_read_input_audio_token_cost_batches":                            "TXE-7350: batch tier",
	"cache_read_input_image_token_cost_batches":                            "TXE-7350: batch tier",
	"cache_read_input_token_cost_above_100k_tokens_batches":                "TXE-7350: batch tier",
	"cache_read_input_token_cost_above_200k_tokens_batches":                "TXE-7350: batch tier",
	"cache_read_input_token_cost_above_272k_tokens_batches":                "TXE-7350: batch tier",
	"cache_read_input_token_cost_batches":                                  "TXE-7350: batch tier",
	"cache_read_input_token_cost_batches_above_272k_tokens":                "TXE-7350: batch tier",
	"input_cost_per_audio_token_above_200k_tokens_batches":                 "TXE-7350: batch tier",
	"input_cost_per_audio_token_batches":                                   "TXE-7350: batch tier",
	"input_cost_per_image_token_batches":                                   "TXE-7350: batch tier",
	"input_cost_per_token_above_100k_tokens_batches":                       "TXE-7350: batch tier",
	"input_cost_per_token_above_200k_tokens_batches":                       "TXE-7350: batch tier",
	"input_cost_per_token_above_272k_tokens_batches":                       "TXE-7350: batch tier",
	"input_cost_per_token_batches_above_100k_tokens":                       "TXE-7350: batch tier",
	"input_cost_per_token_batches_above_272k_tokens":                       "TXE-7350: batch tier",
	"input_cost_per_video_token_batches":                                   "TXE-7350: batch tier",
	"ocr_cost_per_page_batches":                                            "TXE-7350: batch tier",
	"output_cost_per_image_token_batches":                                  "TXE-7350: batch tier",
	"output_cost_per_token_above_100k_tokens_batches":                      "TXE-7350: batch tier",
	"output_cost_per_token_above_200k_tokens_batches":                      "TXE-7350: batch tier",
	"output_cost_per_token_above_272k_tokens_batches":                      "TXE-7350: batch tier",
	"output_cost_per_token_batches_above_100k_tokens":                      "TXE-7350: batch tier",
	"output_cost_per_token_batches_above_272k_tokens":                      "TXE-7350: batch tier",
	// Flex, priority, fast, ultrafast, decisions and balanced tiers.
	"cache_creation_input_token_cost_above_12hr_above_272k_tokens_flex":      "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_12hr_above_272k_tokens_priority":  "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_12hr_above_272k_tokens_ultrafast": "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_12hr_flex":                        "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_12hr_priority":                    "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_12hr_ultrafast":                   "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_272k_tokens_decisions":            "TXE-7350: service tier",
	"cache_creation_input_token_cost_above_272k_tokens_flex":                 "TXE-7350: service tier",
	"cache_creation_input_token_cost_decisions":                              "TXE-7350: service tier",
	"cache_creation_input_token_cost_fast_above_272k_tokens":                 "TXE-7350: service tier",
	"cache_creation_input_token_cost_priority_above_272k_tokens":             "TXE-7350: service tier",
	"cache_read_input_audio_token_cost_flex":                                 "TXE-7350: service tier",
	"cache_read_input_audio_token_cost_priority":                             "TXE-7350: service tier",
	"cache_read_input_token_cost_above_200k_tokens_flex":                     "TXE-7350: service tier",
	"cache_read_input_token_cost_above_272k_tokens_decisions":                "TXE-7350: service tier",
	"cache_read_input_token_cost_above_272k_tokens_flex":                     "TXE-7350: service tier",
	"cache_read_input_token_cost_balanced":                                   "TXE-7350: service tier",
	"cache_read_input_token_cost_decisions":                                  "TXE-7350: service tier",
	"cache_read_input_token_cost_fast_above_272k_tokens":                     "TXE-7350: service tier",
	"cache_read_input_token_cost_priority_above_272k_tokens":                 "TXE-7350: service tier",
	"input_cost_per_audio_token_above_200k_tokens_flex":                      "TXE-7350: service tier",
	"input_cost_per_audio_token_above_200k_tokens_priority":                  "TXE-7350: service tier",
	"input_cost_per_audio_token_flex":                                        "TXE-7350: service tier",
	"input_cost_per_audio_token_priority":                                    "TXE-7350: service tier",
	"input_cost_per_token_above_200k_tokens_flex":                            "TXE-7350: service tier",
	"input_cost_per_token_above_272k_tokens_decisions":                       "TXE-7350: service tier",
	"input_cost_per_token_above_272k_tokens_flex":                            "TXE-7350: service tier",
	"input_cost_per_token_balanced":                                          "TXE-7350: service tier",
	"input_cost_per_token_fast_above_272k_tokens":                            "TXE-7350: service tier",
	"input_cost_per_token_priority_above_272k_tokens":                        "TXE-7350: service tier",
	"output_cost_per_image_token_priority":                                   "TXE-7350: service tier",
	"output_cost_per_token_above_200k_tokens_flex":                           "TXE-7350: service tier",
	"output_cost_per_token_above_272k_tokens_decisions":                      "TXE-7350: service tier",
	"output_cost_per_token_above_272k_tokens_flex":                           "TXE-7350: service tier",
	"output_cost_per_token_balanced":                                         "TXE-7350: service tier",
	"output_cost_per_token_fast_above_272k_tokens":                           "TXE-7350: service tier",
	"output_cost_per_token_priority_above_272k_tokens":                       "TXE-7350: service tier",
	// 12-hour cache write.
	"cache_creation_input_token_cost_above_12hr":                   "TXE-7350: 12-hour cache write",
	"cache_creation_input_token_cost_above_12hr_above_272k_tokens": "TXE-7350: 12-hour cache write",
	// Databricks DBU costs.
	"cache_creation_input_token_dbu_cost": "TXE-7350: Databricks DBU",
	"cache_read_input_token_dbu_cost":     "TXE-7350: Databricks DBU",
	"input_dbu_cost_per_token":            "TXE-7350: Databricks DBU",
	"output_dbu_cost_per_token":           "TXE-7350: Databricks DBU",
	// Long-context thresholds the fork has no tier for.
	"cache_read_input_token_cost_above_128k_tokens": "TXE-7350: long-context threshold",
	"cache_read_input_token_cost_above_256k_tokens": "TXE-7350: long-context threshold",
	"cache_read_input_token_cost_above_32k_tokens":  "TXE-7350: long-context threshold",
	"cache_read_input_token_cost_above_512k_tokens": "TXE-7350: long-context threshold",
	"input_cost_per_audio_token_above_200k_tokens":  "TXE-7350: long-context threshold",
	"input_cost_per_character_above_128k_tokens":    "TXE-7350: long-context threshold",
	"input_cost_per_token_above_256k_tokens":        "TXE-7350: long-context threshold",
	"input_cost_per_token_above_32k_tokens":         "TXE-7350: long-context threshold",
	"input_cost_per_token_above_512k_tokens":        "TXE-7350: long-context threshold",
	"output_cost_per_character_above_128k_tokens":   "TXE-7350: long-context threshold",
	"output_cost_per_token_above_256k_tokens":       "TXE-7350: long-context threshold",
	"output_cost_per_token_above_32k_tokens":        "TXE-7350: long-context threshold",
	"output_cost_per_token_above_512k_tokens":       "TXE-7350: long-context threshold",
	// Video mode tiers, the video-input rate, per-clip prices and video tokens.
	"input_cost_per_video_per_second_above_15s_interval": "TXE-7350: video",
	"input_cost_per_video_per_second_above_8s_interval":  "TXE-7350: video",
	"input_cost_per_video_token":                         "TXE-7350: video",
	"output_cost_per_video":                              "TXE-7350: video",
	"output_cost_per_video_1080p_6s":                     "TXE-7350: video",
	"output_cost_per_video_768p_10s":                     "TXE-7350: video",
	"output_cost_per_video_768p_6s":                      "TXE-7350: video",
	"output_cost_per_video_per_second_1080p_video_in":    "TXE-7350: video",
	"output_cost_per_video_per_second_480p_video_in":     "TXE-7350: video",
	"output_cost_per_video_per_second_4k_video_in":       "TXE-7350: video",
	"output_cost_per_video_per_second_720p_video_in":     "TXE-7350: video",
	"output_cost_per_video_per_second_pro":               "TXE-7350: video",
	"output_cost_per_video_per_second_pro_audio":         "TXE-7350: video",
	"output_cost_per_video_per_second_standard":          "TXE-7350: video",
	"output_cost_per_video_per_second_standard_audio":    "TXE-7350: video",
	"output_cost_per_video_token":                        "TXE-7350: video",
	// Image size, quality and step variants.
	"output_cost_per_image_0.5K":                                        "TXE-7350: image variant",
	"output_cost_per_image_1024":                                        "TXE-7350: image variant",
	"output_cost_per_image_1536":                                        "TXE-7350: image variant",
	"output_cost_per_image_1K":                                          "TXE-7350: image variant",
	"output_cost_per_image_2K":                                          "TXE-7350: image variant",
	"output_cost_per_image_4K":                                          "TXE-7350: image variant",
	"output_cost_per_image_512":                                         "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1024_pixels_50_steps":         "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1024_pixels_hd_quality":       "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1024_pixels_max_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1024_pixels_max_steps":        "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1024_pixels_xhigh_quality":    "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1536_pixels_max_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1536_pixels_xhigh_quality":    "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1792_pixels_hd_quality":       "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_1792_pixels_standard_quality": "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_768_pixels_high_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_768_pixels_low_quality":       "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_768_pixels_max_quality":       "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_768_pixels_medium_quality":    "TXE-7350: image variant",
	"output_cost_per_image_above_1024_and_768_pixels_xhigh_quality":     "TXE-7350: image variant",
	"output_cost_per_image_above_1792_and_1024_pixels_hd_quality":       "TXE-7350: image variant",
	"output_cost_per_image_above_1792_and_1024_pixels_standard_quality": "TXE-7350: image variant",
	"output_cost_per_image_above_1920_and_1080_pixels_high_quality":     "TXE-7350: image variant",
	"output_cost_per_image_above_1920_and_1080_pixels_low_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_1920_and_1080_pixels_max_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_1920_and_1080_pixels_medium_quality":   "TXE-7350: image variant",
	"output_cost_per_image_above_1920_and_1080_pixels_xhigh_quality":    "TXE-7350: image variant",
	"output_cost_per_image_above_2560_and_1440_pixels_high_quality":     "TXE-7350: image variant",
	"output_cost_per_image_above_2560_and_1440_pixels_low_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_2560_and_1440_pixels_max_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_2560_and_1440_pixels_medium_quality":   "TXE-7350: image variant",
	"output_cost_per_image_above_2560_and_1440_pixels_xhigh_quality":    "TXE-7350: image variant",
	"output_cost_per_image_above_3840_and_2160_pixels_high_quality":     "TXE-7350: image variant",
	"output_cost_per_image_above_3840_and_2160_pixels_low_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_3840_and_2160_pixels_max_quality":      "TXE-7350: image variant",
	"output_cost_per_image_above_3840_and_2160_pixels_medium_quality":   "TXE-7350: image variant",
	"output_cost_per_image_above_3840_and_2160_pixels_xhigh_quality":    "TXE-7350: image variant",
	"output_cost_per_image_above_512_and_512_pixels_50_steps":           "TXE-7350: image variant",
	"output_cost_per_image_above_512_and_512_pixels_max_steps":          "TXE-7350: image variant",
	"output_cost_per_image_above_max_and_max_pixels_50_steps":           "TXE-7350: image variant",
	"output_cost_per_image_above_max_and_max_pixels_max_steps":          "TXE-7350: image variant",
	// Everything else.
	"cache_read_input_audio_token_cost":    "TXE-7350: other",
	"citation_cost_per_token":              "TXE-7350: other",
	"cost_per_second":                      "TXE-7350: other",
	"explicit_cache_read_input_token_cost": "TXE-7350: other",
	"google_maps_grounding_cost_per_query": "TXE-7350: other",
	"guardrail_cost_per_unit":              "TXE-7350: other",
	"input_cost_per_request":               "TXE-7350: other",
	"input_cost_per_text_message":          "TXE-7350: other",
	"input_cost_per_token_cache_hit":       "TXE-7350: other",
	"ocr_cost_per_credit":                  "TXE-7350: other",
	"output_cost_per_billable_unit":        "TXE-7350: other",
	"output_cost_per_character":            "TXE-7350: other",
	"output_cost_per_reasoning_token":      "TXE-7350: other",
	"output_cost_per_unit":                 "TXE-7350: other",
}
