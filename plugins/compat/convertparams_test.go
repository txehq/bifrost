package compat

import (
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
)

// An output cap and a thinking budget above the model's catalog limit leave the plugin
// as sent, with should_convert_params on in configuration or on the request: both
// decide what the caller is billed, so the provider delivers or refuses them and the
// plugin never lowers them (contract C4). gemini-test-flash has a 65536 output limit.
func TestPluginNeverRewritesOutputCapOrReasoningBudget(t *testing.T) {
	newPlugin := func(t *testing.T, cfg Config) *CompatPlugin {
		t.Helper()
		ds := datasheet.NewTestStore(nil)
		ds.SetPricingRowsForTest([]configstoreTables.TableModelPricing{
			{Model: "gemini-test-flash", Provider: string(schemas.Gemini), Mode: "chat", MaxOutputTokens: new(65536)},
		})
		p, err := Init(cfg, bifrost.NewNoOpLogger(), modelcatalog.NewTestCatalogWithDatasheet(ds))
		if err != nil {
			t.Fatalf("Init: %v", err)
		}
		return p
	}
	value := func(p *int) int {
		if p == nil {
			return -1
		}
		return *p
	}
	for name, tt := range map[string]struct {
		cfg      Config
		override bool
	}{
		"config on":          {cfg: Config{ShouldConvertParams: true, ShouldDropParams: true}},
		"header override on": {override: true},
	} {
		requests := map[string]*schemas.BifrostRequest{
			"chat": {
				RequestType: schemas.ChatCompletionRequest,
				ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.Gemini, Model: "gemini-test-flash", Params: &schemas.ChatParameters{
					MaxCompletionTokens: new(1000000),
					Reasoning:           &schemas.ChatReasoning{MaxTokens: new(100000)},
				}},
			},
			"responses": newResponsesRequest(schemas.Gemini, "gemini-test-flash", &schemas.ResponsesParameters{
				MaxOutputTokens: new(1000000),
				Reasoning:       &schemas.ResponsesParametersReasoning{MaxTokens: new(100000)},
			}),
			"text": {
				RequestType: schemas.TextCompletionRequest,
				TextCompletionRequest: &schemas.BifrostTextCompletionRequest{Provider: schemas.Gemini, Model: "gemini-test-flash", Params: &schemas.TextCompletionParameters{
					MaxTokens: new(1000000),
				}},
			},
		}
		for shape, req := range requests {
			t.Run(name+"/"+shape, func(t *testing.T) {
				ctx := newTestContext()
				if tt.override {
					ctx.SetValue(schemas.BifrostContextKeyCompatShouldConvertParams, true)
				}
				got, _, err := newPlugin(t, tt.cfg).PreLLMHook(ctx, req)
				if err != nil {
					t.Fatalf("PreLLMHook: %v", err)
				}
				gotMax, gotBudget := -1, 100000
				switch shape {
				case "chat":
					gotMax, gotBudget = value(got.ChatRequest.Params.MaxCompletionTokens), value(got.ChatRequest.Params.Reasoning.MaxTokens)
				case "responses":
					gotMax, gotBudget = value(got.ResponsesRequest.Params.MaxOutputTokens), value(got.ResponsesRequest.Params.Reasoning.MaxTokens)
				case "text":
					gotMax = value(got.TextCompletionRequest.Params.MaxTokens)
				}
				if gotMax != 1000000 || gotBudget != 100000 {
					t.Fatalf("output cap = %d, reasoning budget = %d, want 1000000 and 100000 as sent", gotMax, gotBudget)
				}
			})
		}
	}
}
