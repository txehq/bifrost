package gemini_test

import (
	"math"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/providers/gemini"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Gemini image-generation models (e.g. gemini-2.5-flash-image) return the
// generated image as a Part carrying only InlineData, no text. Bifrost's own
// Responses converter and image converter already preserve this field; the
// Chat Completions converters must too, on both the unary and streaming path.

// TestToBifrostChatResponse_InlineDataImage pins that a unary response whose only
// part is an image InlineData blob yields an image_url content block carrying the
// image as a data URL instead of an empty message.
func TestToBifrostChatResponse_InlineDataImage(t *testing.T) {
	response := &gemini.GenerateContentResponse{
		ResponseID:   "inline-image-test",
		ModelVersion: "gemini-2.5-flash-image",
		Candidates: []*gemini.Candidate{
			{
				FinishReason: gemini.FinishReasonStop,
				Content: &gemini.Content{
					Role: string(gemini.RoleModel),
					Parts: []*gemini.Part{
						{InlineData: &gemini.Blob{MIMEType: "image/png", Data: "cGluZ3BvbmdpbWFnZWJ5dGVz"}},
					},
				},
			},
		},
		UsageMetadata: &gemini.GenerateContentResponseUsageMetadata{
			CandidatesTokenCount: 1290,
		},
	}

	bifrostResp := response.ToBifrostChatResponse()
	require.NotNil(t, bifrostResp)
	require.Len(t, bifrostResp.Choices, 1)

	message := bifrostResp.Choices[0].ChatNonStreamResponseChoice.Message
	require.NotNil(t, message)
	require.NotNil(t, message.Content)
	require.Len(t, message.Content.ContentBlocks, 1,
		"the generated image must appear as a content block instead of being silently dropped")

	block := message.Content.ContentBlocks[0]
	assert.Equal(t, schemas.ChatContentBlockTypeImage, block.Type)
	require.NotNil(t, block.ImageURLStruct)
	assert.Equal(t, "data:image/png;base64,cGluZ3BvbmdpbWFnZWJ5dGVz", block.ImageURLStruct.URL)
}

// TestToBifrostChatResponse_InlineDataAudio pins that a unary response whose only
// part is an audio InlineData blob yields an input_audio content block with the raw
// base64 payload and the format derived from the MIME type.
func TestToBifrostChatResponse_InlineDataAudio(t *testing.T) {
	response := &gemini.GenerateContentResponse{
		ResponseID:   "inline-audio-test",
		ModelVersion: "gemini-2.5-flash-native-audio",
		Candidates: []*gemini.Candidate{
			{
				FinishReason: gemini.FinishReasonStop,
				Content: &gemini.Content{
					Role: string(gemini.RoleModel),
					Parts: []*gemini.Part{
						{InlineData: &gemini.Blob{MIMEType: "audio/wav", Data: "d2F2Ynl0ZXM="}},
					},
				},
			},
		},
	}

	bifrostResp := response.ToBifrostChatResponse()
	require.NotNil(t, bifrostResp)
	message := bifrostResp.Choices[0].ChatNonStreamResponseChoice.Message
	require.NotNil(t, message.Content)
	require.Len(t, message.Content.ContentBlocks, 1)

	block := message.Content.ContentBlocks[0]
	assert.Equal(t, schemas.ChatContentBlockTypeInputAudio, block.Type)
	require.NotNil(t, block.InputAudio)
	assert.Equal(t, "d2F2Ynl0ZXM=", block.InputAudio.Data)
	require.NotNil(t, block.InputAudio.Format)
	assert.Equal(t, "wav", *block.InputAudio.Format)
}

// TestToBifrostChatResponse_InlineDataWithPrecedingText pins that a caption beside
// the image (observed in live traffic as text parts preceding a solo InlineData part)
// keeps both the text block and the image block, in order, rather than collapsing to a
// string or dropping either.
func TestToBifrostChatResponse_InlineDataWithPrecedingText(t *testing.T) {
	response := &gemini.GenerateContentResponse{
		ResponseID:   "inline-image-with-text-test",
		ModelVersion: "gemini-2.5-flash-image",
		Candidates: []*gemini.Candidate{
			{
				FinishReason: gemini.FinishReasonStop,
				Content: &gemini.Content{
					Role: string(gemini.RoleModel),
					Parts: []*gemini.Part{
						{Text: "Here is your image:"},
						{InlineData: &gemini.Blob{MIMEType: "image/png", Data: "aW1hZ2VieXRlcw=="}},
					},
				},
			},
		},
	}

	bifrostResp := response.ToBifrostChatResponse()
	message := bifrostResp.Choices[0].ChatNonStreamResponseChoice.Message
	require.NotNil(t, message.Content)
	require.Len(t, message.Content.ContentBlocks, 2)
	assert.Equal(t, schemas.ChatContentBlockTypeText, message.Content.ContentBlocks[0].Type)
	assert.Equal(t, schemas.ChatContentBlockTypeImage, message.Content.ContentBlocks[1].Type)
}

// TestToBifrostChatCompletionStream_InlineDataImage pins that a streamed chunk whose
// only part is an image InlineData blob converts to exactly one delta whose Content
// carries the image as a data URL, instead of being silently skipped.
func TestToBifrostChatCompletionStream_InlineDataImage(t *testing.T) {
	response := &gemini.GenerateContentResponse{
		ResponseID:   "inline-image-stream-test",
		ModelVersion: "gemini-2.5-flash-image",
		Candidates: []*gemini.Candidate{
			{
				Content: &gemini.Content{
					Role: string(gemini.RoleModel),
					Parts: []*gemini.Part{
						{InlineData: &gemini.Blob{MIMEType: "image/png", Data: "c3RyZWFtaW1hZ2U="}},
					},
				},
			},
		},
	}

	state := gemini.NewGeminiStreamState()
	chunks, bifrostErr, isLast := response.ToBifrostChatCompletionStream(state)
	require.Nil(t, bifrostErr)
	require.Len(t, chunks, 1, "the chunk carrying the image must not be silently skipped")
	assert.False(t, isLast)

	require.Len(t, chunks[0].Choices, 1)
	delta := chunks[0].Choices[0].ChatStreamResponseChoice.Delta
	require.NotNil(t, delta)
	require.NotNil(t, delta.Content, "the image must be recoverable from the streamed delta")
	assert.Equal(t, "data:image/png;base64,c3RyZWFtaW1hZ2U=", *delta.Content)
}

// TestToBifrostChatCompletionStream_InlineDataAudio pins that a streamed chunk whose
// only part is an audio InlineData blob converts to one delta carrying the payload in
// delta.Audio, and that the has-content guard does not skip an audio-only chunk.
func TestToBifrostChatCompletionStream_InlineDataAudio(t *testing.T) {
	response := &gemini.GenerateContentResponse{
		ResponseID:   "inline-audio-stream-test",
		ModelVersion: "gemini-2.5-flash-native-audio",
		Candidates: []*gemini.Candidate{
			{
				Content: &gemini.Content{
					Role: string(gemini.RoleModel),
					Parts: []*gemini.Part{
						{InlineData: &gemini.Blob{MIMEType: "audio/wav", Data: "c3RyZWFtYXVkaW8="}},
					},
				},
			},
		},
	}

	state := gemini.NewGeminiStreamState()
	chunks, bifrostErr, isLast := response.ToBifrostChatCompletionStream(state)
	require.Nil(t, bifrostErr)
	require.Len(t, chunks, 1, "a chunk carrying only inline audio must not be skipped by the has-content guard")
	assert.False(t, isLast)

	delta := chunks[0].Choices[0].ChatStreamResponseChoice.Delta
	require.NotNil(t, delta)
	require.NotNil(t, delta.Audio, "the audio must be recoverable from the streamed delta")
	assert.Equal(t, "c3RyZWFtYXVkaW8=", delta.Audio.Data)
	assert.Nil(t, delta.Content, "audio travels in its own field, not in Content")
}

// TestToBifrostChatCompletionStream_SplitsMixedInlineMedia pins that a single Gemini
// chunk whose parts mix text with inline media is emitted as separate deltas in the
// original part order, one per inline-media part, so an image data URL is never fused
// into surrounding text. Consecutive text parts still share one delta, and the finish
// reason and usage land only on the final delta.
func TestToBifrostChatCompletionStream_SplitsMixedInlineMedia(t *testing.T) {
	const imageDataURL = "data:image/png;base64,bWl4ZWRpbWFnZQ=="
	image := &gemini.Part{InlineData: &gemini.Blob{MIMEType: "image/png", Data: "bWl4ZWRpbWFnZQ=="}}
	audio := &gemini.Part{InlineData: &gemini.Blob{MIMEType: "audio/wav", Data: "bWl4ZWRhdWRpbw=="}}

	type wantDelta struct {
		text      string
		audioData string
	}

	tests := []struct {
		name  string
		parts []*gemini.Part
		want  []wantDelta
	}{
		{
			name:  "text then image",
			parts: []*gemini.Part{{Text: "Here is your image:"}, image},
			want:  []wantDelta{{text: "Here is your image:"}, {text: imageDataURL}},
		},
		{
			name:  "image then text",
			parts: []*gemini.Part{image, {Text: "Done."}},
			want:  []wantDelta{{text: imageDataURL}, {text: "Done."}},
		},
		{
			name:  "text image text",
			parts: []*gemini.Part{{Text: "Before "}, image, {Text: " after"}},
			want:  []wantDelta{{text: "Before "}, {text: imageDataURL}, {text: " after"}},
		},
		{
			name:  "text then audio",
			parts: []*gemini.Part{{Text: "Listen:"}, audio},
			want:  []wantDelta{{text: "Listen:"}, {audioData: "bWl4ZWRhdWRpbw=="}},
		},
		{
			name:  "two text parts stay in one delta",
			parts: []*gemini.Part{{Text: "Hello, "}, {Text: "world"}},
			want:  []wantDelta{{text: "Hello, world"}},
		},
		{
			name:  "image alone needs no split",
			parts: []*gemini.Part{image},
			want:  []wantDelta{{text: imageDataURL}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := &gemini.GenerateContentResponse{
				ResponseID:   "mixed-stream-test",
				ModelVersion: "gemini-2.5-flash-image",
				Candidates: []*gemini.Candidate{
					{
						FinishReason: gemini.FinishReasonStop,
						Content: &gemini.Content{
							Role:  string(gemini.RoleModel),
							Parts: tt.parts,
						},
					},
				},
				UsageMetadata: &gemini.GenerateContentResponseUsageMetadata{
					CandidatesTokenCount: 1297,
				},
			}

			chunks, bifrostErr, isLast := response.ToBifrostChatCompletionStream(gemini.NewGeminiStreamState())
			require.Nil(t, bifrostErr)
			assert.True(t, isLast, "a finish reason with usage closes the stream")
			require.Len(t, chunks, len(tt.want), "each inline-media part must be its own delta")

			for i, want := range tt.want {
				require.Len(t, chunks[i].Choices, 1)
				choice := chunks[i].Choices[0]
				delta := choice.ChatStreamResponseChoice.Delta
				require.NotNil(t, delta)

				if want.audioData != "" {
					require.NotNil(t, delta.Audio, "delta %d must carry the audio", i)
					assert.Equal(t, want.audioData, delta.Audio.Data)
					assert.Nil(t, delta.Content, "delta %d must not put audio into Content", i)
				} else {
					require.NotNil(t, delta.Content, "delta %d must carry content", i)
					assert.Equal(t, want.text, *delta.Content)
					if strings.HasPrefix(*delta.Content, "data:image/") {
						assert.Equal(t, imageDataURL, *delta.Content,
							"an image delta must hold the data URL alone, with no text fused around it")
					} else {
						assert.NotContains(t, *delta.Content, "data:image/",
							"a text delta must not have image bytes appended to it")
					}
				}

				if i == len(tt.want)-1 {
					require.NotNil(t, choice.FinishReason, "the final delta carries the finish reason")
					assert.Equal(t, "stop", *choice.FinishReason)
					assert.NotNil(t, chunks[i].Usage, "the final delta carries usage")
				} else {
					assert.Nil(t, choice.FinishReason, "only the final delta may carry the finish reason")
					assert.Nil(t, chunks[i].Usage, "only the final delta may carry usage")
				}
			}
		})
	}
}

// TestToBifrostChatCompletionStream_ErrorFinishReasonIsNotSplit pins that a chunk
// mixing text and inline media on a candidate whose finish reason is an error (here a
// safety block) is not split into pieces: the converter must return the single error
// response and suppress every part, exactly as it does for an unmixed chunk, instead of
// leaking the text or the image ahead of the content-filter error.
func TestToBifrostChatCompletionStream_ErrorFinishReasonIsNotSplit(t *testing.T) {
	tests := []struct {
		name         string
		finishReason gemini.FinishReason
	}{
		{name: "safety", finishReason: gemini.FinishReasonSafety},
		{name: "image safety", finishReason: gemini.FinishReasonImageSafety},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := &gemini.GenerateContentResponse{
				ResponseID:   "blocked-mixed-stream-test",
				ModelVersion: "gemini-2.5-flash-image",
				Candidates: []*gemini.Candidate{
					{
						FinishReason: tt.finishReason,
						Content: &gemini.Content{
							Role: string(gemini.RoleModel),
							Parts: []*gemini.Part{
								{Text: "Here is your image:"},
								{InlineData: &gemini.Blob{MIMEType: "image/png", Data: "YmxvY2tlZA=="}},
							},
						},
					},
				},
				UsageMetadata: &gemini.GenerateContentResponseUsageMetadata{CandidatesTokenCount: 12},
			}

			chunks, bifrostErr, isLast := response.ToBifrostChatCompletionStream(gemini.NewGeminiStreamState())
			require.Nil(t, bifrostErr)
			assert.True(t, isLast)
			require.Len(t, chunks, 1, "a filtered candidate must yield the single error response, not split pieces")
			require.Len(t, chunks[0].Choices, 1)

			choice := chunks[0].Choices[0]
			require.NotNil(t, choice.FinishReason)
			assert.Equal(t, gemini.ConvertGeminiFinishReasonToBifrost(tt.finishReason), *choice.FinishReason)
			if delta := choice.ChatStreamResponseChoice.Delta; delta != nil && delta.Content != nil {
				assert.NotContains(t, *delta.Content, "data:image/", "the blocked image must not leak")
				assert.NotContains(t, *delta.Content, "Here is your image", "the blocked text must not leak")
			}
		})
	}
}

// TestToGeminiChatCompletionRequest_MidConversationSystemInlined pins the Chat Completions path
// to what the Responses path already does (inlineGeminiSystemReminder): a role:"system" message
// that arrives after the conversation has started is inlined at its position as a user turn, not
// hoisted into systemInstruction. Gemini's implicit cache is prefix-based, so a systemInstruction
// that grows by one reminder per turn invalidates the whole cached conversation behind it.
func TestToGeminiChatCompletionRequest_MidConversationSystemInlined(t *testing.T) {
	str := func(role schemas.ChatMessageRole, text string) schemas.ChatMessage {
		return schemas.ChatMessage{Role: role, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(text)}}
	}
	const reminder = "<total_tokens>15000000 tokens left</total_tokens>"
	turn1 := []schemas.ChatMessage{
		str(schemas.ChatMessageRoleSystem, "You are Claude Code."),
		str(schemas.ChatMessageRoleUser, "first user turn"),
		str(schemas.ChatMessageRoleSystem, "Available agent types for the Agent tool: claude, Explore, Plan."),
		str(schemas.ChatMessageRoleAssistant, "ok"),
		str(schemas.ChatMessageRoleUser, "second user turn"),
		str(schemas.ChatMessageRoleSystem, reminder),
	}
	turn2 := append(append([]schemas.ChatMessage{}, turn1...),
		str(schemas.ChatMessageRoleAssistant, "done"),
		str(schemas.ChatMessageRoleUser, "third user turn"),
		str(schemas.ChatMessageRoleSystem, reminder),
	)
	convert := func(msgs []schemas.ChatMessage) *gemini.GeminiGenerationRequest {
		req, err := gemini.ToGeminiChatCompletionRequest(&schemas.BifrostContext{}, &schemas.BifrostChatRequest{
			Provider: schemas.Gemini, Model: "gemini-2.5-flash", Input: msgs,
		})
		require.NoError(t, err)
		return req
	}
	r1, r2 := convert(turn1), convert(turn2)

	require.NotNil(t, r1.SystemInstruction)
	require.Len(t, r1.SystemInstruction.Parts, 1, "only the leading system prompt belongs in systemInstruction")
	assert.Equal(t, r1.SystemInstruction, r2.SystemInstruction, "turn N+1 must not grow systemInstruction with the new trailing reminder")

	require.GreaterOrEqual(t, len(r2.Contents), len(r1.Contents))
	assert.Equal(t, r1.Contents, r2.Contents[:len(r1.Contents)], "contents of turn N must be a prefix of turn N+1")

	inline := 0
	for _, c := range r2.Contents {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "<system-reminder>\n<total_tokens>") {
				assert.Equal(t, "user", c.Role, "inlined reminder must be a user turn")
				inline++
			}
		}
	}
	assert.Equal(t, 2, inline, "both trailing reminders must be inlined in place, none hoisted")
}

// TestToGeminiRequest_ReminderInsideParallelResponses covers a mid-conversation reminder that
// lands between the tool results of a parallel call (calls A and B, A's result, a reminder,
// B's result). Inlining it in place flushed A's response, inserted the text turn, then emitted
// B's response on its own; Gemini needs the parallel function responses together, so the
// conversation became an invalid request. The reminder must follow the whole group, on both the
// Chat Completions and the Responses path.
func TestToGeminiRequest_ReminderInsideParallelResponses(t *testing.T) {
	const reminder = "Remember the plan."
	// describe renders contents as role:[part kinds] so the expected layout reads at a glance.
	describe := func(contents []gemini.Content) []string {
		var out []string
		for _, c := range contents {
			var kinds []string
			for _, p := range c.Parts {
				switch {
				case p.FunctionCall != nil:
					kinds = append(kinds, "call:"+p.FunctionCall.ID)
				case p.FunctionResponse != nil:
					kinds = append(kinds, "response:"+p.FunctionResponse.ID)
				case strings.Contains(p.Text, reminder):
					kinds = append(kinds, "reminder")
				case p.Text != "":
					kinds = append(kinds, "text")
				}
			}
			out = append(out, c.Role+":["+strings.Join(kinds, ",")+"]")
		}
		return out
	}

	t.Run("chat", func(t *testing.T) {
		text := func(role schemas.ChatMessageRole, s string) schemas.ChatMessage {
			return schemas.ChatMessage{Role: role, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(s)}}
		}
		call := func(id, name string) schemas.ChatAssistantMessageToolCall {
			return schemas.ChatAssistantMessageToolCall{
				ID:       schemas.Ptr(id),
				Type:     schemas.Ptr("function"),
				Function: schemas.ChatAssistantMessageToolCallFunction{Name: schemas.Ptr(name), Arguments: "{}"},
			}
		}
		result := func(id string) schemas.ChatMessage {
			return schemas.ChatMessage{
				Role:            schemas.ChatMessageRoleTool,
				ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: schemas.Ptr(id)},
				Content:         &schemas.ChatMessageContent{ContentStr: schemas.Ptr(`{"ok":true}`)},
			}
		}
		assistant := schemas.ChatMessage{
			Role:                 schemas.ChatMessageRoleAssistant,
			ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{call("a", "get_a"), call("b", "get_b")}},
		}
		convert := func(msgs ...schemas.ChatMessage) []string {
			req, err := gemini.ToGeminiChatCompletionRequest(&schemas.BifrostContext{}, &schemas.BifrostChatRequest{
				Provider: schemas.Gemini, Model: "gemini-2.5-flash", Input: msgs,
			})
			require.NoError(t, err)
			return describe(req.Contents)
		}
		user := text(schemas.ChatMessageRoleUser, "go")
		sys := text(schemas.ChatMessageRoleSystem, reminder)

		assert.Equal(t,
			[]string{"user:[text]", "model:[call:a,call:b]", "user:[response:a,response:b]", "user:[reminder]"},
			convert(user, assistant, result("a"), sys, result("b")),
			"a reminder between parallel responses must follow the group")
		assert.Equal(t,
			[]string{"user:[text]", "model:[call:a,call:b]", "user:[response:a,response:b]", "user:[reminder]", "user:[text]"},
			convert(user, assistant, sys, result("a"), result("b"), text(schemas.ChatMessageRoleUser, "next")),
			"a reminder ahead of the responses must follow the group")
		assert.Equal(t,
			[]string{"user:[text]", "model:[call:a,call:b]", "user:[response:a,response:b]", "user:[reminder]", "user:[text]"},
			convert(user, assistant, result("a"), result("b"), sys, text(schemas.ChatMessageRoleUser, "next")),
			"a reminder after the group stays in place")

		// A reminder deferred inside the group and a newer one after it keep their order,
		// so the newer instruction is not followed by the one it replaces.
		req, err := gemini.ToGeminiChatCompletionRequest(&schemas.BifrostContext{}, &schemas.BifrostChatRequest{
			Provider: schemas.Gemini, Model: "gemini-2.5-flash",
			Input: []schemas.ChatMessage{user, assistant, result("a"), text(schemas.ChatMessageRoleSystem, reminder+" #1"), result("b"),
				text(schemas.ChatMessageRoleSystem, reminder+" #2"), text(schemas.ChatMessageRoleUser, "next")},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"#1", "#2"}, reminderOrder(req.Contents, reminder), "chat reminders out of order")
	})

	t.Run("responses", func(t *testing.T) {
		msg := func(role schemas.ResponsesMessageRoleType, s string) schemas.ResponsesMessage {
			return schemas.ResponsesMessage{
				Type:    schemas.Ptr(schemas.ResponsesMessageTypeMessage),
				Role:    schemas.Ptr(role),
				Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(s)},
			}
		}
		call := func(id, name string) schemas.ResponsesMessage {
			return schemas.ResponsesMessage{
				Type:                 schemas.Ptr(schemas.ResponsesMessageTypeFunctionCall),
				ResponsesToolMessage: &schemas.ResponsesToolMessage{CallID: schemas.Ptr(id), Name: schemas.Ptr(name), Arguments: schemas.Ptr("{}")},
			}
		}
		output := func(id string) schemas.ResponsesMessage {
			return schemas.ResponsesMessage{
				Type: schemas.Ptr(schemas.ResponsesMessageTypeFunctionCallOutput),
				ResponsesToolMessage: &schemas.ResponsesToolMessage{
					CallID: schemas.Ptr(id),
					Output: &schemas.ResponsesToolMessageOutputStruct{ResponsesToolCallOutputStr: schemas.Ptr(`{"ok":true}`)},
				},
			}
		}
		convert := func(msgs ...schemas.ResponsesMessage) []string {
			req, err := gemini.ToGeminiResponsesRequest(&schemas.BifrostContext{}, &schemas.BifrostResponsesRequest{
				Provider: schemas.Gemini, Model: "gemini-2.5-flash", Input: msgs,
			})
			require.NoError(t, err)
			return describe(req.Contents)
		}
		user := msg(schemas.ResponsesInputMessageRoleUser, "go")
		sys := msg(schemas.ResponsesInputMessageRoleSystem, reminder)

		got := convert(user, call("a", "get_a"), call("b", "get_b"), output("a"), sys, output("b"))
		require.NotEmpty(t, got)
		assert.Equal(t, []string{"user:[response:a,response:b]", "user:[reminder]"}, got[len(got)-2:],
			"a reminder between parallel responses must follow the group: %v", got)

		req, err := gemini.ToGeminiResponsesRequest(&schemas.BifrostContext{}, &schemas.BifrostResponsesRequest{
			Provider: schemas.Gemini, Model: "gemini-2.5-flash",
			Input: []schemas.ResponsesMessage{user, call("a", "get_a"), call("b", "get_b"), output("a"),
				msg(schemas.ResponsesInputMessageRoleSystem, reminder+" #1"), output("b"),
				msg(schemas.ResponsesInputMessageRoleSystem, reminder+" #2"), msg(schemas.ResponsesInputMessageRoleUser, "next")},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"#1", "#2"}, reminderOrder(req.Contents, reminder), "responses reminders out of order")
	})
}

// reminderOrder lists the "#n" tags of the reminder parts in contents, in order.
func reminderOrder(contents []gemini.Content, reminder string) []string {
	var order []string
	for _, c := range contents {
		for _, p := range c.Parts {
			if i := strings.Index(p.Text, reminder+" #"); i >= 0 {
				order = append(order, strings.TrimSpace(p.Text[i+len(reminder)+1:])[:2])
			}
		}
	}
	return order
}

// Chat Completions parity: the conversion runs once per retry/fallback attempt on
// the same Bifrost request and removes safety_settings, cached_content and labels
// from the outbound ExtraParams. It used to alias the source map, so the second
// attempt was sent without any of them.
func TestToGeminiChatCompletionRequest_ExtraParamsSurviveRetries(t *testing.T) {
	bifrostReq := &schemas.BifrostChatRequest{
		Provider: schemas.Vertex,
		Model:    "gemini-2.5-flash",
		Input: []schemas.ChatMessage{
			{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{
					ContentStr: schemas.Ptr("Hello"),
				},
			},
		},
		Params: &schemas.ChatParameters{
			ExtraParams: map[string]interface{}{
				"safety_settings": []interface{}{
					map[string]interface{}{
						"category":  "HARM_CATEGORY_HARASSMENT",
						"threshold": "BLOCK_NONE",
					},
				},
				"cached_content":     "cachedContents/abc123",
				"labels":             map[string]interface{}{"team": "platform"},
				"custom_passthrough": "keep-me",
			},
		},
	}

	for attempt := 1; attempt <= 3; attempt++ {
		geminiReq, err := gemini.ToGeminiChatCompletionRequest(nil, bifrostReq)
		require.NoError(t, err, "attempt %d", attempt)
		require.NotNil(t, geminiReq, "attempt %d", attempt)

		require.Len(t, geminiReq.SafetySettings, 1, "attempt %d", attempt)
		assert.Equal(t, "HARM_CATEGORY_HARASSMENT", geminiReq.SafetySettings[0].Category, "attempt %d", attempt)
		assert.Equal(t, "BLOCK_NONE", geminiReq.SafetySettings[0].Threshold, "attempt %d", attempt)
		assert.Equal(t, "cachedContents/abc123", geminiReq.CachedContent, "attempt %d", attempt)
		assert.Equal(t, map[string]string{"team": "platform"}, geminiReq.Labels, "attempt %d", attempt)
		assert.Equal(t, map[string]interface{}{"custom_passthrough": "keep-me"}, geminiReq.GetExtraParams(), "attempt %d", attempt)
	}

	// The Bifrost request itself must be left intact for the next attempt.
	assert.Contains(t, bifrostReq.Params.ExtraParams, "safety_settings")
	assert.Contains(t, bifrostReq.Params.ExtraParams, "cached_content")
	assert.Contains(t, bifrostReq.Params.ExtraParams, "labels")
	assert.Len(t, bifrostReq.Params.ExtraParams, 4)
}

func TestToGeminiChatCompletionRequest_NMapsToCandidateCount(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		want    int32
		wantErr string
	}{
		{name: "normal", n: 3, want: 3},
		{name: "maximum int32", n: math.MaxInt32, want: math.MaxInt32},
		{name: "above maximum int32", n: math.MaxInt32 + 1, wantErr: "n must be between 1 and 2147483647"},
		{name: "zero", n: 0, wantErr: "n must be between 1 and 2147483647"},
		{name: "negative", n: -1, wantErr: "n must be between 1 and 2147483647"},
	}

	for _, provider := range []schemas.ModelProvider{schemas.Gemini, schemas.Vertex} {
		for _, tt := range cases {
			t.Run(string(provider)+"/"+tt.name, func(t *testing.T) {
				result, err := gemini.ToGeminiChatCompletionRequest(nil, &schemas.BifrostChatRequest{
					Provider: provider,
					Model:    "gemini-2.5-flash",
					Input: []schemas.ChatMessage{{
						Role:    schemas.ChatMessageRoleUser,
						Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Hello")},
					}},
					Params: &schemas.ChatParameters{N: schemas.Ptr(tt.n)},
				})
				if tt.wantErr != "" {
					require.ErrorContains(t, err, tt.wantErr)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Equal(t, tt.want, result.GenerationConfig.CandidateCount)
			})
		}
	}
}

// chatToolResultHasRefKey reports whether any object at any depth of raw has a "$ref" key.
// Gemini reads {"$ref": "<displayName>"} inside function_response.response as a pointer to
// a multimodal part, so a caller's JSON Schema / OpenAPI tool output must never reach the
// wire with one (#7694).
func chatToolResultHasRefKey(raw []byte) bool {
	var found bool
	var walk func(v gjson.Result)
	walk = func(v gjson.Result) {
		v.ForEach(func(key, value gjson.Result) bool {
			if v.IsObject() && key.String() == "$ref" {
				found = true
				return false
			}
			if value.IsObject() || value.IsArray() {
				walk(value)
			}
			return !found
		})
	}
	walk(gjson.ParseBytes(raw))
	return found
}

// TestToGeminiChatCompletionRequest_ToolResultRefKeyStaysOpaque pins that a role:"tool"
// message whose content is a JSON object containing a "$ref" key anywhere is sent to
// Gemini as opaque string content ({"content": "<verbatim text>"}), never as a structured
// function_response.response. Gemini reserves "$ref" there for multimodal part references
// and rejects the request with 400 "does not match to a display_name in the
// function_response.parts" otherwise (#7694). Tool output without "$ref" keeps the
// structured fast path.
func TestToGeminiChatCompletionRequest_ToolResultRefKeyStaysOpaque(t *testing.T) {
	const refOutput = `{"schema":{"$ref":"#/components/schemas/SOM_computer_post_response"}}`
	const plainOutput = `{"temperature":22,"condition":"sunny"}`

	build := func(content *schemas.ChatMessageContent) *schemas.BifrostChatRequest {
		return &schemas.BifrostChatRequest{
			Provider: schemas.Gemini,
			Model:    "gemini-flash-latest",
			Input: []schemas.ChatMessage{
				{
					Role:    schemas.ChatMessageRoleUser,
					Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("Fetch the spec, then reply ok.")},
				},
				{
					Role: schemas.ChatMessageRoleAssistant,
					ChatAssistantMessage: &schemas.ChatAssistantMessage{
						ToolCalls: []schemas.ChatAssistantMessageToolCall{{
							ID:   schemas.Ptr("c1"),
							Type: schemas.Ptr("function"),
							Function: schemas.ChatAssistantMessageToolCallFunction{
								Name:      schemas.Ptr("bash"),
								Arguments: `{"command":"cat spec.json"}`,
							},
						}},
					},
				},
				{
					Role:            schemas.ChatMessageRoleTool,
					ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: schemas.Ptr("c1")},
					Content:         content,
				},
			},
		}
	}

	functionResponse := func(t *testing.T, req *schemas.BifrostChatRequest) []byte {
		t.Helper()
		out, err := gemini.ToGeminiChatCompletionRequest(nil, req)
		require.NoError(t, err)
		require.Len(t, out.Contents, 3)
		require.Len(t, out.Contents[2].Parts, 1)
		require.NotNil(t, out.Contents[2].Parts[0].FunctionResponse)
		return out.Contents[2].Parts[0].FunctionResponse.Response
	}

	t.Run("string content with nested $ref is wrapped as opaque text", func(t *testing.T) {
		resp := functionResponse(t, build(&schemas.ChatMessageContent{ContentStr: schemas.Ptr(refOutput)}))
		assert.False(t, chatToolResultHasRefKey(resp), "function_response.response must not carry a $ref key: %s", resp)
		assert.Equal(t, refOutput, gjson.GetBytes(resp, "content").String(), "tool text must survive verbatim under \"content\"")
	})

	t.Run("text block content with nested $ref is wrapped as opaque text", func(t *testing.T) {
		resp := functionResponse(t, build(&schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
			Type: schemas.ChatContentBlockTypeText,
			Text: schemas.Ptr(refOutput),
		}}}))
		assert.False(t, chatToolResultHasRefKey(resp), "function_response.response must not carry a $ref key: %s", resp)
		assert.Equal(t, refOutput, gjson.GetBytes(resp, "content").String(), "tool text must survive verbatim under \"content\"")
	})

	t.Run("top-level $ref is wrapped as opaque text", func(t *testing.T) {
		const topLevel = `{"$ref":"#/definitions/Thing"}`
		resp := functionResponse(t, build(&schemas.ChatMessageContent{ContentStr: schemas.Ptr(topLevel)}))
		assert.False(t, chatToolResultHasRefKey(resp), "function_response.response must not carry a $ref key: %s", resp)
		assert.Equal(t, topLevel, gjson.GetBytes(resp, "content").String())
	})

	t.Run("unicode-escaped $ref key is wrapped as opaque text", func(t *testing.T) {
		// JSON allows any character of a key to be escaped; "$r\u0065f" decodes to "$ref".
		escaped := `{"schema":{"$r\u0065f":"#/x"}}`
		resp := functionResponse(t, build(&schemas.ChatMessageContent{ContentStr: schemas.Ptr(escaped)}))
		assert.False(t, chatToolResultHasRefKey(resp), "function_response.response must not carry a $ref key: %s", resp)
		assert.Equal(t, escaped, gjson.GetBytes(resp, "content").String())
	})

	t.Run("JSON object without $ref keeps the structured fast path", func(t *testing.T) {
		resp := functionResponse(t, build(&schemas.ChatMessageContent{ContentStr: schemas.Ptr(plainOutput)}))
		assert.JSONEq(t, plainOutput, string(resp), "plain JSON tool output must still be forwarded as a structured object")
	})
}
