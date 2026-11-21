package governance

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"reflect"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
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

// isDeferredDelivery reports whether a request asks for its answer to be delivered later, where no
// path through Bifrost charges it (TXE-7963, TXE-8013): a Responses create sent with background:
// true, a Gemini/Vertex Interactions create in the background, a Runware task delivered async or to a
// webhook, or any request charged on its answer that asks the provider to post the result to a
// webhook or callback. It reads the typed request, its extra params (forwarded to the provider), and
// a raw request body forwarded as sent (bifrost-use-raw-request-body). baseProvider is the provider
// whose implementation serves the request (a custom provider's base type).
//
// A job priced at settlement (video, batch) and an image task priced at its create are charged
// whatever their delivery, so a callback on them is not read here.
func isDeferredDelivery(req *schemas.BifrostRequest, baseProvider schemas.ModelProvider) bool {
	if req == nil {
		return false
	}
	capable := deferredDeliveryProviders[baseProvider]
	switch req.RequestType {
	case schemas.PassthroughRequest, schemas.PassthroughStreamRequest:
		if req.PassthroughRequest == nil || len(req.PassthroughRequest.Body) == 0 {
			return false
		}
		return passthroughDefersDelivery(req.PassthroughRequest.Path, req.PassthroughRequest.Body, capable, req.ForwardedBodyContentType())
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest, schemas.WebSocketResponsesRequest:
		if req.ResponsesRequest != nil && req.ResponsesRequest.Params != nil &&
			req.ResponsesRequest.Params.Background != nil && *req.ResponsesRequest.Params.Background {
			return true
		}
	}
	if budgetPricingOf(req.RequestType) != budgetPricingAnswer || queuedAnswerRequestTypes[req.RequestType] == queuedAnswerPricedAtCreate {
		return false
	}
	extra, raw := requestForwardedInputs(req)
	// A raw body is read as a passthrough body to the request's own route.
	route := ""
	if queuedAnswerRequestTypes[req.RequestType] == queuedAnswerBackgroundRefused {
		route = "/responses"
	}
	if len(raw) > 0 && passthroughDefersDelivery(route, raw, capable, "") {
		return true
	}
	if extraNamesDelivery(extra) {
		if encoded, err := providerUtils.MarshalSorted(extra); err == nil {
			return unitDefersDelivery(gjson.ParseBytes(encoded), capable || route != "")
		}
		return true // extra params that cannot be read are not judged foreground
	}
	return false
}

// requestForwardedInputs returns what a typed request forwards to the provider beyond its typed
// fields: the extra params of whichever request it carries, and the raw body it forwards as sent
// (schemas.BifrostRequest.ForwardedBody, which its own test keeps complete). Extra params are
// found through extraParamsFields, built once from the request types, so a request type added
// later is covered without editing this function and a request pays no field-name lookup.
func requestForwardedInputs(req *schemas.BifrostRequest) (map[string]interface{}, []byte) {
	raw := req.ForwardedBody()
	v := reflect.ValueOf(req).Elem()
	for _, f := range extraParamsFields {
		inner := v.Field(f.request)
		if inner.IsNil() {
			continue
		}
		params, err := inner.Elem().FieldByIndexErr(f.params)
		if err != nil || params.IsNil() {
			return nil, raw
		}
		value, err := params.Elem().FieldByIndexErr(f.extra)
		if err != nil {
			return nil, raw
		}
		extra, _ := value.Interface().(map[string]interface{})
		return extra, raw
	}
	return nil, raw
}

// extraParamsField locates Params.ExtraParams on one request variant of schemas.BifrostRequest.
type extraParamsField struct {
	request       int
	params, extra []int
}

// extraParamsFields lists, for every request variant whose Params carry ExtraParams, the field
// indexes that reach them.
var extraParamsFields = func() []extraParamsField {
	var fields []extraParamsField
	t := reflect.TypeOf(schemas.BifrostRequest{})
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i).Type
		if ft.Kind() != reflect.Pointer || ft.Elem().Kind() != reflect.Struct {
			continue
		}
		params, ok := ft.Elem().FieldByName("Params")
		if !ok || params.Type.Kind() != reflect.Pointer || params.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		extra, ok := params.Type.Elem().FieldByName("ExtraParams")
		if !ok || extra.Type != reflect.TypeOf(map[string]interface{}{}) {
			continue
		}
		fields = append(fields, extraParamsField{request: i, params: params.Index, extra: extra.Index})
	}
	return fields
}()

// backgroundCapablePaths name the passthrough APIs that run a create in the background when
// asked: OpenAI-style Responses, and the Gemini/Vertex Interactions API (gemini.IsInteractionsPassthroughPath).
var backgroundCapablePaths = []string{"responses", "/interactions"}

// deferredDeliveryProviders are the providers whose every passthrough body may ask for deferred
// delivery, whatever its path: Runware runs any task async (deliveryMethod) or posts it to a
// webhook (webhookURL), and its answer is then a queued acknowledgment that governance does not
// count as an answer (passthroughQueuedAcknowledgment), so nothing charges the task.
var deferredDeliveryProviders = map[schemas.ModelProvider]bool{
	schemas.Runware: true,
}

// passthroughDefersDelivery reports whether a passthrough body may ask the upstream to deliver its
// answer later. It reads a JSON object, or each object of a JSON array (Runware's task list). The
// upstream's decoder, not gjson, decides what runs, so every key is read (gjson returns the first of
// a duplicated key, most decoders keep the last) and key names compare case-insensitively, as Go's
// encoding/json matches them. A unit defers delivery when:
//   - deliveryMethod is anything but "sync" or null (Runware: async);
//   - webhookURL is a non-empty value (Runware posts the result to it);
//   - background is true or "true"; or, on a capable path or provider, anything but false or null.
//
// On a capable path or provider (capableProvider) the check fails closed: a body gjson cannot read as
// JSON (compressed, UTF-16, malformed) counts. Elsewhere a top-level background can mean something
// else, such as an image background ("transparent"), and a non-JSON body (a multipart upload) is not
// a create that could be deferred.
func passthroughDefersDelivery(path string, body []byte, capableProvider bool, contentType string) bool {
	// The upstream decodes the path, so an escaped one (/v1/respons%65s) is read decoded too.
	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	path = strings.ToLower(path)
	capable := capableProvider
	for _, marker := range backgroundCapablePaths {
		if strings.Contains(path, marker) {
			capable = true
			break
		}
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	if !gjson.ValidBytes(body) {
		// A form body (multipart/form-data or urlencoded) carries its fields as form fields: an
		// ElevenLabs speech-to-text upload sends webhook=true that way. They are read with the same
		// rule; a body that is neither JSON nor a readable form fails closed where deferral is known.
		if fields, isForm, ok := formFields(body, contentType); isForm {
			if !ok {
				// A form the caller declared (or opened with a boundary) that does not parse cannot
				// be judged, and a vendor's lenient decoder may still read a webhook from it.
				return true
			}
			for _, field := range fields {
				if unitDefersDelivery(field, capable) {
					return true
				}
			}
			return false
		}
		return capable
	}
	root := gjson.ParseBytes(body)
	switch {
	case root.IsObject():
		return unitDefersDelivery(root, capable)
	case root.IsArray():
		deferred := false
		root.ForEach(func(_, unit gjson.Result) bool {
			if unit.IsObject() && unitDefersDelivery(unit, capable) {
				deferred = true
				return false
			}
			return true
		})
		return deferred
	}
	return false
}

// maxFormFieldBytes bounds the value of one delivery field read from a form. A longer value is not
// truncated, which could hide a non-empty value behind padding: the form is judged deferred.
const maxFormFieldBytes = 1 << 20

// deliveryFieldName reports whether unitDefersDelivery reads a field of this name. Only those
// fields' values are read from a form.
func deliveryFieldName(name string) bool {
	return strings.EqualFold(name, "taskType") || strings.EqualFold(name, "deliveryMethod") ||
		strings.EqualFold(name, "background") || deferredDeliveryKey(name)
}

// formFields reads the delivery fields (deliveryFieldName) of a form body as single-field JSON
// objects, one per occurrence, so a field given twice is judged at each. isForm reports a multipart/form-data body (declared, read
// by its boundary; or undeclared and opening with a boundary line) or a declared
// application/x-www-form-urlencoded one; ok is false when such a body does not parse. A file part
// is skipped: its name is the upload, not a setting. "true" and "false" are read as booleans, as the
// vendor's form decoder reads them.
func formFields(body []byte, contentType string) (fields []gjson.Result, isForm, ok bool) {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	boundary := params["boundary"]
	if mediaType == "" && bytes.HasPrefix(body, []byte("--")) {
		if line, _, found := bytes.Cut(body[2:], []byte("\n")); found {
			mediaType, boundary = "multipart/form-data", strings.TrimRight(string(line), "\r")
		}
	}
	// field records one delivery field; false when it cannot be encoded, which fails the form closed.
	field := func(name, value string) bool {
		var v interface{} = value
		switch {
		case strings.EqualFold(value, "true"):
			v = true
		case strings.EqualFold(value, "false"):
			v = false
		}
		encoded, err := providerUtils.MarshalSorted(map[string]interface{}{name: v})
		if err != nil {
			return false
		}
		fields = append(fields, gjson.ParseBytes(encoded))
		return true
	}
	switch mediaType {
	case "multipart/form-data":
		if boundary == "" {
			return nil, true, false
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return fields, true, true
			}
			if err != nil {
				return nil, true, false
			}
			if part.FileName() != "" || !deliveryFieldName(part.FormName()) {
				continue
			}
			text, err := io.ReadAll(io.LimitReader(part, maxFormFieldBytes+1))
			if err != nil || len(text) > maxFormFieldBytes {
				return nil, true, false
			}
			if !field(part.FormName(), string(text)) {
				return nil, true, false
			}
		}
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, true, false
		}
		for name, all := range values {
			if !deliveryFieldName(name) {
				continue
			}
			for _, value := range all {
				if !field(name, value) {
					return nil, true, false
				}
			}
		}
		return fields, true, true
	}
	return nil, false, false
}

// deferredDeliveryKeys are the request keys, compared case-insensitively, that send a result to a
// webhook or callback instead of the answer: Runware webhookURL, Replicate and ElevenLabs webhook,
// BFL webhook_url, Kling, MiniMax and BytePlus callback_url, Gemini Interactions webhook_config.
// TestEveryProviderDeliveryFieldIsReadOrExempt fails when a provider request type grows another.
var deferredDeliveryKeys = []string{"webhookURL", "webhook", "webhook_url", "callback_url", "callbackURL", "webhook_config"}

func deferredDeliveryKey(key string) bool {
	for _, k := range deferredDeliveryKeys {
		if strings.EqualFold(key, k) {
			return true
		}
	}
	return false
}

// extraNamesDelivery reports whether extra params carry any key unitDefersDelivery reads. Only then
// are they encoded and judged, so a request whose extra params name nothing about delivery (the
// common case: service_tier, a vendor flag) pays a key scan and no allocation.
func extraNamesDelivery(extra map[string]interface{}) bool {
	for key := range extra {
		if deliveryFieldName(key) {
			return true
		}
	}
	return false
}

func unitDefersDelivery(unit gjson.Result, capable bool) bool {
	deferred := false
	// On a capable provider a task that runs async by default (Runware's videoInference) defers
	// delivery unless it asks for sync explicitly: a missing deliveryMethod is not sync for it.
	asyncByDefault, explicitSync := false, false
	unit.ForEach(func(key, value gjson.Result) bool {
		switch {
		case strings.EqualFold(key.Str, "taskType"):
			asyncByDefault = asyncByDefault || (capable && value.Type == gjson.String && strings.Contains(strings.ToLower(value.Str), "video"))
		case strings.EqualFold(key.Str, "deliveryMethod"):
			explicitSync = value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.Str), "sync")
			deferred = value.Type != gjson.Null && !explicitSync
		case deferredDeliveryKey(key.Str):
			// A webhook or callback destination, or a webhook switch: the provider posts the result
			// there, and the answer Bifrost relays carries none of it.
			deferred = value.Type != gjson.Null && value.Type != gjson.False && !(value.Type == gjson.String && strings.TrimSpace(value.Str) == "")
		case strings.EqualFold(key.Str, "background"):
			truthy := value.Type == gjson.True || (value.Type == gjson.String && strings.EqualFold(value.Str, "true"))
			deferred = truthy || (capable && value.Type != gjson.False && value.Type != gjson.Null)
		}
		return !deferred
	})
	return deferred || (asyncByDefault && !explicitSync)
}

// passthroughEscapedQueue is the after-the-fact half of the deferred-delivery gate (TXE-8013): a
// passthrough answered with only a queued acknowledgment (passthroughQueuedAcknowledgment) under a
// dollar budget means the request asked for deferred delivery in a way the gate did not recognise,
// and nothing will charge the work. It is logged, and the caller treats the answer as priced at $0,
// so the model is refused under its budgets from now on (noteUnpricedAnswer).
func passthroughEscapedQueue(result *schemas.BifrostResponse, logger schemas.Logger, requestID string) bool {
	if result == nil || result.PassthroughResponse == nil {
		return false
	}
	status := result.PassthroughResponse.StatusCode
	if status < 200 || status >= 300 || !passthroughQueuedAcknowledgment(result.PassthroughResponse.Body) {
		return false
	}
	if logger != nil {
		logger.Warn("governance: a passthrough under a dollar budget was answered with a queued acknowledgment (TXE-8013): request %s asked for deferred delivery the gate did not recognise; its model is refused under the budget from now on",
			requestID)
	}
	return true
}

// refuseUnsettledUnderBudget refuses a request under a dollar budget whose queued answer nothing
// will charge. Nil when the request is allowed. The caller has checked that there is a budget.
func (r *BudgetResolver) refuseUnsettledUnderBudget(evaluationRequest *EvaluationRequest) *EvaluationResult {
	requestType := evaluationRequest.RequestType
	if evaluationRequest.Background || (evaluationRequest.request != nil &&
		isDeferredDelivery(evaluationRequest.request, configuredBaseProvider(r.governanceInMemoryStore, evaluationRequest.Provider))) {
		return &EvaluationResult{
			Decision: DecisionBackgroundUnsettled,
			Reason: fmt.Sprintf("a %s request that asks for its answer later (background, async or webhook delivery) cannot be charged to a dollar budget: nothing charges the answer when it arrives; ask for it synchronously",
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
