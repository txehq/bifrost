package jobaccounting

// Azure video settlement (fork, TXE-7054). These tests drive the real Azure provider, through a
// Bifrost client, against a local stand-in for an Azure resource. The ids the settler keys on and
// the dimensions it prices from are therefore the ones the provider really produces: Sora 2 on
// /openai/v1/videos and first-generation sora on the video generation jobs API (TXE-6533).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	cstables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	azureVideoSora2ID = "video_68f0"
	azureVideoJobID   = "task_01jwcet0eje35tc5jy54yjax5q"
	azureVideoGenID   = "gen_01jwcf0a"
)

// azureVideoJob is a jobs API job in the reference's VideoGenerationJob shape; a succeeded one
// carries one generation of 5 seconds at 480x480.
func azureVideoJob(status string) string {
	generations := `[]`
	if status == "succeeded" {
		generations = `[{"object":"video.generation","id":"` + azureVideoGenID + `","job_id":"` + azureVideoJobID +
			`","created_at":1748469990,"width":480,"height":480,"n_seconds":5,"prompt":"A cat playing piano in a jazz bar."}]`
	}
	return `{"object":"video.generation.job","id":"` + azureVideoJobID + `","status":"` + status +
		`","created_at":1748469875,"finished_at":null,"expires_at":null,"generations":` + generations +
		`,"prompt":"A cat playing piano in a jazz bar.","model":"sora","n_variants":1,"n_seconds":5,"height":480,"width":480,"failure_reason":null}`
}

// azureVideoResource stands in for one Azure resource serving both video APIs. The jobs API job
// is queued until succeed is called, as a real job is while it renders.
type azureVideoResource struct {
	mu        sync.Mutex
	succeeded bool
	calls     []string
}

func (r *azureVideoResource) succeed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.succeeded = true
}

func (r *azureVideoResource) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *azureVideoResource) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.calls = append(r.calls, req.Method+" "+req.URL.Path)
	succeeded := r.succeeded
	r.mu.Unlock()
	_, _ = io.Copy(io.Discard, req.Body)

	jobs := "/openai/v1/video/generations/jobs"
	switch {
	case req.Method == http.MethodPost && req.URL.Path == jobs:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, azureVideoJob("queued"))
	case req.Method == http.MethodGet && req.URL.Path == jobs+"/"+azureVideoJobID:
		w.Header().Set("Content-Type", "application/json")
		status := "running"
		if succeeded {
			status = "succeeded"
		}
		_, _ = io.WriteString(w, azureVideoJob(status))
	case req.Method == http.MethodGet && req.URL.Path == "/openai/v1/video/generations/"+azureVideoGenID+"/content/video":
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "\x00\x00\x00\x18ftypmp42")
	case req.Method == http.MethodGet && req.URL.Path == "/openai/v1/videos/"+azureVideoSora2ID:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`+azureVideoSora2ID+`","object":"video","status":"completed","created_at":1,"completed_at":2,"expires_at":90000,"model":"sora-2","seconds":"8","size":"1280x720"}`)
	default:
		http.Error(w, `{"error":{"code":"NotFound","message":"no such route in the azure video stand-in"}}`, http.StatusNotFound)
	}
}

// azureVideoAccount configures azure with one key whose endpoint is the stand-in.
type azureVideoAccount struct{ endpoint string }

func (a *azureVideoAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Azure}, nil
}

func (a *azureVideoAccount) GetKeysForProvider(_ context.Context, _ schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{
		ID: "azure-key", Name: "azure-key", Value: *schemas.NewSecretVar("test-key"), Weight: 1, Models: schemas.WhiteList{"*"},
		AzureKeyConfig: &schemas.AzureKeyConfig{Endpoint: *schemas.NewSecretVar(a.endpoint)},
	}}, nil
}

func (a *azureVideoAccount) GetConfigForProvider(_ schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{
		DefaultRequestTimeoutInSeconds: 5, MaxRetries: 0, AllowPrivateNetwork: true,
	}}, nil
}

// clientVideoRetriever is the sweeper's retriever as the HTTP server builds it: a retrieve
// through the Bifrost client by the job row's provider and id.
type clientVideoRetriever struct {
	client *bifrost.Bifrost
	calls  int
}

func (r *clientVideoRetriever) RetrieveVideo(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostVideoGenerationResponse, error) {
	r.calls++
	resp, bifrostErr := r.client.VideoRetrieveRequest(schemas.NewBifrostContext(ctx, schemas.NoDeadline), &schemas.BifrostVideoRetrieveRequest{
		Provider: schemas.ModelProvider(job.Provider),
		ID:       job.JobID,
	})
	if bifrostErr != nil {
		return nil, NewProviderCallError(bifrostErr)
	}
	return resp, nil
}

func newAzureVideoClient(t *testing.T) (*bifrost.Bifrost, *azureVideoResource) {
	t.Helper()
	resource := &azureVideoResource{}
	server := httptest.NewServer(resource)
	t.Cleanup(server.Close)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: &azureVideoAccount{endpoint: server.URL}, Logger: bifrost.NewNoOpLogger(), InitialPoolSize: 1,
	})
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	return client, resource
}

// azureVideoPricing prices Azure's two video models per second per output, and records the
// dimensions it was asked to price.
type azureVideoPricing struct {
	fakePricing
	mu    sync.Mutex
	asked []modelcatalog.VideoPricingDimensions
}

func (p *azureVideoPricing) CalculateVideoCostDetails(dims modelcatalog.VideoPricingDimensions, provider schemas.ModelProvider, _ *modelcatalog.PricingLookupScopes) modelcatalog.VideoCostDetails {
	p.mu.Lock()
	p.asked = append(p.asked, dims)
	p.mu.Unlock()
	if provider != schemas.Azure || dims.Seconds == nil {
		return modelcatalog.VideoCostDetails{}
	}
	var rate float64
	switch {
	case dims.Model == "sora-2" && dims.Size == "1280x720":
		rate = 0.10
	case dims.Model == "sora" && dims.Size == "480x480":
		rate = 0.02
	default:
		return modelcatalog.VideoCostDetails{}
	}
	return modelcatalog.VideoCostDetails{Cost: float64(*dims.Seconds) * rate * float64(max(1, dims.OutputCount)), Priced: true}
}

// recordedAzureVideoJob is the row recordVideoJobLifecycle writes for an Azure create: the
// task id the create answered, the dimensions the request captured, and the virtual key's
// budgets. It is due now.
func recordedAzureVideoJob(t *testing.T, taskID string, captured modelcatalog.VideoPricingDimensions) *cstables.TableProviderJob {
	t.Helper()
	params, err := MarshalVideoDimensions(captured)
	require.NoError(t, err)
	due := time.Now().UTC().Add(-time.Minute)
	vk := "vk-budgeted"
	budgets := `["budget-dollars"]`
	return &cstables.TableProviderJob{
		ID:               cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.Azure), taskID),
		Kind:             cstables.ProviderJobKindVideo,
		Provider:         string(schemas.Azure),
		JobID:            taskID,
		Model:            captured.Model,
		Params:           params,
		SelectedKeyID:    "azure-key",
		VirtualKeyID:     &vk,
		BudgetIDs:        &budgets,
		AccountingStatus: cstables.ProviderJobAccountingStatusPending,
		NextCheckAt:      &due,
	}
}

// TestVideoSweeper_SettlesAzureSora2AndFirstGenerationSora: the sweeper settles a job from each
// Azure video API, polling it through the real provider by the task id the create returned. Each
// is priced from what its terminal response reports (Sora 2's seconds and size; the jobs API's
// n_seconds, width and height, one output per generation), and each charges the virtual key's
// budget once.
func TestVideoSweeper_SettlesAzureSora2AndFirstGenerationSora(t *testing.T) {
	client, resource := newAzureVideoClient(t)
	resource.succeed()

	sora2ID := azureVideoSora2ID + ":azure"
	sora1ID := "sora1." + azureVideoJobID + ":azure"
	store := newFakeAccountingStore()
	// The captured params name only the model: everything priced must come from the responses.
	require.NoError(t, store.UpsertProviderJob(context.Background(), recordedAzureVideoJob(t, sora2ID, modelcatalog.VideoPricingDimensions{Model: "sora-2", RequestType: schemas.VideoGenerationRequest})))
	require.NoError(t, store.UpsertProviderJob(context.Background(), recordedAzureVideoJob(t, sora1ID, modelcatalog.VideoPricingDimensions{Model: "sora", RequestType: schemas.VideoGenerationRequest})))

	retriever := &clientVideoRetriever{client: client}
	reporter := &fakeUsageReporter{}
	pricing := &azureVideoPricing{}
	sweeper := NewVideoSweeper(store, store, pricing, retriever, nil, reporter, SweeperConfig{Provider: schemas.Azure, Limit: 10})
	sweeper.SweepOnce(context.Background())

	assert.Equal(t, 2, retriever.calls, "each Azure job is polled once")
	assert.ElementsMatch(t, []string{
		"GET /openai/v1/videos/" + azureVideoSora2ID,
		"GET /openai/v1/video/generations/jobs/" + azureVideoJobID,
	}, resource.seen(), "each id is polled on its own API")

	for _, tc := range []struct {
		name, taskID, model, size string
		seconds                   int
		cost                      float64
	}{
		{"sora-2 on /videos", sora2ID, "sora-2", "1280x720", 8, 0.80},
		{"first-generation sora on the jobs API", sora1ID, "sora", "480x480", 5, 0.10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.Azure), tc.taskID)]
			require.NotNil(t, job)
			assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, job.AccountingStatus)

			logID := AccountingLogID(ProviderJobKindVideo, schemas.Azure, tc.taskID)
			entry := store.logs[logID]
			require.NotNil(t, entry, "the aggregate cost row is written")
			require.NotNil(t, entry.Cost)
			assert.InDelta(t, tc.cost, *entry.Cost, 1e-12)
			require.NotNil(t, entry.VideoDebugParsed)
			require.NotNil(t, entry.VideoDebugParsed.Accounting)
			accounting := entry.VideoDebugParsed.Accounting
			require.NotNil(t, accounting.Seconds)
			assert.Equal(t, tc.seconds, *accounting.Seconds)
			assert.Equal(t, tc.size, accounting.Size)
			assert.Equal(t, 1, accounting.OutputCount, "one output per generation")

			var report *UsageReport
			for i := range reporter.reports {
				if reporter.reports[i].RequestID == logID {
					report = &reporter.reports[i]
				}
			}
			require.NotNil(t, report, "governance is told of the cost")
			assert.InDelta(t, tc.cost, report.Cost, 1e-12)
			assert.Equal(t, []string{"budget-dollars"}, report.BudgetIDs, "the virtual key's dollar budget is charged")
			assert.Equal(t, "vk-budgeted", report.VirtualKeyID)
		})
	}
	require.Len(t, reporter.reports, 2)

	sweeper.SweepOnce(context.Background())
	assert.Len(t, reporter.reports, 2, "settlement is at-least-once, so a second sweep charges nothing more")
}

// TestVideoSweeper_SettlesAzureSoraCreateFollowedOnlyByContent is the sequence the Codex review
// of PR #164 found unsettled: a first-generation sora create, then GET /v1/videos/{id}/content
// with no explicit retrieve. The download queries the job inside the provider, so no top-level
// retrieve ever reaches the logging plugin's inline settlement. The create's job row is what is
// left, and the sweeper settles it and charges the budget.
func TestVideoSweeper_SettlesAzureSoraCreateFollowedOnlyByContent(t *testing.T) {
	client, resource := newAzureVideoClient(t)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	seconds := "5"
	created, bifrostErr := client.VideoGenerationRequest(ctx, &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Azure,
		Model:    "sora",
		Input:    &schemas.VideoGenerationInput{Prompt: "A cat playing piano in a jazz bar."},
		Params:   &schemas.VideoGenerationParameters{Seconds: &seconds, Size: "480x480"},
	})
	require.Nil(t, bifrostErr)
	require.NotNil(t, created)
	require.True(t, strings.HasPrefix(created.ID, "sora1."), "the create answers a jobs API task id, got %q", created.ID)

	// What recordVideoJobLifecycle writes for this create.
	five := 5
	store := newFakeAccountingStore()
	require.NoError(t, store.UpsertProviderJob(context.Background(), recordedAzureVideoJob(t, created.ID, modelcatalog.VideoPricingDimensions{
		Model: "sora", RequestType: schemas.VideoGenerationRequest, Seconds: &five, Size: "480x480",
	})))

	// The job finishes, and the caller only downloads it.
	resource.succeed()
	download, bifrostErr := client.VideoDownloadRequest(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), &schemas.BifrostVideoDownloadRequest{
		Provider: schemas.Azure,
		ID:       created.ID,
	})
	require.Nil(t, bifrostErr)
	require.NotNil(t, download)

	logID := AccountingLogID(ProviderJobKindVideo, schemas.Azure, created.ID)
	assert.Nil(t, store.logs[logID], "the download alone settles nothing")

	retriever := &clientVideoRetriever{client: client}
	reporter := &fakeUsageReporter{}
	sweeper := NewVideoSweeper(store, store, &azureVideoPricing{}, retriever, nil, reporter, SweeperConfig{Provider: schemas.Azure, Limit: 10})
	sweeper.SweepOnce(context.Background())

	job := store.jobs[cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(schemas.Azure), created.ID)]
	require.NotNil(t, job)
	assert.Equal(t, cstables.ProviderJobAccountingStatusAccounted, job.AccountingStatus)
	entry := store.logs[logID]
	require.NotNil(t, entry)
	require.NotNil(t, entry.Cost)
	assert.InDelta(t, 0.10, *entry.Cost, 1e-12)
	require.Len(t, reporter.reports, 1)
	assert.Equal(t, logID, reporter.reports[0].RequestID)
	assert.InDelta(t, 0.10, reporter.reports[0].Cost, 1e-12)
	assert.Equal(t, []string{"budget-dollars"}, reporter.reports[0].BudgetIDs, "the virtual key's dollar budget is charged")

	assert.Equal(t, []string{
		"POST /openai/v1/video/generations/jobs",
		"GET /openai/v1/video/generations/jobs/" + azureVideoJobID,
		"GET /openai/v1/video/generations/" + azureVideoGenID + "/content/video",
		"GET /openai/v1/video/generations/jobs/" + azureVideoJobID,
	}, resource.seen(), "create, the download's job query and content, then the sweeper's poll")
}
