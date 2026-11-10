package bfl

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

// BFL image tasks (TXE-6431, contract C4 to C7 and C9, BFL vendor sheet B2 to B10). BFL serves
// images only as tasks, so the provider takes the image task routes; the sync image routes stay
// unsupported.

// imageTaskFields are the unified fields an image generation task delivers. Everything else is
// refused by core before a key is chosen (contract C4); each field below is converted per model
// in toBFLImageBody, which refuses the ones the model has no counterpart for.
var imageTaskFields = []string{
	"n", "size", "aspect_ratio", "resolution", "seed", "output_format", "response_format",
	"num_inference_steps", "input_images", schemas.TaskControlFieldCallbackURL,
}

// imageEditTaskFields adds the edit's mask, which only the fill endpoint takes.
var imageEditTaskFields = append(append([]string(nil), imageTaskFields...), "mask")

// SupportedUnifiedFields declares the unified fields of the image task operations and of FLUX 3 Video. Of the four
// task control fields only callback_url is declared: BFL has no external task id, deadline or
// storage target, so the other three are refused before any upstream call (contract C7).
func (provider *BFLProvider) SupportedUnifiedFields(requestType schemas.RequestType) ([]string, bool) {
	switch requestType {
	case schemas.ImageGenerationTaskRequest:
		return imageTaskFields, true
	case schemas.ImageEditTaskRequest:
		return imageEditTaskFields, true
	case schemas.VideoGenerationRequest:
		// FLUX 3 Video (TXE-6617): no task control field, since the endpoint has no webhook_url,
		// external id or deadline.
		return videoFields, true
	}
	return nil, false
}

// bflImageBody is a converted BFL image request: the endpoint's fields, and the caller's extra
// fields, merged into the body unchanged (safety_tolerance, prompt_upsampling, webhook_secret).
type bflImageBody struct {
	fields map[string]any
	extra  map[string]any
}

// MarshalJSON encodes the endpoint's fields.
// Keys are sorted, so identical requests encode to identical bytes (AGENTS.md).
func (b *bflImageBody) MarshalJSON() ([]byte, error) { return providerUtils.MarshalSorted(b.fields) }

// GetExtraParams returns the caller's extra fields.
func (b *bflImageBody) GetExtraParams() map[string]any { return b.extra }

// imageTaskInput is what the two task creates share once the operation-specific inputs are read.
type imageTaskInput struct {
	model             string
	prompt            string
	n                 *int
	size              *string
	aspectRatio       *string
	resolution        string
	seed              *int
	outputFormat      *string
	responseFormat    *string
	numInferenceSteps *int
	images            []string // each a URL or base64, as BFL takes them
	mask              []byte
	callbackURL       *string
	extra             map[string]any
	keyWebhookSecret  string // the key's bfl_key_config.webhook_secret (TXE-7062); "" when it has none
}

// refuse is the caller_unsupported_parameter error for param, carried out of a converter.
func refuse(param, message string) error {
	return providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(param, message))
}

// toBFLImageBody converts a task create into the body of POST /v1/{model}. A unified field the
// model has no counterpart for is refused, never dropped or changed, except seed (contract C4).
func toBFLImageBody(in imageTaskInput) (*bflImageBody, error) {
	spec := specFor(in.model)
	fields := map[string]any{}
	if in.prompt != "" {
		fields["prompt"] = in.prompt
	}
	if in.n != nil && *in.n != 1 {
		return nil, refuse("n", "BFL returns one image per task; n must be 1")
	}
	if in.responseFormat != nil && *in.responseFormat != "url" {
		return nil, refuse("response_format", "BFL returns images as links; response_format must be url")
	}
	if in.size != nil && *in.size != "auto" {
		if !spec.size {
			return nil, refuse("size", fmt.Sprintf("model %s takes no width and height", in.model))
		}
		width, height, ok := parseSize(*in.size)
		if !ok {
			return nil, refuse("size", "size must be WIDTHxHEIGHT, such as 1024x768")
		}
		fields["width"], fields["height"] = width, height
	}
	if in.aspectRatio != nil {
		if !spec.aspectRatio {
			return nil, refuse("aspect_ratio", fmt.Sprintf("model %s takes no aspect_ratio", in.model))
		}
		fields["aspect_ratio"] = *in.aspectRatio
	}
	if in.resolution != "" {
		if !spec.resolution {
			return nil, refuse("resolution", fmt.Sprintf("model %s takes no resolution", in.model))
		}
		// BFL writes the tiers in lower case (1k, 2k, 4k); the unified tiers are 1K, 2K, 4K.
		fields["resolution"] = strings.ToLower(in.resolution)
	}
	if in.seed != nil && spec.seed {
		fields["seed"] = *in.seed
	}
	if in.outputFormat != nil {
		if !spec.outputFormat {
			return nil, refuse("output_format", fmt.Sprintf("model %s takes no output_format", in.model))
		}
		fields["output_format"] = *in.outputFormat
	}
	if in.numInferenceSteps != nil {
		if !spec.steps {
			return nil, refuse("num_inference_steps", fmt.Sprintf("model %s takes no steps", in.model))
		}
		fields["steps"] = *in.numInferenceSteps
	}
	if err := setInputImages(fields, spec, in); err != nil {
		return nil, err
	}
	if in.callbackURL != nil {
		if !spec.webhook {
			return nil, refuse(schemas.TaskControlFieldCallbackURL, fmt.Sprintf("model %s has no webhook_url", in.model))
		}
		fields["webhook_url"] = *in.callbackURL
		// The key's secret signs the webhook unless the request names its own (TXE-7062).
		if _, own := in.extra["webhook_secret"]; !own && in.keyWebhookSecret != "" {
			fields["webhook_secret"] = in.keyWebhookSecret
		}
	}
	return &bflImageBody{fields: fields, extra: in.extra}, nil
}

// setInputImages writes the input images and the mask in the form the model takes.
func setInputImages(fields map[string]any, spec modelSpec, in imageTaskInput) error {
	if len(in.mask) > 0 && spec.images != inputImageFill {
		return refuse("mask", fmt.Sprintf("model %s takes no mask; masks go to the fill model", in.model))
	}
	if len(in.images) == 0 {
		return nil
	}
	if spec.images == inputImagesNone {
		return refuse("input_images", fmt.Sprintf("model %s takes no input image", in.model))
	}
	if len(in.images) > spec.maxImages {
		return refuse("input_images", fmt.Sprintf("model %s takes at most %d input images", in.model, spec.maxImages))
	}
	switch spec.images {
	case inputImagesArray:
		fields["images"] = in.images
	case inputImagesNumbered:
		for i, image := range in.images {
			key := "input_image"
			if i > 0 {
				key = "input_image_" + strconv.Itoa(i+1)
			}
			fields[key] = image
		}
	case inputImageFill:
		fields["image"] = in.images[0]
		if len(in.mask) > 0 {
			fields["mask"] = base64.StdEncoding.EncodeToString(in.mask)
		}
	}
	return nil
}

// parseSize reads "WIDTHxHEIGHT".
func parseSize(size string) (width, height int, ok bool) {
	w, h, found := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !found {
		return 0, 0, false
	}
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// bflImageInput is an input image as BFL takes it: a URL unchanged, a data URL as its base64
// payload, anything else as given (base64). Bifrost downloads nothing (contract C4).
func bflImageInput(image string) string {
	if strings.HasPrefix(image, "data:") {
		if _, isBase64, payload, ok := schemas.ParseDataURL(image); ok && isBase64 {
			return payload
		}
	}
	return image
}

// extraWithout copies the caller's extra fields without the named ones. The handlers keep each
// unified field in the extras too (schemas.UnifiedImageFields, UnifiedVideoGenerationFields), so a
// provider that converts the typed field must not also send the raw copy: the merge would refuse
// the two differing values ("2K" against the converted "2k"). The caller's map is not modified.
func extraWithout(extra map[string]any, names []string) map[string]any {
	if len(extra) == 0 {
		return nil
	}
	out := make(map[string]any, len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for _, name := range names {
		delete(out, name)
	}
	return out
}

// generationTaskInput reads an image generation task create.
func generationTaskInput(request *schemas.BifrostImageGenerationRequest) imageTaskInput {
	in := imageTaskInput{model: request.Model}
	if request.Input != nil {
		in.prompt = request.Input.Prompt
	}
	if p := request.Params; p != nil {
		in.n, in.size, in.aspectRatio, in.resolution = p.N, p.Size, p.AspectRatio, string(p.Resolution)
		in.seed, in.outputFormat, in.responseFormat = p.Seed, p.OutputFormat, p.ResponseFormat
		in.numInferenceSteps, in.extra = p.NumInferenceSteps, extraWithout(p.ExtraParams, schemas.UnifiedImageFields)
		for _, image := range p.InputImages {
			in.images = append(in.images, bflImageInput(image))
		}
	}
	if request.TaskControl != nil {
		in.callbackURL = request.TaskControl.CallbackURL
	}
	return in
}

// editTaskInput reads an image edit task create: image bytes are sent as base64, URLs unchanged.
func editTaskInput(request *schemas.BifrostImageEditRequest) imageTaskInput {
	in := imageTaskInput{model: request.Model}
	if request.Input != nil {
		in.prompt = request.Input.Prompt
		for _, image := range request.Input.Images {
			if len(image.Image) > 0 {
				in.images = append(in.images, base64.StdEncoding.EncodeToString(image.Image))
			} else if image.URL != "" {
				in.images = append(in.images, bflImageInput(image.URL))
			}
		}
	}
	if p := request.Params; p != nil {
		in.n, in.size, in.aspectRatio, in.resolution = p.N, p.Size, p.AspectRatio, string(p.Resolution)
		in.seed, in.outputFormat, in.responseFormat = p.Seed, p.OutputFormat, p.ResponseFormat
		in.numInferenceSteps, in.mask, in.extra = p.NumInferenceSteps, p.Mask, extraWithout(p.ExtraParams, schemas.UnifiedImageFields)
	}
	if request.TaskControl != nil {
		in.callbackURL = request.TaskControl.CallbackURL
	}
	return in
}

// redactedSecret replaces a configured credential in a captured body.
const redactedSecret = "[REDACTED]"

// redactSecret returns body with every JSON string equal to secret replaced by redactedSecret.
// The secret is matched in its JSON-encoded form, so escaping cannot hide it. body itself is not
// modified; it is returned as is when secret is empty or absent.
func redactSecret(body []byte, secret string) []byte {
	if secret == "" || len(body) == 0 {
		return body
	}
	quoted, err := sonic.Marshal(secret)
	if err != nil || !bytes.Contains(body, quoted) {
		return body
	}
	return bytes.ReplaceAll(body, quoted, []byte(`"`+redactedSecret+`"`))
}

// keyWebhookSecret is the key's BFL webhook secret, or "" when the key configures none.
func keyWebhookSecret(key schemas.Key) string {
	if key.BFLKeyConfig == nil || key.BFLKeyConfig.WebhookSecret == nil {
		return ""
	}
	return key.BFLKeyConfig.WebhookSecret.GetValue()
}

// ImageGenerationTask submits an image generation task: one POST /v1/{model}.
func (provider *BFLProvider) ImageGenerationTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	return provider.submitImageTask(ctx, key, request, request.Model, func() (providerUtils.RequestBodyWithExtraParams, error) {
		in := generationTaskInput(request)
		in.keyWebhookSecret = keyWebhookSecret(key)
		return toBFLImageBody(in)
	})
}

// ImageEditTask submits an image edit task: one POST /v1/{model}.
func (provider *BFLProvider) ImageEditTask(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageEditRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	return provider.submitImageTask(ctx, key, request, request.Model, func() (providerUtils.RequestBodyWithExtraParams, error) {
		in := editTaskInput(request)
		in.keyWebhookSecret = keyWebhookSecret(key)
		return toBFLImageBody(in)
	})
}

// submitImageTask converts, sends the one submit, and returns the queued task object.
func (provider *BFLProvider) submitImageTask(ctx *schemas.BifrostContext, key schemas.Key, request providerUtils.RequestBodyGetter, model string, convert providerUtils.RequestBodyConverter) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	if !modelPathPattern.MatchString(model) {
		return nil, providerUtils.NewBifrostBadRequestError(fmt.Sprintf("invalid BFL model name %q", model))
	}
	// The caller's extra fields (safety_tolerance, prompt_upsampling, webhook_secret) are part of
	// BFL's request body, so they are always merged.
	ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
	jsonData, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, convert)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// BFL receives the key's webhook secret; every copy Bifrost keeps of the body (raw_request on
	// the response or on an error, and the logs built from it) has it redacted (TXE-7062).
	captured := jsonData
	if sendBackRawRequest {
		captured = redactSecret(jsonData, keyWebhookSecret(key))
	}
	submittedAt := time.Now().Unix()
	body, latency, bifrostErr := provider.doRequestCaptured(ctx, key, http.MethodPost, provider.requestURL(ctx, "/v1/"+model), jsonData, captured, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var submit BFLAsyncResponse
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &submit, captured, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	if submit.ID == "" {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewBifrostOperationError("BFL returned no task id", nil), captured, body, sendBackRawRequest, sendBackRawResponse, latency)
	}

	task := &schemas.BifrostImageTaskResponse{
		ID:        providerUtils.AddVideoIDProviderSuffix(encodeTaskID(submit.ID, model, submit.PollingURL, provider.networkConfig.BaseURL), provider.GetProviderKey()),
		Object:    schemas.ImageTaskObject,
		Model:     model,
		Status:    schemas.VideoStatusQueued,
		CreatedAt: submittedAt,
		Data:      []schemas.TaskArtifact{},
		Usage:     submitUsage(&submit),
	}
	// A submit with webhook_url answers with a status string; it is the vendor's word, so it is
	// mapped like a query's.
	if submit.Status != "" {
		applyStatus(task, submit.Status)
	}
	task.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawRequest {
		task.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		task.ExtraFields.RawResponse = rawResponse
	}
	return task, nil
}

// ImageTaskRetrieve queries a task once: GET its polling URL (contract C7, B3, B6 to B8).
func (provider *BFLProvider) ImageTaskRetrieve(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostImageTaskRetrieveRequest) (*schemas.BifrostImageTaskResponse, *schemas.BifrostError) {
	vendorID := providerUtils.StripVideoIDProviderSuffix(request.ID, provider.GetProviderKey())
	ref, ok := decodeTaskID(vendorID)
	if !ok {
		return nil, providerUtils.NewBifrostBadRequestError("invalid BFL task id")
	}
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	body, latency, bifrostErr := provider.doRequest(ctx, key, http.MethodGet, ref.pollURL(provider.networkConfig.BaseURL), nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	var result BFLResultResponse
	_, rawResponse, bifrostErr := providerUtils.HandleProviderResponseCtx(ctx, body, &result, nil, false, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	task := toImageTask(&result, ref)
	task.ID = providerUtils.AddVideoIDProviderSuffix(vendorID, provider.GetProviderKey())
	task.ExtraFields.Latency = latency.Milliseconds()
	if sendBackRawResponse {
		task.ExtraFields.RawResponse = rawResponse
	}
	return task, nil
}

// statusTable maps the nine values of BFL's StatusResponse one to one (contract C7, B7). BFL has
// no cancelled or expired state; "Task not found" is a failure, since the task cannot be read.
var statusTable = providerUtils.VideoStatusTable{
	"Pending":           schemas.VideoStatusQueued,
	"Reasoning":         schemas.VideoStatusInProgress,
	"Generating":        schemas.VideoStatusInProgress,
	"Ready":             schemas.VideoStatusCompleted,
	"Request Moderated": schemas.VideoStatusFailed,
	"Content Moderated": schemas.VideoStatusFailed,
	"No Card Generated": schemas.VideoStatusFailed,
	"Error":             schemas.VideoStatusFailed,
	"Task not found":    schemas.VideoStatusFailed,
}

// moderatedStatuses are the two statuses in which BFL's moderation blocked the task.
var moderatedStatuses = map[string]bool{"Request Moderated": true, "Content Moderated": true}

// applyStatus sets status and provider_status from the vendor's string. A string the table does
// not know is in_progress: Bifrost does not know it to be terminal.
func applyStatus(task *schemas.BifrostImageTaskResponse, raw string) {
	task.ProviderStatus = raw
	status, ok := statusTable.Lookup(raw)
	if !ok {
		status = schemas.VideoStatusInProgress
	}
	task.Status = status
}

// toImageTask maps a get_result response to the task object.
func toImageTask(result *BFLResultResponse, ref bflTask) *schemas.BifrostImageTaskResponse {
	task := &schemas.BifrostImageTaskResponse{
		Object: schemas.ImageTaskObject,
		Model:  ref.model,
		Data:   []schemas.TaskArtifact{},
	}
	applyStatus(task, result.Status)
	if result.Cost != nil {
		task.Usage = &schemas.ImageUsage{VendorCost: creditCost(*result.Cost)}
	}
	sample := readResult(result.Result)
	if sample.endTime != nil {
		task.CompletedAt = schemas.Ptr(int64(*sample.endTime))
	}

	switch {
	case task.Status == schemas.VideoStatusCompleted:
		for _, link := range sample.links {
			task.Data = append(task.Data, schemas.TaskArtifact{
				Type:        schemas.VideoOutputTypeURL,
				URL:         schemas.Ptr(link),
				ContentType: imageContentType(link),
			})
		}
		// expires_at needs the vendor's completion time and the model's documented lifetime; a
		// bare id (no model) or a result without end_time leaves it empty rather than guessed.
		if ref.model != "" {
			task.ExpiresAt = providerUtils.VideoExpiresAt(nil, task.CompletedAt, specFor(ref.model).linkLifetime)
		}
	case task.Status == schemas.VideoStatusFailed:
		task.Error = &schemas.TaskError{Code: result.Status, Message: failureMessage(result)}
		if moderatedStatuses[result.Status] {
			// One task is one image (n is 1), so a blocked task filtered one image (contract C6).
			task.ContentFilter = &schemas.ContentFilterInfo{FilteredCount: 1, Reasons: moderationReasons(result.Details)}
		}
	}
	return task
}

// submitUsage reads the cost and megapixels a submit reports (contract C5, B6).
func submitUsage(submit *BFLAsyncResponse) *schemas.ImageUsage {
	if submit.Cost == nil && submit.InputMP == nil && submit.OutputMP == nil {
		return nil
	}
	usage := &schemas.ImageUsage{InputMegapixels: submit.InputMP, OutputMegapixels: submit.OutputMP}
	if submit.Cost != nil {
		usage.VendorCost = creditCost(*submit.Cost)
	}
	return usage
}

// creditCost is BFL's cost, which BFL reports in credits.
func creditCost(amount float64) *schemas.VendorCost {
	return &schemas.VendorCost{Amount: amount, Unit: schemas.VendorCostUnitCredit}
}

// bflSample is what Bifrost reads from get_result's result, which BFL's OpenAPI leaves
// unconstrained: the sample link (one string, or a list) and the end time.
type bflSample struct {
	links   []string
	endTime *float64
}

// readResult reads result.sample and result.end_time. Any other shape reads as empty.
func readResult(raw []byte) bflSample {
	var out bflSample
	if len(raw) == 0 {
		return out
	}
	var fields struct {
		Sample  any      `json:"sample"`
		EndTime *float64 `json:"end_time"`
	}
	if err := sonic.Unmarshal(raw, &fields); err != nil {
		return out
	}
	out.endTime = fields.EndTime
	switch sample := fields.Sample.(type) {
	case string:
		if sample != "" {
			out.links = []string{sample}
		}
	case []any:
		for _, item := range sample {
			if link, ok := item.(string); ok && link != "" {
				out.links = append(out.links, link)
			}
		}
	}
	return out
}

// moderationReasons reads details["Moderation Reasons"], a list of strings or one string,
// verbatim (contract C6).
func moderationReasons(details map[string]any) []string {
	switch reasons := details["Moderation Reasons"].(type) {
	case []any:
		out := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			if text, ok := reason.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case string:
		return []string{reasons}
	}
	return nil
}

// failureMessage is the message of a failed task: the vendor's own text when it gives one.
func failureMessage(result *BFLResultResponse) string {
	for _, key := range []string{"message", "error", "detail"} {
		if text, ok := result.Details[key].(string); ok && text != "" {
			return text
		}
	}
	var text string
	if err := sonic.Unmarshal(result.Result, &text); err == nil && text != "" {
		return text
	}
	return "BFL reported " + result.Status
}

// imageContentType is the content type a result link's file extension names, or "" when it
// names none Bifrost knows.
func imageContentType(link string) string {
	if i := strings.IndexAny(link, "?#"); i >= 0 {
		link = link[:i]
	}
	switch strings.ToLower(path.Ext(link)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	}
	return ""
}
