package logging

import (
	"reflect"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TestRecalculatedResponsesUsageKeepsEveryInputDetail fails when the Responses rebuild a cost
// recalculation prices drops an input detail the logged usage carries (TXE-7357: video tokens
// were dropped, so recalculating a video log repriced it at the text rate). Every field the chat
// prompt details and the Responses input details share by name and type is set to its own value
// and must reach the rebuilt response, for the ordinary and the streamed request type.
func TestRecalculatedResponsesUsageKeepsEveryInputDetail(t *testing.T) {
	details := &schemas.ChatPromptTokensDetails{}
	src := reflect.ValueOf(details).Elem()
	dstType := reflect.TypeOf(schemas.ResponsesResponseInputTokens{})
	shared := 0
	for i := 0; i < src.NumField(); i++ {
		f := src.Type().Field(i)
		if df, ok := dstType.FieldByName(f.Name); ok && df.Type == f.Type && f.Type.Kind() == reflect.Int {
			src.Field(i).SetInt(int64(100 + i))
			shared++
		}
	}
	if shared < 7 {
		t.Fatalf("only %d shared int fields found; the structs changed shape, update this check", shared)
	}
	for _, rt := range []schemas.RequestType{schemas.ResponsesRequest, schemas.ResponsesStreamRequest} {
		resp := buildResponseForRequestType(rt, &schemas.BifrostLLMUsage{PromptTokens: 1000, PromptTokensDetails: details},
			schemas.BifrostResponseExtraFields{}, servedTier{})
		if resp == nil || resp.ResponsesResponse == nil || resp.ResponsesResponse.Usage == nil || resp.ResponsesResponse.Usage.InputTokensDetails == nil {
			t.Fatalf("%s: no Responses input details rebuilt", rt)
		}
		dst := reflect.ValueOf(resp.ResponsesResponse.Usage.InputTokensDetails).Elem()
		for i := 0; i < src.NumField(); i++ {
			f := src.Type().Field(i)
			if df, ok := dstType.FieldByName(f.Name); ok && df.Type == f.Type && f.Type.Kind() == reflect.Int {
				if got, want := dst.FieldByName(f.Name).Int(), src.Field(i).Int(); got != want {
					t.Errorf("%s: the rebuild drops PromptTokensDetails.%s: got %d, want %d", rt, f.Name, got, want)
				}
			}
		}
	}
}
