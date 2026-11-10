package elevenlabs

import (
	"math"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// characterCostHeader is the response header in which ElevenLabs reports the credits it
// charged for a text-to-speech or sound-generation request.
const characterCostHeader = "character-cost"

// characterCostFromHeaders returns the credits ElevenLabs reported in the character-cost
// response header as a vendor cost in credits, or nil when the header is absent or is not a
// finite, non-negative number. It is the vendor's figure only: nothing is computed, and a
// missing header leaves the usage without a vendor_cost (TXE-6524, contract C5).
func characterCostFromHeaders(resp *fasthttp.Response) *schemas.VendorCost {
	if resp == nil {
		return nil
	}
	raw := strings.TrimSpace(string(resp.Header.Peek(characterCostHeader)))
	if raw == "" {
		return nil
	}
	amount, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return nil
	}
	return &schemas.VendorCost{Amount: amount, Unit: schemas.VendorCostUnitCredit}
}

// speechUsageFromVendorCost wraps a vendor cost in a speech usage, or returns nil when there
// is none, so a response the vendor reported nothing for carries no usage of the provider's
// making.
func speechUsageFromVendorCost(cost *schemas.VendorCost) *schemas.SpeechUsage {
	if cost == nil {
		return nil
	}
	return &schemas.SpeechUsage{VendorCost: cost}
}

var (
	// Maps provider-specific finish reasons to Bifrost format
	bifrostToElevenlabsSpeechFormat = map[string]string{
		"":     "mp3_44100_128",
		"mp3":  "mp3_44100_128",
		"opus": "opus_48000_128",
		"wav":  "pcm_44100",
		"pcm":  "pcm_44100",
	}

	// Maps Bifrost finish reasons to provider-specific format
	elevenlabsSpeechFormatToBifrost = map[string]string{
		"mp3_44100_128":  "mp3",
		"opus_48000_128": "opus",
		"pcm_44100":      "wav",
	}
)

// ConvertBifrostSpeechFormatToElevenlabs converts Bifrost speech format to Elevenlabs format
func ConvertBifrostSpeechFormatToElevenlabs(format string) string {
	if elevenlabsFormat, ok := bifrostToElevenlabsSpeechFormat[format]; ok {
		return elevenlabsFormat
	}
	return format
}

// ConvertElevenlabsSpeechFormatToBifrost converts Elevenlabs speech format to Bifrost format
func ConvertElevenlabsSpeechFormatToBifrost(format string) string {
	if bifrostFormat, ok := elevenlabsSpeechFormatToBifrost[format]; ok {
		return bifrostFormat
	}
	return format
}
