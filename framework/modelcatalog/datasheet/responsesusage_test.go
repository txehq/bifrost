package datasheet

import (
	"reflect"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// TestResponsesUsageKeepsEveryInputDetail fails when responsesUsageToBifrostUsage drops an input
// detail the chat prompt details also carry (TXE-7357: video_tokens was dropped, so a video sent
// through /v1/responses was billed at the text rate). Every field the two structs share by name
// and type is set to its own value and must survive the conversion.
func TestResponsesUsageKeepsEveryInputDetail(t *testing.T) {
	in := &schemas.ResponsesResponseInputTokens{}
	src := reflect.ValueOf(in).Elem()
	dstType := reflect.TypeOf(schemas.ChatPromptTokensDetails{})
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
	out := responsesUsageToBifrostUsage(&schemas.ResponsesResponseUsage{InputTokensDetails: in}).PromptTokensDetails
	if out == nil {
		t.Fatal("input details dropped")
	}
	dst := reflect.ValueOf(out).Elem()
	for i := 0; i < src.NumField(); i++ {
		f := src.Type().Field(i)
		if df, ok := dstType.FieldByName(f.Name); ok && df.Type == f.Type && f.Type.Kind() == reflect.Int {
			if got, want := dst.FieldByName(f.Name).Int(), src.Field(i).Int(); got != want {
				t.Errorf("responsesUsageToBifrostUsage drops InputTokensDetails.%s: got %d, want %d", f.Name, got, want)
			}
		}
	}
}
