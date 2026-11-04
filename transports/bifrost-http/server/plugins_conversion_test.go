package server

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/capsohq/bifrost/core/schemas"

	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// typedFormatUpstream is formatUpstream with the request_type the gateway writes for it.
func typedFormatUpstream(bifrostProvider, keyID, forwardModel string, sameFormat bool, requestType schemas.RequestType) *routingv1.RouteUpstream {
	upstream := formatUpstream(bifrostProvider, keyID, forwardModel, sameFormat)
	upstream.RequestType = string(requestType)
	return upstream
}

// sentAttempts summarizes the attempts of a reply's tk-route-attempts as "<entry>/<status>", or
// "<entry> <skip_reason>" for an entry not sent.
func sentAttempts(t *testing.T, got formatReply) []string {
	t.Helper()
	record := routeAttempts(t, got.attempts)
	var attempts []string
	for _, a := range record.GetAttempts() {
		entry := record.GetOrder()[a.GetIndex()]
		if a.GetSkipReason() != "" {
			attempts = append(attempts, fmt.Sprintf("%s %s", entry.GetKeyId(), a.GetSkipReason()))
			continue
		}
		attempts = append(attempts, fmt.Sprintf("%s/%d", entry.GetKeyId(), a.GetStatusCode()))
	}
	return attempts
}

// checkPaths checks that the upstreams were called on entry and path, in order, and returns the
// calls.
func checkPaths(t *testing.T, server *formatServer, want ...string) []formatCall {
	t.Helper()
	calls := server.upstreams.take()
	var got []string
	for _, c := range calls {
		got = append(got, c.entry+c.path)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream calls %v, want %v", got, want)
	}
	return calls
}

// checkChatBody checks that an upstream received a chat completion for model, converted from the
// caller's request: messages, and nothing of the caller's own format.
func checkChatBody(t *testing.T, body []byte, model string, stream bool) {
	t.Helper()
	chat := gjson.ParseBytes(body)
	if chat.Get("model").String() != model || !chat.Get("messages").IsArray() || chat.Get("input").Exists() ||
		chat.Get("zz_unknown").Exists() || chat.Get("stream").Bool() != stream {
		t.Fatalf("the chat-only upstream received %s, want a chat completion for %s (stream %v)", body, model, stream)
	}
	if stream && !chat.Get("stream_options.include_usage").Bool() {
		t.Fatalf("the chat-only upstream's stream does not ask for usage: %s", body)
	}
}

// checkResponsesUsage checks a Responses usage object against richChatUsage, field by field.
func checkResponsesUsage(t *testing.T, usage gjson.Result) {
	t.Helper()
	for path, want := range map[string]int64{
		"input_tokens":                            1200,
		"output_tokens":                           300,
		"total_tokens":                            1500,
		"input_tokens_details.text_tokens":        1100,
		"input_tokens_details.audio_tokens":       50,
		"input_tokens_details.image_tokens":       50,
		"input_tokens_details.cached_tokens":      400,
		"input_tokens_details.cache_write_tokens": 250,
		"input_tokens_details.cached_write_token_details.cached_write_tokens_5m": 150,
		"input_tokens_details.cached_write_token_details.cached_write_tokens_1h": 100,
		"output_tokens_details.text_tokens":                                      170,
		"output_tokens_details.audio_tokens":                                     5,
		"output_tokens_details.image_tokens":                                     7,
		"output_tokens_details.reasoning_tokens":                                 120,
		"output_tokens_details.accepted_prediction_tokens":                       3,
		"output_tokens_details.rejected_prediction_tokens":                       4,
	} {
		if got := usage.Get(path); !got.Exists() || got.Int() != want {
			t.Errorf("usage.%s = %s, want %d (usage %s)", path, got.Raw, want, usage.Raw)
		}
	}
}

// checkMessagesUsage checks an Anthropic Messages usage object against a chat usage of 1200 input
// tokens, 400 read from the cache and 250 written to it (150 for 5 minutes, 100 for 1 hour), and
// 300 output tokens: Anthropic's input_tokens are those neither read from nor written to the
// cache.
func checkMessagesUsage(t *testing.T, usage gjson.Result, outputTokens int64) {
	t.Helper()
	for path, want := range map[string]int64{
		"input_tokens":                             550,
		"cache_read_input_tokens":                  400,
		"cache_creation_input_tokens":              250,
		"cache_creation.ephemeral_5m_input_tokens": 150,
		"cache_creation.ephemeral_1h_input_tokens": 100,
		"output_tokens":                            outputTokens,
	} {
		if got := usage.Get(path); !got.Exists() || got.Int() != want {
			t.Errorf("usage.%s = %s, want %d (usage %s)", path, got.Raw, want, usage.Raw)
		}
	}
}

// responsesStreamEvents returns the event types of a Responses stream, the text its deltas spell
// and the response of its response.completed.
func responsesStreamEvents(t *testing.T, body string) (types []string, text string, completed gjson.Result) {
	t.Helper()
	var b strings.Builder
	for _, e := range sseEvents(body) {
		event := gjson.Parse(e.data)
		types = append(types, event.Get("type").String())
		switch event.Get("type").String() {
		case "response.output_text.delta":
			b.WriteString(event.Get("delta").String())
		case "response.completed":
			completed = event.Get("response")
		}
	}
	return types, b.String(), completed
}

// TestTokunPluginSendsResponsesToChatOnlyUpstreams sends Responses and Messages requests, streamed
// and not, with signed plans whose candidates are a Responses or Messages upstream and an upstream
// that has only chat completions (up-chatonly, whose request_type is chat_completion). The compat
// plugin converts each attempt to its own upstream's type: the Responses upstream receives the
// caller's body as it is, the chat-only upstream a chat completion, in either order of the
// attempts; the caller receives its own format, with every usage field the chat-only upstream
// reported, cache reads, cache writes for 5 minutes and for 1 hour, and reasoning among them.
func TestTokunPluginSendsResponsesToChatOnlyUpstreams(t *testing.T) {
	server := newFormatServer(t, true)
	const responsesURI, messagesURI = "/openai/v1/responses", "/anthropic/v1/messages"

	for _, stream := range []bool{false, true} {
		responsesType, chatType := schemas.ResponsesRequest, schemas.ChatCompletionRequest
		body, name := responsesCallerBody, "not streamed"
		if stream {
			responsesType, chatType = schemas.ResponsesStreamRequest, schemas.ChatCompletionStreamRequest
			body, name = streamed(responsesCallerBody, ""), "streamed"
		}
		plan := func(id string) *routingv1.RoutePlan {
			return formatPlan(id,
				typedFormatUpstream("up-openai", "oa-1", "responses-model", true, responsesType),
				typedFormatUpstream("up-chatonly", "ch-1", "chat-model", false, chatType))
		}

		t.Run("Responses, "+name+": the Responses upstream receives the caller's body", func(t *testing.T) {
			server.upstreams.reset()
			got := server.sendAsIs(t, responsesURI, body, plan("req_rc_same"), "snd_rc_same_"+name)
			checkCallerFormat(t, responsesURI, stream, got)
			calls := checkPaths(t, server, "up-openai/v1/responses")
			if want := forwarded(t, body, "tk/responses-model", "responses-model"); !bytes.Equal(calls[0].body, want) {
				t.Fatalf("the Responses upstream received\n%s\nwant\n%s", calls[0].body, want)
			}
		})

		t.Run("Responses, "+name+": the chat-only upstream receives a chat completion", func(t *testing.T) {
			server.upstreams.reset("oa-1")
			got := server.sendAsIs(t, responsesURI, body, plan("req_rc_chat"), "snd_rc_chat_"+name)
			if got.status != http.StatusOK {
				t.Fatalf("status %d: %s", got.status, got.body)
			}
			calls := checkPaths(t, server, "up-openai/v1/responses", "up-chatonly/v1/chat/completions")
			checkChatBody(t, calls[1].body, "chat-model", stream)
			if attempts := sentAttempts(t, got); !reflect.DeepEqual(attempts, []string{"oa-1/500", "ch-1/200"}) {
				t.Fatalf("attempts %v", attempts)
			}
			response := gjson.Parse(got.body)
			if !stream {
				if response.Get("object").String() != "response" || response.Get("output.0.content.0.text").String() != "ok" || response.Get("status").String() != "completed" {
					t.Fatalf("not a completed response saying ok: %s", got.body)
				}
			} else {
				types, text, completed := responsesStreamEvents(t, got.body)
				if text != "ok" || len(types) == 0 || types[0] != "response.created" || types[len(types)-1] != "response.completed" {
					t.Fatalf("not a Responses stream saying ok from response.created to response.completed: %v %s", types, got.body)
				}
				response = completed
			}
			checkResponsesUsage(t, response.Get("usage"))
			// Server-side tool calls are on the response itself.
			if searches := response.Get("tool_usage.web_search.num_requests"); searches.Int() != 2 {
				t.Errorf("tool_usage.web_search.num_requests = %s, want 2: %s", searches.Raw, response.Raw)
			}
		})
	}

	for _, stream := range []bool{false, true} {
		messagesType, chatType := schemas.ResponsesRequest, schemas.ChatCompletionRequest
		body, name := messagesCallerBody, "not streamed"
		if stream {
			messagesType, chatType = schemas.ResponsesStreamRequest, schemas.ChatCompletionStreamRequest
			body, name = streamed(messagesCallerBody, ""), "streamed"
		}
		t.Run("Messages, "+name+": the chat-only upstream after the Messages upstream", func(t *testing.T) {
			server.upstreams.reset("an-1")
			plan := formatPlan("req_mc",
				typedFormatUpstream("up-anthropic", "an-1", "claude-model", true, messagesType),
				typedFormatUpstream("up-chatonly", "ch-1", "chat-model", false, chatType))
			got := server.sendAsIs(t, messagesURI, body, plan, "snd_mc_"+name)
			checkCallerFormat(t, messagesURI, stream, got)
			calls := checkPaths(t, server, "up-anthropic/v1/messages", "up-chatonly/v1/chat/completions")
			checkChatBody(t, calls[1].body, "chat-model", stream)
			if !stream {
				checkMessagesUsage(t, gjson.Get(got.body, "usage"), 300)
				if searches := gjson.Get(got.body, "usage.server_tool_use.web_search_requests"); searches.Int() != 2 {
					t.Errorf("usage.server_tool_use.web_search_requests = %s, want 2: %s", searches.Raw, got.body)
				}
				return
			}
			for _, e := range sseEvents(got.body) {
				event := gjson.Parse(e.data)
				switch event.Get("type").String() {
				case "message_start":
					usage := event.Get("message.usage")
					if usage.Get("input_tokens").Exists() && usage.Get("input_tokens").Int() != 0 {
						// The input usage arrives with the first event only when the upstream reports
						// it before the output; a chat stream reports it at the end.
						checkMessagesUsage(t, usage, usage.Get("output_tokens").Int())
					}
				case "message_delta":
					checkMessagesUsage(t, event.Get("usage"), 300)
				}
			}
		})
	}

	// The conversion is per attempt: a chat-only upstream that fails first, then a Responses
	// upstream, which receives a Responses request.
	t.Run("Messages: a Responses upstream after the chat-only upstream", func(t *testing.T) {
		server.upstreams.reset("ch-1")
		plan := formatPlan("req_cm",
			typedFormatUpstream("up-chatonly", "ch-1", "chat-model", false, schemas.ChatCompletionRequest),
			typedFormatUpstream("up-openai", "oa-1", "responses-model", false, schemas.ResponsesRequest))
		got := server.sendAsIs(t, messagesURI, messagesCallerBody, plan, "snd_cm")
		checkCallerFormat(t, messagesURI, false, got)
		calls := checkPaths(t, server, "up-chatonly/v1/chat/completions", "up-openai/v1/responses")
		if upstream := gjson.ParseBytes(calls[1].body); upstream.Get("model").String() != "responses-model" || !upstream.Get("input").IsArray() || upstream.Get("messages").Exists() {
			t.Fatalf("the Responses upstream received %s, want a Responses request", calls[1].body)
		}
	})

	// Anthropic's Messages upstream reports its cache writes by duration; sent a Responses request
	// as a chat completion, the caller's Responses usage keeps both parts.
	t.Run("Responses: an Anthropic upstream that takes chat completions", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_ac", typedFormatUpstream("up-anthropic-cache", "ac-1", "claude-model", false, schemas.ChatCompletionRequest))
		got := server.sendAsIs(t, responsesURI, responsesCallerBody, plan, "snd_ac")
		if got.status != http.StatusOK {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		checkPaths(t, server, "up-anthropic-cache/v1/messages")
		checkResponsesCacheUsage(t, gjson.Get(got.body, "usage"))
	})

	// Anthropic's provider streams chat chunks for a chat completion and does not make Responses
	// events of them: the attempt fails before the caller receives anything, and the next entry
	// answers.
	t.Run("Responses, streamed: a provider that does not make Responses events of a chat stream", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_unconverted",
			typedFormatUpstream("up-anthropic-cache", "ac-1", "claude-model", false, schemas.ChatCompletionStreamRequest),
			typedFormatUpstream("up-chatonly", "ch-1", "chat-model", false, schemas.ChatCompletionStreamRequest))
		got := server.sendAsIs(t, responsesURI, streamed(responsesCallerBody, ""), plan, "snd_unconverted")
		types, text, completed := responsesStreamEvents(t, got.body)
		if got.status != http.StatusOK || text != "ok" || types[len(types)-1] != "response.completed" {
			t.Fatalf("status %d, not a Responses stream saying ok: %s", got.status, got.body)
		}
		checkResponsesUsage(t, completed.Get("usage"))
		checkPaths(t, server, "up-anthropic-cache/v1/messages", "up-chatonly/v1/chat/completions")
		record := routeAttempts(t, got.attempts)
		first := record.GetAttempts()[0]
		if first.GetKeyId() != "ac-1" || first.GetStatusCode() != 0 || first.GetErrorScope() != "upstream" || first.GetErrorClass() != "transport" {
			t.Fatalf("the first attempt is recorded as %v, want ac-1 failed in Bifrost", first)
		}
		if attempts := sentAttempts(t, got); !reflect.DeepEqual(attempts, []string{"ac-1/0", "ch-1/200"}) {
			t.Fatalf("attempts %v", attempts)
		}
	})
}

// checkResponsesCacheUsage checks a Responses usage object against up-anthropic-cache's Messages
// usage: 550 input tokens neither read from nor written to the cache, 400 read, 250 written (150
// for 5 minutes and 100 for 1 hour), and 300 output tokens.
func checkResponsesCacheUsage(t *testing.T, usage gjson.Result) {
	t.Helper()
	for path, want := range map[string]int64{
		"input_tokens":                            1200,
		"output_tokens":                           300,
		"total_tokens":                            1500,
		"input_tokens_details.cached_tokens":      400,
		"input_tokens_details.cache_write_tokens": 250,
		"input_tokens_details.cached_write_token_details.cached_write_tokens_5m": 150,
		"input_tokens_details.cached_write_token_details.cached_write_tokens_1h": 100,
	} {
		if got := usage.Get(path); !got.Exists() || got.Int() != want {
			t.Errorf("usage.%s = %s, want %d (usage %s)", path, got.Raw, want, usage.Raw)
		}
	}
}

// TestTokunPluginCountsTokens sends Messages token counts with signed plans: an upstream whose
// interface has the count_tokens operation receives the request and its count is the answer,
// while one without the operation is passed over; with no such upstream in the plan, the plugin
// answers with the local count and no upstream is called.
func TestTokunPluginCountsTokens(t *testing.T) {
	server := newFormatServer(t, true)
	const uri = "/anthropic/v1/messages/count_tokens"

	t.Run("an upstream that counts", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_count",
			typedFormatUpstream("up-anthropic", "an-1", "up-model", true, ""),
			typedFormatUpstream("up-openai", "oa-1", "up-model", false, schemas.CountTokensRequest))
		got := server.sendAsIs(t, uri, countCallerBody, plan, "snd_count")
		if got.status != http.StatusOK || gjson.Get(got.body, "input_tokens").Int() != 12 {
			t.Fatalf("status %d: %s, want the upstream's count of 12", got.status, got.body)
		}
		checkPaths(t, server, "up-openai/v1/responses/input_tokens")
		if attempts := sentAttempts(t, got); !reflect.DeepEqual(attempts, []string{"an-1 operation_not_offered", "oa-1/200"}) {
			t.Fatalf("attempts %v", attempts)
		}
	})

	t.Run("no upstream that counts", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_estimate",
			typedFormatUpstream("up-anthropic", "an-1", "up-model", true, ""),
			typedFormatUpstream("up-openai", "oa-1", "up-model", false, ""))
		plan.TokenEstimate = &routingv1.TokenEstimate{
			InputBytesPerToken: 3, InputOverheadTokens: 512, ImagePatchPixels: 28, ImageMaxEdgePixels: 2576, ImageMaxTokens: 4784, DocumentPageTokens: 4568,
		}
		body := `{"model":"tk/claude","system":"You are terse.","messages":[{"role":"user","content":[{"type":"text","text":"How many tokens is this?"}]}],"tools":[{"name":"t","input_schema":{"type":"object"}}]}`
		got := server.sendAsIs(t, uri, body, plan, "snd_estimate")
		tool := `{"name":"t","input_schema":{"type":"object"}}`
		want := int64(math.Ceil(float64(len("You are terse.")+len("How many tokens is this?")+len(tool))/3)) + 512
		if got.status != http.StatusOK || gjson.Get(got.body, "input_tokens").Int() != want {
			t.Fatalf("status %d: %s, want the local count of %d", got.status, got.body, want)
		}
		checkPaths(t, server)
		if attempts := sentAttempts(t, got); !reflect.DeepEqual(attempts, []string{"an-1 operation_not_offered", "oa-1 operation_not_offered"}) {
			t.Fatalf("attempts %v", attempts)
		}
	})

	t.Run("no upstream that counts, an image by URL", func(t *testing.T) {
		server.upstreams.reset()
		plan := formatPlan("req_estimate_url", typedFormatUpstream("up-anthropic", "an-1", "up-model", true, ""))
		plan.TokenEstimate = &routingv1.TokenEstimate{
			InputBytesPerToken: 3, InputOverheadTokens: 512, ImagePatchPixels: 28, ImageMaxEdgePixels: 2576, ImageMaxTokens: 4784, DocumentPageTokens: 4568,
		}
		body := `{"model":"tk/claude","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`
		got := server.sendAsIs(t, uri, body, plan, "snd_estimate_url")
		if got.status != http.StatusBadRequest || gjson.Get(got.body, "error.type").String() != "invalid_request_error" {
			t.Fatalf("status %d: %s, want 400 invalid_request_error", got.status, got.body)
		}
		checkPaths(t, server)
	})
}
