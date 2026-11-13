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
	breakdown := computeVideoCost(pricing, nil, dims.Seconds, dims.bandSize(provider, pricing, audio), dims.OutputCount, audio, serviceTier{})
	if breakdown == nil || breakdown.TotalCost <= 0 {
		return VideoCostDetails{}
	}
	return VideoCostDetails{Cost: breakdown.TotalCost, Priced: true, Breakdown: breakdown}
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

// videoProvidersRenderingRequestedTier are the providers whose create converter renders the
// requested resolution tier even when the request also names a size, with the code that does
// it. For them the tier is the rendered resolution. A provider is added here only from its
// converter: a caller-supplied tier that the provider ignores must not choose a cheaper band.
var videoProvidersRenderingRequestedTier = map[schemas.ModelProvider]string{
	schemas.Gemini:  "gemini.ToGeminiVideoGenerationRequest reads size only for aspectRatio and sends resolution as parameters.resolution",
	schemas.Vertex:  "vertex sends Veo through gemini.ToGeminiVideoGenerationRequest",
	schemas.MiniMax: "minimax video create: a requested resolution replaces the tier derived from size",
}

// bandSize returns what the rate band is read from (TXE-7349):
//   - a size the provider reported, which is what it rendered;
//   - else, when the request named only one of a tier and a size, that one;
//   - else, when it named both, the tier for a provider that renders the requested tier
//     (Veo with size 3840x2160 and resolution 1080p renders 1080p), and otherwise whichever
//     of the two bands costs more. A provider that ignores the tier may render the size, so
//     a caller cannot lower the price by adding a cheaper tier the provider never reads.
func (d VideoPricingDimensions) bandSize(provider schemas.ModelProvider, pricing *configstoreTables.TableModelPricing, audio bool) string {
	sizeBand := videoResolutionBand(d.Size) > 0
	if d.SizeReported && sizeBand {
		return d.Size
	}
	tier := d.BandResolution()
	if tier == "" {
		return d.Size
	}
	if !sizeBand {
		return tier
	}
	if _, ok := videoProvidersRenderingRequestedTier[provider]; ok {
		return tier
	}
	if videoOutputPerSecondRate(pricing, tier, audio) > videoOutputPerSecondRate(pricing, d.Size, audio) {
		return tier
	}
	return d.Size
}

// PricedResolution returns the tier the settlement records for a reprice: "" when a reported
// size set the band, else the requested tier. A reprice has no SizeReported, so recording no
// tier leaves the size to decide; a recorded tier reruns the same choice bandSize made.
func (d VideoPricingDimensions) PricedResolution() string {
	if d.SizeReported && videoResolutionBand(d.Size) > 0 {
		return ""
	}
	return d.BandResolution()
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
	return dims
}
