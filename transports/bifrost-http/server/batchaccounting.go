package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	cstables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/jobaccounting"
	"github.com/capsohq/bifrost/plugins/governance"
	"github.com/capsohq/bifrost/plugins/logging"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/google/uuid"
)

type bifrostBatchResultFetcher struct {
	client *bifrost.Bifrost
}

func (f *bifrostBatchResultFetcher) RetrieveBatch(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostBatchRetrieveResponse, error) {
	if f == nil || f.client == nil {
		return nil, fmt.Errorf("bifrost client is nil")
	}
	if job == nil {
		return nil, fmt.Errorf("batch job is nil")
	}
	req := &schemas.BifrostBatchRetrieveRequest{
		Provider: schemas.ModelProvider(job.Provider),
		Model:    modelPtr(job.Model),
		BatchID:  job.JobID,
	}
	resp, bifrostErr := f.client.BatchRetrieveRequest(internalJobContext(ctx, job.SelectedKeyID), req)
	if bifrostErr != nil {
		return nil, jobaccounting.NewProviderCallError(bifrostErr)
	}
	return resp, nil
}

func (f *bifrostBatchResultFetcher) FetchBatchResults(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostBatchResultsResponse, error) {
	if f == nil || f.client == nil {
		return nil, fmt.Errorf("bifrost client is nil")
	}
	if job == nil {
		return nil, fmt.Errorf("batch job is nil")
	}
	req := &schemas.BifrostBatchResultsRequest{
		Provider: schemas.ModelProvider(job.Provider),
		Model:    modelPtr(job.Model),
		BatchID:  job.JobID,
	}
	resp, bifrostErr := f.client.BatchResultsRequest(internalJobContext(ctx, job.SelectedKeyID), req)
	if bifrostErr != nil {
		return nil, jobaccounting.NewProviderCallError(bifrostErr)
	}
	return resp, nil
}

// internalJobContext builds the context the sweeper polls a provider job with,
// pinned to the key that created the job when the row recorded one.
//
// The sweeper skips the plugin pipeline, so nothing upstream resolves a key for it
// and core is otherwise free to pick any key registered for the provider. On OpenAI
// a video id is scoped to its creating key, so an unpinned poll asks a key that has
// never heard of the job and is told it does not exist.
//
// There is deliberately no unpinned retry when a pinned poll fails, for either kind.
// It would fire on every transient error — a provider outage, a rate limit — and so
// double the load on an upstream already in trouble, to rescue only the rare case of
// a key deleted mid-flight. That case already has two better answers: the job parks
// as unpriceable, which is visible and re-drivable, and a user hitting
// /v1/batches/{id}/results settles it inline with their own key.
//
// BifrostContextKeyAPIKeyID pins by id through the registered pool rather than
// injecting a raw secret, and it is only write-blocked while the plugin pipeline
// holds restricted writes — which is exactly the phase this context never enters.
func internalJobContext(parent context.Context, selectedKeyID string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(parent, schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, uuid.NewString())
	ctx.SetValue(schemas.BifrostContextKeySkipPluginPipeline, true)
	ctx.SetValue(schemas.BifrostContextKeySkipBudgetAndRateLimits, true)
	if selectedKeyID = strings.TrimSpace(selectedKeyID); selectedKeyID != "" {
		ctx.SetValue(schemas.BifrostContextKeyAPIKeyID, selectedKeyID)
	}
	return ctx
}

func modelPtr(model string) *string {
	if model == "" {
		return nil
	}
	return &model
}

// WireBatchAccountingSweeper starts the batch and video accounting sweepers against the live
// logging and governance plugins, or, when client.disable_provider_job_accounting is set
// (fork, TXE-6519), stops them and tells the logging plugin to record no provider job. It runs
// at bootstrap, on a logging or governance plugin reload, and when a client-config save flips
// the switch; every run is safe to repeat.
func (s *BifrostHTTPServer) WireBatchAccountingSweeper() {
	if s == nil || s.Config == nil {
		return
	}
	loggerPlugin, err := lib.FindPluginAs[*logging.LoggerPlugin](s.Config, logging.PluginName)
	if err != nil || loggerPlugin == nil {
		logger.Warn("batch accounting sweeper not wired: logging plugin not found (err=%v)", err)
		return
	}
	// Applied before anything starts and before the client check below, so a disabled
	// deployment stops a running sweeper and writes no job row whether or not the client
	// exists yet. Disabled, the plugin also refuses the two starts further down.
	accounting := s.Config.ShouldRunProviderJobAccounting()
	loggerPlugin.SetProviderJobAccountingEnabled(accounting)
	if !accounting {
		logger.Info("provider job accounting is off (client.disable_provider_job_accounting): no batch or video accounting sweeper, no provider job rows")
	}
	if s.Client == nil {
		return
	}
	// The reporter must be cleared, not just left alone, when governance is gone:
	// this runs on reload, and a stale reporter keeps the logging plugin (and the
	// sweeper it is about to start, which snapshots it) reporting batch usage
	// through a torn-down plugin instance.
	var usageReporter jobaccounting.UsageReporter
	if governancePlugin, govErr := lib.FindPluginAs[governance.BaseGovernancePlugin](s.Config, s.getGovernancePluginName()); govErr == nil && governancePlugin != nil {
		if reporter, ok := governancePlugin.(jobaccounting.UsageReporter); ok {
			usageReporter = reporter
		}
	}
	loggerPlugin.SetBatchUsageReporter(usageReporter)
	// Re-wire the settlement tracer on reload. No-op at first bootstrap (tracer not
	// built yet); Bootstrap calls it again once the tracer exists.
	s.setSettlementTracer()
	if !accounting {
		return
	}
	loggerPlugin.StartBatchAccountingSweeper(&bifrostBatchResultFetcher{client: s.Client}, time.Minute, s.Config.KVStore)
	// Video jobs finish in minutes, not hours, so their sweeper runs on a much
	// tighter tick than the batch one.
	loggerPlugin.StartVideoAccountingSweeper(&bifrostVideoRetriever{client: s.Client, config: s.Config}, 30*time.Second, s.Config.KVStore)
}

// ReconcileProviderJobAccounting re-wires the sweepers when the logging plugin's runtime state
// disagrees with client.disable_provider_job_accounting (fork, TXE-6519). It is what a
// client-config reload calls: a save through PUT /api/config publishes the new value into the
// live config before it asks for the reload (handlers/config.go), so the reload cannot tell a
// change from the config alone, and a cluster peer's reload sees only the stored value. The
// plugin's own flag is the state that must follow the config, so that is what is compared.
func (s *BifrostHTTPServer) ReconcileProviderJobAccounting() {
	if s == nil || s.Config == nil {
		return
	}
	loggerPlugin, err := lib.FindPluginAs[*logging.LoggerPlugin](s.Config, logging.PluginName)
	if err != nil || loggerPlugin == nil {
		return
	}
	if loggerPlugin.ProviderJobAccountingEnabled() == s.Config.ShouldRunProviderJobAccounting() {
		return
	}
	s.WireBatchAccountingSweeper()
}

// setSettlementTracer gives the logging plugin the tracer for emitSettlementSpan.
// No-op until the tracing middleware exists. Re-run on reload to re-wire the rebuilt
// logging plugin (the tracer itself is stable).
func (s *BifrostHTTPServer) setSettlementTracer() {
	if s == nil || s.Config == nil || s.TracingMiddleware == nil {
		return
	}
	loggerPlugin, err := lib.FindPluginAs[*logging.LoggerPlugin](s.Config, logging.PluginName)
	if err != nil || loggerPlugin == nil {
		return
	}
	// Nil-check the concrete *tracing.Tracer so a missing tracer stays a nil interface
	// (not a non-nil interface wrapping a nil pointer, which would panic).
	tracer := s.TracingMiddleware.GetTracer()
	if tracer == nil {
		return
	}
	loggerPlugin.SetSettlementTracer(tracer)
}

type bifrostVideoRetriever struct {
	client *bifrost.Bifrost
	config *lib.Config // fork: resolves a custom provider's base type for the settler
}

// BaseProvider implements jobaccounting.BaseProviderResolver: the base provider type of a
// configured custom provider, or "" when the name is not one.
func (f *bifrostVideoRetriever) BaseProvider(provider schemas.ModelProvider) schemas.ModelProvider {
	if f == nil || f.config == nil {
		return ""
	}
	cfg, err := f.config.GetProviderConfigRaw(provider)
	if err != nil || cfg == nil || cfg.CustomProviderConfig == nil {
		return ""
	}
	return cfg.CustomProviderConfig.BaseProviderType
}

func (f *bifrostVideoRetriever) RetrieveVideo(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostVideoGenerationResponse, error) {
	if f == nil || f.client == nil {
		return nil, fmt.Errorf("bifrost client is nil")
	}
	if job == nil {
		return nil, fmt.Errorf("video job is nil")
	}
	resp, bifrostErr := f.client.VideoRetrieveRequest(internalJobContext(ctx, job.SelectedKeyID), &schemas.BifrostVideoRetrieveRequest{
		Provider: schemas.ModelProvider(job.Provider),
		ID:       job.JobID,
	})
	if bifrostErr != nil {
		return nil, jobaccounting.NewProviderCallError(bifrostErr)
	}
	return resp, nil
}
