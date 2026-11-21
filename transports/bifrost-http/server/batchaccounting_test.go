package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/network"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	cstables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/jobaccounting"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/plugins/governance"
	"github.com/capsohq/bifrost/plugins/logging"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/handlers"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

// The sweeper skips the plugin pipeline, so nothing upstream resolves a key for it.
// Pinning to the key that created the job is what stops core from polling with an
// arbitrary one — which for a video is not a preference but a correctness matter:
// OpenAI scopes a video id to its creating key.
func TestInternalJobContextPinsTheCreatingKey(t *testing.T) {
	ctx := internalJobContext(context.Background(), "key-abc")

	keyID, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string)
	if !ok || keyID != "key-abc" {
		t.Fatalf("api key id = %q (ok=%v), want key-abc", keyID, ok)
	}
}

// BifrostContextKeyAPIKeyID is a reserved key: writes to it are dropped while the
// plugin pipeline holds restricted writes. This context never enters that phase, so
// the write must land — if that ever changes, the pin silently stops working and
// the sweeper goes back to polling with whatever key core picks.
func TestInternalJobContextReservedWriteIsNotDropped(t *testing.T) {
	ctx := internalJobContext(context.Background(), "key-reserved")
	if _, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string); !ok {
		t.Fatal("a reserved-key write on a fresh sweeper context must not be dropped")
	}
	// Same reasoning as the skip flags this context has always set.
	if skip, ok := ctx.Value(schemas.BifrostContextKeySkipBudgetAndRateLimits).(bool); !ok || !skip {
		t.Fatal("expected the sweeper context to keep skipping budgets")
	}
}

// A job row written before the key was recorded, or by a path that never had one,
// must still poll rather than pin to the empty string and match nothing.
func TestInternalJobContextWithoutKeyLeavesSelectionOpen(t *testing.T) {
	for _, selectedKeyID := range []string{"", "   "} {
		ctx := internalJobContext(context.Background(), selectedKeyID)
		if _, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string); ok {
			t.Fatalf("selectedKeyID %q must leave key selection unpinned", selectedKeyID)
		}
	}
}

// The sweeper's own invariants must survive the pin.
func TestInternalJobContextKeepsSweeperInvariants(t *testing.T) {
	ctx := internalJobContext(context.Background(), "key-abc")

	if skip, ok := ctx.Value(schemas.BifrostContextKeySkipPluginPipeline).(bool); !ok || !skip {
		t.Fatal("the sweeper must not re-enter the plugin pipeline")
	}
	if requestID, ok := ctx.Value(schemas.BifrostContextKeyRequestID).(string); !ok || requestID == "" {
		t.Fatal("every sweeper call needs its own request id")
	}
	// Core's allowed_requests check exempts the sweeper's polls (TXE-7370), so a job a
	// caller was allowed to create is still billed.
	if exempt, ok := ctx.Value(schemas.BifrostContextKeyProviderJobAccounting).(bool); !ok || !exempt {
		t.Fatal("the sweeper's polls must carry the job-accounting marker")
	}
}

// Both job kinds poll through the same pinned context. An earlier revision gave
// batch an unpinned retry on failure, on the theory that a batch id is org-scoped
// so any key could rescue it. That fires on every transient error — an outage, a
// rate limit — doubling load on an upstream already failing, to cover only a key
// deleted mid-flight; and that case is already answered by the job parking as
// unpriceable and by /v1/batches/{id}/results settling inline with the caller's own
// key. One code path, no retry.
func TestInternalJobContextIsTheOnlySweeperContext(t *testing.T) {
	batchCtx := internalJobContext(context.Background(), "key-batch")
	videoCtx := internalJobContext(context.Background(), "key-video")

	for name, ctx := range map[string]*schemas.BifrostContext{"batch": batchCtx, "video": videoCtx} {
		if _, ok := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string); !ok {
			t.Fatalf("%s: both kinds must poll pinned to their creating key", name)
		}
	}
}

// The video settler recognises a custom provider through this resolver, so a custom provider
// built on a video provider has its jobs settled and billed.
func TestBifrostVideoRetrieverResolvesCustomBaseProvider(t *testing.T) {
	retriever := &bifrostVideoRetriever{config: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
		"my-minimax":    {CustomProviderConfig: &schemas.CustomProviderConfig{CustomProviderKey: "my-minimax", BaseProviderType: schemas.MiniMax}},
		schemas.MiniMax: {},
	}}}
	if got := retriever.BaseProvider("my-minimax"); got != schemas.MiniMax {
		t.Errorf("BaseProvider(my-minimax) = %q, want minimax", got)
	}
	if got := retriever.BaseProvider(schemas.MiniMax); got != "" {
		t.Errorf("BaseProvider(minimax) = %q, want empty for a built-in provider", got)
	}
	if got := retriever.BaseProvider("unknown"); got != "" {
		t.Errorf("BaseProvider(unknown) = %q, want empty", got)
	}
	if got := (&bifrostVideoRetriever{}).BaseProvider("my-minimax"); got != "" {
		t.Errorf("a retriever without config resolved %q", got)
	}
	var _ jobaccounting.BaseProviderResolver = retriever
}

// The video settler fetches a rendered video on the provider's configured base URL origin whatever
// its host list says (fork, TXE-7386): the origin is scheme://host[:port] of network_config.base_url,
// and "" when none is configured or it does not parse.
func TestBifrostVideoRetrieverReportsTheProviderOrigin(t *testing.T) {
	retriever := &bifrostVideoRetriever{config: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.Replicate: {NetworkConfig: &schemas.NetworkConfig{BaseURL: "http://127.0.0.1:8791/pp/replicate"}},
		"my-replicate":    {NetworkConfig: &schemas.NetworkConfig{BaseURL: "https://proxy.internal.example/replicate/"}},
		schemas.MiniMax:   {},
		"broken":          {NetworkConfig: &schemas.NetworkConfig{BaseURL: "not a url"}},
	}}}
	for provider, want := range map[schemas.ModelProvider]string{
		schemas.Replicate: "http://127.0.0.1:8791",
		"my-replicate":    "https://proxy.internal.example",
		schemas.MiniMax:   "",
		"broken":          "",
		"unknown":         "",
	} {
		if got := retriever.ProviderOrigin(provider); got != want {
			t.Errorf("ProviderOrigin(%s) = %q, want %q", provider, got, want)
		}
	}
	if got := (&bifrostVideoRetriever{}).ProviderOrigin(schemas.Replicate); got != "" {
		t.Errorf("a retriever without config resolved %q", got)
	}
	var _ jobaccounting.ProviderOriginResolver = retriever
}

// client.disable_provider_job_accounting (fork, TXE-6519) reaches the logging plugin through this
// wiring, with or without a client: a disabled deployment records no provider job and starts no
// sweeper, and the default leaves accounting on, as before the switch existed.
func TestWireBatchAccountingSweeperAppliesProviderJobAccountingSwitch(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	for _, tc := range []struct {
		name     string
		disabled bool
	}{{"default keeps accounting on", false}, {"switched off", true}} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := &logging.LoggerPlugin{}
			governancePlugin, err := governance.Init(t.Context(), &governance.Config{}, bifrost.NewNoOpLogger(), nil, &configstore.GovernanceConfig{}, nil, nil, nil)
			if err != nil {
				t.Fatalf("governance.Init() error = %v", err)
			}
			t.Cleanup(func() { _ = governancePlugin.Cleanup() })
			config := &lib.Config{ClientConfig: &configstore.ClientConfig{DisableProviderJobAccounting: tc.disabled}}
			config.BasePlugins.Store(&[]schemas.BasePlugin{plugin, governancePlugin})
			s := &BifrostHTTPServer{Config: config}
			s.WireBatchAccountingSweeper()
			if got := plugin.ProviderJobAccountingEnabled(); got != !tc.disabled {
				t.Fatalf("ProviderJobAccountingEnabled() = %v with disable_provider_job_accounting=%v", got, tc.disabled)
			}
			// Governance admits a settled job under a dollar budget only when it is settled (TXE-7963).
			if got := governancePlugin.ProviderJobAccountingEnabled(); got != !tc.disabled {
				t.Fatalf("governance ProviderJobAccountingEnabled() = %v with disable_provider_job_accounting=%v", got, tc.disabled)
			}
			// With the logging plugin gone nothing settles a job, so governance is told accounting
			// is off, whatever the switch says (RemovePlugin rewires when logging is removed).
			config.BasePlugins.Store(&[]schemas.BasePlugin{governancePlugin})
			s.WireBatchAccountingSweeper()
			if governancePlugin.ProviderJobAccountingEnabled() {
				t.Fatalf("governance kept provider job accounting on after the logging plugin was removed (disable_provider_job_accounting=%v)", tc.disabled)
			}
		})
	}
}

// A client-config save that flips the switch is honoured without a restart. The save path
// publishes the new client config into the live struct and only then asks for the reload
// (handlers/config.go), so the reload cannot see the change in the config; it reconciles the
// logging plugin's own state instead (fork, TXE-6519).
func TestReloadClientConfigReconcilesProviderJobAccounting(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	ctx := context.Background()
	store := newSQLiteConfigStore(t)

	plugin := &logging.LoggerPlugin{}
	live := lib.DefaultClientConfig
	config := &lib.Config{ClientConfig: &live, ConfigStore: store}
	config.BasePlugins.Store(&[]schemas.BasePlugin{plugin})
	s := &BifrostHTTPServer{Config: config}
	s.WireBatchAccountingSweeper()
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("accounting must start on with the default client config")
	}

	save := func(disabled bool) {
		t.Helper()
		stored := lib.DefaultClientConfig
		stored.DisableProviderJobAccounting = disabled
		if err := store.UpdateClientConfig(ctx, &stored); err != nil {
			t.Fatalf("UpdateClientConfig() error = %v", err)
		}
		// What the config handler does before it asks for the reload: the live config
		// already carries the saved value when ReloadClientConfigFromConfigStore runs.
		config.Mu.Lock()
		*config.ClientConfig = stored
		config.Mu.Unlock()
		if err := s.ReloadClientConfigFromConfigStore(ctx); err != nil {
			t.Fatalf("ReloadClientConfigFromConfigStore() error = %v", err)
		}
	}

	save(true)
	if plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a reload after a save that sets the switch must stop provider job accounting")
	}
	save(false)
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a reload after a save that clears the switch must resume provider job accounting")
	}
}

// The whole save path, PUT /api/config through the real config handler with this server as its
// manager, flips the logging plugin without a restart (fork, TXE-6519).
func TestConfigAPISaveFlipsProviderJobAccounting(t *testing.T) {
	SetLogger(bifrost.NewNoOpLogger())
	handlers.SetLogger(bifrost.NewNoOpLogger())
	ctx := context.Background()
	store := newSQLiteConfigStore(t)

	plugin := &logging.LoggerPlugin{}
	live := lib.DefaultClientConfig
	config := &lib.Config{ClientConfig: &live, ConfigStore: store}
	config.BasePlugins.Store(&[]schemas.BasePlugin{plugin})
	if err := store.UpdateClientConfig(ctx, &live); err != nil {
		t.Fatalf("UpdateClientConfig() error = %v", err)
	}
	s := &BifrostHTTPServer{Config: config}
	s.WireBatchAccountingSweeper()
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("accounting must start on with the default client config")
	}

	r := router.New()
	handlers.NewConfigHandler(s, config).RegisterRoutes(r)
	put := func(body string) {
		t.Helper()
		var req fasthttp.Request
		req.Header.SetMethod(fasthttp.MethodPut)
		req.SetRequestURI("/api/config")
		req.Header.SetContentType("application/json")
		req.SetBodyString(body)
		rc := &fasthttp.RequestCtx{}
		rc.Init(&req, nil, nil)
		r.Handler(rc)
		if rc.Response.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("PUT /api/config = %d: %s", rc.Response.StatusCode(), rc.Response.Body())
		}
	}

	// The compat block is sent as the live config holds it: an omitted block decodes to the
	// compat defaults, and the handler would then reload the compat plugin through a client
	// this test does not have. The save under test is the switch alone.
	compat := `"compat":{"convert_text_to_chat":false,"convert_chat_to_responses":false,"should_drop_params":false,"should_convert_params":false,"azure_deepseek":false,"force_reasoning_only_models_to_responses":true,"convert_decision_to_responses":false}`
	put(`{"client_config":{"log_retention_days":8,` + compat + `,"disable_provider_job_accounting":true}}`)
	if plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a save that sets the switch must stop provider job accounting")
	}
	if config.ShouldRunProviderJobAccounting() {
		t.Fatal("the live config must carry the saved switch")
	}
	put(`{"client_config":{"log_retention_days":8,` + compat + `,"disable_provider_job_accounting":false}}`)
	if !plugin.ProviderJobAccountingEnabled() {
		t.Fatal("a save that clears the switch must resume provider job accounting")
	}
}

// newSQLiteConfigStore is a fresh config store with every migration applied, closed with the test.
func newSQLiteConfigStore(t *testing.T) configstore.ConfigStore {
	t.Helper()
	ctx := context.Background()
	store, err := configstore.NewConfigStore(ctx, &configstore.Config{
		Enabled: true,
		Type:    configstore.ConfigStoreTypeSQLite,
		Config:  &configstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, bifrost.NewNoOpLogger())
	if err != nil {
		t.Fatalf("NewConfigStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })
	return store
}

// replicateVideoAccount configures replicate alone, at a loopback stand-in.
type replicateVideoAccount struct{ baseURL string }

func (a *replicateVideoAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Replicate}, nil
}

func (a *replicateVideoAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "k", Name: "k", Value: *schemas.NewSecretVar("test-key"), Weight: 1, Models: schemas.WhiteList{"*"}}}, nil
}

func (a *replicateVideoAccount) GetConfigForProvider(_ schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: a.baseURL, DefaultRequestTimeoutInSeconds: 5, MaxRetries: 0, AllowPrivateNetwork: true,
	}}, nil
}

// TXE-7329: the sweeper's retriever downloads a finished Replicate prediction's video through
// core, and the settler prices the length it rendered. The prediction was asked for 5 seconds
// and its output is TXE-6774's 12-second ffmpeg MP4; Replicate's prediction reports neither.
// A file past client.max_response_body_size_mb (TXE-7148) is measured from the streamed download.
func TestBifrostVideoRetrieverMeasuresTheRenderedReplicateVideo(t *testing.T) {
	video, err := os.ReadFile("../../../framework/videoduration/testdata/twelve-seconds.mp4")
	if err != nil {
		t.Fatal(err)
	}
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/predictions/pred_long":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"pred_long","status":"succeeded","output":%q}`, upstream.URL+"/files/out.mp4")
		case "/files/out.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(video)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &replicateVideoAccount{baseURL: upstream.URL}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Shutdown)

	// The server's config names the same base URL the account does; the output on that origin is
	// one the settler may fetch (TXE-7386).
	retriever := &bifrostVideoRetriever{client: client, config: &lib.Config{Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
		schemas.Replicate: {NetworkConfig: &schemas.NetworkConfig{BaseURL: upstream.URL}},
	}}}
	var _ jobaccounting.VideoDownloader = retriever
	seconds := 5
	params, err := jobaccounting.MarshalVideoDimensions(modelcatalog.VideoPricingDimensions{Model: "wan-video/wan-2.5-t2v", Seconds: &seconds})
	if err != nil {
		t.Fatal(err)
	}
	// A job row keeps the video id the create answered, which carries the provider suffix.
	job := &cstables.TableProviderJob{
		ID:       cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.Replicate), "pred_long:replicate"),
		Kind:     cstables.ProviderJobKindVideo,
		Provider: string(schemas.Replicate),
		JobID:    "pred_long:replicate",
		Params:   params,
	}

	poll, err := jobaccounting.NewVideoSettler(retriever).Poll(context.Background(), job)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	payload, _ := poll.Payload.(jobaccounting.VideoPayload)
	if !poll.Settleable || payload.RenderedSeconds == nil || *payload.RenderedSeconds != 12 {
		t.Fatalf("poll = %+v, rendered seconds %v; want settleable at the 12 rendered seconds", poll, payload.RenderedSeconds)
	}

	// TXE-7334: the download is streamed, so a file past client.max_response_body_size_mb (TXE-7148)
	// is measured too. The same clip with 3 MB more media in its mdat, against a 1 MB limit.
	network.SetMaxResponseBodySizeMB(1)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })
	video = inflateMdat(t, video, 3<<20)
	poll, err = jobaccounting.NewVideoSettler(retriever).Poll(context.Background(), job)
	if err != nil {
		t.Fatalf("Poll past the limit: %v", err)
	}
	if payload, _ := poll.Payload.(jobaccounting.VideoPayload); !poll.Settleable || payload.RenderedSeconds == nil || *payload.RenderedSeconds != 12 {
		t.Fatalf("poll of a %d-byte file past the 1 MB limit = %+v (%q); want the 12 rendered seconds", len(video), payload.RenderedSeconds, payload.RenderedUnmeasurable)
	}
}

// inflateMdat returns the MP4 with n zero bytes added after the media in its top-level mdat. The
// samples' chunk offsets point before them, so the clip is the same; a moov after the mdat moves.
func inflateMdat(t *testing.T, data []byte, n int) []byte {
	t.Helper()
	for at := 0; at+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[at:]))
		if size < 8 || at+size > len(data) {
			t.Fatalf("a box at byte %d of %d bytes", at, size)
		}
		if string(data[at+4:at+8]) == "mdat" {
			out := make([]byte, 0, len(data)+n)
			out = append(out, data[:at+size]...)
			binary.BigEndian.PutUint32(out[at:], uint32(size+n))
			out = append(out, make([]byte, n)...)
			return append(out, data[at+size:]...)
		}
		at += size
	}
	t.Fatal("no mdat")
	return nil
}

// openAIBatchAccount configures openai alone, at a loopback stand-in.
type openAIBatchAccount struct{ baseURL string }

func (a *openAIBatchAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (a *openAIBatchAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "k", Name: "k", Value: *schemas.NewSecretVar("test-key"), Weight: 1, Models: schemas.WhiteList{"*"}, UseForBatchAPI: schemas.Ptr(true)}}, nil
}

func (a *openAIBatchAccount) GetConfigForProvider(_ schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: a.baseURL, DefaultRequestTimeoutInSeconds: 5, MaxRetries: 0, AllowPrivateNetwork: true,
	}}, nil
}

// TXE-7494 and TXE-7513: every results read is held to client.max_response_body_size_mb, the
// sweeper's included, and a finished batch past it is still billed in full: the settler walks
// the pages through core against a loopback OpenAI and prices every line.
func TestBatchSettlerBillsAResultSetPastTheResponseLimitInFull(t *testing.T) {
	const lines = 1500
	var output []byte
	for i := range lines {
		output = fmt.Appendf(output, `{"id":"r%d","custom_id":"c%d","response":{"status_code":200,"body":{"object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"%0900d"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}}}`+"\n", i, i, 0)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/batches/batch_big":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"batch_big","object":"batch","endpoint":"/v1/chat/completions","status":"completed","output_file_id":"file_out"}`)
		case "/v1/files/file_out/content":
			w.Header().Set("Content-Type", "application/jsonl")
			_, _ = w.Write(output)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &openAIBatchAccount{baseURL: upstream.URL}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Shutdown)

	network.SetMaxResponseBodySizeMB(1)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })
	if len(output) <= 1<<20 {
		t.Fatalf("the result file is %d bytes, not past the 1 MiB limit", len(output))
	}
	job := &cstables.TableProviderJob{Kind: cstables.ProviderJobKindBatch, Provider: string(schemas.OpenAI), JobID: "batch_big"}

	// A caller's unpaged read of the same batch is refused and told to page.
	_, bifrostErr := client.BatchResultsRequest(internalJobContext(context.Background(), ""), &schemas.BifrostBatchResultsRequest{Provider: schemas.OpenAI, BatchID: "batch_big"})
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest || bifrostErr.Error.Param != "limit" {
		t.Fatalf("unpaged caller read: error = %+v, want a 400 naming limit", bifrostErr)
	}

	// One page of the sweeper's read is held to the limit like any other read (TXE-7513).
	first, err := (&bifrostBatchResultFetcher{client: client}).FetchBatchResultsPage(context.Background(), job, nil)
	if err != nil {
		t.Fatalf("FetchBatchResultsPage: %v", err)
	}
	if len(first.Results) == 0 || len(first.Results) >= lines || !first.HasMore || first.NextCursor == nil {
		t.Fatalf("the first settlement page held %d of %d lines (has_more %v); want a page within the limit", len(first.Results), lines, first.HasMore)
	}

	// The settler walks the pages and bills every line: the reason the exemption existed.
	settler := jobaccounting.NewBatchSettler(&bifrostBatchResultFetcher{client: client})
	poll, err := settler.Poll(context.Background(), job)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !poll.Settleable {
		t.Fatalf("poll = %+v, want settleable", poll)
	}
	settlement, err := settler.Settle(context.Background(), perTokenBatchPricing{}, jobaccounting.JobRequest{Provider: schemas.OpenAI, ProviderJobID: "batch_big", Job: poll.Job, Payload: poll.Payload})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if settlement.PricedCount != lines || settlement.Usage.PromptTokens != 3*lines || settlement.Usage.CompletionTokens != 2*lines {
		t.Fatalf("settled %d lines, %d prompt and %d completion tokens; want all %d lines, %d and %d", settlement.PricedCount, settlement.Usage.PromptTokens, settlement.Usage.CompletionTokens, lines, 3*lines, 2*lines)
	}
	if want := float64(5*lines) * 1e-06; math.Abs(settlement.Cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v for the whole batch", settlement.Cost, want)
	}
}

// TXE-7526: one result line larger than client.max_response_body_size_mb fits no page. Before,
// every poll of such a batch failed, and it was parked unbilled after MaxPollAttempts. Now the
// line is read from a slim copy and billed from its usage like any other line. When the slim
// copy shows no usage, or the line has none at all, every other line is billed and the row
// says how much it is short.
func TestBatchSettlerBillsABatchWithALinePastTheLimit(t *testing.T) {
	line := func(i int, pad int) []byte {
		return fmt.Appendf(nil, `{"id":"r%d","custom_id":"c%d","response":{"status_code":200,"body":{"object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"%s"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}}}`+"\n", i, i, strings.Repeat("x", pad))
	}
	big := strings.Repeat("x", 1200<<10) // 1.2 MiB: past the 1 MiB limit on its own
	const lines = 600
	for _, tc := range []struct {
		name                         string
		huge                         string
		results, parseErrors, priced int
		unbilled                     int
		incomplete                   bool
	}{
		{
			name:    "the slim copy has its usage: billed in full",
			huge:    string(line(300, len(big))),
			results: lines, priced: lines,
		},
		{
			name:    "the line carries no usage: unbilled, counted, incomplete",
			huge:    `{"id":"r300","custom_id":"c300","response":{"status_code":200,"body":{"object":"chat.completion","model":"gpt-4o-mini","choices":[{"message":{"content":"` + big + `"}}]}}}` + "\n",
			results: lines, priced: lines - 1, unbilled: 1, incomplete: true,
		},
		{
			name:    "the line is not JSON: a parse error, incomplete",
			huge:    `not json ` + big + "\n",
			results: lines - 1, parseErrors: 1, priced: lines - 1, incomplete: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testBatchSettlerWithAnOversizedLine(t, lines, line, tc.huge, tc.results, tc.parseErrors, tc.priced, tc.unbilled, tc.incomplete)
		})
	}
}

func testBatchSettlerWithAnOversizedLine(t *testing.T, lines int, line func(i, pad int) []byte, huge string, wantResults, wantParseErrors, wantPriced, wantUnbilled int, wantIncomplete bool) {
	var output []byte
	for i := range lines {
		if i == 300 {
			output = append(output, huge...)
			continue
		}
		output = append(output, line(i, 900)...)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/batches/batch_oversized":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"batch_oversized","object":"batch","endpoint":"/v1/chat/completions","status":"completed","output_file_id":"file_out"}`)
		case "/v1/files/file_out/content":
			w.Header().Set("Content-Type", "application/jsonl")
			_, _ = w.Write(output)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &openAIBatchAccount{baseURL: upstream.URL}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Shutdown)
	network.SetMaxResponseBodySizeMB(1)
	t.Cleanup(func() { network.SetMaxResponseBodySizeMB(0) })

	job := &cstables.TableProviderJob{Kind: cstables.ProviderJobKindBatch, Provider: string(schemas.OpenAI), JobID: "batch_oversized"}
	if len(output) < 1<<20 {
		t.Fatalf("the result file is %d bytes, so its long line is not past the 1 MiB limit", len(output))
	}
	settler := jobaccounting.NewBatchSettler(&bifrostBatchResultFetcher{client: client})
	poll, err := settler.Poll(context.Background(), job)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !poll.Settleable {
		t.Fatalf("poll = %+v: a batch with one line past the limit was not settleable", poll)
	}
	payload := poll.Payload.(jobaccounting.BatchPayload)
	if len(payload.Results) != wantResults || len(payload.ParseErrors) != wantParseErrors {
		t.Fatalf("payload holds %d lines and parse errors %+v; want %d lines and %d parse errors", len(payload.Results), payload.ParseErrors, wantResults, wantParseErrors)
	}
	for _, parseErr := range payload.ParseErrors {
		if parseErr.Code != "line_too_large" {
			t.Fatalf("parse error %+v, want line_too_large", parseErr)
		}
	}
	truncated := 0
	for _, item := range payload.Results {
		if item.Truncated != nil {
			truncated++
			if item.CustomID != "c300" || item.Truncated.Reason != "line_too_large" || item.Truncated.Bytes != int64(len(huge)) {
				t.Fatalf("truncated item %s: %+v, want c300 marked line_too_large at %d bytes", item.CustomID, item.Truncated, len(huge))
			}
		}
	}
	if truncated != wantResults-(lines-1) {
		t.Fatalf("%d items are marked truncated, want %d", truncated, wantResults-(lines-1))
	}
	settlement, err := settler.Settle(context.Background(), perTokenBatchPricing{}, jobaccounting.JobRequest{Provider: schemas.OpenAI, ProviderJobID: "batch_oversized", Job: poll.Job, Payload: poll.Payload})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if settlement.PricedCount != wantPriced || settlement.Usage.TotalTokens != 5*wantPriced || settlement.UnbilledLines != wantUnbilled {
		t.Fatalf("settled %d lines, %d tokens, %d unbilled; want %d lines, %d tokens, %d unbilled", settlement.PricedCount, settlement.Usage.TotalTokens, settlement.UnbilledLines, wantPriced, 5*wantPriced, wantUnbilled)
	}
	entry := &logstore.Log{}
	settlement.ApplyDebug(entry)
	accounting := entry.BatchDebugParsed.Accounting
	if accounting == nil || accounting.Incomplete != wantIncomplete || accounting.ParseErrorCount != wantParseErrors || accounting.UnbilledLines != wantUnbilled {
		t.Fatalf("settlement debug %+v: want incomplete %v, %d parse errors, %d unbilled lines", accounting, wantIncomplete, wantParseErrors, wantUnbilled)
	}
}

// perTokenBatchPricing prices every batch token at 1e-06.
type perTokenBatchPricing struct{}

func (perTokenBatchPricing) CalculateBatchCostDetailsForUsage(usage *schemas.BifrostLLMUsage, _ schemas.ModelProvider, _ string, _ schemas.RequestType, _ *modelcatalog.PricingLookupScopes) modelcatalog.BatchCostDetails {
	return modelcatalog.BatchCostDetails{Cost: float64(usage.TotalTokens) * 1e-06, Priced: true}
}

func (perTokenBatchPricing) CalculateVideoCostDetails(modelcatalog.VideoPricingDimensions, schemas.ModelProvider, *modelcatalog.PricingLookupScopes) modelcatalog.VideoCostDetails {
	return modelcatalog.VideoCostDetails{}
}
