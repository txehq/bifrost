package governance

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"mime/multipart"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hook tests (TestBackgroundResponses..., TestSettledJob...) call only hooks that existed before
// TXE-7963, so they ran red on its base; the class checks after them name its new symbols.

func requireUnsettledRefusal(t *testing.T, shortCircuit *schemas.LLMPluginShortCircuit, decision, what string) {
	t.Helper()
	require.NotNil(t, shortCircuit, "%s: a queued answer nothing will charge must be refused under a dollar budget before the call", what)
	require.NotNil(t, shortCircuit.Error, what)
	require.NotNil(t, shortCircuit.Error.StatusCode, what)
	assert.Equal(t, 403, *shortCircuit.Error.StatusCode, what)
	assert.Equal(t, schemas.ErrorTypePolicyModelBlocked, shortCircuit.Error.ExtraFields.ErrorType, what)
	require.NotNil(t, shortCircuit.Error.Type, what)
	assert.Equal(t, decision, *shortCircuit.Error.Type, what)
}

func responsesOn(requestType schemas.RequestType, model string, background *bool) *schemas.BifrostRequest {
	req := &schemas.BifrostResponsesRequest{Provider: schemas.OpenAI, Model: model}
	if background != nil {
		req.Params = &schemas.ResponsesParameters{Background: background}
	}
	return &schemas.BifrostRequest{RequestType: requestType, ResponsesRequest: req}
}

func passthroughResponses(body string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o", Method: "POST", Path: "/v1/responses", Body: []byte(body),
	}}
}

// A background Responses create answers "queued" with no usage, and the retrieve that delivers the
// finished answer is never priced, so under a dollar budget it would be charged $0 on any model.
// It is refused before the call, on every route that can send one. In the foreground, and on a
// key with no dollar budget, it is served.
func TestBackgroundResponsesUnderADollarBudgetIsRefusedBeforeTheCall(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	// Each request ends at admission (preLLM), so the next is admitted after it rather than
	// concurrently with it on a row no answer has priced yet (TXE-7977).
	pre := func(vk string, req *schemas.BifrostRequest) *schemas.LLMPluginShortCircuit {
		t.Helper()
		return preLLM(t, plugin, vk, req)
	}
	background, foreground := true, false

	for _, requestType := range []schemas.RequestType{schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest} {
		requireUnsettledRefusal(t, pre("sk-bf-budget", responsesOn(requestType, "gpt-4o", &background)), "background_unsettled",
			string(requestType)+" with background: true on a priced model")
	}
	requireUnsettledRefusal(t, pre("sk-bf-budget", passthroughResponses(`{"model":"gpt-4o","input":"hi","background":true}`)), "background_unsettled",
		"a passthrough Responses body with background: true")
	requireUnsettledRefusal(t, pre("sk-bf-budget", passthroughResponses(`{"model":"gpt-4o","background":false,"input":"hi","background":true}`)), "background_unsettled",
		"a passthrough body whose duplicated background key ends true, as a last-key-wins decoder reads it")
	extra := responsesOn(schemas.ResponsesRequest, "gpt-4o", nil)
	extra.ResponsesRequest.Params = &schemas.ResponsesParameters{ExtraParams: map[string]interface{}{"Background": true}}
	requireUnsettledRefusal(t, pre("sk-bf-budget", extra), "background_unsettled",
		"background forwarded in extra params, any key case (BifrostContextKeyPassthroughExtraParams)")
	extraOverride := responsesOn(schemas.ResponsesRequest, "gpt-4o", &foreground)
	extraOverride.ResponsesRequest.Params.ExtraParams = map[string]interface{}{"background": "yes"}
	requireUnsettledRefusal(t, pre("sk-bf-budget", extraOverride), "background_unsettled",
		"a typed background: false with a non-false background in extra params, read failing closed")
	raw := responsesOn(schemas.ResponsesStreamRequest, "gpt-4o", nil)
	raw.ResponsesRequest.RawRequestBody = []byte(`{"model":"gpt-4o","input":"hi","background":true}`)
	requireUnsettledRefusal(t, pre("sk-bf-budget", raw), "background_unsettled", "background in the raw request body")
	runwareAsync := passthroughResponses(`[{"taskType":"imageInference","taskUUID":"u","model":"gpt-4o","deliveryMethod":"async"}]`)
	runwareAsync.PassthroughRequest.Path = "/"
	requireUnsettledRefusal(t, pre("sk-bf-budget", runwareAsync), "background_unsettled", "a Runware task delivered async (TXE-8013)")
	runwareWebhook := passthroughResponses(`[{"taskType":"imageInference","taskUUID":"u","model":"gpt-4o","webhookURL":"https://hooks.example.invalid/r"}]`)
	runwareWebhook.PassthroughRequest.Path = "/"
	requireUnsettledRefusal(t, pre("sk-bf-budget", runwareWebhook), "background_unsettled", "a Runware task delivered to a webhook (TXE-8013)")
	webhookTranscription := &schemas.BifrostRequest{RequestType: schemas.TranscriptionRequest, TranscriptionRequest: &schemas.BifrostTranscriptionRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o", Params: &schemas.TranscriptionParameters{ExtraParams: map[string]interface{}{"webhook": true}},
	}}
	requireUnsettledRefusal(t, pre("sk-bf-budget", webhookTranscription), "background_unsettled", "a transcription whose result is posted to a webhook (TXE-8013)")
	interactions := passthroughResponses(`{"model":"gemini-omni","input":"hi","background":true}`)
	interactions.PassthroughRequest.Path = "/v1beta/interactions"
	requireUnsettledRefusal(t, pre("sk-bf-budget", interactions), "background_unsettled",
		"a passthrough Interactions body with background: true")

	assert.Nil(t, pre("sk-bf-budget", responsesOn(schemas.ResponsesRequest, "gpt-4o", &foreground)), "background: false is a finished, priced answer")
	assert.Nil(t, pre("sk-bf-budget", responsesOn(schemas.ResponsesRequest, "gpt-4o", nil)), "a Responses create with no background is served")
	assert.Nil(t, pre("sk-bf-budget", passthroughResponses(`{"model":"gpt-4o","input":"hi"}`)), "a passthrough body with no background is served")
	unrelated := responsesOn(schemas.ResponsesRequest, "gpt-4o", nil)
	unrelated.ResponsesRequest.Params = &schemas.ResponsesParameters{ExtraParams: map[string]interface{}{"service_tier": "flex"}}
	unrelated.ResponsesRequest.RawRequestBody = []byte(`{"model":"gpt-4o","input":"hi"}`)
	assert.Nil(t, pre("sk-bf-budget", unrelated), "extra params and a raw body with no background are served")
	assert.Nil(t, pre("sk-bf-open", responsesOn(schemas.ResponsesRequest, "gpt-4o", &background)), "a key with no dollar budget has nothing to charge")
}

// A video or batch create is charged when framework/jobaccounting settles its job. With provider
// job accounting off (client.disable_provider_job_accounting, the production setting) nothing
// settles, so under a dollar budget every job would be charged $0. Until the transport says jobs
// are settled, they are refused under a dollar budget; a key with no budget is served.
func TestSettledJobUnderADollarBudgetIsRefusedWhenNothingSettlesIt(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	video := &schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "clip-1"},
	}
	_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), video)
	require.NoError(t, err)
	requireUnsettledRefusal(t, shortCircuit, "job_unsettled", "a priced video create under a budget with no job accounting")

	_, shortCircuit, err = plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-open"), video)
	require.NoError(t, err)
	assert.Nil(t, shortCircuit, "a key with no dollar budget is served")
}

// queuedStatusResponses names, for every Bifrost*Response in core/schemas with a Status field,
// the request types that create the work it reports on: the requests a budget is charged for. A
// response that only reads, cancels or deletes existing work creates nothing and lists none.
var queuedStatusResponses = map[string][]schemas.RequestType{
	"BifrostResponsesResponse":         {schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest},
	"BifrostVideoGenerationResponse":   {schemas.VideoGenerationRequest, schemas.VideoEditRequest, schemas.VideoRemixRequest},
	"BifrostBatchCreateResponse":       {schemas.BatchCreateRequest},
	"BifrostImageTaskResponse":         {schemas.ImageGenerationTaskRequest, schemas.ImageEditTaskRequest},
	"BifrostContainerCreateResponse":   {schemas.ContainerCreateRequest},
	"BifrostBatchRetrieveResponse":     nil,
	"BifrostBatchCancelResponse":       nil,
	"BifrostBatchDeleteResponse":       nil,
	"BifrostContainerRetrieveResponse": nil,
	"BifrostFileUploadResponse":        nil, // a file costs nothing; the batch that reads it is charged
	"BifrostFileRetrieveResponse":      nil,
}

// rawBodyRequestTypes forward a body governance does not type, so they can carry any create,
// including a background Responses one.
var rawBodyRequestTypes = []schemas.RequestType{schemas.PassthroughRequest, schemas.PassthroughStreamRequest}

// declaredStatusResponses reads every Bifrost*Response struct with a Status field from
// core/schemas, so a new asynchronous answer is checked without editing this test.
func declaredStatusResponses(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "../../core/schemas", nil, 0)
	require.NoError(t, err)
	var names []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				spec, ok := n.(*ast.TypeSpec)
				if !ok || !strings.HasPrefix(spec.Name.Name, "Bifrost") || !strings.HasSuffix(spec.Name.Name, "Response") {
					return true
				}
				st, ok := spec.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						if name.Name == "Status" {
							names = append(names, spec.Name.Name)
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(names)
	require.GreaterOrEqual(t, len(names), 5, "found only %v; the parser no longer sees the status-bearing responses", names)
	return names
}

// requestFor builds a create of the given type, sent in the background where the type can be.
func requestFor(requestType schemas.RequestType, background bool) *schemas.BifrostRequest {
	for _, raw := range rawBodyRequestTypes {
		if requestType == raw {
			body := `{"model":"gpt-4o"}`
			if background {
				body = `{"model":"gpt-4o","background":true}`
			}
			return &schemas.BifrostRequest{RequestType: requestType, PassthroughRequest: &schemas.BifrostPassthroughRequest{Path: "/v1/responses", Body: []byte(body)}}
		}
	}
	req := responsesOn(requestType, "gpt-4o", &background)
	return req
}

// TestEveryQueuedAnswerIsSettledOrRefusedUnderABudget is the TXE-7963 class check: a request whose
// answer can report unfinished work is charged to a dollar budget only if something prices the
// finished work. Every such request type, derived from core/schemas, must be listed in
// queuedAnswerRequestTypes, and governance must act on each entry as listed: a settled job is
// refused under a budget unless job accounting settles it, a background create is refused, and a
// type priced at create is priced on its answer.
func TestEveryQueuedAnswerIsSettledOrRefusedUnderABudget(t *testing.T) {
	creates := map[schemas.RequestType]bool{}
	for _, name := range declaredStatusResponses(t) {
		types, classified := queuedStatusResponses[name]
		if !assert.True(t, classified, "%s reports a status, so its answer can be unfinished work: list the request types that create it in queuedStatusResponses, and classify each in queuedAnswerRequestTypes", name) {
			continue
		}
		for _, requestType := range types {
			creates[requestType] = true
		}
	}
	declared := map[string]bool{}
	for _, name := range declaredStatusResponses(t) {
		declared[name] = true
	}
	for name := range queuedStatusResponses {
		assert.True(t, declared[name], "queuedStatusResponses lists %s, which core/schemas no longer declares with a Status field", name)
	}
	for _, requestType := range rawBodyRequestTypes {
		creates[requestType] = true
	}
	// Anything priced at settlement answers with a queued job by definition.
	for requestType, pricing := range requestTypeBudgetPricing {
		if pricing == budgetPricingSettled {
			creates[requestType] = true
		}
	}

	for requestType := range creates {
		_, listed := queuedAnswerRequestTypes[requestType]
		assert.True(t, listed, "%s can answer with unfinished work, but queuedAnswerRequestTypes does not say how a dollar budget is charged for it", requestType)
	}
	for requestType := range queuedAnswerRequestTypes {
		assert.True(t, creates[requestType], "queuedAnswerRequestTypes lists %s, which no status-bearing response or settled job answers", requestType)
	}

	r := &BudgetResolver{}
	for requestType, handling := range queuedAnswerRequestTypes {
		switch handling {
		case queuedAnswerSettled:
			require.Equal(t, budgetPricingSettled, budgetPricingOf(requestType), "%s is settled, so requestTypeBudgetPricing must price it at settlement", requestType)
			r.providerJobAccounting.Store(false)
			refusal := r.refuseUnsettledUnderBudget(&EvaluationRequest{RequestType: requestType})
			if assert.NotNil(t, refusal, "%s under a budget with job accounting off is never charged, so it must be refused", requestType) {
				assert.Equal(t, DecisionJobUnsettled, refusal.Decision)
			}
			r.providerJobAccounting.Store(true)
			assert.Nil(t, r.refuseUnsettledUnderBudget(&EvaluationRequest{RequestType: requestType}), "%s with job accounting on is charged at settlement", requestType)
		case queuedAnswerBackgroundRefused:
			require.Equal(t, budgetPricingAnswer, budgetPricingOf(requestType), "%s in the foreground is priced on its answer", requestType)
			background := requestFor(requestType, true)
			require.True(t, isDeferredDelivery(background, schemas.OpenAI), "%s sent in the background must be recognised as one", requestType)
			assert.False(t, isDeferredDelivery(requestFor(requestType, false), schemas.OpenAI), "%s in the foreground is not a background create", requestType)
			refusal := r.refuseUnsettledUnderBudget(&EvaluationRequest{RequestType: requestType, Background: true})
			if assert.NotNil(t, refusal, "a background %s is never charged, so it must be refused under a budget", requestType) {
				assert.Equal(t, DecisionBackgroundUnsettled, refusal.Decision)
			}
		case queuedAnswerPricedAtCreate:
			assert.Equal(t, budgetPricingAnswer, budgetPricingOf(requestType), "%s is priced at create, so its answer must be priced", requestType)
		default:
			t.Errorf("%s has an unknown handling %d", requestType, handling)
		}
	}
}

// The transport tells governance whether provider jobs are settled; until it does, they are not.
func TestProviderJobAccountingSwitchGatesSettledJobs(t *testing.T) {
	plugin, _ := unpricedBudgetPlugin(t)
	video := &schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{Provider: schemas.OpenAI, Model: "clip-1"},
	}
	pre := func() *schemas.LLMPluginShortCircuit {
		_, shortCircuit, err := plugin.PreLLMHook(resolverCtx(plugin.store, "sk-bf-budget"), video)
		require.NoError(t, err)
		return shortCircuit
	}
	requireUnsettledRefusal(t, pre(), "job_unsettled", "before the transport says jobs are settled")
	plugin.SetProviderJobAccountingEnabled(true)
	assert.Nil(t, pre(), "a priced video under a budget is admitted when its job is settled and charged")
	plugin.SetProviderJobAccountingEnabled(false)
	requireUnsettledRefusal(t, pre(), "job_unsettled", "after the transport turns job accounting off")
}

// A passthrough Responses body is judged as the upstream's decoder might read it, failing closed:
// any occurrence of the key in any letter case, any value but false or null, an escaped key name, a
// byte order mark, and a body gjson cannot read all count. Other paths are not Responses creates.
func TestPassthroughDefersDelivery(t *testing.T) {
	for body, want := range map[string]bool{
		`{"model":"m","background":true}`:             true,
		`{"model":"m","background":"true"}`:           true,
		`{"model":"m","background":1}`:                true,
		`{"model":"m","background":"yes"}`:            true,
		`{"background":false,"background":true}`:      true,
		`{"background":true,"background":false}`:      true,
		`{"Background":true}`:                         true,
		`{"BACKGROUND":true}`:                         true,
		`{"backgr\u006fund":true}`:                    true,
		"\xef\xbb\xbf{\"background\":true}":           true,
		`{"model":"m","background":true,}`:            true, // gjson cannot read it
		"\x1f\x8b\x08\x00compressed":                  true, // gjson cannot read it
		"\xff\xfe{\x00\"\x00b\x00":                    true, // UTF-16: gjson cannot read it
		`not json at all`:                             true,
		`{"model":"m","background":false}`:            false,
		`{"model":"m","background":null}`:             false,
		`{"model":"m","input":"draw a background"}`:   false,
		`{"model":"m","tools":[{"background":true}]}`: false, // not top level
		`[{"model":"m","background":true}]`:           true,  // each unit of a task list is read
		``:                                            false,
		`   `:                                         false,
	} {
		assert.Equal(t, want, passthroughDefersDelivery("/v1/responses", []byte(body), false, ""), "%q", body)
	}
	for _, path := range []string{"/v1/responses", "/openai/v1/responses", "/openai/RESPONSES", "/v1/responses/", "/v1/respons%65s"} {
		assert.True(t, passthroughDefersDelivery(path, []byte(`{"background":true}`), false, ""), path)
	}
	for _, path := range []string{"/v1beta/interactions", "/v1/projects/p/locations/l/interactions"} {
		assert.True(t, passthroughDefersDelivery(path, []byte(`{"model":"m","background":true}`), false, ""), "%s runs background interactions", path)
		assert.True(t, passthroughDefersDelivery(path, []byte(`{"background":1}`), false, ""), "%s fails closed on a coercible value", path)
		assert.True(t, passthroughDefersDelivery(path, []byte("\x1f\x8bgzip"), false, ""), "%s fails closed on an unreadable body", path)
	}
	for _, path := range []string{"/v1/files", "/v1/fine_tuning/jobs", "/v1/images/generations", "/v1/some/new/api"} {
		assert.True(t, passthroughDefersDelivery(path, []byte(`{"background":true}`), false, ""), "%s: an explicit background true is refused on any path", path)
		assert.True(t, passthroughDefersDelivery(path, []byte(`{"Background":"TRUE"}`), false, ""), "%s: in any letter case", path)
		assert.False(t, passthroughDefersDelivery(path, []byte(`{"model":"gpt-image-1","background":"transparent"}`), false, ""), "%s: an image background is not a background run", path)
		assert.False(t, passthroughDefersDelivery(path, []byte("--multipart-boundary"), false, ""), "%s with a non-JSON body is not a background create", path)
	}
}

// A form body carries its settings as form fields: an ElevenLabs speech-to-text upload asks for a
// webhook with webhook=true. Each text field is read with the same rule; file parts are not.
func TestFormPassthroughDefersDelivery(t *testing.T) {
	form := func(fields ...string) ([]byte, string) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		for i := 0; i+1 < len(fields); i += 2 {
			require.NoError(t, w.WriteField(fields[i], fields[i+1]))
		}
		fw, err := w.CreateFormFile("webhook_url", "clip.mp3") // a file part named like a key is the upload
		require.NoError(t, err)
		_, _ = fw.Write([]byte("ID3 audio"))
		require.NoError(t, w.Close())
		return b.Bytes(), w.FormDataContentType()
	}
	path := "/v1/speech-to-text"
	body, ct := form("model_id", "scribe_v1", "webhook", "true")
	assert.True(t, passthroughDefersDelivery(path, body, false, ct), "a multipart webhook=true field")
	assert.True(t, passthroughDefersDelivery(path, body, false, ""), "an undeclared multipart body is read by its opening boundary")
	body, ct = form("model_id", "scribe_v1", "Webhook", "TRUE", "webhook", "false")
	assert.True(t, passthroughDefersDelivery(path, body, false, ct), "each occurrence of a repeated field is judged")
	body, ct = form("model_id", "scribe_v1", "callback_url", "https://hooks.example.invalid/c")
	assert.True(t, passthroughDefersDelivery(path, body, false, ct), "a multipart callback URL")
	body, ct = form("model_id", "scribe_v1", "webhook", "false", "diarize", "true")
	assert.False(t, passthroughDefersDelivery(path, body, false, ct), "webhook=false and a file part named like a key are synchronous")
	assert.True(t, passthroughDefersDelivery(path, []byte("webhook=true&model_id=scribe_v1"), false, "application/x-www-form-urlencoded"), "an urlencoded webhook")
	assert.False(t, passthroughDefersDelivery(path, []byte("model_id=scribe_v1"), false, "application/x-www-form-urlencoded"))
	assert.True(t, passthroughDefersDelivery(path, []byte("--b\r\nbroken"), false, "multipart/form-data; boundary=b"), "an unreadable multipart form fails closed")
	assert.True(t, passthroughDefersDelivery(path, []byte("webhook=%zz&model_id=m"), false, "application/x-www-form-urlencoded"), "an urlencoded form that does not parse fails closed")
	body, ct = form("model_id", "scribe_v1", "webhook_url", strings.Repeat(" ", 8192)+"https://hooks.example.invalid/p")
	assert.True(t, passthroughDefersDelivery(path, body, false, ct), "a delivery value padded past any read limit is still judged in full")
	body, ct = form("model_id", "scribe_v1", "callback_url", strings.Repeat("x", maxFormFieldBytes+1))
	assert.True(t, passthroughDefersDelivery(path, body, false, ct), "a delivery value longer than the limit fails closed")
	body, ct = form("model_id", "scribe_v1", "prompt", strings.Repeat("p", maxFormFieldBytes+1))
	assert.False(t, passthroughDefersDelivery(path, body, false, ct), "a long field that names no delivery is not read")
	assert.False(t, passthroughDefersDelivery(path, []byte("ID3 audio"), false, "audio/mpeg"), "a raw audio body is no form")
	assert.True(t, passthroughDefersDelivery(path, []byte(`{"x":1}&webhook=true`), false, "application/x-www-form-urlencoded"), "a declared form is read as a form, whatever it opens with")
	assert.True(t, passthroughDefersDelivery(path, []byte(`["a"]`), false, "multipart/form-data; boundary=b"), "valid JSON declared multipart does not parse as a form and fails closed")
	assert.False(t, passthroughDefersDelivery(path, []byte(`{"model":"m"}`), false, "application/x-www-form-urlencoded"), "a declared form naming no delivery field is served")
	assert.True(t, passthroughDefersDelivery(path, []byte(`{"model":"m","webhook":true}`), false, "application/x-www-form-urlencoded"), "a JSON body is judged as JSON too, whatever it declares")
}

// A Runware passthrough task asks for deferred delivery with deliveryMethod (async) or webhookURL;
// its answer is then a queued acknowledgment nothing charges (TXE-8013). Every task of the list is
// read, in any key case, and on Runware a body gjson cannot read counts.
func TestRunwarePassthroughDefersDelivery(t *testing.T) {
	for body, want := range map[string]bool{
		`[{"taskType":"imageInference","model":"runware:101@1","deliveryMethod":"async"}]`:                                                       true,
		`[{"taskType":"imageInference","model":"m","deliveryMethod":"sync"},{"taskType":"videoInference","model":"m","deliveryMethod":"async"}]`: true,
		`[{"taskType":"imageInference","model":"m","webhookURL":"https://hooks.example.invalid/r"}]`:                                             true,
		`[{"taskType":"imageInference","model":"m","WebhookUrl":"https://hooks.example.invalid/r"}]`:                                             true,
		`[{"taskType":"imageInference","model":"m","DeliveryMethod":"ASYNC"}]`:                                                                   true,
		`[{"taskType":"imageInference","model":"m","deliveryMethod":"sync","deliveryMethod":"async"}]`:                                           true,
		`[{"taskType":"imageInference","model":"m","deliveryMethod":1}]`:                                                                         true,
		`{"taskType":"imageInference","model":"m","deliveryMethod":"async"}`:                                                                     true,
		"\x1f\x8bgzip": true,
		`[{"taskType":"imageInference","model":"m"}]`:                                       false,
		`[{"taskType":"imageInference","model":"m","deliveryMethod":"sync"}]`:               false,
		`[{"taskType":"imageInference","model":"m","deliveryMethod":null,"webhookURL":""}]`: false,
		`[{"taskType":"imageInference","model":"m","deliveryMethod":" Sync "}]`:             false,
		`[{"taskType":"videoInference","model":"m"}]`:                                       true, // video runs async by default
		`[{"taskType":"videoInference","model":"m","deliveryMethod":null}]`:                 true,
		`[{"TaskType":"VideoInference","model":"m"}]`:                                       true,
		`[{"taskType":"videoInference","model":"m","deliveryMethod":"sync"}]`:               false,
	} {
		assert.Equal(t, want, passthroughDefersDelivery("/", []byte(body), true, ""), "%q", body)
	}
	runware := &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Path: "/", Body: []byte("\x1f\x8bgzip"),
	}}
	assert.True(t, isDeferredDelivery(runware, schemas.Runware), "a Runware body gjson cannot read fails closed")
	assert.False(t, isDeferredDelivery(runware, schemas.OpenAI), "elsewhere a non-JSON body off a Responses path is not deferred")
	video := &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Path: "/", Body: []byte(`[{"taskType":"videoInference","model":"m"}]`),
	}}
	assert.True(t, isDeferredDelivery(video, schemas.Runware), "a Runware video task with no deliveryMethod runs async")
	assert.False(t, isDeferredDelivery(video, schemas.OpenAI), "the async default is Runware's")
}

// Every passthrough answer governance does not count as an answer because it only acknowledges queued
// work (passthroughQueuedAcknowledgment) must come from a request the gate refuses under a dollar
// budget before the call: otherwise the work runs and nothing ever charges it (TXE-8013).
func TestEveryQueuedAcknowledgmentIsRefusedBeforeTheCall(t *testing.T) {
	for _, tc := range []struct {
		name, request, answer string
		provider              schemas.ModelProvider
	}{
		{"runware async", `[{"taskType":"imageInference","taskUUID":"u","model":"m","deliveryMethod":"async"}]`,
			`{"data":[{"taskType":"imageInference","taskUUID":"u","status":"processing"}]}`, schemas.Runware},
		{"runware webhook", `[{"taskType":"imageInference","taskUUID":"u","model":"m","webhookURL":"https://hooks.example.invalid/r"}]`,
			`{"data":[{"taskType":"imageInference","taskUUID":"u","deliveryMethod":"webhook"}]}`, schemas.Runware},
		{"runware video", `[{"taskType":"videoInference","taskUUID":"u","model":"m","deliveryMethod":"async"}]`,
			`{"data":[{"taskType":"videoInference","taskUUID":"u"}]}`, schemas.Runware},
	} {
		require.True(t, passthroughQueuedAcknowledgment([]byte(tc.answer)), "%s: the fixture answer must be a queued acknowledgment", tc.name)
		req := &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Path: "/", Body: []byte(tc.request)}}
		assert.True(t, isDeferredDelivery(req, tc.provider), "%s: a request answered with a queued acknowledgment must be refused under a dollar budget", tc.name)
	}
	// The other side: a synchronous Runware task is answered with its result and priced on it.
	require.False(t, passthroughQueuedAcknowledgment([]byte(`{"data":[{"taskType":"imageInference","taskUUID":"u","imageURL":"https://x.invalid/i.png","cost":0.004}]}`)))
}

// A typed request forwards its extra params and, in raw-body mode, its body as sent: a webhook or
// callback, or a background flag, there defers delivery as it would in a passthrough body. A job
// priced at settlement and an image task priced at create are charged whatever their delivery.
func TestTypedRequestForwardedInputsDeferDelivery(t *testing.T) {
	transcription := func(extra map[string]interface{}) *schemas.BifrostRequest {
		return &schemas.BifrostRequest{RequestType: schemas.TranscriptionRequest, TranscriptionRequest: &schemas.BifrostTranscriptionRequest{
			Provider: schemas.Elevenlabs, Model: "scribe_v1", Params: &schemas.TranscriptionParameters{ExtraParams: extra},
		}}
	}
	assert.True(t, isDeferredDelivery(transcription(map[string]interface{}{"webhook": true}), schemas.Elevenlabs), "ElevenLabs speech-to-text posted to a webhook")
	assert.True(t, isDeferredDelivery(transcription(map[string]interface{}{"Webhook": "true"}), schemas.Elevenlabs), "in any key case")
	assert.False(t, isDeferredDelivery(transcription(map[string]interface{}{"webhook": false}), schemas.Elevenlabs), "webhook false is synchronous")
	assert.False(t, isDeferredDelivery(transcription(map[string]interface{}{"diarize": true}), schemas.Elevenlabs), "other extra params are not read")
	assert.False(t, isDeferredDelivery(transcription(nil), schemas.Elevenlabs))
	assert.True(t, isDeferredDelivery(transcription(map[string]interface{}{"extra_params": map[string]interface{}{"webhook": true}}), schemas.Elevenlabs), "Bifrost's extra_params wrapper kept as one field is read too")

	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o", RawRequestBody: []byte(`{"model":"gpt-4o","messages":[],"callback_url":"https://hooks.example.invalid/c"}`),
	}}
	assert.True(t, isDeferredDelivery(chat, schemas.OpenAI), "a raw body forwarded as sent is read")
	responses := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o", RawRequestBody: []byte(`{"model":"gpt-4o","input":"hi","background":1}`),
	}}
	assert.True(t, isDeferredDelivery(responses, schemas.OpenAI), "a raw Responses body is read as a Responses route, failing closed")
	responses.ResponsesRequest.RawRequestBody = []byte(`{"model":"gpt-4o","input":"hi"}`)
	assert.False(t, isDeferredDelivery(responses, schemas.OpenAI))

	video := &schemas.BifrostRequest{RequestType: schemas.VideoGenerationRequest, VideoGenerationRequest: &schemas.BifrostVideoGenerationRequest{
		Provider: schemas.Kling, Model: "kling-v2", Params: &schemas.VideoGenerationParameters{ExtraParams: map[string]interface{}{"callback_url": "https://hooks.example.invalid/k"}},
	}}
	assert.False(t, isDeferredDelivery(video, schemas.Kling), "a video is charged when its job settles, whatever its delivery")
}

// Every request variant whose Params carry ExtraParams is read for deferred delivery: the extra
// params are forwarded to the provider, so a webhook there defers delivery like one in the body.
func TestForwardedExtraParamsCoverEveryRequestVariant(t *testing.T) {
	listed := map[int]bool{}
	for _, f := range extraParamsFields {
		listed[f.request] = true
	}
	rt := reflect.TypeOf(schemas.BifrostRequest{})
	variants := 0
	for i := 0; i < rt.NumField(); i++ {
		ft := rt.Field(i).Type
		if ft.Kind() != reflect.Pointer || ft.Elem().Kind() != reflect.Struct {
			continue
		}
		params, ok := ft.Elem().FieldByName("Params")
		if !ok || params.Type.Kind() != reflect.Pointer || params.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		if _, ok := params.Type.Elem().FieldByName("ExtraParams"); !ok {
			continue
		}
		variants++
		assert.True(t, listed[i], "%s carries Params.ExtraParams but requestForwardedInputs does not read it", rt.Field(i).Name)
	}
	require.Greater(t, variants, 10, "the scan found too few request variants with extra params; it no longer matches the schema")

	chat := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Params:         &schemas.ChatParameters{ExtraParams: map[string]interface{}{"callback_url": "https://hooks.example.invalid/c"}},
		RawRequestBody: []byte(`{"model":"m"}`),
	}}
	extra, raw := requestForwardedInputs(chat)
	assert.Equal(t, "https://hooks.example.invalid/c", extra["callback_url"])
	assert.Equal(t, `{"model":"m"}`, string(raw))
	chat.ChatRequest.Params = nil
	extra, raw = requestForwardedInputs(chat)
	assert.Nil(t, extra)
	assert.Equal(t, `{"model":"m"}`, string(raw))
}

// deliveryFieldExemptions are provider request fields whose names look like deferred delivery but
// do not send a result anywhere but the answer, each with the reason.
var deliveryFieldExemptions = map[string]string{
	"background":              "read by the detector (Responses, Interactions); elsewhere an image backdrop",
	"backgroundremovalparams": "Bedrock image background removal: an image edit, not a delivery",
	"delivery":                "Gemini Interactions response_format.delivery: inline or a URI in the answer, not a deferral",
	"deliverymethod":          "read by the detector (Runware)",
	"webhook_id":              "names the ElevenLabs webhook that the webhook switch, which the detector reads, selects",
	"webhook_metadata":        "rides with the ElevenLabs webhook switch, which the detector reads",
	"webhook_events_filter":   "rides with the Replicate webhook, which the detector reads",
	"webhook_completed":       "a Replicate response field, not a request field",
}

// TestEveryProviderDeliveryFieldIsReadOrExempt is the provider half of the TXE-8013 class check: every
// JSON field a provider sends or reads whose name asks for delivery elsewhere (a webhook, a callback,
// a delivery method, background) is either a key the deferred-delivery detector reads, or exempt
// with a reason. A provider that gains a new async or webhook route fails here until governance
// either refuses it under a dollar budget or says why its answer is still charged.
func TestEveryProviderDeliveryFieldIsReadOrExempt(t *testing.T) {
	pattern := regexp.MustCompile(`json:"([A-Za-z_]*(?:[Bb]ackground|[Dd]eliver|[Ww]ebhook|[Cc]allback)[A-Za-z_]*)`)
	found := map[string][]string{}
	require.NoError(t, filepath.WalkDir("../../core/providers", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range pattern.FindAllSubmatch(src, -1) {
			name := strings.ToLower(string(m[1]))
			found[name] = append(found[name], path)
		}
		return nil
	}))
	require.NotEmpty(t, found, "the scan found no delivery fields in core/providers; the pattern no longer matches")
	for name, paths := range found {
		if deferredDeliveryKey(name) {
			continue
		}
		_, exempt := deliveryFieldExemptions[name]
		assert.True(t, exempt, "provider field %q (%v) looks like deferred delivery: add it to deferredDeliveryKeys so the gate refuses it under a dollar budget, or to deliveryFieldExemptions with the reason its answer is still charged", name, paths)
	}
	for name := range deliveryFieldExemptions {
		_, ok := found[name]
		assert.True(t, ok, "deliveryFieldExemptions lists %q, which no provider declares any more", name)
	}
}
