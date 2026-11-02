package bifrost

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	schemas "github.com/capsohq/bifrost/core/schemas"
)

// ElevenLabs 流式语音读循环的回归测试（TXE-5677，TXE-4880 复现）。
// 上游在带 Content-Length 的响应上把最后一块数据和 io.EOF 一起返回时，
// 读循环必须先转发这块数据，再发送 speech.audio.done。

const elevenlabsStreamVoice = "owned-voice"

var elevenlabsStreamAudio = bytes.Repeat([]byte("owned-audio"), 512)

// elevenlabsStreamUpstream 是测试自己起的本地 HTTP 上游，按请求文本返回不同形状的响应。
type elevenlabsStreamUpstream struct {
	t         *testing.T
	cancelled chan struct{}
}

func (u *elevenlabsStreamUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := u.t
	if r.URL.Path != "/v1/text-to-speech/"+elevenlabsStreamVoice+"/stream" {
		t.Errorf("unexpected upstream path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if got := r.Header.Get("xi-api-key"); got != "owned-elevenlabs-value" {
		t.Errorf("unexpected xi-api-key: %q", got)
	}
	var request struct {
		Text  string `json:"text"`
		Model string `json:"model_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode upstream request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if request.Model != "eleven_turbo_v2_5" {
		t.Errorf("unexpected model_id: %q", request.Model)
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	flusher := w.(http.Flusher)
	switch request.Text {
	case "short":
		// 一次写完：客户端第一次读取就同时拿到全部数据和 io.EOF
		w.Header().Set("Content-Length", "28")
		u.write(w, []byte("TXE-5677-owned-audio-payload"))
	case "empty":
		w.Header().Set("Content-Length", "0")
	case "multi":
		w.Header().Set("Content-Length", strconv.Itoa(2*len(elevenlabsStreamAudio)))
		u.write(w, elevenlabsStreamAudio)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
		u.write(w, elevenlabsStreamAudio)
	case "gzip":
		// TXE-4880 复现里 gzip 编码的 Content-Length 响应同样丢失结尾
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(elevenlabsStreamAudio); err != nil {
			t.Errorf("gzip upstream body: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close gzip writer: %v", err)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		u.write(w, compressed.Bytes())
	case "truncated":
		// 声明 100 字节只写出一部分，服务端随后关闭连接
		w.Header().Set("Content-Length", "100")
		u.write(w, []byte("partial-audio"))
		flusher.Flush()
	case "cancel":
		u.write(w, elevenlabsStreamAudio)
		flusher.Flush()
		select {
		case <-r.Context().Done():
			u.cancelled <- struct{}{}
		case <-time.After(5 * time.Second):
			t.Error("upstream stream was not cancelled")
		}
	default:
		t.Errorf("unexpected speech text: %q", request.Text)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (u *elevenlabsStreamUpstream) write(w http.ResponseWriter, data []byte) {
	if _, err := w.Write(data); err != nil {
		u.t.Errorf("write upstream body: %v", err)
	}
}

type elevenlabsStreamResult struct {
	audio  []byte
	deltas int
	done   bool
	errors []string
}

func newElevenlabsStreamClient(t *testing.T) (*Bifrost, *elevenlabsStreamUpstream) {
	t.Helper()
	upstream := &elevenlabsStreamUpstream{t: t, cancelled: make(chan struct{}, 1)}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	account := NewMockAccount()
	account.AddProviderWithBaseURL(schemas.Elevenlabs, 4, 16, server.URL)
	account.configs[schemas.Elevenlabs].NetworkConfig.MaxRetries = 0
	account.SetKeysForProvider(schemas.Elevenlabs, []schemas.Key{{
		ID:     "owned-elevenlabs",
		Value:  *schemas.NewSecretVar("owned-elevenlabs-value"),
		Models: schemas.WhiteList{"*"},
		Weight: 100,
	}})
	return newStreamTestClient(t, account), upstream
}

func elevenlabsSpeechStreamRequest(text string) *schemas.BifrostSpeechRequest {
	voice := elevenlabsStreamVoice
	return &schemas.BifrostSpeechRequest{
		Provider: schemas.Elevenlabs,
		Model:    "eleven_turbo_v2_5",
		Input:    &schemas.SpeechInput{Input: text},
		Params:   &schemas.SpeechParameters{VoiceConfig: &schemas.SpeechVoiceInput{Voice: &voice}},
	}
}

func drainElevenlabsSpeechStream(t *testing.T, stream chan *schemas.BifrostStreamChunk) elevenlabsStreamResult {
	t.Helper()
	var result elevenlabsStreamResult
	for chunk := range stream {
		if chunk.BifrostError != nil {
			message := ""
			if chunk.BifrostError.Error != nil {
				message = chunk.BifrostError.Error.Message
			}
			result.errors = append(result.errors, message)
			continue
		}
		response := chunk.BifrostSpeechStreamResponse
		if response == nil {
			t.Fatalf("unexpected stream chunk: %+v", chunk)
		}
		if result.done {
			t.Fatalf("stream chunk %q arrived after speech.audio.done", response.Type)
		}
		switch response.Type {
		case schemas.SpeechStreamResponseTypeDelta:
			result.deltas++
			result.audio = append(result.audio, response.Audio...)
		case schemas.SpeechStreamResponseTypeDone:
			result.done = true
		default:
			t.Fatalf("unexpected speech stream type %q", response.Type)
		}
	}
	return result
}

func runElevenlabsSpeechStream(t *testing.T, client *Bifrost, text string) elevenlabsStreamResult {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), time.Now().Add(10*time.Second))
	stream, err := client.SpeechStreamRequest(ctx, elevenlabsSpeechStreamRequest(text))
	if err != nil {
		t.Fatalf("speech stream %q: %+v", text, err.Error)
	}
	return drainElevenlabsSpeechStream(t, stream)
}

func TestElevenlabsSpeechStreamDeliversDataReturnedWithEOF(t *testing.T) {
	client, _ := newElevenlabsStreamClient(t)
	result := runElevenlabsSpeechStream(t, client, "short")
	if !bytes.Equal(result.audio, []byte("TXE-5677-owned-audio-payload")) {
		t.Fatalf("short response audio = %q, want the full 28-byte payload", result.audio)
	}
	if !result.done || len(result.errors) != 0 {
		t.Fatalf("short response done=%v errors=%v, want done and no error", result.done, result.errors)
	}
}

func TestElevenlabsSpeechStreamEmptyResponse(t *testing.T) {
	client, _ := newElevenlabsStreamClient(t)
	result := runElevenlabsSpeechStream(t, client, "empty")
	if len(result.audio) != 0 || result.deltas != 0 {
		t.Fatalf("empty response produced %d deltas with %d bytes", result.deltas, len(result.audio))
	}
	if !result.done || len(result.errors) != 0 {
		t.Fatalf("empty response done=%v errors=%v, want done and no error", result.done, result.errors)
	}
}

func TestElevenlabsSpeechStreamMultiChunkResponse(t *testing.T) {
	client, _ := newElevenlabsStreamClient(t)
	result := runElevenlabsSpeechStream(t, client, "multi")
	want := append(append([]byte{}, elevenlabsStreamAudio...), elevenlabsStreamAudio...)
	if !bytes.Equal(result.audio, want) {
		t.Fatalf("multi-chunk audio has %d bytes, want %d in order", len(result.audio), len(want))
	}
	if result.deltas < 2 {
		t.Fatalf("multi-chunk response produced %d deltas, want at least 2", result.deltas)
	}
	if !result.done || len(result.errors) != 0 {
		t.Fatalf("multi-chunk response done=%v errors=%v, want done and no error", result.done, result.errors)
	}
}

func TestElevenlabsSpeechStreamGzipResponse(t *testing.T) {
	client, _ := newElevenlabsStreamClient(t)
	result := runElevenlabsSpeechStream(t, client, "gzip")
	if !bytes.Equal(result.audio, elevenlabsStreamAudio) {
		t.Fatalf("gzip response audio has %d bytes, want the %d decompressed bytes", len(result.audio), len(elevenlabsStreamAudio))
	}
	if !result.done || len(result.errors) != 0 {
		t.Fatalf("gzip response done=%v errors=%v, want done and no error", result.done, result.errors)
	}
}

func TestElevenlabsSpeechStreamTruncatedResponseReportsError(t *testing.T) {
	client, _ := newElevenlabsStreamClient(t)
	result := runElevenlabsSpeechStream(t, client, "truncated")
	if !bytes.Equal(result.audio, []byte("partial-audio")) {
		t.Fatalf("truncated response audio = %q, want the bytes the upstream sent", result.audio)
	}
	if len(result.errors) != 1 {
		t.Fatalf("truncated response errors = %v, want exactly one error", result.errors)
	}
	if result.done {
		t.Fatal("truncated response sent speech.audio.done")
	}
}

func TestElevenlabsSpeechStreamCancellationClosesUpstream(t *testing.T) {
	client, upstream := newElevenlabsStreamClient(t)
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	stream, err := client.SpeechStreamRequest(ctx, elevenlabsSpeechStreamRequest("cancel"))
	if err != nil {
		t.Fatalf("speech stream cancel: %+v", err.Error)
	}
	first := <-stream
	if first == nil || first.BifrostSpeechStreamResponse == nil || first.BifrostSpeechStreamResponse.Type != schemas.SpeechStreamResponseTypeDelta {
		t.Fatalf("first chunk = %+v, want an audio delta", first)
	}
	cancel()
	for chunk := range stream {
		if chunk.BifrostSpeechStreamResponse != nil && chunk.BifrostSpeechStreamResponse.Type == schemas.SpeechStreamResponseTypeDone {
			t.Fatal("cancelled stream sent speech.audio.done")
		}
	}
	select {
	case <-upstream.cancelled:
	case <-time.After(6 * time.Second):
		t.Fatal("cancelling the stream did not close the upstream request")
	}
}
