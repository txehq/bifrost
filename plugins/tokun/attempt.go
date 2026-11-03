package tokun

import (
	"github.com/capsohq/bifrost/core/schemas"
)

// TODO(tokun-B1): 每次尝试从原始请求开始改写：同格式的尝试使用原始正文，在原字节上把 model 换成这一项的 forward_model、去掉 strip_params，流式请求加 stream_options.include_usage
// 阶段：B1 放入；B1-D7「每次尝试的改写」完成时删除这两行和占位
func (p *Plugin) rewriteAttempt(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, e entry) *schemas.BifrostRequest {
	return req
}

// TODO(tokun-B1): 判断这次尝试失败的错误范围（key、upstream、request），记下尝试记录（序号、provider_id、upstream_id、key_id、状态码、错误范围、耗时、上游耗时）；错误范围在计划的 stop_scopes 里时把 AllowFallbacks 设为 false
// 阶段：B1 放入；B1-D6「执行」完成时删除这两行和占位
func (p *Plugin) recordAttempt(ctx *schemas.BifrostContext, state *requestState, attempt int, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) *schemas.BifrostError {
	return bifrostErr
}

// TODO(tokun-B1): 把 request_id、send_id、抽出的顺序和每次尝试（包括跳过的项）编码成 RouteAttempts，写进响应头 tk-route-attempts
// 阶段：B1 放入；B1-D6「执行」完成时删除这两行和占位
func (p *Plugin) writeAttempts(state *requestState, resp *schemas.HTTPResponseMetadata) {}

// TODO(tokun-B1): 调用方自己没有要求用量时，去掉插件加了 include_usage 以后上游多发的最后那个只有用量的数据块
// 阶段：B1 放入；B1-D7「每次尝试的改写」完成时删除这两行和占位
func (p *Plugin) filterChunk(state *requestState, chunk *schemas.BifrostStreamChunk) *schemas.BifrostStreamChunk {
	return chunk
}
