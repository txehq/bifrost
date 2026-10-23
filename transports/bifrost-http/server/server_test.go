package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"errors"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/plugins/governance"
	"github.com/capsohq/bifrost/transports/bifrost-http/handlers"
	"github.com/capsohq/bifrost/transports/bifrost-http/lib"
	"github.com/stretchr/testify/require"
)

func startCustomAudioGateway(t *testing.T, dir string) (*BifrostHTTPServer, string, func()) {
	t.Helper()
	log := bifrost.NewNoOpLogger()
	SetLogger(log)
	lib.SetLogger(log)
	handlers.SetLogger(log)
	s := NewBifrostHTTPServer("txe5087", embed.FS{})
	s.AppDir = dir
	require.NoError(t, s.Bootstrap(context.Background()))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- s.Server.Serve(listener) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			s.shutdownGracefully()
			require.NoError(t, <-done)
			require.ErrorIs(t, listener.Close(), net.ErrClosed)
		})
	}
	t.Cleanup(stop)
	return s, "http://" + listener.Addr().String(), stop
}

type customAudioUpstream struct {
	t                  *testing.T
	server             *httptest.Server
	audio              []byte
	speechCalls        atomic.Int32
	transcriptionCalls atomic.Int32
	selectedKey        atomic.Value
	cancelled          chan struct{}
}

func (u *customAudioUpstream) serveChat(w http.ResponseWriter, r *http.Request) {
	t := u.t
	if r.URL.Path == "/openai/v1/models" || r.URL.Path == "/anthropic/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"data":[{"id":"owned-chat","object":"model","type":"model","display_name":"Owned chat"}]}`)
		require.NoError(t, err)
		return
	}
	if r.URL.Path == "/openai/v1/chat/completions" {
		require.Equal(t, "Bearer owned-openai-value", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":"chatcmpl-owned","object":"chat.completion","model":"owned-chat","choices":[{"index":0,"message":{"role":"assistant","content":"Owned chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		require.NoError(t, err)
		return
	}
	if r.URL.Path == "/anthropic/v1/messages" {
		require.Equal(t, "owned-anthropic-value", r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":"msg-owned","type":"message","role":"assistant","model":"owned-chat","content":[{"type":"text","text":"Owned chat"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		require.NoError(t, err)
		return
	}
	t.Fatalf("unexpected chat upstream path: %s", r.URL.Path)
}

// ServeHTTP 校验并记录任务自有 upstream 的请求。
func (u *customAudioUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := u.t
	if strings.HasPrefix(r.URL.Path, "/openai/") || strings.HasPrefix(r.URL.Path, "/anthropic/") {
		u.serveChat(w, r)
		return
	}
	if r.URL.Path == "/reseller/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `[{"model_id":"eleven_turbo_v2_5","name":"Owned speech","can_do_text_to_speech":true}]`)
		require.NoError(t, err)
		return
	}
	u.selectedKey.Store(r.Header.Get("xi-api-key"))
	if r.URL.Path == "/reseller/v1/speech-to-text" {
		u.serveTranscription(w, r)
		return
	}
	u.serveSpeech(w, r)
}

func (u *customAudioUpstream) serveTranscription(w http.ResponseWriter, r *http.Request) {
	t := u.t
	u.transcriptionCalls.Add(1)
	require.NoError(t, r.ParseMultipartForm(1024*1024))
	require.Equal(t, "scribe_v2", r.FormValue("model_id"))
	file, _, err := r.FormFile("file")
	require.NoError(t, err)
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, []byte("owned-upload"), data)
	w.Header().Set("Content-Type", "application/json")
	_, err = io.WriteString(w, `{"text":"Owned transcription","language_code":"eng","language_probability":1,"words":[],"audio_duration_secs":1}`)
	require.NoError(t, err)
}

func (u *customAudioUpstream) serveSpeech(w http.ResponseWriter, r *http.Request) {
	t, audio := u.t, u.audio
	require.True(t, r.URL.Path == "/reseller/v1/text-to-speech/owned-voice" || r.URL.Path == "/reseller/v1/text-to-speech/owned-voice/stream", r.URL.Path)
	u.speechCalls.Add(1)
	var request struct {
		Text  string `json:"text"`
		Model string `json:"model_id"`
	}
	require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
	require.Equal(t, "eleven_turbo_v2_5", request.Model)
	if request.Text == "fail" {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := io.WriteString(w, `{"detail":{"status":"owned_failure","message":"controlled failure"}}`)
		require.NoError(t, err)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("request-id", "owned-request")
	w.Header().Set("x-character-count", "5")
	if request.Text == "empty" {
		w.Header().Set("Content-Length", "0")
		return
	}
	if request.Text == "truncated" {
		w.Header().Set("Content-Length", "100")
		_, err := io.WriteString(w, "partial-audio")
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		return
	}
	if request.Text == "short" {
		w.Header().Set("Content-Length", "28")
		_, err := io.WriteString(w, "TXE-5087-owned-audio-payload")
		require.NoError(t, err)
		return
	}
	if request.Text == "cancel" {
		_, err := w.Write(audio)
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			u.cancelled <- struct{}{}
		case <-time.After(5 * time.Second):
			t.Error("upstream stream was not cancelled")
		}
		return
	}
	if request.Text == "slow" {
		_, err := w.Write(audio)
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		time.Sleep(1200 * time.Millisecond)
	}
	_, err := w.Write(audio)
	require.NoError(t, err)
}

const customAudioProvider = "tkp-native-elevenlabs"
const customAudioPolicy = `{"base_provider_type":"elevenlabs","allowed_requests":{"list_models":true,"speech":true,"speech_stream":true,"transcription":true}}`

type customAudioGatewayFixture struct {
	t            *testing.T
	upstream     *customAudioUpstream
	gateway      *BifrostHTTPServer
	base         string
	dir          string
	stop         func()
	client       *http.Client
	selectedName string
	network      string
	settings     string
	keys         []byte
}

func newCustomAudioGatewayFixture(t *testing.T) *customAudioGatewayFixture {
	t.Helper()
	upstream := &customAudioUpstream{t: t, audio: bytes.Repeat([]byte("owned-audio"), 512), cancelled: make(chan struct{}, 1)}
	upstream.server = httptest.NewServer(upstream)
	t.Cleanup(upstream.server.Close)
	dir := t.TempDir()
	for name, data := range map[string]string{"pricing.json": "{}", "params.json": "{}", "mcp.json": `{"servers":[]}`} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(data), 0600))
	}
	config := fmt.Sprintf(`{"client":{"enable_logging":false,"disable_content_logging":true},"providers":{},"framework":{"pricing":{"pricing_url":%q,"model_parameters_url":%q,"mcp_library_url":%q,"mcp_library_sync_interval":0,"live_models_sync_interval":0}}}`, "file://"+filepath.Join(dir, "pricing.json"), "file://"+filepath.Join(dir, "params.json"), "file://"+filepath.Join(dir, "mcp.json"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0600))
	gateway, base, stop := startCustomAudioGateway(t, dir)
	client := &http.Client{Timeout: 10 * time.Second}
	network := fmt.Sprintf(`{"base_url":%q,"allow_private_network":true,"default_request_timeout_in_seconds":1,"max_retries":0,"retry_backoff_initial":500,"retry_backoff_max":5000}`, upstream.server.URL+"/reseller")
	return &customAudioGatewayFixture{t: t, upstream: upstream, gateway: gateway, base: base, dir: dir, stop: stop,
		client: client, selectedName: "owned-selected", network: network,
		settings: `"network_config":` + network + `,"concurrency_and_buffer_size":{"concurrency":1000,"buffer_size":5000},"custom_provider_config":` + customAudioPolicy}
}

func (f *customAudioGatewayFixture) call(method, path, body, contentType string, want int) ([]byte, http.Header) {
	t := f.t
	t.Helper()
	req, err := http.NewRequest(method, f.base+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-bf-api-key", f.selectedName)
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, resp.StatusCode, string(data))
	return data, resp.Header
}

func (f *customAudioGatewayFixture) registerAndUpdate() {
	t, call, provider := f.t, f.call, customAudioProvider
	t.Helper()
	settings := f.settings
	call("POST", "/api/providers", `{"provider":"`+provider+`",`+settings+`}`, "application/json", 200)
	for _, key := range []string{"owned-other", "owned-selected"} {
		call("POST", "/api/providers/"+provider+"/keys", fmt.Sprintf(`{"name":%q,"value":%q,"models":["*"],"weight":1}`, key, key+"-value"), "application/json", 200)
	}
	f.assertContract(true)
	f.keys, _ = call("GET", "/api/providers/"+provider+"/keys", "", "application/json", 200)
	call("PUT", "/api/providers/"+provider, `{`+settings+`}`, "application/json", 200)
	f.assertKeys()
	call("PUT", "/api/providers/"+provider, `{"custom_provider_config":`+customAudioPolicy+`}`, "application/json", 400)
	f.assertKeys()
	f.assertContract(true)
}

func (f *customAudioGatewayFixture) assertKeys() {
	f.t.Helper()
	updated, _ := f.call("GET", "/api/providers/"+customAudioProvider+"/keys", "", "application/json", 200)
	require.JSONEq(f.t, string(f.keys), string(updated))
}

func (f *customAudioGatewayFixture) assertContract(speechAllowed bool) {
	t, call, provider := f.t, f.call, customAudioProvider
	t.Helper()
	data, _ := call("GET", "/api/providers/"+provider, "", "application/json", 200)
	var cfg handlers.ProviderResponse
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.Equal(t, schemas.ModelProvider(provider), cfg.Name)
	require.Equal(t, schemas.SupportedBaseProviders, cfg.SupportedBaseProviders)
	require.Equal(t, "active", cfg.ProviderStatus)
	require.Equal(t, f.upstream.server.URL+"/reseller", cfg.NetworkConfig.BaseURL)
	require.Contains(t, string(data), `"max_retries":0`)
	require.Equal(t, 500*time.Millisecond, cfg.NetworkConfig.RetryBackoffInitial)
	require.Equal(t, 5*time.Second, cfg.NetworkConfig.RetryBackoffMax)
	require.Equal(t, schemas.ConcurrencyAndBufferSize{Concurrency: 1000, BufferSize: 5000}, cfg.ConcurrencyAndBufferSize)
	require.Equal(t, schemas.Elevenlabs, cfg.CustomProviderConfig.BaseProviderType)
	require.True(t, cfg.CustomProviderConfig.AllowedRequests.ListModels)
	require.Equal(t, speechAllowed, cfg.CustomProviderConfig.AllowedRequests.Speech)
	require.True(t, cfg.CustomProviderConfig.AllowedRequests.SpeechStream)
	require.True(t, cfg.CustomProviderConfig.AllowedRequests.Transcription)
}

func (f *customAudioGatewayFixture) setSpeechAllowed(allowed bool) {
	f.t.Helper()
	policy := fmt.Sprintf(`{"base_provider_type":"elevenlabs","allowed_requests":{"list_models":true,"speech":%t,"speech_stream":true,"transcription":true}}`, allowed)
	settings := `"network_config":` + f.network + `,"concurrency_and_buffer_size":{"concurrency":1000,"buffer_size":5000},"custom_provider_config":` + policy
	f.call("PUT", "/api/providers/"+customAudioProvider, `{`+settings+`}`, "application/json", 200)
}

func (f *customAudioGatewayFixture) speech(text, stream string, want int) ([]byte, http.Header) {
	f.t.Helper()
	return f.call("POST", "/v1/audio/speech", fmt.Sprintf(`{"model":"%s/eleven_turbo_v2_5","voice":"owned-voice","input":%q%s}`, customAudioProvider, text, stream), "application/json", want)
}

func (f *customAudioGatewayFixture) assertSpeech() {
	t, speech, provider := f.t, f.speech, customAudioProvider
	t.Helper()
	audio := f.upstream.audio
	data, headers := speech("hello", "", 200)
	require.Equal(t, audio, data)
	require.Equal(t, provider, headers.Get(lib.HeaderBifrostProvider))
	require.Equal(t, "owned-selected-value", f.upstream.selectedKey.Load())
	data, headers = speech("slow", `,"stream_format":"sse"`, 200)
	require.Contains(t, string(data), "speech.audio.done")
	require.Equal(t, append(append([]byte{}, audio...), audio...), decodeCustomAudio(t, data))
	require.Equal(t, provider, headers.Get(lib.HeaderBifrostProvider))
	data, _ = speech("short", `,"stream_format":"sse"`, 200)
	require.Equal(t, []byte("TXE-5087-owned-audio-payload"), decodeCustomAudio(t, data))
	data, _ = speech("empty", `,"stream_format":"sse"`, 200)
	require.Empty(t, decodeCustomAudio(t, data))
	require.Contains(t, string(data), "speech.audio.done")
	data, _ = speech("truncated", `,"stream_format":"sse"`, 200)
	require.Equal(t, []byte("partial-audio"), decodeCustomAudio(t, data))
	require.Contains(t, string(data), `"error"`)
	require.NotContains(t, string(data), "speech.audio.done")
}

func decodeCustomAudio(t *testing.T, data []byte) []byte {
	t.Helper()
	var result []byte
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Audio []byte `json:"audio"`
		}
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		if event.Type == "speech.audio.delta" {
			result = append(result, event.Audio...)
		}
	}
	return result
}

func customAudioUpload(t *testing.T) (string, string) {
	t.Helper()
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	require.NoError(t, writer.WriteField("model", customAudioProvider+"/scribe_v2"))
	file, err := writer.CreateFormFile("file", "owned.wav")
	require.NoError(t, err)
	_, err = file.Write([]byte("owned-upload"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return upload.String(), writer.FormDataContentType()
}

func (f *customAudioGatewayFixture) assertTranscription() {
	t := f.t
	t.Helper()
	body, contentType := customAudioUpload(t)
	data, headers := f.call("POST", "/v1/audio/transcriptions", body, contentType, 200)
	require.Contains(t, string(data), "Owned transcription")
	require.Equal(t, customAudioProvider, headers.Get(lib.HeaderBifrostProvider))
	require.Equal(t, "owned-selected-value", f.upstream.selectedKey.Load())
	require.EqualValues(t, 1, f.upstream.transcriptionCalls.Load())
}

func (f *customAudioGatewayFixture) assertOneUpstreamAttempt() {
	f.t.Helper()
	before := f.upstream.speechCalls.Load()
	f.speech("fail", "", 503)
	require.Equal(f.t, before+1, f.upstream.speechCalls.Load())
}

func (f *customAudioGatewayFixture) assertCancellation() {
	t, provider := f.t, customAudioProvider
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", f.base+"/v1/audio/speech", strings.NewReader(fmt.Sprintf(`{"model":"%s/eleven_turbo_v2_5","voice":"owned-voice","input":"cancel","stream_format":"sse"}`, provider)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-bf-api-key", "owned-selected")
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	cancel()
	require.NoError(t, resp.Body.Close())
	select {
	case <-f.upstream.cancelled:
	case <-time.After(6 * time.Second):
		t.Fatal("gateway did not cancel upstream stream")
	}
}

func (f *customAudioGatewayFixture) assertPolicy() {
	t, call, speech, provider := f.t, f.call, f.speech, customAudioProvider
	t.Helper()
	body, contentType := customAudioUpload(t)
	for _, operation := range []string{"speech", "speech_stream", "transcription"} {
		policy := fmt.Sprintf(`{"base_provider_type":"elevenlabs","allowed_requests":{"list_models":true,"speech":%t,"speech_stream":%t,"transcription":%t}}`, operation != "speech", operation != "speech_stream", operation != "transcription")
		settings := `"network_config":` + f.network + `,"concurrency_and_buffer_size":{"concurrency":1000,"buffer_size":5000},"custom_provider_config":` + policy
		call("PUT", "/api/providers/"+provider, `{`+settings+`}`, "application/json", 200)
		before := f.upstream.speechCalls.Load() + f.upstream.transcriptionCalls.Load()
		switch operation {
		case "speech":
			speech("hello", "", 400)
		case "speech_stream":
			speech("hello", `,"stream_format":"sse"`, 400)
		case "transcription":
			call("POST", "/v1/audio/transcriptions", body, contentType, 400)
		}
		require.Equal(t, before, f.upstream.speechCalls.Load()+f.upstream.transcriptionCalls.Load(), operation)
	}
	call("POST", "/api/providers", `{"provider":"owned-unsupported","custom_provider_config":{"base_provider_type":"azure"}}`, "application/json", 400)
}

func (f *customAudioGatewayFixture) assertControlProviders() {
	t, call := f.t, f.call
	t.Helper()
	for _, channel := range []struct{ name, base, prefix, key string }{
		{"elevenlabs", "", "/reseller", "owned-builtin"},
		{"owned-openai-custom", "openai", "/openai", "owned-openai"},
		{"owned-anthropic-custom", "anthropic", "/anthropic", "owned-anthropic"},
	} {
		custom := ""
		if channel.base != "" {
			custom = fmt.Sprintf(`,"custom_provider_config":{"base_provider_type":%q}`, channel.base)
		}
		call("POST", "/api/providers", fmt.Sprintf(`{"provider":%q,"network_config":{"base_url":%q,"allow_private_network":true,"max_retries":0}%s}`, channel.name, f.upstream.server.URL+channel.prefix, custom), "application/json", 200)
		call("POST", "/api/providers/"+channel.name+"/keys", fmt.Sprintf(`{"name":%q,"value":%q,"models":["*"],"weight":1}`, channel.key, channel.key+"-value"), "application/json", 200)
		f.selectedName = channel.key
		var data []byte
		var headers http.Header
		if channel.base == "" {
			data, headers = call("POST", "/v1/audio/speech", `{"model":"elevenlabs/eleven_turbo_v2_5","voice":"owned-voice","input":"hello"}`, "application/json", 200)
			require.Equal(t, f.upstream.audio, data)
			require.Equal(t, "owned-builtin-value", f.upstream.selectedKey.Load())
			data, _ = call("POST", "/v1/audio/speech", `{"model":"elevenlabs/eleven_turbo_v2_5","voice":"owned-voice","input":"short","stream_format":"sse"}`, "application/json", 200)
			require.Equal(t, []byte("TXE-5087-owned-audio-payload"), decodeCustomAudio(t, data))
		} else {
			data, headers = call("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"%s/owned-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`, channel.name), "application/json", 200)
			require.Contains(t, string(data), "Owned chat")
		}
		require.Equal(t, channel.name, headers.Get(lib.HeaderBifrostProvider))
	}
}

func (f *customAudioGatewayFixture) assertKeyStatusSnapshot() {
	t, gateway := f.t, f.gateway
	provider := schemas.ModelProvider(customAudioProvider)
	t.Helper()
	snapshot, err := gateway.Config.GetProviderConfigRaw(provider)
	require.NoError(t, err)
	oldKey := snapshot.Keys[0]
	gateway.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: provider,
		KeyID:    oldKey.ID,
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "owned status update"}},
	}})
	require.Equal(t, oldKey.Status, snapshot.Keys[0].Status)
	require.Equal(t, oldKey.Description, snapshot.Keys[0].Description)
	updated, err := gateway.Config.GetProviderConfigRaw(provider)
	require.NoError(t, err)
	require.Equal(t, schemas.KeyStatusListModelsFailed, updated.Keys[0].Status)
	require.Equal(t, "owned status update", updated.Keys[0].Description)
}

func TestCustomElevenlabsGateway(t *testing.T) {
	f := newCustomAudioGatewayFixture(t)
	f.registerAndUpdate()
	f.assertSpeech()
	f.assertTranscription()
	f.assertOneUpstreamAttempt()
	f.setSpeechAllowed(false)
	f.assertContract(false)
	f.assertKeys()
	f.stop()
	f.gateway, f.base, f.stop = startCustomAudioGateway(t, f.dir)
	f.assertContract(false)
	f.assertKeys()
	f.setSpeechAllowed(true)
	f.assertContract(true)
	f.assertOneUpstreamAttempt()
	f.assertCancellation()
	f.assertPolicy()
	f.assertControlProviders()
	f.assertKeyStatusSnapshot()
	f.stop()
}

// reloadVirtualKeyConfigStore provides the persistence calls used by ReloadVirtualKey.
type reloadVirtualKeyConfigStore struct {
	configstore.ConfigStore
	vk *configstoreTables.TableVirtualKey
}

// RetryOnNotFound executes the supplied lookup once for deterministic tests.
func (s *reloadVirtualKeyConfigStore) RetryOnNotFound(ctx context.Context, fn func(context.Context) (any, error), _ int, _ time.Duration) (any, error) {
	return fn(ctx)
}

// GetVirtualKey returns the configured persisted virtual key.
func (s *reloadVirtualKeyConfigStore) GetVirtualKey(context.Context, string) (*configstoreTables.TableVirtualKey, error) {
	return s.vk, nil
}

// GetModelConfigsByScopeAndScopeIDs returns no scoped model configs.
func (s *reloadVirtualKeyConfigStore) GetModelConfigsByScopeAndScopeIDs(context.Context, string, []string) ([]configstoreTables.TableModelConfig, error) {
	return nil, nil
}

// reloadVirtualKeyGovernanceStore counts snapshot calls while delegating all
// mutation and direct lookup behavior to a real in-memory store.
type reloadVirtualKeyGovernanceStore struct {
	governance.GovernanceStore
	governanceDataCalls int
}

// GetGovernanceData records any accidental full-snapshot lookup.
func (s *reloadVirtualKeyGovernanceStore) GetGovernanceData(context.Context) *governance.GovernanceData {
	s.governanceDataCalls++
	return nil
}

// reloadVirtualKeyPlugin exposes the test governance store.
type reloadVirtualKeyPlugin struct {
	governance.BaseGovernancePlugin
	store governance.GovernanceStore
}

// GetName returns the standard governance plugin name.
func (p *reloadVirtualKeyPlugin) GetName() string { return governance.PluginName }

// GetGovernanceStore returns the test governance store.
func (p *reloadVirtualKeyPlugin) GetGovernanceStore() governance.GovernanceStore { return p.store }

// reloadVirtualKeyToolManager provides an empty MCP tool set.
type reloadVirtualKeyToolManager struct{}

// GetAvailableMCPTools returns no tools.
func (reloadVirtualKeyToolManager) GetAvailableMCPTools(context.Context) []schemas.ChatTool {
	return nil
}

// ExecuteChatMCPTool is unused by these tests.
func (reloadVirtualKeyToolManager) ExecuteChatMCPTool(context.Context, *schemas.ChatAssistantMessageToolCall) (*schemas.ChatMessage, *schemas.BifrostError) {
	return nil, nil
}

// ExecuteResponsesMCPTool is unused by these tests.
func (reloadVirtualKeyToolManager) ExecuteResponsesMCPTool(context.Context, *schemas.ResponsesToolMessage) (*schemas.ResponsesMessage, *schemas.BifrostError) {
	return nil, nil
}

// TestReloadVirtualKeyAvoidsGovernanceSnapshot pins that reloading one key reads and replaces it
// directly rather than through a full governance snapshot, which is what keeps a key edit cheap on
// a deployment holding many keys.
func TestReloadVirtualKeyAvoidsGovernanceSnapshot(t *testing.T) {
	handlers.SetLogger(noopTestLogger{})
	ctx := context.Background()
	baseStore, err := governance.NewLocalGovernanceStore(ctx, governance.NewMockLogger(), nil, &configstore.GovernanceConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("create governance store: %v", err)
	}
	baseStore.CreateVirtualKeyInMemory(ctx, &configstoreTables.TableVirtualKey{ID: "vk-id", Name: "old", Value: *schemas.NewSecretVar("sk-bf-old")})
	store := &reloadVirtualKeyGovernanceStore{GovernanceStore: baseStore}
	plugin := &reloadVirtualKeyPlugin{store: store}
	persistedVK := &configstoreTables.TableVirtualKey{ID: "vk-id", Name: "new", Value: *schemas.NewSecretVar("sk-bf-new")}
	config := &lib.Config{
		ConfigStore:  &reloadVirtualKeyConfigStore{vk: persistedVK},
		ClientConfig: &configstore.ClientConfig{},
	}
	plugins := []schemas.BasePlugin{plugin}
	config.BasePlugins.Store(&plugins)
	mcpHandler, err := handlers.NewMCPServerHandler(ctx, config, reloadVirtualKeyToolManager{}, nil, nil, store)
	if err != nil {
		t.Fatalf("create MCP handler: %v", err)
	}
	server := &BifrostHTTPServer{Ctx: schemas.NewBifrostContext(ctx, schemas.NoDeadline), Config: config, MCPServerHandler: mcpHandler}

	if _, err := server.ReloadVirtualKey(ctx, persistedVK.ID); err != nil {
		t.Fatalf("ReloadVirtualKey returned unexpected error: %v", err)
	}
	if store.governanceDataCalls != 0 {
		t.Fatalf("GetGovernanceData called %d times, want 0", store.governanceDataCalls)
	}
	reloaded, ok := baseStore.GetVirtualKeyByID(ctx, "vk-id")
	if !ok || reloaded.Value.GetValue() != "sk-bf-new" {
		t.Fatalf("store does not answer with the reloaded key: %+v", reloaded)
	}
}

// TestConfig is a sample config struct for testing
type TestConfig struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Count   int    `json:"count"`
}

type updateStatusOnlyConfigStore struct {
	configstore.ConfigStore
	calls []schemas.KeyStatus
}

type noopTestLogger struct{}

func (noopTestLogger) Debug(string, ...any)                   {}
func (noopTestLogger) Info(string, ...any)                    {}
func (noopTestLogger) Warn(string, ...any)                    {}
func (noopTestLogger) Error(string, ...any)                   {}
func (noopTestLogger) Fatal(string, ...any)                   {}
func (noopTestLogger) SetLevel(schemas.LogLevel)              {}
func (noopTestLogger) SetOutputType(schemas.LoggerOutputType) {}
func (noopTestLogger) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func (s *updateStatusOnlyConfigStore) UpdateStatus(ctx context.Context, provider schemas.ModelProvider, keyID string, status, errorMsg string) error {
	s.calls = append(s.calls, schemas.KeyStatus{
		Provider: provider,
		KeyID:    keyID,
		Status:   schemas.KeyStatusType(status),
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: errorMsg}},
	})
	return nil
}

func TestUpdateKeyStatus_KeylessProviderUpdatesProviderStatusInMemory(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"mock-openai": {
					CustomProviderConfig: &schemas.CustomProviderConfig{IsKeyLess: true},
					Status:               "unknown",
				},
			},
		},
	}

	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "mock-openai",
		KeyID:    "",
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "preview missing model"}},
	}})

	provider := server.Config.Providers["mock-openai"]
	if provider.Status != string(schemas.KeyStatusListModelsFailed) {
		t.Fatalf("expected provider status %q, got %q", schemas.KeyStatusListModelsFailed, provider.Status)
	}
	if provider.Description != "preview missing model" {
		t.Fatalf("expected provider description to be updated, got %q", provider.Description)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected one status update call, got %d", len(store.calls))
	}
	if store.calls[0].Provider != "mock-openai" || store.calls[0].KeyID != "" {
		t.Fatalf("expected provider-level status update, got provider=%q keyID=%q", store.calls[0].Provider, store.calls[0].KeyID)
	}
}

func TestUpdateKeyStatus_EmptyKeyIDDoesNotOverwriteKeyedProviderStatus(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {
					Keys:   []schemas.Key{{ID: "key-1"}},
					Status: "healthy",
				},
			},
		},
	}

	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "",
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "malformed status"}},
	}})

	provider := server.Config.Providers["openai"]
	if provider.Status != "healthy" {
		t.Fatalf("expected keyed provider status to remain unchanged, got %q", provider.Status)
	}
	if provider.Description != "" {
		t.Fatalf("expected keyed provider description to remain unchanged, got %q", provider.Description)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected one status update call, got %d", len(store.calls))
	}
	if store.calls[0].Provider != "openai" || store.calls[0].KeyID != "" {
		t.Fatalf("expected DB status update to retain empty key ID, got provider=%q keyID=%q", store.calls[0].Provider, store.calls[0].KeyID)
	}
}

// TestUpdateKeyStatus_SkipsWriteWhenUnchanged pins the write-gating that makes
// the background refresher affordable: ConfigStore.UpdateStatus is an
// unconditional SQL UPDATE, so a status that has not moved must not reach it.
// Without this gate a 30-key deployment refreshing on an interval writes 30
// no-op UPDATEs per pass, per node, forever.
func TestUpdateKeyStatus_SkipsWriteWhenUnchanged(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {
					Keys: []schemas.Key{{
						ID:          "key-1",
						Status:      schemas.KeyStatusListModelsFailed,
						Description: "upstream 503",
					}},
				},
			},
		},
	}

	unchanged := []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "key-1",
		Status:   schemas.KeyStatusListModelsFailed,
		Error:    &schemas.BifrostError{Error: &schemas.ErrorField{Message: "upstream 503"}},
	}}

	server.updateKeyStatus(context.Background(), unchanged)
	server.updateKeyStatus(context.Background(), unchanged)

	if len(store.calls) != 0 {
		t.Fatalf("expected no status update calls for an unchanged status, got %d", len(store.calls))
	}

	// A genuine change must still get through, otherwise the gate would hide
	// a key recovering or breaking.
	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "key-1",
		Status:   schemas.KeyStatusSuccess,
	}})

	if len(store.calls) != 1 {
		t.Fatalf("expected one status update call after the status changed, got %d", len(store.calls))
	}
	if got := server.Config.Providers["openai"].Keys[0].Status; got != schemas.KeyStatusSuccess {
		t.Fatalf("expected in-memory status to become success, got %q", got)
	}
	if got := server.Config.Providers["openai"].Keys[0].Description; got != "" {
		t.Fatalf("expected in-memory description to be cleared, got %q", got)
	}
}

// TestUpdateKeyStatus_WritesWhenKeyMissingFromMemory guards the fallback: with
// nothing to compare against, the gate must let the write through rather than
// silently swallowing it.
func TestUpdateKeyStatus_WritesWhenKeyMissingFromMemory(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	store := &updateStatusOnlyConfigStore{}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ConfigStore: store,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"openai": {Keys: []schemas.Key{{ID: "key-1"}}},
			},
		},
	}

	server.updateKeyStatus(context.Background(), []schemas.KeyStatus{{
		Provider: "openai",
		KeyID:    "key-unknown",
		Status:   schemas.KeyStatusSuccess,
	}})

	if len(store.calls) != 1 {
		t.Fatalf("expected the write to proceed for an unknown key, got %d calls", len(store.calls))
	}
}

// TestRefreshAllLiveModels_NilClientIsNoop covers the boot-order guard: the
// refresher can fire before or after the client exists, and dereferencing a
// nil client would panic the background goroutine.
func TestRefreshAllLiveModels_NilClientIsNoop(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {Keys: []schemas.Key{{ID: "key-1"}}},
			},
		},
	}

	// Reaching the end without panicking is the assertion: s.Client is nil, so
	// any scheduled fetch would dereference it.
	server.RefreshAllLiveModels(context.Background())

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected no live models to be written, got %v", models)
	}
}

// TestRefreshAllLiveModels_SkipsProvidersWithNoEnabledKeys cross-checks that
// the per-provider skip rules still apply when the fan-out is driven by the
// background pass rather than the bootstrap loop.
func TestRefreshAllLiveModels_SkipsProvidersWithNoEnabledKeys(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"all-disabled": {Keys: []schemas.Key{
					{ID: "key-1", Enabled: schemas.Ptr(false)},
					{ID: "key-2", Enabled: schemas.Ptr(false)},
				}},
				"no-keys": {},
			},
		},
	}
	// Non-nil sentinel would be required for a fetch to be attempted; leaving
	// Client nil means any scheduled fetch panics and fails this test.
	server.RefreshAllLiveModels(context.Background())
}

// TestStartLiveModelRefresher_ZeroIntervalDisabled pins the opt-out: a
// non-positive interval must spawn nothing and still return a usable stop func.
func TestStartLiveModelRefresher_ZeroIntervalDisabled(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{}

	for _, interval := range []time.Duration{0, -time.Second} {
		before := runtime.NumGoroutine()
		stop := server.startLiveModelRefresher(context.Background(), interval)
		if stop == nil {
			t.Fatalf("interval %v: expected a non-nil stop func", interval)
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Fatalf("interval %v: expected no goroutine to be spawned, went from %d to %d", interval, before, after)
		}
		stop() // must not panic
	}
}

// TestStartLiveModelRefresher_StopsOnContextCancel makes sure the refresher
// does not outlive the server: a leaked ticker would keep calling every
// upstream forever after shutdown.
func TestStartLiveModelRefresher_StopsOnContextCancel(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{}

	baseline := runtime.NumGoroutine()
	stop := server.startLiveModelRefresher(context.Background(), 10*time.Millisecond)

	// Let it turn over a few cycles so we are stopping a running loop, not one
	// that never started.
	time.Sleep(50 * time.Millisecond)

	stop()

	// Poll rather than sleeping a fixed amount: goroutine teardown is not
	// synchronous with cancel, and a generous window beats a flaky one.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("refresher goroutine still running 2s after stop: baseline %d, now %d", baseline, runtime.NumGoroutine())
}

// TestJitteredInterval_StaysWithinBounds pins the jitter window. Pods in a
// deployment boot together, so this is what keeps their refresh cycles from
// converging into a synchronized burst against every upstream.
func TestJitteredInterval_StaysWithinBounds(t *testing.T) {
	base := time.Hour
	lower := time.Duration(float64(base) * (1 - liveRefreshJitterFraction))
	upper := time.Duration(float64(base) * (1 + liveRefreshJitterFraction))

	seen := make(map[time.Duration]struct{})
	for range 1000 {
		got := jitteredInterval(base)
		if got < lower || got > upper {
			t.Fatalf("jitteredInterval(%v) = %v, want within [%v, %v]", base, got, lower, upper)
		}
		seen[got] = struct{}{}
	}
	// A constant would satisfy the bounds check above but defeat the purpose.
	if len(seen) < 2 {
		t.Fatalf("expected jittered intervals to vary, got %d distinct value(s)", len(seen))
	}
}

// TestBeginKeyRefresh_CoordinatesAtKeyGranularity covers the per-key in-flight
// guard: duplicate key refreshes collapse, while distinct keys and providers
// remain independent.
func TestBeginKeyRefresh_CoordinatesAtKeyGranularity(t *testing.T) {
	server := &BifrostHTTPServer{}

	release, ok := server.beginKeyRefresh("openai", "key-1")
	if !ok {
		t.Fatal("expected the first claim to succeed")
	}
	if _, ok := server.beginKeyRefresh("openai", "key-1"); ok {
		t.Fatal("expected a concurrent claim for the same key to be rejected")
	}

	otherKeyRelease, ok := server.beginKeyRefresh("openai", "key-2")
	if !ok {
		t.Fatal("expected a different key for the same provider to succeed")
	}
	otherKeyRelease()

	otherProviderRelease, ok := server.beginKeyRefresh("anthropic", "key-1")
	if !ok {
		t.Fatal("expected a claim for a different provider to succeed")
	}
	otherProviderRelease()

	release()
	release, ok = server.beginKeyRefresh("openai", "key-1")
	if !ok {
		t.Fatal("expected the key to be claimable again after release")
	}
	release()
}

func TestBeginAllKeysRefresh_IsExclusiveWithinProvider(t *testing.T) {
	server := &BifrostHTTPServer{}

	keyRelease, ok := server.beginKeyRefresh("openai", "key-1")
	if !ok {
		t.Fatal("expected the key claim to succeed")
	}
	if _, ok := server.beginAllKeysRefresh("openai"); ok {
		t.Fatal("expected all-keys refresh to conflict with an active key refresh")
	}
	keyRelease()

	allRelease, ok := server.beginAllKeysRefresh("openai")
	if !ok {
		t.Fatal("expected all-keys refresh to succeed after the key refresh completed")
	}
	if _, ok := server.beginAllKeysRefresh("openai"); ok {
		t.Fatal("expected a second all-keys refresh to be rejected")
	}
	if _, ok := server.beginKeyRefresh("openai", "key-2"); ok {
		t.Fatal("expected key refresh to conflict with an active all-keys refresh")
	}

	otherProviderRelease, ok := server.beginKeyRefresh("anthropic", "key-1")
	if !ok {
		t.Fatal("expected another provider to remain independent")
	}
	otherProviderRelease()

	allRelease()
	allRelease, ok = server.beginAllKeysRefresh("openai")
	if !ok {
		t.Fatal("expected all-keys refresh to be claimable again after release")
	}
	allRelease()
}

func TestKeyEnabled(t *testing.T) {
	tests := []struct {
		name string
		key  schemas.Key
		want bool
	}{
		{"nil Enabled defaults to enabled", schemas.Key{}, true},
		{"explicit true is enabled", schemas.Key{Enabled: schemas.Ptr(true)}, true},
		{"explicit false is disabled", schemas.Key{Enabled: schemas.Ptr(false)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keyEnabled(tt.key); got != tt.want {
				t.Fatalf("keyEnabled(%+v) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestRefreshLiveModelsForProvider_AllKeysDisabled cross-checks the "0
// enabled keys" case for issue #5037: discovery must skip scheduling any
// fetch (and therefore never touch s.Client, left nil here) rather than
// attempting doomed-to-fail per-key calls.
func TestRefreshLiveModelsForProvider_AllKeysDisabled(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {},
			},
		},
	}

	keys := []schemas.Key{
		{ID: "key-1", Enabled: schemas.Ptr(false)},
		{ID: "key-2", Enabled: schemas.Ptr(false)},
	}

	// Must return without panicking: s.Client is nil, so any scheduled fetch
	// would panic on the first ListModelsRequest call. Reaching the end of
	// this test proves no fetch was scheduled for either disabled key.
	server.RefreshLiveModelsForProvider(context.Background(), "custom-provider", keys)

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected no live models to be written when all keys are disabled, got %v", models)
	}
}

// TestOnKeyAdded_DisabledKeySkipsFetch cross-checks that adding a key that
// is already disabled never schedules a discovery fetch for it.
func TestOnKeyAdded_DisabledKeySkipsFetch(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	disabledKey := schemas.Key{ID: "key-1", Enabled: schemas.Ptr(false)}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: modelcatalog.NewTestCatalog(nil),
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {Keys: []schemas.Key{disabledKey}},
			},
		},
	}

	// s.Client is left nil: if the disabled-key guard regresses, the fetch
	// goroutine would dereference it and panic, failing this test.
	if err := server.OnKeyAdded(context.Background(), "custom-provider", disabledKey); err != nil {
		t.Fatalf("OnKeyAdded returned unexpected error: %v", err)
	}

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected no live models to be written for a disabled key, got %v", models)
	}
}

// TestOnKeyUpdated_DisabledKeySkipsFetchButInvalidatesCache cross-checks
// that toggling a key to disabled still evicts its stale cached models
// (InvalidateLive) even though the refetch is correctly skipped.
func TestOnKeyUpdated_DisabledKeySkipsFetchButInvalidatesCache(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"custom-provider/some-model"})

	disabledKey := schemas.Key{ID: "key-1", Enabled: schemas.Ptr(false)}
	server := &BifrostHTTPServer{
		Config: &lib.Config{
			ModelCatalog: catalog,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				"custom-provider": {Keys: []schemas.Key{disabledKey}},
			},
		},
	}

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) == 0 {
		t.Fatalf("expected seeded live model before update, got none")
	}

	// s.Client is left nil: if the disabled-key guard regresses, the fetch
	// goroutine would dereference it and panic, failing this test.
	if err := server.OnKeyUpdated(context.Background(), "custom-provider", disabledKey); err != nil {
		t.Fatalf("OnKeyUpdated returned unexpected error: %v", err)
	}

	if models := server.Config.ModelCatalog.GetModelsForProvider("custom-provider"); len(models) != 0 {
		t.Fatalf("expected InvalidateLive to drop the disabled key's cached models, got %v", models)
	}
}

// reloadProviderConfigStore is the minimum ConfigStore ReloadProvider needs:
// it only ever calls GetProvider before touching the model catalog.
type reloadProviderConfigStore struct {
	configstore.ConfigStore
	provider *configstoreTables.TableProvider
}

func (s *reloadProviderConfigStore) GetProvider(context.Context, schemas.ModelProvider) (*configstoreTables.TableProvider, error) {
	return s.provider, nil
}

// newReloadProviderServer builds a BifrostHTTPServer wired for ReloadProvider
// with model discovery guaranteed to write nothing.
//
// Disabling list_models via AllowedRequests makes FetchAndStoreLiveForKey
// return before it issues a request, which is the same observable outcome as
// the transient failures this suite is about (upstream 5xx, rate limit,
// timeout, refused connection): that function writes to the catalog only on a
// successful response, so every failure mode is indistinguishable from here.
// It also lets Client stay a bare non-nil pointer, so any regression that does
// attempt a fetch panics instead of silently reaching the network.
// The custom provider config is set on both the stored row and the in-memory
// config on purpose: ReloadProvider reads the keyless flag off the row it
// loads from the config store, while the isKeylessProvider helper that
// RefreshLiveModelsForProvider and the OnKey* handlers use reads the in-memory
// copy. A fixture that populated only one would exercise a state the running
// server never has.
func newReloadProviderServer(catalog *modelcatalog.ModelCatalog, provider schemas.ModelProvider, keys []schemas.Key, keyless bool) *BifrostHTTPServer {
	customProviderConfig := &schemas.CustomProviderConfig{
		IsKeyLess:       keyless,
		AllowedRequests: &schemas.AllowedRequests{ListModels: false},
	}
	return &BifrostHTTPServer{
		Ctx:    schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		Client: &bifrost.Bifrost{},
		Config: &lib.Config{
			ConfigStore: &reloadProviderConfigStore{provider: &configstoreTables.TableProvider{
				Name:                 string(provider),
				CustomProviderConfig: customProviderConfig,
			}},
			ModelCatalog: catalog,
			Providers: map[schemas.ModelProvider]configstore.ProviderConfig{
				provider: {
					Keys:                 keys,
					CustomProviderConfig: customProviderConfig,
				},
			},
		},
	}
}

// TestReloadProvider_FailedRefetchKeepsPreviousCatalog is the regression guard
// for issue #5554: ReloadProvider used to wipe the provider's whole live
// catalog before re-fetching, so a re-fetch that wrote nothing left the
// provider with no live models at all. Editing an unrelated provider field
// while the upstream was briefly unhealthy therefore emptied GET /v1/models
// until a later edit or a restart repaired it.
func TestReloadProvider_FailedRefetchKeepsPreviousCatalog(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"some-model"})
	catalog.UpsertLive("custom-provider", "key-1", true, []string{"some-model", "unlisted-model"})

	server := newReloadProviderServer(catalog, "custom-provider", []schemas.Key{{ID: "key-1"}}, false)

	if _, err := server.ReloadProvider(context.Background(), "custom-provider"); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model"}) {
		t.Errorf("filtered catalog after failed refetch = %v, want [some-model] retained", got)
	}
	if got := catalog.GetUnfilteredModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model", "unlisted-model"}) {
		t.Errorf("unfiltered catalog after failed refetch = %v, want [some-model unlisted-model] retained", got)
	}
}

// TestReloadProvider_PrunesRemovedAndDisabledKeys pins the half of the old
// invalidate that must survive the fix. Retaining is not "skip the cleanup":
// keys that left the provider's set, and keys that are still configured but
// disabled, must still lose their cached entries. Disabled keys matter because
// RefreshLiveModelsForProvider never re-fetches for them, so a retained entry
// would be served indefinitely for a key core rejects at routing time.
func TestReloadProvider_PrunesRemovedAndDisabledKeys(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-enabled", false, []string{"kept-model"})
	catalog.UpsertLive("custom-provider", "key-disabled", false, []string{"disabled-model"})
	catalog.UpsertLive("custom-provider", "key-removed", false, []string{"removed-model"})
	catalog.UpsertLive("custom-provider", "key-removed", true, []string{"removed-model"})

	keys := []schemas.Key{
		{ID: "key-enabled"},
		{ID: "key-disabled", Enabled: schemas.Ptr(false)},
	}
	server := newReloadProviderServer(catalog, "custom-provider", keys, false)

	if _, err := server.ReloadProvider(context.Background(), "custom-provider"); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("custom-provider"); !slices.Equal(got, []string{"kept-model"}) {
		t.Errorf("filtered catalog = %v, want only [kept-model] (disabled and removed keys pruned)", got)
	}
	if got := catalog.GetUnfilteredModelsForProvider("custom-provider"); len(got) != 0 {
		t.Errorf("unfiltered catalog = %v, want empty (only the removed key had an unfiltered entry)", got)
	}
}

// TestReloadProvider_KeylessProviderRetainsSentinelEntry covers keyless
// providers (Vertex workload identity, Bedrock IAM, custom keyless), whose
// entries are cached under the "" key ID rather than a real key ID. The retain
// set has to name that sentinel explicitly or the fix would still empty every
// keyless provider's catalog on a failed refetch.
func TestReloadProvider_KeylessProviderRetainsSentinelEntry(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("keyless-provider", "", false, []string{"keyless-model"})

	server := newReloadProviderServer(catalog, "keyless-provider", nil, true)

	if _, err := server.ReloadProvider(context.Background(), "keyless-provider"); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("keyless-provider"); !slices.Equal(got, []string{"keyless-model"}) {
		t.Errorf("keyless catalog after failed refetch = %v, want [keyless-model] retained", got)
	}
}

// TestReloadProvider_NoKeysDropsEverything pins the branch that must keep
// wiping: a keyed provider with no keys left can serve nothing, so no cached
// entry is still meaningful and none will ever be refreshed.
func TestReloadProvider_NoKeysDropsEverything(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"orphaned-model"})

	server := newReloadProviderServer(catalog, "custom-provider", nil, false)

	if _, err := server.ReloadProvider(context.Background(), "custom-provider"); err != nil {
		t.Fatalf("ReloadProvider returned unexpected error: %v", err)
	}

	if got := catalog.GetModelsForProvider("custom-provider"); len(got) != 0 {
		t.Errorf("catalog for keyless-less provider = %v, want empty", got)
	}
}

// TestReloadProvider_ConcurrentReloadsAndReadsAreRaceFree covers the layer
// above the store. ReloadProvider's catalog work is three separate store calls
// (SetKeyConfigForProvider, RetainLiveKeys, then the per-key upserts inside
// RefreshLiveModelsForProvider), so it is atomic per call but not as a
// sequence. Two provider edits landing at once, or an edit landing while
// GET /v1/models is being served, must stay race-free and leave the catalog
// coherent even though the interleaving of those three steps is unspecified.
//
// The keep map is built fresh inside each ReloadProvider call and never
// escapes it, which is what keeps concurrent callers from sharing it.
func TestReloadProvider_ConcurrentReloadsAndReadsAreRaceFree(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()

	catalog := modelcatalog.NewTestCatalog(nil)
	catalog.UpsertLive("custom-provider", "key-1", false, []string{"some-model"})
	catalog.UpsertLive("custom-provider", "key-1", true, []string{"some-model", "unlisted-model"})

	server := newReloadProviderServer(catalog, "custom-provider", []schemas.Key{{ID: "key-1"}}, false)

	const reloaders = 4
	const readers = 4
	const iterations = 50

	var wg sync.WaitGroup
	for range reloaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				if _, err := server.ReloadProvider(context.Background(), "custom-provider"); err != nil {
					t.Errorf("concurrent ReloadProvider returned error: %v", err)
					return
				}
			}
		}()
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				_ = catalog.GetModelsForProvider("custom-provider")
				_ = catalog.GetUnfilteredModelsForProvider("custom-provider")
			}
		}()
	}
	wg.Wait()

	// Retention is not just race-free but stable under repetition: no
	// interleaving of concurrent reloads may drop an entry whose key is
	// present and enabled in every one of them.
	if got := catalog.GetModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model"}) {
		t.Errorf("filtered catalog after concurrent reloads = %v, want [some-model] retained", got)
	}
	if got := catalog.GetUnfilteredModelsForProvider("custom-provider"); !slices.Equal(got, []string{"some-model", "unlisted-model"}) {
		t.Errorf("unfiltered catalog after concurrent reloads = %v, want both entries retained", got)
	}
}

func TestMarshalPluginConfig_WithPointerType(t *testing.T) {
	// Test case 1: source is already *T
	expected := &TestConfig{
		Name:    "test-plugin",
		Enabled: true,
		Count:   42,
	}

	result, err := MarshalPluginConfig[TestConfig](expected)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result != expected {
		t.Errorf("Expected same pointer, got different pointer")
	}

	if result.Name != expected.Name {
		t.Errorf("Expected Name=%s, got %s", expected.Name, result.Name)
	}
	if result.Enabled != expected.Enabled {
		t.Errorf("Expected Enabled=%v, got %v", expected.Enabled, result.Enabled)
	}
	if result.Count != expected.Count {
		t.Errorf("Expected Count=%d, got %d", expected.Count, result.Count)
	}
}

func TestMarshalPluginConfig_WithMap(t *testing.T) {
	// Test case 2: source is map[string]any
	configMap := map[string]any{
		"name":    "test-plugin",
		"enabled": true,
		"count":   42,
	}

	result, err := MarshalPluginConfig[TestConfig](configMap)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if result.Name != "test-plugin" {
		t.Errorf("Expected Name=test-plugin, got %s", result.Name)
	}
	if result.Enabled != true {
		t.Errorf("Expected Enabled=true, got %v", result.Enabled)
	}
	if result.Count != 42 {
		t.Errorf("Expected Count=42, got %d", result.Count)
	}
}

func TestMarshalPluginConfig_WithString(t *testing.T) {
	// Test case 3: source is string (JSON)
	configStr := `{"name":"test-plugin","enabled":true,"count":42}`

	result, err := MarshalPluginConfig[TestConfig](configStr)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if result.Name != "test-plugin" {
		t.Errorf("Expected Name=test-plugin, got %s", result.Name)
	}
	if result.Enabled != true {
		t.Errorf("Expected Enabled=true, got %v", result.Enabled)
	}
	if result.Count != 42 {
		t.Errorf("Expected Count=42, got %d", result.Count)
	}
}

func TestMarshalPluginConfig_WithInvalidType(t *testing.T) {
	// Test case 4: source is invalid type (should return error)
	invalidSource := 12345

	result, err := MarshalPluginConfig[TestConfig](invalidSource)
	if err == nil {
		t.Fatal("Expected error for invalid type, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for invalid type, got %v", result)
	}

	expectedError := "invalid config type"
	if err.Error() != expectedError {
		t.Errorf("Expected error message '%s', got '%s'", expectedError, err.Error())
	}
}

func TestMarshalPluginConfig_WithInvalidJSONString(t *testing.T) {
	// Test case 5: source is string but invalid JSON
	invalidJSON := `{"name":"test-plugin","enabled":true,count:42}` // missing quotes around count

	result, err := MarshalPluginConfig[TestConfig](invalidJSON)
	if err == nil {
		t.Fatal("Expected error for invalid JSON, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for invalid JSON, got %v", result)
	}
}

func TestMarshalPluginConfig_WithInvalidMapData(t *testing.T) {
	// Test case 6: source is map but contains invalid data types
	configMap := map[string]any{
		"name":    "test-plugin",
		"enabled": "not-a-boolean", // wrong type
		"count":   42,
	}

	result, err := MarshalPluginConfig[TestConfig](configMap)
	if err == nil {
		t.Fatal("Expected error for invalid map data, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for invalid map data, got %v", result)
	}
}

func TestMarshalPluginConfig_WithEmptyMap(t *testing.T) {
	// Test case 7: source is empty map (should work, return zero values)
	configMap := map[string]any{}

	result, err := MarshalPluginConfig[TestConfig](configMap)
	if err != nil {
		t.Fatalf("Expected no error for empty map, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	// All fields should have zero values
	if result.Name != "" {
		t.Errorf("Expected empty Name, got %s", result.Name)
	}
	if result.Enabled != false {
		t.Errorf("Expected Enabled=false, got %v", result.Enabled)
	}
	if result.Count != 0 {
		t.Errorf("Expected Count=0, got %d", result.Count)
	}
}

func TestMarshalPluginConfig_WithEmptyString(t *testing.T) {
	// Test case 8: source is empty string (should fail as invalid JSON)
	configStr := ""

	result, err := MarshalPluginConfig[TestConfig](configStr)
	if err == nil {
		t.Fatal("Expected error for empty string, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for empty string, got %v", result)
	}
}

func TestMarshalPluginConfig_WithNil(t *testing.T) {
	// Test case 9: source is nil (should return error as invalid type)
	result, err := MarshalPluginConfig[TestConfig](nil)
	if err == nil {
		t.Fatal("Expected error for nil source, got nil")
	}

	if result != nil {
		t.Errorf("Expected nil result for nil source, got %v", result)
	}
}

// Benchmark tests
func BenchmarkMarshalPluginConfig_WithPointerType(b *testing.B) {
	config := &TestConfig{
		Name:    "test-plugin",
		Enabled: true,
		Count:   42,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MarshalPluginConfig[TestConfig](config)
	}
}

func BenchmarkMarshalPluginConfig_WithMap(b *testing.B) {
	configMap := map[string]any{
		"name":    "test-plugin",
		"enabled": true,
		"count":   42,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MarshalPluginConfig[TestConfig](configMap)
	}
}

func BenchmarkMarshalPluginConfig_WithString(b *testing.B) {
	configStr := `{"name":"test-plugin","enabled":true,"count":42}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = MarshalPluginConfig[TestConfig](configStr)
	}
}

// Complex config for additional testing
type ComplexConfig struct {
	Settings map[string]string `json:"settings"`
	Tags     []string          `json:"tags"`
	Metadata map[string]any    `json:"metadata"`
	Nested   *TestConfig       `json:"nested"`
}

func TestMarshalPluginConfig_WithComplexType(t *testing.T) {
	// Test with a more complex nested structure
	configMap := map[string]any{
		"settings": map[string]any{
			"key1": "value1",
			"key2": "value2",
		},
		"tags": []any{"tag1", "tag2", "tag3"},
		"metadata": map[string]any{
			"version": "1.0.0",
			"author":  "test",
		},
		"nested": map[string]any{
			"name":    "nested-config",
			"enabled": true,
			"count":   10,
		},
	}

	result, err := MarshalPluginConfig[ComplexConfig](configMap)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if len(result.Settings) != 2 {
		t.Errorf("Expected 2 settings, got %d", len(result.Settings))
	}
	if len(result.Tags) != 3 {
		t.Errorf("Expected 3 tags, got %d", len(result.Tags))
	}
	if result.Nested == nil {
		t.Fatal("Expected non-nil nested config")
	}
	if result.Nested.Name != "nested-config" {
		t.Errorf("Expected nested name=nested-config, got %s", result.Nested.Name)
	}
}

type fakePromptsPlugin struct {
	name    string
	reloads int
	err     error
}

func (p *fakePromptsPlugin) GetName() string { return p.name }
func (p *fakePromptsPlugin) Cleanup() error  { return nil }
func (p *fakePromptsPlugin) Reload(context.Context) error {
	p.reloads++
	return p.err
}

// nonReloadingPlugin is registered under the prompts plugin name but cannot reload.
type nonReloadingPlugin struct{ name string }

func (p *nonReloadingPlugin) GetName() string { return p.name }
func (p *nonReloadingPlugin) Cleanup() error  { return nil }

func promptCacheServer(plugins ...schemas.BasePlugin) *BifrostHTTPServer {
	SetLogger(bifrost.NewNoOpLogger())
	config := &lib.Config{}
	config.BasePlugins.Store(&plugins)
	return &BifrostHTTPServer{
		Config: config,
		Ctx:    schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
	}
}

func TestReloadPromptCacheReloadsThePlugin(t *testing.T) {
	plugin := &fakePromptsPlugin{name: "prompts"}
	server := promptCacheServer(plugin)

	require.NoError(t, server.ReloadPromptCache(context.Background()))
	require.Equal(t, 1, plugin.reloads)
}

// A prompt write must not fail just because the prompts plugin is disabled.
func TestReloadPromptCacheWithoutPluginIsNoOp(t *testing.T) {
	server := promptCacheServer()

	require.NoError(t, server.ReloadPromptCache(context.Background()))
}

func TestReloadPromptCacheUsesPluginNameFromContext(t *testing.T) {
	plugin := &fakePromptsPlugin{name: "enterprise-prompts"}
	server := promptCacheServer(plugin)
	ctx := context.WithValue(context.Background(), schemas.BifrostContextKeyPromptsPluginName, "enterprise-prompts")
	server.Ctx = schemas.NewBifrostContext(ctx, schemas.NoDeadline)

	require.NoError(t, server.ReloadPromptCache(context.Background()))
	require.Equal(t, 1, plugin.reloads, "enterprise registers the plugin under its own name")
}

func TestReloadPromptCacheReturnsPluginError(t *testing.T) {
	plugin := &fakePromptsPlugin{name: "prompts", err: errors.New("database unavailable")}
	server := promptCacheServer(plugin)

	require.ErrorContains(t, server.ReloadPromptCache(context.Background()), "database unavailable")
}

// A plugin that cannot reload must not fail the write, but it does warn rather than pass silently.
func TestReloadPromptCacheWithIncompatiblePluginIsNoOp(t *testing.T) {
	server := promptCacheServer(&nonReloadingPlugin{name: "prompts"})

	require.NoError(t, server.ReloadPromptCache(context.Background()))
}

// GetConfiguredProviders hands back the live provider map uncopied, which is safe for the callers
// that index it once. The name listing cannot be written that way: provider add, delete and status
// updates all write to that map in place under the write lock, so a caller ranging it after the read
// lock is released hits a concurrent map iteration and write. That is fatal rather than merely
// stale — it takes the process down — so the slice is built under the lock instead. Run with -race.
func TestGetConfiguredProviderNamesIsSafeAgainstConcurrentProviderEdits(t *testing.T) {
	config := &lib.Config{
		Providers: map[schemas.ModelProvider]configstore.ProviderConfig{schemas.OpenAI: {}},
	}
	store := &GovernanceInMemoryStore{Config: config}

	churn := []schemas.ModelProvider{"churn-a", "churn-b", "churn-c"}
	done := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			// The same in-place mutation AddProvider and RemoveProvider make.
			for _, provider := range churn {
				config.Mu.Lock()
				config.Providers[provider] = configstore.ProviderConfig{}
				delete(config.Providers, provider)
				config.Mu.Unlock()
			}
		}
	}()

	for range 500 {
		if names := store.GetConfiguredProviderNames(); len(names) == 0 {
			t.Fatal("expected the configured providers to be listed")
		}
	}

	close(done)
	writers.Wait()
}
