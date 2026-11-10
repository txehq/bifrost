package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	"github.com/capsohq/bifrost/core/schemas"
)

const defaultVideoContentType = "video/mp4"

// veoSize is what one unified size means to Veo: an aspect ratio and a resolution tier.
type veoSize struct{ aspectRatio, resolution string }

// veoSizes are the sizes Veo renders exactly. A size outside the table is refused with
// caller_unsupported_parameter: it is never replaced by another ratio (fork, TXE-6537,
// contract C4; before, an unknown size became 16:9 and 1792x1024 was sent as 16:9).
var veoSizes = map[string]veoSize{
	"1280x720":  {"16:9", "720p"},
	"720x1280":  {"9:16", "720p"},
	"1920x1080": {"16:9", "1080p"},
	"1080x1920": {"9:16", "1080p"},
	"3840x2160": {"16:9", "4k"},
	"2160x3840": {"9:16", "4k"},
}

// veoDefaultResolution is the tier Veo renders when parameters.resolution is absent. A size at
// this tier sends no resolution, so Veo 2, which has no such parameter, keeps taking its sizes.
const veoDefaultResolution = "720p"

// veoReferenceTypeAsset is the referenceType sent for a unified reference_images entry, the
// one type Veo 3.1 takes. A caller that needs another type sends the vendor's own
// referenceImages objects as an extra param.
const veoReferenceTypeAsset = "asset"

// unsupportedVideoParam is the converter's refusal of a field it cannot deliver as given: 400
// caller_unsupported_parameter with the field in error.param (contract C4).
func unsupportedVideoParam(param, format string, args ...any) error {
	return providerUtils.ConverterBifrostError(schemas.NewUnsupportedParameterError(param, fmt.Sprintf(format, args...)))
}

// veoExtras is a video request's extra params minus every one the converter sent in its own
// place. What is left is merged into the top level of the vendor body when the caller asked for
// extra-param passthrough; a consumed key left in would be sent twice, the second time at the
// top level, where predictLongRunning has no such field.
type veoExtras map[string]any

func (e veoExtras) stringPtr(key string) *string {
	if v, ok := schemas.SafeExtractStringPointer(e[key]); ok {
		delete(e, key)
		return v
	}
	return nil
}

func (e veoExtras) intPtr(key string) *int {
	if v, ok := schemas.SafeExtractIntPointer(e[key]); ok {
		delete(e, key)
		return v
	}
	return nil
}

func (e veoExtras) boolPtr(key string) *bool {
	if v, ok := schemas.SafeExtractBoolPointer(e[key]); ok {
		delete(e, key)
		return v
	}
	return nil
}

// value takes a parameter of any JSON type, as the caller gave it.
func (e veoExtras) value(key string) any {
	v, ok := e[key]
	if !ok || v == nil {
		return nil
	}
	delete(e, key)
	return v
}

// unifiedString reads one string parameter that can arrive as a typed unified field and as an
// extra param under any of keys (the unified name, which the handlers also keep in the extra
// params, and the vendor's own name). A value that is not a string, or two values that
// differ, is refused naming param: sending one of them would drop the other.
func (e veoExtras) unifiedString(param, typed string, keys ...string) (*string, error) {
	value := typed
	for _, key := range keys {
		raw, present := e[key]
		if !present || raw == nil {
			continue
		}
		given, ok := raw.(string)
		if !ok {
			return nil, unsupportedVideoParam(param, "%s must be a string", key)
		}
		if value != "" && given != value {
			return nil, unsupportedVideoParam(param, "%s is given twice with different values (%q and %q)", param, value, given)
		}
		value = given
		delete(e, key)
	}
	if value == "" {
		return nil, nil
	}
	// A copy escapes, not value itself: an absent parameter then allocates nothing.
	out := value
	return &out, nil
}

// veoImage converts a unified frame or reference image (a data URL, or a URL) into the vendor's
// image object. Veo takes bytes; on Vertex it also takes a Cloud Storage URI. Anything else is
// refused naming param, never sent without its image (fork, TXE-6537, contract C4: before, an
// http URL left only a mimeType on the wire).
func veoImage(value, param string, vertex bool) (*VideoImageData, error) {
	accepted := "a data URL"
	if vertex {
		accepted = "a data URL or a gs:// URI"
	}
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "gs://") {
		if !vertex {
			return nil, unsupportedVideoParam(param, "%s takes %s on this provider; a gs:// URI cannot be sent", param, accepted)
		}
		// The URI is sent exactly as given: re-encoding it would name another object.
		image := &VideoImageData{GCSURI: &trimmed}
		if info := schemas.ExtractURLTypeInfo(trimmed); info.MediaType != nil {
			image.MimeType = info.MediaType
		}
		return image, nil
	}
	sanitized, err := schemas.SanitizeImageURLWithAllowedSchemes(trimmed, defaultGeminiImageURLSchemes...)
	if err != nil {
		return nil, unsupportedVideoParam(param, "invalid %s: %v", param, err)
	}
	info := schemas.ExtractURLTypeInfo(sanitized)
	if info.DataURLWithoutPrefix == nil || *info.DataURLWithoutPrefix == "" {
		return nil, unsupportedVideoParam(param, "%s takes %s on this provider; an http(s) URL cannot be sent", param, accepted)
	}
	image := &VideoImageData{BytesBase64Encoded: info.DataURLWithoutPrefix, MimeType: schemas.Ptr("image/png")}
	if info.MediaType != nil {
		image.MimeType = info.MediaType
	}
	return image, nil
}

func videoImageDataFromExtraParam(value any) (*VideoImageData, bool) {
	if value == nil {
		return nil, false
	}

	switch v := value.(type) {
	case *VideoImageData:
		return v, v != nil
	case VideoImageData:
		return &v, true
	case map[string]any:
		image := &VideoImageData{}
		hasValue := false
		if bytesBase64Encoded, ok := schemas.SafeExtractStringPointer(v["bytesBase64Encoded"]); ok {
			image.BytesBase64Encoded = bytesBase64Encoded
			hasValue = true
		}
		if gcsURI, ok := schemas.SafeExtractStringPointer(v["gcsUri"]); ok {
			image.GCSURI = gcsURI
			hasValue = true
		}
		if mimeType, ok := schemas.SafeExtractStringPointer(v["mimeType"]); ok {
			image.MimeType = mimeType
			hasValue = true
		}
		if maskMode, ok := schemas.SafeExtractStringPointer(v["maskMode"]); ok {
			image.MaskMode = maskMode
			hasValue = true
		}
		if fileDataValue, exists := v["fileData"]; exists {
			if fileData, ok := fileDataFromExtraParam(fileDataValue); ok {
				image.FileData = fileData
				hasValue = true
			}
		}
		return image, hasValue
	default:
		if data, err := providerUtils.MarshalSorted(value); err == nil {
			var image VideoImageData
			if sonic.Unmarshal(data, &image) == nil && videoImageDataHasValue(&image) {
				return &image, true
			}
		}
	}
	return nil, false
}

func fileDataFromExtraParam(value any) (*FileData, bool) {
	if value == nil {
		return nil, false
	}

	switch v := value.(type) {
	case *FileData:
		return v, v != nil
	case FileData:
		return &v, true
	case map[string]any:
		fileData := &FileData{}
		hasValue := false
		if displayName, ok := schemas.SafeExtractString(v["displayName"]); ok {
			fileData.DisplayName = displayName
			hasValue = true
		}
		if fileURI, ok := schemas.SafeExtractString(v["fileUri"]); ok {
			fileData.FileURI = fileURI
			hasValue = true
		}
		if mimeType, ok := schemas.SafeExtractString(v["mimeType"]); ok {
			fileData.MIMEType = mimeType
			hasValue = true
		}
		return fileData, hasValue
	default:
		if data, err := providerUtils.MarshalSorted(value); err == nil {
			var fileData FileData
			if sonic.Unmarshal(data, &fileData) == nil && fileDataHasValue(&fileData) {
				return &fileData, true
			}
		}
	}
	return nil, false
}

func videoReferenceImagesFromExtraParam(value any) ([]VideoReferenceImage, bool) {
	switch v := value.(type) {
	case []VideoReferenceImage:
		return v, v != nil
	case []map[string]any:
		referenceImages := make([]VideoReferenceImage, 0, len(v))
		for _, item := range v {
			if referenceImage, ok := videoReferenceImageFromExtraParam(item); ok {
				referenceImages = append(referenceImages, referenceImage)
			}
		}
		return referenceImages, len(referenceImages) > 0
	case []any:
		referenceImages := make([]VideoReferenceImage, 0, len(v))
		for _, item := range v {
			if referenceImage, ok := videoReferenceImageFromExtraParam(item); ok {
				referenceImages = append(referenceImages, referenceImage)
			}
		}
		return referenceImages, len(referenceImages) > 0
	default:
		if data, err := providerUtils.MarshalSorted(value); err == nil {
			var referenceImages []VideoReferenceImage
			if sonic.Unmarshal(data, &referenceImages) == nil {
				return referenceImages, true
			}
		}
	}
	return nil, false
}

func videoReferenceImageFromExtraParam(value any) (VideoReferenceImage, bool) {
	switch v := value.(type) {
	case VideoReferenceImage:
		return v, true
	case map[string]any:
		referenceImage := VideoReferenceImage{}
		hasValue := false
		if imageValue, exists := v["image"]; exists {
			if image, ok := videoImageDataFromExtraParam(imageValue); ok {
				referenceImage.Image = *image
				hasValue = true
			}
		}
		if referenceType, ok := schemas.SafeExtractString(v["referenceType"]); ok {
			referenceImage.ReferenceType = referenceType
			hasValue = true
		}
		return referenceImage, hasValue
	default:
		if data, err := providerUtils.MarshalSorted(value); err == nil {
			var referenceImage VideoReferenceImage
			if sonic.Unmarshal(data, &referenceImage) == nil && videoReferenceImageHasValue(referenceImage) {
				return referenceImage, true
			}
		}
	}
	return VideoReferenceImage{}, false
}

func videoImageDataHasValue(image *VideoImageData) bool {
	return image != nil &&
		(image.BytesBase64Encoded != nil ||
			image.GCSURI != nil ||
			image.FileData != nil ||
			image.MimeType != nil ||
			image.MaskMode != nil)
}

func fileDataHasValue(fileData *FileData) bool {
	return fileData != nil &&
		(fileData.DisplayName != "" ||
			fileData.FileURI != "" ||
			fileData.MIMEType != "")
}

func videoReferenceImageHasValue(referenceImage VideoReferenceImage) bool {
	return referenceImage.ReferenceType != "" || videoImageDataHasValue(&referenceImage.Image)
}

func addVideoURLOutput(uri, contentType string) *schemas.VideoOutput {
	if uri == "" {
		return nil
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = defaultVideoContentType
	}
	return &schemas.VideoOutput{
		Type:        schemas.VideoOutputTypeURL,
		URL:         schemas.Ptr(uri),
		ContentType: contentType,
	}
}

// addVideoGCSOutput is the entry for a video the vendor wrote to Cloud Storage. Reading it needs
// the vendor's credentials, so it is a content entry with no url: the caller fetches it through
// GET /v1/videos/{id}/content (contract C9). The gs:// URI is the entry's id.
func addVideoGCSOutput(gcsURI, contentType string) *schemas.VideoOutput {
	if gcsURI == "" {
		return nil
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = defaultVideoContentType
	}
	return &schemas.VideoOutput{
		ID:          gcsURI,
		Type:        schemas.VideoOutputTypeContent,
		ContentType: contentType,
	}
}

func addVideoBase64Output(base64Value, contentType string) *schemas.VideoOutput {
	if base64Value == "" {
		return nil
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = defaultVideoContentType
	}
	return &schemas.VideoOutput{
		Type:        schemas.VideoOutputTypeBase64,
		Base64Data:  schemas.Ptr(base64Value),
		ContentType: contentType,
	}
}

func parseVideoDataURL(data string) (mimeType string, base64Payload string, ok bool) {
	if !strings.HasPrefix(data, "data:") {
		return "", "", false
	}
	parts := strings.SplitN(data, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	header := parts[0]
	payload := parts[1]
	if payload == "" {
		return "", "", false
	}
	header = strings.TrimPrefix(header, "data:")
	if before, _, found := strings.Cut(header, ";"); found {
		return before, payload, true
	}
	return header, payload, true
}

// ToGeminiVideoGenerationRequest converts a Bifrost video generation request to the Gemini API
// body of POST /models/{model}:predictLongRunning.
func ToGeminiVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*GeminiVideoGenerationRequest, error) {
	return toVeoVideoGenerationRequest(bifrostReq, false)
}

// ToVertexVideoGenerationRequest is ToGeminiVideoGenerationRequest for Vertex AI, whose Veo
// also takes frames and reference images as Cloud Storage URIs (gcsUri).
func ToVertexVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*GeminiVideoGenerationRequest, error) {
	return toVeoVideoGenerationRequest(bifrostReq, true)
}

// toVeoVideoGenerationRequest builds the predictLongRunning body. Every price-affecting field
// reaches the vendor under the vendor's name or the request is refused with
// caller_unsupported_parameter naming the field; none is dropped or replaced (contract C4).
func toVeoVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest, vertex bool) (*GeminiVideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil {
		return nil, fmt.Errorf("bifrost request or input is nil")
	}

	bifrostReq.Model = NormalizeModelName(bifrostReq.Model)

	instance := GeminiVideoGenerationInstance{
		Prompt: bifrostReq.Input.Prompt,
	}

	// Handle input reference (first frame for image-to-video)
	if bifrostReq.Input.InputReference != nil && *bifrostReq.Input.InputReference != "" {
		image, err := veoImage(*bifrostReq.Input.InputReference, "input_reference", vertex)
		if err != nil {
			return nil, err
		}
		instance.Image = image
	}

	if bifrostReq.Input.VideoURI != nil {
		instance.Video = &VideoGenerationVideoInput{
			URI: bifrostReq.Input.VideoURI,
		}
	}

	req := &GeminiVideoGenerationRequest{}

	// Map parameters if provided
	if p := bifrostReq.Params; p != nil {
		params := &VideoGenerationParameters{
			NegativePrompt: p.NegativePrompt,
			Seed:           p.Seed,
			GenerateAudio:  p.Audio,
		}

		if p.Seconds != nil {
			seconds, err := strconv.Atoi(*p.Seconds)
			if err != nil {
				return nil, unsupportedVideoParam("seconds", "seconds must be a whole number of seconds, got %q", *p.Seconds)
			}
			params.DurationSeconds = &seconds
		}

		// The caller's map is not changed: the converter works on a copy. A request with no
		// extra params copies nothing; every read and delete below is safe on the nil map.
		var extras veoExtras
		if len(p.ExtraParams) > 0 {
			extras = make(veoExtras, len(p.ExtraParams))
			for key, value := range p.ExtraParams {
				extras[key] = value
			}
		}

		var err error
		if params.AspectRatio, err = extras.unifiedString("aspect_ratio", string(p.AspectRatio), "aspect_ratio", "aspectRatio"); err != nil {
			return nil, err
		}
		if params.Resolution, err = extras.unifiedString("resolution", string(p.Resolution), "resolution"); err != nil {
			return nil, err
		}
		// output_storage_uri is the unified name of parameters.storageUri (contract C7). The
		// vendor's own name is taken as an extra param too, with either spelling of URI.
		storageURI := ""
		if p.OutputStorageURI != nil {
			storageURI = *p.OutputStorageURI
		}
		if params.StorageURI, err = extras.unifiedString(schemas.TaskControlFieldOutputStorageURI, storageURI, "storageUri", "storageURI"); err != nil {
			return nil, err
		}

		params.SampleCount = extras.intPtr("sampleCount")
		params.PersonGeneration = extras.stringPtr("personGeneration")
		params.NumberOfVideos = extras.intPtr("numberOfVideos")
		params.CompressionQuality = extras.stringPtr("compressionQuality")
		params.EnhancePrompt = extras.boolPtr("enhancePrompt")
		params.ResizeMode = extras.stringPtr("resizeMode")
		params.FPS = extras.value("fps")
		params.PubsubTopic = extras.value("pubsubTopic")
		params.Task = extras.value("task")

		if instance.LastFrame, err = veoLastFrame(p, extras, vertex); err != nil {
			return nil, err
		}
		if instance.ReferenceImages, err = veoReferenceImages(p, extras, vertex); err != nil {
			return nil, err
		}

		// size names an aspect ratio and a resolution tier at once.
		if p.Size != "" {
			size, ok := veoSizes[p.Size]
			if !ok {
				return nil, unsupportedVideoParam("size", "size %q is not a size Veo renders (1280x720, 720x1280, 1920x1080, 1080x1920, 3840x2160, 2160x3840); give aspect_ratio and resolution for anything else", p.Size)
			}
			if params.AspectRatio != nil && *params.AspectRatio != size.aspectRatio {
				return nil, unsupportedVideoParam("size", "size %q is %s, which contradicts aspect_ratio %q", p.Size, size.aspectRatio, *params.AspectRatio)
			}
			if params.Resolution != nil && !strings.EqualFold(*params.Resolution, size.resolution) {
				return nil, unsupportedVideoParam("size", "size %q is %s, which contradicts resolution %q", p.Size, size.resolution, *params.Resolution)
			}
			params.AspectRatio = &size.aspectRatio
			if params.Resolution == nil && size.resolution != veoDefaultResolution {
				params.Resolution = &size.resolution
			}
		}

		if len(extras) > 0 {
			req.ExtraParams = extras
		}
		req.Parameters = params
	}

	req.Instances = []GeminiVideoGenerationInstance{instance}
	return req, nil
}

// veoLastFrame returns instances[0].lastFrame: the unified last_frame (a data URL or a URL), or
// the vendor's own lastFrame object given as an extra param.
func veoLastFrame(p *schemas.VideoGenerationParameters, extras veoExtras, vertex bool) (*VideoImageData, error) {
	const param = "last_frame"
	if p.LastFrame != "" {
		if raw, twice := extras["lastFrame"]; twice && raw != nil {
			return nil, unsupportedVideoParam(param, "the last frame is given twice, as last_frame and as lastFrame")
		}
		delete(extras, param)
		return veoImage(string(p.LastFrame), param, vertex)
	}
	// A last_frame the typed field could not hold (not a string) is still in the extra params.
	if raw, ok := extras[param]; ok && raw != nil {
		image, ok := videoImageDataFromExtraParam(raw)
		if !ok {
			return nil, unsupportedVideoParam(param, "last_frame must be a data URL, a URL, or the vendor's image object")
		}
		delete(extras, param)
		return image, nil
	}
	if raw, ok := extras["lastFrame"]; ok {
		if image, ok := videoImageDataFromExtraParam(raw); ok {
			delete(extras, "lastFrame")
			return image, nil
		}
	}
	return nil, nil
}

// veoReferenceImages returns instances[0].referenceImages: the unified reference_images (data
// URLs or URLs, sent as asset references), or the vendor's own referenceImages objects given as
// an extra param.
func veoReferenceImages(p *schemas.VideoGenerationParameters, extras veoExtras, vertex bool) ([]VideoReferenceImage, error) {
	const param = "reference_images"
	if len(p.ReferenceImages) > 0 {
		if raw, twice := extras["referenceImages"]; twice && raw != nil {
			return nil, unsupportedVideoParam(param, "the reference images are given twice, as reference_images and as referenceImages")
		}
		delete(extras, param)
		referenceImages := make([]VideoReferenceImage, 0, len(p.ReferenceImages))
		for _, reference := range p.ReferenceImages {
			image, err := veoImage(reference, param, vertex)
			if err != nil {
				return nil, err
			}
			referenceImages = append(referenceImages, VideoReferenceImage{Image: *image, ReferenceType: veoReferenceTypeAsset})
		}
		return referenceImages, nil
	}
	// reference_images the typed field could not hold (not a list of strings) is still in the
	// extra params.
	if raw, ok := extras[param]; ok && raw != nil {
		referenceImages, ok := videoReferenceImagesFromExtraParam(raw)
		if !ok || len(referenceImages) == 0 {
			return nil, unsupportedVideoParam(param, "reference_images must be a list of data URLs or URLs, or the vendor's reference image objects")
		}
		delete(extras, param)
		return referenceImages, nil
	}
	if raw, ok := extras["referenceImages"]; ok {
		if referenceImages, ok := videoReferenceImagesFromExtraParam(raw); ok {
			delete(extras, "referenceImages")
			return referenceImages, nil
		}
	}
	return nil, nil
}

// The provider_status of a Veo task: the operation's done flag, followed by ",error.code=<n>"
// when the operation failed.
const (
	veoProviderStatusRunning = "done=false"
	veoProviderStatusDone    = "done=true"
)

// veoOperationOutputs reads a finished operation's response in each shape the vendor uses: the
// delivered videos, and the vendor's count of filtered videos with its reasons verbatim (nil
// when none was filtered).
func veoOperationOutputs(resp *GenerateVideosOperationResponse) ([]schemas.VideoOutput, *schemas.ContentFilterInfo) {
	if resp == nil {
		return nil, nil
	}
	var videos []schemas.VideoOutput
	add := func(output *schemas.VideoOutput) {
		if output != nil {
			videos = append(videos, *output)
		}
	}
	addGenerated := func(samples []*GeneratedVideo) {
		for _, sample := range samples {
			if sample == nil || sample.Video == nil {
				continue
			}
			add(addVideoURLOutput(sample.Video.URI, sample.Video.MIMEType))
			if len(sample.Video.VideoBytes) > 0 {
				add(addVideoBase64Output(base64.StdEncoding.EncodeToString(sample.Video.VideoBytes), sample.Video.MIMEType))
			}
		}
	}

	filteredCount, reasons := resp.RAIMediaFilteredCount, resp.RAIMediaFilteredReasons
	switch {
	case resp.GenerateVideoResponse != nil:
		// Gemini API shape.
		addGenerated(resp.GenerateVideoResponse.GeneratedSamples)
		if filteredCount == 0 && len(reasons) == 0 {
			filteredCount, reasons = resp.GenerateVideoResponse.RAIMediaFilteredCount, resp.GenerateVideoResponse.RAIMediaFilteredReasons
		}
	case len(resp.GeneratedVideos) > 0:
		// Backward compatibility for older response shapes
		addGenerated(resp.GeneratedVideos)
	default:
		// Vertex shape: bytes in the response, or a Cloud Storage URI when the request named a
		// storageUri.
		for _, video := range resp.Videos {
			mimeType := ""
			if video.MIMEType != nil {
				mimeType = *video.MIMEType
			}
			if video.GCSURI != nil && *video.GCSURI != "" {
				add(addVideoGCSOutput(*video.GCSURI, mimeType))
			} else if video.BytesBase64Encoded != nil {
				add(addVideoBase64Output(*video.BytesBase64Encoded, mimeType))
			}
		}
	}

	if filteredCount <= 0 {
		return videos, nil
	}
	return videos, &schemas.ContentFilterInfo{FilteredCount: int(filteredCount), Reasons: reasons}
}

// ToBifrostVideoGenerationResponse converts Gemini operation response to Bifrost format
func ToBifrostVideoGenerationResponse(operation *GenerateVideosOperation, model string) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	model = NormalizeModelName(model)

	if operation == nil {
		return nil, providerUtils.NewBifrostOperationError("operation is nil", nil)
	}

	response := &schemas.BifrostVideoGenerationResponse{
		ID:        operation.Name,
		Object:    "video",
		CreatedAt: time.Now().Unix(),
	}
	if model != "" {
		response.Model = model
	}

	// Set status based on operation state. A long-running operation has no status string: its
	// raw state is the done flag and, when it failed, error.code, and provider_status carries
	// exactly those (fork, TXE-6537, contract C7).
	switch {
	case !operation.Done:
		response.Status = schemas.VideoStatusInProgress
		response.ProviderStatus = veoProviderStatusRunning
		if operation.Metadata != nil {
			if p := providerUtils.GetJSONField([]byte(operation.Metadata), "progress"); p.Exists() {
				progress := p.Float()
				response.Progress = &progress
			}
		}
	case operation.Error != nil:
		response.Status = schemas.VideoStatusFailed
		code := providerUtils.GetJSONField(operation.Error, "code").String()
		message := providerUtils.GetJSONField(operation.Error, "message").String()
		response.ProviderStatus = veoProviderStatusDone + ",error.code=" + code
		if code == "" {
			code = "video_generation_failed"
		}
		if message == "" {
			message = string(operation.Error)
		}
		response.Error = &schemas.VideoCreateError{
			Code:    code,
			Message: message,
		}
	default:
		response.ProviderStatus = veoProviderStatusDone
		videos, filter := veoOperationOutputs(operation.Response)
		if len(videos) > 0 {
			response.Videos = videos
		}
		response.ContentFilter = filter
		// Delivered videos are in videos; filtered ones take no slot and are counted in
		// content_filter. A task whose every video was filtered failed (contract C6, C7).
		if len(videos) == 0 && filter != nil {
			response.Status = schemas.VideoStatusFailed
			errorMsg := "Content filtered by safety policies"
			if len(filter.Reasons) > 0 {
				errorMsg = filter.Reasons[0]
			}
			response.Error = &schemas.VideoCreateError{
				Code:    "content_filtered",
				Message: errorMsg,
			}
		} else {
			response.Status = schemas.VideoStatusCompleted
		}
	}

	// Try to extract timestamps from metadata
	if operation.Metadata != nil {
		if ct := providerUtils.GetJSONField([]byte(operation.Metadata), "createTime"); ct.Exists() {
			if t, err := time.Parse(time.RFC3339, ct.String()); err == nil {
				response.CreatedAt = t.Unix()
			}
		}
		if ut := providerUtils.GetJSONField([]byte(operation.Metadata), "updateTime"); ut.Exists() {
			if t, err := time.Parse(time.RFC3339, ut.String()); err == nil && operation.Done {
				response.CompletedAt = schemas.Ptr(t.Unix())
			}
		}
	}

	return response, nil
}

func (request *GeminiVideoGenerationRequest) ToBifrostVideoGenerationRequest(ctx *schemas.BifrostContext) (*schemas.BifrostVideoGenerationRequest, error) {
	if request == nil || len(request.Instances) == 0 {
		return nil, fmt.Errorf("request is nil or has no instances")
	}

	// Use the first instance for the main input
	instance := request.Instances[0]

	provider, model := schemas.ParseModelString(request.Model, "")

	bifrostReq := &schemas.BifrostVideoGenerationRequest{
		Provider: provider,
		Model:    model,
		Input: &schemas.VideoGenerationInput{
			Prompt: instance.Prompt,
		},
	}

	// Handle image input for image-to-video
	if instance.Image != nil && instance.Image.BytesBase64Encoded != nil && *instance.Image.BytesBase64Encoded != "" {
		// attach mime type and base64 string to input reference
		mimeType := "image/png"
		if instance.Image.MimeType != nil && *instance.Image.MimeType != "" {
			mimeType = *instance.Image.MimeType
		}
		bifrostReq.Input.InputReference = schemas.Ptr(fmt.Sprintf("data:%s;base64,%s", mimeType, *instance.Image.BytesBase64Encoded))
	} else if instance.Image != nil && instance.Image.GCSURI != nil && *instance.Image.GCSURI != "" {
		// A first frame named by its Cloud Storage URI stays a URI (fork, TXE-6537).
		bifrostReq.Input.InputReference = instance.Image.GCSURI
	}

	// Helper to ensure params are initialized
	ensureParams := func() {
		if bifrostReq.Params == nil {
			bifrostReq.Params = &schemas.VideoGenerationParameters{
				ExtraParams: make(map[string]any),
			}
		}
	}

	// Handle reference images
	if len(instance.ReferenceImages) > 0 {
		ensureParams()
		bifrostReq.Params.ExtraParams["referenceImages"] = instance.ReferenceImages
	}

	// Handle video URI
	if instance.Video != nil && instance.Video.URI != nil {
		bifrostReq.Input.VideoURI = instance.Video.URI
	}

	// Handle last frame
	if instance.LastFrame != nil {
		ensureParams()
		bifrostReq.Params.ExtraParams["lastFrame"] = instance.LastFrame
	}

	// Map parameters if provided
	if request.Parameters != nil {
		ensureParams()
		params := bifrostReq.Params

		if request.Parameters.NegativePrompt != nil {
			params.NegativePrompt = request.Parameters.NegativePrompt
		}
		if request.Parameters.DurationSeconds != nil {
			seconds := strconv.Itoa(*request.Parameters.DurationSeconds)
			params.Seconds = &seconds
		}
		if request.Parameters.Seed != nil {
			params.Seed = request.Parameters.Seed
		}
		if request.Parameters.GenerateAudio != nil {
			params.Audio = request.Parameters.GenerateAudio
		}
		if request.Parameters.AspectRatio != nil {
			// The unified field carries the ratio as well, so core's price-field rule sees
			// it as sent (schemas.CheckExtraParamPriceFieldsFor, TXE-7065).
			params.AspectRatio = schemas.TolerantString(*request.Parameters.AspectRatio)
			params.ExtraParams["aspectRatio"] = *request.Parameters.AspectRatio
		}
		if request.Parameters.Resolution != nil {
			// The unified field carries the tier; the extra param stays for the converters
			// that still read it there (schemas.UnifiedVideoGenerationFields). Sent only as
			// an extra param it would be refused (schemas.CheckExtraParamPriceFields).
			params.Resolution = schemas.TolerantString(*request.Parameters.Resolution)
			params.ExtraParams["resolution"] = *request.Parameters.Resolution
		}
		if request.Parameters.SampleCount != nil {
			params.ExtraParams["sampleCount"] = *request.Parameters.SampleCount
		}
		if request.Parameters.PersonGeneration != nil {
			params.ExtraParams["personGeneration"] = *request.Parameters.PersonGeneration
		}
		if request.Parameters.NumberOfVideos != nil {
			params.ExtraParams["numberOfVideos"] = *request.Parameters.NumberOfVideos
		}
		if request.Parameters.StorageURI != nil {
			params.ExtraParams["storageUri"] = *request.Parameters.StorageURI
		}
		if request.Parameters.FPS != nil {
			params.ExtraParams["fps"] = request.Parameters.FPS
		}
		if request.Parameters.PubsubTopic != nil {
			params.ExtraParams["pubsubTopic"] = request.Parameters.PubsubTopic
		}
		if request.Parameters.Task != nil {
			params.ExtraParams["task"] = request.Parameters.Task
		}
		if request.Parameters.CompressionQuality != nil {
			params.ExtraParams["compressionQuality"] = *request.Parameters.CompressionQuality
		}
		if request.Parameters.EnhancePrompt != nil {
			params.ExtraParams["enhancePrompt"] = *request.Parameters.EnhancePrompt
		}
		if request.Parameters.ResizeMode != nil {
			params.ExtraParams["resizeMode"] = *request.Parameters.ResizeMode
		}
	}

	return bifrostReq, nil
}

func ToGeminiVideoGenerationResponse(response *schemas.BifrostVideoGenerationResponse) *GenerateVideosOperation {
	if response == nil {
		return nil
	}

	decodedID := response.ID
	if decoded, err := url.PathUnescape(decodedID); err == nil {
		decodedID = decoded
	}

	// if id is in gemini or vertex format, set name in format models/model/operations/operation_id:provider
	// else make the id in gemini format
	if !(strings.HasPrefix(decodedID, "models/") && strings.Contains(decodedID, response.Model) && strings.Contains(decodedID, "operations/")) {
		// url encode model
		encodedModel := url.PathEscape(response.Model)
		decodedID = "models/" + encodedModel + "/operations/" + decodedID
	}
	operation := &GenerateVideosOperation{
		Name: decodedID,
	}

	switch response.Status {
	case schemas.VideoStatusCompleted:
		operation.Done = true
		if len(response.Videos) > 0 {
			generatedSamples := make([]*GeneratedVideo, 0, len(response.Videos))
			for _, output := range response.Videos {
				var video *Video

				switch output.Type {
				case schemas.VideoOutputTypeURL:
					if output.URL == nil || *output.URL == "" {
						continue
					}
					video = &Video{
						URI: *output.URL,
					}
					if output.ContentType != "" {
						video.MIMEType = output.ContentType
					}
				case schemas.VideoOutputTypeBase64:
					if output.Base64Data == nil || *output.Base64Data == "" {
						continue
					}
					base64Payload := *output.Base64Data
					mimeType := output.ContentType
					if parsedMimeType, payload, ok := parseVideoDataURL(*output.Base64Data); ok {
						base64Payload = payload
						if mimeType == "" {
							mimeType = parsedMimeType
						}
					}
					decoded, err := base64.StdEncoding.DecodeString(base64Payload)
					if err != nil {
						continue
					}
					if mimeType == "" {
						mimeType = defaultVideoContentType
					}
					video = &Video{
						VideoBytes: decoded,
						MIMEType:   mimeType,
					}
				case schemas.VideoOutputTypeContent:
					// A video the vendor wrote to Cloud Storage: its gs:// URI is the entry's id.
					if !strings.HasPrefix(output.ID, "gs://") {
						continue
					}
					video = &Video{
						URI:      output.ID,
						MIMEType: output.ContentType,
					}
				default:
					continue
				}

				if video == nil {
					continue
				}
				generatedSamples = append(generatedSamples, &GeneratedVideo{
					Video: video,
				})
			}
			if len(generatedSamples) > 0 {
				operation.Response = &GenerateVideosOperationResponse{
					GenerateVideoResponse: &GenerateVideoResponse{
						GeneratedSamples: generatedSamples,
					},
				}
				// Some videos were delivered and some filtered: the count travels with them.
				if response.ContentFilter != nil && response.ContentFilter.FilteredCount > 0 {
					operation.Response.GenerateVideoResponse.RAIMediaFilteredCount = int32(response.ContentFilter.FilteredCount)
					operation.Response.GenerateVideoResponse.RAIMediaFilteredReasons = response.ContentFilter.Reasons
				}
			}
		}
	case schemas.VideoStatusFailed:
		operation.Done = true
		// Check if this is a content filtering case
		if response.ContentFilter != nil && response.ContentFilter.FilteredCount > 0 {
			operation.Response = &GenerateVideosOperationResponse{
				GenerateVideoResponse: &GenerateVideoResponse{
					RAIMediaFilteredCount:   int32(response.ContentFilter.FilteredCount),
					RAIMediaFilteredReasons: response.ContentFilter.Reasons,
				},
			}
		} else if response.Error != nil {
			errBytes, _ := providerUtils.MarshalSorted(map[string]any{
				"message": response.Error.Message,
				"code":    response.Error.Code,
			})
			operation.Error = json.RawMessage(errBytes)
		}
	default:
		operation.Done = false
	}

	return operation
}
