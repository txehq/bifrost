package governance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			require.True(t, isBackgroundResponsesCreate(background), "%s sent in the background must be recognised as one", requestType)
			assert.False(t, isBackgroundResponsesCreate(requestFor(requestType, false)), "%s in the foreground is not a background create", requestType)
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
func TestPassthroughIsBackground(t *testing.T) {
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
		`[{"model":"m","background":true}]`:           false, // an array is not a Responses create
		``:                                            false,
		`   `:                                         false,
	} {
		assert.Equal(t, want, passthroughIsBackground("/v1/responses", []byte(body)), "%q", body)
	}
	for _, path := range []string{"/v1/responses", "/openai/v1/responses", "/openai/RESPONSES", "/v1/responses/", "/v1/respons%65s"} {
		assert.True(t, passthroughIsBackground(path, []byte(`{"background":true}`)), path)
	}
	for _, path := range []string{"/v1/files", "/v1/fine_tuning/jobs", "/v1/images/generations"} {
		assert.False(t, passthroughIsBackground(path, []byte(`{"background":true}`)), "%s is not a Responses create", path)
		assert.False(t, passthroughIsBackground(path, []byte("--multipart-boundary")), "%s with a non-JSON body is not a Responses create", path)
	}
}
