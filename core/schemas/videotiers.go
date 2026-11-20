package schemas

// VideoTierReader says where one provider's video create converter reads the resolution tier
// from (fork, TXE-7349, TXE-7101). Typed is the unified field (VideoGenerationParameters.Resolution);
// Extra is extra_params.resolution. The HTTP adapters fill both; a Go SDK caller may set one.
//
// Video settlement reads the rate band from a requested tier only when the provider that
// rendered the job reads it from where it was named (framework/modelcatalog/datasheet bandSize).
// A tier the provider never reads cannot set the band: the job is priced at the dearer of that
// tier and what it would cost without it. So the table has to match the converters both ways:
//   - a provider left out that does render the tier is priced as if it might not, so a job sent
//     with a resolution and no size can be charged the unbanded rate instead of its tier's;
//   - a provider listed that no longer reads the tier is priced at the tier the caller named,
//     whatever size it actually rendered.
//
// core's TestVideoTierReadersMatchConverters sends a video create with a tier through every
// standard provider's converter and fails when what lands in the body disagrees with this table.
type VideoTierReader struct {
	Typed, Extra bool
	// Tiers, when set, are the tiers the provider documents, keyed by the lower-case spelling a
	// caller may send, each to the tier label its rate band is read from. A tier outside it is
	// not trusted to be what the provider rendered, so it never lowers the price. Set it for a
	// provider whose converter forwards the tier string unchecked, or accepts its own spellings.
	// Nil means any tier the rate bands know.
	Tiers map[string]string
	// Source names the code that reads the tier.
	Source string
}

var videoTierReaders = map[ModelProvider]VideoTierReader{
	Gemini:    {Typed: true, Extra: true, Source: "gemini.ToGeminiVideoGenerationRequest: parameters.resolution from extra_params.resolution, else the typed field; size sets only aspectRatio"},
	Vertex:    {Typed: true, Extra: true, Source: "vertex sends Veo through gemini.ToGeminiVideoGenerationRequest"},
	MiniMax:   {Typed: true, Source: "minimax.ToMiniMaxVideoGenerationRequest: resolution from the typed field, upper-cased; extra_params.resolution is not read"},
	DashScope: {Typed: true, Source: "dashscope video create: parameters.resolution from the typed field (Wan 2.7+, and image-to-video before it)"},
	Kling:     {Typed: true, Source: "kling video create: settings.resolution from the typed field"},
	BytePlus:  {Typed: true, Extra: true, Source: "byteplus.toBytePlusVideoGenerationRequest: the typed field overrides the tier a size implies; extra_params.resolution when the typed field is empty"},
	// VolcEngine and ModelArk run BytePlus's Ark video converter (TXE-7493).
	Volcengine: {Typed: true, Extra: true, Source: "volcengine runs byteplus.toBytePlusVideoGenerationRequest"},
	ModelArk:   {Typed: true, Extra: true, Source: "modelark runs byteplus.toBytePlusVideoGenerationRequest"},
	// xAI's converter sends the tier string as given; only the tiers xAI documents
	// (xai.xaiVideoResolutions) are trusted.
	XAI: {Typed: true, Extra: true, Tiers: map[string]string{"480p": "480p", "720p": "720p", "1080p": "1080p"},
		Source: "xai.ToXAIVideoGenerationRequest: resolution from the typed field; extra_params.resolution is merged into the body when the typed field is empty"},
	// BFL's converter refuses any other tier (bfl.videoResolutions); its own names are aliases.
	BFL: {Typed: true, Tiers: map[string]string{
		"720p": "720p", "hd": "720p", "1080p": "1080p", "fhd": "1080p",
		"1440p": "1440p", "qhd": "1440p", "2160p": "2160p", "4k": "2160p", "uhd": "2160p",
	}, Source: "bfl.toBFLVideoBody: resolution from the typed field, as BFL's hd/fhd/qhd/uhd"},
}

// VideoTierReaderFor returns where provider's video converter reads the resolution tier from.
// The zero value means it reads it from neither.
func VideoTierReaderFor(provider ModelProvider) VideoTierReader {
	return videoTierReaders[provider]
}

// VideoTierReaders returns a copy of the whole table, for the test that checks it against the
// converters.
func VideoTierReaders() map[ModelProvider]VideoTierReader {
	out := make(map[ModelProvider]VideoTierReader, len(videoTierReaders))
	for provider, reader := range videoTierReaders {
		if reader.Tiers != nil {
			tiers := make(map[string]string, len(reader.Tiers))
			for spelling, tier := range reader.Tiers {
				tiers[spelling] = tier
			}
			reader.Tiers = tiers
		}
		out[provider] = reader
	}
	return out
}
