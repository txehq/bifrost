package vertex

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
	"golang.org/x/oauth2"
)

const (
	vertexOmniInteractionID = "v1_ChdPU0F4YWFtNkFwS2kxZThQZ05lbXdROBIXT1NBeGFhbTZBcEtpMWU4UGdOZW13UTg"
	vertexOmniName          = "projects/proj/locations/global/interactions/" + vertexOmniInteractionID
)

// vertexOmniUpstream is a fake Vertex API. Every host the provider dials lands on it, and it
// records the host, the call and the body of each request.
type vertexOmniUpstream struct {
	mu     sync.Mutex
	calls  []string
	bodies []string
	auth   []string
}

func (u *vertexOmniUpstream) snapshot() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.calls)
}

func newVertexOmniProvider(t *testing.T, region string, handler func(w http.ResponseWriter, r *http.Request)) (*VertexProvider, schemas.Key, *vertexOmniUpstream) {
	t.Helper()
	recorded := &vertexOmniUpstream{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded.mu.Lock()
		recorded.calls = append(recorded.calls, r.Method+" "+r.Host+r.URL.RequestURI())
		recorded.bodies = append(recorded.bodies, string(body))
		recorded.auth = append(recorded.auth, r.Header.Get("Authorization"))
		recorded.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)

	config := &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{DefaultRequestTimeoutInSeconds: 10}}
	config.CheckAndSetDefaults()
	provider, err := NewVertexProvider(config, nil)
	if err != nil {
		t.Fatalf("NewVertexProvider: %v", err)
	}
	for _, client := range []*fasthttp.Client{provider.client, provider.streamingClient} {
		client.Dial = func(string) (net.Conn, error) { return net.Dial("tcp", upstream.Listener.Addr().String()) }
		client.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}
	key := schemas.Key{VertexKeyConfig: &schemas.VertexKeyConfig{
		ProjectID:       *schemas.NewSecretVar("proj"),
		Region:          *schemas.NewSecretVar(region),
		AuthCredentials: *schemas.NewSecretVar("test-credentials"),
	}}
	provider.tokenSources.Store(getClientKey(vertexCredentialIdentity(key)), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}))
	return provider, key, recorded
}

func vertexOmniContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

func vertexOmniRequest(model string, params *schemas.VideoGenerationParameters) *schemas.BifrostVideoGenerationRequest {
	return &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Vertex,
		Model:    model,
		Input:    &schemas.VideoGenerationInput{Prompt: "a marble on a track"},
		Params:   params,
	}
}

func TestVertexInteractionURL(t *testing.T) {
	for name, want := range map[string]string{
		"projects/proj/locations/global/interactions/v1_abc": "https://aiplatform.googleapis.com/v1/projects/proj/locations/global/interactions/v1_abc",
		"projects/proj/locations/us/interactions/v1_abc":     "https://aiplatform.us.rep.googleapis.com/v1/projects/proj/locations/us/interactions/v1_abc",
		"projects/proj/locations/eu/interactions/v1_abc":     "https://aiplatform.eu.rep.googleapis.com/v1/projects/proj/locations/eu/interactions/v1_abc",
	} {
		got, isInteraction, bifrostErr := vertexInteractionURL(name)
		if bifrostErr != nil || !isInteraction || got != want {
			t.Errorf("vertexInteractionURL(%q) = %q, %v, %v; want %q", name, got, isInteraction, bifrostErr, want)
		}
	}
	// A Veo operation name, or anything else, is not an interaction: it keeps its own path.
	for _, name := range []string{
		"projects/proj/locations/us-central1/publishers/google/models/veo-3/operations/op-1",
		"interactions/v1_abc",
		"projects/proj/locations/global/interactions",
		"",
	} {
		if got, isInteraction, bifrostErr := vertexInteractionURL(name); isInteraction || bifrostErr != nil {
			t.Errorf("vertexInteractionURL(%q) = %q, %v, %v; want not an interaction", name, got, isInteraction, bifrostErr)
		}
	}
	// An interaction name that could reach another host or path is refused.
	for _, name := range []string{
		"projects/proj/locations/attacker.example#/interactions/v1_abc",
		"projects/proj/locations/us-central1/interactions/v1_abc",
		"projects/../locations/global/interactions/v1_abc",
		"projects/proj/locations/global/interactions/..",
		"projects/proj/locations/global/interactions/a?x=1",
		"projects/proj/regions/global/interactions/v1_abc",
	} {
		if got, isInteraction, bifrostErr := vertexInteractionURL(name); !isInteraction || bifrostErr == nil {
			t.Errorf("vertexInteractionURL(%q) = %q, %v, %v; want an error", name, got, isInteraction, bifrostErr)
		}
	}
}

// An Omni create on Vertex goes to the documented interactions path of the key's location with
// the Interactions body, and the task id is the interaction's resource name.
func TestVertexOmniVideoGeneration(t *testing.T) {
	provider, key, upstream := newVertexOmniProvider(t, "global", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + vertexOmniInteractionID + `","object":"interaction","status":"in_progress","model":"gemini-omni-1.1-flash-preview","created":"2026-11-10T12:22:47Z","updated":"2026-11-10T12:22:47Z"}`))
	})

	resp, bifrostErr := provider.VideoGeneration(vertexOmniContext(), key, vertexOmniRequest("gemini-omni-1.1-flash-preview", &schemas.VideoGenerationParameters{
		Seconds:     schemas.Ptr("8"),
		AspectRatio: "16:9",
		Resolution:  "720p",
	}))
	if bifrostErr != nil {
		t.Fatalf("VideoGeneration: %s", bifrostErr.GetErrorString())
	}
	if want := []string{"POST aiplatform.googleapis.com/v1/projects/proj/locations/global/interactions"}; !slices.Equal(upstream.snapshot(), want) {
		t.Fatalf("calls %v, want %v", upstream.snapshot(), want)
	}
	// No delivery: Vertex documents no uri artifact, so the vendor's default (inline) stays.
	const wantBody = `{"model":"gemini-omni-1.1-flash-preview","input":[{"type":"text","text":"a marble on a track"}],"response_format":{"type":"video","aspect_ratio":"16:9","duration":"8s","resolution":"720p"},"background":true}`
	if upstream.bodies[0] != wantBody {
		t.Errorf("body\n got %s\nwant %s", upstream.bodies[0], wantBody)
	}
	if upstream.auth[0] != "Bearer test-token" {
		t.Errorf("Authorization = %q", upstream.auth[0])
	}
	if want := providerUtils.AddVideoIDProviderSuffix(vertexOmniName, schemas.Vertex); resp.ID != want {
		t.Errorf("task id %q, want %q", resp.ID, want)
	}
	if resp.Status != schemas.VideoStatusInProgress || resp.ProviderStatus != "in_progress" {
		t.Errorf("status %q / %q", resp.Status, resp.ProviderStatus)
	}
}

// What Vertex cannot deliver is refused before any call: the webhook fields (webhook_config is
// not documented for Vertex), a field the Interactions API has no field for, and a region the
// Interactions API does not serve. A model that is neither Omni nor Veo is still rejected.
func TestVertexOmniVideoGeneration_Refusals(t *testing.T) {
	provider, key, upstream := newVertexOmniProvider(t, "global", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	for param, params := range map[string]*schemas.VideoGenerationParameters{
		"callback_url":        {TaskControlParameters: schemas.TaskControlParameters{CallbackURL: schemas.Ptr("https://hooks.tokun.sh/v/task-1")}},
		"client_reference_id": {TaskControlParameters: schemas.TaskControlParameters{ClientReferenceID: schemas.Ptr("task-1")}},
		"size":                {Size: "1280x720"},
		"audio":               {Audio: schemas.Ptr(true)},
	} {
		_, bifrostErr := provider.VideoGeneration(vertexOmniContext(), key, vertexOmniRequest("gemini-omni-1.1-flash-preview", params))
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest ||
			bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeCallerUnsupportedParameter || bifrostErr.Error.Param != param {
			t.Errorf("%s: want 400 caller_unsupported_parameter, got %+v", param, bifrostErr)
		}
	}
	if _, bifrostErr := provider.VideoGeneration(vertexOmniContext(), key, vertexOmniRequest("gemini-3.8-flash", nil)); bifrostErr == nil {
		t.Error("a model that is neither Omni nor Veo was accepted for video generation")
	}

	regional, regionalKey, regionalUpstream := newVertexOmniProvider(t, "us-central1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	_, bifrostErr := regional.VideoGeneration(vertexOmniContext(), regionalKey, vertexOmniRequest("gemini-omni-1.1-flash-preview", nil))
	if bifrostErr == nil || !strings.Contains(bifrostErr.Error.Message, "global, us, eu") {
		t.Errorf("a single region was accepted for the Interactions API: %+v", bifrostErr)
	}

	if calls := append(upstream.snapshot(), regionalUpstream.snapshot()...); len(calls) != 0 {
		t.Errorf("a refused request made upstream calls: %v", calls)
	}
}

// The task id decides the API and the host: an interaction is queried with GET on its own
// location whatever region the selected key has, a Veo operation with fetchPredictOperation.
func TestVertexVideoRetrieve_TaskIDKindReachesItsOwnAPI(t *testing.T) {
	video := []byte("\x00\x00\x00 ftypisom-not-a-real-video")
	completed := `{"id":"` + vertexOmniInteractionID + `","status":"completed","model":"gemini-omni-1.1-flash-preview","created":"2026-11-10T12:22:47Z","updated":"2026-11-10T12:25:15Z",` +
		`"steps":[{"type":"thought","content":[{"type":"thought","text":"plan"}]},{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"` + base64.StdEncoding.EncodeToString(video) + `"}]}],` +
		`"usage":{"output_tokens_by_modality":[{"modality":"video","tokens":46336}]}}`
	provider, key, upstream := newVertexOmniProvider(t, "us-east5", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/interactions/") {
			_, _ = w.Write([]byte(completed))
			return
		}
		_, _ = w.Write([]byte(`{"name":"projects/proj/locations/us-central1/publishers/google/models/veo-3/operations/op-1","done":false}`))
	})

	omniID := providerUtils.AddVideoIDProviderSuffix("projects/proj/locations/eu/interactions/"+vertexOmniInteractionID, schemas.Vertex)
	resp, bifrostErr := provider.VideoRetrieve(vertexOmniContext(), key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Vertex, ID: omniID})
	if bifrostErr != nil {
		t.Fatalf("VideoRetrieve: %s", bifrostErr.GetErrorString())
	}
	if resp.ID != omniID || resp.Status != schemas.VideoStatusCompleted || resp.ProviderStatus != "completed" {
		t.Errorf("task %q %q %q", resp.ID, resp.Status, resp.ProviderStatus)
	}
	if resp.Usage == nil || resp.Usage.OutputTokens == nil || *resp.Usage.OutputTokens != 46336 {
		t.Errorf("usage %+v, want output_tokens 46336", resp.Usage)
	}
	if len(resp.Videos) != 1 || resp.Videos[0].Type != schemas.VideoOutputTypeContent || resp.Videos[0].Base64Data != nil || resp.Videos[0].URL != nil {
		t.Errorf("videos %+v, want one content entry", resp.Videos)
	}
	// Vertex keeps a stored interaction 7 days from creation.
	if resp.ExpiresAt == nil || *resp.ExpiresAt != resp.CreatedAt+7*24*3600 {
		t.Errorf("expires_at %v, created_at %d", resp.ExpiresAt, resp.CreatedAt)
	}

	download, bifrostErr := provider.VideoDownload(vertexOmniContext(), key, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Vertex, ID: omniID})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload: %s", bifrostErr.GetErrorString())
	}
	if string(download.Content) != string(video) || download.ContentType != "video/mp4" {
		t.Errorf("download %q %q", download.Content, download.ContentType)
	}
	// A request that asked for a stream gets the inline bytes as Body.
	streamed, bifrostErr := provider.VideoDownload(vertexOmniContext(), key, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Vertex, ID: omniID, Stream: true})
	if bifrostErr != nil {
		t.Fatalf("VideoDownload(stream): %s", bifrostErr.GetErrorString())
	}
	if streamed.Body == nil || streamed.Content != nil || streamed.ContentLength != int64(len(video)) {
		t.Fatalf("streamed download %+v", streamed)
	}
	if got, _ := io.ReadAll(streamed.Body); string(got) != string(video) {
		t.Errorf("streamed body %q", got)
	}

	veoID := providerUtils.AddVideoIDProviderSuffix("projects/proj/locations/us-central1/publishers/google/models/veo-3/operations/op-1", schemas.Vertex)
	if _, bifrostErr := provider.VideoRetrieve(vertexOmniContext(), key, &schemas.BifrostVideoRetrieveRequest{Provider: schemas.Vertex, ID: veoID}); bifrostErr != nil {
		t.Fatalf("VideoRetrieve(veo): %s", bifrostErr.GetErrorString())
	}

	interaction := "GET aiplatform.eu.rep.googleapis.com/v1/projects/proj/locations/eu/interactions/" + vertexOmniInteractionID
	want := []string{
		interaction,
		interaction,
		interaction,
		"POST us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/veo-3:fetchPredictOperation",
	}
	if got := upstream.snapshot(); !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
}

// A uri artifact has no documented form on Vertex: the credentials are not sent to it.
func TestVertexOmniVideoDownload_URIArtifactIsNotFetched(t *testing.T) {
	provider, key, upstream := newVertexOmniProvider(t, "global", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + vertexOmniInteractionID + `","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","uri":"https://attacker.example/video.mp4"}]}]}`))
	})
	omniID := providerUtils.AddVideoIDProviderSuffix(vertexOmniName, schemas.Vertex)
	if _, bifrostErr := provider.VideoDownload(vertexOmniContext(), key, &schemas.BifrostVideoDownloadRequest{Provider: schemas.Vertex, ID: omniID}); bifrostErr == nil {
		t.Fatal("a uri artifact was downloaded")
	}
	if got := upstream.snapshot(); len(got) != 1 {
		t.Errorf("calls %v, want only the interaction query", got)
	}
}

// Cancel queries once and calls the vendor cancel only for a running interaction; every other
// state, and a Veo operation, is refused with provider_cancel_unsupported.
func TestVertexOmniVideoCancel(t *testing.T) {
	omniID := providerUtils.AddVideoIDProviderSuffix(vertexOmniName, schemas.Vertex)
	base := "aiplatform.googleapis.com/v1/" + vertexOmniName

	for _, status := range []string{"in_progress", "completed", "failed", "cancelled", "requires_action", "incomplete", "budget_exceeded", "requires_continuation"} {
		provider, key, upstream := newVertexOmniProvider(t, "global", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(`{"id":"` + vertexOmniInteractionID + `","status":"cancelled","updated":"2026-11-10T12:23:10Z"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"` + vertexOmniInteractionID + `","status":"` + status + `"}`))
		})
		resp, bifrostErr := provider.VideoCancel(vertexOmniContext(), key, &schemas.BifrostVideoCancelRequest{Provider: schemas.Vertex, ID: omniID})
		calls := upstream.snapshot()
		if status == "in_progress" {
			if bifrostErr != nil {
				t.Fatalf("VideoCancel(in_progress): %s", bifrostErr.GetErrorString())
			}
			if want := []string{"GET " + base, "POST " + base + "/cancel"}; !slices.Equal(calls, want) {
				t.Errorf("calls %v, want %v", calls, want)
			}
			if resp.ID != omniID || resp.Status != schemas.VideoStatusCancelled || resp.ProviderStatus != "cancelled" {
				t.Errorf("task %q %q %q", resp.ID, resp.Status, resp.ProviderStatus)
			}
			continue
		}
		if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != http.StatusBadRequest ||
			bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
			t.Errorf("%s: want 400 provider_cancel_unsupported, got %+v", status, bifrostErr)
		}
		if want := []string{"GET " + base}; !slices.Equal(calls, want) {
			t.Errorf("%s: calls %v, want %v", status, calls, want)
		}
	}

	provider, key, upstream := newVertexOmniProvider(t, "global", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	veoID := providerUtils.AddVideoIDProviderSuffix("projects/proj/locations/us-central1/publishers/google/models/veo-3/operations/op-1", schemas.Vertex)
	_, bifrostErr := provider.VideoCancel(vertexOmniContext(), key, &schemas.BifrostVideoCancelRequest{Provider: schemas.Vertex, ID: veoID})
	if bifrostErr == nil || bifrostErr.ExtraFields.ErrorType != schemas.ErrorTypeProviderCancelUnsupported {
		t.Errorf("veo cancel: want provider_cancel_unsupported, got %+v", bifrostErr)
	}
	if calls := upstream.snapshot(); len(calls) != 0 {
		t.Errorf("a Veo cancel made upstream calls: %v", calls)
	}

	var canceller any = provider
	if _, ok := canceller.(schemas.VideoCanceller); !ok {
		t.Error("VertexProvider does not implement schemas.VideoCanceller")
	}
}
