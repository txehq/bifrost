package jobaccounting

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/modelcatalog"
)

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
