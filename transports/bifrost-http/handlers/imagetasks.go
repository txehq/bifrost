package handlers

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// The image task routes (fork, TXE-6431, contract C2 and C7, work item BF16), for vendors whose
// image API is itself task-based (BFL). The two creates take the bodies of /v1/images/generations
// and /v1/images/edits, plus the four task control fields; every route answers with the image
// task object. Bifrost stores no task: each route is one call to the vendor.

// takeTaskControl moves the task control fields out of a create's extra params into their typed
// form, so they never reach a vendor as unconverted extras. The sync image routes do not call it:
// there a field of the same name stays an extra param, as before.
func takeTaskControl(extra map[string]any) (*schemas.TaskControlParameters, error) {
	var control schemas.TaskControlParameters
	text := func(name string) (*string, error) {
		raw, ok := extra[name]
		if !ok {
			return nil, nil
		}
		delete(extra, name)
		value, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a string", name)
		}
		return &value, nil
	}
	var err error
	if control.CallbackURL, err = text(schemas.TaskControlFieldCallbackURL); err != nil {
		return nil, err
	}
	if control.ClientReferenceID, err = text(schemas.TaskControlFieldClientReferenceID); err != nil {
		return nil, err
	}
	if control.OutputStorageURI, err = text(schemas.TaskControlFieldOutputStorageURI); err != nil {
		return nil, err
	}
	if raw, ok := extra[schemas.TaskControlFieldExpiresAfterSeconds]; ok {
		delete(extra, schemas.TaskControlFieldExpiresAfterSeconds)
		seconds, ok := wholeSeconds(raw)
		if !ok {
			return nil, fmt.Errorf("%s must be a whole number of seconds", schemas.TaskControlFieldExpiresAfterSeconds)
		}
		control.ExpiresAfterSeconds = &seconds
	}
	if len(control.Present()) == 0 {
		return nil, nil
	}
	return &control, nil
}

// wholeSeconds reads a whole number from JSON (a float64) or from a multipart form (a string).
func wholeSeconds(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		if value != math.Trunc(value) || value > math.MaxInt32 || value < math.MinInt32 {
			return 0, false
		}
		return int(value), true
	case string:
		seconds, err := strconv.Atoi(strings.TrimSpace(value))
		return seconds, err == nil
	}
	return 0, false
}

// imageGenerationTask handles POST /v1/images/generations/tasks.
func (h *CompletionHandler) imageGenerationTask(ctx *fasthttp.RequestCtx) {
	req, bifrostReq, err := prepareImageGenerationRequest(ctx, h.config)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	if req.BifrostParams.Stream != nil && *req.BifrostParams.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported on image task routes")
		return
	}
	control, err := takeTaskControl(bifrostReq.Params.ExtraParams)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	bifrostReq.TaskControl = control
	h.runImageTask(ctx, func(bifrostCtx *schemas.BifrostContext) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
		return h.client.ImageGenerationTaskRequest(bifrostCtx, bifrostReq)
	})
}

// imageEditTask handles POST /v1/images/edits/tasks, JSON or multipart like /v1/images/edits.
func (h *CompletionHandler) imageEditTask(ctx *fasthttp.RequestCtx) {
	req, bifrostReq, err := prepareImageEditRequest(ctx, h.config)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	if req.Stream != nil && *req.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported on image task routes")
		return
	}
	control, err := takeTaskControl(bifrostReq.Params.ExtraParams)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	bifrostReq.TaskControl = control
	h.runImageTask(ctx, func(bifrostCtx *schemas.BifrostContext) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
		return h.client.ImageEditTaskRequest(bifrostCtx, bifrostReq)
	})
}

// imageTaskRetrieve handles GET /v1/images/tasks/{task_id}: one query of the vendor.
func (h *CompletionHandler) imageTaskRetrieve(ctx *fasthttp.RequestCtx) {
	ref, ok := imageTaskReference(ctx)
	if !ok {
		return
	}
	h.runImageTask(ctx, func(bifrostCtx *schemas.BifrostContext) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
		return h.client.ImageTaskRetrieveRequest(bifrostCtx, ref)
	})
}

// imageTaskCancel handles POST /v1/images/tasks/{task_id}/cancel. A provider with no cancel API
// answers 400 with extra_fields.error_type provider_cancel_unsupported. The request takes no body.
func (h *CompletionHandler) imageTaskCancel(ctx *fasthttp.RequestCtx) {
	ref, ok := imageTaskReference(ctx)
	if !ok {
		return
	}
	h.runImageTask(ctx, func(bifrostCtx *schemas.BifrostContext) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
		return h.client.ImageTaskCancelRequest(bifrostCtx, ref)
	})
}

// imageTaskReference reads {task_id}, "{URL-encoded vendor task id}:{provider}", as the video
// routes read theirs. It has answered 400 when ok is false.
func imageTaskReference(ctx *fasthttp.RequestCtx) (*schemas.BifrostImageTaskReferenceRequest, bool) {
	taskID, ok := ctx.UserValue("task_id").(string)
	if !ok || taskID == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "task_id is required")
		return nil, false
	}
	decodedID, err := url.PathUnescape(taskID)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, "invalid task_id encoding")
		return nil, false
	}
	idParts := strings.SplitN(decodedID, ":", 2)
	if len(idParts) != 2 || idParts[0] == "" || idParts[1] == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "task_id must be in id:provider format")
		return nil, false
	}
	return &schemas.BifrostImageTaskReferenceRequest{Provider: schemas.ModelProvider(idParts[1]), ID: idParts[0]}, true
}

// runImageTask converts the context, runs call and writes the task object or the error.
func (h *CompletionHandler) runImageTask(ctx *fasthttp.RequestCtx, call func(*schemas.BifrostContext) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError)) {
	bifrostCtx, cancel := lib.ConvertToBifrostContext(ctx, h.config)
	defer cancel()
	if bifrostCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	resp, bifrostErr := call(bifrostCtx)
	if bifrostErr != nil {
		forwardProviderHeadersFromContext(ctx, bifrostCtx)
		SendBifrostError(ctx, bifrostErr)
		return
	}
	if resp != nil {
		lib.ApplyBifrostResponseHeaders(ctx, bifrostCtx, resp.ExtraFields)
	}
	if streamLargeResponseIfActive(ctx, bifrostCtx) {
		return
	}
	SendJSON(ctx, resp)
}
