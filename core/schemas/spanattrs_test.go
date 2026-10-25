package schemas

import (
	"reflect"
	"testing"
)

// A Responses span exports its stop_reason as finish_reasons, as the chat path
// does, so a refusal on /v1/responses stays visible to connectors (upstream
// #7205, TXE-5038). The typed renderer dropped it for both Responses families.
func TestResponseAttributesCarryResponsesFinishReasons(t *testing.T) {
	for _, requestType := range []RequestType{ResponsesRequest, ResponsesStreamRequest} {
		d := &LLMSpanData{RequestType: requestType, ResponseID: "resp_1", FinishReasons: []string{"refusal"}}
		got, ok := d.ResponseAttributes()[AttrFinishReasons]
		if !ok {
			t.Fatalf("%s: response attributes have no %s", requestType, AttrFinishReasons)
		}
		if !reflect.DeepEqual(got, []string{"refusal"}) {
			t.Fatalf("%s: %s = %#v, want [refusal]", requestType, AttrFinishReasons, got)
		}
	}
}

// A text completion sent without parameters (valid through the Go SDK) still
// exports its prompt; the typed renderer used to key the whole branch on the
// parameters and dropped gen_ai.input.text.
func TestAttributesCarryTextPromptWithoutParams(t *testing.T) {
	prompt := "say hi"
	d := &LLMSpanData{RequestType: TextCompletionRequest, TextPrompt: &TextCompletionInput{PromptStr: &prompt}}
	if got := d.Attributes()[AttrInputText]; got != prompt {
		t.Fatalf("%s = %#v, want %q", AttrInputText, got, prompt)
	}
}
