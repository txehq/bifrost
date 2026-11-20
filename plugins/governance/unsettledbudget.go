package governance

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// A request whose answer is queued is charged to a dollar budget only when something prices the
// finished work later. Two such paths charged nothing (TXE-7963):
//
//   - A Responses create sent with background: true answers "queued" with no usage. The finished
//     answer comes back on GET /v1/responses/{id}, which is never priced, so the budget paid $0
//     on any model.
//   - A video or batch create is priced when framework/jobaccounting settles the job. With
//     client.disable_provider_job_accounting set (TXE-6519, the production setting), nothing
//     settles, so the budget paid $0 for every job.
//
// Governance refuses both under a dollar budget, before the upstream call, in the same gate that
// refuses an unpriced model (refuseUnpricedUnderBudget).

// DecisionBackgroundUnsettled refuses a background Responses create under a dollar budget: its
// answer is delivered by a retrieve that is never charged.
const DecisionBackgroundUnsettled Decision = "background_unsettled"

// DecisionJobUnsettled refuses a request priced at settlement under a dollar budget when this
// deployment does not settle provider jobs.
const DecisionJobUnsettled Decision = "job_unsettled"

// queuedAnswerHandling is how a request type whose answer can report unfinished work is charged.
type queuedAnswerHandling int

const (
	// queuedAnswerSettled: charged when framework/jobaccounting settles the job, and refused under
	// a dollar budget when provider job accounting is off.
	queuedAnswerSettled queuedAnswerHandling = iota + 1
	// queuedAnswerBackgroundRefused: queued only when sent in the background, which is refused
	// under a dollar budget. In the foreground the answer is finished and priced.
	queuedAnswerBackgroundRefused
	// queuedAnswerPricedAtCreate: the create's own answer carries the whole price, even while the
	// work is still running.
	queuedAnswerPricedAtCreate
)

// queuedAnswerRequestTypes lists every request type whose answer can report work that has not
// finished: a queued or in-progress status. TestEveryQueuedAnswerIsSettledOrRefusedUnderABudget
// derives the set from core/schemas (every Bifrost*Response with a Status field, and the create
// types that answer with it) and fails when one is missing here or stale, and checks each entry's
// handling against governance's decision.
var queuedAnswerRequestTypes = map[schemas.RequestType]queuedAnswerHandling{
	schemas.VideoGenerationRequest: queuedAnswerSettled,
	schemas.VideoEditRequest:       queuedAnswerSettled,
	schemas.VideoRemixRequest:      queuedAnswerSettled,
	schemas.BatchCreateRequest:     queuedAnswerSettled,

	schemas.ResponsesRequest:          queuedAnswerBackgroundRefused,
	schemas.ResponsesStreamRequest:    queuedAnswerBackgroundRefused,
	schemas.WebSocketResponsesRequest: queuedAnswerBackgroundRefused,
	// A passthrough body can be a background Responses create.
	schemas.PassthroughRequest:       queuedAnswerBackgroundRefused,
	schemas.PassthroughStreamRequest: queuedAnswerBackgroundRefused,

	// An image task is billed once, at its create (TXE-6431).
	schemas.ImageGenerationTaskRequest: queuedAnswerPricedAtCreate,
	schemas.ImageEditTaskRequest:       queuedAnswerPricedAtCreate,
	// A container's status is its lifecycle; the create is priced on its answer.
	schemas.ContainerCreateRequest: queuedAnswerPricedAtCreate,
}

// SetProviderJobAccountingEnabled tells governance whether framework/jobaccounting settles
// provider jobs on this deployment (client.disable_provider_job_accounting, TXE-6519). Off, a
// request priced at settlement is refused under a dollar budget. The transport sets it wherever
// it wires the accounting sweepers; until then it is off.
func (p *GovernancePlugin) SetProviderJobAccountingEnabled(enabled bool) {
	if p == nil || p.resolver == nil {
		return
	}
	p.resolver.providerJobAccounting.Store(enabled)
}

// ProviderJobAccountingEnabled reports what SetProviderJobAccountingEnabled last set.
func (p *GovernancePlugin) ProviderJobAccountingEnabled() bool {
	return p != nil && p.resolver != nil && p.resolver.providerJobAccounting.Load()
}

// isBackgroundResponsesCreate reports whether a request is a Responses create sent with
// background: true, on the typed request or at the top of a passthrough body.
func isBackgroundResponsesCreate(req *schemas.BifrostRequest) bool {
	if req == nil {
		return false
	}
	switch req.RequestType {
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest:
		return req.ResponsesRequest != nil && req.ResponsesRequest.Params != nil &&
			req.ResponsesRequest.Params.Background != nil && *req.ResponsesRequest.Params.Background
	case schemas.PassthroughRequest, schemas.PassthroughStreamRequest:
		if req.PassthroughRequest == nil || len(req.PassthroughRequest.Body) == 0 {
			return false
		}
		return passthroughIsBackground(req.PassthroughRequest.Path, req.PassthroughRequest.Body)
	}
	return false
}

// passthroughIsBackground reports whether a passthrough request may ask the upstream to run a
// Responses create in the background. background means nothing outside the Responses API, so only
// a path that addresses it is read. On such a path the check fails closed, since the upstream's
// decoder, not gjson, decides what runs:
//   - every top-level key is read: gjson returns the first of a duplicated key and most decoders
//     keep the last, so {"background":false,"background":true} must not pass;
//   - key names compare case-insensitively, as Go's encoding/json matches them;
//   - any value other than false or null counts, since a lenient decoder may coerce one;
//   - a body gjson cannot read as JSON (compressed, UTF-16, malformed) counts: what an upstream
//     makes of it cannot be judged here.
func passthroughIsBackground(path string, body []byte) bool {
	// The upstream decodes the path, so an escaped one (/v1/respons%65s) is read decoded too.
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	if !strings.Contains(strings.ToLower(path), "responses") {
		return false
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	if !gjson.ValidBytes(body) {
		return true
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return false
	}
	background := false
	root.ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.Str, "background") && value.Type != gjson.False && value.Type != gjson.Null {
			background = true
			return false
		}
		return true
	})
	return background
}

// refuseUnsettledUnderBudget refuses a request under a dollar budget whose queued answer nothing
// will charge. Nil when the request is allowed. The caller has checked that there is a budget.
func (r *BudgetResolver) refuseUnsettledUnderBudget(evaluationRequest *EvaluationRequest) *EvaluationResult {
	requestType := evaluationRequest.RequestType
	if evaluationRequest.Background || isBackgroundResponsesCreate(evaluationRequest.request) {
		return &EvaluationResult{
			Decision: DecisionBackgroundUnsettled,
			Reason: fmt.Sprintf("a background %s request cannot be charged to a dollar budget: its answer is delivered by a retrieve that is never priced; send it without background",
				requestType),
		}
	}
	if budgetPricingOf(requestType) == budgetPricingSettled && !r.providerJobAccounting.Load() {
		return &EvaluationResult{
			Decision: DecisionJobUnsettled,
			Reason: fmt.Sprintf("a %s request cannot be charged to a dollar budget: it is priced when its job settles, and provider job accounting is off on this gateway",
				requestType),
		}
	}
	return nil
}
