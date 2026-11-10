package handlers

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// TestResolveBatchProvider covers the three resolution paths introduced to make
// model optional on POST /v1/batches (OpenAI spec: model lives in the JSONL body).
func TestResolveBatchProvider(t *testing.T) {
	config := &lib.Config{}

	cases := []struct {
		name         string
		model        string
		header       string // x-model-provider; empty = unset
		query        string // ?provider=; empty = unset
		wantProvider string
		wantModel    string
		wantErrMsg   string // non-empty = error expected, substring match
	}{
		{
			name:         "model field: provider+model parsed",
			model:        "openai/gpt-4o-mini",
			wantProvider: "openai",
			wantModel:    "gpt-4o-mini",
		},
		{
			name:         "no model, x-model-provider header",
			header:       "openai",
			wantProvider: "openai",
			wantModel:    "",
		},
		{
			name:         "no model, ?provider= query param",
			query:        "anthropic",
			wantProvider: "anthropic",
			wantModel:    "",
		},
		{
			name:       "no model, no provider → error",
			wantErrMsg: "provider query parameter or x-model-provider header is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			if tc.header != "" {
				ctx.Request.Header.Set("x-model-provider", tc.header)
			}
			if tc.query != "" {
				ctx.QueryArgs().Set("provider", tc.query)
			}

			provider, modelName, err := resolveBatchProvider(ctx, config, tc.model)

			if tc.wantErrMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrMsg)
				}
				if !strings.Contains(err.Error(), tc.wantErrMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(provider) != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", provider, tc.wantProvider)
			}
			if modelName != tc.wantModel {
				t.Fatalf("modelName = %q, want %q", modelName, tc.wantModel)
			}
		})
	}
}

// A batch create names its work once: an uploaded file, a blob, or inline requests. No provider
// runs inline items beside a file (OpenAI, Azure and Bedrock submit the file and drop them,
// Gemini and Vertex refuse the mix), so the gateway refuses the mix before anything runs rather
// than let a client's inline items vanish.
func TestBatchCreateRefusesFileAndInlineRequestsTogether(t *testing.T) {
	h := &CompletionHandler{config: &lib.Config{}}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"file beside inline requests", `{"model":"openai/gpt-4o-mini","input_file_id":"file-x","endpoint":"/v1/chat/completions","requests":[{"custom_id":"a","body":{"model":"gpt-4o-mini"}}]}`, fasthttp.StatusBadRequest},
		{"blob beside inline requests", `{"model":"azure/gpt-4o-mini","input_blob":"https://blob.example/in.jsonl","endpoint":"/v1/chat/completions","requests":[{"custom_id":"a","body":{"model":"gpt-4o-mini"}}]}`, fasthttp.StatusBadRequest},
		{"nothing named", `{"model":"openai/gpt-4o-mini","endpoint":"/v1/chat/completions"}`, fasthttp.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &fasthttp.RequestCtx{}
			ctx.Request.Header.SetMethod(fasthttp.MethodPost)
			ctx.Request.Header.SetContentType("application/json")
			ctx.Request.SetBodyString(tc.body)

			h.batchCreate(ctx)

			if ctx.Response.StatusCode() != tc.want {
				t.Fatalf("status = %d, want %d: %s", ctx.Response.StatusCode(), tc.want, ctx.Response.Body())
			}
			if tc.name != "nothing named" && !strings.Contains(string(ctx.Response.Body()), "not both") {
				t.Fatalf("the refusal should say the inputs are exclusive: %s", ctx.Response.Body())
			}
		})
	}
}

// The unified route dropped output_expires_after before any provider saw it (contract C4,
// BF19). It is a known field now, so it reaches the Bifrost request and is not also sent as
// an extra param.
func TestBatchCreateCarriesOutputExpiresAfter(t *testing.T) {
	body := []byte(`{"model":"openai/gpt-4o-mini","endpoint":"/v1/chat/completions","completion_window":"24h",` +
		`"metadata":{"tokun_batch_id":"batch_1"},"output_expires_after":{"anchor":"created_at","seconds":86400},` +
		`"vendor_only":true,"requests":[{"custom_id":"line-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4o-mini"}}]}`)
	var req BatchCreateRequest
	if err := sonic.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	extraParams, err := extractExtraParams(body, batchCreateParamsKnownFields)
	if err != nil {
		t.Fatalf("extractExtraParams: %v", err)
	}
	got := req.toBifrostBatchCreateRequest(schemas.OpenAI, schemas.Ptr("gpt-4o-mini"), extraParams)

	if got.OutputExpiresAfter == nil || got.OutputExpiresAfter.Anchor != "created_at" || got.OutputExpiresAfter.Seconds != 86400 {
		t.Fatalf("OutputExpiresAfter = %+v, want {created_at 86400}", got.OutputExpiresAfter)
	}
	if got.Metadata["tokun_batch_id"] != "batch_1" {
		t.Fatalf("Metadata = %v, want tokun_batch_id", got.Metadata)
	}
	if _, dup := got.ExtraParams["output_expires_after"]; dup {
		t.Fatalf("output_expires_after is a unified field and must not also be an extra param: %v", got.ExtraParams)
	}
	if got.ExtraParams["vendor_only"] != true {
		t.Fatalf("ExtraParams = %v, want the unknown field vendor_only kept", got.ExtraParams)
	}
	if len(got.Requests) != 1 || got.Requests[0].CustomID != "line-1" || got.Requests[0].URL != "/v1/chat/completions" {
		t.Fatalf("Requests = %+v, want the inline line unchanged", got.Requests)
	}
}

// Results page by limit and after; no limit keeps the one-response behaviour, and a limit
// that is not a positive integer is refused rather than read as "everything".
func TestParseBatchResultsPaging(t *testing.T) {
	cases := []struct {
		query     string
		wantLimit int
		wantAfter string
		wantErr   bool
	}{
		{"provider=openai", 0, "", false},
		{"provider=openai&limit=500", 500, "", false},
		{"provider=openai&limit=500&after=djE6MToxMjM", 500, "djE6MToxMjM", false},
		{"provider=openai&after=djE6MToxMjM", 0, "djE6MToxMjM", false},
		{"provider=openai&limit=0", 0, "", true},
		{"provider=openai&limit=-3", 0, "", true},
		{"provider=openai&limit=many", 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			var args fasthttp.Args
			args.Parse(tc.query)
			limit, after, err := parseBatchResultsPaging(&args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if limit != tc.wantLimit {
				t.Fatalf("limit = %d, want %d", limit, tc.wantLimit)
			}
			gotAfter := ""
			if after != nil {
				gotAfter = *after
			}
			if gotAfter != tc.wantAfter {
				t.Fatalf("after = %q, want %q", gotAfter, tc.wantAfter)
			}
		})
	}
}

// POST /v1/files dropped OpenAI's expires_after[anchor] / expires_after[seconds] form fields
// into extra params, which the multipart upload never sends (contract C4).
func TestParseFileExpiresAfter(t *testing.T) {
	cases := []struct {
		name    string
		values  map[string][]string
		want    *schemas.FileExpiresAfter
		wantErr bool
	}{
		{"absent", map[string][]string{"purpose": {"batch"}}, nil, false},
		{"both", map[string][]string{"expires_after[anchor]": {"created_at"}, "expires_after[seconds]": {"3600"}}, &schemas.FileExpiresAfter{Anchor: "created_at", Seconds: 3600}, false},
		{"anchor only", map[string][]string{"expires_after[anchor]": {"created_at"}}, nil, true},
		{"seconds only", map[string][]string{"expires_after[seconds]": {"3600"}}, nil, true},
		{"seconds not a number", map[string][]string{"expires_after[anchor]": {"created_at"}, "expires_after[seconds]": {"soon"}}, nil, true},
		{"seconds not positive", map[string][]string{"expires_after[anchor]": {"created_at"}, "expires_after[seconds]": {"0"}}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFileExpiresAfter(tc.values)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
