package bifrost

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// resourceOverrideID is the resource id every probe sends. It needs no escaping, so the
// override path that reaches the wire carries it verbatim.
const resourceOverrideID = "rid-7"

// resourceOverrideCalls sends, for each request type listed in
// providerUtils.ResourcePathPlaceholders, the call that names a resource by id. A request
// type added to the table without a call here fails TestCustomProviderResourcePathOverrides.
var resourceOverrideCalls = map[schemas.RequestType]func(ctx *schemas.BifrostContext, provider schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError{
	schemas.BatchRetrieveRequest: func(ctx *schemas.BifrostContext, p schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError {
		_, err := p.BatchRetrieve(ctx, keys, &schemas.BifrostBatchRetrieveRequest{Provider: base, BatchID: id})
		return err
	},
	schemas.BatchCancelRequest: func(ctx *schemas.BifrostContext, p schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError {
		_, err := p.BatchCancel(ctx, keys, &schemas.BifrostBatchCancelRequest{Provider: base, BatchID: id})
		return err
	},
	schemas.BatchResultsRequest: func(ctx *schemas.BifrostContext, p schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError {
		_, err := p.BatchResults(ctx, keys, &schemas.BifrostBatchResultsRequest{Provider: base, BatchID: id})
		return err
	},
	schemas.FileRetrieveRequest: func(ctx *schemas.BifrostContext, p schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError {
		_, err := p.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{Provider: base, FileID: id})
		return err
	},
	schemas.FileDeleteRequest: func(ctx *schemas.BifrostContext, p schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError {
		_, err := p.FileDelete(ctx, keys, &schemas.BifrostFileDeleteRequest{Provider: base, FileID: id})
		return err
	},
	schemas.FileContentRequest: func(ctx *schemas.BifrostContext, p schemas.Provider, keys []schemas.Key, base schemas.ModelProvider, id string) *schemas.BifrostError {
		_, err := p.FileContent(ctx, keys, &schemas.BifrostFileContentRequest{Provider: base, FileID: id})
		return err
	},
}

// resourceOverrideGaps are the calls a base serves without honouring their override: it
// sends xAI's or the vendor's own path, or the override with the placeholder still in it.
// Each entry names the ticket that fixes it. An entry is removed when its base honours the
// override (the test fails on a stale entry), and nothing else may be added here: a base
// that templates these calls belongs in providerUtils.ResourcePathOverrideBases instead.
var resourceOverrideGaps = map[schemas.ModelProvider]map[schemas.RequestType]string{
	schemas.Anthropic: {
		schemas.BatchRetrieveRequest: "TXE-7085", schemas.BatchCancelRequest: "TXE-7085", schemas.BatchResultsRequest: "TXE-7085",
		schemas.FileRetrieveRequest: "TXE-7085", schemas.FileDeleteRequest: "TXE-7085", schemas.FileContentRequest: "TXE-7085",
	},
	schemas.Gemini: {
		schemas.BatchRetrieveRequest: "TXE-7085", schemas.BatchCancelRequest: "TXE-7085", schemas.BatchResultsRequest: "TXE-7085",
		schemas.FileRetrieveRequest: "TXE-7085", schemas.FileDeleteRequest: "TXE-7085",
	},
	schemas.ModelArk:   {schemas.FileRetrieveRequest: "TXE-7085", schemas.FileDeleteRequest: "TXE-7085", schemas.FileContentRequest: "TXE-7085"},
	schemas.Volcengine: {schemas.FileRetrieveRequest: "TXE-7085", schemas.FileDeleteRequest: "TXE-7085", schemas.FileContentRequest: "TXE-7085"},
	schemas.Replicate:  {schemas.FileRetrieveRequest: "TXE-7085", schemas.FileDeleteRequest: "TXE-7085"},
	// Bedrock's ids are job ARNs and S3 URIs and its calls go to AWS endpoints; the probe's
	// id is refused before any call, so these never reach the wire here.
	schemas.Bedrock: {
		schemas.BatchRetrieveRequest: "TXE-7085", schemas.BatchCancelRequest: "TXE-7085", schemas.BatchResultsRequest: "TXE-7085",
		schemas.FileRetrieveRequest: "TXE-7085", schemas.FileDeleteRequest: "TXE-7085", schemas.FileContentRequest: "TXE-7085",
	},
}

// resourceOverridePath is the override every probe configures for requestType, and the
// path the call must reach with the id in place of the placeholder.
func resourceOverridePath(requestType schemas.RequestType, id string) string {
	return "/ovr/" + string(requestType) + "/" + id + "/tail"
}

// TestCustomProviderResourcePathOverrides runs every call that names a resource by id
// (providerUtils.ResourcePathPlaceholders) on a custom provider of every base type, with a
// request_path_overrides template for each, and records what reaches the wire (TXE-7084).
// A call a base serves must reach the override's path with the id in place, unless it is a
// recorded gap (resourceOverrideGaps). A base that honours any such override must be in
// providerUtils.ResourcePathOverrideBases, so its overrides are checked when the provider is
// written, and a base listed there may have no gap. A new base, or a new request type in
// the table, that ignores an override or loses the id fails here.
func TestCustomProviderResourcePathOverrides(t *testing.T) {
	upstream := newGuardUpstream(t)
	client := &Bifrost{logger: NewNoOpLogger()}

	requestTypes := make([]schemas.RequestType, 0, len(providerUtils.ResourcePathPlaceholders))
	for requestType := range providerUtils.ResourcePathPlaceholders {
		requestTypes = append(requestTypes, requestType)
	}
	slices.Sort(requestTypes)
	overrides := make(map[schemas.RequestType]string, len(requestTypes))
	for _, requestType := range requestTypes {
		if _, ok := resourceOverrideCalls[requestType]; !ok {
			t.Fatalf("%s is in ResourcePathPlaceholders but resourceOverrideCalls has no call for it", requestType)
		}
		overrides[requestType] = resourceOverridePath(requestType, providerUtils.ResourcePathPlaceholders[requestType])
	}

	for _, base := range schemas.SupportedBaseProviders {
		t.Run(string(base), func(t *testing.T) {
			name := schemas.ModelProvider("custom-" + string(base))
			config := &schemas.ProviderConfig{
				NetworkConfig: schemas.NetworkConfig{BaseURL: "http://" + guardUpstreamHost, DefaultRequestTimeoutInSeconds: 10},
				ProxyConfig: &schemas.ProxyConfig{
					Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(upstream.proxyURL), CACertPEM: schemas.NewSecretVar(upstream.caPEM),
				},
				CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: string(name), BaseProviderType: base, RequestPathOverrides: overrides},
			}
			provider, err := client.createBaseProvider(name, config)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			keys := []schemas.Key{guardKey(base)}
			listed := slices.Contains(providerUtils.ResourcePathOverrideBases, base)
			honoursAny := false
			for _, requestType := range requestTypes {
				upstream.take()
				ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
				var bifrostErr *schemas.BifrostError
				func() {
					defer func() { _ = recover() }()
					bifrostErr = resourceOverrideCalls[requestType](ctx, provider, keys, base, resourceOverrideID)
				}()
				requests := upstream.take()
				targets := make([]string, 0, len(requests))
				for _, request := range requests {
					targets = append(targets, request.target)
				}
				code := ""
				if bifrostErr != nil && bifrostErr.Error != nil && bifrostErr.Error.Code != nil {
					code = *bifrostErr.Error.Code
				}
				served := code != "unsupported_operation" || len(targets) > 0
				want := resourceOverridePath(requestType, resourceOverrideID)
				honoured := false
				for _, target := range targets {
					if strings.Contains(target, want) {
						honoured = true
					}
				}
				honoursAny = honoursAny || honoured
				ticket, gap := resourceOverrideGaps[base][requestType]
				switch {
				case gap && listed:
					t.Errorf("%s is in ResourcePathOverrideBases but carries a gap for %s (%s)", base, requestType, ticket)
				case gap && (honoured || !served):
					t.Errorf("%s %s: the gap entry (%s) is stale: the call now honours the override or is no longer served; remove it", base, requestType, ticket)
				case served && !honoured && !gap:
					t.Errorf("%s %s ignores its request_path_overrides template or loses the id: want a call to %s, sent %s", base, requestType, want, fmt.Sprint(targets))
				}
			}
			if honoursAny && !listed {
				t.Errorf("%s honours resource path overrides but is not in providerUtils.ResourcePathOverrideBases, so they are not checked when the provider is written", base)
			}
		})
	}
}
