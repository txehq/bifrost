package schemas

import "time"

// RequestedCacheControlTTL returns the longest cache_control.ttl a chat or Responses request
// carries on any block, tool or message, or "" when none carries one. Bifrost stamps it on the
// context of an attempt whose base provider is in CacheControlTTLPricedCacheWrites
// (BifrostContextKeyRequestedCacheControlTTL), and the request path then reads the TTL from the
// body actually sent (fork, TXE-8086). The walk is over the typed request, so it costs a few
// nil checks per message and no allocation. TestRequestedCacheControlTTLReadsEveryCacheControl
// finds every *CacheControl a request can carry and fails when one is not read here.
func (br *BifrostRequest) RequestedCacheControlTTL() string {
	if br == nil {
		return ""
	}
	var w cacheControlTTLWalk
	switch {
	case br.ChatRequest != nil:
		w.chat(br.ChatRequest)
	case br.ResponsesRequest != nil:
		w.responses(br.ResponsesRequest)
	}
	return w.max.String()
}

// LongerCacheTTL returns whichever of two TTLs is longer, by the rule of CacheTTLMax.
func LongerCacheTTL(a, b string) string {
	var m CacheTTLMax
	m.Add(a)
	m.Add(b)
	return m.String()
}

// maxCacheTTLLen is the longest TTL that is parsed. A real one is a few characters ("5m", "1h",
// "12h"); a longer string is unparseable for pricing and is never handed to time.ParseDuration.
const maxCacheTTLLen = 32

// CacheTTLMax keeps the longest of a stream of TTLs, parsing each one once, so reading thousands of
// markers costs time linear in their total length (codex security review on #436, TXE-8086). An
// empty TTL is ignored, and a TTL that is not a Go duration, or is longer than maxCacheTTLLen,
// ranks below every duration, so pricing costs it at the base rate. The first TTL is kept on a tie
// and while nothing parseable follows it.
type CacheTTLMax struct {
	best     string
	duration time.Duration
	parsed   bool
}

// Add offers ttl.
func (m *CacheTTLMax) Add(ttl string) {
	if ttl == "" {
		return
	}
	duration, parsed := cacheTTLDuration(ttl)
	switch {
	case m.best == "":
		m.best, m.duration, m.parsed = ttl, duration, parsed
	case parsed && (!m.parsed || duration > m.duration):
		m.best, m.duration, m.parsed = ttl, duration, true
	}
}

// String returns the longest TTL offered, or "" when none was.
func (m CacheTTLMax) String() string { return m.best }

func cacheTTLDuration(ttl string) (time.Duration, bool) {
	if len(ttl) > maxCacheTTLLen {
		return 0, false
	}
	// A Go duration starts with a sign, a digit or a decimal point. Anything else is refused here,
	// so a word ("forever") costs no error allocation.
	if c := ttl[0]; c != '+' && c != '-' && c != '.' && (c < '0' || c > '9') {
		return 0, false
	}
	duration, err := time.ParseDuration(ttl)
	return duration, err == nil
}

type cacheControlTTLWalk struct{ max CacheTTLMax }

func (w *cacheControlTTLWalk) add(cc *CacheControl) {
	if cc != nil && cc.TTL != nil {
		w.max.Add(*cc.TTL)
	}
}

func (w *cacheControlTTLWalk) chat(req *BifrostChatRequest) {
	if p := req.Params; p != nil {
		w.add(p.CacheControl)
		for i := range p.Tools {
			w.add(p.Tools[i].CacheControl)
		}
	}
	for i := range req.Input {
		msg := &req.Input[i]
		if msg.Content != nil {
			for j := range msg.Content.ContentBlocks {
				w.add(msg.Content.ContentBlocks[j].CacheControl)
			}
		}
		if msg.ChatAssistantMessage != nil {
			for j := range msg.ChatAssistantMessage.ToolCalls {
				w.add(msg.ChatAssistantMessage.ToolCalls[j].CacheControl)
			}
		}
	}
}

func (w *cacheControlTTLWalk) responses(req *BifrostResponsesRequest) {
	if p := req.Params; p != nil {
		for i := range p.Tools {
			w.add(p.Tools[i].CacheControl)
		}
	}
	for i := range req.Input {
		msg := &req.Input[i]
		w.add(msg.CacheControl)
		if msg.Content != nil {
			w.responsesBlocks(msg.Content.ContentBlocks)
		}
		if msg.ResponsesToolMessage != nil && msg.ResponsesToolMessage.Output != nil {
			w.responsesBlocks(msg.ResponsesToolMessage.Output.ResponsesFunctionToolCallOutputBlocks)
		}
	}
}

func (w *cacheControlTTLWalk) responsesBlocks(blocks []ResponsesMessageContentBlock) {
	for i := range blocks {
		block := &blocks[i]
		w.add(block.CacheControl)
		if block.ToolChange != nil && block.ToolChange.Definition != nil {
			w.add(block.ToolChange.Definition.CacheControl)
		}
	}
}
