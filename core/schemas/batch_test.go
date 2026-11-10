package schemas

import (
	"fmt"
	"testing"
)

func TestBatchRequestCountsIsZero(t *testing.T) {
	if !(BatchRequestCounts{}).IsZero() {
		t.Fatalf("expected empty counts to be zero")
	}
	if (BatchRequestCounts{Total: 1}).IsZero() {
		t.Fatalf("expected total count to be non-zero")
	}
	if (BatchRequestCounts{Pending: 1}).IsZero() {
		t.Fatalf("expected provider-specific pending count to be non-zero")
	}
}

func TestBatchResultItemFailed(t *testing.T) {
	tests := []struct {
		name string
		item BatchResultItem
		want bool
	}{
		{
			name: "explicit error",
			item: BatchResultItem{Error: &BatchResultError{Message: "failed"}},
			want: true,
		},
		{
			name: "http status error",
			item: BatchResultItem{Response: &BatchResultResponse{StatusCode: 500}},
			want: true,
		},
		{
			name: "anthropic succeeded",
			item: BatchResultItem{Result: &BatchResultData{Type: "succeeded"}},
			want: false,
		},
		{
			name: "anthropic errored",
			item: BatchResultItem{Result: &BatchResultData{Type: "errored"}},
			want: true,
		},
		{
			name: "openai success",
			item: BatchResultItem{Response: &BatchResultResponse{StatusCode: 200}},
			want: false,
		},
		{
			// Gemini's inline path and Bedrock emit a bare item (no response, no
			// error) when a record produced no output. That is not a success.
			name: "no response, result, or error",
			item: BatchResultItem{CustomID: "req-1"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.Failed(); got != tt.want {
				t.Fatalf("Failed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBatchRequestCountsFromResults(t *testing.T) {
	counts := BatchRequestCountsFromResults([]BatchResultItem{
		{Response: &BatchResultResponse{StatusCode: 200}},
		{Response: &BatchResultResponse{StatusCode: 429}},
		{Result: &BatchResultData{Type: "succeeded"}},
		{Result: &BatchResultData{Type: "expired"}},
	})

	if counts.Total != 4 || counts.Completed != 2 || counts.Failed != 2 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
}

// A line's status decides whether it failed: an expired or cancelled line has no response
// to bill even when the vendor attaches no error object.
func TestBatchResultItemFailedReadsStatus(t *testing.T) {
	ok := &BatchResultResponse{StatusCode: 200, Body: map[string]any{}}
	cases := []struct {
		name string
		item BatchResultItem
		want bool
	}{
		{"succeeded", BatchResultItem{Status: BatchResultStatusSucceeded, Response: ok}, false},
		{"failed", BatchResultItem{Status: BatchResultStatusFailed, Response: ok}, true},
		{"expired", BatchResultItem{Status: BatchResultStatusExpired}, true},
		{"cancelled", BatchResultItem{Status: BatchResultStatusCancelled}, true},
		{"no status, a response", BatchResultItem{Response: ok}, false},
		{"no status, nothing", BatchResultItem{}, true},
	}
	for _, tc := range cases {
		if got := tc.item.Failed(); got != tc.want {
			t.Errorf("%s: Failed() = %v, want %v", tc.name, got, tc.want)
		}
	}
	counts := BatchRequestCountsFromResults([]BatchResultItem{cases[0].item, cases[1].item, cases[2].item, cases[3].item})
	if counts != (BatchRequestCounts{Total: 4, Completed: 1, Failed: 3}) {
		t.Errorf("counts = %+v, want 1 completed and 3 failed of 4", counts)
	}
}

// A batch line's usage becomes the unified usage of the line's operation, the shape the
// sync route returns; an endpoint with no unified usage type, or a body with no usage, is
// left alone.
func TestUnifyBatchResultUsage(t *testing.T) {
	chat := map[string]any{"usage": map[string]any{
		"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 60},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 5},
	}}
	usage := UnifyBatchResultUsage(BatchEndpointChatCompletions, chat)["usage"].(map[string]any)
	details := usage["prompt_tokens_details"].(map[string]any)
	if fmt.Sprint(details["cached_read_tokens"]) != "60" || fmt.Sprint(details["cached_tokens"]) != "60" {
		t.Errorf("prompt_tokens_details = %v, want cached_read_tokens 60 beside cached_tokens 60", details)
	}
	if fmt.Sprint(usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"]) != "5" {
		t.Errorf("completion_tokens_details = %v, want reasoning_tokens 5", usage["completion_tokens_details"])
	}

	responses := map[string]any{"usage": map[string]any{"input_tokens": 9, "output_tokens": 3, "total_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 4}}}
	usage = UnifyBatchResultUsage(BatchEndpointResponses, responses)["usage"].(map[string]any)
	if fmt.Sprint(usage["input_tokens"]) != "9" || fmt.Sprint(usage["output_tokens"]) != "3" {
		t.Errorf("responses usage = %v, want input_tokens 9 and output_tokens 3", usage)
	}

	untouched := map[string]any{"usage": map[string]any{"vendor_units": 3}}
	if got := UnifyBatchResultUsage(BatchEndpoint("/v1/audio/speech"), untouched)["usage"].(map[string]any); fmt.Sprint(got["vendor_units"]) != "3" {
		t.Errorf("an endpoint with no unified usage must keep the vendor's usage, got %v", got)
	}
	if got := UnifyBatchResultUsage(BatchEndpointChatCompletions, map[string]any{"id": "x"}); len(got) != 1 {
		t.Errorf("a body with no usage must be unchanged, got %v", got)
	}
}
