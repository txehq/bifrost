package tokun

import "fmt"

// routeClass is what the plugin does with a request on a route. It is decided by the route the
// request matched, before the plan is read.
type routeClass uint8

const (
	// routeUnclassified is a route the table does not list: the request is refused with 500 until
	// the route is classified here.
	routeUnclassified routeClass = iota
	// routePlan needs a signed route plan, and the request is dispatched inside it.
	routePlan
	// routeNotOffered is not one of tokun's endpoint types: refused with 404.
	routeNotOffered
	// routeNotImplemented is the realtime conversation: refused with 501.
	routeNotImplemented
)

// route is a route of Bifrost's HTTP transport: a method and the template the router registered,
// which is what HTTPRequest.Route carries.
type route struct {
	method, template string
}

// routeClasses classifies, by method and route template, every route Bifrost registers with the
// inference middlewares (RegisterInferenceRoutes in transports/bifrost-http/server). A route
// added there and missing here is refused as unclassified, and the server's
// TestTokunRouteClassesCoverEveryInferenceRoute fails until it is classified.
var routeClasses = buildRouteClasses()

func buildRouteClasses() map[string]map[string]routeClass {
	classes := make(map[string]map[string]routeClass)
	add := func(class routeClass, r route) {
		if classes[r.method] == nil {
			classes[r.method] = make(map[string]routeClass)
		}
		if _, classified := classes[r.method][r.template]; classified {
			panic(fmt.Sprintf("tokun: route %s %s is classified twice", r.method, r.template))
		}
		classes[r.method][r.template] = class
	}
	for _, routes := range [][]route{inferenceRoutes, asyncRoutes, openAIRoutes, genAIRoutes, cursorRoutes, anthropicRoutes, bedrockRoutes, cohereRoutes, typesafeRoutes, ownPlanRoutes} {
		for _, r := range routes {
			add(routePlan, r)
		}
	}
	for _, prefix := range sdkPrefixes {
		for _, r := range sdkRoutes {
			add(routePlan, route{r.method, prefix + r.template})
		}
	}
	for _, template := range passthroughTemplates {
		for _, method := range passthroughMethods {
			add(routePlan, route{method, template})
		}
	}
	for _, r := range notOfferedRoutes {
		add(routeNotOffered, r)
	}
	for _, r := range notImplementedRoutes {
		add(routeNotImplemented, r)
	}
	return classes
}

// classifyRoute returns the class of the route a request matched, routeUnclassified for a route
// the table does not list (and for a request without a route template).
func classifyRoute(method, template string) routeClass {
	class, classified := routeClasses[method][template]
	if !classified {
		return routeUnclassified
	}
	return class
}

// RouteClass names the class of a route of Bifrost's HTTP transport, by method and route
// template: "plan", "not_offered" or "not_implemented", and "" for a route the plugin does not
// classify.
func RouteClass(method, template string) string {
	switch class := classifyRoute(method, template); class {
	case routePlan:
		return "plan"
	case routeNotOffered:
		return "not_offered"
	case routeNotImplemented:
		return "not_implemented"
	case routeUnclassified:
		return ""
	default:
		panic(fmt.Sprintf("tokun: route class %d has no name", class))
	}
}

// ClassifiedRoutes lists every route the plugin classifies, as "<method> <template>".
func ClassifiedRoutes() []string {
	var routes []string
	for method, templates := range routeClasses {
		for template := range templates {
			routes = append(routes, method+" "+template)
		}
	}
	return routes
}

// notOfferedRoutes are not tokun's endpoint types (the platform design's table of endpoint
// types): the WebSocket Responses API, live sessions over WebSocket and WebRTC, MCP and the Agent
// Gateway. Tokun does not offer them, so they are refused whatever the plan says.
var notOfferedRoutes = []route{
	// WebSocket Responses: the GET upgrades of the Responses routes.
	{"GET", "/v1/responses"},
	{"GET", "/openai/responses"},
	{"GET", "/openai/v1/responses"},
	{"GET", "/openai/openai/responses"},
	// Live sessions and live control.
	{"GET", "/v1/live/sessions"},
	{"POST", "/v1/live/sessions"},
	{"GET", "/v1/live/sessions/{session_id}/attach"},
	{"GET", "/v1/live/sessions/{session_id}/content"},
	{"GET", "/openai/live/sessions"},
	{"POST", "/openai/live/sessions"},
	{"GET", "/openai/live/sessions/{session_id}/attach"},
	{"GET", "/openai/live/sessions/{session_id}/content"},
	{"GET", "/openai/v1/live/sessions"},
	{"POST", "/openai/v1/live/sessions"},
	{"GET", "/openai/v1/live/sessions/{session_id}/attach"},
	{"GET", "/openai/v1/live/sessions/{session_id}/content"},
	// MCP tool execution and Bifrost's MCP server.
	{"POST", "/v1/mcp/tool/execute"},
	{"GET", "/mcp"},
	{"POST", "/mcp"},
	{"GET", "/mcp/{slug}"},
	{"POST", "/mcp/{slug}"},
	// Agent Gateway, its push callback included.
	{"POST", "/agents/a2a/{name}/json-rpc"},
	{"POST", "/agents/a2a/{name}/push/callback"},
	{"DELETE", "/agents/a2a/{name}/rest/{restpath:*}"},
	{"GET", "/agents/a2a/{name}/rest/{restpath:*}"},
	{"POST", "/agents/a2a/{name}/rest/{restpath:*}"},
	{"GET", "/agents/a2a/{name}/.well-known/agent-card.json"},
	// The Anthropic routers' /v1/messages wildcard: it handles any other path under
	// /v1/messages as a message to create, and no endpoint type of tokun has such a path. Its
	// Messages path and the token count are routes of their own.
	{"POST", "/anthropic/v1/messages/{path:*}"},
	{"POST", "/cursor/v1/messages/{path:*}"},
	{"POST", "/langchain/v1/messages/{path:*}"},
	{"POST", "/litellm/v1/messages/{path:*}"},
	{"POST", "/pydanticai/v1/messages/{path:*}"},
}

// TODO(tokun-B3): 实时对话的路由按签名计划处理：只在打开连接、创建通话或创建 client secret 的请求上验证计划，连接期间的调度受计划约束，连接寿命不超过计划的有效期
// 阶段：B1 放入；B3-10「实时对话」完成时删除这两行和占位
var notImplementedRoutes = []route{
	{"GET", "/v1/realtime"},
	{"POST", "/v1/realtime"},
	{"POST", "/v1/realtime/calls"},
	{"POST", "/v1/realtime/client_secrets"},
	{"POST", "/v1/realtime/sessions"},
	{"GET", "/openai/realtime"},
	{"POST", "/openai/realtime"},
	{"POST", "/openai/realtime/calls"},
	{"GET", "/openai/openai/realtime"},
	{"POST", "/openai/openai/realtime"},
	{"POST", "/openai/openai/realtime/calls"},
	{"GET", "/openai/v1/realtime"},
	{"POST", "/openai/v1/realtime"},
	{"POST", "/openai/v1/realtime/calls"},
	{"POST", "/openai/v1/realtime/client_secrets"},
	{"POST", "/openai/v1/realtime/sessions"},
}

// inferenceRoutes are the inference handler's routes.
var inferenceRoutes = []route{
	{"POST", "/v1/audio/speech"},
	{"POST", "/v1/audio/transcriptions"},
	{"GET", "/v1/batches"},
	{"POST", "/v1/batches"},
	{"GET", "/v1/batches/{batch_id}"},
	{"POST", "/v1/batches/{batch_id}/cancel"},
	{"GET", "/v1/batches/{batch_id}/results"},
	{"POST", "/v1/chat/completions"},
	{"POST", "/v1/completions"},
	{"GET", "/v1/containers"},
	{"POST", "/v1/containers"},
	{"DELETE", "/v1/containers/{container_id}"},
	{"GET", "/v1/containers/{container_id}"},
	{"GET", "/v1/containers/{container_id}/files"},
	{"POST", "/v1/containers/{container_id}/files"},
	{"DELETE", "/v1/containers/{container_id}/files/{file_id}"},
	{"GET", "/v1/containers/{container_id}/files/{file_id}"},
	{"GET", "/v1/containers/{container_id}/files/{file_id}/content"},
	{"POST", "/v1/decisions"},
	{"POST", "/v1/embeddings"},
	{"GET", "/v1/files"},
	{"POST", "/v1/files"},
	{"DELETE", "/v1/files/{file_id}"},
	{"GET", "/v1/files/{file_id}"},
	{"GET", "/v1/files/{file_id}/content"},
	{"POST", "/v1/images/edits"},
	{"POST", "/v1/images/generations"},
	{"POST", "/v1/images/variations"},
	{"GET", "/v1/models"},
	{"GET", "/v1/models/{model:*}"},
	{"POST", "/v1/ocr"},
	{"POST", "/v1/rerank"},
	{"POST", "/v1/responses"},
	{"POST", "/v1/responses/compact"},
	{"POST", "/v1/responses/input_tokens"},
	{"DELETE", "/v1/responses/{response_id}"},
	{"GET", "/v1/responses/{response_id}"},
	{"POST", "/v1/responses/{response_id}/cancel"},
	{"GET", "/v1/responses/{response_id}/input_items"},
	{"GET", "/v1/videos"},
	{"POST", "/v1/videos"},
	{"POST", "/v1/videos/edits"},
	{"DELETE", "/v1/videos/{video_id}"},
	{"GET", "/v1/videos/{video_id}"},
	{"GET", "/v1/videos/{video_id}/content"},
	{"POST", "/v1/videos/{video_id}/remix"},
}

// asyncRoutes are the async handler's submissions and polls.
var asyncRoutes = []route{
	{"POST", "/v1/async/audio/speech"},
	{"GET", "/v1/async/audio/speech/{job_id}"},
	{"POST", "/v1/async/audio/transcriptions"},
	{"GET", "/v1/async/audio/transcriptions/{job_id}"},
	{"POST", "/v1/async/chat/completions"},
	{"GET", "/v1/async/chat/completions/{job_id}"},
	{"POST", "/v1/async/completions"},
	{"GET", "/v1/async/completions/{job_id}"},
	{"POST", "/v1/async/embeddings"},
	{"GET", "/v1/async/embeddings/{job_id}"},
	{"POST", "/v1/async/images/edits"},
	{"GET", "/v1/async/images/edits/{job_id}"},
	{"POST", "/v1/async/images/generations"},
	{"GET", "/v1/async/images/generations/{job_id}"},
	{"POST", "/v1/async/images/variations"},
	{"GET", "/v1/async/images/variations/{job_id}"},
	{"POST", "/v1/async/ocr"},
	{"GET", "/v1/async/ocr/{job_id}"},
	{"POST", "/v1/async/rerank"},
	{"GET", "/v1/async/rerank/{job_id}"},
	{"POST", "/v1/async/responses"},
	{"GET", "/v1/async/responses/{job_id}"},
}

// openAIRoutes are the OpenAI SDK router's HTTP routes.
var openAIRoutes = []route{
	{"POST", "/openai/audio/speech"},
	{"POST", "/openai/audio/transcriptions"},
	{"GET", "/openai/batches"},
	{"POST", "/openai/batches"},
	{"GET", "/openai/batches/{batch_id}"},
	{"POST", "/openai/batches/{batch_id}/cancel"},
	{"POST", "/openai/chat/completions"},
	{"POST", "/openai/completions"},
	{"GET", "/openai/containers"},
	{"POST", "/openai/containers"},
	{"DELETE", "/openai/containers/{container_id}"},
	{"GET", "/openai/containers/{container_id}"},
	{"GET", "/openai/containers/{container_id}/files"},
	{"POST", "/openai/containers/{container_id}/files"},
	{"DELETE", "/openai/containers/{container_id}/files/{file_id}"},
	{"GET", "/openai/containers/{container_id}/files/{file_id}"},
	{"GET", "/openai/containers/{container_id}/files/{file_id}/content"},
	{"POST", "/openai/decisions"},
	{"POST", "/openai/embeddings"},
	{"GET", "/openai/files"},
	{"POST", "/openai/files"},
	{"DELETE", "/openai/files/{file_id}"},
	{"GET", "/openai/files/{file_id}"},
	{"GET", "/openai/files/{file_id}/content"},
	{"POST", "/openai/images/edits"},
	{"POST", "/openai/images/generations"},
	{"POST", "/openai/images/variations"},
	{"GET", "/openai/models"},
	{"GET", "/openai/models/{model:*}"},
	{"GET", "/openai/openai/batches"},
	{"POST", "/openai/openai/batches"},
	{"GET", "/openai/openai/batches/{batch_id}"},
	{"POST", "/openai/openai/batches/{batch_id}/cancel"},
	{"POST", "/openai/openai/deployments/{deploymentPath:*}"},
	{"GET", "/openai/openai/files"},
	{"POST", "/openai/openai/files"},
	{"DELETE", "/openai/openai/files/{file_id}"},
	{"GET", "/openai/openai/files/{file_id}"},
	{"GET", "/openai/openai/files/{file_id}/content"},
	{"GET", "/openai/openai/models"},
	{"GET", "/openai/openai/models/{model:*}"},
	{"POST", "/openai/openai/responses"},
	{"POST", "/openai/openai/responses/compact"},
	{"POST", "/openai/openai/responses/input_tokens"},
	{"DELETE", "/openai/openai/responses/{response_id}"},
	{"GET", "/openai/openai/responses/{response_id}"},
	{"POST", "/openai/openai/responses/{response_id}/cancel"},
	{"GET", "/openai/openai/responses/{response_id}/input_items"},
	{"GET", "/openai/openai/videos"},
	{"POST", "/openai/openai/videos"},
	{"DELETE", "/openai/openai/videos/{video_id}"},
	{"GET", "/openai/openai/videos/{video_id}"},
	{"GET", "/openai/openai/videos/{video_id}/content"},
	{"POST", "/openai/openai/videos/{video_id}/remix"},
	{"POST", "/openai/responses"},
	{"POST", "/openai/responses/compact"},
	{"POST", "/openai/responses/input_tokens"},
	{"DELETE", "/openai/responses/{response_id}"},
	{"GET", "/openai/responses/{response_id}"},
	{"POST", "/openai/responses/{response_id}/cancel"},
	{"GET", "/openai/responses/{response_id}/input_items"},
	{"POST", "/openai/v1/audio/speech"},
	{"POST", "/openai/v1/audio/transcriptions"},
	{"GET", "/openai/v1/batches"},
	{"POST", "/openai/v1/batches"},
	{"GET", "/openai/v1/batches/{batch_id}"},
	{"POST", "/openai/v1/batches/{batch_id}/cancel"},
	{"POST", "/openai/v1/chat/completions"},
	{"POST", "/openai/v1/completions"},
	{"GET", "/openai/v1/containers"},
	{"POST", "/openai/v1/containers"},
	{"DELETE", "/openai/v1/containers/{container_id}"},
	{"GET", "/openai/v1/containers/{container_id}"},
	{"GET", "/openai/v1/containers/{container_id}/files"},
	{"POST", "/openai/v1/containers/{container_id}/files"},
	{"DELETE", "/openai/v1/containers/{container_id}/files/{file_id}"},
	{"GET", "/openai/v1/containers/{container_id}/files/{file_id}"},
	{"GET", "/openai/v1/containers/{container_id}/files/{file_id}/content"},
	{"POST", "/openai/v1/decisions"},
	{"POST", "/openai/v1/embeddings"},
	{"GET", "/openai/v1/files"},
	{"POST", "/openai/v1/files"},
	{"DELETE", "/openai/v1/files/{file_id}"},
	{"GET", "/openai/v1/files/{file_id}"},
	{"GET", "/openai/v1/files/{file_id}/content"},
	{"POST", "/openai/v1/images/edits"},
	{"POST", "/openai/v1/images/generations"},
	{"POST", "/openai/v1/images/variations"},
	{"GET", "/openai/v1/models"},
	{"GET", "/openai/v1/models/{model:*}"},
	{"POST", "/openai/v1/responses"},
	{"POST", "/openai/v1/responses/compact"},
	{"POST", "/openai/v1/responses/input_tokens"},
	{"DELETE", "/openai/v1/responses/{response_id}"},
	{"GET", "/openai/v1/responses/{response_id}"},
	{"POST", "/openai/v1/responses/{response_id}/cancel"},
	{"GET", "/openai/v1/responses/{response_id}/input_items"},
	{"GET", "/openai/v1/videos"},
	{"POST", "/openai/v1/videos"},
	{"POST", "/openai/v1/videos/edits"},
	{"DELETE", "/openai/v1/videos/{video_id}"},
	{"GET", "/openai/v1/videos/{video_id}"},
	{"GET", "/openai/v1/videos/{video_id}/content"},
	{"POST", "/openai/v1/videos/{video_id}/remix"},
	{"GET", "/openai/videos"},
	{"POST", "/openai/videos"},
	{"POST", "/openai/videos/edits"},
	{"DELETE", "/openai/videos/{video_id}"},
	{"GET", "/openai/videos/{video_id}"},
	{"GET", "/openai/videos/{video_id}/content"},
	{"POST", "/openai/videos/{video_id}/remix"},
}

// sdkPrefixes are the routers that each register sdkRoutes under their own prefix.
var sdkPrefixes = []string{"/langchain", "/litellm", "/pydanticai"}

// sdkRoutes are the routes the LangChain, LiteLLM and PydanticAI routers register, without the
// router's prefix: the OpenAI, Anthropic, GenAI, Bedrock and Cohere SDK routes.
var sdkRoutes = []route{
	{"DELETE", "/openai/responses/{response_id}"},
	{"DELETE", "/openai/videos/{video_id}"},
	{"DELETE", "/responses/{response_id}"},
	{"DELETE", "/v1/responses/{response_id}"},
	{"DELETE", "/v1/videos/{video_id}"},
	{"DELETE", "/videos/{video_id}"},
	{"GET", "/openai/responses/{response_id}"},
	{"GET", "/openai/responses/{response_id}/input_items"},
	{"GET", "/openai/videos"},
	{"GET", "/openai/videos/{video_id}"},
	{"GET", "/openai/videos/{video_id}/content"},
	{"GET", "/responses/{response_id}"},
	{"GET", "/responses/{response_id}/input_items"},
	{"GET", "/v1beta/models"},
	{"GET", "/v1beta/models/{model}"},
	{"GET", "/v1beta/models/{model}/operations/{operation_id:*}"},
	{"GET", "/v1/responses/{response_id}"},
	{"GET", "/v1/responses/{response_id}/input_items"},
	{"GET", "/v1/videos"},
	{"GET", "/v1/videos/{video_id}"},
	{"GET", "/v1/videos/{video_id}/content"},
	{"GET", "/videos"},
	{"GET", "/videos/{video_id}"},
	{"GET", "/videos/{video_id}/content"},
	{"POST", "/audio/speech"},
	{"POST", "/audio/transcriptions"},
	{"POST", "/chat/completions"},
	{"POST", "/completions"},
	{"POST", "/decisions"},
	{"POST", "/embeddings"},
	{"POST", "/images/edits"},
	{"POST", "/images/generations"},
	{"POST", "/images/variations"},
	{"POST", "/model/{modelId}/converse"},
	{"POST", "/model/{modelId}/converse-stream"},
	{"POST", "/model/{modelId}/count-tokens"},
	{"POST", "/model/{modelId}/invoke"},
	{"POST", "/model/{modelId}/invoke-with-response-stream"},
	{"POST", "/openai/deployments/{deploymentPath:*}"},
	{"POST", "/openai/responses"},
	{"POST", "/openai/responses/compact"},
	{"POST", "/openai/responses/input_tokens"},
	{"POST", "/openai/responses/{response_id}/cancel"},
	{"POST", "/openai/videos"},
	{"POST", "/openai/videos/{video_id}/remix"},
	{"POST", "/rerank"},
	{"POST", "/responses"},
	{"POST", "/responses/compact"},
	{"POST", "/responses/input_tokens"},
	{"POST", "/responses/{response_id}/cancel"},
	{"POST", "/v1/audio/speech"},
	{"POST", "/v1/audio/transcriptions"},
	{"POST", "/v1beta/models/{model:*}"},
	{"POST", "/v1/chat/completions"},
	{"POST", "/v1/complete"},
	{"POST", "/v1/completions"},
	{"POST", "/v1/decisions"},
	{"POST", "/v1/embeddings"},
	{"POST", "/v1/images/edits"},
	{"POST", "/v1/images/generations"},
	{"POST", "/v1/images/variations"},
	{"POST", "/v1/messages"},
	{"POST", "/v1/messages/count_tokens"},
	{"POST", "/v1/rank"},
	{"POST", "/v1/responses"},
	{"POST", "/v1/responses/compact"},
	{"POST", "/v1/responses/input_tokens"},
	{"POST", "/v1/responses/{response_id}/cancel"},
	{"POST", "/v1/tokenize"},
	{"POST", "/v1/videos"},
	{"POST", "/v1/videos/edits"},
	{"POST", "/v1/videos/{video_id}/remix"},
	{"POST", "/v2/chat"},
	{"POST", "/v2/embed"},
	{"POST", "/v2/rerank"},
	{"POST", "/videos"},
	{"POST", "/videos/edits"},
	{"POST", "/videos/{video_id}/remix"},
}

// genAIRoutes are the GenAI SDK router's routes.
var genAIRoutes = []route{
	{"POST", "/genai/upload/v1beta/files"},
	{"GET", "/genai/v1beta/batches"},
	{"DELETE", "/genai/v1beta/batches/{batch_id}"},
	{"GET", "/genai/v1beta/batches/{batch_id}"},
	{"POST", "/genai/v1beta/batches/{batch_id}"},
	{"GET", "/genai/v1beta/cachedContents"},
	{"POST", "/genai/v1beta/cachedContents"},
	{"DELETE", "/genai/v1beta/cachedContents/{cached_id}"},
	{"GET", "/genai/v1beta/cachedContents/{cached_id}"},
	{"PATCH", "/genai/v1beta/cachedContents/{cached_id}"},
	{"GET", "/genai/v1beta/files"},
	{"DELETE", "/genai/v1beta/files/{file_id}"},
	{"GET", "/genai/v1beta/files/{file_id}"},
	{"GET", "/genai/v1beta/models"},
	{"POST", "/genai/v1beta/models/{model:*}"},
	{"GET", "/genai/v1beta/models/{model}"},
	{"GET", "/genai/v1beta/models/{model}/operations/{operation_id:*}"},
	{"GET", "/genai/v1beta/projects/{project}/locations/{location}/cachedContents"},
	{"POST", "/genai/v1beta/projects/{project}/locations/{location}/cachedContents"},
	{"DELETE", "/genai/v1beta/projects/{project}/locations/{location}/cachedContents/{cached_id}"},
	{"GET", "/genai/v1beta/projects/{project}/locations/{location}/cachedContents/{cached_id}"},
	{"PATCH", "/genai/v1beta/projects/{project}/locations/{location}/cachedContents/{cached_id}"},
	{"GET", "/genai/v1/projects/{project}/locations/{location}/batchPredictionJobs"},
	{"POST", "/genai/v1/projects/{project}/locations/{location}/batchPredictionJobs"},
	{"DELETE", "/genai/v1/projects/{project}/locations/{location}/batchPredictionJobs/{batch_id}"},
	{"GET", "/genai/v1/projects/{project}/locations/{location}/batchPredictionJobs/{batch_id}"},
	{"POST", "/genai/v1/projects/{project}/locations/{location}/batchPredictionJobs/{batch_id}"},
	{"POST", "/genai/v1/rank"},
}

// cursorRoutes are the Cursor router's routes.
var cursorRoutes = []route{
	{"POST", "/cursor/chat/completions"},
	{"POST", "/cursor/model/{modelId}/converse"},
	{"POST", "/cursor/model/{modelId}/converse-stream"},
	{"POST", "/cursor/model/{modelId}/count-tokens"},
	{"POST", "/cursor/model/{modelId}/invoke"},
	{"POST", "/cursor/model/{modelId}/invoke-with-response-stream"},
	{"GET", "/cursor/models"},
	{"GET", "/cursor/models/{model:*}"},
	{"GET", "/cursor/openai/models"},
	{"GET", "/cursor/openai/models/{model:*}"},
	{"POST", "/cursor/rerank"},
	{"GET", "/cursor/v1beta/models"},
	{"POST", "/cursor/v1beta/models/{model:*}"},
	{"GET", "/cursor/v1beta/models/{model}"},
	{"GET", "/cursor/v1beta/models/{model}/operations/{operation_id:*}"},
	{"POST", "/cursor/v1/chat/completions"},
	{"POST", "/cursor/v1/complete"},
	{"POST", "/cursor/v1/messages"},
	{"POST", "/cursor/v1/messages/count_tokens"},
	{"GET", "/cursor/v1/models"},
	{"GET", "/cursor/v1/models/{model:*}"},
	{"POST", "/cursor/v1/rank"},
	{"POST", "/cursor/v1/tokenize"},
	{"POST", "/cursor/v2/chat"},
	{"POST", "/cursor/v2/embed"},
	{"POST", "/cursor/v2/rerank"},
}

// anthropicRoutes are the Anthropic SDK router's routes.
var anthropicRoutes = []route{
	{"POST", "/anthropic/v1/complete"},
	{"GET", "/anthropic/v1/files"},
	{"POST", "/anthropic/v1/files"},
	{"DELETE", "/anthropic/v1/files/{file_id}"},
	{"GET", "/anthropic/v1/files/{file_id}"},
	{"GET", "/anthropic/v1/files/{file_id}/content"},
	{"POST", "/anthropic/v1/messages"},
	{"GET", "/anthropic/v1/messages/batches"},
	{"POST", "/anthropic/v1/messages/batches"},
	{"GET", "/anthropic/v1/messages/batches/{batch_id}"},
	{"POST", "/anthropic/v1/messages/batches/{batch_id}/cancel"},
	{"GET", "/anthropic/v1/messages/batches/{batch_id}/results"},
	{"POST", "/anthropic/v1/messages/count_tokens"},
	{"GET", "/anthropic/v1/models"},
}

// bedrockRoutes are the Bedrock SDK router's routes.
var bedrockRoutes = []route{
	{"GET", "/bedrock/files/{bucket}"},
	{"DELETE", "/bedrock/files/{bucket}/{key:*}"},
	{"GET", "/bedrock/files/{bucket}/{key:*}"},
	{"HEAD", "/bedrock/files/{bucket}/{key:*}"},
	{"PUT", "/bedrock/files/{bucket}/{key:*}"},
	{"POST", "/bedrock/model-invocation-job"},
	{"GET", "/bedrock/model-invocation-job/{job_arn}"},
	{"POST", "/bedrock/model-invocation-job/{job_arn}/stop"},
	{"GET", "/bedrock/model-invocation-jobs"},
	{"POST", "/bedrock/model/{modelId}/converse"},
	{"POST", "/bedrock/model/{modelId}/converse-stream"},
	{"POST", "/bedrock/model/{modelId}/count-tokens"},
	{"POST", "/bedrock/model/{modelId}/invoke"},
	{"POST", "/bedrock/model/{modelId}/invoke-with-response-stream"},
	{"POST", "/bedrock/rerank"},
}

// cohereRoutes are the Cohere SDK router's routes.
var cohereRoutes = []route{
	{"POST", "/cohere/v1/tokenize"},
	{"POST", "/cohere/v2/chat"},
	{"POST", "/cohere/v2/embed"},
	{"POST", "/cohere/v2/rerank"},
}

// typesafeRoutes are the typesafe router's routes.
var typesafeRoutes = []route{
	{"GET", "/typesafe/v1/models"},
	{"POST", "/typesafe/v1/systemone"},
}

// ownPlanRoutes are single routes of routers whose other routes are listed with a shared set: the
// ChatGPT passthrough, which registers one method.
var ownPlanRoutes = []route{
	{"POST", "/chatgpt_passthrough/backend-api/codex/responses"},
}

// passthroughTemplates are the passthrough routers' routes, each registered for every method in
// passthroughMethods.
var passthroughTemplates = []string{
	"/genai_passthrough/{path:*}",
	"/genai_passthrough/v1beta1/{path:*}",
	"/genai_passthrough/v1beta/{path:*}",
	"/genai_passthrough/v1/{path:*}",
	"/openai_passthrough/{path:*}",
	"/anthropic_passthrough/{path:*}",
	"/azure_passthrough/{path:*}",
	"/bedrock_passthrough/{path:*}",
	"/runware_passthrough/{path:*}",
}

var passthroughMethods = []string{"DELETE", "GET", "HEAD", "PATCH", "POST", "PUT"}
