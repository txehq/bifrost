package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// newDeepgramAsyncTestHandler builds the async handler over a real job executor (a SQLite
// log store in the test's temp dir) and a Bifrost client whose Deepgram provider points at
// upstream.
func newDeepgramAsyncTestHandler(t *testing.T, upstream *deepgramUpstream) *AsyncHandler {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: audioUpstreamAccount{baseURLs: map[schemas.ModelProvider]string{schemas.Deepgram: srv.URL}},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(client.Shutdown)

	ctx := context.Background()
	store, err := logstore.NewLogStore(ctx, &logstore.Config{
		Type:   logstore.LogStoreTypeSQLite,
		Config: &logstore.SQLiteConfig{Path: filepath.Join(t.TempDir(), "async-transcription.db")},
	}, testLogger{})
	if err != nil {
		t.Fatalf("logstore.NewLogStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(ctx) })

	config := &lib.Config{ClientConfig: &configstore.ClientConfig{}}
	return &AsyncHandler{
		client:       client,
		executor:     logstore.NewAsyncJobExecutor(store, nil, nil, nil, testLogger{}),
		handlerStore: config,
		config:       config,
	}
}

// asyncTranscriptionJob is the part of the async job response the tests read.
type asyncTranscriptionJob struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code"`
	Result     struct {
		Text  string `json:"text"`
		Usage struct {
			Type    string  `json:"type"`
			Seconds float64 `json:"seconds"`
		} `json:"usage"`
	} `json:"result"`
	Error *struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		StatusCode  int `json:"status_code"`
		ExtraFields struct {
			Provider string `json:"provider"`
		} `json:"extra_fields"`
	} `json:"error"`
}

// submitAndAwaitTranscription posts the form to /v1/async/audio/transcriptions, expects 202
// and a job id, then reads GET /v1/async/audio/transcriptions/{job_id} until the job leaves
// pending and processing.
func submitAndAwaitTranscription(t *testing.T, h *AsyncHandler, submit *fasthttp.RequestCtx) asyncTranscriptionJob {
	t.Helper()
	submit.Request.SetRequestURI("/v1/async/audio/transcriptions")
	h.asyncTranscription(submit)
	if submit.Response.StatusCode() != fasthttp.StatusAccepted {
		t.Fatalf("submit status = %d, body %s", submit.Response.StatusCode(), submit.Response.Body())
	}
	var accepted asyncTranscriptionJob
	if err := json.Unmarshal(submit.Response.Body(), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("submit response carries no job id: %v: %s", err, submit.Response.Body())
	}

	retrieve := h.getJob(schemas.TranscriptionRequest)
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fasthttp.MethodGet)
		ctx.Request.SetRequestURI("/v1/async/audio/transcriptions/" + accepted.ID)
		ctx.SetUserValue("job_id", accepted.ID)
		retrieve(ctx)
		var job asyncTranscriptionJob
		if err := json.Unmarshal(ctx.Response.Body(), &job); err != nil {
			t.Fatalf("retrieve response is not JSON: %v: %s", err, ctx.Response.Body())
		}
		switch job.Status {
		case string(schemas.AsyncJobStatusPending), string(schemas.AsyncJobStatusProcessing):
			if ctx.Response.StatusCode() != fasthttp.StatusAccepted {
				t.Fatalf("a job still %s must answer 202, got %d", job.Status, ctx.Response.StatusCode())
			}
		default:
			if ctx.Response.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("a finished job must answer 200, got %d: %s", ctx.Response.StatusCode(), ctx.Response.Body())
			}
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %s after 30s", accepted.ID, job.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// POST /v1/async/audio/transcriptions with model deepgram/nova-3 and a url in place of the
// file: the job is accepted, the background run makes exactly one call to Deepgram with the
// JSON url body and the options in the query, and the retrieved job carries the transcript
// and Deepgram's billable duration, duration × channels (contract C3, C4, C5). The async
// route shares prepareTranscriptionRequest with the sync one; this pins the submission and
// the retrieval around it.
func TestAsyncTranscriptionRouteRunsURLOnlyDeepgramJob(t *testing.T) {
	const audioURL = "https://example.com/audio/call.wav"
	upstream := &deepgramUpstream{status: http.StatusOK, body: deepgramTwoChannelBody}
	h := newDeepgramAsyncTestHandler(t, upstream)

	job := submitAndAwaitTranscription(t, h, multipartTranscriptionCtxWithoutFile(t, [][2]string{
		{"model", "deepgram/nova-3"},
		{"url", audioURL},
		{"multichannel", "true"},
		{"keyterm", "Bifrost gateway"},
		{"keyterm", "Tokun"},
	}))

	if job.Status != string(schemas.AsyncJobStatusCompleted) || job.Error != nil {
		t.Fatalf("job = %+v, want completed without an error", job)
	}
	if job.Result.Text != "thank you for calling\nhello" || job.Result.Usage.Type != "duration" || job.Result.Usage.Seconds != 61 {
		t.Fatalf("result = %+v, want both channels and usage {type: duration, seconds: 61}", job.Result)
	}
	got := upstream.only(t)
	if got.path != "/v1/listen" || got.contentType != "application/json" || string(got.body) != `{"url":"`+audioURL+`"}` {
		t.Fatalf("upstream request = %s %q (%s), want the JSON url body on /v1/listen", got.path, got.body, got.contentType)
	}
	if got.query != "keyterm=Bifrost+gateway&keyterm=Tokun&model=nova-3&multichannel=true" {
		t.Fatalf("upstream query = %q, want every option under its own name and no url", got.query)
	}
}

// A file submitted on the async route reaches Deepgram as the audio bytes, and a Deepgram
// error ends the job as failed with Deepgram's status and code, after one upstream call.
// The error a job stores is the provider's error before the transport resolves
// extra_fields.error_type, for every provider; that field is not asserted here.
func TestAsyncTranscriptionRouteKeepsDeepgramErrorOnTheJob(t *testing.T) {
	upstream := &deepgramUpstream{status: http.StatusPaymentRequired, body: `{"err_code":"ASR_PAYMENT_REQUIRED","err_msg":"Project does not have enough credits for an ASR request and does not have an overage agreement.","request_id":"r-402"}`}
	h := newDeepgramAsyncTestHandler(t, upstream)

	job := submitAndAwaitTranscription(t, h, multipartTranscriptionCtx(t, [][2]string{{"model", "deepgram/nova-3"}}))

	if job.Status != string(schemas.AsyncJobStatusFailed) || job.Error == nil {
		t.Fatalf("job = %+v, want failed with the vendor error", job)
	}
	if job.StatusCode != http.StatusPaymentRequired || job.Error.StatusCode != http.StatusPaymentRequired || job.Error.Error.Code != "ASR_PAYMENT_REQUIRED" || job.Error.ExtraFields.Provider != "deepgram" {
		t.Fatalf("job = %+v / %+v, want Deepgram's 402 and code kept on the job", job, job.Error)
	}
	got := upstream.only(t)
	if got.contentType != "audio/mpeg" || string(got.body) != "ID3fake-mp3" {
		t.Fatalf("upstream request = %q (%s), want the audio bytes", got.body, got.contentType)
	}
}
