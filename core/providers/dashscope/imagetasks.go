package dashscope

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

// Wan text-to-image and Wan image edit on the image task routes (fork, TXE-7050, contract C7
// BF16, items B6 and B9). Wan's image API is task-based: a create is one POST to
// image-generation/generation with X-DashScope-Async: enable, and the task is then queried and
// cancelled through DashScope's task API, as Wan video is. The synchronous image routes keep
// refusing a Wan model (checkDashScopeSyncImageModel).

const (
	// dashScopeImageTaskPath is the async create of Wan's new image protocol (wan2.6-t2i and the
	// Wan image generation and edit models): input.messages and parameters, the shape
	// ToDashScopeImageGenerationRequest builds.
	dashScopeImageTaskPath = "/api/v1/services/aigc/image-generation/generation"

	// dashScopeImageLinkLifetime is how long DashScope keeps a task and its result links: 24
	// hours. expires_at is counted from the task's completion (TXE-7050).
	dashScopeImageLinkLifetime = 24 * time.Hour
)

// wanLegacyImagePrefixes are the Wan image models on the old protocol (text2image/image-synthesis,
// results in output.results[].url): wan2.5-t2i-preview and earlier. They are not served on the
// task routes: their body and result shape differ, and the newer models replace them. A request
// naming one is refused before any upstream call.
var wanLegacyImagePrefixes = []string{"wan2.5", "wan2.2", "wan2.1", "wanx"}

// The unified fields the Wan image task creates deliver (contract C4). resolution and size both
// reach DashScope as parameters.size: Wan takes a tier ("1K", "2K", "4K" on wan2.7-image-pro) or
// width*height there. No task control field is declared: DashScope has no callback, external task
// id, deadline or storage target, so core refuses all four before a key is chosen (contract C7).
var (
	dashScopeImageTaskGenerationFields = []string{"n", "size", "resolution", "response_format", "input_images"}
	dashScopeImageTaskEditFields       = []string{"n", "size", "resolution", "response_format"}
)

// checkDashScopeImageTaskModel refuses a model the image task routes cannot serve: anything but a
// Wan image model on the new protocol.
func checkDashScopeImageTaskModel(model string) *schemas.BifrostError {
	m := strings.ToLower(strings.TrimSpace(model))
	switch dashScopeImageFamilyOf(model) {
	case dashScopeImageWan:
		for _, prefix := range wanLegacyImagePrefixes {
			if strings.HasPrefix(m, prefix) {
				return newDashScopeBadRequest("model", fmt.Sprintf(
					"dashscope: model %q is on Wan's earlier image protocol (text2image/image-synthesis), which the image task routes do not serve; use wan2.6-t2i or a later Wan image model", model))
			}
		}
		return nil
	case dashScopeImageQwen, dashScopeImageZ:
		return newDashScopeBadRequest("model", fmt.Sprintf(
			"dashscope: model %q is synchronous; use POST /v1/images/generations or /v1/images/edits", model))
	}
	bifrostErr := providerUtils.NewBifrostBadRequestError(fmt.Sprintf(
		"dashscope: model %q is not a Wan image model the image task routes serve (expected wan2.6-t2i or a later wan* image model)", model))
	bifrostErr.Error.Param = "model"
	bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerModelUnknown
	return bifrostErr
}

// wanImageTaskSize is the parameters.size a Wan image task sends: the resolution tier verbatim,
// or size as width*height. Both set is refused: they name the same parameter.
func wanImageTaskSize(size *string, resolution schemas.TolerantString) (*string, *schemas.BifrostError) {
	tier := strings.TrimSpace(string(resolution))
	hasSize := size != nil && strings.TrimSpace(*size) != ""
	if tier != "" && hasSize {
		return nil, newDashScopeBadRequest("resolution",
			"dashscope: resolution and size both set Wan's parameters.size; send one")
	}
	if tier != "" {
		return &tier, nil
	}
	return nil, nil
}

// validateDashScopeImageTask checks a Wan image task create before any upstream call: the model,
// the shared image parameter rules, and resolution against size.
func validateDashScopeImageTask(model string, n *int, size, responseFormat *string, resolution schemas.TolerantString, extraParams map[string]interface{}) (*string, *schemas.BifrostError) {
	if bifrostErr := checkDashScopeImageTaskModel(model); bifrostErr != nil {
		return nil, bifrostErr
	}
	if bifrostErr := validateDashScopeImageParams(dashScopeImageWan, n, size, responseFormat, extraParams); bifrostErr != nil {
		return nil, bifrostErr
	}
	// An extra parameters object is merged into parameters (GetExtraParams), so one naming n or
	// size would add the field beside, or in place of, the request's own value, and Wan bills per
	// image. Core's nested price-field check does not run for the image task request types yet,
	// so the refusal is made here.
	for name, value := range extraParams {
		object, ok := value.(map[string]interface{})
		if !ok || !providerUtils.ExtraParamNamesField(name, "parameters") {
			continue
		}
		for nested := range object {
			for _, field := range dashScopeImagePriceParameters {
				if schemas.SameJSONFieldName(nested, field) {
					return nil, newDashScopeBadRequest(name, fmt.Sprintf(
						"dashscope: extra params cannot set parameters.%s; send the value in the request's own %s field", nested, field))
				}
			}
		}
	}
	if name := wanInterleaveSwitch(extraParams); name != "" {
		return nil, schemas.NewUnsupportedParameterError(name, fmt.Sprintf(
			"dashscope: %s cannot be delivered on the image task routes: Wan's interleaved mode makes up to max_images images whatever n says, and a task create is billed for the images it asks for", name))
	}
	return wanImageTaskSize(size, resolution)
}

// wanInterleaveSwitches are the extra params that turn on Wan's interleaved text-and-image mode,
// where max_images, not n, sets how many images are made (and billed). A task create is billed for
// the images it asks for at submission (BilledImages), and the query is not billed, so a count the
// create cannot know is refused rather than under-billed.
var wanInterleaveSwitches = []string{"enable_interleave", "max_images"}

// wanInterleaveSwitch returns the name of an interleaved-mode switch among the extra params, at
// the top level or inside an extra parameters object (GetExtraParams puts both in parameters),
// in the caller's spelling; "" when there is none. enable_interleave set to false is not one.
func wanInterleaveSwitch(extraParams map[string]interface{}) string {
	check := func(name string, value interface{}) string {
		for _, field := range wanInterleaveSwitches {
			if !schemas.SameJSONFieldName(name, field) || value == nil {
				continue
			}
			if on, isBool := value.(bool); isBool && !on {
				continue
			}
			return name
		}
		return ""
	}
	for name, value := range extraParams {
		if object, ok := value.(map[string]interface{}); ok && providerUtils.ExtraParamNamesField(name, "parameters") {
			for nested, nestedValue := range object {
				if found := check(nested, nestedValue); found != "" {
					return name + "." + found
				}
			}
			continue
		}
		if found := check(name, value); found != "" {
			return found
		}
	}
	return ""
}

// ImageGenerationTask creates a Wan text-to-image task.
func (provider *DashScopeProvider) ImageGenerationTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: image generation task request is nil")
	}
	if bifrostErr := checkDashScopeImageTaskModel(request.Model); bifrostErr != nil {
		return nil, bifrostErr
	}
	rawBody, bifrostErr := dashScopeRawImageBody(ctx, request, schemas.ImageGenerationTaskRequest)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if rawBody != nil {
		billed, countErr := wanRawImageTaskCount(rawBody)
		if countErr != nil {
			return nil, countErr
		}
		return provider.submitImageTask(ctx, key, schemas.ImageGenerationTaskRequest, request.Model, rawBody, billed)
	}
	var tierSize *string
	var n *int
	if p := request.Params; p != nil {
		n = p.N
		tierSize, bifrostErr = validateDashScopeImageTask(request.Model, p.N, p.Size, p.ResponseFormat, p.Resolution, p.ExtraParams)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		for _, image := range p.InputImages {
			if !isDashScopeImageReference(image) {
				return nil, schemas.NewUnsupportedParameterError("input_images",
					"dashscope takes an input image as an http(s) link or a base64 data URI; bare base64 cannot be delivered")
			}
		}
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			body, err := ToDashScopeImageGenerationRequest(request)
			return withWanImageTaskSize(body, tierSize), err
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.submitImageTask(ctx, key, schemas.ImageGenerationTaskRequest, request.Model, jsonData, wanImageTaskCount(n))
}

// ImageEditTask creates a Wan image edit task. Input images go in input.messages[].content[].image:
// a link unchanged, bytes as a base64 data URI (ToDashScopeImageEditRequest).
func (provider *DashScopeProvider) ImageEditTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	if request == nil {
		return nil, providerUtils.NewBifrostBadRequestError("dashscope: image edit task request is nil")
	}
	if bifrostErr := checkDashScopeImageTaskModel(request.Model); bifrostErr != nil {
		return nil, bifrostErr
	}
	rawBody, bifrostErr := dashScopeRawImageBody(ctx, request, schemas.ImageEditTaskRequest)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if rawBody != nil {
		billed, countErr := wanRawImageTaskCount(rawBody)
		if countErr != nil {
			return nil, countErr
		}
		return provider.submitImageTask(ctx, key, schemas.ImageEditTaskRequest, request.Model, rawBody, billed)
	}
	var tierSize *string
	var n *int
	if p := request.Params; p != nil {
		n = p.N
		tierSize, bifrostErr = validateDashScopeImageTask(request.Model, p.N, p.Size, p.ResponseFormat, p.Resolution, p.ExtraParams)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
	}
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			body, err := ToDashScopeImageEditRequest(request)
			return withWanImageTaskSize(body, tierSize), err
		})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.submitImageTask(ctx, key, schemas.ImageEditTaskRequest, request.Model, jsonData, wanImageTaskCount(n))
}

// wanDefaultImageCount is the number of images Wan makes when a create names no n: Alibaba's Wan
// text-to-image reference documents n as defaulting to 4, and each image is billed.
const wanDefaultImageCount = 4

// wanImageTaskCount is the number of images a Wan task create asks DashScope for, and so bills:
// n, or Wan's default when the request names none.
func wanImageTaskCount(n *int) int {
	if n != nil && *n > 0 {
		return *n
	}
	return wanDefaultImageCount
}

// wanRawImageTaskCount reads parameters.n from a body the caller wrote whole (large payload
// passthrough). A body whose n Bifrost cannot read is counted at Wan's default. A body that turns
// on Wan's interleaved mode is refused, as on the converted path (wanInterleaveSwitch).
func wanRawImageTaskCount(body []byte) (int, *schemas.BifrostError) {
	var raw struct {
		Parameters *struct {
			N                *int            `json:"n"`
			EnableInterleave json.RawMessage `json:"enable_interleave"`
			MaxImages        json.RawMessage `json:"max_images"`
		} `json:"parameters"`
	}
	if json.Unmarshal(body, &raw) != nil || raw.Parameters == nil {
		return wanDefaultImageCount, nil
	}
	extra := map[string]interface{}{}
	for name, value := range map[string]json.RawMessage{"enable_interleave": raw.Parameters.EnableInterleave, "max_images": raw.Parameters.MaxImages} {
		var decoded interface{}
		if len(value) > 0 && json.Unmarshal(value, &decoded) == nil {
			extra[name] = decoded
		}
	}
	if name := wanInterleaveSwitch(extra); name != "" {
		return 0, schemas.NewUnsupportedParameterError("parameters."+name, fmt.Sprintf(
			"dashscope: parameters.%s cannot be delivered on the image task routes: Wan's interleaved mode makes up to max_images images whatever n says, and a task create is billed for the images it asks for", name))
	}
	return wanImageTaskCount(raw.Parameters.N), nil
}

// withWanImageTaskSize writes the resolution tier into parameters.size. A size set from the
// request's own size field is already there, converted to width*height.
func withWanImageTaskSize(body *DashScopeImageRequest, tierSize *string) *DashScopeImageRequest {
	if body == nil || tierSize == nil {
		return body
	}
	if body.Parameters == nil {
		body.Parameters = &DashScopeImageParameters{}
	}
	body.Parameters.Size = tierSize
	return body
}

// submitImageTask sends the one create and returns the task object DashScope's answer gives.
// billedImages is the number of images the create asks for, which the create is billed for
// (BifrostImageTaskResponse.BilledImages).
func (provider *DashScopeProvider) submitImageTask(ctx *schemas.BifrostContext, key schemas.Key, requestType schemas.RequestType, model string, jsonData []byte, billedImages int) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	submittedAt := time.Now().Unix()
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, requestType, http.MethodPost, dashScopeImageTaskPath, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var task DashScopeImageTaskResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, jsonData, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if task.Output == nil || task.Output.TaskID == "" {
		if task.Code != "" {
			taskErr := &DashScopeTaskResponse{Code: task.Code, Message: task.Message}
			return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(taskErr), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("dashscope returned no task_id", nil), jsonData, body, sendBackRawRequest, sendBackRawResponse, latency)
	}
	result := toBifrostImageTask(&task)
	result.ID = providerUtils.AddVideoIDProviderSuffix(task.Output.TaskID, provider.GetProviderKey())
	result.Model = model
	result.BilledImages = billedImages
	if result.CreatedAt == 0 {
		// The create response carries no submit_time; a later query reports the vendor's.
		result.CreatedAt = submittedAt
	}
	result.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		result.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		result.ExtraFields.RawResponse = rawResponse
	}
	return result, nil
}

// ImageTaskRetrieve queries a task once: GET /api/v1/tasks/{id}.
func (provider *DashScopeProvider) ImageTaskRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageTaskRetrieveRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "task_id")
	if idErr != nil {
		return nil, idErr
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, schemas.ImageTaskRetrieveRequest, http.MethodGet, dashScopeTasksPath+"/"+escapedTaskID, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var task DashScopeImageTaskResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &task, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if task.Output == nil {
		if task.Code != "" {
			taskErr := &DashScopeTaskResponse{Code: task.Code, Message: task.Message}
			return nil, providerUtils.EnrichError(ctx, dashScopeErrorObjectError(taskErr), nil, body, false, sendBackRawResponse, latency)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("dashscope returned no task", nil), nil, body, false, sendBackRawResponse, latency)
	}
	result := toBifrostImageTask(&task)
	result.ID = providerUtils.AddVideoIDProviderSuffix(taskID, providerName)
	result.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		result.ExtraFields.RawResponse = rawResponse
	}
	return result, nil
}

// ImageTaskCancel cancels a task with one call: POST /api/v1/tasks/{id}/cancel. DashScope cancels
// only a PENDING task; for any other state it answers 400 UnsupportedOperation, returned as
// provider_cancel_unsupported. The cancel deletes nothing, so a finished task's images stay.
func (provider *DashScopeProvider) ImageTaskCancel(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageTaskCancelRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	providerName := provider.GetProviderKey()
	taskID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	escapedTaskID, idErr := providerUtils.EscapeResourceID(taskID, "task_id")
	if idErr != nil {
		return nil, idErr
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doDashScopeRequest(ctx, key, schemas.ImageTaskCancelRequest, http.MethodPost, dashScopeTasksPath+"/"+escapedTaskID+"/cancel", nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		if isDashScopeCancelRefused(bifrostErr) {
			unsupported := schemas.NewImageTaskCancelUnsupportedError(bifrostErr.Error.Message)
			unsupported.ExtraFields.RawResponse = bifrostErr.ExtraFields.RawResponse
			unsupported.ExtraFields.Latency = bifrostErr.ExtraFields.Latency
			return nil, unsupported
		}
		return nil, bifrostErr
	}
	// DashScope's answer to a cancel is a request id and no task: the task object carries the id
	// and status cancelled, and no provider_status, since the vendor reported no state.
	result := &schemas.BifrostImageTaskResponse{
		ID:     providerUtils.AddVideoIDProviderSuffix(taskID, providerName),
		Object: schemas.ImageTaskObject,
		Status: schemas.VideoStatusCancelled,
		Data:   []schemas.TaskArtifact{},
	}
	result.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse && len(body) > 0 {
		var rawResponse interface{}
		if err := schemas.Unmarshal(body, &rawResponse); err == nil {
			result.ExtraFields.RawResponse = rawResponse
		} else {
			result.ExtraFields.RawResponse = string(body)
		}
	}
	return result, nil
}

// toBifrostImageTask converts a Wan image task (a create answer or a query) to the task object,
// with no id suffix and no model: DashScope's task carries neither. The states map one to one, as
// for video (dashScopeVideoStatuses).
func toBifrostImageTask(task *DashScopeImageTaskResponse) *schemas.BifrostImageTaskResponse {
	output := task.Output
	result := &schemas.BifrostImageTaskResponse{
		Object:         schemas.ImageTaskObject,
		ProviderStatus: output.TaskStatus,
		CreatedAt:      parseDashScopeTaskTime(output.SubmitTime),
		Data:           []schemas.TaskArtifact{},
	}
	status, ok := dashScopeVideoStatuses.Lookup(output.TaskStatus)
	if !ok {
		// A state DashScope added that Bifrost does not know is not known to be terminal.
		status = schemas.VideoStatusInProgress
	}
	result.Status = status
	if status.IsTerminal() {
		if endTime := parseDashScopeTaskTime(output.EndTime); endTime > 0 {
			result.CompletedAt = schemas.Ptr(endTime)
		}
	}
	if output.TaskStatus == dashScopeTaskFailed && (output.Code != "" || output.Message != "") {
		result.Error = &schemas.TaskError{Code: output.Code, Message: output.Message}
		for _, code := range dashScopeContentFilterCodes {
			if output.Code == code {
				result.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: []string{output.Code}}
			}
		}
	}
	if status == schemas.VideoStatusCompleted {
		for _, choice := range output.Choices {
			for _, part := range choice.Message.Content {
				if part.Image != "" {
					result.Data = append(result.Data, schemas.TaskArtifact{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(part.Image), ContentType: wanImageContentType(part.Image)})
				}
			}
		}
		// The links live 24 hours from the task's completion.
		if result.CompletedAt != nil {
			result.ExpiresAt = schemas.Ptr(*result.CompletedAt + int64(dashScopeImageLinkLifetime/time.Second))
		}
	}
	if usage := task.Usage; usage != nil && usage.ImageCount != nil {
		result.Usage = &schemas.ImageUsage{OutputImages: usage.ImageCount}
	}
	return result
}

// wanImageContentType is a result link's image type, read from its path's extension, and PNG when
// the path names no extension Bifrost knows.
func wanImageContentType(link string) string {
	if parsed, err := url.Parse(link); err == nil {
		switch strings.ToLower(path.Ext(parsed.Path)) {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".webp":
			return "image/webp"
		}
	}
	return "image/png"
}
