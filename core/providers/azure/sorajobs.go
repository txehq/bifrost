package azure

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/providers/openai"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// First-generation sora on Azure (fork, TXE-6533, contract C7 and C9). Sora 2 uses the
// OpenAI-shaped /openai/v1/videos API. The first-generation model has its own API: video
// generation jobs, /openai/v1/video/generations/jobs, whose job holds one generation per
// requested variant, and whose artifact is fetched per generation with the resource's key.
//
// What the Azure reference (Foundry Models v1 preview, "Video generation jobs") documents and
// this file relies on: create takes prompt, model (the deployment), width, height, n_seconds
// (1–20, default 5) and n_variants (1–5, default 1); the job carries id, status, created_at,
// finished_at, expires_at ("the time when the job gets automatically deleted from the
// service"), failure_reason and generations[] ({id, job_id, created_at, width, height,
// n_seconds, prompt}); a generation's video is GET /video/generations/{id}/content/video and
// its thumbnail .../content/thumbnail; a job is deleted with DELETE /video/generations/jobs/{id}.
//
// What it does not document, and so this file does not assume: a status enum (the quickstart
// names queued, preprocessing, running, processing, succeeded, failed and cancelled); any
// failure_reason value; a cancel endpoint (there is none, so the provider has no VideoCancel
// and core answers provider_cancel_unsupported); any usage or cost field.

// azureSoraJobsModel is the canonical name of the first-generation model. Sora 2 is "sora-2".
const azureSoraJobsModel = "sora"

// azureSoraJobIDPrefix marks a task id as a video generation job. Query, download and delete
// carry only the task id, not the model, so the id itself says which Azure video API owns it:
// "sora1.<job id>:azure". Sora 2 ids ("video_...") never carry it.
const azureSoraJobIDPrefix = "sora1."

// azureSoraJobsAPIVersion is the api-version every documented call of the jobs API uses.
const azureSoraJobsAPIVersion = AzureAPIVersionPreview

// azureSoraJobsContentType is the media type of a generation's video (the reference's 200
// response of .../content/video).
const azureSoraJobsContentType = "video/mp4"

// azureSoraJobsStatuses maps the job statuses the Azure quickstart names to the task object's
// status. A status not listed maps to in_progress with the vendor's word in provider_status.
var azureSoraJobsStatuses = providerUtils.VideoStatusTable{
	"queued":        schemas.VideoStatusQueued,
	"preprocessing": schemas.VideoStatusInProgress,
	"running":       schemas.VideoStatusInProgress,
	"processing":    schemas.VideoStatusInProgress,
	"succeeded":     schemas.VideoStatusCompleted,
	"failed":        schemas.VideoStatusFailed,
	"cancelled":     schemas.VideoStatusCancelled,
}

// azureSoraJobsFields are the unified video create fields the jobs API takes: seconds becomes
// n_seconds, size becomes width and height. audio and output_format are checked by value in
// toAzureSoraJobRequest. Every other priced field is refused (contract C4, convention three).
var azureSoraJobsFields = []string{"seconds", "size", "audio", "output_format"}

// azureSoraJobRequest is the JSON body of POST /openai/v1/video/generations/jobs.
type azureSoraJobRequest struct {
	Prompt      string         `json:"prompt"`
	Model       string         `json:"model"`
	Width       *int           `json:"width,omitempty"`
	Height      *int           `json:"height,omitempty"`
	NSeconds    *int           `json:"n_seconds,omitempty"`
	ExtraParams map[string]any `json:"-"`
}

// GetExtraParams implements providerUtils.RequestBodyWithExtraParams.
func (r *azureSoraJobRequest) GetExtraParams() map[string]any { return r.ExtraParams }

// azureSoraJob is a video generation job as the jobs API returns it.
type azureSoraJob struct {
	Object        string                   `json:"object"`
	ID            string                   `json:"id"`
	Status        schemas.TolerantString   `json:"status"`
	CreatedAt     int64                    `json:"created_at"`
	FinishedAt    *int64                   `json:"finished_at"`
	ExpiresAt     *int64                   `json:"expires_at"`
	Generations   []azureSoraJobGeneration `json:"generations"`
	Prompt        string                   `json:"prompt"`
	Model         string                   `json:"model"`
	NVariants     *int                     `json:"n_variants"`
	NSeconds      *int                     `json:"n_seconds"`
	Height        *int                     `json:"height"`
	Width         *int                     `json:"width"`
	FailureReason *string                  `json:"failure_reason"`
}

// azureSoraJobGeneration is one generated video of a job.
type azureSoraJobGeneration struct {
	Object    string `json:"object"`
	ID        string `json:"id"`
	JobID     string `json:"job_id"`
	CreatedAt int64  `json:"created_at"`
	Width     *int   `json:"width"`
	Height    *int   `json:"height"`
	NSeconds  *int   `json:"n_seconds"`
	Prompt    string `json:"prompt"`
}

// isAzureSoraJobsModel reports whether a create for model goes to the jobs API: the canonical
// model (the alias's model name when the key maps one) is first-generation sora.
func isAzureSoraJobsModel(ctx *schemas.BifrostContext, model string) bool {
	return strings.EqualFold(bareAzureModel(schemas.ResolveCanonicalModel(ctx, model)), azureSoraJobsModel)
}

// bareAzureModel drops an "azure/" provider prefix.
func bareAzureModel(model string) string {
	if len(model) > len(schemas.Azure)+1 && model[len(schemas.Azure)] == '/' && strings.EqualFold(model[:len(schemas.Azure)], string(schemas.Azure)) {
		return model[len(schemas.Azure)+1:]
	}
	return model
}

// azureSoraJobID returns the job id a task id names, and whether the task is a jobs-API job.
func azureSoraJobID(taskID string, provider schemas.ModelProvider) (string, bool) {
	// The marker is never escaped ("." is unreserved), so an id without it is not a job and
	// costs one prefix comparison.
	if !strings.HasPrefix(taskID, azureSoraJobIDPrefix) {
		return "", false
	}
	stripped := providerUtils.StripVideoIDProviderSuffix(taskID, provider)
	jobID, ok := strings.CutPrefix(stripped, azureSoraJobIDPrefix)
	return jobID, ok && jobID != ""
}

// toAzureSoraJobRequest converts a unified video create to the jobs API body. A field the jobs
// API cannot take, or a value it cannot be sent as, is refused with caller_unsupported_parameter
// before any upstream call; nothing is dropped, changed or clamped.
func toAzureSoraJobRequest(request *schemas.BifrostVideoGenerationRequest) (*azureSoraJobRequest, *schemas.BifrostError) {
	if request.Input == nil || strings.TrimSpace(request.Input.Prompt) == "" {
		return nil, newAzureSoraJobsInvalidRequest("prompt", "prompt is required")
	}
	if request.Input.InputReference != nil {
		return nil, schemas.NewUnsupportedParameterError("input_reference",
			"input_reference is not supported for first-generation sora on Azure: Bifrost does not send image or video prompts to the video generation jobs API")
	}
	if request.Input.VideoURI != nil {
		return nil, schemas.NewUnsupportedParameterError("video_uri", "")
	}
	if bifrostErr := schemas.CheckUnifiedFields(&schemas.BifrostRequest{
		RequestType:            schemas.VideoGenerationRequest,
		VideoGenerationRequest: request,
	}, azureSoraJobsFields); bifrostErr != nil {
		return nil, bifrostErr
	}

	body := &azureSoraJobRequest{Prompt: request.Input.Prompt, Model: request.Model}
	params := request.Params
	if params == nil {
		return body, nil
	}
	if params.Audio != nil && *params.Audio {
		return nil, schemas.NewUnsupportedParameterError("audio", "first-generation sora generates video without audio")
	}
	if params.OutputFormat != nil && !strings.EqualFold(*params.OutputFormat, "mp4") {
		return nil, schemas.NewUnsupportedParameterError("output_format", "first-generation sora generates mp4 only")
	}
	if params.Seconds != nil {
		seconds, err := strconv.Atoi(strings.TrimSpace(*params.Seconds))
		if err != nil {
			return nil, schemas.NewUnsupportedParameterError("seconds",
				fmt.Sprintf("seconds %q is not a whole number of seconds, which n_seconds requires", *params.Seconds))
		}
		body.NSeconds = &seconds
	}
	if params.Size != "" {
		width, height, ok := parseAzureSoraJobSize(params.Size)
		if !ok {
			return nil, schemas.NewUnsupportedParameterError("size",
				fmt.Sprintf("size %q is not WIDTHxHEIGHT in pixels, which width and height require", params.Size))
		}
		body.Width, body.Height = &width, &height
	}
	// The variant count has no unified field. One sent as an extra param is refused unless it
	// asks for one video: the task object, and the download route, carry one artifact per task.
	for _, name := range []string{"n", "n_variants"} {
		if v, ok := params.ExtraParams[name]; ok && !isOne(v) {
			return nil, schemas.NewUnsupportedParameterError(name,
				"first-generation sora on Azure delivers one video per task; more than one variant cannot be requested")
		}
	}
	body.ExtraParams = params.ExtraParams
	return body, nil
}

// parseAzureSoraJobSize splits "WIDTHxHEIGHT" into positive pixel counts.
func parseAzureSoraJobSize(size string) (width, height int, ok bool) {
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

// isOne reports whether an extra-param value is the number 1, in any JSON-decoded form.
func isOne(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 1
	case int64:
		return n == 1
	case float64:
		return n == 1
	case string:
		return strings.TrimSpace(n) == "1"
	default:
		if s, ok := v.(fmt.Stringer); ok {
			return s.String() == "1"
		}
		return false
	}
}

// newAzureSoraJobsInvalidRequest is a 400 the caller caused, made before any upstream call.
func newAzureSoraJobsInvalidRequest(param, message string) *schemas.BifrostError {
	return &schemas.BifrostError{
		StatusCode: schemas.Ptr(http.StatusBadRequest),
		Error: &schemas.ErrorField{
			Type:    schemas.Ptr("invalid_request_error"),
			Message: message,
			Param:   param,
		},
		ExtraFields: schemas.BifrostErrorExtraFields{ErrorType: schemas.ErrorTypeCallerInvalidRequest},
	}
}

// toBifrostVideoResponse converts a job to the task object (contract C7).
func (job *azureSoraJob) toBifrostVideoResponse(provider schemas.ModelProvider) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:        providerUtils.AddVideoIDProviderSuffix(azureSoraJobIDPrefix+job.ID, provider),
		Object:    "video",
		Model:     job.Model,
		CreatedAt: job.CreatedAt,
		Prompt:    job.Prompt,
	}
	if job.NSeconds != nil {
		resp.Seconds = schemas.Ptr(strconv.Itoa(*job.NSeconds))
	}
	if job.Width != nil && job.Height != nil {
		resp.Size = fmt.Sprintf("%dx%d", *job.Width, *job.Height)
	}
	azureSoraJobsStatuses.Apply(resp, string(job.Status))
	if job.FinishedAt != nil {
		resp.CompletedAt = schemas.Ptr(*job.FinishedAt)
	}

	switch resp.Status {
	case schemas.VideoStatusCompleted:
		if len(job.Generations) == 0 {
			return nil, schemas.NewProviderEmptyResultError("the video generation job succeeded with no generations")
		}
		// The artifact needs the resource's key, so it is a content entry the caller fetches
		// through GET /v1/videos/{id}/content (contract C9).
		var outputSeconds float64
		reported := true
		for _, generation := range job.Generations {
			resp.Videos = append(resp.Videos, schemas.VideoOutput{
				ID:          generation.ID,
				Type:        schemas.VideoOutputTypeContent,
				ContentType: azureSoraJobsContentType,
			})
			if generation.NSeconds == nil {
				reported = false
				continue
			}
			outputSeconds += float64(*generation.NSeconds)
		}
		// Usage is the duration the vendor reports on each generation, never the request's.
		if reported {
			resp.Usage = &schemas.VideoUsage{OutputSeconds: schemas.Ptr(outputSeconds)}
		}
	case schemas.VideoStatusFailed:
		resp.Error = &schemas.VideoCreateError{Message: "the video generation job failed"}
		if job.FailureReason != nil && *job.FailureReason != "" {
			resp.Error.Code = *job.FailureReason
			resp.Error.Message = *job.FailureReason
		}
	}
	// expires_at is the vendor's own: the time the job, and its content, is deleted.
	providerUtils.SetVideoExpiresAt(resp, job.ExpiresAt, 0)
	return resp, nil
}

// soraJobsCall makes one call to the jobs API and returns the response body on a 2xx. A vendor
// error keeps the vendor's status; a ResponsibleAIPolicyViolation is caller_content_filtered.
func (provider *AzureProvider) soraJobsCall(ctx *schemas.BifrostContext, key schemas.Key, method, path string, body []byte) ([]byte, *fasthttp.Response, int64, *schemas.BifrostError) {
	endpoint := resolveAzureEndpoint(ctx, key)
	if endpoint == "" {
		return nil, nil, 0, providerUtils.NewConfigurationError("endpoint not set")
	}
	authHeaders, bifrostErr := provider.getAzureAuthHeaders(ctx, key, false)
	if bifrostErr != nil {
		return nil, nil, 0, bifrostErr
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(fmt.Sprintf("%s/openai/v1%s?api-version=%s", strings.TrimRight(endpoint, "/"), path, azureSoraJobsAPIVersion))
	req.Header.SetMethod(method)
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.SetContentType("application/json")
		req.SetBody(body)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	wait()
	if bifrostErr != nil {
		fasthttp.ReleaseResponse(resp)
		return nil, nil, 0, bifrostErr
	}
	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	if resp.StatusCode() < 200 || resp.StatusCode() > 299 {
		bifrostErr := parseAzureSoraJobsError(resp)
		fasthttp.ReleaseResponse(resp)
		return nil, nil, 0, providerUtils.SetErrorLatency(bifrostErr, latency)
	}
	decoded, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		return nil, nil, 0, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}
	return append([]byte(nil), decoded...), resp, latency.Milliseconds(), nil
}

// parseAzureSoraJobsError parses a jobs API error. The reference's error body carries code,
// message, param and inner_error{code}. Under "error" it is the shape openai.ParseOpenAIError
// reads, including ResponsibleAIPolicyViolation as caller_content_filtered (TXE-6532, contract
// C6). At the top level, without the "error" envelope, ParseOpenAIError reads nothing, so this
// fills the same fields and the same content-filter class from there.
func parseAzureSoraJobsError(resp *fasthttp.Response) *schemas.BifrostError {
	bifrostErr := openai.ParseOpenAIError(resp)
	var top struct {
		Code       *string `json:"code"`
		Message    string  `json:"message"`
		Param      string  `json:"param"`
		InnerError *struct {
			Code string `json:"code"`
		} `json:"inner_error"`
		Error json.RawMessage `json:"error"`
	}
	raw, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil || sonic.Unmarshal(raw, &top) != nil || len(top.Error) > 0 {
		return bifrostErr
	}
	if top.Code != nil {
		bifrostErr.Error.Code = top.Code
	}
	if top.Message != "" {
		bifrostErr.Error.Message = top.Message
	}
	if top.Param != "" {
		bifrostErr.Error.Param = top.Param
	}
	if top.InnerError != nil && top.InnerError.Code != "" {
		bifrostErr.Error.InnerCode = schemas.Ptr(top.InnerError.Code)
		if top.InnerError.Code == "ResponsibleAIPolicyViolation" {
			bifrostErr.ExtraFields.ErrorType = schemas.ErrorTypeCallerContentFiltered
		}
	}
	return bifrostErr
}

// soraJobsCreate creates a video generation job: one POST, the task object back.
func (provider *AzureProvider) soraJobsCreate(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	jobRequest, bifrostErr := toAzureSoraJobRequest(request)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	jsonBody, bifrostErr := providerUtils.CheckContextAndGetRequestBody(ctx, request, func() (providerUtils.RequestBodyWithExtraParams, error) {
		return jobRequest, nil
	})
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if jsonBody == nil {
		return nil, providerUtils.NewBifrostOperationError("first-generation sora on Azure needs a JSON request body", nil)
	}
	return provider.soraJobsTask(ctx, key, http.MethodPost, "/video/generations/jobs", jsonBody)
}

// soraJobsRetrieve queries a video generation job once.
func (provider *AzureProvider) soraJobsRetrieve(ctx *schemas.BifrostContext, key schemas.Key, jobID string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	escaped, bifrostErr := providerUtils.EscapeResourceID(jobID, "video_id")
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	return provider.soraJobsTask(ctx, key, http.MethodGet, "/video/generations/jobs/"+escaped, nil)
}

// soraJobsTask makes a call that answers with a job and converts the job to the task object.
func (provider *AzureProvider) soraJobsTask(ctx *schemas.BifrostContext, key schemas.Key, method, path string, body []byte) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	responseBody, resp, latency, bifrostErr := provider.soraJobsCall(ctx, key, method, path, body)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	headers := providerUtils.ExtractProviderResponseHeaders(resp)
	fasthttp.ReleaseResponse(resp)

	var job azureSoraJob
	rawRequest, rawResponse, bifrostErr := providerUtils.HandleProviderResponse(responseBody, &job, body, sendBackRawRequest, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if job.ID == "" {
		return nil, schemas.NewProviderEmptyResultError("the video generation jobs API returned a job with no id")
	}
	task, bifrostErr := job.toBifrostVideoResponse(provider.GetProviderKey())
	if bifrostErr != nil {
		if sendBackRawResponse {
			bifrostErr.ExtraFields.RawResponse = rawResponse
		}
		return nil, bifrostErr
	}
	task.ExtraFields = schemas.BifrostResponseExtraFields{
		Latency:                 latency,
		ProviderResponseHeaders: headers,
	}
	if sendBackRawRequest {
		task.ExtraFields.RawRequest = rawRequest
	}
	if sendBackRawResponse {
		task.ExtraFields.RawResponse = rawResponse
	}
	return task, nil
}

// soraJobsDownload streams the first generation's video (or thumbnail) of a succeeded job. A job
// id does not name the generation, so the job is queried first; querying is not billed.
func (provider *AzureProvider) soraJobsDownload(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDownloadRequest, jobID string) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	content, contentType := "video", azureSoraJobsContentType
	if request.Variant != nil {
		switch *request.Variant {
		case schemas.VideoDownloadVariantVideo:
		case schemas.VideoDownloadVariantThumbnail:
			content, contentType = "thumbnail", "image/jpeg"
		default:
			return nil, schemas.NewUnsupportedParameterError("variant",
				fmt.Sprintf("variant %q is not available for first-generation sora on Azure: video and thumbnail are", *request.Variant))
		}
	}

	task, bifrostErr := provider.soraJobsRetrieve(ctx, key, jobID)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if task.Status != schemas.VideoStatusCompleted || len(task.Videos) == 0 {
		return nil, newAzureSoraJobsInvalidRequest("video_id",
			fmt.Sprintf("the video is not ready: the job is %s (%s)", task.Status, task.ProviderStatus))
	}
	// The job lookup above is a buffered call; the file itself is streamed to a caller that
	// asked for it, never held whole (contract C9 / BF20).
	response, bifrostErr := provider.soraJobsContent(ctx, key, task.Videos[0].ID, content, contentType, request.Stream)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	response.VideoID = request.ID
	return response, nil
}

// soraJobsContent fetches one generation's video or thumbnail through
// providerUtils.FetchVideoContent: streamed when stream is set (the download route), buffered
// otherwise (the GenAI byte enrichment). contentType is the media type to report when Azure
// sends none.
func (provider *AzureProvider) soraJobsContent(ctx *schemas.BifrostContext, key schemas.Key, generationID, content, contentType string, stream bool) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	escaped, bifrostErr := providerUtils.EscapeResourceID(generationID, "generation_id")
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	endpoint := resolveAzureEndpoint(ctx, key)
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}
	authHeaders, bifrostErr := provider.getAzureAuthHeaders(ctx, key, false)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(fmt.Sprintf("%s/openai/v1/video/generations/%s/content/%s?api-version=%s",
		strings.TrimRight(endpoint, "/"), escaped, content, azureSoraJobsAPIVersion))
	req.Header.SetMethod(http.MethodGet)
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}
	return providerUtils.FetchVideoContent(ctx, providerUtils.VideoContentFetch{
		Request:                  req,
		Client:                   provider.client,
		StreamingClient:          provider.streamingClient,
		Stream:                   stream,
		VideoID:                  generationID,
		StreamIdleTimeoutSeconds: provider.networkConfig.StreamIdleTimeoutInSeconds,
		ParseError:               parseAzureSoraJobsError,
		ContentType: func(upstream string) string {
			if upstream == "" {
				return contentType
			}
			return upstream
		},
	})
}

// soraJobsRetrieveForCaller is the query of the unified retrieve route. A caller that needs the
// bytes in the task object (the GenAI SDK operation poll sets BifrostContextKeyVideoOutputRequested)
// gets each completed generation's video as a base64 entry next to its content entry, as the
// OpenAI-shaped retrieve does for Sora 2.
func (provider *AzureProvider) soraJobsRetrieveForCaller(ctx *schemas.BifrostContext, key schemas.Key, jobID string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	task, bifrostErr := provider.soraJobsRetrieve(ctx, key, jobID)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	if wanted, _ := ctx.Value(schemas.BifrostContextKeyVideoOutputRequested).(bool); !wanted || task.Status != schemas.VideoStatusCompleted {
		return task, nil
	}
	videos := task.Videos
	for _, video := range videos {
		if video.Type != schemas.VideoOutputTypeContent || video.ID == "" {
			continue
		}
		file, bifrostErr := provider.soraJobsContent(ctx, key, video.ID, "video", azureSoraJobsContentType, false)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		encoded := base64.StdEncoding.EncodeToString(file.Content)
		task.Videos = append(task.Videos, schemas.VideoOutput{
			ID:          video.ID,
			Type:        schemas.VideoOutputTypeBase64,
			Base64Data:  &encoded,
			ContentType: file.ContentType,
		})
	}
	return task, nil
}

// soraJobsDelete deletes a video generation job (documented: DELETE answers 204).
func (provider *AzureProvider) soraJobsDelete(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostVideoDeleteRequest, jobID string) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	escaped, bifrostErr := providerUtils.EscapeResourceID(jobID, "video_id")
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	_, resp, latency, bifrostErr := provider.soraJobsCall(ctx, key, http.MethodDelete, "/video/generations/jobs/"+escaped, nil)
	if bifrostErr != nil {
		return nil, bifrostErr
	}
	fasthttp.ReleaseResponse(resp)
	// The handler passes the id without its provider suffix; the response carries the task id.
	return &schemas.BifrostVideoDeleteResponse{
		ID:          providerUtils.AddVideoIDProviderSuffix(azureSoraJobIDPrefix+jobID, provider.GetProviderKey()),
		Deleted:     true,
		Object:      "video.deleted",
		ExtraFields: schemas.BifrostResponseExtraFields{Latency: latency},
	}, nil
}
