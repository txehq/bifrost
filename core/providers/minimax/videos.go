package minimax

import (
	"maps"
	"math"
	"strconv"
	"strings"

	providerUtils "github.com/capsohq/bifrost/core/providers/utils"
	schemas "github.com/capsohq/bifrost/core/schemas"
)

const (
	miniMaxDefaultVideoDuration = 6
	miniMaxVideoMaxDuration     = 15
)

// miniMaxVideoRatios are the concrete ratios /v2/video_generation accepts besides "adaptive".
var miniMaxVideoRatios = []struct {
	name  string
	ratio float64
}{
	{"21:9", 21.0 / 9},
	{"16:9", 16.0 / 9},
	{"4:3", 4.0 / 3},
	{"1:1", 1},
	{"3:4", 3.0 / 4},
	{"9:16", 9.0 / 16},
}

func isMiniMaxH3MaxModel(model string) bool {
	return strings.EqualFold(model, "MiniMax-H3-Max")
}

// ToMiniMaxVideoGenerationRequest converts a Bifrost video generation request to MiniMax's /v2/video_generation body.
func ToMiniMaxVideoGenerationRequest(bifrostReq *schemas.BifrostVideoGenerationRequest) (*MiniMaxVideoGenerationRequest, error) {
	if bifrostReq == nil || bifrostReq.Input == nil || strings.TrimSpace(bifrostReq.Input.Prompt) == "" {
		return nil, providerUtils.InvalidRequestErrorf("prompt is required")
	}
	if bifrostReq.Input.VideoURI != nil && *bifrostReq.Input.VideoURI != "" {
		return nil, providerUtils.InvalidRequestErrorf("video_uri is not supported by minimax video generation")
	}

	req := &MiniMaxVideoGenerationRequest{
		Model:    bifrostReq.Model,
		Content:  []MiniMaxVideoContentItem{{Type: "text", Text: bifrostReq.Input.Prompt}},
		Duration: miniMaxDefaultVideoDuration,
	}
	hasFrameImage := false
	if bifrostReq.Input.InputReference != nil && *bifrostReq.Input.InputReference != "" {
		url, err := schemas.SanitizeImageURL(*bifrostReq.Input.InputReference)
		if err != nil {
			return nil, providerUtils.InvalidRequestErrorf("invalid input reference: %v", err)
		}
		req.Content = append(req.Content, MiniMaxVideoContentItem{Type: "image_url", Role: "first_frame", ImageURL: &MiniMaxVideoMediaURL{URL: url}})
		hasFrameImage = true
	}

	var width, height int
	params := bifrostReq.Params
	if params != nil {
		if params.Seconds != nil && *params.Seconds != "" {
			seconds, err := strconv.ParseFloat(*params.Seconds, 64)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid seconds value: %v", err)
			}
			req.Duration = int(math.Round(seconds))
		}
		if params.Size != "" {
			w, h, err := parseImageSize(params.Size)
			if err != nil {
				return nil, err
			}
			width, height = w, h
		}
		// The unified aspect_ratio is MiniMax's ratio (fork, TXE-7065). It overrides the
		// ratio derived from size, with or without extra params: a Go SDK request sends
		// none.
		if params.AspectRatio != "" {
			req.Ratio = schemas.Ptr(string(params.AspectRatio))
		}
		if len(params.ExtraParams) > 0 {
			req.ExtraParams = make(map[string]interface{}, len(params.ExtraParams))
			maps.Copy(req.ExtraParams, params.ExtraParams)
		}
		// The unified last_frame and reference_images (TXE-6518) are read from their typed fields,
		// else from the copy the handlers keep in the extra params. A Go SDK request sets only the
		// typed field, and its images reached MiniMax as a text-only job (fork, TXE-7409).
		// MiniMax takes an image only as a URL or data URL string. Any other form (xAI's {"url": ...}
		// object, a bare string of reference images) is refused rather than passed through as an
		// unknown field: settlement counts what a vendor can receive, and a request that is refused
		// is never billed.
		lastFrame := string(params.LastFrame)
		if lastFrame == "" {
			if raw, present := req.ExtraParams["last_frame"]; present && raw != nil {
				s, ok := raw.(string)
				if !ok {
					return nil, providerUtils.InvalidRequestErrorf("last_frame must be a URL or data URL string")
				}
				lastFrame = s
			}
		}
		if lastFrame != "" {
			delete(req.ExtraParams, "last_frame")
			url, err := schemas.SanitizeImageURL(lastFrame)
			if err != nil {
				return nil, providerUtils.InvalidRequestErrorf("invalid last_frame: %v", err)
			}
			req.Content = append(req.Content, MiniMaxVideoContentItem{Type: "image_url", Role: "last_frame", ImageURL: &MiniMaxVideoMediaURL{URL: url}})
			hasFrameImage = true
		}
		refs := []string(params.ReferenceImages)
		if len(refs) == 0 {
			if raw, present := req.ExtraParams["reference_images"]; present && raw != nil {
				fromExtra, err := schemas.ConvertViaJSON[[]string](raw)
				if err != nil {
					return nil, providerUtils.InvalidRequestErrorf("reference_images must be a list of URL or data URL strings")
				}
				refs = fromExtra
			}
		}
		if len(refs) > 0 {
			if hasFrameImage {
				return nil, providerUtils.InvalidRequestErrorf("reference_images cannot be combined with first or last frame images")
			}
			for _, ref := range refs {
				url, err := schemas.SanitizeImageURL(ref)
				if err != nil {
					return nil, providerUtils.InvalidRequestErrorf("invalid reference image: %v", err)
				}
				req.Content = append(req.Content, MiniMaxVideoContentItem{Type: "image_url", Role: "reference_image", ImageURL: &MiniMaxVideoMediaURL{URL: url}})
			}
			delete(req.ExtraParams, "reference_images")
		}
		if len(req.ExtraParams) > 0 {
			if v, ok := req.ExtraParams["resolution"].(string); ok && v != "" {
				req.Resolution = v
				delete(req.ExtraParams, "resolution")
			}
			// The copy of aspect_ratio the handlers keep among the extra params is dropped,
			// so it is not also sent as a field MiniMax does not know. MiniMax's own name,
			// ratio, read below, works only for a request that also sends aspect_ratio with
			// the same value; core refuses it otherwise
			// (schemas.CheckExtraParamPriceFieldsFor).
			delete(req.ExtraParams, "aspect_ratio")
			if v, ok := req.ExtraParams["ratio"].(string); ok && v != "" {
				req.Ratio = &v
				delete(req.ExtraParams, "ratio")
			}
			if v, ok := req.ExtraParams["callback_url"].(string); ok && v != "" {
				req.CallbackURL = &v
				delete(req.ExtraParams, "callback_url")
			}
		}
	}

	isMax := isMiniMaxH3MaxModel(bifrostReq.Model)
	minDuration := 4
	if isMax {
		minDuration = 5
	}
	req.Duration = max(minDuration, min(miniMaxVideoMaxDuration, req.Duration))

	if req.Resolution == "" {
		req.Resolution = miniMaxVideoResolution(width, height, isMax)
	}
	if req.Ratio == nil {
		switch {
		case width > 0 && height > 0:
			req.Ratio = schemas.Ptr(nearestMiniMaxVideoRatio(width, height))
		case hasFrameImage:
			req.Ratio = schemas.Ptr("adaptive")
		default:
			// Text-to-video requires a concrete ratio.
			req.Ratio = schemas.Ptr("16:9")
		}
	}
	return req, nil
}

// miniMaxVideoResolution picks a resolution tier from the requested size, staying inside what the model supports (H3: 768P/2K, H3-Max: 480P/768P).
func miniMaxVideoResolution(width, height int, isMax bool) string {
	short := min(width, height)
	switch {
	case short == 0:
		return "768P"
	case short <= 480 && isMax:
		return "480P"
	case short > 1080 && !isMax:
		return "2K"
	default:
		return "768P"
	}
}

func nearestMiniMaxVideoRatio(width, height int) string {
	target := float64(width) / float64(height)
	best := miniMaxVideoRatios[0].name
	bestDiff := math.Inf(1)
	for _, r := range miniMaxVideoRatios {
		if diff := math.Abs(math.Log(target / r.ratio)); diff < bestDiff {
			best, bestDiff = r.name, diff
		}
	}
	return best
}

// miniMaxVideoShortEdges maps MiniMax resolution tiers to their short edge in pixels.
var miniMaxVideoShortEdges = map[string]int{
	"480P":  480,
	"720P":  720,
	"768P":  768,
	"1080P": 1080,
	"2K":    1440,
}

// miniMaxVideoSize turns a task's resolution tier and ratio into the "WxH" size the video pricing
// bands read (a short edge of 480 selects the 480p rate). It returns "" when either is unknown, so
// settlement keeps the size captured from the request instead of an unparseable tier label.
func miniMaxVideoSize(resolution, ratio string) string {
	short, ok := miniMaxVideoShortEdges[strings.ToUpper(strings.TrimSpace(resolution))]
	if !ok {
		return ""
	}
	w, h, ok := strings.Cut(ratio, ":")
	if !ok {
		return ""
	}
	rw, errW := strconv.Atoi(strings.TrimSpace(w))
	rh, errH := strconv.Atoi(strings.TrimSpace(h))
	if errW != nil || errH != nil || rw <= 0 || rh <= 0 {
		return ""
	}
	if rw >= rh {
		return strconv.Itoa(int(math.Round(float64(short)*float64(rw)/float64(rh)))) + "x" + strconv.Itoa(short)
	}
	return strconv.Itoa(short) + "x" + strconv.Itoa(int(math.Round(float64(short)*float64(rh)/float64(rw))))
}

// ToBifrostVideoGenerationResponse converts a queried MiniMax video task to Bifrost format.
func ToBifrostVideoGenerationResponse(task *MiniMaxVideoTask) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	if task == nil {
		return nil, providerUtils.NewBifrostOperationError("video task is nil", nil)
	}
	resp := &schemas.BifrostVideoGenerationResponse{
		ID:        task.ID,
		Object:    "video",
		Model:     task.Model,
		CreatedAt: task.CreatedAt,
		Size:      miniMaxVideoSize(task.Resolution, task.Ratio),
	}
	if task.Duration > 0 {
		resp.Seconds = schemas.Ptr(strconv.Itoa(task.Duration))
	}
	switch task.Status {
	case MiniMaxVideoTaskStatusQueued:
		resp.Status = schemas.VideoStatusQueued
	case MiniMaxVideoTaskStatusRunning:
		resp.Status = schemas.VideoStatusInProgress
	case MiniMaxVideoTaskStatusSucceeded:
		resp.Status = schemas.VideoStatusCompleted
		if task.UpdatedAt > 0 {
			resp.CompletedAt = schemas.Ptr(task.UpdatedAt)
		}
	case MiniMaxVideoTaskStatusFailed, MiniMaxVideoTaskStatusCancelled:
		resp.Status = schemas.VideoStatusFailed
		resp.Error = &schemas.VideoCreateError{Code: string(task.Status), Message: "task " + string(task.Status)}
		if task.Error != nil {
			if task.Error.Code != "" {
				resp.Error.Code = task.Error.Code
			}
			if task.Error.Message != "" {
				resp.Error.Message = task.Error.Message
			}
		}
	default:
		resp.Status = schemas.VideoStatusQueued
	}
	if task.Content != nil {
		if task.Content.URL != "" {
			resp.Videos = []schemas.VideoOutput{{Type: schemas.VideoOutputTypeURL, URL: schemas.Ptr(task.Content.URL), ContentType: "video/mp4"}}
		}
		resp.Prompt = task.Content.Prompt
	}
	return resp, nil
}
