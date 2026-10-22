package gemini

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	schemas "github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A thoughtSignature can ride on an executableCode or codeExecutionResult part. The
// code-execution handlers consume those parts, so the signature has to be carried on as
// an encrypted reasoning item, as the toolCall/toolResponse handlers do, or Responses
// replay and the native GenAI stream lose it.
func TestGeminiCodeExecutionKeepsThoughtSignatures(t *testing.T) {
	codeSig := base64.StdEncoding.EncodeToString([]byte("code-signature"))
	resultSig := base64.StdEncoding.EncodeToString([]byte("result-signature"))
	chunks := []string{
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"executableCode":{"language":"PYTHON","code":"print(6*7)"},"thoughtSignature":"` + codeSig + `"}]}}],"modelVersion":"gemini-3.6-flash"}`,
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"42\n"},"thoughtSignature":"` + resultSig + `"}]}}],"modelVersion":"gemini-3.6-flash"}`,
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"The answer is 42."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-3.6-flash"}`,
	}

	t.Run("streaming", func(t *testing.T) {
		state := acquireGeminiResponsesStreamState()
		defer releaseGeminiResponsesStreamState(state)
		outState := NewBifrostToGeminiStreamState()

		var encrypted []string
		var emitted []string
		codeItems := 0
		seq := 0
		for _, c := range chunks {
			var resp GenerateContentResponse
			require.NoError(t, json.Unmarshal([]byte(c), &resp))
			events, bifrostErr := resp.ToBifrostResponsesStream(seq, state)
			require.Nil(t, bifrostErr)
			for _, e := range events {
				if e.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && e.Item != nil && e.Item.Type != nil {
					switch *e.Item.Type {
					case schemas.ResponsesMessageTypeReasoning:
						if e.Item.ResponsesReasoning != nil && e.Item.ResponsesReasoning.EncryptedContent != nil {
							encrypted = append(encrypted, *e.Item.ResponsesReasoning.EncryptedContent)
						}
					case schemas.ResponsesMessageTypeCodeInterpreterCall:
						codeItems++
					}
				}
				out := ToGeminiResponsesStreamResponse(e, outState)
				if out == nil {
					continue
				}
				for _, cand := range out.Candidates {
					if cand.Content == nil {
						continue
					}
					for _, part := range cand.Content.Parts {
						encoded, err := json.Marshal(part)
						require.NoError(t, err)
						emitted = append(emitted, string(encoded))
					}
				}
			}
			seq += len(events)
		}

		assert.Equal(t, 1, codeItems, "code and result still close one code_interpreter_call")
		assert.Equal(t, []string{codeSig, resultSig}, encrypted,
			"each code-execution signature must reach Responses clients in order")

		var nativeSigs []string
		for _, p := range emitted {
			if sig := gjson.Get(p, "thoughtSignature"); sig.Exists() {
				nativeSigs = append(nativeSigs, sig.String())
			}
		}
		assert.Equal(t, []string{codeSig, resultSig}, nativeSigs,
			"the native GenAI stream must re-emit both signatures once each: %v", emitted)
	})

	t.Run("non-streaming", func(t *testing.T) {
		var resp GenerateContentResponse
		require.NoError(t, json.Unmarshal([]byte(`{"candidates":[{"index":0,"finishReason":"STOP","content":{"role":"model","parts":[`+
			`{"executableCode":{"language":"PYTHON","code":"print(6*7)"},"thoughtSignature":"`+codeSig+`"},`+
			`{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"42\n"},"thoughtSignature":"`+resultSig+`"},`+
			`{"text":"The answer is 42."}]}}],"modelVersion":"gemini-3.6-flash"}`), &resp))
		br := resp.ToResponsesBifrostResponsesResponse()

		var encrypted []string
		codeItems := 0
		for _, item := range br.Output {
			if item.Type == nil {
				continue
			}
			switch *item.Type {
			case schemas.ResponsesMessageTypeReasoning:
				if item.ResponsesReasoning != nil && item.ResponsesReasoning.EncryptedContent != nil {
					encrypted = append(encrypted, *item.ResponsesReasoning.EncryptedContent)
				}
			case schemas.ResponsesMessageTypeCodeInterpreterCall:
				codeItems++
				require.NotNil(t, item.ResponsesToolMessage.ResponsesCodeInterpreterToolCall)
				assert.NotEmpty(t, item.ResponsesToolMessage.ResponsesCodeInterpreterToolCall.Outputs,
					"the result still folds onto its call")
			}
		}
		assert.Equal(t, 1, codeItems)
		assert.Equal(t, []string{codeSig, resultSig}, encrypted)
	})
}
