package handlers

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	openaiProvider "github.com/capsohq/bifrost/core/providers/openai"
	"github.com/capsohq/bifrost/core/schemas"

	"context"

	bifrost "github.com/capsohq/bifrost/core"
	bfws "github.com/capsohq/bifrost/transports/v2/bifrost-http/websocket"
	"github.com/fasthttp/router"
	ws "github.com/fasthttp/websocket"
	"github.com/valyala/fasthttp"
)

// dialRealtimeTestConn returns the server side of a live websocket connection.
//
// The upgrade handler is held open until cleanup runs, so the connection stays
// valid for the duration of the test.
func dialRealtimeTestConn(t *testing.T) (*ws.Conn, *ws.Conn, func()) {
	t.Helper()

	upgrader := ws.FastHTTPUpgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(*fasthttp.RequestCtx) bool { return true },
	}

	serverConns := make(chan *ws.Conn, 1)
	release := make(chan struct{})
	released := make(chan struct{})

	r := router.New()
	r.GET("/realtime", func(ctx *fasthttp.RequestCtx) {
		_ = upgrader.Upgrade(ctx, func(conn *ws.Conn) {
			serverConns <- conn
			<-release
			close(released)
		})
	})

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fasthttp.Server{Handler: r.Handler}
	go func() { _ = srv.Serve(ln) }()

	client, _, err := ws.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/realtime", nil)
	if err != nil {
		_ = srv.Shutdown()
		t.Fatalf("dial: %v", err)
	}

	var serverConn *ws.Conn
	select {
	case serverConn = <-serverConns:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the server side of the connection")
	}

	return serverConn, client, func() {
		close(release)
		<-released
		_ = client.Close()
		_ = srv.Shutdown()
	}
}

func TestDiscoverRealtimeTranscriptionModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "nested transcription model",
			message: `{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":" openai/gpt-4o-transcribe "}}}}}`,
			want:    "openai/gpt-4o-transcribe",
		},
		{
			name:    "beta transcription_session.update, legacy shape",
			message: `{"type":"transcription_session.update","session":{"input_audio_transcription":{"model":"openai/whisper-1"}}}`,
			want:    "openai/whisper-1",
		},
		{
			name:    "session.update, legacy shape",
			message: `{"type":"session.update","session":{"input_audio_transcription":{"model":"gpt-4o-mini-transcribe"}}}`,
			want:    "gpt-4o-mini-transcribe",
		},
		{
			name:    "GA shape wins when both are present",
			message: `{"type":"transcription_session.update","session":{"audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}},"input_audio_transcription":{"model":"whisper-1"}}}`,
			want:    "gpt-4o-transcribe",
		},
		{name: "wrong event type", message: `{"type":"input_audio_buffer.append","session":{"audio":{"input":{"transcription":{"model":"openai/gpt-4o-transcribe"}}}}}`},
		{name: "top-level model", message: `{"type":"session.update","model":"openai/gpt-4o-transcribe"}`},
		{name: "invalid JSON", message: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := discoverRealtimeTranscriptionModel([]byte(tt.message)); got != tt.want {
				t.Fatalf("discoverRealtimeTranscriptionModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPinRealtimeTranscriptionModel(t *testing.T) {
	t.Parallel()

	provider := &openaiProvider.OpenAIProvider{}
	tests := []struct {
		name                 string
		transcriptionSession bool
		message              string
		wantModel            string
	}{
		{
			name:                 "transcription session uses pinned alias-resolved model",
			transcriptionSession: true,
			message:              `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"transcription":{"model":"openai/transcription-alias","language":"en"}}}}}`,
			wantModel:            "gpt-4o-transcribe",
		},
		{
			name:                 "normal realtime keeps input transcription model",
			transcriptionSession: false,
			message:              `{"type":"session.update","session":{"type":"realtime","model":"gpt-realtime","audio":{"input":{"transcription":{"model":"openai/whisper-1","language":"en"}}}}}`,
			wantModel:            "whisper-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event, err := schemas.ParseRealtimeEvent([]byte(tt.message))
			if err != nil {
				t.Fatalf("ParseRealtimeEvent() error = %v", err)
			}
			if err := pinRealtimeTranscriptionModel(event, "gpt-4o-transcribe", tt.transcriptionSession); err != nil {
				t.Fatalf("pinRealtimeTranscriptionModel() error = %v", err)
			}
			sanitizeRealtimeSessionEventForProvider(event)
			serialized, err := provider.ToProviderRealtimeEvent(event)
			if err != nil {
				t.Fatalf("ToProviderRealtimeEvent() error = %v", err)
			}

			var payload struct {
				Session struct {
					Audio struct {
						Input struct {
							Transcription struct {
								Model    string `json:"model"`
								Language string `json:"language"`
							} `json:"transcription"`
						} `json:"input"`
					} `json:"audio"`
				} `json:"session"`
			}
			if err := json.Unmarshal(serialized, &payload); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if payload.Session.Audio.Input.Transcription.Model != tt.wantModel {
				t.Fatalf("transcription model = %q, want %q", payload.Session.Audio.Input.Transcription.Model, tt.wantModel)
			}
			if payload.Session.Audio.Input.Transcription.Language != "en" {
				t.Fatalf("transcription language = %q, want en", payload.Session.Audio.Input.Transcription.Language)
			}
		})
	}
}

func TestSnapshotRealtimeMiddlewareValuesWithContext(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "transport-virtual-key-id")
	ctx.SetUserValue(schemas.BifrostContextKeyTraceID, "dead-upgrade-trace")

	bifrostCtx := schemas.NewBifrostContext(t.Context(), schemas.NoDeadline)
	bifrostCtx.SetValue(schemas.BifrostContextKeySelectedKeyID, "selected-key")

	values := snapshotRealtimeMiddlewareValuesWithContext(ctx, bifrostCtx)
	if got := values[schemas.BifrostContextKeyGovernanceVirtualKeyID]; got != "transport-virtual-key-id" {
		t.Fatalf("governance virtual key ID = %v, want transport-virtual-key-id", got)
	}
	if got := values[schemas.BifrostContextKeySelectedKeyID]; got != "selected-key" {
		t.Fatalf("selected key ID = %v, want selected-key", got)
	}
	if _, ok := values[schemas.BifrostContextKeyTraceID]; ok {
		t.Fatal("upgrade trace ID must not be inherited by realtime turns")
	}
}

func TestBufferRealtimeTranscriptionBootstrapPreservesFrames(t *testing.T) {
	serverConn, peerConn, cleanup := dialRealtimeTestConn(t)
	defer cleanup()

	client := newRealtimeClientConn(serverConn)
	frames := []realtimeWebSocketFrame{
		{messageType: ws.BinaryMessage, data: []byte{0, 1, 2, 3}},
		{messageType: ws.TextMessage, data: []byte(`{"type":"input_audio_buffer.append","audio":"AQID"}`)},
		{messageType: ws.TextMessage, data: []byte(`{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"openai/gpt-4o-transcribe"}}}}}`)},
	}

	writeErr := make(chan error, 1)
	go func() {
		for _, frame := range frames {
			if err := peerConn.WriteMessage(frame.messageType, frame.data); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	buffered, model, err := bufferRealtimeTranscriptionBootstrap(client)
	if err != nil {
		t.Fatalf("bufferRealtimeTranscriptionBootstrap() error = %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write bootstrap frames: %v", err)
	}
	if model != "openai/gpt-4o-transcribe" {
		t.Fatalf("model = %q, want %q", model, "openai/gpt-4o-transcribe")
	}
	if len(buffered) != len(frames) {
		t.Fatalf("buffered frame count = %d, want %d", len(buffered), len(frames))
	}
	for i := range frames {
		if buffered[i].messageType != frames[i].messageType || string(buffered[i].data) != string(frames[i].data) {
			t.Fatalf("buffered[%d] = (%d, %q), want (%d, %q)", i, buffered[i].messageType, buffered[i].data, frames[i].messageType, frames[i].data)
		}
	}
}

// TestRealtimeStopHeartbeatWaitsForPingGoroutine pins the invariant that makes
// the realtime upgrade handler safe to return from: once stopHeartbeat returns,
// no goroutine can still be writing to the client connection.
//
// fasthttp nils out the hijacked connection's net.Conn the moment the handler
// returns, so a ping still in flight at that point dereferences nil. There is no
// recover on the heartbeat goroutine, so that panic is fatal to the process.
func TestRealtimeStopHeartbeatWaitsForPingGoroutine(t *testing.T) {
	serverConn, _, cleanup := dialRealtimeTestConn(t)
	defer cleanup()

	client := newRealtimeClientConn(serverConn)
	client.pingInterval = time.Millisecond
	client.startHeartbeat()

	// Let the heartbeat tick a few times so it is genuinely running.
	time.Sleep(50 * time.Millisecond)

	client.stopHeartbeat()

	select {
	case <-client.heartbeatDone:
	default:
		t.Fatal("stopHeartbeat returned while the ping goroutine was still running")
	}
}

// TestRealtimeStopHeartbeatWithoutStart covers the early-return paths that fail
// before the heartbeat is ever started.
func TestRealtimeStopHeartbeatWithoutStart(t *testing.T) {
	serverConn, _, cleanup := dialRealtimeTestConn(t)
	defer cleanup()

	client := newRealtimeClientConn(serverConn)

	done := make(chan struct{})
	go func() {
		defer close(done)
		client.stopHeartbeat()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopHeartbeat blocked when the heartbeat had never been started")
	}
}

// TestRealtimeStopHeartbeatIsIdempotent guards the deferred-stop paths, which
// can run more than once as a session unwinds.
func TestRealtimeStopHeartbeatIsIdempotent(t *testing.T) {
	serverConn, _, cleanup := dialRealtimeTestConn(t)
	defer cleanup()

	client := newRealtimeClientConn(serverConn)
	client.pingInterval = time.Millisecond
	client.startHeartbeat()
	time.Sleep(20 * time.Millisecond)

	client.stopHeartbeat()
	client.stopHeartbeat()
}

// TestPinRealtimeTranscriptionModelCoversEveryShape: a transcription session is
// held to its admitted model whichever update event and transcription shape the
// client uses, including an update carrying both shapes, and a shape that does
// not parse is refused instead of forwarded.
func TestPinRealtimeTranscriptionModelCoversEveryShape(t *testing.T) {
	t.Parallel()

	const admitted = "gpt-4o-mini-transcribe"
	tests := []struct {
		name    string
		message string
		wantErr bool
		// wantModels maps each transcription shape the update names to the model it must carry.
		wantModels map[string]string
	}{
		{
			name:       "beta transcription_session.update, legacy shape",
			message:    `{"type":"transcription_session.update","session":{"input_audio_transcription":{"model":"gpt-4o-transcribe","language":"en"}}}`,
			wantModels: map[string]string{"input_audio_transcription": admitted},
		},
		{
			name:       "session.update, legacy shape",
			message:    `{"type":"session.update","session":{"input_audio_transcription":{"model":"whisper-1"}}}`,
			wantModels: map[string]string{"input_audio_transcription": admitted},
		},
		{
			name:       "transcription_session.update, GA shape",
			message:    `{"type":"transcription_session.update","session":{"audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}}}}`,
			wantModels: map[string]string{"audio": admitted},
		},
		{
			name:       "both shapes in one update",
			message:    `{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}},"input_audio_transcription":{"model":"whisper-1"}}}`,
			wantModels: map[string]string{"audio": admitted, "input_audio_transcription": admitted},
		},
		{
			name:    "malformed legacy shape is refused",
			message: `{"type":"transcription_session.update","session":{"input_audio_transcription":"whisper-1"}}`,
			wantErr: true,
		},
		{
			name:    "malformed GA shape is refused",
			message: `{"type":"session.update","session":{"audio":{"input":"gpt-4o-transcribe"}}}`,
			wantErr: true,
		},
		{
			name:    "session.model is refused",
			message: `{"type":"session.update","session":{"model":"gpt-realtime"}}`,
			wantErr: true,
		},
		{
			name:    "switching to a realtime session is refused",
			message: `{"type":"session.update","session":{"type":"realtime","audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}}}}`,
			wantErr: true,
		},
		{
			name:       "transcription session type is accepted",
			message:    `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}}}}`,
			wantModels: map[string]string{"audio": admitted},
		},
		{
			name:       "transcription disabled with null stays null",
			message:    `{"type":"session.update","session":{"input_audio_transcription":null}}`,
			wantModels: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event, err := schemas.ParseRealtimeEvent([]byte(tt.message))
			if err != nil {
				t.Fatalf("ParseRealtimeEvent() error = %v", err)
			}
			err = pinRealtimeTranscriptionModel(event, admitted, true)
			if tt.wantErr {
				if err == nil {
					t.Fatal("pinRealtimeTranscriptionModel() accepted a malformed transcription shape")
				}
				return
			}
			if err != nil {
				t.Fatalf("pinRealtimeTranscriptionModel() error = %v", err)
			}
			for shape, want := range tt.wantModels {
				var got string
				switch shape {
				case "audio":
					var audio struct {
						Input struct {
							Transcription struct {
								Model string `json:"model"`
							} `json:"transcription"`
						} `json:"input"`
					}
					if err := json.Unmarshal(event.Session.ExtraParams["audio"], &audio); err != nil {
						t.Fatalf("audio: %v", err)
					}
					got = audio.Input.Transcription.Model
				default:
					var legacy struct {
						Model    string `json:"model"`
						Language string `json:"language"`
					}
					if err := json.Unmarshal(event.Session.ExtraParams[shape], &legacy); err != nil {
						t.Fatalf("%s: %v", shape, err)
					}
					got = legacy.Model
				}
				if got != want {
					t.Fatalf("%s model = %q, want %q", shape, got, want)
				}
			}
			if raw, ok := event.Session.ExtraParams["input_audio_transcription"]; ok && len(tt.wantModels) == 0 && string(raw) != "null" {
				t.Fatalf("input_audio_transcription = %s, want null", raw)
			}
		})
	}

	t.Run("normal realtime session is not pinned", func(t *testing.T) {
		t.Parallel()
		event, err := schemas.ParseRealtimeEvent([]byte(`{"type":"session.update","session":{"input_audio_transcription":{"model":"whisper-1"}}}`))
		if err != nil {
			t.Fatalf("ParseRealtimeEvent() error = %v", err)
		}
		if err := pinRealtimeTranscriptionModel(event, admitted, false); err != nil {
			t.Fatalf("pinRealtimeTranscriptionModel() error = %v", err)
		}
		if got := string(event.Session.ExtraParams["input_audio_transcription"]); got != `{"model":"whisper-1"}` {
			t.Fatalf("input_audio_transcription = %s, want the client's own model", got)
		}
	})
}

// TestTranscriptionUpgradeCountsAgainstMaxConnectionsDuringBootstrap: a transcription
// client can hold its socket through the whole bootstrap window without sending a
// model, so it must be admitted (and counted against MaxConnections, and reachable
// by CloseAll) before the bootstrap read, not after it.
func TestTranscriptionUpgradeCountsAgainstMaxConnectionsDuringBootstrap(t *testing.T) {
	// Upstream #7317's credential check reads the handler's config and store.
	h := newRealtimeUpgradeHandler(false)
	h.sessions = bfws.NewSessionManager(1)
	r := router.New()
	r.GET("/v1/realtime", func(ctx *fasthttp.RequestCtx) {
		preReqCtx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
		h.handleTranscriptionUpgrade(ctx, preReqCtx, cancel, &authHeaders{}, "/v1/realtime", "")
	})
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fasthttp.Server{Handler: r.Handler}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown() //nolint:errcheck
	url := "ws://" + ln.Addr().String() + "/v1/realtime?intent=transcription"

	// First client upgrades and sends nothing: it is waiting in the bootstrap.
	idle, _, err := ws.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial idle client: %v", err)
	}
	defer idle.Close()
	deadline := time.Now().Add(2 * time.Second)
	for h.sessions.Count() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.sessions.Count(); got != 1 {
		t.Fatalf("sessions during bootstrap = %d, want 1: an idle bootstrap must already hold its slot", got)
	}

	// The second is refused at once, well inside the 15s bootstrap window.
	second, _, err := ws.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial second client: %v", err)
	}
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := second.ReadMessage()
	if err != nil {
		t.Fatalf("second client got no refusal: %v", err)
	}
	if !strings.Contains(string(msg), "rate_limit_exceeded") {
		t.Fatalf("second client got %s, want a rate_limit_exceeded refusal", msg)
	}

	h.Close()
	_ = idle.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := idle.ReadMessage(); err == nil {
		t.Fatal("CloseAll left the bootstrapping connection open")
	}
}

// TestTranscriptionUpgradeWithQueryModelSkipsTheBootstrap: with ?model= the provider
// and key are known up front, so the handler must go straight to opening the
// upstream session instead of waiting up to 15s for a model-bearing update, which
// a client that waits for session.created first would never send. Here no
// realtime provider is configured, so "straight on" shows as an immediate refusal.
func TestTranscriptionUpgradeWithQueryModelSkipsTheBootstrap(t *testing.T) {
	client, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account: wsSpanTestAccount{},
		Logger:  bifrost.NewDefaultLogger(schemas.LogLevelError),
	})
	if err != nil {
		t.Fatalf("init bifrost: %v", err)
	}
	defer client.Shutdown()
	h := newRealtimeUpgradeHandler(false)
	h.client, h.sessions = client, bfws.NewSessionManager(0)
	r := router.New()
	r.GET("/v1/realtime", func(ctx *fasthttp.RequestCtx) {
		preReqCtx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
		h.handleTranscriptionUpgrade(ctx, preReqCtx, cancel, &authHeaders{}, "/v1/realtime", "elevenlabs/scribe_v2_realtime")
	})
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fasthttp.Server{Handler: r.Handler}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown() //nolint:errcheck

	conn, _, err := ws.DefaultDialer.Dial("ws://"+ln.Addr().String()+"/v1/realtime?intent=transcription&model=elevenlabs/scribe_v2_realtime", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no answer without a bootstrap update: %v", err)
	}
	if strings.Contains(string(msg), "bootstrap") {
		t.Fatalf("handler waited for a bootstrap update despite ?model=: %s", msg)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("answered after %v, want it at once", elapsed)
	}
}

// A relay runs on its own goroutine, past the recover deferred by the upgrade callback, so a
// panic inside it (a plugin hook on the turn pipeline) must be caught by the relay runner
// itself: it ends the session like any other relay error instead of ending the process.
func TestRunRealtimeRelaysEndsSessionWhenARelayPanics(t *testing.T) {
	closed := make(chan struct{})
	var closes int
	toProvider := func() error {
		panic("plugin hook exploded")
	}
	toClient := func() error {
		<-closed // a blocking read, released by the teardown
		return net.ErrClosed
	}
	closeBoth := func() {
		closes++
		close(closed)
	}

	result := make(chan error, 1)
	go func() { result <- runRealtimeRelays(nil, toProvider, toClient, closeBoth) }()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("expected the panic to end the session as a relay error, got %v", err)
		}
		if strings.Contains(err.Error(), "exploded") {
			t.Fatalf("the panic value must not be echoed into the relay error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runRealtimeRelays did not return after a relay panicked")
	}
	if closes != 1 {
		t.Fatalf("expected both connections to be closed once, closeBoth ran %d times", closes)
	}
}
