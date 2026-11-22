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
	return w.best
}

// LongerCacheTTL returns whichever of two TTLs is longer. An empty TTL loses to any other, and a
// TTL that is not a Go duration ranks below every duration, so pricing costs it at the base rate.
// On a tie, a is kept.
func LongerCacheTTL(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	da, errA := time.ParseDuration(a)
	db, errB := time.ParseDuration(b)
	switch {
	case errB != nil:
		return a
	case errA != nil:
		return b
	case db > da:
		return b
	}
	return a
}

type cacheControlTTLWalk struct{ best string }

func (w *cacheControlTTLWalk) add(cc *CacheControl) {
	if cc != nil && cc.TTL != nil {
		w.best = LongerCacheTTL(w.best, *cc.TTL)
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
