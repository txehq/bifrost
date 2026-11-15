package schemas

// Image tasks (fork, TXE-6431, contract C2 and C7, work item BF16).
//
// Some vendors serve images only as tasks: a submit returns a task id, and the result is polled
// (BFL). The four image task routes give those vendors the same task object as video:
//
//	POST /v1/images/generations/tasks   the body of POST /v1/images/generations
//	POST /v1/images/edits/tasks         the body of POST /v1/images/edits
//	GET  /v1/images/tasks/{id}          one query of the vendor
//	POST /v1/images/tasks/{id}/cancel   the vendor's cancel API, or 400 provider_cancel_unsupported
//
// Bifrost stores no task: create, query and cancel are each one call to the vendor, and the task
// id ({URL-encoded vendor task id}:{provider}) carries whatever the provider needs to query it
// again. The fork's /v1/async/images/* routes, which store jobs in Bifrost's database, are a
// different thing and are not used by these routes.

import (
	"strconv"
	"strings"
)

// TaskStatus is the status of a task object. Image and video tasks share the six values.
type TaskStatus = VideoStatus

// TaskError is the error of a failed task: the vendor's own code and message, verbatim.
type TaskError = VideoCreateError

// TaskArtifact is one artifact of a task object: {type: "url", url, content_type} for a public
// link, {type: "content", content_type} for one only the vendor's key can fetch (contract C9).
// Image and video tasks share the shape.
type TaskArtifact = VideoOutput

// BifrostImageTaskResponse is the image task object, the response of all four image task routes.
// It has the shape of the video task object, with the artifacts in data[].
type BifrostImageTaskResponse struct {
	ID             string             `json:"id"`                        // {URL-encoded vendor task id}:{provider}
	Object         string             `json:"object"`                    // always "image.task"
	Model          string             `json:"model,omitempty"`           // the model the task was created with, when known
	Status         TaskStatus         `json:"status"`                    // queued, in_progress, completed, failed, cancelled, expired
	ProviderStatus string             `json:"provider_status,omitempty"` // the vendor's raw status string, verbatim
	CreatedAt      int64              `json:"created_at,omitempty"`      // Unix seconds
	CompletedAt    *int64             `json:"completed_at,omitempty"`    // Unix seconds, the vendor's completion time
	ExpiresAt      *int64             `json:"expires_at,omitempty"`      // Unix seconds, when the artifact links stop working
	Data           []TaskArtifact     `json:"data"`                      // the delivered images; a blocked image takes no slot
	Error          *TaskError         `json:"error,omitempty"`           // the vendor's code and message when the task failed
	ContentFilter  *ContentFilterInfo `json:"content_filter,omitempty"`  // required when the vendor blocked the task (contract C6)
	Usage          *ImageUsage        `json:"usage,omitempty"`           // what the vendor reported: vendor_cost, megapixels

	// BilledImages is the number of images a create asked the vendor for, which the vendor bills
	// (TXE-7050): Bifrost's own count, never serialised, since usage carries only what the vendor
	// reported (contract C5). The cost path bills a create for this many images when it is more
	// than the delivered ones. 0 when the provider makes one image per task (BFL refuses n != 1).
	// A provider whose create takes n, or makes several images by default, must set it.
	BilledImages int `json:"-"`

	// BilledParameters are the pricing dimensions a create asked for (size, quality, aspect ratio,
	// resolution tier, steps), never serialised. Core fills them from the request on every image
	// task create (BackfillCreateParams), as BackfillParams does for a sync image response, so the
	// create, the only image task response that is billed, is priced at the tier it asked for.
	BilledParameters *ImageGenerationResponseParameters `json:"-"`

	ExtraFields BifrostResponseExtraFields `json:"extra_fields"`
}

// BackfillCreateParams fills BilledParameters from the create request req (an image generation or
// edit task) with the same reading a sync image response gets (BifrostImageGenerationResponse
// .BackfillParams). Nothing it writes is serialised: the task object's wire shape is unchanged.
// A provider that set BilledParameters itself, from a raw body core never parsed, is left alone.
func (r *BifrostImageTaskResponse) BackfillCreateParams(req *BifrostRequest) {
	if r == nil || req == nil || r.BilledParameters != nil {
		return
	}
	reading := &BifrostImageGenerationResponse{}
	reading.BackfillParams(req)
	r.BilledParameters = reading.ImageGenerationResponseParameters
	if r.BilledParameters != nil {
		r.BilledParameters.Size = normalizeBilledImageSize(r.BilledParameters.Size)
	}
}

// normalizeBilledImageSize writes a width-and-height size the way the pricing path reads it,
// "2048x2048": a provider may accept "2048*2048" or "2048X2048" and send it, and the tier and
// pixel pricing parse only a lower-case x. Anything else is kept as it is.
func normalizeBilledImageSize(size string) string {
	trimmed := strings.TrimSpace(size)
	sep := strings.IndexAny(trimmed, "xX*")
	if sep <= 0 || sep == len(trimmed)-1 {
		return size
	}
	// Parsed as the providers parse it (strconv.Atoi, positive), so "+2048" and "02048" are 2048
	// here as they are on the wire, and written back in canonical form.
	width, errW := strconv.Atoi(trimmed[:sep])
	height, errH := strconv.Atoi(trimmed[sep+1:])
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return size
	}
	return strconv.Itoa(width) + "x" + strconv.Itoa(height)
}

// ImageTaskObject is the value of BifrostImageTaskResponse.Object.
const ImageTaskObject = "image.task"

// BifrostImageTaskReferenceRequest names the image task a query or a cancel addresses. ID is the
// vendor part of the task id, with the ":{provider}" suffix removed by the route.
type BifrostImageTaskReferenceRequest struct {
	Provider ModelProvider `json:"provider"`
	ID       string        `json:"id"`
}

// BifrostImageTaskRetrieveRequest is GET /v1/images/tasks/{id}.
type BifrostImageTaskRetrieveRequest = BifrostImageTaskReferenceRequest

// BifrostImageTaskCancelRequest is POST /v1/images/tasks/{id}/cancel.
type BifrostImageTaskCancelRequest = BifrostImageTaskReferenceRequest

// ImageTaskProvider is implemented by a provider that serves images as tasks. It is not part of
// the Provider interface: a provider without it answers the task routes with 400
// unsupported_operation and makes no upstream call.
//
// Each method is one call to the vendor. The two creates receive the same request as the sync
// routes, with the task control fields in request.TaskControl; a field the provider does not
// declare in SupportedUnifiedFields for the task request type has already been refused.
type ImageTaskProvider interface {
	ImageGenerationTask(ctx *BifrostContext, key Key, request *BifrostImageGenerationRequest) (*BifrostImageTaskResponse, *BifrostError)
	ImageEditTask(ctx *BifrostContext, key Key, request *BifrostImageEditRequest) (*BifrostImageTaskResponse, *BifrostError)
	ImageTaskRetrieve(ctx *BifrostContext, key Key, request *BifrostImageTaskRetrieveRequest) (*BifrostImageTaskResponse, *BifrostError)
}

// ImageTaskCanceller is implemented by a provider whose vendor has a cancel API for image tasks.
// A provider without it answers the cancel route with 400 provider_cancel_unsupported before a
// key is chosen. Like VideoCancel, ImageTaskCancel must never remove a finished task's artifact.
type ImageTaskCanceller interface {
	ImageTaskCancel(ctx *BifrostContext, key Key, request *BifrostImageTaskCancelRequest) (*BifrostImageTaskResponse, *BifrostError)
}

// IsImageTaskRequestType reports whether t is one of the four image task request types.
func IsImageTaskRequestType(t RequestType) bool {
	switch t {
	case ImageGenerationTaskRequest, ImageEditTaskRequest, ImageTaskRetrieveRequest, ImageTaskCancelRequest:
		return true
	}
	return false
}

// NewImageTaskCancelUnsupportedError is the error of the image task cancel route when the task
// cannot be cancelled: 400, error_type provider_cancel_unsupported. message empty gives the
// no-cancel-API default.
func NewImageTaskCancelUnsupportedError(message string) *BifrostError {
	if message == "" {
		message = "this provider has no API to cancel an image task"
	}
	err := NewVideoCancelUnsupportedError(message)
	err.ExtraFields.RequestType = ImageTaskCancelRequest
	return err
}

// CheckImageTaskCancelSupported returns the provider_cancel_unsupported error when req is an
// image task cancel and provider has no cancel API, nil otherwise. Core runs it before a key is
// chosen, so the refusal makes no upstream call.
func CheckImageTaskCancelSupported(provider any, req *BifrostRequest) *BifrostError {
	if req == nil || req.RequestType != ImageTaskCancelRequest {
		return nil
	}
	if _, ok := provider.(ImageTaskCanceller); ok {
		return nil
	}
	return NewImageTaskCancelUnsupportedError("")
}
