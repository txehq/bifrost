package schemas

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Typed payload for LLM-call spans, replacing the map[string]any attribute bag on
// the hot path. Producers fill fields; connectors read fields and render their own
// wire format. Nothing here is pre-serialized — a connector that needs JSON
// marshals at its own edge, so the cost is only paid when that connector is loaded.
//
// Wire-key namespacing (prompt_token_details.* for chat vs input_token_details.*
// for Responses) is a rendering concern and lives in each connector's mapper, not
// here. RequestType is what a mapper switches on.
//
// The per-family params are the request's own *Parameters structs, held by
// pointer rather than projected into span-local copies. Anything the old
// attribute map derived from them (joined stop sequences, joined prompt arrays,
// formatted logit bias, joined multi-voice names) is a rendering concern too.

// LLMSpanData is the typed payload of an LLM-call span.
type LLMSpanData struct {
	// Identity
	Provider      ModelProvider
	RequestType   RequestType
	RequestModel  string
	ResponseModel string
	Alias         string // originally requested model when a fallback or alias changed it

	// Response envelope
	ResponseID        string
	Object            string
	SystemFingerprint string
	ServiceTier       string
	Created           int64
	FinishReasons     []string

	// Content. Typed, never pre-marshalled.
	MessageCount   int
	InputMessages  []MessageSummary
	OutputMessages []MessageSummary
	ReasoningText  string

	// Normalized across every request family via BifrostResponse.NormalizedUsage,
	// so the tracer and the logging plugin can never disagree on token counts.
	Usage *BifrostLLMUsage

	// Bifrost's computed cost, from CalculateCostBreakdown — the same call the
	// logging plugin makes, so a span and its log row cannot disagree. Distinct
	// from Usage.Cost, which is provider-reported and nil for most providers;
	// this supersedes it and falls back to it when the catalog cannot price the
	// model, so it may alias Usage.Cost. Provenance is load-bearing: pricing
	// itself branches on whether the provider reported a cost.
	Cost *BifrostCost

	// Raw provider bodies, JSON-encoded and capped. Content: stripped with messages.
	RawRequest  string
	RawResponse string

	// Streaming
	TimeToFirstChunkMs *float64
	TotalChunks        int

	Error *SpanError

	// Exactly one non-nil, mirroring BifrostRequest's discriminated union. These
	// alias the request's own params; treat them as read-only.
	//
	// Batch and file spans are not modelled here — they are cold-path and their
	// attributes stay in Extra until there is a reason to type them.
	Chat           *ChatParameters
	Responses      *ResponsesParameters
	TextCompletion *TextCompletionParameters
	Embedding      *EmbeddingParameters
	Speech         *SpeechParameters
	Transcription  *TranscriptionParameters

	// Params the Responses API echoes back on the response. Distinct from
	// Responses above so a mapper can emit gen_ai.response.* for these and
	// gen_ai.request.* for the request side.
	ResponsesEcho *ResponsesParameters

	// Request inputs that live on the request rather than its params.
	TextPrompt     *TextCompletionInput
	EmbeddingInput EmbeddingInput
	SpeechInput    *SpeechInput

	// ExtraParams, x-bf-dim-* dimensions, and cold-path families. Anything with
	// an Attr* constant belongs in a field, not here.
	Extra map[string]any
}

// SpanError is the error detail stamped on a failed LLM span. It reuses
// ErrorField rather than re-flattening message/type/code, and deliberately does
// not hold the whole BifrostError — ExtraFields there carries raw request and
// response payloads that must not be retained on a span by accident.
type SpanError struct {
	Detail     *ErrorField
	StatusCode *int
	Type       string // classified type from ClassifyErrorType
}

// SpanEnrichment carries the governance and identity dimensions. It hangs off
// Span rather than LLMSpanData: these are context-sourced and apply to any span,
// not just LLM calls. Field order mirrors EnrichmentDims so the conformance test
// can pin the two together.
//
// This overlaps routing/rules.GovernanceScope (a 7-field subset) and the fields
// logging carries on its queue message; it is the superset both should adopt.
type SpanEnrichment struct {
	VirtualKeyID     string
	VirtualKeyName   string
	SelectedKeyID    string
	SelectedKeyName  string
	RoutingRuleID    string
	RoutingRuleName  string
	TeamID           string
	TeamName         string
	CustomerID       string
	CustomerName     string
	BusinessUnitID   string
	BusinessUnitName string
	ProjectID        string
	ProjectName      string
	PromptID         string
	PromptName       string
	PromptVersion    string
	UserID           string
	UserName         string
	UserEmail        string
	App              string

	TeamIDs           []string
	TeamNames         []string
	CustomerIDs       []string
	CustomerNames     []string
	BusinessUnitIDs   []string
	BusinessUnitNames []string

	RoutingEnginesUsed  []string
	ComplexityTier      string
	ComplexityMechanism string
	ComplexityScore     *float64

	Retries int
	// Nil when the request never carried a fallback index.
	FallbackIndex *int
}

// ---------------------------------------------------------------------------
// Message summaries
//
// Moved here from framework/tracing so the span payload can be typed in
// core/schemas. framework/tracing keeps aliases, so existing importers (the
// Datadog connector among them) compile unchanged.
//
// This is the one genuinely new normalization in this file: it unifies
// ChatMessage and ResponsesMessage into a single shape connectors can render
// without switching on request family. Unlike usage — where BifrostLLMUsage
// already is the canonical normalized type — no such type exists for messages.
// ---------------------------------------------------------------------------

// MessageSummary is a summarized chat or responses message carried on a span.
type MessageSummary struct {
	Role             string                   `json:"role"`
	Content          string                   `json:"content"`
	Attachments      []AttachmentSummary      `json:"attachments,omitempty"`
	ToolCalls        []ToolCallSummary        `json:"tool_calls,omitempty"`
	Reasoning        string                   `json:"reasoning,omitempty"`
	ReasoningDetails []ReasoningDetailSummary `json:"reasoning_details,omitempty"`
	Audio            *AudioSummary            `json:"audio,omitempty"`
	Refusal          string                   `json:"refusal,omitempty"`
	ToolCallID       string                   `json:"tool_call_id,omitempty"`
}

// AttachmentKind classifies a non-text content block.
type AttachmentKind string

const (
	AttachmentImage AttachmentKind = "image"
	AttachmentAudio AttachmentKind = "audio"
	AttachmentFile  AttachmentKind = "file"
)

// AttachmentSummary describes a non-text content block. References (http(s) URLs,
// provider file IDs) are carried verbatim; inline payloads (data: URLs, base64
// audio and file data) are reduced to media type and size unless a connector has
// opted into inline bytes.
//
// Spans outlive the request across an async flush, so copying base64 blobs here
// by default would pin megabytes per request until every connector has drained.
type AttachmentSummary struct {
	Kind      AttachmentKind `json:"kind"`
	MediaType string         `json:"media_type,omitempty"` // image/png, audio/mp3, application/pdf
	URL       string         `json:"url,omitempty"`        // reference only; never a data: URL
	FileID    string         `json:"file_id,omitempty"`
	Filename  string         `json:"filename,omitempty"`
	Detail    string         `json:"detail,omitempty"` // image detail hint: low | high | auto
	Format    string         `json:"format,omitempty"` // audio container: mp3 | wav

	// Inline is true when the source carried bytes rather than a reference.
	// ByteSize is the decoded payload size; 0 for a reference.
	Inline   bool `json:"inline,omitempty"`
	ByteSize int  `json:"byte_size,omitempty"`

	// Data holds the base64 payload, and is populated only when a connector
	// declared it reads inline attachments and the payload is under the cap.
	Data string `json:"data,omitempty"`
}

// ToolCallSummary is a summarized tool call carried on a span.
type ToolCallSummary struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

// ReasoningDetailSummary is a summarized reasoning detail carried on a span.
type ReasoningDetailSummary struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// AudioSummary is summarized audio output carried on a span.
type AudioSummary struct {
	ID         string `json:"id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

// redact applies guardrail replacements to the typed payload. Request-side fields
// take input replacements, response-side take output.
func (d *LLMSpanData) redact(input, output map[string]string) {
	if d == nil {
		return
	}
	if len(input) > 0 {
		d.RawRequest = redactRawPayload(d.RawRequest, input)
		redactMessageSummaries(d.InputMessages, input)
		d.redactRequestInputs(input)
		d.redactRequestParams(input)
	}
	if len(output) > 0 {
		d.RawResponse = redactRawPayload(d.RawResponse, output)
		redactMessageSummaries(d.OutputMessages, output)
		d.ReasoningText = ApplyLiteralReplacements(d.ReasoningText, output)
	}
}

// redactRequestInputs redacts the text, embedding and speech inputs. They alias the
// caller's request, so each one that changes is replaced with a redacted copy
// rather than edited.
func (d *LLMSpanData) redactRequestInputs(replacements map[string]string) {
	if in := d.TextPrompt; in != nil {
		var str *string
		if in.PromptStr != nil {
			if r := ApplyLiteralReplacements(*in.PromptStr, replacements); r != *in.PromptStr {
				str = Ptr(r)
			}
		}
		array := redactStrings(in.PromptArray, replacements)
		if str != nil || array != nil {
			cp := *in
			if str != nil {
				cp.PromptStr = str
			}
			if array != nil {
				cp.PromptArray = array
			}
			d.TextPrompt = &cp
		}
	}
	if in := d.SpeechInput; in != nil {
		if r := ApplyLiteralReplacements(in.Input, replacements); r != in.Input {
			cp := *in
			cp.Input = r
			d.SpeechInput = &cp
		}
	}
	if items := redactEmbeddingInput(d.EmbeddingInput, replacements); items != nil {
		d.EmbeddingInput = items
	}
}

// redactRequestParams redacts the request params that carry caller text: speech
// instructions, a transcription prompt and a text-completion suffix. Params alias
// the caller's request too, so a changed one is replaced with a shallow copy.
func (d *LLMSpanData) redactRequestParams(replacements map[string]string) {
	if p := d.Speech; p != nil {
		if r := ApplyLiteralReplacements(p.Instructions, replacements); r != p.Instructions {
			cp := *p
			cp.Instructions = r
			d.Speech = &cp
		}
	}
	if p := d.Transcription; p != nil && p.Prompt != nil {
		if r := ApplyLiteralReplacements(*p.Prompt, replacements); r != *p.Prompt {
			cp := *p
			cp.Prompt = Ptr(r)
			d.Transcription = &cp
		}
	}
	if p := d.TextCompletion; p != nil && p.Suffix != nil {
		if r := ApplyLiteralReplacements(*p.Suffix, replacements); r != *p.Suffix {
			cp := *p
			cp.Suffix = Ptr(r)
			d.TextCompletion = &cp
		}
	}
}

// redactStrings returns a redacted copy of values, or nil when nothing changed.
func redactStrings(values []string, replacements map[string]string) []string {
	var out []string
	for i, v := range values {
		r := ApplyLiteralReplacements(v, replacements)
		if out == nil {
			if r == v {
				continue
			}
			out = make([]string, len(values))
			copy(out, values[:i])
		}
		out[i] = r
	}
	return out
}

// redactEmbeddingInput returns a redacted copy of the embedding input's text
// parts, or nil when nothing changed. Media and token parts are shared as is.
func redactEmbeddingInput(in EmbeddingInput, replacements map[string]string) EmbeddingInput {
	var out EmbeddingInput
	for i, item := range in {
		var content EmbeddingContent
		for j, part := range item.Content {
			if part.Text == nil {
				if content != nil {
					content[j] = part
				}
				continue
			}
			r := ApplyLiteralReplacements(*part.Text, replacements)
			if content == nil {
				if r == *part.Text {
					continue
				}
				content = make(EmbeddingContent, len(item.Content))
				copy(content, item.Content[:j])
			}
			if r != *part.Text {
				part.Text = Ptr(r)
			}
			content[j] = part
		}
		if content == nil {
			if out != nil {
				out[i] = item
			}
			continue
		}
		if out == nil {
			out = make(EmbeddingInput, len(in))
			copy(out, in[:i])
		}
		item.Content = content
		out[i] = item
	}
	return out
}

// redactRawPayload applies guardrail replacements to a raw provider body. The
// replacements are literal text, but a JSON body carries that text escaped (a
// quote, backslash or newline in it, or a non-ASCII rune written as \uXXXX), so a
// literal match on the bytes can miss content the typed fields did redact. A JSON
// body is therefore redacted on its decoded string values and re-encoded, and only
// when a value changed; anything else gets the literal and the JSON-escaped forms.
func redactRawPayload(raw string, replacements map[string]string) string {
	if raw == "" || len(replacements) == 0 {
		return raw
	}
	if redacted, ok := redactJSONStrings(raw, replacements); ok {
		return redacted
	}
	escaped := make(map[string]string, len(replacements))
	for from, to := range replacements {
		if from == "" {
			continue
		}
		escaped[from] = to
		for _, escapeHTML := range []bool{false, true} {
			if quotedFrom := jsonStringBody(from, escapeHTML); quotedFrom != from {
				escaped[quotedFrom] = jsonStringBody(to, escapeHTML)
			}
		}
	}
	return ApplyLiteralReplacements(raw, escaped)
}

// redactJSONStrings reports ok when raw is a JSON document, returning it with the
// replacements applied to every string value and object key.
func redactJSONStrings(raw string, replacements map[string]string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	changed := false
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			if r := ApplyLiteralReplacements(t, replacements); r != t {
				changed = true
				return r
			}
			return t
		case json.Number:
			// A matched number (a phone or account number, say) becomes the
			// replacement string, as the literal pass on the raw bytes did.
			if r := ApplyLiteralReplacements(t.String(), replacements); r != t.String() {
				changed = true
				return r
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, val := range t {
				nk := ApplyLiteralReplacements(k, replacements)
				if nk != k {
					changed = true
				}
				out[nk] = walk(val)
			}
			return out
		}
		return v
	}
	doc = walk(doc)
	if !changed {
		return raw, true
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return "", false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// jsonStringBody is s as it appears between the quotes of a JSON string, with or
// without HTML escaping (<, >, & as \u003c and so on), since encoders differ.
func jsonStringBody(s string, escapeHTML bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(s); err != nil {
		return s
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}

func redactMessageSummaries(msgs []MessageSummary, replacements map[string]string) {
	for i := range msgs {
		msgs[i].Content = ApplyLiteralReplacements(msgs[i].Content, replacements)
		msgs[i].Reasoning = ApplyLiteralReplacements(msgs[i].Reasoning, replacements)
		msgs[i].Refusal = ApplyLiteralReplacements(msgs[i].Refusal, replacements)
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].Args = ApplyLiteralReplacements(msgs[i].ToolCalls[j].Args, replacements)
		}
		for j := range msgs[i].ReasoningDetails {
			msgs[i].ReasoningDetails[j].Text = ApplyLiteralReplacements(msgs[i].ReasoningDetails[j].Text, replacements)
		}
		if msgs[i].Audio != nil {
			msgs[i].Audio.Transcript = ApplyLiteralReplacements(msgs[i].Audio.Transcript, replacements)
		}
	}
}
