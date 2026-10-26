package complexity_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore"
	configstoreTables "github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/kvstore"
	"github.com/capsohq/bifrost/plugins/routing"
	"github.com/capsohq/bifrost/plugins/routing/complexity"
	"github.com/capsohq/bifrost/plugins/routing/rules"
)

func chatString(text string) *schemas.ChatMessageContent {
	return &schemas.ChatMessageContent{ContentStr: &text}
}

// newComplexityRuleFixture builds a routing plugin whose store carries one
// rule that fires only when a complexity tier was published.
func newComplexityRuleFixture(t *testing.T) *routing.RoutingPlugin {
	return newComplexityRuleFixtureWithConfig(t, nil)
}

func newComplexityRuleFixtureWithConfig(t *testing.T, config *routing.Config) *routing.RoutingPlugin {
	t.Helper()
	logger := rules.NewMockLogger()
	provider := "openai"
	model := "gpt-4o-mini"

	ruleStore, err := rules.NewLocalStore(context.Background(), logger, nil)
	require.NoError(t, err)
	require.NoError(t, ruleStore.UpsertRule(context.Background(), &configstoreTables.TableRoutingRule{
		ID:            "rule-1",
		Name:          "Complexity Available",
		CelExpression: `complexity_tier != ""`,
		Targets: []configstoreTables.TableRoutingTarget{
			{Provider: &provider, Model: &model, Weight: 1.0},
		},
		Enabled:  schemas.Ptr(true),
		Scope:    "global",
		Priority: 0,
	}))

	plugin, err := routing.InitFromStore(context.Background(), config, logger, nil, ruleStore, routing.NewMockGovernance())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
	return plugin
}

func newSessionComplexityRuleFixture(t *testing.T) *routing.RoutingPlugin {
	t.Helper()
	store, err := kvstore.New(kvstore.Config{CleanupInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return newComplexityRuleFixtureWithConfig(t, &routing.Config{KVStore: store})
}

func chatRequest(text string) *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Input: []schemas.ChatMessage{
				{
					Role:    schemas.ChatMessageRoleUser,
					Content: chatString(text),
				},
			},
		},
	}
}

// TestPreRequestHook_ComplexitySkippedWithoutEmbeddingModel pins the contract
// that semantic classification is the only mechanism. A deployment with phrase
// lists but no embedding model publishes no tier at all rather than quietly
// scoring those phrases as literal keywords.
func TestPreRequestHook_ComplexitySkippedWithoutEmbeddingModel(t *testing.T) {
	plugin := newComplexityRuleFixture(t)

	req := chatRequest("What is a vector database?")
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	// No embedding model is configured, so there is no mechanism left to
	// classify with: the rule's complexity_tier reference cannot match, the
	// routing-rule engine never fires, and the request keeps the model it
	// arrived with.
	engines, _ := bfCtx.Value(schemas.BifrostContextKeyRoutingEnginesUsed).([]string)
	require.NotContains(t, engines, schemas.RoutingEngineRoutingRule)

	providerOut, modelOut, _ := req.GetRequestFields()
	require.Equal(t, schemas.OpenAI, providerOut)
	require.Equal(t, "gpt-4o", modelOut)

	_, ok := bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier).(string)
	require.False(t, ok, "no complexity tier should be published without an embedding model")
	mechanism, ok := bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism).(string)
	require.True(t, ok, "routing mechanism should be recorded in context")
	require.Equal(t, complexity.MechanismSkipped, mechanism)
	_, ok = bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityScore).(float64)
	require.False(t, ok, "no complexity score should be published without an embedding model")
}

// testVectorForText gives every tier's exemplar a distinct axis so nearest-
// neighbour classification is exact and deterministic.
func testVectorForText(text string) []float64 {
	switch {
	case strings.Contains(text, "casual greeting"):
		return []float64{1, 0, 0}
	case strings.Contains(text, "implementation detail"):
		return []float64{0, 1, 0}
	case strings.Contains(text, "deep architectural tradeoff"):
		return []float64{0, 0, 1}
	case strings.Contains(text, "medium request"):
		return []float64{0, 1, 0}
	case strings.Contains(text, "complex request"):
		return []float64{0, 0, 1}
	default: // request text: nearest to the SIMPLE exemplar
		return []float64{0.9, 0.1, 0}
	}
}

func sessionAnalyzerConfig() *complexity.AnalyzerConfig {
	return &complexity.AnalyzerConfig{
		Keywords: configstore.ComplexityEditableKeywordConfig{
			SimpleKeywords:  []string{"a casual greeting"},
			MediumKeywords:  []string{"an implementation detail question"},
			ComplexKeywords: []string{"a deep architectural tradeoff analysis"},
		},
		Semantic: &configstore.ComplexitySemanticConfig{
			Provider:       "openai",
			EmbeddingModel: "test-embedding-model",
		},
		Session: &configstore.ComplexitySessionConfig{Enabled: true},
	}
}

func waitForSemanticClassifier(t *testing.T, plugin *routing.RoutingPlugin) {
	t.Helper()
	require.Eventually(t, func() bool {
		return plugin.ComplexitySemanticStatus().State == complexity.SemanticStatusReady
	}, 5*time.Second, 10*time.Millisecond, "semantic warmup should become ready")
}

func complexitySessionContext(sessionID string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if sessionID != "" {
		ctx.SetValue(schemas.BifrostContextKeySessionID, sessionID)
	}
	return ctx
}

func testEmbeddingExecutor(_ *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	texts := make([]string, 0, len(req.Input))
	for _, item := range req.Input {
		for _, part := range item.Content {
			if part.Text != nil {
				texts = append(texts, *part.Text)
			}
		}
	}
	data := make([]schemas.EmbeddingData, len(texts))
	for i, text := range texts {
		data[i] = schemas.EmbeddingData{Index: i, Embedding: schemas.EmbeddingStruct{EmbeddingArray: testVectorForText(text)}}
	}
	return &schemas.BifrostEmbeddingResponse{
		Data:  data,
		Usage: &schemas.BifrostLLMUsage{TotalTokens: len(texts) * 3},
	}, nil
}

// TestPreRequestHook_SemanticComplexityPublishesTierAndRoutes runs the full
// path: exemplar warmup through the embedded store, nearest-neighbour
// classification of the request, tier/mechanism/score publication, and the
// complexity_tier rule firing on the published tier.
func TestPreRequestHook_SemanticComplexityPublishesTierAndRoutes(t *testing.T) {
	plugin := newComplexityRuleFixture(t)
	plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
	plugin.ReloadComplexityAnalyzerConfig(&complexity.AnalyzerConfig{
		Keywords: configstore.ComplexityEditableKeywordConfig{
			SimpleKeywords:  []string{"a casual greeting"},
			MediumKeywords:  []string{"an implementation detail question"},
			ComplexKeywords: []string{"a deep architectural tradeoff analysis"},
		},
		Semantic: &configstore.ComplexitySemanticConfig{
			Provider:       "openai",
			EmbeddingModel: "test-embedding-model",
		},
	})

	require.Eventually(t, func() bool {
		return plugin.ComplexitySemanticStatus().State == complexity.SemanticStatusReady
	}, 5*time.Second, 10*time.Millisecond, "semantic warmup should become ready")

	req := chatRequest("hey, quick question")
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	tier, ok := bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier).(string)
	require.True(t, ok, "complexity tier should be published")
	require.Equal(t, complexity.TierSimple, tier)
	mechanism, _ := bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism).(string)
	require.Equal(t, complexity.MechanismSemantic, mechanism)
	score, ok := bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityScore).(float64)
	require.True(t, ok, "complexity score should be published")
	require.Greater(t, score, 0.0)

	// The rule fired on the published tier and rewrote the model.
	engines, _ := bfCtx.Value(schemas.BifrostContextKeyRoutingEnginesUsed).([]string)
	require.Contains(t, engines, schemas.RoutingEngineRoutingRule)
	_, modelOut, _ := req.GetRequestFields()
	require.Equal(t, "gpt-4o-mini", modelOut)
}

func TestPreRequestHook_SessionComplexityOnlyEscalates(t *testing.T) {
	plugin := newSessionComplexityRuleFixture(t)
	plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(sessionAnalyzerConfig()))
	waitForSemanticClassifier(t, plugin)

	tests := []struct {
		requestText   string
		wantTier      string
		wantMechanism string
		wantLogParts  []string
	}{
		{
			requestText:   "a simple request",
			wantTier:      complexity.TierSimple,
			wantMechanism: complexity.MechanismSemantic,
			wantLogParts:  []string{"Session complexity initialized:", "effective=SIMPLE", "proposed=SIMPLE", "source=semantic", "proposed_similarity=", `proposed_matched="a casual greeting"`},
		},
		{
			requestText:   "a medium request",
			wantTier:      complexity.TierMedium,
			wantMechanism: complexity.MechanismSemantic,
			wantLogParts:  []string{"Session complexity escalated:", "effective=MEDIUM", "previous=SIMPLE", "proposed=MEDIUM", "source=semantic", "proposed_similarity=", `proposed_matched="an implementation detail question"`},
		},
		{
			requestText:   "another medium request",
			wantTier:      complexity.TierMedium,
			wantMechanism: complexity.MechanismSemantic,
			wantLogParts:  []string{"Session complexity confirmed:", "effective=MEDIUM", "proposed=MEDIUM", "source=semantic", "proposed_similarity=", `proposed_matched="an implementation detail question"`},
		},
		{
			requestText:   "another simple request",
			wantTier:      complexity.TierMedium,
			wantMechanism: complexity.MechanismSession,
			wantLogParts:  []string{"Session complexity held:", "effective=MEDIUM", "proposed=SIMPLE", "source=semantic", "proposed_similarity=", `proposed_matched="a casual greeting"`},
		},
		{
			requestText:   "a complex request",
			wantTier:      complexity.TierComplex,
			wantMechanism: complexity.MechanismSemantic,
			wantLogParts:  []string{"Session complexity escalated:", "effective=COMPLEX", "previous=MEDIUM", "proposed=COMPLEX", "source=semantic", "proposed_similarity=", `proposed_matched="a deep architectural tradeoff analysis"`},
		},
		{
			requestText:   "one more simple request",
			wantTier:      complexity.TierComplex,
			wantMechanism: complexity.MechanismSession,
			wantLogParts:  []string{"Session complexity reused:", "effective=COMPLEX", "reason=complex-ceiling"},
		},
	}

	for _, tt := range tests {
		ctx := complexitySessionContext("session-ladder")
		require.NoError(t, plugin.PreRequestHook(ctx, chatRequest(tt.requestText)))
		require.Equal(t, tt.wantTier, ctx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
		require.Equal(t, tt.wantMechanism, ctx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))

		var sessionLog string
		for _, entry := range ctx.GetRoutingEngineLogs() {
			if strings.HasPrefix(entry.Message, "Session complexity ") {
				sessionLog = entry.Message
				break
			}
		}
		require.NotEmpty(t, sessionLog)
		for _, part := range tt.wantLogParts {
			require.Contains(t, sessionLog, part)
		}
		if strings.Contains(sessionLog, "proposed=") {
			require.NotContains(t, sessionLog, " similarity=", "proposal evidence must not look like evidence for the effective tier")
			require.NotContains(t, sessionLog, " matched=", "proposal evidence must not look like evidence for the effective tier")
		}
	}
}

func TestPreRequestHook_SessionComplexityIsIsolatedBySessionID(t *testing.T) {
	plugin := newSessionComplexityRuleFixture(t)
	plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(sessionAnalyzerConfig()))
	waitForSemanticClassifier(t, plugin)

	complexCtx := complexitySessionContext("session-a")
	require.NoError(t, plugin.PreRequestHook(complexCtx, chatRequest("a complex request")))
	require.Equal(t, complexity.TierComplex, complexCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))

	simpleCtx := complexitySessionContext("session-b")
	require.NoError(t, plugin.PreRequestHook(simpleCtx, chatRequest("a simple request")))
	require.Equal(t, complexity.TierSimple, simpleCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSemantic, simpleCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
}

func TestPreRequestHook_SessionModeWithoutIdentityRemainsPerRequest(t *testing.T) {
	plugin := newSessionComplexityRuleFixture(t)
	plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(sessionAnalyzerConfig()))
	waitForSemanticClassifier(t, plugin)

	complexCtx := complexitySessionContext("")
	require.NoError(t, plugin.PreRequestHook(complexCtx, chatRequest("a complex request")))
	require.Equal(t, complexity.TierComplex, complexCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))

	simpleCtx := complexitySessionContext("")
	require.NoError(t, plugin.PreRequestHook(simpleCtx, chatRequest("a simple request")))
	require.Equal(t, complexity.TierSimple, simpleCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSemantic, simpleCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
}

func TestPreRequestHook_SessionStoreFailureFallsBackToCurrentClassification(t *testing.T) {
	store, err := kvstore.New(kvstore.Config{CleanupInterval: time.Hour})
	require.NoError(t, err)
	plugin := newComplexityRuleFixtureWithConfig(t, &routing.Config{KVStore: store})
	plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(sessionAnalyzerConfig()))
	waitForSemanticClassifier(t, plugin)
	require.NoError(t, store.Close())

	ctx := complexitySessionContext("store-failure")
	require.NoError(t, plugin.PreRequestHook(ctx, chatRequest("a medium request")))
	require.Equal(t, complexity.TierMedium, ctx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSemantic, ctx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
}

func TestPreRequestHook_SessionContinuationReusesOrClassifiesRecoveredTask(t *testing.T) {
	plugin := newSessionComplexityRuleFixture(t)
	plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(sessionAnalyzerConfig()))
	waitForSemanticClassifier(t, plugin)

	continuationRequest := func() *schemas.BifrostRequest {
		return &schemas.BifrostRequest{
			RequestType: schemas.ChatCompletionRequest,
			ChatRequest: &schemas.BifrostChatRequest{
				Provider: schemas.OpenAI,
				Model:    "gpt-4o",
				Input: []schemas.ChatMessage{
					{Role: schemas.ChatMessageRoleUser, Content: chatString("a complex request")},
					{Role: schemas.ChatMessageRoleAssistant, Content: chatString("Calling the tool")},
					{Role: schemas.ChatMessageRoleTool, Content: chatString("Tool result received")},
				},
			},
		}
	}

	absentCtx := complexitySessionContext("new-session")
	require.NoError(t, plugin.PreRequestHook(absentCtx, continuationRequest()))
	require.Equal(t, complexity.TierComplex, absentCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSemantic, absentCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))

	initialCtx := complexitySessionContext("existing-session")
	require.NoError(t, plugin.PreRequestHook(initialCtx, chatRequest("a medium request")))
	require.Equal(t, complexity.TierMedium, initialCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))

	continuationCtx := complexitySessionContext("existing-session")
	require.NoError(t, plugin.PreRequestHook(continuationCtx, continuationRequest()))
	require.Equal(t, complexity.TierMedium, continuationCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSession, continuationCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Nil(t, continuationCtx.Value(schemas.BifrostContextKeyGovernanceComplexityScore))
}

func TestPreRequestHook_ComplexSessionSkipsLaterClassifierCalls(t *testing.T) {
	plugin := newSessionComplexityRuleFixture(t)
	var calls atomic.Int64
	plugin.SetEmbeddingRequestExecutor(func(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
		calls.Add(1)
		return testEmbeddingExecutor(ctx, req)
	})
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(sessionAnalyzerConfig()))
	waitForSemanticClassifier(t, plugin)

	beforeRequest := calls.Load()
	complexCtx := complexitySessionContext("complex-ceiling")
	require.NoError(t, plugin.PreRequestHook(complexCtx, chatRequest("a complex request")))
	require.Equal(t, beforeRequest+1, calls.Load(), "the first human turn should be classified")

	secondCtx := complexitySessionContext("complex-ceiling")
	require.NoError(t, plugin.PreRequestHook(secondCtx, chatRequest("a simple request")))
	require.Equal(t, beforeRequest+1, calls.Load(), "COMPLEX is the ceiling, so later turns need no classifier call")
	require.Equal(t, complexity.TierComplex, secondCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSession, secondCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Nil(t, secondCtx.Value(schemas.BifrostContextKeyGovernanceComplexityScore))
}

func TestPreRequestHook_SemanticComplexityNotReadyLogsInfo(t *testing.T) {
	plugin := newComplexityRuleFixture(t)
	warmupStarted := make(chan struct{}, 1)
	releaseWarmup := make(chan struct{})
	defer close(releaseWarmup)

	plugin.SetEmbeddingRequestExecutor(func(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
		select {
		case warmupStarted <- struct{}{}:
		default:
		}
		<-releaseWarmup
		return testEmbeddingExecutor(ctx, req)
	})
	plugin.ReloadComplexityAnalyzerConfig(&complexity.AnalyzerConfig{
		Keywords: configstore.ComplexityEditableKeywordConfig{
			SimpleKeywords:  []string{"a casual greeting"},
			MediumKeywords:  []string{"an implementation detail question"},
			ComplexKeywords: []string{"a deep architectural tradeoff analysis"},
		},
		Semantic: &configstore.ComplexitySemanticConfig{
			Provider:       "openai",
			EmbeddingModel: "test-embedding-model",
		},
	})

	select {
	case <-warmupStarted:
	case <-time.After(time.Second):
		t.Fatal("semantic warmup did not start")
	}

	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, chatRequest("hey, quick question")))

	logs := bfCtx.GetRoutingEngineLogs()
	var outcome *schemas.RoutingEngineLogEntry
	for i := range logs {
		entry := &logs[i]
		if strings.Contains(entry.Message, "Semantic complexity classification unavailable") {
			outcome = entry
			break
		}
	}
	require.NotNil(t, outcome, "expected semantic no-classification outcome log")
	require.Equal(t, schemas.LogLevelInfo, outcome.Level)
}

// TestPreRequestHook_ComplexityUnsupportedInputRecordsSkippedMechanism pins
// that a request type the extractor cannot read records mechanism=skipped
// instead of silently publishing nothing.
func TestPreRequestHook_ComplexityUnsupportedInputRecordsSkippedMechanism(t *testing.T) {
	plugin := newComplexityRuleFixture(t)

	req := &schemas.BifrostRequest{
		RequestType: schemas.EmbeddingRequest,
		EmbeddingRequest: &schemas.BifrostEmbeddingRequest{
			Provider: schemas.OpenAI,
			Model:    "text-embedding-3-small",
			Input:    []schemas.EmbeddingInputItem{{Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: schemas.Ptr("some text")}}}},
		},
	}
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	mechanism, ok := bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism).(string)
	require.True(t, ok, "skipped mechanism should be recorded for unsupported input")
	require.Equal(t, complexity.MechanismSkipped, mechanism)
}

func TestPreRequestHook_ComplexitySkippedWhenNoRulesReferenceIt(t *testing.T) {
	logger := rules.NewMockLogger()
	provider := "openai"
	model := "gpt-4o-mini"

	ruleStore, err := rules.NewLocalStore(context.Background(), logger, nil)
	require.NoError(t, err)
	require.NoError(t, ruleStore.UpsertRule(context.Background(), &configstoreTables.TableRoutingRule{
		ID:            "rule-1",
		Name:          "Always match",
		CelExpression: "true",
		Targets: []configstoreTables.TableRoutingTarget{
			{Provider: &provider, Model: &model, Weight: 1.0},
		},
		Enabled:  schemas.Ptr(true),
		Scope:    "global",
		Priority: 0,
	}))

	plugin, err := routing.InitFromStore(context.Background(), nil, logger, nil, ruleStore, routing.NewMockGovernance())
	require.NoError(t, err)
	defer func() {
		require.NoError(t, plugin.Cleanup())
	}()

	req := &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-4o",
			Input: []schemas.ChatMessage{
				{
					Role:    schemas.ChatMessageRoleUser,
					Content: chatString("Hello"),
				},
			},
		},
	}

	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	logs := bfCtx.GetRoutingEngineLogs()
	for _, entry := range logs {
		if entry.Engine == schemas.RoutingEngineRoutingRule && strings.Contains(entry.Message, "Complexity") {
			t.Fatalf("expected no complexity logs when no rules reference complexity_tier, got: %s", entry.Message)
		}
	}
}

// jevRecorder is a fake Typesafe decision executor. It answers with the tier
// mapped to the latest user message and records every request it receives.
type jevRecorder struct {
	mu       sync.Mutex
	requests []*schemas.BifrostDecisionRequest
	tiers    map[string]string
	err      *schemas.BifrostError
}

// execute implements routing.DecisionRequestExecutor.
func (r *jevRecorder) execute(_ *schemas.BifrostContext, req *schemas.BifrostDecisionRequest) (*schemas.BifrostDecisionResponse, *schemas.BifrostError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if r.err != nil {
		return nil, r.err
	}
	state, _ := req.State.([]complexity.ConversationMessage)
	tier := "SIMPLE"
	if len(state) > 0 {
		if mapped, ok := r.tiers[state[len(state)-1].Content]; ok {
			tier = mapped
		}
	}
	confidence := 0.9
	return &schemas.BifrostDecisionResponse{
		Model: "jev-1.13.0",
		Answers: map[string]schemas.DecisionAnswer{
			"complexity_tier": {Kind: schemas.DecisionKindChoice, Value: tier, Confidence: &confidence},
		},
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 30, CompletionTokens: 1},
	}, nil
}

// calls returns how many decision requests were made.
func (r *jevRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// lastState returns the conversation state of the most recent decision request.
func (r *jevRecorder) lastState(t *testing.T) []complexity.ConversationMessage {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.requests)
	state, ok := r.requests[len(r.requests)-1].State.([]complexity.ConversationMessage)
	require.True(t, ok)
	return state
}

// jevAnalyzerTestConfig selects Jev as the primary classifier.
func jevAnalyzerTestConfig() *complexity.AnalyzerConfig {
	return &complexity.AnalyzerConfig{
		Classifier: complexity.ClassifierJev,
		Keywords: complexity.EditableKeywordConfig{
			SimpleKeywords:  []string{"papaya amber"},
			MediumKeywords:  []string{"cedar cobalt"},
			ComplexKeywords: []string{"obsidian comet"},
		},
	}
}

// jevFallbackAnalyzerTestConfig is the llm-fallback fixture with Jev as the
// semantic fallback. The llm block stays saved so the tests can prove Jev,
// not the chat classifier, answers.
func jevFallbackAnalyzerTestConfig() *complexity.AnalyzerConfig {
	config := llmFallbackAnalyzerTestConfig()
	config.Semantic.Fallback = configstore.ComplexitySemanticFallbackJev
	return config
}

// jevTestPlugin builds a routing plugin whose one rule sends COMPLEX to
// gpt-4o-mini, with the given analyzer config and Jev fake installed.
func jevTestPlugin(t *testing.T, config *routing.Config, analyzerConfig *complexity.AnalyzerConfig, jev *jevRecorder) *routing.RoutingPlugin {
	t.Helper()
	provider := "openai"
	routeModel := "gpt-4o-mini"
	logger := rules.NewMockLogger()
	ruleStore, err := rules.NewLocalStore(context.Background(), logger, nil)
	require.NoError(t, err)
	require.NoError(t, ruleStore.UpsertRule(context.Background(), &configstoreTables.TableRoutingRule{
		ID:            "jev-complex-rule",
		Name:          "Jev complex route",
		CelExpression: `complexity_tier == "COMPLEX"`,
		Targets: []configstoreTables.TableRoutingTarget{
			{Provider: &provider, Model: &routeModel, Weight: 1.0},
		},
		Enabled:  schemas.Ptr(true),
		Scope:    "global",
		Priority: 0,
	}))
	plugin, err := routing.InitFromStore(context.Background(), config, logger, nil, ruleStore, routing.NewMockGovernance())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
	plugin.SetDecisionRequestExecutor(jev.execute)
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(analyzerConfig))
	return plugin
}

// failOnChatClassifier installs a chat executor that fails the test if the llm classifier runs.
func failOnChatClassifier(t *testing.T, plugin *routing.RoutingPlugin) {
	plugin.SetChatRequestExecutor(func(_ *schemas.BifrostContext, _ *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
		t.Error("the llm classifier must not run when Jev is selected")
		return nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "unexpected llm classifier call"}}
	})
}

// routingLogs joins every routing-engine log message on the context.
func routingLogs(ctx *schemas.BifrostContext) string {
	var messages []string
	for _, entry := range ctx.GetRoutingEngineLogs() {
		messages = append(messages, entry.Message)
	}
	return strings.Join(messages, "\n")
}

// TestPreRequestHook_JevPrimaryPublishesTierAndRoutes proves Jev as the
// primary classifier end to end: its tier drives the routing rule, the
// mechanism records jev, and a saved semantic block stays dormant (no
// warmup, no embedding call) while Jev is selected.
func TestPreRequestHook_JevPrimaryPublishesTierAndRoutes(t *testing.T) {
	var embedCalls atomic.Int64
	analyzerConfig := jevAnalyzerTestConfig()
	analyzerConfig.Semantic = &complexity.SemanticConfig{
		Provider:       schemas.OpenAI,
		EmbeddingModel: "test-embedding-model",
		VectorStore:    configstore.ComplexitySemanticVectorStoreEmbedded,
	}
	jev := &jevRecorder{tiers: map[string]string{"prove the scheduler is deadlock-free": complexity.TierComplex}}
	plugin := jevTestPlugin(t, nil, analyzerConfig, jev)
	plugin.SetEmbeddingRequestExecutor(func(_ *schemas.BifrostContext, _ *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
		embedCalls.Add(1)
		return nil, &schemas.BifrostError{Error: &schemas.ErrorField{Message: "semantic must stay dormant"}}
	})
	failOnChatClassifier(t, plugin)

	req := llmComplexityChatRequest("prove the scheduler is deadlock-free")
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	require.Equal(t, 1, jev.calls())
	require.Equal(t, complexity.TierComplex, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismJev, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Nil(t, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityScore), "Jev has no similarity score to publish")
	_, modelOut, _ := req.GetRequestFields()
	require.Equal(t, "gpt-4o-mini", modelOut)
	require.Contains(t, routingLogs(bfCtx), "Jev complexity: tier=COMPLEX")
	require.Zero(t, embedCalls.Load(), "a saved semantic block must not warm or embed while Jev is primary")
}

// TestPreRequestHook_JevPrimaryLowerTierDoesNotRoute checks that a non-matching
// Jev tier leaves the request model untouched while still publishing the tier.
func TestPreRequestHook_JevPrimaryLowerTierDoesNotRoute(t *testing.T) {
	jev := &jevRecorder{}
	plugin := jevTestPlugin(t, nil, jevAnalyzerTestConfig(), jev)

	req := llmComplexityChatRequest("what is 2 + 2")
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	require.Equal(t, complexity.TierSimple, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	_, modelOut, _ := req.GetRequestFields()
	require.Equal(t, "gpt-4o", modelOut)
}

// TestPreRequestHook_JevReceivesOnlyUserTurns pins extraction into Jev's
// state for both Chat and Responses requests: system prompts and assistant
// replies are never sent, and the configured history window is honoured.
func TestPreRequestHook_JevReceivesOnlyUserTurns(t *testing.T) {
	userRole := schemas.ResponsesInputMessageRoleUser
	assistantRole := schemas.ResponsesInputMessageRoleAssistant
	systemRole := schemas.ResponsesInputMessageRoleSystem
	responsesText := func(text string) *schemas.ResponsesMessageContent {
		return &schemas.ResponsesMessageContent{ContentStr: &text}
	}

	requests := map[string]func() *schemas.BifrostRequest{
		"chat": func() *schemas.BifrostRequest {
			return &schemas.BifrostRequest{
				RequestType: schemas.ChatCompletionRequest,
				ChatRequest: &schemas.BifrostChatRequest{
					Provider: schemas.OpenAI,
					Model:    "gpt-4o",
					Input: []schemas.ChatMessage{
						{Role: schemas.ChatMessageRoleSystem, Content: chatString("you are a helpful bot")},
						{Role: schemas.ChatMessageRoleUser, Content: chatString("first question")},
						{Role: schemas.ChatMessageRoleAssistant, Content: chatString("first answer")},
						{Role: schemas.ChatMessageRoleUser, Content: chatString("second question")},
						{Role: schemas.ChatMessageRoleAssistant, Content: chatString("second answer")},
						{Role: schemas.ChatMessageRoleUser, Content: chatString("current question")},
					},
				},
			}
		},
		"responses": func() *schemas.BifrostRequest {
			return &schemas.BifrostRequest{
				RequestType: schemas.ResponsesRequest,
				ResponsesRequest: &schemas.BifrostResponsesRequest{
					Provider: schemas.OpenAI,
					Model:    "gpt-4o",
					Input: []schemas.ResponsesMessage{
						{Role: &systemRole, Content: responsesText("you are a helpful bot")},
						{Role: &userRole, Content: responsesText("first question")},
						{Role: &assistantRole, Content: responsesText("first answer")},
						{Role: &userRole, Content: responsesText("second question")},
						{Role: &assistantRole, Content: responsesText("second answer")},
						{Role: &userRole, Content: responsesText("current question")},
					},
				},
			}
		},
	}

	for name, build := range requests {
		t.Run(name, func(t *testing.T) {
			count := 1
			analyzerConfig := jevAnalyzerTestConfig()
			analyzerConfig.Jev = &complexity.JevConfig{PreviousMessageCount: &count}
			jev := &jevRecorder{}
			plugin := jevTestPlugin(t, nil, analyzerConfig, jev)

			require.NoError(t, plugin.PreRequestHook(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), build()))

			require.Equal(t, []complexity.ConversationMessage{
				{Role: "user", Content: "second question"},
				{Role: "user", Content: "current question"},
			}, jev.lastState(t))
		})
	}
}

// TestPreRequestHook_JevPrimaryFailurePublishesNoTier checks that a failed Jev
// call leaves the request on its original model, records mechanism=skipped,
// and never cascades into the llm classifier, even when an llm block is saved.
func TestPreRequestHook_JevPrimaryFailurePublishesNoTier(t *testing.T) {
	analyzerConfig := jevAnalyzerTestConfig()
	analyzerConfig.LLM = &complexity.LLMConfig{Provider: schemas.OpenAI, Model: "test-classifier-model", Timeout: time.Second}
	jev := &jevRecorder{err: &schemas.BifrostError{Error: &schemas.ErrorField{Message: "no keys found that support model: jev-latest"}}}
	plugin := jevTestPlugin(t, nil, analyzerConfig, jev)
	failOnChatClassifier(t, plugin)

	req := llmComplexityChatRequest("prove the scheduler is deadlock-free")
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	require.Equal(t, 1, jev.calls())
	require.Nil(t, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSkipped, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	_, modelOut, _ := req.GetRequestFields()
	require.Equal(t, "gpt-4o", modelOut)
	require.Contains(t, routingLogs(bfCtx), "no keys found that support model: jev-latest")
}

// TestPreRequestHook_SemanticFallsBackToJev proves the fallback flow: a
// confident semantic match never reaches Jev, a rejection is handed to Jev,
// and Jev wins over a saved llm block because only the selected fallback runs.
func TestPreRequestHook_SemanticFallsBackToJev(t *testing.T) {
	analyzerConfig := jevFallbackAnalyzerTestConfig()
	analyzerConfig.Semantic.MinSimilarity = 0.9
	jev := &jevRecorder{tiers: map[string]string{"prove the scheduler is deadlock-free": complexity.TierComplex}}
	plugin := jevTestPlugin(t, nil, analyzerConfig, jev)
	failOnChatClassifier(t, plugin)
	installSemanticEmbeddingFake(t, plugin)

	semanticCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(semanticCtx, llmComplexityChatRequest("papaya amber")))
	require.Equal(t, complexity.MechanismSemantic, semanticCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Zero(t, jev.calls(), "a confident semantic answer must not consult Jev")

	fallbackReq := llmComplexityChatRequest("prove the scheduler is deadlock-free")
	fallbackCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(fallbackCtx, fallbackReq))

	require.Equal(t, 1, jev.calls())
	require.Equal(t, complexity.TierComplex, fallbackCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismJev, fallbackCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Nil(t, fallbackCtx.Value(schemas.BifrostContextKeyGovernanceComplexityScore), "the rejected semantic score must not leak onto a Jev decision")
	_, modelOut, _ := fallbackReq.GetRequestFields()
	require.Equal(t, "gpt-4o-mini", modelOut)
	logs := routingLogs(fallbackCtx)
	require.Contains(t, logs, "below min_similarity")
	require.Contains(t, logs, "falling back to Jev")
	require.Contains(t, logs, "Jev complexity: tier=COMPLEX")
}

// TestPreRequestHook_JevFallbackCoversSemanticWarmup covers the startup gap:
// while semantic exemplars are still warming, Jev classifies instead of
// every request going unrouted.
func TestPreRequestHook_JevFallbackCoversSemanticWarmup(t *testing.T) {
	warmupStarted := make(chan struct{}, 1)
	releaseWarmup := make(chan struct{})
	defer close(releaseWarmup)

	jev := &jevRecorder{tiers: map[string]string{"prove the scheduler is deadlock-free": complexity.TierComplex}}
	plugin := jevTestPlugin(t, nil, jevAnalyzerTestConfig(), jev)
	plugin.SetEmbeddingRequestExecutor(func(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
		select {
		case warmupStarted <- struct{}{}:
		default:
		}
		<-releaseWarmup
		return testEmbeddingExecutor(ctx, req)
	})
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(jevFallbackAnalyzerTestConfig()))
	failOnChatClassifier(t, plugin)

	select {
	case <-warmupStarted:
	case <-time.After(time.Second):
		t.Fatal("semantic warmup did not start")
	}

	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, llmComplexityChatRequest("prove the scheduler is deadlock-free")))

	require.Equal(t, 1, jev.calls())
	require.Equal(t, complexity.TierComplex, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismJev, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Contains(t, routingLogs(bfCtx), "falling back to Jev")
}

// TestPreRequestHook_JevFallbackFailureDoesNotCascade checks that when both
// semantic and its Jev fallback produce nothing, no tier is published and the
// saved llm block is not tried as a third classifier.
func TestPreRequestHook_JevFallbackFailureDoesNotCascade(t *testing.T) {
	analyzerConfig := jevFallbackAnalyzerTestConfig()
	analyzerConfig.Semantic.MinSimilarity = 0.9
	jev := &jevRecorder{err: &schemas.BifrostError{Error: &schemas.ErrorField{Message: "typesafe unavailable"}}}
	plugin := jevTestPlugin(t, nil, analyzerConfig, jev)
	failOnChatClassifier(t, plugin)
	installSemanticEmbeddingFake(t, plugin)

	req := llmComplexityChatRequest("prove the scheduler is deadlock-free")
	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, req))

	require.Equal(t, 1, jev.calls())
	require.Nil(t, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSkipped, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	_, modelOut, _ := req.GetRequestFields()
	require.Equal(t, "gpt-4o", modelOut)
}

// TestPreRequestHook_SemanticWithoutJevFallbackNeverCallsJev checks that a
// wired decision executor alone never runs Jev: semantic with fallback "none"
// publishes nothing on a rejection.
func TestPreRequestHook_SemanticWithoutJevFallbackNeverCallsJev(t *testing.T) {
	analyzerConfig := llmFallbackAnalyzerTestConfig()
	analyzerConfig.Semantic.Fallback = configstore.ComplexitySemanticFallbackNone
	analyzerConfig.Semantic.MinSimilarity = 0.9
	jev := &jevRecorder{tiers: map[string]string{"prove the scheduler is deadlock-free": complexity.TierComplex}}
	plugin := jevTestPlugin(t, nil, analyzerConfig, jev)
	failOnChatClassifier(t, plugin)
	installSemanticEmbeddingFake(t, plugin)

	bfCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(bfCtx, llmComplexityChatRequest("prove the scheduler is deadlock-free")))

	require.Zero(t, jev.calls())
	require.Nil(t, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityTier))
	require.Equal(t, complexity.MechanismSkipped, bfCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
}

// TestPreRequestHook_SwitchingClassifierTakesEffect covers the UI toggle: a
// reload from Jev to semantic re-arms semantic warmup and stops Jev calls,
// and a reload back to Jev stops embedding requests.
func TestPreRequestHook_SwitchingClassifierTakesEffect(t *testing.T) {
	jev := &jevRecorder{}
	plugin := jevTestPlugin(t, nil, jevAnalyzerTestConfig(), jev)

	jevCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(jevCtx, llmComplexityChatRequest("papaya amber")))
	require.Equal(t, complexity.MechanismJev, jevCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Equal(t, 1, jev.calls())

	semanticConfig := llmFallbackAnalyzerTestConfig()
	semanticConfig.Semantic.Fallback = configstore.ComplexitySemanticFallbackNone
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(semanticConfig))
	installSemanticEmbeddingFake(t, plugin)

	semanticCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(semanticCtx, llmComplexityChatRequest("papaya amber")))
	require.Equal(t, complexity.MechanismSemantic, semanticCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Equal(t, 1, jev.calls(), "Jev must stop being called once semantic is primary")

	var embedCalls atomic.Int64
	plugin.SetEmbeddingRequestExecutor(func(ctx *schemas.BifrostContext, req *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
		embedCalls.Add(1)
		return testEmbeddingExecutor(ctx, req)
	})
	require.NoError(t, plugin.ReloadComplexityAnalyzerConfig(jevAnalyzerTestConfig()))
	backCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, plugin.PreRequestHook(backCtx, llmComplexityChatRequest("papaya amber")))
	require.Equal(t, complexity.MechanismJev, backCtx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism))
	require.Equal(t, 2, jev.calls())
	require.Zero(t, embedCalls.Load(), "switching back to Jev must not embed the request")
}

// TestPreRequestHook_JevSessionOnlyEscalates proves session routing with Jev
// as the primary classifier and no semantic block: lower Jev proposals are
// held at the session tier, COMPLEX escalates, and the COMPLEX ceiling skips
// further Jev calls entirely.
func TestPreRequestHook_JevSessionOnlyEscalates(t *testing.T) {
	store, err := kvstore.New(kvstore.Config{CleanupInterval: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	analyzerConfig := jevAnalyzerTestConfig()
	analyzerConfig.Session = &complexity.SessionConfig{Enabled: true}
	jev := &jevRecorder{tiers: map[string]string{
		"a medium request":  complexity.TierMedium,
		"a simple request":  complexity.TierSimple,
		"a complex request": complexity.TierComplex,
	}}
	plugin := jevTestPlugin(t, &routing.Config{KVStore: store}, analyzerConfig, jev)

	steps := []struct {
		text          string
		wantTier      string
		wantMechanism string
		wantJevCalls  int
		wantLogParts  []string
	}{
		{"a medium request", complexity.TierMedium, complexity.MechanismJev, 1, []string{"Session complexity initialized:", "source=jev", "proposed_confidence=0.90"}},
		{"a simple request", complexity.TierMedium, complexity.MechanismSession, 2, []string{"Session complexity held:", "effective=MEDIUM", "proposed=SIMPLE", "source=jev"}},
		{"a complex request", complexity.TierComplex, complexity.MechanismJev, 3, []string{"Session complexity escalated:", "previous=MEDIUM", "source=jev"}},
		{"a simple request", complexity.TierComplex, complexity.MechanismSession, 3, []string{"reason=complex-ceiling"}},
	}
	for _, step := range steps {
		ctx := complexitySessionContext("jev-session")
		require.NoError(t, plugin.PreRequestHook(ctx, chatRequest(step.text)))
		require.Equal(t, step.wantTier, ctx.Value(schemas.BifrostContextKeyGovernanceComplexityTier), step.text)
		require.Equal(t, step.wantMechanism, ctx.Value(schemas.BifrostContextKeyGovernanceComplexityMechanism), step.text)
		require.Equal(t, step.wantJevCalls, jev.calls(), step.text)
		logs := routingLogs(ctx)
		for _, part := range step.wantLogParts {
			require.Contains(t, logs, part, step.text)
		}
	}
}

// TestValidateComplexityAnalyzerConfig_Jev pins save-time validation: Jev as
// primary or fallback needs the decision executor, and Jev as primary does
// not need the embedding executor even with a semantic block saved.
func TestValidateComplexityAnalyzerConfig_Jev(t *testing.T) {
	newPlugin := func(t *testing.T) *routing.RoutingPlugin {
		t.Helper()
		logger := rules.NewMockLogger()
		ruleStore, err := rules.NewLocalStore(context.Background(), logger, nil)
		require.NoError(t, err)
		plugin, err := routing.InitFromStore(context.Background(), nil, logger, nil, ruleStore, routing.NewMockGovernance())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
		return plugin
	}

	t.Run("jev primary without executor", func(t *testing.T) {
		err := newPlugin(t).ValidateComplexityAnalyzerConfig(jevAnalyzerTestConfig())
		require.ErrorContains(t, err, "Jev complexity decision executor is unavailable")
	})

	t.Run("jev fallback without executor", func(t *testing.T) {
		plugin := newPlugin(t)
		plugin.SetEmbeddingRequestExecutor(testEmbeddingExecutor)
		err := plugin.ValidateComplexityAnalyzerConfig(jevFallbackAnalyzerTestConfig())
		require.ErrorContains(t, err, "Jev complexity decision executor is unavailable")
	})

	t.Run("jev primary ignores a saved semantic block", func(t *testing.T) {
		plugin := newPlugin(t)
		plugin.SetDecisionRequestExecutor((&jevRecorder{}).execute)
		config := jevAnalyzerTestConfig()
		config.Semantic = &complexity.SemanticConfig{Provider: schemas.OpenAI, EmbeddingModel: "test-embedding-model"}
		require.NoError(t, plugin.ValidateComplexityAnalyzerConfig(config))
	})

	t.Run("jev session without semantic block", func(t *testing.T) {
		store, err := kvstore.New(kvstore.Config{CleanupInterval: time.Hour})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		logger := rules.NewMockLogger()
		ruleStore, err := rules.NewLocalStore(context.Background(), logger, nil)
		require.NoError(t, err)
		plugin, err := routing.InitFromStore(context.Background(), &routing.Config{KVStore: store}, logger, nil, ruleStore, routing.NewMockGovernance())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })
		plugin.SetDecisionRequestExecutor((&jevRecorder{}).execute)

		config := jevAnalyzerTestConfig()
		config.Session = &complexity.SessionConfig{Enabled: true}
		require.NoError(t, plugin.ValidateComplexityAnalyzerConfig(config))
	})
}
