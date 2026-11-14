package datasheet

import (
	"math"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
)

// VideoPricingDimensions is every factor that can move the price of one video job.
//
// It exists because a video is billed long after it is asked for. The request says
// what was ordered, the terminal response says what was actually produced, and
// neither alone is enough: most providers' retrieve response reports little beyond
// "done", so a dimension not captured at submission is usually unrecoverable by the
// time settlement runs.
//
// Adding a dimension is deliberately cheap. The whole struct is persisted as JSON
// in a single TEXT column, so a new field needs no migration and no capture-side
// change beyond setting it — which is the point: the provider rate cards are not
// fully mapped yet, and a factor worth capturing should not have to wait for one.
// Fields that no rate reads yet are still worth recording.
type VideoPricingDimensions struct {
	// Model is the resolved model, which matters because several providers never
	// echo it back on retrieve.
	Model string `json:"model,omitempty"`
	// RequestType distinguishes generation from remix and edit. They can price on
	// different bases even for the same model.
	RequestType schemas.RequestType `json:"request_type,omitempty"`

	// Seconds and Size are the two dimensions every published video rate card uses
	// today. Size is "WxH"; the rate is banded on its short edge.
	Seconds *int   `json:"seconds,omitempty"`
	Size    string `json:"size,omitempty"`
	// Resolution is the tier label ("480p", "2K", "4k") a request names instead of a size.
	// Veo, Wan 2.7+ and MiniMax take one; it selects the rate band when Size names none
	// (TXE-7349).
	Resolution string `json:"resolution,omitempty"`
	// SizeReported is true when Size came from the provider's response rather than the
	// request. A requested size is not always what was rendered: Veo reads it only for the
	// aspect ratio and renders at the requested tier, so only a reported size outranks the
	// tier. It is set by VideoDimensionsFromResponse and MergedWith and never persisted.
	SizeReported bool `json:"-"`
	// ResolutionSettled is true when Resolution is the tier a settlement recorded as the one that
	// set the band (VideoCostDetails.PricedResolution, or TierOnlyResolution on an unpriced job).
	// That choice was made with the request's sources and the custom provider's base in hand,
	// neither of which a reprice has, so the tier prices the job as recorded instead of being
	// judged again against videoTierReaders. Only RepriceFromLog sets it; it is never persisted.
	ResolutionSettled bool `json:"-"`
	// BaseProvider is the base provider of a custom provider, set by the caller that can
	// resolve it. Pricing reads it only to know which converter rendered the job (see
	// videoProvidersRenderingRequestedTier); the custom name still selects the rates. It is
	// never persisted.
	BaseProvider schemas.ModelProvider `json:"-"`

	// Type is the operation selector ("upscale", "3d"). These do not bill per
	// second of output at all, so a rate keyed only on duration is wrong for them.
	Type *string `json:"type,omitempty"`
	// UpscaleFactor and TargetMegapixels are the upscale operation's own basis;
	// they are mutually exclusive.
	UpscaleFactor    *int `json:"upscale_factor,omitempty"`
	TargetMegapixels *int `json:"target_megapixels,omitempty"`

	// Audio is the create's audio flag. It selects a band's with-audio rate where the model
	// publishes one (TXE-7306); nil takes the provider's audio default (VideoAudioDefaults).
	Audio *bool `json:"audio,omitempty"`

	// Mode is Kling's generation mode ("pro", "standard"), which has its own per-second
	// rate (fork, TXE-7357). Empty reads the "mode" the create carried in Extra.
	Mode string `json:"mode,omitempty"`
	// VideoInput is whether the job took a video as input (a video_uri, Seedance's
	// reference_videos, or input video seconds the vendor billed). Seedance prices such a
	// job at its band's video-input rate (TXE-7357).
	VideoInput *bool `json:"video_input,omitempty"`
	// OutputTokens is the output video tokens the vendor reported (Seedance's completion
	// tokens, Gemini Omni's video tokens). A model priced per video token is billed by them
	// rather than by seconds (TXE-7357).
	OutputTokens *int `json:"output_tokens,omitempty"`

	// OutputCount is how many clips the job actually returned. It is 0 until the
	// job produces them, and a job that has not finished still owes one clip's
	// worth rather than nothing — see computeVideoCost.
	OutputCount int `json:"output_count,omitempty"`

	// ProviderCost is the provider's own figure for the job, when it reports one
	// (Runware does). It wins over every rate below.
	ProviderCost *float64 `json:"provider_cost,omitempty"`

	// Extra carries request knobs that are not modeled above yet. It is the
	// research hatch: capture a provider-specific parameter here as soon as it
	// looks price-relevant, and promote it to a typed field once its rate is
	// actually published. Pricing reads only its "resolution", and only for rows
	// written before Resolution existed (see BandResolution).
	Extra map[string]any `json:"extra,omitempty"`
}

// MergedWith layers observed dimensions over captured ones: whatever the argument
// actually knows wins, and the receiver fills every remaining gap.
//
// The direction matters at settlement. The request is a statement of intent — a
// provider may clamp a duration, downscale a resolution, or return fewer clips than
// asked for — so the terminal response is authoritative wherever it says anything
// at all, and the submitted request is the fallback for everything it stays silent
// about (which, for most providers, is nearly everything).
func (d VideoPricingDimensions) MergedWith(observed VideoPricingDimensions) VideoPricingDimensions {
	out := d
	if observed.Model != "" {
		out.Model = observed.Model
	}
	if observed.RequestType != "" {
		out.RequestType = observed.RequestType
	}
	if observed.Seconds != nil {
		out.Seconds = observed.Seconds
	}
	if observed.Size != "" {
		out.Size = observed.Size
		out.SizeReported = observed.SizeReported
	}
	if observed.Resolution != "" {
		out.Resolution = observed.Resolution
	}
	if observed.Type != nil {
		out.Type = observed.Type
	}
	if observed.UpscaleFactor != nil {
		out.UpscaleFactor = observed.UpscaleFactor
	}
	if observed.TargetMegapixels != nil {
		out.TargetMegapixels = observed.TargetMegapixels
	}
	if observed.Audio != nil {
		out.Audio = observed.Audio
	}
	if observed.Mode != "" {
		out.Mode = observed.Mode
	}
	// A video input the request named stays one: a response that reports no input seconds
	// does not mean the job had no video input.
	if observed.VideoInput != nil && (out.VideoInput == nil || *observed.VideoInput) {
		out.VideoInput = observed.VideoInput
	}
	if observed.OutputTokens != nil {
		out.OutputTokens = observed.OutputTokens
	}
	// A finished job reporting zero clips is a job that produced nothing, not a job
	// we lack a count for — but only the response can say that, so it only ever
	// overwrites upward from the request, which never carries a count.
	if observed.OutputCount > 0 {
		out.OutputCount = observed.OutputCount
	}
	if observed.ProviderCost != nil {
		out.ProviderCost = observed.ProviderCost
	}
	for key, value := range observed.Extra {
		if out.Extra == nil {
			out.Extra = map[string]any{}
		}
		out.Extra[key] = value
	}
	return out
}

// VideoCostDetails is the outcome of pricing one video job. Priced separates "this
// costs nothing" from "no rate could be found", which the settlement engine treats
// as entirely different states.
type VideoCostDetails struct {
	Cost             float64
	Priced           bool
	ProviderCostUsed bool
	Breakdown        *schemas.BifrostCost
	// PricedResolution is the resolution tier that selected the rate band, or "" when a size
	// did (or nothing banded the job). The settlement records it, so a reprice prices from
	// the tier alone, or the size alone, exactly as settlement chose (TXE-7349): a reprice
	// cannot resolve a custom provider's base, so it must not rerun that choice.
	PricedResolution string
}

// CalculateVideoCostDetails prices a video job from its merged dimensions.
//
// Resolution order is provider-reported cost, then the catalog rate, then nothing.
// A provider that hands back an exact figure is always right about its own bill, so
// no estimate is allowed to override it, unless the provider is configured to ignore its reported cost.
func (s *Store) CalculateVideoCostDetails(dims VideoPricingDimensions, provider schemas.ModelProvider, scopes *LookupScopes) VideoCostDetails {
	if dims.ProviderCost != nil && *dims.ProviderCost > 0 && !s.IsProviderCostIgnored(provider) {
		return VideoCostDetails{
			Cost:             *dims.ProviderCost,
			Priced:           true,
			ProviderCostUsed: true,
			Breakdown:        &schemas.BifrostCost{TotalCost: *dims.ProviderCost},
		}
	}

	requestType := dims.RequestType
	if requestType == "" {
		requestType = schemas.VideoGenerationRequest
	}

	var lookupScopes LookupScopes
	if scopes != nil {
		lookupScopes = *scopes
	}
	pricing := s.resolvePricing(schemas.RoutingInfo{Provider: provider, Model: dims.Model}, requestType, lookupScopes)
	if pricing == nil {
		return VideoCostDetails{}
	}

	// Duration is the only basis wired today. Operation-selector jobs (upscale, 3d)
	// bill on factor or megapixels instead, and no rate for those is published to
	// the catalog yet — so they resolve to unpriced rather than being silently
	// billed as if they were a per-second generation of the same length.
	// The audio flag the create captured selects a band's with-audio rate where the model has
	// one (TXE-7306); an unstated flag takes the provider's audio default.
	audio := VideoPricedWithAudio(provider, dims.Audio)
	band := dims.bandSize(provider, pricing, audio)
	breakdown := computeVideoCost(pricing, nil, dims.Seconds, band, dims.OutputCount, audio, dims.job(), serviceTier{})
	if breakdown == nil || breakdown.TotalCost <= 0 {
		return VideoCostDetails{}
	}
	details := VideoCostDetails{Cost: breakdown.TotalCost, Priced: true, Breakdown: breakdown}
	if band != dims.Size {
		details.PricedResolution = band
	}
	return details
}

// BandResolution returns the resolution tier label that names a rate band: Resolution,
// else a "resolution" captured in Extra, else "". Extra covers job rows written before
// Resolution existed, which hold the tier only there, so a job in flight across the
// upgrade still settles at its band (TXE-7349).
func (d VideoPricingDimensions) BandResolution() string {
	if videoResolutionBand(d.Resolution) > 0 {
		return d.Resolution
	}
	if resolution, ok := d.Extra["resolution"].(string); ok && videoResolutionBand(resolution) > 0 {
		return resolution
	}
	return ""
}

// videoTierReader says where a provider's create converter reads the resolution tier from, so a
// tier the provider renders can set the band and one it never reads cannot (TXE-7349). Typed is
// the unified field (VideoGenerationParameters.Resolution); extra is extra_params.resolution.
// The HTTP adapters fill both; a Go SDK caller may set one. Each entry names the code that reads
// it, and a provider is added only from its converter.
type videoTierReader struct {
	typed, extra bool
	source       string
}

var videoTierReaders = map[schemas.ModelProvider]videoTierReader{
	schemas.Gemini:    {typed: true, extra: true, source: "gemini.ToGeminiVideoGenerationRequest: parameters.resolution from extra_params.resolution, else the typed field; size sets only aspectRatio"},
	schemas.Vertex:    {typed: true, extra: true, source: "vertex sends Veo through gemini.ToGeminiVideoGenerationRequest"},
	schemas.MiniMax:   {extra: true, source: "minimax video create: extra_params.resolution replaces the tier derived from size"},
	schemas.DashScope: {typed: true, source: "dashscope video create: parameters.resolution from the typed field (Wan 2.7+, and image-to-video before it)"},
	schemas.Kling:     {typed: true, source: "kling video create: settings.resolution from the typed field"},
	schemas.BytePlus:  {typed: true, source: "byteplus video create: the typed field overrides the tier a size implies"},
}

// requestedTier is the resolution tier the request named and whether the provider that rendered
// the job reads it from where it was named. A tier the provider reads from either source counts
// as rendered.
func (d VideoPricingDimensions) requestedTier(renderer schemas.ModelProvider) (string, bool) {
	reader := videoTierReaders[renderer]
	typed := ""
	if videoResolutionBand(d.Resolution) > 0 {
		typed = d.Resolution
	}
	extra := ""
	if resolution, ok := d.Extra["resolution"].(string); ok && videoResolutionBand(resolution) > 0 {
		extra = resolution
	}
	switch {
	case typed != "" && reader.typed:
		return typed, true
	case extra != "" && reader.extra:
		return extra, true
	case typed != "":
		return typed, false
	default:
		return extra, false
	}
}

func (d VideoPricingDimensions) renderer(provider schemas.ModelProvider) schemas.ModelProvider {
	if d.BaseProvider != "" {
		return d.BaseProvider
	}
	return provider
}

// bandSize returns what the rate band is read from (TXE-7349):
//   - a tier a settlement recorded as the one that set the band (ResolutionSettled), on a
//     reprice;
//   - a size the provider reported, which is what it rendered;
//   - else a requested tier the provider reads (see videoTierReaders), which is what it
//     rendered whatever size was also asked for (Veo with size 3840x2160 and resolution 1080p
//     renders 1080p);
//   - else the requested size, or the unbanded rate when there is none, unless a tier the
//     provider may ignore costs more. A caller cannot lower the price with a tier the provider
//     never reads: the job is priced at the dearer of that tier and what it would cost without it.
func (d VideoPricingDimensions) bandSize(provider schemas.ModelProvider, pricing *configstoreTables.TableModelPricing, audio bool) string {
	if d.ResolutionSettled && videoResolutionBand(d.Resolution) > 0 {
		return d.Resolution
	}
	if d.SizeReported && videoResolutionBand(d.Size) > 0 {
		return d.Size
	}
	tier, rendered := d.requestedTier(d.renderer(provider))
	if tier == "" {
		return d.Size
	}
	if rendered {
		return tier
	}
	job := d.job()
	if videoBandCharge(pricing, tier, audio, d.Seconds, job) > videoBandCharge(pricing, d.Size, audio, d.Seconds, job) {
		return tier
	}
	return d.Size
}

// videoBandCharge is what a job of this length is charged when its band is read from size: the
// band's per-clip price for the length, else the job's per-second rate at the band (video-input,
// band, mode, unbanded) for its seconds. bandSize compares two sizes by it, so a model priced
// only per clip (MiniMax Hailuo's 768p and 1080p clips, TXE-7357) still prices a requested tier
// the provider may ignore when that tier is dearer.
func videoBandCharge(pricing *configstoreTables.TableModelPricing, size string, audio bool, seconds *int, job videoJob) float64 {
	band := videoResolutionBand(size)
	if clip := videoClipRate(pricing, band, seconds); clip != nil {
		return *clip
	}
	length := 1
	if seconds != nil && *seconds > 0 {
		length = *seconds
	}
	return videoJobOutputPerSecondRate(pricing, band, audio, job) * float64(length)
}

// TierOnlyResolution is the tier a settlement that could not be priced records: a tier the
// provider reads, unless the provider reported a size that names a band, and "" otherwise. It
// follows bandSize's first two rules, which need no rates: a reported size is what was rendered,
// and a read tier is rendered whatever size was requested (Veo with size 1280x720 and resolution
// 4k renders 4K). Without rates the dearer of an unread tier and the size cannot be told, so the
// record leaves that choice to the size; a reprice then never takes a cheaper tier the provider
// may ignore.
func (d VideoPricingDimensions) TierOnlyResolution(provider schemas.ModelProvider) string {
	if d.SizeReported && videoResolutionBand(d.Size) > 0 {
		return ""
	}
	if tier, rendered := d.requestedTier(d.renderer(provider)); rendered {
		return tier
	}
	return ""
}

// PricedMode returns the generation mode the job is priced at: Mode, else the "mode" the
// create carried in Extra (Replicate's Kling takes it as an extra param), else "".
func (d VideoPricingDimensions) PricedMode() string {
	if d.Mode != "" {
		return d.Mode
	}
	if mode, ok := d.Extra["mode"].(string); ok {
		return mode
	}
	return ""
}

// job returns the dimensions computeVideoCost chooses a rate by beyond size and length.
func (d VideoPricingDimensions) job() videoJob {
	return videoJob{
		mode:         d.PricedMode(),
		videoInput:   d.VideoInput != nil && *d.VideoInput,
		outputTokens: d.OutputTokens,
	}
}

// videoBillingExtraKeys are the extra params video pricing reads from VideoPricingDimensions.Extra:
// Kling's mode (PricedMode) and the resolution tier a provider reads from extra_params
// (requestedTier). TestVideoBillingExtraKeysCoverEveryExtraRead parses this file and fails when
// pricing reads another key from Extra without it being listed here.
var videoBillingExtraKeys = []string{"mode", "resolution"}

// VideoBillingSelectors returns the extra params of a video create that choose its rate (see
// videoBillingExtraKeys), or nil when it carries none. It is what a job keeps of the extra params
// when the request's content may not be stored (TXE-7357): the rest of extra_params can carry the
// caller's content (prompts, reference URLs), and pricing reads none of it.
func VideoBillingSelectors(extra map[string]any) map[string]any {
	var out map[string]any
	for _, key := range videoBillingExtraKeys {
		if value, ok := extra[key].(string); ok {
			if out == nil {
				out = make(map[string]any, len(videoBillingExtraKeys))
			}
			out[key] = value
		}
	}
	return out
}

// VideoRequestHasVideoInput reports whether a video create sends the vendor a video as input:
// a video_uri, or the reference_videos extra param that Seedance reads (BytePlus maps it to its
// reference_video content; Replicate passes extra params to the model as they are). provider is
// the name the create was routed under; the answer no longer depends on it.
func VideoRequestHasVideoInput(provider schemas.ModelProvider, input *schemas.VideoGenerationInput, extra map[string]any) bool {
	// Every video converter either sends a video_uri or refuses the create (Replicate and the
	// OpenAI create form, which OpenAI, Azure's v1 videos and Volcengine send, refuse it:
	// TXE-7374), so a recorded job with one received it.
	if input != nil && input.VideoURI != nil && strings.TrimSpace(*input.VideoURI) != "" {
		return true
	}
	switch refs := extra["reference_videos"].(type) {
	case []string:
		return len(refs) > 0
	case []any:
		return len(refs) > 0
	case string:
		return strings.TrimSpace(refs) != ""
	}
	return false
}

// videoClipCount counts the billable clips in outputs, skipping image outputs such as a task's last frame.
func videoClipCount(outputs []schemas.VideoOutput) int {
	n := 0
	for i := range outputs {
		if !strings.HasPrefix(outputs[i].ContentType, "image/") {
			n++
		}
	}
	return n
}

// VideoDimensionsFromResponse reads back everything a terminal video response
// actually reports. What that amounts to varies enormously by provider — OpenAI
// echoes model, duration and size; several others report only a status and a URL —
// which is precisely why the submitted request is kept as the fallback.
// VideoSeconds reads a video duration as the whole seconds a per-second rate is multiplied by.
// Vendors report a rendered clip's length with a fraction (Kling "5.1", xAI 6.04, DashScope
// 5.0), which is rounded to the nearest second, and never below one second for a clip that has
// a length. ok is false for a value that is not a positive number, such as "auto". A fraction
// that was dropped instead left the job with no duration, and it settled unpriced (TXE-7297).
func VideoSeconds(value string) (seconds int, ok bool) {
	value = strings.TrimSpace(value)
	if whole, err := strconv.Atoi(value); err == nil {
		if whole <= 0 {
			return 0, false
		}
		return whole, true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, false
	}
	return max(1, int(math.Round(parsed))), true
}

func VideoDimensionsFromResponse(resp *schemas.BifrostVideoGenerationResponse) VideoPricingDimensions {
	if resp == nil {
		return VideoPricingDimensions{}
	}
	dims := VideoPricingDimensions{
		Model:        resp.Model,
		Size:         resp.Size,
		SizeReported: resp.Size != "",
		OutputCount:  videoClipCount(resp.Videos),
	}
	if resp.Seconds != nil {
		if seconds, ok := VideoSeconds(*resp.Seconds); ok {
			dims.Seconds = &seconds
		}
	}
	if resp.Usage != nil && resp.Usage.Cost != nil && resp.Usage.Cost.TotalCost > 0 {
		total := resp.Usage.Cost.TotalCost
		dims.ProviderCost = &total
	}
	// Fork (TXE-7357): the output video tokens and the input video seconds the vendor billed.
	if resp.Usage != nil {
		if resp.Usage.OutputTokens != nil && *resp.Usage.OutputTokens > 0 {
			tokens := *resp.Usage.OutputTokens
			dims.OutputTokens = &tokens
		}
		if resp.Usage.InputSeconds != nil && *resp.Usage.InputSeconds > 0 {
			dims.VideoInput = schemas.Ptr(true)
		}
	}
	return dims
}
