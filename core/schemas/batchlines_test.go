package schemas

import (
	"errors"
	"testing"
)

func batchLine(t *testing.T, line string) BatchRequestItem {
	t.Helper()
	var item BatchRequestItem
	if err := Unmarshal([]byte(line), &item); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	return item
}

// A chat line becomes the request the sync chat route builds from the same body: messages,
// the chat parameters, max_tokens read as max_completion_tokens, and every field the unified
// request does not model kept as an extra param.
func TestBatchRequestItemToBifrostChatRequest(t *testing.T) {
	item := batchLine(t, `{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions","body":{"model":"some-model","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}],"max_tokens":128,"temperature":0.3,"reasoning_effort":"low","response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"vendor_switch":{"on":true}}}`)
	request, err := item.ToBifrostChatRequest(Gemini, "batch-model")
	if err != nil {
		t.Fatalf("ToBifrostChatRequest: %v", err)
	}
	if request.Provider != Gemini || request.Model != "batch-model" {
		t.Errorf("provider/model = %s/%s, want the batch's gemini/batch-model", request.Provider, request.Model)
	}
	if len(request.Input) != 2 || request.Input[0].Role != ChatMessageRoleSystem || request.Input[1].Role != ChatMessageRoleUser {
		t.Errorf("messages = %+v, want system then user", request.Input)
	}
	params := request.Params
	if params.MaxCompletionTokens == nil || *params.MaxCompletionTokens != 128 {
		t.Errorf("MaxCompletionTokens = %v, want 128 from max_tokens", params.MaxCompletionTokens)
	}
	if params.Temperature == nil || *params.Temperature != 0.3 {
		t.Errorf("Temperature = %v, want 0.3", params.Temperature)
	}
	if params.Reasoning == nil || params.Reasoning.Effort == nil || *params.Reasoning.Effort != "low" {
		t.Errorf("Reasoning = %+v, want effort low from reasoning_effort", params.Reasoning)
	}
	if params.ResponseFormat == nil || len(params.Tools) != 1 {
		t.Errorf("response_format or tools were dropped: %+v", params)
	}
	if _, kept := params.ExtraParams["vendor_switch"]; !kept {
		t.Errorf("ExtraParams = %v, want the unmodelled vendor_switch", params.ExtraParams)
	}
	for _, modelled := range []string{"max_tokens", "messages", "model", "temperature", "tools", "response_format", "reasoning_effort"} {
		if _, extra := params.ExtraParams[modelled]; extra {
			t.Errorf("%s is a unified field and must not be an extra param: %v", modelled, params.ExtraParams)
		}
	}

	// max_completion_tokens wins over the legacy max_tokens, as on the sync route.
	both := batchLine(t, `{"custom_id":"l","body":{"messages":[{"role":"user","content":"Hi"}],"max_tokens":10,"max_completion_tokens":20}}`)
	request, err = both.ToBifrostChatRequest(Gemini, "m")
	if err != nil || *request.Params.MaxCompletionTokens != 20 {
		t.Errorf("max_completion_tokens = %v (err %v), want 20", request.Params.MaxCompletionTokens, err)
	}

	// Anthropic's name for the body is read too.
	viaParams := batchLine(t, `{"custom_id":"l","params":{"messages":[{"role":"user","content":"Hi"}]}}`)
	if _, err := viaParams.ToBifrostChatRequest(Gemini, "m"); err != nil {
		t.Errorf("a line with params instead of body: %v", err)
	}

	for name, line := range map[string]string{
		"no body":     `{"custom_id":"l"}`,
		"no messages": `{"custom_id":"l","body":{"model":"m"}}`,
		"bad params":  `{"custom_id":"l","body":{"messages":[{"role":"user","content":"Hi"}],"temperature":"warm"}}`,
		"conflicting": `{"custom_id":"l","body":{"messages":[{"role":"user","content":"Hi"}],"reasoning_effort":"low","reasoning":{"effort":"high"}}}`,
	} {
		if _, err := batchLine(t, line).ToBifrostChatRequest(Gemini, "m"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestBatchRequestItemToBifrostImageGenerationRequest(t *testing.T) {
	item := batchLine(t, `{"custom_id":"img-1","url":"/v1/images/generations","body":{"model":"some-image-model","prompt":"A lighthouse","n":2,"size":"1024x1024","aspect_ratio":"16:9","vendor_knob":7}}`)
	request, err := item.ToBifrostImageGenerationRequest(Gemini, "batch-image-model")
	if err != nil {
		t.Fatalf("ToBifrostImageGenerationRequest: %v", err)
	}
	if request.Model != "batch-image-model" || request.Input.Prompt != "A lighthouse" {
		t.Errorf("model/prompt = %s/%q", request.Model, request.Input.Prompt)
	}
	if request.Params.N == nil || *request.Params.N != 2 || request.Params.Size == nil || *request.Params.Size != "1024x1024" {
		t.Errorf("params = %+v, want n 2 and size 1024x1024", request.Params)
	}
	if _, kept := request.Params.ExtraParams["vendor_knob"]; !kept || len(request.Params.ExtraParams) != 1 {
		t.Errorf("ExtraParams = %v, want only vendor_knob", request.Params.ExtraParams)
	}
	if _, err := batchLine(t, `{"custom_id":"img-2","body":{"model":"m"}}`).ToBifrostImageGenerationRequest(Gemini, "m"); err == nil {
		t.Error("a line with no prompt: no error")
	}
}

func TestBatchRequestItemEndpointAndLineError(t *testing.T) {
	if got := (BatchRequestItem{URL: " /v1/images/generations "}).Endpoint(BatchEndpointChatCompletions); got != BatchEndpointImageGeneration {
		t.Errorf("Endpoint = %q, want the line's own url", got)
	}
	if got := (BatchRequestItem{}).Endpoint(BatchEndpointChatCompletions); got != BatchEndpointChatCompletions {
		t.Errorf("Endpoint = %q, want the batch's endpoint", got)
	}
	var lineErr *BatchLineError
	err := error(&BatchLineError{Index: 3, CustomID: "line-4", Param: "url", Message: "cannot run"})
	if !errors.As(err, &lineErr) || err.Error() != `batch request item 3 (custom_id "line-4"): cannot run` {
		t.Errorf("BatchLineError = %q", err.Error())
	}
}

// An embeddings line is read as the sync /v1/embeddings route reads the same body: every input
// shape that route accepts, the embedding parameters, and unmodelled fields as extra params.
func TestBatchRequestItemToBifrostEmbeddingRequest(t *testing.T) {
	line := func(body string) BatchRequestItem {
		var item BatchRequestItem
		if err := Unmarshal([]byte(`{"custom_id":"e-1","url":"/v1/embeddings","body":`+body+`}`), &item); err != nil {
			t.Fatalf("line: %v", err)
		}
		return item
	}

	request, err := line(`{"model":"gemini-embedding-001","input":["a","b"],"dimensions":256,"task_type":"RETRIEVAL_QUERY","encoding_format":"float","vendor_only":1}`).ToBifrostEmbeddingRequest(Gemini, "gemini-embedding-001")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if request.Provider != Gemini || request.Model != "gemini-embedding-001" {
		t.Fatalf("provider/model = %s/%s", request.Provider, request.Model)
	}
	if got := EmbeddingInput(request.Input).Texts(); len(got) != 2 || got[0] != "a" || got[1] != "b" || len(request.Input) != 2 {
		t.Fatalf("input = %v (%d items)", got, len(request.Input))
	}
	if request.Params.Dimensions == nil || *request.Params.Dimensions != 256 || request.Params.TaskType == nil || *request.Params.TaskType != "RETRIEVAL_QUERY" {
		t.Fatalf("params = %+v", request.Params)
	}
	if len(request.Params.ExtraParams) != 1 || request.Params.ExtraParams["vendor_only"] == nil {
		t.Fatalf("extra params = %v: only the unmodelled field belongs there", request.Params.ExtraParams)
	}

	for name, tc := range map[string]struct {
		body  string
		items int
	}{
		"a string":                 {`{"input":"hello"}`, 1},
		"token ids":                {`{"input":[15339,1917]}`, 1},
		"several token inputs":     {`{"input":[[15339,1917],[9906]]}`, 2},
		"content parts":            {`{"input":[[{"type":"text","text":"a"}],[{"type":"text","text":"b"}]]}`, 2},
		"items with params":        {`{"input":[{"content":[{"type":"text","text":"a"}],"params":{"task_type":"RETRIEVAL_QUERY"}}]}`, 1},
		"the flat multimodal list": {`{"input":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, 1},
	} {
		request, err := line(tc.body).ToBifrostEmbeddingRequest(Gemini, "m")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(request.Input) != tc.items {
			t.Fatalf("%s: %d items, want %d", name, len(request.Input), tc.items)
		}
	}

	for name, body := range map[string]string{
		"no input":       `{"model":"m"}`,
		"an empty input": `{"input":[]}`,
		"an empty text":  `{"input":[[{"type":"text","text":""}]]}`,
	} {
		if _, err := line(body).ToBifrostEmbeddingRequest(Gemini, "m"); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	if _, err := (BatchRequestItem{CustomID: "e-2"}).ToBifrostEmbeddingRequest(Gemini, "m"); err == nil || err.Error() != "body is required" {
		t.Fatalf("no body: %v", err)
	}
}
