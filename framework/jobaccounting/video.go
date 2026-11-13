package jobaccounting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/capsohq/bifrost/core/schemas"
	cstables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/logstore"
	"github.com/capsohq/bifrost/framework/modelcatalog"
	"github.com/capsohq/bifrost/framework/videoduration"
	"github.com/google/uuid"
)

const ProviderJobKindVideo ProviderJobKind = cstables.ProviderJobKindVideo

const (
	UnpriceableReasonMissingVideoPricing = "missing_video_pricing"
	// UnpriceableReasonVideoGone is a job the provider no longer knows about.
	// Generated assets expire, so this is an ordinary end state, not an error.
	UnpriceableReasonVideoGone = "video_gone"
	// UnpriceableReasonVideoAccessDenied is the creating key being refused. Distinct
	// from video_gone on purpose: one is an asset ageing out and needs no action, the
	// other means a key was revoked or rotated and every in-flight job on that
	// provider is quietly failing behind it.
	UnpriceableReasonVideoAccessDenied = "video_access_denied"
	// UnpriceableReasonVideoRequestRejected is the provider refusing the retrieve
	// itself — nothing expired, Bifrost built a request it will not accept.
	UnpriceableReasonVideoRequestRejected = "video_request_rejected"
	// UnpriceableReasonContentFiltered is deliberately not a price. A safety-filtered
	// job consumed real compute, but whether the provider bills for it is a
	// per-provider policy none of them document consistently. Guessing free
	// under-bills and guessing billable over-bills; parking it costs nothing, keeps
	// the job re-drivable, and the existing missing-cost backfill can settle it in
	// bulk once the policy is actually known.
	UnpriceableReasonContentFiltered = "content_filtered_policy_unknown"
)

// videoLogNamespace keys the aggregate cost row for a settled video. It is its own
// namespace so a video can never collide with a batch that happens to share a
// provider-side id, and it is fixed forever: it is both the aggregate write's
// idempotency key and the governance dedupe key.
var videoLogNamespace = uuid.MustParse("b1e0f7c4-2a63-4d18-9f5b-6c0a3d8e41d7")

func init() { RegisterLogNamespace(ProviderJobKindVideo, videoLogNamespace) }

// videoProviders is the set Bifrost can actually settle video for. Every provider
// implements the VideoGeneration method, so method presence proves nothing — this
// is the list with a real implementation behind it.
//
// Fork (TXE-7118): TestVideoSettlerCoversEveryProviderThatImplementsVideo builds every standard
// provider and fails when one that implements video is neither here nor in
// videoSettlementExemptions, and when an entry here does not implement video.
var videoProviders = map[schemas.ModelProvider]struct{}{
	schemas.OpenAI:    {},
	schemas.Gemini:    {},
	schemas.Vertex:    {},
	schemas.Replicate: {},
	schemas.Runway:    {},
	schemas.Runware:   {},
	schemas.MiniMax:   {},
	schemas.BytePlus:  {},
	schemas.Kling:     {},
	// Fork: Wan video on DashScope (TXE-6432). Without this entry a create is recorded and never
	// settled, so a budgeted virtual key is never charged for it.
	schemas.DashScope: {},
	// xAI's video retrieve reports the bill itself (usage.cost_in_usd_ticks), so a finished job
	// settles at the vendor's figure (fork, TXE-6429).
	schemas.XAI: {},
	// FLUX 3 Video on BFL (fork, TXE-6617). BFL reports its cost in credits (usage.vendor_cost),
	// never usage.cost, so a job is priced from the datasheet rate and the seconds the create
	// recorded; with no rate it is kept as an unpriced row and repriced when the rate lands.
	schemas.BFL: {},
}

// videoSettlementExemptions are the providers that implement video and are deliberately not
// settled, each with the reason. TestVideoSettlerCoversEveryProviderThatImplementsVideo fails
// for a provider that implements video and is in neither this map nor videoProviders
// (fork, TXE-7118).
var videoSettlementExemptions = map[schemas.ModelProvider]string{
	schemas.ModelArk:   "TXE-7131 adds modelark with a settle-and-read-cost regression: it serves Seedance tasks through the Ark API and has never been settled",
	schemas.Volcengine: "TXE-7131 adds volcengine with a settle-and-read-cost regression: it serves Seedance tasks through the Ark API and has never been settled",
	schemas.Azure:      "TXE-7054 adds azure: its two video APIs (Sora 2 and the first-generation jobs API) use different ids and dimensions, and settlement must also be reached from /content",
}

// renderedVideoHostPolicy is where a settled provider's rendered video may be fetched from to
// measure it (fork, TXE-7386). The settler downloads a finished job's video to read its length,
// and the download fetches whatever URL the job's response names. A provider whose output URL a
// caller can choose, such as a Replicate model the caller runs that answers with any URL, lists
// the vendor's own CDN hosts: an output anywhere else is never fetched, and the job is charged as
// an unmeasurable one, up to its bound (TXE-7401), marked incomplete. A provider whose output URL only the
// vendor generates says why, and its URLs are fetched wherever they point. The provider's
// configured base URL origin is trusted for every provider: the operator set it.
type renderedVideoHostPolicy struct {
	// hosts are the vendor CDN hostnames a rendered video may be fetched from, each also matching
	// its subdomains. Empty means any host, and vendorGenerated must then say why.
	hosts []string
	// vendorGenerated is why a caller cannot choose this provider's output URL.
	vendorGenerated string
}

// renderedVideoHosts holds a policy for every provider in videoProviders.
// TestEverySettledVideoProviderDeclaresWhereItsRenderedVideoMayBeFetched fails for a provider in
// videoProviders that has none, so a new provider cannot be settled without saying whether a
// caller can point its output at a host of their choosing.
var renderedVideoHosts = map[schemas.ModelProvider]renderedVideoHostPolicy{
	// A Replicate model is code anyone can publish, and its output can be any string: a model a
	// caller runs can answer with a URL on a host it controls. Files Replicate itself stores are
	// served from replicate.delivery.
	schemas.Replicate: {hosts: []string{"replicate.delivery"}},
	schemas.OpenAI:    {vendorGenerated: "the video is downloaded from OpenAI's own API (/v1/videos/{id}/content), never from a URL in the response"},
	schemas.Gemini:    {vendorGenerated: "Gemini names a file it stores (a Files API URI) or returns the video inline"},
	schemas.Vertex:    {vendorGenerated: "Vertex returns the video inline or names a Cloud Storage object, read through the Cloud Storage API with the key's credentials"},
	schemas.Runway:    {vendorGenerated: "Runway runs only its own models, and the output URL is one it stores"},
	schemas.Runware:   {vendorGenerated: "Runware runs only its own hosted models, and the output URL is one it stores"},
	schemas.MiniMax:   {vendorGenerated: "MiniMax answers with a download URL for a file it stores"},
	schemas.BytePlus:  {vendorGenerated: "BytePlus Seedance answers with a URL for a video it stores"},
	schemas.Kling:     {vendorGenerated: "Kling answers with a URL for a video it stores"},
	schemas.DashScope: {vendorGenerated: "DashScope answers with a URL for a video it stores"},
	schemas.XAI:       {vendorGenerated: "xAI answers with a URL for a video it stores, and reports the cost itself"},
	schemas.BFL:       {vendorGenerated: "BFL answers with a delivery URL for a video it stores"},
}

// ProviderOriginResolver is implemented by a VideoRetriever that can say which origin
// (scheme://host[:port]) a provider's configured base URL names, "" when none is configured. A
// rendered video on that origin is always fetched: the operator set it (fork, TXE-7386).
type ProviderOriginResolver interface {
	ProviderOrigin(provider schemas.ModelProvider) string
}

// untrustedRenderedVideoHost returns why the job's rendered video must not be fetched to measure
// it, or "" when it may be. Only the clip the download fetches, the first, is checked. A clip with
// no URL (inline, or downloaded from the vendor's own API) is fetched from no host the response
// names.
func (s *VideoSettler) untrustedRenderedVideoHost(job *cstables.TableProviderJob, resp *schemas.BifrostVideoGenerationResponse) string {
	if resp == nil || len(resp.Videos) == 0 || resp.Videos[0].URL == nil || *resp.Videos[0].URL == "" {
		return ""
	}
	provider := schemas.ModelProvider(job.Provider)
	policy, ok := renderedVideoHosts[provider]
	if !ok {
		if resolver, isResolver := s.retriever.(BaseProviderResolver); isResolver {
			policy, ok = renderedVideoHosts[resolver.BaseProvider(provider)]
		}
	}
	if !ok {
		return fmt.Sprintf("the rendered video is not fetched: %s declares no host its videos may be fetched from", provider)
	}
	if len(policy.hosts) == 0 {
		return ""
	}
	u, err := url.Parse(*resp.Videos[0].URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "the rendered video is not fetched: its URL is not an http(s) URL"
	}
	if resolver, isResolver := s.retriever.(ProviderOriginResolver); isResolver {
		if origin := resolver.ProviderOrigin(provider); origin != "" && sameOrigin(origin, u) {
			return ""
		}
	}
	host := strings.ToLower(u.Hostname())
	for _, allowed := range policy.hosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return ""
		}
	}
	return fmt.Sprintf("the rendered video is not fetched: %s is not a host %s serves its videos from", host, provider)
}

// sameOrigin reports whether u is on origin (scheme://host[:port]): the same scheme, host and
// port, with a scheme's default port (80, 443) filled in where either leaves it out, as the
// provider's download does (sameParsedOrigin in core/providers/utils). A different port is a
// different origin.
func sameOrigin(origin string, u *url.URL) bool {
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" || !strings.EqualFold(o.Scheme, u.Scheme) {
		return false
	}
	hostPort := func(v *url.URL) string {
		port := v.Port()
		if port == "" {
			switch strings.ToLower(v.Scheme) {
			case "http":
				port = "80"
			case "https":
				port = "443"
			}
		}
		return strings.ToLower(v.Hostname()) + ":" + port
	}
	return hostPort(o) == hostPort(u)
}

// unmeasuredVideoBoundSeconds is each settled provider's default bound, in seconds, for a rendered
// video the settler could not measure (fork, TXE-7401). Such a job is charged the larger of the
// seconds its create named and the bound, marked incomplete, so a caller who makes an output
// unreadable cannot pick a short request and pay for it. A pricing override's
// unmeasured_video_seconds replaces the default per model. The datasheet publishes no maximum
// length for any video model, so the default is the fork's. 30 seconds is above every listed
// vendor's single clip today. TestEverySettledVideoProviderDeclaresAnUnmeasuredBound fails for a
// provider in videoProviders with no entry here, or with one that is not positive.
var unmeasuredVideoBoundSeconds = map[schemas.ModelProvider]int{
	schemas.OpenAI:    30,
	schemas.Gemini:    30,
	schemas.Vertex:    30,
	schemas.Replicate: 30,
	schemas.Runway:    30,
	schemas.Runware:   30,
	schemas.MiniMax:   30,
	schemas.BytePlus:  30,
	schemas.Kling:     30,
	schemas.DashScope: 30,
	schemas.XAI:       30,
	schemas.BFL:       30,
}

// unmeasuredBound is the provider's default bound for an unmeasurable rendered video, a custom
// provider taking its base provider's.
func (s *VideoSettler) unmeasuredBound(provider schemas.ModelProvider) int {
	if bound, ok := unmeasuredVideoBoundSeconds[provider]; ok {
		return bound
	}
	if resolver, ok := s.retriever.(BaseProviderResolver); ok {
		return unmeasuredVideoBoundSeconds[resolver.BaseProvider(provider)]
	}
	return 0
}

// VideoRetriever is the provider call the video settler needs: given a job row,
// report where that job now stands.
type VideoRetriever interface {
	RetrieveVideo(ctx context.Context, job *cstables.TableProviderJob) (*schemas.BifrostVideoGenerationResponse, error)
}

// VideoDownloader is implemented by a VideoRetriever that can fetch a finished job's video file:
// the provider's own download of its first clip, streamed where the provider can stream it, so the
// settler holds the MP4's header and never its media (fork, TXE-7329, TXE-7334). The caller closes
// the reader. An error wrapping ErrVideoDurationUnmeasurable, or a provider call's
// (NewProviderCallError) that a retrieve would treat as final, is final; any other error is retried.
type VideoDownloader interface {
	DownloadVideo(ctx context.Context, job *cstables.TableProviderJob) (io.ReadCloser, error)
}

// maxRenderedVideoBytes and renderedVideoTimeout bound one measurement, media included: the media is
// read past, never held, but the sweeper settles one job at a time, so a measurement must end
// quickly whatever the file. A file past either is unmeasurable, final, and charged its floor; it is
// not retried, or one oversized output would hold the sweeper on every poll until the cap. 2 GiB is
// many times any generated clip and reachable in the time at tens of MB/s.
const maxRenderedVideoBytes int64 = 2 << 30

var renderedVideoTimeout = 2 * time.Minute

// ErrVideoDurationUnmeasurable marks a VideoDownloader error that no retry can change, such as a
// buffered file past the response body limit.
var ErrVideoDurationUnmeasurable = errors.New("rendered video duration is unmeasurable")

// VideoSettler prices provider video jobs. It is the Settler for
// ProviderJobKindVideo.
//
// retriever may be nil on the inline settlement path, where the caller already
// holds a terminal response and never polls.
type VideoSettler struct {
	retriever VideoRetriever
}

func NewVideoSettler(retriever VideoRetriever) *VideoSettler {
	return &VideoSettler{retriever: retriever}
}

// VideoPayload is the video kind's settlement input, carried opaquely by the engine
// from Poll (or an inline caller) into Settle.
type VideoPayload struct {
	Response *schemas.BifrostVideoGenerationResponse
	// RenderedSeconds is the length of the rendered video, measured from its file, for a response
	// that reports neither a duration nor a cost (RenderedDurationRequired). Settle prices such a
	// job on it, whatever the create asked for (fork, TXE-7329).
	RenderedSeconds *int
	// RenderedUnmeasurable is why the rendered length could not be measured. Settle then charges
	// the larger of the seconds the create named and the provider's bound (TXE-7401), and marks the
	// row incomplete: the job rendered a video, so it is never left uncharged, and a cost from an
	// unconfirmed length is known to be uncertain.
	RenderedUnmeasurable string
}

func (s *VideoSettler) Kind() ProviderJobKind { return ProviderJobKindVideo }

func (s *VideoSettler) SupportsProvider(provider schemas.ModelProvider) bool {
	if _, ok := videoProviders[provider]; ok {
		return true
	}
	// Fork: a custom provider records its jobs under its own name. Settle it when its base
	// provider is one Bifrost can settle video for; the job keeps the custom name for routing
	// and accounting identity.
	if resolver, ok := s.retriever.(BaseProviderResolver); ok {
		if base := resolver.BaseProvider(provider); base != "" && base != provider {
			_, ok := videoProviders[base]
			return ok
		}
	}
	return false
}

// baseProvider is the base provider of a custom provider, or "" when the name is not one or the
// retriever cannot say. Pricing reads it to know which converter rendered the job (TXE-7349).
func (s *VideoSettler) baseProvider(provider schemas.ModelProvider) schemas.ModelProvider {
	if resolver, ok := s.retriever.(BaseProviderResolver); ok {
		if base := resolver.BaseProvider(provider); base != provider {
			return base
		}
	}
	return ""
}

// BaseProviderResolver is implemented by a VideoRetriever that can name the base provider of a
// configured custom provider ("" when the name is not a custom provider). Fork addition.
type BaseProviderResolver interface {
	BaseProvider(provider schemas.ModelProvider) schemas.ModelProvider
}

// Backoff is minutes where batch's is hours. A video job typically finishes in one
// to five minutes, so the batch ladder would leave a finished job unsettled — and
// its assets unreadable — for a quarter of an hour after it was ready.
func (s *VideoSettler) Backoff(attempts int, interval time.Duration) time.Duration {
	delay := interval
	switch {
	case attempts >= 40:
		delay = maxDuration(delay, 5*time.Minute)
	case attempts >= 20:
		delay = maxDuration(delay, 2*time.Minute)
	}
	return delay
}

func (s *VideoSettler) Poll(ctx context.Context, job *cstables.TableProviderJob) (*PollResult, error) {
	if s.retriever == nil {
		return nil, fmt.Errorf("video retriever is nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultProviderPollTimeout)
	defer cancel()

	resp, err := s.retriever.RetrieveVideo(callCtx, job)
	if err != nil {
		if reason := videoUnreachableReason(err); reason != "" {
			return &PollResult{Terminal: true, UnpriceableReason: reason}, nil
		}
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("video retrieve returned nil response for job %s", job.ID)
	}
	if resp.ID != "" && resp.ID != job.JobID {
		return nil, fmt.Errorf("video retrieve returned mismatched video id for job %s", job.ID)
	}

	latest := videoJobFromResponse(job, resp)
	if !isTerminalVideoStatus(resp.Status) {
		return &PollResult{Job: latest}, nil
	}
	payload := VideoPayload{Response: resp}
	if RenderedDurationRequired(resp) {
		seconds, unmeasurable := s.measureRenderedVideo(ctx, job, resp)
		switch {
		case unmeasurable == nil:
			payload.RenderedSeconds = &seconds
		case errors.Is(unmeasurable, errRenderedDurationRetry) && job.PollAttempts+1 < MaxPollAttempts:
			return nil, unmeasurable
		default:
			payload.RenderedUnmeasurable = unmeasurable.Error()
		}
	}
	return &PollResult{
		Job:        latest,
		Terminal:   true,
		Settleable: true,
		Payload:    payload,
	}, nil
}

// RenderedDurationRequired reports whether a terminal response leaves the job's length to be
// measured from its file (fork, TXE-7329): the job finished with a video, and the vendor reported
// neither the video's duration nor a cost. The seconds the create named are then only what was
// asked for. A vendor's hosted model may define its own length input (Replicate's num_frames or
// length, sent through extra_params) and render a longer video than that, so the job is priced on
// the length of the video it rendered.
func RenderedDurationRequired(resp *schemas.BifrostVideoGenerationResponse) bool {
	if resp == nil || resp.Status != schemas.VideoStatusCompleted {
		return false
	}
	if resp.ContentFilter != nil && resp.ContentFilter.FilteredCount > 0 {
		return false
	}
	observed := modelcatalog.VideoDimensionsFromResponse(resp)
	return observed.Seconds == nil && observed.ProviderCost == nil
}

// errRenderedDurationRetry marks a measurement failure a later poll could get past (a timeout, a
// reset connection, a vendor 5xx). Poll retries it for as long as the sweeper's poll budget
// lasts, and on the last attempt settles the job at its floor instead of letting the cap park it
// uncharged. The budget is the job's whole one: PollAttempts also counts the polls that found it
// still running, so a smaller budget would be spent before the video was ever finished.
var errRenderedDurationRetry = errors.New("rendered video duration not measured yet")

// measureRenderedVideo downloads a finished job's video through the provider and reads its
// length from the MP4 header. It returns the length in whole seconds, or why it could not: an
// error wrapping errRenderedDurationRetry is worth another poll, any other is final. A response
// with several clips is measured on its first, the one the provider's download serves: every
// clip of one job comes from the same input.
func (s *VideoSettler) measureRenderedVideo(ctx context.Context, job *cstables.TableProviderJob, resp *schemas.BifrostVideoGenerationResponse) (int, error) {
	downloader, ok := s.retriever.(VideoDownloader)
	if !ok {
		return 0, errors.New("the settler cannot download the video")
	}
	if modelcatalog.VideoDimensionsFromResponse(resp).OutputCount == 0 {
		return 0, errors.New("the job returned no video clip")
	}
	// A video on a host the caller may have chosen is never fetched (TXE-7386): final, so the job
	// is charged its floor, marked incomplete.
	if reason := s.untrustedRenderedVideoHost(job, resp); reason != "" {
		return 0, errors.New(reason)
	}
	callCtx, cancel := context.WithTimeout(ctx, renderedVideoTimeout)
	defer cancel()
	// The measurement's own deadline running out is final; the sweeper's context ending (a
	// shutdown) is not.
	outOfTime := func() bool { return ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) }
	body, err := downloader.DownloadVideo(callCtx, job)
	if err != nil && outOfTime() {
		return 0, fmt.Errorf("download video: no answer within %v: %w", renderedVideoTimeout, err)
	}
	if err != nil {
		// A file past the size limit, or a download the vendor refuses (the output expired, the
		// key was revoked), is final; anything else may pass on a later poll.
		if errors.Is(err, ErrVideoDurationUnmeasurable) || videoUnreachableReason(err) != "" {
			return 0, fmt.Errorf("download video: %w", err)
		}
		return 0, fmt.Errorf("%w: download video for job %s: %w", errRenderedDurationRetry, job.ID, err)
	}
	defer body.Close()
	duration, err := videoduration.ReadStream(callCtx, body, videoduration.DefaultReadLimits, maxRenderedVideoBytes)
	if err != nil {
		if errors.Is(err, videoduration.ErrUnmeasurable) {
			return 0, err
		}
		if outOfTime() {
			return 0, fmt.Errorf("read video: not read within %v: %w", renderedVideoTimeout, err)
		}
		return 0, fmt.Errorf("%w: read video duration for job %s: %w", errRenderedDurationRetry, job.ID, err)
	}
	if duration.VideoMS <= 0 {
		return 0, errors.New("the video has no video track")
	}
	return RenderedSeconds(duration.VideoMS), nil
}

// RenderedSeconds is a measured length in milliseconds as the whole seconds a video rate is
// charged on: rounded to the nearest second, a half second up, and never below one second. A
// rendered clip runs a frame or two past or short of its nominal length (Wan's 81 frames at 16
// per second are 5.0625 s), and a rate card's seconds are that nominal length.
func RenderedSeconds(ms int64) int {
	seconds := (ms + 500) / 1000
	if seconds < 1 {
		return 1
	}
	return int(seconds)
}

// Settle prices one video job from its merged dimensions: what was ordered, as
// recorded on the job row at submission, overlaid with what the terminal response
// says actually happened.
//
// The merge is the whole design. Most providers' retrieve reports little beyond a
// status and a URL, so the request captured at submission is usually the only place
// the duration and resolution exist by the time this runs.
func (s *VideoSettler) Settle(ctx context.Context, pricing PricingManager, jobReq JobRequest) (*Settlement, error) {
	load, _ := jobReq.Payload.(VideoPayload)
	resp := load.Response

	captured := VideoDimensionsFromJob(jobReq.Job)
	dims := captured.MergedWith(modelcatalog.VideoDimensionsFromResponse(resp))
	if dims.Model == "" && jobReq.FallbackModel != "" {
		dims.Model = jobReq.FallbackModel
	}
	// Fork (TXE-7329): a job whose vendor reported neither a duration nor a cost is priced on the
	// length of the video it rendered, which Poll measures. When it could not be measured, the
	// larger of the seconds the create named and the provider's bound is charged (TXE-7401) and
	// the row is marked incomplete. A payload
	// that never tried settles nothing, so the job is retried.
	unmeasured := false
	if RenderedDurationRequired(resp) {
		switch {
		case load.RenderedSeconds != nil:
			// The measured length raises the price and never lowers it below the seconds the
			// create named. The file is read after the job finished, and some outputs live where
			// the caller can write (a Veo storageURI in the caller's bucket): a caller who swaps in
			// a shorter MP4 before the sweeper reads it must not shrink the bill.
			seconds := *load.RenderedSeconds
			if dims.Seconds == nil || seconds > *dims.Seconds {
				dims.Seconds = &seconds
			}
		case load.RenderedUnmeasurable != "":
			unmeasured = true
			// Charged up to a bound the caller cannot choose (TXE-7401).
			dims.UnmeasuredDefaultSeconds = s.unmeasuredBound(jobReq.Provider)
		default:
			return nil, fmt.Errorf("video job %s finished without a reported duration or cost, and its rendered duration was not measured", jobReq.ProviderJobID)
		}
	}

	settlement := &Settlement{
		Model:  dims.Model,
		Object: videoObjectFor(dims.RequestType),
	}

	// Attached on every path below, including the unpriceable ones: a row with no
	// cost is exactly the row an operator needs the detail on.
	videoStatus := schemas.VideoStatus("")
	if resp != nil {
		videoStatus = resp.Status
	}
	// The base provider names the converter that rendered a custom provider's job (TXE-7349).
	dims.BaseProvider = s.baseProvider(jobReq.Provider)
	settlement.ApplyDebug = videoDebugFor(jobReq.ProviderJobID, videoStatus, dims, jobReq.Provider, dims.TierOnlyResolution(jobReq.Provider), false)

	if resp != nil && resp.ContentFilter != nil && resp.ContentFilter.FilteredCount > 0 {
		settlement.UnpriceableReason = UnpriceableReasonContentFiltered
		return settlement, nil
	}

	// Fork (TXE-6432): a vendor that can no longer say what became of the job has not said the
	// job produced nothing. It is the 200-status form of a retrieve that finds no job, so it
	// parks as video_gone like one, not as a final price of zero.
	if resp != nil && videoRecordGone(jobReq.Provider, resp) {
		settlement.UnpriceableReason = UnpriceableReasonVideoGone
		return settlement, nil
	}

	// A failed generation is priced, at zero. Providers are explicit that an
	// unsuccessful video is not billed, and that is a final answer about the money —
	// not an absence of one. Recording it as unpriceable instead would leave free
	// work parked in the sweeper forever. A cancelled or expired job produced no video
	// either and settles the same way.
	if resp != nil && isUnbilledTerminalVideoStatus(resp.Status) {
		settlement.Priced = true
		settlement.Complete = true
		return settlement, nil
	}

	details := pricing.CalculateVideoCostDetails(dims, jobReq.Provider, jobReq.Scopes)
	if !details.Priced {
		settlement.UnpriceableReason = UnpriceableReasonMissingVideoPricing
		// Keep the row so the usage stays visible and the backfill can reprice it
		// once the rate lands, rather than dropping the job's cost entirely.
		settlement.RecordUnpriced = true
		settlement.UnpricedModel = dims.Model
		settlement.ApplyDebug = withUnmeasuredBound(videoDebugFor(jobReq.ProviderJobID, videoStatus, dims, jobReq.Provider, dims.TierOnlyResolution(jobReq.Provider), true), dims.Seconds, dims.UnmeasuredDefaultSeconds)
		return settlement, nil
	}

	settlement.Priced = true
	settlement.Complete = true
	settlement.Cost = details.Cost
	settlement.ApplyDebug = withUnmeasuredBound(videoDebugFor(jobReq.ProviderJobID, videoStatus, dims, jobReq.Provider, details.PricedResolution, unmeasured), dims.Seconds, details.UnmeasuredBoundSeconds)
	return settlement, nil
}

// withUnmeasuredBound records on the settlement row the bound an unmeasurable video was charged up
// to, the seconds that charge was for, and the seconds the create named (fork, TXE-7401), so the
// row says why it costs more than was asked for and a reprice can price it again from the request.
func withUnmeasuredBound(apply func(*logstore.Log), requested *int, bound int) func(*logstore.Log) {
	if bound <= 0 {
		return apply
	}
	return func(entry *logstore.Log) {
		apply(entry)
		accounting := entry.VideoDebugParsed.Accounting
		b := bound
		accounting.UnmeasuredBoundSeconds = &b
		accounting.RequestedSeconds = requested
		if requested == nil || *requested < bound {
			charged := bound
			accounting.Seconds = &charged
		}
	}
}

// videoDebugFor builds the closure that attaches video detail to the aggregate row.
// Accounting being set is what marks this row as the settlement rather than the
// submission it settles — the two are otherwise identical but for a cost.
func videoDebugFor(videoID string, status schemas.VideoStatus, dims modelcatalog.VideoPricingDimensions, provider schemas.ModelProvider, resolution string, incomplete bool) func(*logstore.Log) {
	return func(entry *logstore.Log) {
		entry.VideoDebugParsed = &schemas.BifrostVideoDebug{
			VideoID: videoID,
			Status:  status,
			Accounting: &schemas.VideoAccountingDebug{
				Seconds:      dims.Seconds,
				Size:         dims.Size,
				Audio:        dims.Audio,
				Resolution:   resolution,
				Mode:         dims.PricedMode(),
				VideoInput:   dims.VideoInput,
				OutputTokens: dims.OutputTokens,
				OutputCount:  dims.OutputCount,
				Incomplete:   incomplete,
				RequestType:  videoRequestTypeFor(dims.RequestType),
				// Decided with a custom provider's base in hand, which a reprice lacks (TXE-7351).
				InputVideoRateExempt: dims.InputVideoRateExemptFor(provider),
				ProviderCost:         dims.ProviderCost,
			},
		}
	}
}

// videoRequestTypeFor is what videoObjectFor used to return: the creating request
// type, normalized so an unset or unexpected value still prices as a generation.
func videoRequestTypeFor(requestType schemas.RequestType) schemas.RequestType {
	switch requestType {
	case schemas.VideoRemixRequest, schemas.VideoEditRequest:
		return requestType
	default:
		return schemas.VideoGenerationRequest
	}
}

// RepriceFromLog recomputes a settled video's cost from the dimensions it was
// priced with, which live in video_debug rather than in token_usage: a video's
// price comes from seconds, resolution and clip count, none of which are tokens.
func (s *VideoSettler) RepriceFromLog(entry *logstore.Log, pricing PricingManager) (*RepricedCost, error) {
	if entry.VideoDebugParsed == nil && entry.VideoDebug != "" {
		if err := entry.DeserializeFields(); err != nil {
			return nil, nil
		}
	}
	// Accounting being set is what marks the settlement. The submission row carries
	// video_debug too, and Object cannot decide it either — every poll is also a
	// video_retrieve.
	if entry.VideoDebugParsed == nil || entry.VideoDebugParsed.Accounting == nil {
		return nil, nil
	}
	accounting := entry.VideoDebugParsed.Accounting

	// A provider that reported its own cost is right about its own bill. Without
	// this a full reprice would quietly replace an exact figure with an estimate.
	// The calculator decides, so a provider configured to ignore its cost reprices.
	if accounting.ProviderCost != nil && *accounting.ProviderCost > 0 {
		details := pricing.CalculateVideoCostDetails(modelcatalog.VideoPricingDimensions{
			Model:        entry.Model,
			ProviderCost: accounting.ProviderCost,
		}, schemas.ModelProvider(entry.Provider), nil)
		if details.ProviderCostUsed {
			return &RepricedCost{Cost: details.Cost}, nil
		}
	}

	// A failed generation settled at a real price of zero, not an absent one.
	// Repricing must not resurrect it as unpriceable for every backfill to retry.
	if isUnbilledTerminalVideoStatus(entry.VideoDebugParsed.Status) {
		return &RepricedCost{}, nil
	}

	requestType := accounting.RequestType
	if requestType == "" {
		// Rows written before RequestType was recorded kept the creating type in
		// Object. Reading it back keeps them repriceable at the rates they used.
		requestType = schemas.RequestType(entry.Object)
		if requestType == schemas.VideoRetrieveRequest {
			requestType = schemas.VideoGenerationRequest
		}
	}

	// An unmeasurable video was charged up to a bound (TXE-7401): it reprices from the seconds
	// asked for and that bound, so an override set since replaces it.
	seconds, unmeasuredDefault := accounting.Seconds, 0
	if accounting.UnmeasuredBoundSeconds != nil {
		seconds, unmeasuredDefault = accounting.RequestedSeconds, *accounting.UnmeasuredBoundSeconds
	}
	scopes := PricingScopesForLog(entry)
	details := pricing.CalculateVideoCostDetails(modelcatalog.VideoPricingDimensions{
		Model:                    entry.Model,
		RequestType:              requestType,
		Seconds:                  seconds,
		UnmeasuredDefaultSeconds: unmeasuredDefault,
		// A recorded tier set the band at settlement, so the size is left out and the tier
		// alone prices the reprice; without one the size alone does (TXE-7349).
		Size: repriceSize(accounting),
		// The settlement's audio flag (TXE-7306): without it an explicit audio false
		// would reprice at the provider's default, the with-audio rate.
		Audio:      accounting.Audio,
		Resolution: accounting.Resolution,
		// The recorded tier is the one settlement chose, with the request's sources and a custom
		// provider's base in hand; a reprice has neither, so it takes the tier as recorded rather
		// than judging it again (TXE-7349).
		ResolutionSettled: accounting.Resolution != "",
		// Mode, video input and output tokens choose the rate too (TXE-7357).
		Mode:         accounting.Mode,
		VideoInput:   accounting.VideoInput,
		OutputTokens: accounting.OutputTokens,
		OutputCount:  accounting.OutputCount,
		// The settlement's exemption from the input-video rate, decided by the base provider of a
		// custom one, which a reprice cannot resolve (TXE-7351).
		InputVideoRateExempt: accounting.InputVideoRateExempt,
	}, schemas.ModelProvider(entry.Provider), &scopes)
	if !details.Priced {
		return nil, fmt.Errorf("%w: no video rate for log %s", ErrPricingInputsUnavailable, entry.ID)
	}

	// Incomplete is left as settlement wrote it: it can stand for dimensions the
	// provider never confirmed, which repricing cannot see and must not claim closed.
	debugJSON, err := sonic.Marshal(entry.VideoDebugParsed)
	if err != nil {
		return nil, fmt.Errorf("serialize video_debug for log %s: %w", entry.ID, err)
	}
	return &RepricedCost{
		Cost:        details.Cost,
		DebugJSON:   string(debugJSON),
		DebugColumn: "video_debug",
	}, nil
}

// repriceSize is the size a reprice prices from: none when the settlement recorded the tier
// that set the band, else the recorded size.
func repriceSize(accounting *schemas.VideoAccountingDebug) string {
	if accounting.Resolution != "" {
		return ""
	}
	return accounting.Size
}

// HydrateFromLog reads this kind's detail back off an already-written aggregate row,
// for a caller that lost the settlement claim and only wants to display a price.
// The inverse of the closure videoDebugFor builds.
func (s *VideoSettler) HydrateFromLog(entry *logstore.Log, out *Outcome) {
	if entry.VideoDebugParsed == nil {
		return
	}
	out.Status = string(entry.VideoDebugParsed.Status)
}

// VideoDimensionsFromJob reads the pricing dimensions captured on the job row at
// submission. An unreadable blob yields empty dimensions rather than an error: the
// response may still carry enough to price the job, and refusing to settle over a
// malformed params column would strand real money.
func VideoDimensionsFromJob(job *cstables.TableProviderJob) modelcatalog.VideoPricingDimensions {
	if job == nil || job.Params == nil || *job.Params == "" {
		return modelcatalog.VideoPricingDimensions{}
	}
	var dims modelcatalog.VideoPricingDimensions
	if err := sonic.Unmarshal([]byte(*job.Params), &dims); err != nil {
		return modelcatalog.VideoPricingDimensions{}
	}
	return dims
}

// MarshalVideoDimensions renders dimensions for the job row's params column.
func MarshalVideoDimensions(dims modelcatalog.VideoPricingDimensions) (*string, error) {
	data, err := sonic.Marshal(dims)
	if err != nil {
		return nil, err
	}
	encoded := string(data)
	return &encoded, nil
}

// AccountVideoJob settles a video from a terminal response the caller already holds.
// It is the retrieve path's inline settlement — the counterpart to
// AccountBatchResults on /v1/batches/{id}/results.
//
// It exists to avoid throwing away work: a user polling GET /v1/videos/{id} has
// already fetched the terminal response, cost included for providers that report
// one, and waiting for the sweeper to fetch it again costs both a provider call and
// up to a full sweep interval of delay.
//
// Two things separate it from the batch entry point:
//
// It never creates the coordination row. A video submitted before this build was
// charged at submission, so settling one whose row we never wrote would bill it a
// second time. No row means we did not see the submission — leave it alone.
//
// It does not force the claim. A caller holding batch results has something the
// parked job lacked and may re-drive it; a video retrieve carries no more than the
// settlement that parked the job already saw, and clients poll it repeatedly, so
// re-driving on every poll would be churn. An already-settled or parked job takes
// the engine's mirror path and reports the price it already has.
func AccountVideoJob(ctx context.Context, stateStore JobStore, logStore AggregateLogStore, pricing PricingManager, req JobRequest) (*Outcome, error) {
	if stateStore == nil {
		return nil, fmt.Errorf("video accounting state store is nil")
	}
	if req.Provider == "" || req.ProviderJobID == "" {
		return nil, nil
	}

	load, _ := req.Payload.(VideoPayload)
	if load.Response == nil || !isTerminalVideoStatus(load.Response.Status) {
		// Still running: nothing to settle, and the sweeper is already scheduled.
		return nil, nil
	}
	if RenderedDurationRequired(load.Response) && load.RenderedSeconds == nil {
		// Fork (TXE-7329): the job's price waits on its rendered duration, which only the sweeper
		// measures: the caller's retrieve is not held up by a download of the video, and the job
		// stays due until the sweeper settles it.
		return nil, nil
	}

	job, err := stateStore.GetProviderJob(ctx, cstables.ProviderJobID(cstables.ProviderJobKindVideo, string(req.Provider), req.ProviderJobID))
	if err != nil || job == nil {
		return nil, nil
	}
	req.Job = job
	return AccountJob(ctx, stateStore, logStore, pricing, NewVideoSettler(nil), req)
}

// NewVideoSweeper binds the generic sweeper to the video settler.
func NewVideoSweeper(store SweepStore, logStore AggregateLogStore, pricing PricingManager, retriever VideoRetriever, emitter AggregateLogEmitter, usageReporter UsageReporter, config SweeperConfig) *Sweeper {
	if config.ClaimedBy == "" {
		config.ClaimedBy = "video-sweeper:" + newInstanceID()
	}
	return NewSweeper(store, logStore, pricing, NewVideoSettler(retriever), emitter, usageReporter, config)
}

func videoJobFromResponse(existing *cstables.TableProviderJob, resp *schemas.BifrostVideoGenerationResponse) *cstables.TableProviderJob {
	job := *existing
	job.ProviderStatus = string(resp.Status)
	if resp.Model != "" {
		job.Model = resp.Model
	}
	return &job
}

// videoUnreachableReason names why a failed retrieve is final, or returns empty
// when the call is worth retrying.
func videoUnreachableReason(err error) string {
	switch ClassifyProviderCall(err) {
	case ProviderCallGone:
		return UnpriceableReasonVideoGone
	case ProviderCallAccessDenied:
		return UnpriceableReasonVideoAccessDenied
	case ProviderCallRejected:
		return UnpriceableReasonVideoRequestRejected
	default:
		return ""
	}
}

func isTerminalVideoStatus(status schemas.VideoStatus) bool {
	return status.IsTerminal()
}

// dashScopeTaskUnknown is DashScope's task state for a task older than its 24-hour record and
// for a task id the queried key does not know. The task object reports it as expired.
const dashScopeTaskUnknown = "UNKNOWN"

// videoRecordGone reports whether the vendor answered that it has no record of the job, as
// opposed to reporting how the job ended. DashScope answers a task query that way with a 200:
// the task may have finished and been billed before its record aged out, or the key that asked
// may not be the one that created it, so the answer is no evidence that the video was free.
func videoRecordGone(provider schemas.ModelProvider, resp *schemas.BifrostVideoGenerationResponse) bool {
	return provider == schemas.DashScope && resp.Status == schemas.VideoStatusExpired && resp.ProviderStatus == dashScopeTaskUnknown
}

// isUnbilledTerminalVideoStatus reports whether a job ended without producing a video: failed,
// or (fork, TXE-6521) cancelled or expired. Such a job settles at a real price of zero.
func isUnbilledTerminalVideoStatus(status schemas.VideoStatus) bool {
	return status.IsTerminal() && status != schemas.VideoStatusCompleted
}

// videoObjectFor labels the aggregate row as the settlement it is, mirroring the
// batch_results row a settled batch writes: the submission keeps the request type
// that created it, and the row carrying the bill is named for the read that
// produced it. Two rows sharing video_generation read as two generations.
//
// Object therefore no longer names the rates this was priced at, so the creating
// request type is recorded in VideoAccountingDebug.RequestType for repricing —
// the same move batch makes with BatchAccountingDebug.Endpoint.
func videoObjectFor(schemas.RequestType) schemas.RequestType {
	return schemas.VideoRetrieveRequest
}
