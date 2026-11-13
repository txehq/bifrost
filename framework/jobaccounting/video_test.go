package jobaccounting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mp4 "github.com/abema/go-mp4"
	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	cstables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/modelcatalog/datasheet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeVideoRetriever struct {
	resp  *schemas.BifrostVideoGenerationResponse
	err   error
	calls int
	// video is the file DownloadVideo serves, and downloadErr its error (TXE-7329).
	video       []byte
	downloadErr error
	downloads   int
}

func (f *fakeVideoRetriever) RetrieveVideo(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostVideoGenerationResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeVideoRetriever) DownloadVideo(ctx context.Context, job *cstables.TableProviderJob) ([]byte, error) {
	f.downloads++
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.video, nil
}

// videoJob builds a submitted video job row carrying the dimensions the request
// asked for — the state recordVideoJobLifecycle leaves behind.
func videoJob(t *testing.T, videoID string, dims modelcatalog.VideoPricingDimensions) *cstables.TableProviderJob {
	t.Helper()
	params, err := MarshalVideoDimensions(dims)
	require.NoError(t, err)
	return &cstables.TableProviderJob{
		ID:               cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), videoID),
		Kind:             cstables.ProviderJobKindVideo,
		Provider:         string(schemas.OpenAI),
		JobID:            videoID,
		Model:            dims.Model,
		Params:           params,
		AccountingStatus: cstables.ProviderJobAccountingStatusPending,
	}
}

func generationDims(model, size string, seconds int) modelcatalog.VideoPricingDimensions {
	return modelcatalog.VideoPricingDimensions{
		Model:       model,
		RequestType: schemas.VideoGenerationRequest,
		Seconds:     &seconds,
		Size:        size,
	}
}

func settleVideo(t *testing.T, job *cstables.TableProviderJob, resp *schemas.BifrostVideoGenerationResponse) *Settlement {
	t.Helper()
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
		Provider:      schemas.OpenAI,
		ProviderJobID: job.JobID,
		FallbackModel: job.Model,
		Job:           job,
		Payload:       VideoPayload{Response: resp},
	})
	require.NoError(t, err)
	require.NotNil(t, settlement)
	return settlement
}

// settleVideoRendered is settleVideo for a response that reports neither a duration nor a cost:
// the payload carries the rendered length Poll measured (TXE-7329).
func settleVideoRendered(t *testing.T, job *cstables.TableProviderJob, resp *schemas.BifrostVideoGenerationResponse, renderedSeconds int) *Settlement {
	t.Helper()
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
		Provider:      schemas.OpenAI,
		ProviderJobID: job.JobID,
		FallbackModel: job.Model,
		Job:           job,
		Payload:       VideoPayload{Response: resp, RenderedSeconds: &renderedSeconds},
	})
	require.NoError(t, err)
	require.NotNil(t, settlement)
	return settlement
}

func TestVideoDimensionsRoundTripThroughJobParams(t *testing.T) {
	audio := true
	dims := generationDims("sora-2-pro", "1920x1080", 8)
	dims.Audio = &audio
	dims.Extra = map[string]any{"sample_count": float64(2)}

	got := VideoDimensionsFromJob(videoJob(t, "vid_round", dims))
	assert.Equal(t, "sora-2-pro", got.Model)
	assert.Equal(t, "1920x1080", got.Size)
	require.NotNil(t, got.Seconds)
	assert.Equal(t, 8, *got.Seconds)
	require.NotNil(t, got.Audio)
	assert.True(t, *got.Audio)
	assert.Equal(t, float64(2), got.Extra["sample_count"],
		"the research hatch must survive the round trip even though nothing prices on it yet")
}

func TestVideoDimensionsFromJob_MalformedParamsDoNotBlockSettlement(t *testing.T) {
	// A params blob we cannot read must not strand the job: the response may still
	// carry enough to price it.
	garbage := "{not json"
	job := &cstables.TableProviderJob{Kind: cstables.ProviderJobKindVideo, Params: &garbage}
	assert.Equal(t, modelcatalog.VideoPricingDimensions{}, VideoDimensionsFromJob(job))
}

func TestVideoSettle_PricesFromCapturedParamsAndRenderedLengthWhenResponseIsSilent(t *testing.T) {
	// The common provider: a retrieve that reports a status and nothing else. The size comes from
	// the submitted request; the length from the rendered file (TXE-7329).
	job := videoJob(t, "vid_silent", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideoRendered(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}, 8)

	assert.True(t, settlement.Priced)
	assert.True(t, settlement.Complete)
	assert.InDelta(t, 8*0.70, settlement.Cost, 1e-12,
		"the submitted request is the only place the size still exists")
	// The settlement is named for the read that produced it, mirroring batch_results:
	// two rows both reading video_generation read as two generations. The creating
	// request type moves to the debug blob, which is what repricing reads.
	assert.Equal(t, schemas.VideoRetrieveRequest, settlement.Object)
}

func TestVideoSettle_ResponseOverridesCapturedParams(t *testing.T) {
	// The provider downscaled: the bill follows what was produced, not what was asked.
	job := videoJob(t, "vid_downscaled", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status:  schemas.VideoStatusCompleted,
		Seconds: bifrost.Ptr("4"),
		Size:    "1280x720",
		Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	require.True(t, settlement.Priced)
	assert.InDelta(t, 4*0.30, settlement.Cost, 1e-12)
}

func TestVideoSettle_BillsEveryReturnedClip(t *testing.T) {
	job := videoJob(t, "vid_multi", generationDims("sora-2-pro", "1280x720", 8))
	settlement := settleVideoRendered(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{
			{Type: schemas.VideoOutputTypeURL},
			{Type: schemas.VideoOutputTypeURL},
			{Type: schemas.VideoOutputTypeURL},
		},
	}, 8)

	require.True(t, settlement.Priced)
	assert.InDelta(t, 3*8*0.30, settlement.Cost, 1e-12)
}

func TestVideoSettle_ProviderReportedCostWins(t *testing.T) {
	job := videoJob(t, "vid_provider_cost", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		Usage:  &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.42}},
	})

	require.True(t, settlement.Priced)
	assert.InDelta(t, 0.42, settlement.Cost, 1e-12,
		"a provider that reports its own figure is never overridden by an estimate")
}

// A failed generation is a real price of zero, not an absence of one. Recording it
// as unpriceable would leave free work parked in the sweeper forever.
func TestVideoSettle_FailedJobIsPricedAtZero(t *testing.T) {
	job := videoJob(t, "vid_failed", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusFailed,
		Error:  &schemas.VideoCreateError{Code: "generation_failed"},
	})

	assert.True(t, settlement.Priced, "failure is a final answer about the money")
	assert.True(t, settlement.Complete)
	assert.Zero(t, settlement.Cost)
	assert.Empty(t, settlement.UnpriceableReason)
}

// TXE-6521: a cancelled or expired job produced no video. Like a failed one it is terminal and
// settles at a real price of zero, so the sweeper stops polling it instead of waiting forever for
// a status it does not know.
func TestVideoSettle_CancelledAndExpiredJobsAreTerminalAndPricedAtZero(t *testing.T) {
	for _, status := range []schemas.VideoStatus{schemas.VideoStatusCancelled, schemas.VideoStatusExpired} {
		t.Run(string(status), func(t *testing.T) {
			job := videoJob(t, "vid_"+string(status), generationDims("sora-2-pro", "1920x1080", 8))
			resp := &schemas.BifrostVideoGenerationResponse{ID: "vid_" + string(status), Status: status, ProviderStatus: string(status)}

			poll, err := NewVideoSettler(&fakeVideoRetriever{resp: resp}).Poll(context.Background(), job)
			require.NoError(t, err)
			require.NotNil(t, poll)
			assert.True(t, poll.Terminal, "the job will not change state again")
			assert.True(t, poll.Settleable)

			settlement := settleVideo(t, job, resp)
			assert.True(t, settlement.Priced)
			assert.True(t, settlement.Complete)
			assert.Zero(t, settlement.Cost)
			assert.Empty(t, settlement.UnpriceableReason)
		})
	}
}

// Content filtering is the one case we refuse to guess: real compute was spent, and
// whether the provider bills for it is undocumented. Parking it keeps the job
// re-drivable instead of over- or under-charging.
func TestVideoSettle_ContentFilteredParksRatherThanGuesses(t *testing.T) {
	job := videoJob(t, "vid_filtered", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideo(t, job, &schemas.BifrostVideoGenerationResponse{
		Status:        schemas.VideoStatusFailed,
		ContentFilter: &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{"safety"}},
	})

	assert.False(t, settlement.Priced)
	assert.Equal(t, UnpriceableReasonContentFiltered, settlement.UnpriceableReason)
	assert.Zero(t, settlement.Cost)
}

// DashScope answers UNKNOWN for a task whose record aged out and for an id the key does not
// know. Neither says the video was free, so the job parks as video_gone instead of settling at
// zero. An expired job of another provider, and a DashScope job that reports how it ended, keep
// settling at zero.
func TestVideoSettle_DashScopeUnknownTaskParksAsGone(t *testing.T) {
	job := videoJob(t, "ds-task", generationDims("wan2.6-t2v", "1280x720", 5))
	settle := func(provider schemas.ModelProvider, resp *schemas.BifrostVideoGenerationResponse) *Settlement {
		settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
			Provider: provider, ProviderJobID: job.JobID, FallbackModel: job.Model, Job: job, Payload: VideoPayload{Response: resp},
		})
		require.NoError(t, err)
		return settlement
	}
	unknown := &schemas.BifrostVideoGenerationResponse{ID: "ds-task", Status: schemas.VideoStatusExpired, ProviderStatus: "UNKNOWN"}

	gone := settle(schemas.DashScope, unknown)
	assert.False(t, gone.Priced, "an unknown task is not a free one")
	assert.False(t, gone.Complete)
	assert.Equal(t, UnpriceableReasonVideoGone, gone.UnpriceableReason)
	assert.Zero(t, gone.Cost)

	cancelled := settle(schemas.DashScope, &schemas.BifrostVideoGenerationResponse{ID: "ds-task", Status: schemas.VideoStatusCancelled, ProviderStatus: "CANCELED"})
	assert.True(t, cancelled.Priced, "a cancelled DashScope task still settles at zero")
	assert.Empty(t, cancelled.UnpriceableReason)

	other := settle(schemas.OpenAI, unknown)
	assert.True(t, other.Priced, "another provider's expired job still settles at zero")
	assert.Empty(t, other.UnpriceableReason)
}

func TestVideoSettle_NoRateRecordsUnpricedRatherThanFree(t *testing.T) {
	job := videoJob(t, "vid_norate", generationDims("some-unknown-model", "1920x1080", 8))
	settlement := settleVideoRendered(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}, 8)

	assert.False(t, settlement.Priced, "no rate must never read as free")
	assert.True(t, settlement.RecordUnpriced, "the row must stay visible so backfill can reprice it")
	assert.Equal(t, UnpriceableReasonMissingVideoPricing, settlement.UnpriceableReason)
	assert.Equal(t, "some-unknown-model", settlement.UnpricedModel)
}

func TestVideoPoll_InProgressJobIsNotSettleable(t *testing.T) {
	job := videoJob(t, "vid_running", generationDims("sora-2-pro", "1920x1080", 8))
	retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_running",
		Status: schemas.VideoStatusInProgress,
	}}

	poll, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.NoError(t, err)
	require.NotNil(t, poll)
	assert.False(t, poll.Terminal)
	assert.False(t, poll.Settleable)
	require.NotNil(t, poll.Job)
	assert.Equal(t, string(schemas.VideoStatusInProgress), poll.Job.ProviderStatus)
}

func TestVideoPoll_MismatchedIDIsAnError(t *testing.T) {
	job := videoJob(t, "vid_expected", generationDims("sora-2-pro", "1920x1080", 8))
	retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_someone_elses",
		Status: schemas.VideoStatusCompleted,
	}}

	_, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.Error(t, err, "settling another job's response would bill the wrong request")
}

func TestVideoPoll_RetrieveErrorReschedules(t *testing.T) {
	job := videoJob(t, "vid_err", generationDims("sora-2-pro", "1920x1080", 8))
	retriever := &fakeVideoRetriever{err: errors.New("upstream unavailable")}

	_, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.Error(t, err)
}

// Video jobs finish in minutes; the batch ladder would leave a finished one
// unsettled for a quarter of an hour.
func TestVideoBackoffStaysInMinutes(t *testing.T) {
	settler := NewVideoSettler(nil)
	base := time.Minute
	assert.Equal(t, time.Minute, settler.Backoff(1, base))
	assert.Equal(t, 2*time.Minute, settler.Backoff(20, base))
	assert.Equal(t, 5*time.Minute, settler.Backoff(40, base))
	assert.Less(t, settler.Backoff(40, base), NewBatchSettler(nil).Backoff(40, base))
}

func TestVideoSettlerSupportsOnlyRealVideoProviders(t *testing.T) {
	settler := NewVideoSettler(nil)
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Gemini, schemas.Vertex, schemas.Replicate, schemas.Runway, schemas.Runware, schemas.MiniMax, schemas.BytePlus, schemas.Kling, schemas.DashScope, schemas.XAI} {
		assert.True(t, settler.SupportsProvider(provider), string(provider))
	}
	// Every provider implements the VideoGeneration method, so a real allowlist is
	// the only thing keeping the sweeper from burning attempts on providers that
	// merely return "unsupported".
	assert.False(t, settler.SupportsProvider(schemas.Anthropic))
	assert.False(t, settler.SupportsProvider(schemas.Cohere))
}

// The video namespace must never collide with batch: the id is both the aggregate
// write's idempotency key and the governance dedupe key.
func TestVideoAccountingLogIDIsDistinctFromBatch(t *testing.T) {
	video := AccountingLogID(ProviderJobKindVideo, schemas.OpenAI, "same_id")
	batch := AccountingLogID(ProviderJobKindBatch, schemas.OpenAI, "same_id")
	assert.NotEqual(t, video, batch)
	assert.Equal(t, video, AccountingLogID(ProviderJobKindVideo, schemas.OpenAI, "same_id"), "the id must be deterministic")
}

// End to end through the shared engine: the sweeper picks up a due video job,
// settles it exactly once, and reports it exactly once.
func TestVideoSweeper_SettlesCompletedJobOnce(t *testing.T) {
	store := newFakeAccountingStore()
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_sweep", generationDims("sora-2-pro", "1920x1080", 8))
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	// A batch job sitting in the same table must be left entirely alone.
	batchDue := due
	require.NoError(t, store.UpsertProviderJob(context.Background(), &cstables.TableProviderJob{
		ID:               cstables.ProviderJobID(cstables.ProviderJobKindBatch, string(schemas.OpenAI), "batch_untouched"),
		Kind:             cstables.ProviderJobKindBatch,
		Provider:         string(schemas.OpenAI),
		JobID:            "batch_untouched",
		AccountingStatus: cstables.ProviderJobAccountingStatusPending,
		NextCheckAt:      &batchDue,
	}))

	retriever := &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_sweep",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}, video: renderedMP4(t, 8)}
	reporter := &fakeUsageReporter{}
	sweeper := NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{
		Provider: schemas.OpenAI,
		Limit:    10,
	})

	sweeper.SweepOnce(context.Background())

	assert.Equal(t, 1, retriever.calls, "the video sweeper must not poll the batch row")
	settled := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), "vid_sweep")]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus)

	logID := AccountingLogID(ProviderJobKindVideo, schemas.OpenAI, "vid_sweep")
	entry := store.logs[logID]
	require.NotNil(t, entry, "the aggregate cost row must be keyed by the video namespace")
	require.NotNil(t, entry.Cost)
	assert.InDelta(t, 8*0.70, *entry.Cost, 1e-12)
	assert.Equal(t, string(schemas.VideoRetrieveRequest), entry.Object)
	require.NotNil(t, entry.VideoDebugParsed)
	require.NotNil(t, entry.VideoDebugParsed.Accounting)
	assert.Equal(t, schemas.VideoGenerationRequest, entry.VideoDebugParsed.Accounting.RequestType,
		"Object no longer names the rates this priced at, so repricing reads the creating type from here")
	require.Len(t, reporter.reports, 1)
	assert.Equal(t, logID, reporter.reports[0].RequestID)

	untouched := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindBatch, string(schemas.OpenAI), "batch_untouched")]
	require.NotNil(t, untouched)
	assert.Equal(t, cstables.ProviderJobAccountingStatusPending, untouched.AccountingStatus,
		"a batch row must never be settled by the video sweeper")

	// Settlement is at-least-once by design, so a second sweep must be inert.
	sweeper.SweepOnce(context.Background())
	assert.Len(t, store.logs, 1)
	assert.Len(t, reporter.reports, 1, "governance must be told exactly once")
}

// --- Inline settlement on the retrieve path ---

func accountVideoInline(t *testing.T, store *fakeAccountingStore, reporter *fakeUsageReporter, videoID string, resp *schemas.BifrostVideoGenerationResponse) *Outcome {
	t.Helper()
	out, err := AccountVideoJob(context.Background(), store, store, fakePricing{}, JobRequest{
		Provider:      schemas.OpenAI,
		ProviderJobID: videoID,
		FallbackModel: "sora-2-pro",
		UsageReporter: reporter,
		ClaimedBy:     "logging-node",
		Payload:       VideoPayload{Response: resp},
	})
	require.NoError(t, err)
	return out
}

// A user polling GET /v1/videos/{id} has already fetched the terminal response.
// Settling from it saves a provider call and a full sweep interval of delay.
func TestAccountVideoJob_SettlesFromTheCallersTerminalResponse(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := videoJob(t, "vid_inline", generationDims("sora-2-pro", "1920x1080", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	// Sora reports its seconds, so the job settles inline without a download (TXE-7329).
	out := accountVideoInline(t, store, reporter, "vid_inline", &schemas.BifrostVideoGenerationResponse{
		ID:      "vid_inline",
		Status:  schemas.VideoStatusCompleted,
		Seconds: bifrost.Ptr("8"),
		Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	require.NotNil(t, out)
	assert.True(t, out.Accounted)
	assert.InDelta(t, 8*0.70, out.Cost, 1e-12)

	settled := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), "vid_inline")]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus)
	assert.Len(t, store.logs, 1)
	require.Len(t, reporter.reports, 1)
}

// A video submitted before this build was charged at submission. Settling it now,
// off a row we never wrote, would bill it a second time.
func TestAccountVideoJob_RefusesAJobWeNeverSawSubmitted(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}

	out := accountVideoInline(t, store, reporter, "vid_pre_upgrade", &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_pre_upgrade",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	})

	assert.Nil(t, out, "no coordination row means we never saw the submission")
	assert.Empty(t, store.logs, "nothing may be billed for a job we did not record")
	assert.Empty(t, reporter.reports)
}

// Clients poll a running job repeatedly; only a terminal response is settlement
// input.
func TestAccountVideoJob_IgnoresAStillRunningJob(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := videoJob(t, "vid_running_inline", generationDims("sora-2-pro", "1920x1080", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	for _, status := range []schemas.VideoStatus{schemas.VideoStatusQueued, schemas.VideoStatusInProgress} {
		out := accountVideoInline(t, store, reporter, "vid_running_inline", &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_running_inline",
			Status: status,
		})
		assert.Nil(t, out, string(status))
	}
	assert.Empty(t, store.logs)
	assert.Empty(t, reporter.reports)
}

// Polling is not billing. A client that keeps calling retrieve after the job
// finished must not be charged again on every call.
func TestAccountVideoJob_RepeatedRetrievesBillOnce(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := videoJob(t, "vid_repoll", generationDims("sora-2-pro", "1280x720", 8))
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	resp := &schemas.BifrostVideoGenerationResponse{
		ID:      "vid_repoll",
		Status:  schemas.VideoStatusCompleted,
		Seconds: bifrost.Ptr("8"),
		Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}
	first := accountVideoInline(t, store, reporter, "vid_repoll", resp)
	require.NotNil(t, first)
	assert.True(t, first.Accounted)

	for i := 0; i < 3; i++ {
		again := accountVideoInline(t, store, reporter, "vid_repoll", resp)
		require.NotNil(t, again)
		assert.False(t, again.Claimed, "a settled job must not be re-claimed")
		// The caller still wants a price to display, mirrored off the written row.
		assert.InDelta(t, first.Cost, again.Cost, 1e-12)
	}
	assert.Len(t, store.logs, 1)
	assert.Len(t, reporter.reports, 1, "governance must be told exactly once")
}

// The Runware case: the retrieve the caller already holds carries the provider's
// own figure, so inline settlement prices exactly without any datasheet rate.
func TestAccountVideoJob_UsesProviderReportedCostFromTheRetrieve(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	// No duration and an unknown model: nothing here is priceable from the catalog.
	job := videoJob(t, "vid_runware", modelcatalog.VideoPricingDimensions{
		Model:       "runware:tripo-v3.1",
		RequestType: schemas.VideoGenerationRequest,
	})
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	out := accountVideoInline(t, store, reporter, "vid_runware", &schemas.BifrostVideoGenerationResponse{
		ID:     "vid_runware",
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		Usage:  &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.4}},
	})

	require.NotNil(t, out)
	assert.True(t, out.Accounted)
	assert.InDelta(t, 0.4, out.Cost, 1e-12,
		"a provider-reported cost settles a job the catalog cannot price at all")
}

// Settling inline and settling from the sweeper must land on the same row, or the
// two paths would each write their own cost record for one video.
func TestAccountVideoJob_SharesTheSweepersAggregateRow(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_both", generationDims("sora-2-pro", "1280x720", 8))
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	resp := &schemas.BifrostVideoGenerationResponse{
		ID:      "vid_both",
		Status:  schemas.VideoStatusCompleted,
		Seconds: bifrost.Ptr("8"),
		Videos:  []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}
	require.NotNil(t, accountVideoInline(t, store, reporter, "vid_both", resp))

	retriever := &fakeVideoRetriever{resp: resp}
	NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{
		Provider: schemas.OpenAI,
		Limit:    10,
	}).SweepOnce(context.Background())

	assert.Len(t, store.logs, 1, "one video, one cost row, whichever path got there first")
	assert.Len(t, reporter.reports, 1)
	assert.Equal(t, 0, retriever.calls, "an accounted job is no longer due, so the sweeper skips it")
}

// --- Unreachable jobs (the retry-loop fix) ---

func providerErr(status int, message string) error {
	return NewProviderCallError(&schemas.BifrostError{
		StatusCode: &status,
		Error:      &schemas.ErrorField{Message: message},
	})
}

// The three status families a provider actually returns for a job it will not
// serve, taken from real responses: Runway and OpenAI 404, Gemini 403.
func TestClassifyProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want ProviderCallOutcome
	}{
		{"runway 404", providerErr(404, "Could not find Task"), ProviderCallGone},
		{"openai 404", providerErr(404, "Video with id 'video_x' not found."), ProviderCallGone},
		{"gone", providerErr(410, "gone"), ProviderCallGone},
		{"gemini 403", providerErr(403, "You do not have permission to access the Operation"), ProviderCallAccessDenied},
		{"unauthorized", providerErr(401, "invalid api key"), ProviderCallAccessDenied},
		{"billing", providerErr(402, "payment required"), ProviderCallAccessDenied},
		{"bad request", providerErr(400, "malformed id"), ProviderCallRejected},
		{"unprocessable", providerErr(422, "unprocessable"), ProviderCallRejected},
		// The two 4xx that mean "ask again", not "no".
		{"rate limited", providerErr(429, "slow down"), ProviderCallRetry},
		{"request timeout", providerErr(408, "timeout"), ProviderCallRetry},
		{"server error", providerErr(503, "unavailable"), ProviderCallRetry},
		{"anthropic overloaded", providerErr(529, "overloaded"), ProviderCallRetry},
		// No response at all must never be mistaken for a client error.
		{"transport failure", errors.New("dial tcp: connection refused"), ProviderCallRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyProviderCall(tc.err))
		})
	}
}

// A provider that has forgotten the job must stop the polling on the first attempt.
// Rescheduling instead spends the whole 120-attempt budget over hours and then parks
// the job under max_poll_attempts, which names the wrong cause.
func TestVideoPoll_UnreachableJobIsTerminalOnTheFirstAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"gone", providerErr(404, "Could not find Task"), UnpriceableReasonVideoGone},
		{"access denied", providerErr(403, "PERMISSION_DENIED"), UnpriceableReasonVideoAccessDenied},
		{"rejected", providerErr(400, "malformed id"), UnpriceableReasonVideoRequestRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := videoJob(t, "vid_unreachable", generationDims("sora-2-pro", "1920x1080", 8))
			poll, err := NewVideoSettler(&fakeVideoRetriever{err: tc.err}).Poll(context.Background(), job)

			require.NoError(t, err, "an unreachable job is an outcome, not a poll failure")
			require.NotNil(t, poll)
			assert.True(t, poll.Terminal)
			assert.False(t, poll.Settleable)
			assert.Equal(t, tc.reason, poll.UnpriceableReason)
			assert.Nil(t, poll.Job, "a failed call learned nothing worth persisting")
		})
	}
}

// A transient failure keeps the existing reschedule path.
func TestVideoPoll_TransientFailureStillRetries(t *testing.T) {
	job := videoJob(t, "vid_transient", generationDims("sora-2-pro", "1920x1080", 8))
	for _, err := range []error{providerErr(503, "unavailable"), providerErr(429, "slow down"), errors.New("timeout")} {
		poll, pollErr := NewVideoSettler(&fakeVideoRetriever{err: err}).Poll(context.Background(), job)
		require.Error(t, pollErr, "%v must stay a retryable poll failure", err)
		assert.Nil(t, poll)
	}
}

// End to end: the sweeper parks a gone video after exactly one provider call.
func TestVideoSweeper_GoneJobStopsPollingImmediately(t *testing.T) {
	store := newFakeAccountingStore()
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_gone", generationDims("sora-2-pro", "1920x1080", 8))
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	retriever := &fakeVideoRetriever{err: providerErr(404, "Video with id 'vid_gone' not found.")}
	sweeper := NewVideoSweeper(store, store, fakePricing{}, retriever, nil, nil, SweeperConfig{
		Provider: schemas.OpenAI,
		Limit:    10,
	})

	sweeper.SweepOnce(context.Background())
	sweeper.SweepOnce(context.Background())

	assert.Equal(t, 1, retriever.calls,
		"a job the provider has forgotten must not be polled again")
	settled := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.OpenAI), "vid_gone")]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusUnpriceable, settled.AccountingStatus)
	require.NotNil(t, settled.UnpriceableReason)
	assert.Equal(t, UnpriceableReasonVideoGone, *settled.UnpriceableReason)
	assert.Empty(t, store.logs, "an unreachable job bills nothing")
}

// --- Aggregate row detail ---

// A settled video writes its cost on its own row, minutes after the submission —
// same object, same model, differing only by a cost. The accounting block is what
// tells the two apart, so it must be present on the settlement and absent from the
// submission.
func TestVideoSettle_AggregateRowCarriesAccountingDetail(t *testing.T) {
	job := videoJob(t, "vid_debug", generationDims("sora-2-pro", "1920x1080", 8))
	settlement := settleVideoRendered(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}, 8)
	require.NotNil(t, settlement.ApplyDebug)

	entry := &logstore.Log{}
	settlement.ApplyDebug(entry)
	require.NotNil(t, entry.VideoDebugParsed)
	assert.Equal(t, "vid_debug", entry.VideoDebugParsed.VideoID)
	assert.Equal(t, schemas.VideoStatusCompleted, entry.VideoDebugParsed.Status)

	acct := entry.VideoDebugParsed.Accounting
	require.NotNil(t, acct, "the accounting block is what marks this as the settlement row")
	require.NotNil(t, acct.Seconds)
	assert.Equal(t, 8, *acct.Seconds)
	assert.Equal(t, "1920x1080", acct.Size)
	assert.Equal(t, 1, acct.OutputCount)
	assert.False(t, acct.Incomplete)
}

// An unpriceable row is exactly the one an operator needs the detail on, so it must
// carry the same block — marked incomplete.
func TestVideoSettle_UnpriceableRowStillCarriesDetail(t *testing.T) {
	job := videoJob(t, "vid_norate_debug", generationDims("some-unknown-model", "1920x1080", 8))
	settlement := settleVideoRendered(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}, 8)

	require.False(t, settlement.Priced)
	require.NotNil(t, settlement.ApplyDebug)
	entry := &logstore.Log{}
	settlement.ApplyDebug(entry)
	require.NotNil(t, entry.VideoDebugParsed.Accounting)
	assert.True(t, entry.VideoDebugParsed.Accounting.Incomplete)
}

// HydrateFromLog is the inverse of the closure: a caller that lost the settlement
// claim reads the status back off the already-written row.
func TestVideoHydrateFromLog(t *testing.T) {
	entry := &logstore.Log{VideoDebugParsed: &schemas.BifrostVideoDebug{
		VideoID: "vid_x",
		Status:  schemas.VideoStatusCompleted,
	}}
	out := &Outcome{}
	NewVideoSettler(nil).HydrateFromLog(entry, out)
	assert.Equal(t, string(schemas.VideoStatusCompleted), out.Status)

	// A row written before video detail existed must not panic or invent a status.
	bare := &Outcome{}
	NewVideoSettler(nil).HydrateFromLog(&logstore.Log{}, bare)
	assert.Empty(t, bare.Status)
}

// resolvingVideoRetriever is a fakeVideoRetriever that also names custom providers' bases.
type resolvingVideoRetriever struct {
	fakeVideoRetriever
	bases map[schemas.ModelProvider]schemas.ModelProvider
}

func (r *resolvingVideoRetriever) BaseProvider(provider schemas.ModelProvider) schemas.ModelProvider {
	return r.bases[provider]
}

// A custom provider records its video jobs under its own name, so the settler must recognise it
// through its base provider, or its jobs are never settled and the video is never billed.
func TestVideoSettlerSupportsCustomProviderOnAVideoBase(t *testing.T) {
	retriever := &resolvingVideoRetriever{bases: map[schemas.ModelProvider]schemas.ModelProvider{
		"my-minimax":   schemas.MiniMax,
		"my-openai":    schemas.OpenAI,
		"my-anthropic": schemas.Anthropic,
	}}
	settler := NewVideoSettler(retriever)
	assert.True(t, settler.SupportsProvider("my-minimax"))
	assert.True(t, settler.SupportsProvider("my-openai"))
	assert.False(t, settler.SupportsProvider("my-anthropic"), "a base with no video settlement stays unsupported")
	assert.False(t, settler.SupportsProvider("unknown"))
	assert.True(t, settler.SupportsProvider(schemas.MiniMax))
	assert.False(t, NewVideoSettler(nil).SupportsProvider("my-minimax"), "without a resolver only built-in names match")
}

func TestVideoSweeper_SettlesCustomProviderJob(t *testing.T) {
	store := newFakeAccountingStore()
	due := time.Now().UTC().Add(-time.Minute)
	job := videoJob(t, "vid_custom", generationDims("sora-2-pro", "1920x1080", 8))
	job.Provider = "my-minimax"
	job.ID = cstables.ProviderJobID(cstables.ProviderJobKindVideo, job.Provider, job.JobID)
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	retriever := &resolvingVideoRetriever{
		fakeVideoRetriever: fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{
			ID:     "vid_custom",
			Status: schemas.VideoStatusCompleted,
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		}, video: renderedMP4(t, 8)},
		bases: map[schemas.ModelProvider]schemas.ModelProvider{"my-minimax": schemas.MiniMax},
	}
	reporter := &fakeUsageReporter{}
	sweeper := NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{Provider: "my-minimax", Limit: 10})
	sweeper.SweepOnce(context.Background())

	assert.Equal(t, 1, retriever.calls, "the custom provider's job must be polled")
	settled := store.jobs[job.ID]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus)
	require.Len(t, reporter.reports, 1, "governance must receive the custom provider's video cost")
	assert.NotNil(t, store.logs[AccountingLogID(ProviderJobKindVideo, "my-minimax", "vid_custom")], "the aggregate row keeps the custom provider name")
}

// videoCoverageAccount configures every standard provider at one base URL with one test key.
type videoCoverageAccount struct {
	baseURL string
}

func (a *videoCoverageAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return schemas.StandardProviders, nil
}

func (a *videoCoverageAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "k", Name: "k", Value: *schemas.NewSecretVar("test-key"), Weight: 1, Models: schemas.WhiteList{"*"}}}, nil
}

func (a *videoCoverageAccount) GetConfigForProvider(_ schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: a.baseURL, DefaultRequestTimeoutInSeconds: 5, MaxRetries: 0, AllowPrivateNetwork: true,
	}}, nil
}

// videoDimsRecordingPricing records the dimensions each video price is computed from.
type videoDimsRecordingPricing struct {
	requestTypePricing
	dims []modelcatalog.VideoPricingDimensions
}

func (p *videoDimsRecordingPricing) CalculateVideoCostDetails(dims modelcatalog.VideoPricingDimensions, _ schemas.ModelProvider, _ *modelcatalog.PricingLookupScopes) modelcatalog.VideoCostDetails {
	p.dims = append(p.dims, dims)
	return modelcatalog.VideoCostDetails{Cost: 0.125, Priced: true}
}

// TestVideoReprice_KeepsTheSettlementsAudioFlag (TXE-7306): a reprice prices a settled video
// with the audio flag its create stated, read back from the stored video_debug. Without it, an
// explicit audio false repriced at the provider's default, the with-audio rate (folder 549's
// silent job: 0.125 settled, 0.25 after a recalculation).
func TestVideoReprice_KeepsTheSettlementsAudioFlag(t *testing.T) {
	off := false
	seconds := 5
	for name, audio := range map[string]*bool{"audio false": &off, "audio unstated": nil} {
		t.Run(name, func(t *testing.T) {
			settled := &logstore.Log{ID: "log-1", Provider: string(schemas.DashScope), Model: "wan2.5-t2v-preview"}
			videoDebugFor("ds-audio-off", schemas.VideoStatusCompleted, modelcatalog.VideoPricingDimensions{
				RequestType: schemas.VideoGenerationRequest, Seconds: &seconds, Size: "1280x720", Audio: audio, OutputCount: 1,
			}, "", false)(settled)
			// The row as the store keeps it: video_debug serialized, read back by the reprice.
			stored, err := json.Marshal(settled.VideoDebugParsed)
			require.NoError(t, err)
			row := &logstore.Log{ID: settled.ID, Provider: settled.Provider, Model: settled.Model, VideoDebug: string(stored)}

			pricing := &videoDimsRecordingPricing{}
			repriced, err := (&VideoSettler{}).RepriceFromLog(row, pricing)
			require.NoError(t, err)
			require.NotNil(t, repriced)
			require.Len(t, pricing.dims, 1)
			assert.Equal(t, audio, pricing.dims[0].Audio)
		})
	}
}

// TestEveryVideoProviderHasAnAudioDefault (TXE-7306) fails when a provider in videoProviders has
// no entry in datasheet.VideoAudioDefaults, or an entry cites no source. A job whose create does
// not say whether to generate audio is priced at its provider's default, and a provider added
// without one would fall to the catch-all instead of its vendor's documented behaviour. An entry
// for a provider that settles no video is stale and fails too.
func TestEveryVideoProviderHasAnAudioDefault(t *testing.T) {
	for provider := range videoProviders {
		def, ok := datasheet.VideoAudioDefaults[provider]
		switch {
		case !ok:
			t.Errorf("%s settles video but has no entry in datasheet.VideoAudioDefaults: add what its vendor does when a create does not say whether to generate audio, with the vendor page", provider)
		case strings.TrimSpace(def.Source) == "":
			t.Errorf("%s's entry in datasheet.VideoAudioDefaults cites no source", provider)
		}
	}
	for provider := range datasheet.VideoAudioDefaults {
		if _, settled := videoProviders[provider]; !settled {
			t.Errorf("%s is in datasheet.VideoAudioDefaults but not in videoProviders: remove the entry", provider)
		}
	}
}

// TestVideoSettlerCoversEveryProviderThatImplementsVideo (TXE-7118) reads the set of video
// providers from the providers themselves. videoProviders is a hand-kept list, and a provider
// missing from it has its video jobs recorded and never settled: nothing is charged to the
// virtual key (found on dashscope, TXE-6432, and azure, TXE-7054). A provider implements video
// when neither its VideoGeneration nor its VideoRetrieve answers unsupported_operation. Such a
// provider must be in videoProviders or in videoSettlementExemptions with a reason.
//
// It runs on every pull request in the Go unit tests (offline) job (TXE-7078), which finds
// every framework package with tests; it needs no CI entry of its own.
func TestVideoSettlerCoversEveryProviderThatImplementsVideo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"video coverage fixture"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &videoCoverageAccount{baseURL: upstream.URL}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)

	unsupported := func(bifrostErr *schemas.BifrostError) bool {
		return bifrostErr != nil && bifrostErr.Error != nil && bifrostErr.Error.Code != nil && *bifrostErr.Error.Code == "unsupported_operation"
	}
	key := schemas.Key{ID: "k", Name: "k", Value: *schemas.NewSecretVar("test-key"), Weight: 1, Models: schemas.WhiteList{"*"}}
	// implements reports whether one operation gets past the unsupported stub. A provider that
	// panics on a bare test key or request is past the stub too.
	implements := func(call func() *schemas.BifrostError) (implemented bool) {
		defer func() {
			if recover() != nil {
				implemented = true
			}
		}()
		return !unsupported(call())
	}

	settler := NewVideoSettler(nil)
	var built int
	for _, provider := range schemas.StandardProviders {
		instance := client.GetProviderByKey(provider)
		if instance == nil {
			continue
		}
		built++
		creates := implements(func() *schemas.BifrostError {
			ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, bifrostErr := instance.VideoGeneration(ctx, key, &schemas.BifrostVideoGenerationRequest{
				Provider: provider, Model: "video-coverage-model", Input: &schemas.VideoGenerationInput{Prompt: "a paper boat"},
			})
			return bifrostErr
		})
		retrieves := implements(func() *schemas.BifrostError {
			ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, bifrostErr := instance.VideoRetrieve(ctx, key, &schemas.BifrostVideoRetrieveRequest{Provider: provider, ID: "video-coverage-task"})
			return bifrostErr
		})
		implementsVideo := creates && retrieves
		reason, exempt := videoSettlementExemptions[provider]
		settled := settler.SupportsProvider(provider)
		t.Logf("%-18s create=%v retrieve=%v settled=%v exempt=%v", provider, creates, retrieves, settled, exempt)
		switch {
		case implementsVideo && !settled && !exempt:
			t.Errorf("%s implements video create and retrieve but is not in videoProviders: its jobs are recorded and never settled or charged. Add it to videoProviders, or to videoSettlementExemptions with the reason", provider)
		case settled && exempt:
			t.Errorf("%s is in videoProviders and in videoSettlementExemptions (%q): keep one", provider, reason)
		case exempt && !implementsVideo:
			t.Errorf("%s is in videoSettlementExemptions (%q) but does not implement video: remove the exemption", provider, reason)
		case exempt && reason == "":
			t.Errorf("%s is in videoSettlementExemptions without a reason", provider)
		case settled && !implementsVideo:
			t.Errorf("%s is in videoProviders but does not implement video create and retrieve: the sweeper would poll a provider that answers unsupported", provider)
		}
	}
	// A provider that cannot be built from a base URL and a key would be skipped silently.
	assert.Equal(t, len(schemas.StandardProviders), built, "every standard provider must be built, or it is not checked")
}

// --- The rendered duration (TXE-7329) ---
//
// A video job whose vendor reports neither the video's duration nor a cost is priced on the length
// of the video it rendered, read from the file's MP4 header, never on the seconds its create
// named. A hosted model may take its own length input (Replicate's num_frames or length, through
// extra_params) and render a longer video than the create asked for.

// renderedMP4 builds a small MP4 whose one video track lasts the given seconds: four samples in an
// mdat, timed by the sample table and stated by mdhd.
func renderedMP4(t *testing.T, seconds int) []byte {
	t.Helper()
	return renderedMP4MS(t, int64(seconds)*1000)
}

func renderedMP4MS(t *testing.T, ms int64) []byte {
	t.Helper()
	f := &mp4MemFile{}
	w := mp4.NewWriter(f)
	box := func(typ mp4.BoxType, children func()) {
		_, err := w.StartBox(&mp4.BoxInfo{Type: typ})
		require.NoError(t, err)
		children()
		_, err = w.EndBox()
		require.NoError(t, err)
	}
	leaf := func(b mp4.IBox) {
		box(b.GetType(), func() {
			_, err := mp4.Marshal(w, b, mp4.Context{})
			require.NoError(t, err)
		})
	}
	leaf(&mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}})
	var at int64
	box(mp4.BoxTypeMdat(), func() {
		pos, err := w.Seek(0, io.SeekCurrent)
		require.NoError(t, err)
		at = pos
		_, err = w.Write(make([]byte, 100))
		require.NoError(t, err)
	})
	require.Zero(t, ms%4, "four samples of equal length")
	box(mp4.BoxTypeMoov(), func() {
		leaf(&mp4.Mvhd{Timescale: 1000, DurationV0: uint32(ms)})
		box(mp4.BoxTypeTrak(), func() {
			leaf(&mp4.Tkhd{TrackID: 1})
			box(mp4.BoxTypeMdia(), func() {
				leaf(&mp4.Mdhd{Timescale: 1000, DurationV0: uint32(ms)})
				leaf(&mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
				box(mp4.BoxTypeMinf(), func() {
					box(mp4.BoxTypeStbl(), func() {
						leaf(&mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: 4, SampleDelta: uint32(ms / 4)}}})
						leaf(&mp4.Stsz{SampleSize: 25, SampleCount: 4})
						leaf(&mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 4, SampleDescriptionIndex: 1}}})
						leaf(&mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{uint32(at)}})
					})
				})
			})
		})
	})
	return f.data
}

// mp4MemFile is an in-memory io.WriteSeeker for go-mp4's Writer.
type mp4MemFile struct {
	data []byte
	pos  int64
}

func (m *mp4MemFile) Write(p []byte) (int, error) {
	if end := m.pos + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	n := copy(m.data[m.pos:], p)
	m.pos += int64(n)
	return n, nil
}

func (m *mp4MemFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(len(m.data)) + offset
	}
	return m.pos, nil
}

// replicateVideoJob is a Replicate job whose create named seconds; Replicate's prediction reports
// neither the rendered duration nor a cost.
func replicateVideoJob(t *testing.T, videoID string, requestedSeconds int) *cstables.TableProviderJob {
	t.Helper()
	job := videoJob(t, videoID, generationDims("sora-2-pro", "1920x1080", requestedSeconds))
	job.Provider = string(schemas.Replicate)
	job.ID = cstables.ProviderJobID(cstables.ProviderJobKindVideo, job.Provider, videoID)
	return job
}

func silentReplicateResponse(videoID string) *schemas.BifrostVideoGenerationResponse {
	return &schemas.BifrostVideoGenerationResponse{
		ID:     videoID,
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: bifrost.Ptr("https://replicate.delivery/out.mp4"), ContentType: "video/mp4"}},
	}
}

func sweepVideoJob(t *testing.T, store *fakeAccountingStore, job *cstables.TableProviderJob, retriever VideoRetriever) *fakeUsageReporter {
	t.Helper()
	due := time.Now().UTC().Add(-time.Minute)
	job.NextCheckAt = &due
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))
	reporter := &fakeUsageReporter{}
	NewVideoSweeper(store, store, fakePricing{}, retriever, nil, reporter, SweeperConfig{
		Provider: schemas.ModelProvider(job.Provider),
		Limit:    10,
	}).SweepOnce(context.Background())
	return reporter
}

// The ticket's case: the create asked for 5 seconds, a model-defined input made the model render
// 12, and Replicate reports neither. The job is charged for the 12 seconds it rendered.
func TestVideoSweeper_PricesTheRenderedLengthNotTheRequestedSeconds(t *testing.T) {
	store := newFakeAccountingStore()
	job := replicateVideoJob(t, "pred_longer", 5)
	retriever := &fakeVideoRetriever{resp: silentReplicateResponse("pred_longer"), video: renderedMP4(t, 12)}

	reporter := sweepVideoJob(t, store, job, retriever)

	assert.Equal(t, 1, retriever.downloads, "the rendered file is read once")
	settled := store.jobs[job.ID]
	require.NotNil(t, settled)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus)
	entry := store.logs[AccountingLogID(ProviderJobKindVideo, schemas.Replicate, "pred_longer")]
	require.NotNil(t, entry)
	require.NotNil(t, entry.Cost)
	assert.InDelta(t, 12*0.70, *entry.Cost, 1e-12, "priced on the 12 rendered seconds, not the 5 requested")
	require.NotNil(t, entry.VideoDebugParsed.Accounting.Seconds)
	assert.Equal(t, 12, *entry.VideoDebugParsed.Accounting.Seconds, "the row records the seconds it was priced on, which repricing reads")
	require.Len(t, reporter.reports, 1)
}

// A shorter file never lowers the bill below the seconds the create named: the file is read after
// the job finished, and an output in the caller's own storage (a Veo storageURI) can be replaced
// by a shorter MP4 before the sweeper reads it.
func TestVideoSweeper_AShorterFileDoesNotLowerTheRequestedSeconds(t *testing.T) {
	store := newFakeAccountingStore()
	job := replicateVideoJob(t, "pred_shorter", 8)
	retriever := &fakeVideoRetriever{resp: silentReplicateResponse("pred_shorter"), video: renderedMP4(t, 1)}

	sweepVideoJob(t, store, job, retriever)

	entry := store.logs[AccountingLogID(ProviderJobKindVideo, schemas.Replicate, "pred_shorter")]
	require.NotNil(t, entry)
	require.NotNil(t, entry.Cost)
	assert.InDelta(t, 8*0.70, *entry.Cost, 1e-12, "charged the 8 seconds asked for, not the 1-second file")
	assert.Equal(t, 8, *entry.VideoDebugParsed.Accounting.Seconds)
	assert.False(t, entry.VideoDebugParsed.Accounting.Incomplete)
}

// An ffmpeg-written MP4 (TXE-6774's 12-second fixture, moov after mdat) measures as 12 seconds.
func TestVideoPoll_MeasuresAnFFmpegMP4(t *testing.T) {
	video, err := os.ReadFile("../videoduration/testdata/twelve-seconds.mp4")
	require.NoError(t, err)
	job := replicateVideoJob(t, "pred_ffmpeg", 5)
	poll, err := NewVideoSettler(&fakeVideoRetriever{resp: silentReplicateResponse("pred_ffmpeg"), video: video}).Poll(context.Background(), job)
	require.NoError(t, err)
	require.True(t, poll.Settleable)
	payload, ok := poll.Payload.(VideoPayload)
	require.True(t, ok)
	require.NotNil(t, payload.RenderedSeconds)
	assert.Equal(t, 12, *payload.RenderedSeconds)
}

// A job whose rendered length cannot be measured rendered a video all the same, so it is never
// left uncharged: the seconds its create named are charged as a floor, and the row is marked
// incomplete, since a model-defined input may have made the video longer. Leaving it unpriced
// would let a caller pick an output the settler cannot read and pay nothing.
func TestVideoSweeper_UnmeasurableVideoIsChargedTheRequestedSecondsAsAFloor(t *testing.T) {
	whole := renderedMP4(t, 12)
	for name, retriever := range map[string]VideoRetriever{
		"no downloader":                    &retrieveOnly{resp: silentReplicateResponse("pred_x")},
		"not an MP4":                       &fakeVideoRetriever{resp: silentReplicateResponse("pred_x"), video: []byte("<html>not a video</html>")},
		"an empty file":                    &fakeVideoRetriever{resp: silentReplicateResponse("pred_x"), video: []byte{}},
		"a truncated MP4":                  &fakeVideoRetriever{resp: silentReplicateResponse("pred_x"), video: whole[:len(whole)-20]},
		"a file past the limit":            &fakeVideoRetriever{resp: silentReplicateResponse("pred_x"), downloadErr: fmt.Errorf("download: %w", ErrVideoDurationUnmeasurable)},
		"an output the vendor has deleted": &fakeVideoRetriever{resp: silentReplicateResponse("pred_x"), downloadErr: providerErr(404, "output expired")},
		"a job that has no clips":          &fakeVideoRetriever{resp: &schemas.BifrostVideoGenerationResponse{ID: "pred_x", Status: schemas.VideoStatusCompleted}, video: whole},
	} {
		t.Run(name, func(t *testing.T) {
			job := replicateVideoJob(t, "pred_x", 5)
			poll, err := NewVideoSettler(retriever).Poll(context.Background(), job)
			require.NoError(t, err)
			require.NotNil(t, poll)
			assert.True(t, poll.Terminal)
			assert.True(t, poll.Settleable)
			payload := poll.Payload.(VideoPayload)
			assert.Nil(t, payload.RenderedSeconds)
			assert.NotEmpty(t, payload.RenderedUnmeasurable)

			store := newFakeAccountingStore()
			reporter := sweepVideoJob(t, store, replicateVideoJob(t, "pred_x", 5), retriever)
			entry := store.logs[AccountingLogID(ProviderJobKindVideo, schemas.Replicate, "pred_x")]
			require.NotNil(t, entry, "the job is charged")
			require.NotNil(t, entry.Cost)
			assert.InDelta(t, 5*0.70, *entry.Cost, 1e-12, "at the requested seconds, the floor")
			assert.True(t, entry.VideoDebugParsed.Accounting.Incomplete, "a length nobody confirmed is marked")
			require.Len(t, reporter.reports, 1, "the budget is charged")
			assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, store.jobs[job.ID].AccountingStatus)
		})
	}
}

// retrieveOnly is a VideoRetriever with no download.
type retrieveOnly struct {
	resp *schemas.BifrostVideoGenerationResponse
}

func (r *retrieveOnly) RetrieveVideo(context.Context, *cstables.TableProviderJob) (*schemas.BifrostVideoGenerationResponse, error) {
	return r.resp, nil
}

// A download that fails for a reason a retry could change is retried, with the job still due, even
// for a job that spent most of its polls rendering; the last attempt before the sweeper's cap
// settles it at its floor rather than letting the cap park it uncharged.
func TestVideoPoll_TransientDownloadFailureRetriesThenSettlesAtTheFloor(t *testing.T) {
	transient := errors.New("connection reset")
	retriever := &fakeVideoRetriever{resp: silentReplicateResponse("pred_dl"), downloadErr: transient}

	job := replicateVideoJob(t, "pred_dl", 5)
	_, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.ErrorIs(t, err, transient, "a transient download failure is retried")

	store := newFakeAccountingStore()
	sweepVideoJob(t, store, job, retriever)
	assert.Empty(t, store.logs)
	rescheduled := store.jobs[job.ID]
	assert.Equal(t, cstables.ProviderJobAccountingStatusPending, rescheduled.AccountingStatus)
	assert.Equal(t, 1, rescheduled.PollAttempts)

	job.PollAttempts = 30 // rendered for 30 polls, then the first download failed
	_, err = NewVideoSettler(retriever).Poll(context.Background(), job)
	require.ErrorIs(t, err, transient, "the polls spent rendering do not use up the download's retries")

	job.PollAttempts = MaxPollAttempts - 1
	poll, err := NewVideoSettler(retriever).Poll(context.Background(), job)
	require.NoError(t, err)
	assert.True(t, poll.Settleable, "the last attempt settles at the floor rather than run out the poll cap uncharged")
	assert.Contains(t, poll.Payload.(VideoPayload).RenderedUnmeasurable, "connection reset")
}

// A vendor that reports the duration or its own cost is priced from that, with no download.
func TestVideoPoll_ReportedDurationOrCostNeedsNoDownload(t *testing.T) {
	for name, resp := range map[string]*schemas.BifrostVideoGenerationResponse{
		"seconds": {ID: "vid_r", Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("8"), Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}},
		"cost":    {ID: "vid_r", Status: schemas.VideoStatusCompleted, Usage: &schemas.VideoUsage{Cost: &schemas.BifrostCost{TotalCost: 0.4}}, Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}}},
		"failed":  {ID: "vid_r", Status: schemas.VideoStatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			retriever := &fakeVideoRetriever{resp: resp, video: renderedMP4(t, 12)}
			poll, err := NewVideoSettler(retriever).Poll(context.Background(), videoJob(t, "vid_r", generationDims("sora-2-pro", "1920x1080", 8)))
			require.NoError(t, err)
			assert.True(t, poll.Settleable)
			assert.Zero(t, retriever.downloads)
			assert.False(t, RenderedDurationRequired(resp))
		})
	}
}

// Settle never prices a silent response from the request alone: without the measurement it
// returns an error, which the sweeper retries.
func TestVideoSettle_SilentResponseWithoutAMeasurementSettlesNothing(t *testing.T) {
	job := replicateVideoJob(t, "pred_unmeasured", 5)
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
		Provider: schemas.Replicate, ProviderJobID: job.JobID, Job: job,
		Payload: VideoPayload{Response: silentReplicateResponse("pred_unmeasured")},
	})
	require.Error(t, err)
	assert.Nil(t, settlement)
}

// The retrieve path never downloads the video: a silent response is left to the sweeper, which
// then settles it at the measured length.
func TestAccountVideoJob_LeavesASilentResponseToTheSweeper(t *testing.T) {
	store := newFakeAccountingStore()
	reporter := &fakeUsageReporter{}
	job := replicateVideoJob(t, "pred_inline", 5)
	require.NoError(t, store.UpsertProviderJob(context.Background(), job))

	out, err := AccountVideoJob(context.Background(), store, store, fakePricing{}, JobRequest{
		Provider: schemas.Replicate, ProviderJobID: "pred_inline", UsageReporter: reporter, ClaimedBy: "logging-node",
		Payload: VideoPayload{Response: silentReplicateResponse("pred_inline")},
	})
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.Empty(t, store.logs)
	assert.Equal(t, cstables.ProviderJobAccountingStatusPending, store.jobs[job.ID].AccountingStatus, "the job stays due")

	retriever := &fakeVideoRetriever{resp: silentReplicateResponse("pred_inline"), video: renderedMP4(t, 12)}
	sweepVideoJob(t, store, job, retriever)
	entry := store.logs[AccountingLogID(ProviderJobKindVideo, schemas.Replicate, "pred_inline")]
	require.NotNil(t, entry)
	assert.InDelta(t, 12*0.70, *entry.Cost, 1e-12)
}

func TestRenderedSeconds(t *testing.T) {
	for ms, want := range map[int64]int{
		1:     1,
		499:   1,
		1499:  1,
		1500:  2,
		5063:  5, // Wan's 81 frames at 16 per second
		5042:  5, // 121 frames at 24 per second
		4960:  5,
		12000: 12,
		12500: 13,
	} {
		assert.Equal(t, want, RenderedSeconds(ms), "%d ms", ms)
	}
	// The built fixture measures as built.
	poll, err := NewVideoSettler(&fakeVideoRetriever{resp: silentReplicateResponse("pred_ms"), video: renderedMP4MS(t, 7600)}).Poll(context.Background(), replicateVideoJob(t, "pred_ms", 5))
	require.NoError(t, err)
	assert.Equal(t, 8, *poll.Payload.(VideoPayload).RenderedSeconds)
}

// BenchmarkRenderedDurationRequired is the request-path cost TXE-7329 adds: the logging plugin's
// inline settlement on a terminal retrieve asks it once, to leave a silent response to the
// sweeper.
func BenchmarkRenderedDurationRequired(b *testing.B) {
	silent := silentReplicateResponse("pred_bench")
	reported := &schemas.BifrostVideoGenerationResponse{ID: "vid_bench", Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("8"), Videos: silent.Videos}
	b.ReportAllocs()
	for b.Loop() {
		_ = RenderedDurationRequired(silent)
		_ = RenderedDurationRequired(reported)
	}
}

// videoDurationFixture is one video provider's stand-in vendor for
// TestEveryVideoProviderSettlesAJobCreatedWithoutSeconds: the model a create names, and the
// vendor's answers to that create and to the query of the finished task, in the vendor's own
// shape, as the vendor answers a create that names no duration.
type videoDurationFixture struct {
	model string
	// answer returns the vendor's answer to one request below the provider's base URL.
	answer func(method, body string) (status int, response string)
}

// videoDurationFixtures holds one stand-in vendor per provider in videoProviders. Each answer is
// the task as the vendor documents it, with only the fields the provider reads.
var videoDurationFixtures = map[schemas.ModelProvider]videoDurationFixture{
	// Sora echoes the duration it renders on every video object; "4" is its default.
	schemas.OpenAI: {model: "sora-2", answer: func(method, _ string) (int, string) {
		status := "queued"
		if method == http.MethodGet {
			status = "completed"
		}
		return 200, `{"id":"video_1","object":"video","model":"sora-2","status":"` + status + `","seconds":"4","size":"720x1280","created_at":1762700000}`
	}},
	// Veo reports no duration; core fills Veo's default when a create names none (BackfillParams).
	schemas.Gemini: {model: "veo-3.1-generate-preview", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"name":"models/veo-3.1-generate-preview/operations/op1"}`
		}
		return 200, `{"name":"models/veo-3.1-generate-preview/operations/op1","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://example.invalid/v.mp4"}}]}}}`
	}},
	// A hosted model picks its own length and the prediction reports neither the duration nor a
	// cost, so replicate refuses a create that names no seconds before sending it (TXE-7297).
	// The answers are what Replicate would say if the create were sent anyway.
	schemas.Replicate: {model: "google/veo-3", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 201, `{"id":"pred1","model":"google/veo-3","status":"starting","created_at":"2026-11-13T00:00:00Z"}`
		}
		return 200, `{"id":"pred1","model":"google/veo-3","status":"succeeded","output":"https://example.invalid/v.mp4","created_at":"2026-11-13T00:00:00Z","completed_at":"2026-11-13T00:01:00Z","metrics":{"predict_time":42.5}}`
	}},
	schemas.Runway: {model: "gen4_turbo", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"id":"task1"}`
		}
		return 200, `{"id":"task1","status":"SUCCEEDED","createdAt":"2026-11-13T00:00:00Z","output":["https://example.invalid/v.mp4"]}`
	}},
	// Runware reports the task's cost in USD (includeCost).
	schemas.Runware: {model: "klingai:5@3", answer: func(_, body string) (int, string) {
		if strings.Contains(body, `"videoInference"`) {
			return 200, `{"data":[{"taskType":"videoInference","taskUUID":"` + runwareTaskUUID(body) + `"}]}`
		}
		return 200, `{"data":[{"taskType":"getResponse","taskUUID":"` + runwareTaskUUID(body) + `","status":"success","videoURL":"https://example.invalid/v.mp4","cost":0.5}]}`
	}},
	schemas.MiniMax: {model: "MiniMax-Hailuo-02", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"task_id":"t1","base_resp":{"status_code":0,"status_msg":"success"}}`
		}
		return 200, `{"task":{"id":"t1","model":"MiniMax-Hailuo-02","status":"succeeded","content":{"url":"https://example.invalid/v.mp4"},"resolution":"768P","ratio":"16:9","duration":6},"base_resp":{"status_code":0,"status_msg":"success"}}`
	}},
	schemas.BytePlus: {model: "seedance-1-0-pro-250528", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"id":"cgt-1"}`
		}
		return 200, `{"id":"cgt-1","model":"seedance-1-0-pro-250528","status":"succeeded","content":{"video_url":"https://example.invalid/v.mp4"},"resolution":"720p","ratio":"16:9","duration":5,"usage":{"completion_tokens":108900,"total_tokens":108900}}`
	}},
	schemas.Kling: {model: "kling-3.0", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"code":0,"message":"SUCCEED","request_id":"r1","data":{"id":"k1","status":"submitted"}}`
		}
		return 200, `{"code":0,"message":"SUCCEED","request_id":"r2","data":[{"id":"k1","status":"succeeded","outputs":[{"type":"video","id":"o1","url":"https://example.invalid/v.mp4","duration":"5.1"}]}]}`
	}},
	schemas.DashScope: {model: "wan2.6-t2v", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"request_id":"r1","output":{"task_id":"ds1","task_status":"PENDING"}}`
		}
		return 200, `{"request_id":"r2","output":{"task_id":"ds1","task_status":"SUCCEEDED","submit_time":"2026-11-13 08:00:00.000","end_time":"2026-11-13 08:01:00.000","video_url":"https://example.invalid/v.mp4"},"usage":{"output_video_duration":5}}`
	}},
	// xAI reports the clip's duration and its cost in USD ticks.
	schemas.XAI: {model: "grok-imagine-video", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			return 200, `{"request_id":"x1"}`
		}
		return 200, `{"status":"done","model":"grok-imagine-video","video":{"url":"https://example.invalid/v.mp4","duration":6},"usage":{"cost_in_usd_ticks":3000000000}}`
	}},
	// BFL's get_result reports no duration, only its settled cost in credits (TXE-7297).
	schemas.BFL: {model: "flux-3-video", answer: func(method, _ string) (int, string) {
		if method == http.MethodPost {
			// No polling_url: the task id then records no polling host, and the query polls the
			// configured base, which is this stand-in.
			return 200, `{"id":"b1","cost":30}`
		}
		return 200, `{"id":"b1","status":"Ready","result":{"sample":"https://example.invalid/v.mp4","end_time":1762700000},"cost":30}`
	}},
}

// videoDurationExemptProviders are providers in videoProviders this test cannot reach through a
// stand-in vendor, each with the reason and where the same guarantee is checked instead.
var videoDurationExemptProviders = map[schemas.ModelProvider]string{
	schemas.Vertex: "Vertex dials its regional Google host over TLS whatever the base URL. It runs Gemini's Veo converter, and core's BackfillParams gives it Veo's default duration, which TestVertexVeoWithoutSecondsGetsVeosDefault checks",
}

// runwareTaskUUID reads the taskUUID a Runware request names.
func runwareTaskUUID(body string) string {
	const marker = `"taskUUID":"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	return rest[:strings.IndexByte(rest, '"')]
}

type videoDurationAccount struct{ base string }

func (a *videoDurationAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	providers := make([]schemas.ModelProvider, 0, len(videoProviders))
	for provider := range videoProviders {
		providers = append(providers, provider)
	}
	return providers, nil
}

func (a *videoDurationAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "k", Name: "k", Value: *schemas.NewSecretVar("test-key"), Weight: 1, Models: schemas.WhiteList{"*"}}}, nil
}

func (a *videoDurationAccount) GetConfigForProvider(provider schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		BaseURL: a.base + "/" + string(provider), DefaultRequestTimeoutInSeconds: 5, MaxRetries: 0, AllowPrivateNetwork: true,
	}}, nil
}

// TestEveryVideoProviderSettlesAJobCreatedWithoutSeconds (TXE-7297) is the class check for a
// video job that settles unpriced. The settler prices a job at the vendor's own USD cost when the
// task reports one, and otherwise at the datasheet's per-second rate times the job's seconds: the
// request's, the create response's or the finished task's. A job with neither a duration nor a
// provider cost settles unpriced, and the virtual key's budget is never charged (bfl with seconds
// auto or omitted, TXE-6617). Every provider in videoProviders is driven through core with a
// create that names no seconds, then the query of the finished task, against a stand-in vendor
// answering as that vendor does; the dimensions settlement reads must hold a duration or a
// provider cost. A provider added to videoProviders without a fixture fails here.
func TestEveryVideoProviderSettlesAJobCreatedWithoutSeconds(t *testing.T) {
	var mu sync.Mutex
	reached := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		provider, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		mu.Lock()
		reached[provider]++
		mu.Unlock()
		fixture, ok := videoDurationFixtures[schemas.ModelProvider(provider)]
		if !ok {
			http.Error(w, `{"error":{"message":"no fixture"}}`, http.StatusNotFound)
			return
		}
		status, answer := fixture.answer(r.Method, string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(upstream.Close)

	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &videoDurationAccount{base: upstream.URL}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Shutdown)

	providers := make([]string, 0, len(videoProviders))
	for provider := range videoProviders {
		providers = append(providers, string(provider))
	}
	sort.Strings(providers)
	for _, name := range providers {
		provider := schemas.ModelProvider(name)
		t.Run(name, func(t *testing.T) {
			if reason, exempt := videoDurationExemptProviders[provider]; exempt {
				t.Skip(reason)
			}
			fixture, ok := videoDurationFixtures[provider]
			if !ok {
				t.Fatalf("%s is in videoProviders with no stand-in vendor in videoDurationFixtures: add one that answers as the vendor does to a create naming no seconds, so this test can tell whether its jobs settle priced", provider)
			}
			ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			created, bifrostErr := client.VideoGenerationRequest(ctx, &schemas.BifrostVideoGenerationRequest{
				Provider: provider, Model: fixture.model, Input: &schemas.VideoGenerationInput{Prompt: "a paper boat"},
			})
			if bifrostErr != nil {
				// The other way to meet the rule: refuse the create before any upstream call,
				// naming seconds, so no job exists to settle.
				mu.Lock()
				calls := reached[name]
				mu.Unlock()
				if calls == 0 && bifrostErr.StatusCode != nil && *bifrostErr.StatusCode == http.StatusBadRequest &&
					bifrostErr.ExtraFields.ErrorType == schemas.ErrorTypeCallerUnsupportedParameter &&
					bifrostErr.Error != nil && bifrostErr.Error.Param == "seconds" {
					return
				}
				t.Fatalf("create (upstream calls %d): %s", calls, videoErrorText(bifrostErr))
			}
			queryCtx, cancelQuery := schemas.NewBifrostContextWithTimeout(context.Background(), 10*time.Second)
			defer cancelQuery()
			finished, bifrostErr := client.VideoRetrieveRequest(queryCtx, &schemas.BifrostVideoRetrieveRequest{Provider: provider, ID: created.ID})
			if bifrostErr != nil {
				t.Fatalf("query of %q: %s", created.ID, videoErrorText(bifrostErr))
			}
			if finished.Status != schemas.VideoStatusCompleted {
				t.Fatalf("the fixture's finished task reads as %q, not completed: fix the fixture", finished.Status)
			}
			// What the job row captures at the create, layered under what the finished task says:
			// the settler's own merge (Settle).
			dims := modelcatalog.VideoDimensionsFromResponse(created).MergedWith(modelcatalog.VideoDimensionsFromResponse(finished))
			if dims.Seconds == nil && dims.ProviderCost == nil {
				t.Errorf("%s: a job created without seconds settles with neither a duration nor a provider cost, so it is recorded unpriced and the virtual key's budget is never charged. Record the duration the vendor renders (on the create or the finished task), or report the vendor's cost in USD as usage.cost", provider)
			}
		})
	}
}

func videoErrorText(bifrostErr *schemas.BifrostError) string {
	if bifrostErr == nil || bifrostErr.Error == nil {
		return "<no error body>"
	}
	return bifrostErr.Error.Message
}

// TestVertexVeoWithoutSecondsGetsVeosDefault is the check videoDurationExemptProviders names for
// vertex: a Veo create on Vertex that names no seconds carries Veo's default duration after
// core's BackfillParams, so its job settles priced. An Omni model picks its own length and gets
// no default; Vertex Omni, like Gemini Omni, is outside this check's Veo row.
func TestVertexVeoWithoutSecondsGetsVeosDefault(t *testing.T) {
	req := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Vertex, Model: "veo-3.1-generate-preview", Input: &schemas.VideoGenerationInput{Prompt: "a paper boat"},
	}}
	resp := &schemas.BifrostVideoGenerationResponse{Model: "veo-3.1-generate-preview"}
	resp.BackfillParams(req)
	dims := modelcatalog.VideoDimensionsFromResponse(resp)
	if dims.Seconds == nil || *dims.Seconds <= 0 {
		t.Fatalf("a Vertex Veo create with no seconds settles with no duration: seconds %v", resp.Seconds)
	}
}

// dimsRecordingPricing is fakePricing that keeps the dimensions it was asked to price.
type dimsRecordingPricing struct {
	fakePricing
	seen []modelcatalog.VideoPricingDimensions
	// tierWins makes the requested tier set the band even beside a banded size, as the real
	// catalog does for a provider that renders the requested tier.
	tierWins bool
}

func (p *dimsRecordingPricing) CalculateVideoCostDetails(dims modelcatalog.VideoPricingDimensions, provider schemas.ModelProvider, scopes *modelcatalog.PricingLookupScopes) modelcatalog.VideoCostDetails {
	p.seen = append(p.seen, dims)
	details := p.fakePricing.CalculateVideoCostDetails(dims, provider, scopes)
	if p.tierWins && details.Priced {
		details.PricedResolution = dims.BandResolution()
	}
	return details
}

// A tier-only request (Veo "4k", Wan "480P") has no size, so the tier is the only record of its
// rate band. The settlement row must carry it and a reprice must hand it back to pricing, or a
// repriced 4K Veo clip falls back to the unbanded rate (TXE-7349).
func TestVideoSettle_ResolutionTierSurvivesSettlementAndReprice(t *testing.T) {
	for _, tc := range []struct {
		name string
		dims modelcatalog.VideoPricingDimensions
	}{
		{name: "typed resolution", dims: func() modelcatalog.VideoPricingDimensions {
			d := generationDims("sora-2-pro", "", 8)
			d.Resolution = "4k"
			return d
		}()},
		{name: "job row written before Resolution existed", dims: func() modelcatalog.VideoPricingDimensions {
			d := generationDims("sora-2-pro", "", 8)
			d.Extra = map[string]any{"resolution": "4k"}
			return d
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Veo reads the tier from both the typed field and extra_params.
			job := videoJob(t, "vid_tier", tc.dims)
			renderedSeconds := 8
			settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
				Provider: schemas.Gemini, ProviderJobID: job.JobID, FallbackModel: job.Model, Job: job,
				Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{
					Status: schemas.VideoStatusCompleted,
					Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
				}, RenderedSeconds: &renderedSeconds},
			})
			require.NoError(t, err)
			require.NotNil(t, settlement.ApplyDebug)
			entry := &logstore.Log{Model: "sora-2-pro", Provider: string(schemas.Gemini)}
			settlement.ApplyDebug(entry)
			require.NotNil(t, entry.VideoDebugParsed.Accounting)
			assert.Equal(t, "4k", entry.VideoDebugParsed.Accounting.Resolution)

			pricing := &dimsRecordingPricing{}
			_, err = NewVideoSettler(nil).RepriceFromLog(entry, pricing)
			require.NoError(t, err)
			require.Len(t, pricing.seen, 1)
			assert.Equal(t, "4k", pricing.seen[0].Resolution)
		})
	}
}

// A custom provider on a Veo base renders the requested tier like Gemini does, so settlement hands
// pricing the base provider to choose the band by, and records the tier that won. A reprice runs
// with no resolver (RepriceLog builds NewVideoSettler(nil)), so it prices from that recorded tier
// alone rather than choosing again (TXE-7349).
func TestVideoSettle_CustomProviderPricesWithItsBaseProvider(t *testing.T) {
	retriever := &resolvingVideoRetriever{bases: map[schemas.ModelProvider]schemas.ModelProvider{"my-veo": schemas.Gemini}}
	dims := generationDims("sora-2-pro", "3840x2160", 8)
	dims.Resolution = "1080p"
	job := videoJob(t, "vid_custom_veo", dims)
	pricing := &dimsRecordingPricing{tierWins: true}
	settlement, err := NewVideoSettler(retriever).Settle(context.Background(), pricing, JobRequest{
		Provider: "my-veo", ProviderJobID: job.JobID, FallbackModel: job.Model, Job: job,
		Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("8"),
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		}},
	})
	require.NoError(t, err)
	require.Len(t, pricing.seen, 1)
	assert.Equal(t, schemas.Gemini, pricing.seen[0].BaseProvider)

	entry := &logstore.Log{Model: "sora-2-pro", Provider: "my-veo"}
	settlement.ApplyDebug(entry)
	assert.Equal(t, "1080p", entry.VideoDebugParsed.Accounting.Resolution)

	repricing := &dimsRecordingPricing{}
	_, err = NewVideoSettler(nil).RepriceFromLog(entry, repricing)
	require.NoError(t, err)
	require.Len(t, repricing.seen, 1)
	assert.Equal(t, "1080p", repricing.seen[0].Resolution)
	assert.Equal(t, "", repricing.seen[0].Size, "the recorded tier prices alone; the size must not reopen the choice")
}

// Without a winning tier the settlement records none, and a reprice prices from the size alone.
func TestVideoSettle_SizeThatSetTheBandRepricesBySize(t *testing.T) {
	dims := generationDims("sora-2-pro", "1920x1080", 8)
	dims.Resolution = "480p"
	job := videoJob(t, "vid_size_band", dims)
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), &dimsRecordingPricing{}, JobRequest{
		Provider: schemas.OpenAI, ProviderJobID: job.JobID, FallbackModel: job.Model, Job: job,
		Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("8"),
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		}},
	})
	require.NoError(t, err)
	entry := &logstore.Log{Model: "sora-2-pro", Provider: string(schemas.OpenAI)}
	settlement.ApplyDebug(entry)
	assert.Equal(t, "", entry.VideoDebugParsed.Accounting.Resolution)

	repricing := &dimsRecordingPricing{}
	_, err = NewVideoSettler(nil).RepriceFromLog(entry, repricing)
	require.NoError(t, err)
	require.Len(t, repricing.seen, 1)
	assert.Equal(t, "1920x1080", repricing.seen[0].Size)
	assert.Equal(t, "", repricing.seen[0].Resolution)
}

// A tier the provider never reads is not recorded as the band, so a reprice cannot take it
// either: OpenAI reads no resolution tier (TXE-7349).
func TestVideoSettle_TierTheProviderIgnoresIsNotRecorded(t *testing.T) {
	dims := generationDims("sora-2-pro", "", 8)
	dims.Resolution = "480p"
	job := videoJob(t, "vid_ignored_tier", dims)
	settlement := settleVideoRendered(t, job, &schemas.BifrostVideoGenerationResponse{
		Status: schemas.VideoStatusCompleted,
		Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
	}, 8)
	entry := &logstore.Log{Model: "sora-2-pro", Provider: string(schemas.OpenAI)}
	settlement.ApplyDebug(entry)
	assert.Equal(t, "", entry.VideoDebugParsed.Accounting.Resolution)
}

// A Veo job that requested size 1280x720 and resolution 4k renders 4K: Veo reads the size only for
// the aspect ratio. When its rate is missing at settlement, the unpriced record must still carry
// the 4k tier, or the backfill reprices the job at the requested 720p size once the rate lands
// (TXE-7349).
func TestVideoSettle_UnpricedJobKeepsARenderedTierBesideARequestedSize(t *testing.T) {
	dims := generationDims("veo-3.1-generate-001", "1280x720", 8)
	dims.Resolution = "4k"
	job := videoJob(t, "vid_unpriced_tier", dims)
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
		Provider: schemas.Gemini, ProviderJobID: job.JobID, FallbackModel: job.Model, Job: job,
		Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("8"),
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		}},
	})
	require.NoError(t, err)
	require.True(t, settlement.RecordUnpriced, "fakePricing has no rate for this model")
	entry := &logstore.Log{Model: "veo-3.1-generate-001", Provider: string(schemas.Gemini)}
	settlement.ApplyDebug(entry)
	assert.Equal(t, "4k", entry.VideoDebugParsed.Accounting.Resolution)

	repricing := &dimsRecordingPricing{}
	_, _ = NewVideoSettler(nil).RepriceFromLog(entry, repricing)
	require.Len(t, repricing.seen, 1)
	assert.Equal(t, "4k", repricing.seen[0].Resolution)
	assert.Equal(t, "", repricing.seen[0].Size)
	assert.True(t, repricing.seen[0].ResolutionSettled)
}

// A reprice takes the recorded tier as settled. MiniMax reads its tier from extra_params only, and
// a reprice hands pricing the tier in the typed field; judged again against the request sources it
// would be an unread tier and lose to a dearer unbanded rate, so a 768P job settled at the band
// would reprice above it (TXE-7349).
func TestVideoReprice_RecordedTierIsSettled(t *testing.T) {
	dims := generationDims("sora-2-pro", "", 6)
	dims.Extra = map[string]any{"resolution": "768P"}
	job := videoJob(t, "vid_minimax_tier", dims)
	settlement, err := NewVideoSettler(nil).Settle(context.Background(), fakePricing{}, JobRequest{
		Provider: schemas.MiniMax, ProviderJobID: job.JobID, FallbackModel: job.Model, Job: job,
		Payload: VideoPayload{Response: &schemas.BifrostVideoGenerationResponse{
			Status: schemas.VideoStatusCompleted, Seconds: bifrost.Ptr("6"),
			Videos: []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL}},
		}},
	})
	require.NoError(t, err)
	entry := &logstore.Log{Model: "sora-2-pro", Provider: string(schemas.MiniMax)}
	settlement.ApplyDebug(entry)
	require.Equal(t, "768P", entry.VideoDebugParsed.Accounting.Resolution)

	repricing := &dimsRecordingPricing{}
	_, err = NewVideoSettler(nil).RepriceFromLog(entry, repricing)
	require.NoError(t, err)
	require.Len(t, repricing.seen, 1)
	assert.Equal(t, "768P", repricing.seen[0].Resolution)
	assert.True(t, repricing.seen[0].ResolutionSettled)
	assert.Nil(t, repricing.seen[0].Extra, "the tier reaches pricing only as the settled typed field")
}
