package configstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/framework/queryscope"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupRDBTestStore creates an in-memory SQLite database and returns an RDBConfigStore for testing
func setupRDBTestStore(t *testing.T) *RDBConfigStore {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err, "Failed to create test database")

	// Run migrations for all tables
	err = db.AutoMigrate(
		&tables.TableProvider{},
		&tables.TableKey{},
		&tables.TableBudget{},
		&tables.TableRateLimit{},
		&tables.TableModelConfig{},
		&tables.TableRoutingRule{},
		&tables.TableRoutingTarget{},
		&tables.TablePricingOverride{},
		&tables.TableVirtualKey{},
		&tables.TableVirtualKeyProviderConfig{},
		&tables.TableVirtualKeyProviderConfigKey{},
		&tables.TableModelConfig{},
		&tables.TableCustomer{},
		&tables.TableTeam{},
		&tables.TableClientConfig{},
		&tables.TableGovernanceConfig{},
		&tables.TablePlugin{},
		&tables.TableMCPClient{},
		&tables.TableMCPLibrary{},
		&tables.TableVirtualKeyMCPConfig{},
		&tables.TableFolder{},
		&tables.TablePrompt{},
		&tables.TablePromptVersion{},
		&tables.TablePromptVersionMessage{},
		&tables.TablePromptSession{},
		&tables.TablePromptSessionMessage{},
		&tables.TableOauthUserSession{},
		&tables.TableOauthUserToken{},
		&tables.TableOauthToken{},
		&tables.TableMCPOauthToken{},
		&tables.TableMCPOauthFlow{},
		&tables.TableMCPPerUserHeaderCredential{},
		&tables.TableMCPPerUserHeaderFlow{},
		&tables.TableOAuth2RefreshToken{},
		&tables.TableWebhookEndpoint{},
		&tables.TableWebhookJob{},
	)
	require.NoError(t, err, "Failed to migrate test database")
	require.NoError(t, migrationAddAgentGatewayTables(context.Background(), db, testMigrationLogger), "Failed to migrate Agent Gateway tables")

	// Virtual MCP tables (separate call: the in-batch AutoMigrate above does not create
	// enterprise_mcp_tool_groups reliably). MCP client create now cross-checks slugs against them.
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualMCP{}, &tables.TableVirtualKeyVirtualMCP{}), "Failed to migrate virtual MCP tables")

	// Setup join table
	err = db.SetupJoinTable(&tables.TableVirtualKeyProviderConfig{}, "Keys", &tables.TableVirtualKeyProviderConfigKey{})
	require.NoError(t, err, "Failed to setup join table")

	s := &RDBConfigStore{logger: nil}
	s.db.Store(db)
	s.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, s.DB())
	}
	s.refreshPoolFn = func(ctx context.Context) error { return nil }
	return s
}

func testComplexityAnalyzerConfig() *ComplexityAnalyzerConfig {
	return &ComplexityAnalyzerConfig{
		TierBoundaries: ComplexityTierBoundaries{
			SimpleMedium:  0.10,
			MediumComplex: 0.30,
		},
		Keywords: ComplexityEditableKeywordConfig{
			SimpleKeywords:  []string{"hello"},
			MediumKeywords:  []string{" Function ", "api", "API", "latency"},
			ComplexKeywords: []string{"tradeoffs"},
		},
	}
}

func TestRDBConfigStore_UpsertModelPricesSyncsIsDeprecated(t *testing.T) {
	store := setupRDBTestStore(t)
	require.NoError(t, store.DB().AutoMigrate(&tables.TableModelPricing{}))
	ctx := context.Background()

	require.NoError(t, store.UpsertModelPrices(ctx, &tables.TableModelPricing{
		Model:        "deprecated-model",
		Provider:     "openai",
		Mode:         "chat",
		IsDeprecated: true,
	}))

	prices, err := store.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.True(t, prices[0].IsDeprecated)

	require.NoError(t, store.UpsertModelPrices(ctx, &tables.TableModelPricing{
		Model:        "deprecated-model",
		Provider:     "openai",
		Mode:         "chat",
		IsDeprecated: false,
	}))

	prices, err = store.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.False(t, prices[0].IsDeprecated)
}

func TestRDBConfigStore_ComplexityAnalyzerConfigRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := testComplexityAnalyzerConfig()
	cfg.ConfigHashes = ComplexityAnalyzerConfigHashes{
		TierBoundaries:  "tier-hash-1",
		SimpleKeywords:  "simple-hash-1",
		MediumKeywords:  "medium-hash-1",
		ComplexKeywords: "complex-hash-1",
	}
	require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, cfg))

	got, err := store.GetComplexityAnalyzerConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, ComplexityTierBoundaries{
		SimpleMedium:  0.10,
		MediumComplex: 0.30,
	}, got.TierBoundaries)
	assert.Equal(t, []string{"api", "function", "latency"}, got.Keywords.MediumKeywords)
	assert.Equal(t, cfg.ConfigHashes, got.ConfigHashes)
}

// TestRDBConfigStore_GetComplexityAnalyzerConfigMarksUnreadableRows pins that a
// stored config this version cannot run is reported as unreadable rather than as
// a plain error. The API handler degrades to defaults on that distinction and
// still fails on anything else, so losing it here turns a recoverable page into
// a 500 without any test noticing.
func TestRDBConfigStore_GetComplexityAnalyzerConfigMarksUnreadableRows(t *testing.T) {
	ctx := context.Background()

	t.Run("analyzer row", func(t *testing.T) {
		store := setupRDBTestStore(t)
		require.NoError(t, store.UpdateConfig(ctx, &tables.TableGovernanceConfig{
			Key:   tables.ConfigComplexityAnalyzerConfigKey,
			Value: `{"tier_boundaries":{"simple_medium":0.9,"medium_complex":0.1}}`,
		}))

		_, err := store.GetComplexityAnalyzerConfig(ctx)
		require.ErrorIs(t, err, ErrConfigUnreadable)
	})

	t.Run("semantic row", func(t *testing.T) {
		store := setupRDBTestStore(t)
		require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, testComplexityAnalyzerConfig()))
		require.NoError(t, store.UpdateConfig(ctx, &tables.TableGovernanceConfig{
			Key:   tables.ConfigComplexitySemanticConfigKey,
			Value: `{"keywords":`,
		}))

		_, err := store.GetComplexityAnalyzerConfig(ctx)
		require.ErrorIs(t, err, ErrConfigUnreadable)
	})

	t.Run("rows that do not combine into a runnable config", func(t *testing.T) {
		store := setupRDBTestStore(t)
		require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, testComplexityAnalyzerConfig()))
		// Each row is well formed on its own; the pair is not, because one
		// phrase claims two tiers. The semantic block has to be present for the
		// phrase rules to apply at all — without a classifier to embed them,
		// the lists are just unused strings.
		require.NoError(t, store.UpdateConfig(ctx, &tables.TableGovernanceConfig{
			Key: tables.ConfigComplexitySemanticConfigKey,
			Value: `{"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small"},` +
				`"keywords":{"simple_keywords":["shared phrase"],` +
				`"medium_keywords":["shared phrase"],"complex_keywords":["complex"]}}`,
		}))

		_, err := store.GetComplexityAnalyzerConfig(ctx)
		require.ErrorIs(t, err, ErrConfigUnreadable)
	})

	t.Run("stored semantic config exceeds phrase limit", func(t *testing.T) {
		store := setupRDBTestStore(t)
		base := testComplexityAnalyzerConfig()
		require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, base))

		oversized := *base
		oversized.Semantic = &ComplexitySemanticConfig{
			Provider:       "openai",
			EmbeddingModel: "text-embedding-3-small",
		}
		oversized.Keywords.SimpleKeywords = make([]string, MaxComplexitySemanticPhrases-1)
		for index := range oversized.Keywords.SimpleKeywords {
			oversized.Keywords.SimpleKeywords[index] = fmt.Sprintf("simple-%d", index)
		}
		oversized.Keywords.MediumKeywords = []string{"medium"}
		oversized.Keywords.ComplexKeywords = []string{"complex"}
		semanticRaw, err := encodeComplexitySemanticConfigRow(oversized)
		require.NoError(t, err)
		require.NoError(t, store.UpdateConfig(ctx, &tables.TableGovernanceConfig{
			Key:   tables.ConfigComplexitySemanticConfigKey,
			Value: string(semanticRaw),
		}))

		_, err = store.GetComplexityAnalyzerConfig(ctx)
		require.ErrorIs(t, err, ErrConfigUnreadable)
		require.ErrorContains(t, err, "contains 751 phrases")
	})
}

func TestRDBConfigStore_GetComplexityAnalyzerConfigMissingReturnsNil(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	got, err := store.GetComplexityAnalyzerConfig(ctx)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRDBConfigStore_UpdateComplexityAnalyzerConfigPreservesExistingHashesOnRuntimeUpdate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	fileConfig := testComplexityAnalyzerConfig()
	fileConfig.ConfigHashes = ComplexityAnalyzerConfigHashes{
		TierBoundaries:  "tier-hash-1",
		SimpleKeywords:  "simple-hash-1",
		MediumKeywords:  "medium-hash-1",
		ComplexKeywords: "complex-hash-1",
	}
	require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, fileConfig))

	runtimeConfig := testComplexityAnalyzerConfig()
	runtimeConfig.TierBoundaries.SimpleMedium = 0.12
	runtimeConfig.ConfigHashes = ComplexityAnalyzerConfigHashes{}
	require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, runtimeConfig))

	got, err := store.GetComplexityAnalyzerConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 0.12, got.TierBoundaries.SimpleMedium)
	assert.Equal(t, fileConfig.ConfigHashes, got.ConfigHashes)
}

func TestGenerateComplexityAnalyzerConfigHashesCanonicalizesKeywords(t *testing.T) {
	left := testComplexityAnalyzerConfig()
	right := testComplexityAnalyzerConfig()
	right.Keywords.MediumKeywords = []string{"api", "function", "latency"}
	left.ConfigHashes = ComplexityAnalyzerConfigHashes{MediumKeywords: "stored-medium-hash-a"}
	right.ConfigHashes = ComplexityAnalyzerConfigHashes{MediumKeywords: "stored-medium-hash-b"}

	leftHashes, err := GenerateComplexityAnalyzerConfigHashes(left)
	require.NoError(t, err)
	rightHashes, err := GenerateComplexityAnalyzerConfigHashes(right)
	require.NoError(t, err)

	assert.Equal(t, leftHashes, rightHashes)
}

func TestMergeComplexityAnalyzerConfigAddsKeywordsAndOverlaysBoundaries(t *testing.T) {
	base := testComplexityAnalyzerConfig()
	file := testComplexityAnalyzerConfig()
	file.TierBoundaries = ComplexityTierBoundaries{
		SimpleMedium:  0.20,
		MediumComplex: 0.40,
	}
	file.Keywords.MediumKeywords = []string{"GraphQL", "api", "latency", "kubernetes"}
	file.Keywords.ComplexKeywords = []string{"tradeoffs", "step by step"}
	file.Keywords.SimpleKeywords = []string{"hello", "thanks"}

	merged, err := MergeComplexityAnalyzerConfig(base, file)
	require.NoError(t, err)
	require.NotNil(t, merged)

	assert.Equal(t, file.TierBoundaries, merged.TierBoundaries)
	assert.Equal(t, []string{"api", "function", "graphql", "kubernetes", "latency"}, merged.Keywords.MediumKeywords)
	assert.Equal(t, []string{"step by step", "tradeoffs"}, merged.Keywords.ComplexKeywords)
	assert.Equal(t, []string{"hello", "thanks"}, merged.Keywords.SimpleKeywords)
}

func TestMergeComplexityAnalyzerConfigByHashesOnlyAppliesChangedSections(t *testing.T) {
	base := testComplexityAnalyzerConfig()
	base.ConfigHashes = ComplexityAnalyzerConfigHashes{
		TierBoundaries:  "tier-hash-1",
		SimpleKeywords:  "simple-hash-1",
		MediumKeywords:  "medium-hash-1",
		ComplexKeywords: "complex-hash-1",
	}
	base.TierBoundaries.SimpleMedium = 0.12
	base.Keywords.MediumKeywords = []string{"ui-medium"}
	base.Keywords.ComplexKeywords = []string{"ui-complex"}

	file := testComplexityAnalyzerConfig()
	file.ConfigHashes = base.ConfigHashes
	file.ConfigHashes.MediumKeywords = "medium-hash-2"
	file.TierBoundaries.SimpleMedium = 0.20
	file.Keywords.MediumKeywords = []string{"file-medium"}
	file.Keywords.ComplexKeywords = []string{"file-complex"}

	merged, err := MergeComplexityAnalyzerConfigByHashes(base, file)
	require.NoError(t, err)
	require.NotNil(t, merged)

	assert.Equal(t, 0.12, merged.TierBoundaries.SimpleMedium)
	assert.Equal(t, []string{"file-medium", "ui-medium"}, merged.Keywords.MediumKeywords)
	assert.Equal(t, []string{"ui-complex"}, merged.Keywords.ComplexKeywords)
	assert.Equal(t, "medium-hash-2", merged.ConfigHashes.MediumKeywords)
	assert.Equal(t, "complex-hash-1", merged.ConfigHashes.ComplexKeywords)
}

func TestRDBConfigStore_GetGovernanceConfigIncludesComplexityAnalyzerConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	cfg := testComplexityAnalyzerConfig()
	cfg.ConfigHashes = ComplexityAnalyzerConfigHashes{
		TierBoundaries:  "tier-hash-2",
		SimpleKeywords:  "simple-hash-2",
		MediumKeywords:  "medium-hash-2",
		ComplexKeywords: "complex-hash-2",
	}
	require.NoError(t, store.UpdateComplexityAnalyzerConfig(ctx, cfg))

	governanceConfig, err := store.GetGovernanceConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, governanceConfig)
	require.NotNil(t, governanceConfig.ComplexityAnalyzerConfig)
	assert.Equal(t, 0.30, governanceConfig.ComplexityAnalyzerConfig.TierBoundaries.MediumComplex)
	assert.Equal(t, cfg.ConfigHashes, governanceConfig.ComplexityAnalyzerConfig.ConfigHashes)
}

func TestRDBConfigStore_UpdateComplexityAnalyzerConfigRejectsInvalidConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		mutate func(*ComplexityAnalyzerConfig)
	}{
		{
			name: "simple medium below minimum",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.TierBoundaries.SimpleMedium = -0.1
			},
		},
		{
			name: "simple medium at minimum",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.TierBoundaries.SimpleMedium = 0
			},
		},
		{
			name: "medium complex at minimum",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.TierBoundaries.MediumComplex = 0
			},
		},
		{
			name: "medium complex at maximum",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.TierBoundaries.MediumComplex = 1.0
			},
		},
		{
			name: "boundaries out of order",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.TierBoundaries.MediumComplex = cfg.TierBoundaries.SimpleMedium - 0.05
			},
		},
		{
			name: "boundaries equal",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.TierBoundaries.MediumComplex = cfg.TierBoundaries.SimpleMedium
			},
		},
		{
			name: "empty medium keywords",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.Keywords.MediumKeywords = nil
			},
		},
		{
			name: "empty complex keywords",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.Keywords.ComplexKeywords = nil
			},
		},
		{
			name: "empty simple keywords",
			mutate: func(cfg *ComplexityAnalyzerConfig) {
				cfg.Keywords.SimpleKeywords = nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invalid := testComplexityAnalyzerConfig()
			tt.mutate(invalid)

			err := store.UpdateComplexityAnalyzerConfig(ctx, invalid)
			require.Error(t, err)
		})
	}
}

func TestUpsertMCPLibraryEntry(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	entry := &tables.TableMCPLibrary{
		Slug:           "filesystem",
		Name:           "Filesystem",
		Description:    "original",
		ConnectionType: schemas.MCPConnectionTypeSTDIO,
		AuthType:       schemas.MCPAuthTypeNone,
		Source:         "remote",
	}
	require.NoError(t, store.UpsertMCPLibraryEntry(ctx, entry))

	entry.Description = "updated"
	require.NoError(t, store.UpsertMCPLibraryEntry(ctx, entry))

	entries, totalCount, err := store.GetMCPLibraryPaginated(ctx, MCPLibraryQueryParams{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, int64(1), totalCount)
	require.Len(t, entries, 1)
	require.Equal(t, "updated", entries[0].Description)
}

func TestValidateSkillVersionIncrementRequiresGreaterVersion(t *testing.T) {
	tests := []struct {
		name    string
		latest  string
		next    string
		wantErr bool
	}{
		{name: "rejects lower prerelease core", latest: "1.0.3", next: "1.0.2-1", wantErr: true},
		{name: "accepts same core with suffix after release", latest: "1.0.3", next: "1.0.3-1", wantErr: false},
		{name: "accepts release after same core suffix", latest: "1.0.3-beta1", next: "1.0.3", wantErr: false},
		{name: "accepts higher patch", latest: "1.0.3", next: "1.0.4", wantErr: false},
		{name: "accepts higher minor", latest: "1.0.3", next: "1.1.0", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSkillVersionIncrement(tt.latest, tt.next)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLatestCreatedSkillVersionUsesCreationOrder(t *testing.T) {
	store := setupRDBTestStore(t)
	err := store.DB().AutoMigrate(
		&tables.TableSkill{},
		&tables.TableSkillVersion{},
		&tables.TableSkillFile{},
		&tables.TableSkillFileBlob{},
	)
	require.NoError(t, err)
	ctx := context.Background()
	baseTime := time.Now()

	skillID := "skill-latest-created"
	err = store.DB().Create(&tables.TableSkill{
		ID:            skillID,
		Name:          "latest-created",
		Description:   "Latest created version test",
		SkillMDBody:   "body",
		LatestVersion: "1.0.3",
		CreatedAt:     baseTime,
		UpdatedAt:     baseTime,
	}).Error
	require.NoError(t, err)
	err = store.DB().Create(&tables.TableSkillVersion{
		ID:                  "skill-version-old",
		SkillID:             skillID,
		Version:             "1.0.3",
		SkillMDBody:         "body",
		FrontmatterSnapshot: tables.SkillJSONMap{"name": "latest-created", "description": "Latest created version test"},
		CreatedAt:           baseTime,
	}).Error
	require.NoError(t, err)
	err = store.DB().Create(&tables.TableSkillVersion{
		ID:                  "skill-version-new",
		SkillID:             skillID,
		Version:             "1.0.2-1",
		SkillMDBody:         "body",
		FrontmatterSnapshot: tables.SkillJSONMap{"name": "latest-created", "description": "Latest created version test"},
		CreatedAt:           baseTime.Add(time.Minute),
	}).Error
	require.NoError(t, err)

	latest, err := latestCreatedSkillVersion(store.DB(), skillID)
	require.NoError(t, err)
	assert.Equal(t, "1.0.2-1", latest)

	skill, err := store.GetSkillLean(ctx, skillID)
	require.NoError(t, err)
	assert.Equal(t, "1.0.2-1", skill.HighestVersion)
}

func setupSkillTestStore(t *testing.T) *RDBConfigStore {
	store := setupRDBTestStore(t)
	err := store.DB().AutoMigrate(
		&tables.TableSkill{},
		&tables.TableSkillVersion{},
		&tables.TableSkillFile{},
		&tables.TableSkillFileBlob{},
	)
	require.NoError(t, err)
	return store
}

func skillURLFile(path string) tables.TableSkillFile {
	sourceURL := "https://example.com/" + path
	return tables.TableSkillFile{Path: path, SourceType: tables.SkillSourceTypeURL, SourceURL: &sourceURL}
}

// The config-driven skills registry passes a freshly built skill whose
// LatestVersion is empty, so serving the new version must not depend on it.
func TestUpdateSkillServesNewVersionWhenLatestVersionIsUnset(t *testing.T) {
	store := setupSkillTestStore(t)
	ctx := context.Background()

	created := &tables.TableSkill{
		Name:        "config-update",
		Description: "first",
		SkillMDBody: "body 1",
		Files:       []tables.TableSkillFile{skillURLFile("references/v1.md")},
	}
	require.NoError(t, store.CreateSkill(ctx, created, "1.0.0", nil))

	updated := &tables.TableSkill{
		ID:          created.ID,
		Name:        created.Name,
		Description: "second",
		SkillMDBody: "body 2",
		Files:       []tables.TableSkillFile{skillURLFile("references/v2.md")},
	}
	require.NoError(t, store.UpdateSkill(ctx, updated, "1.1.0", true, nil))

	assert.Equal(t, "1.1.0", updated.LatestVersion)
	require.Len(t, updated.Files, 1)
	assert.Equal(t, "references/v2.md", updated.Files[0].Path)

	skill, err := store.GetSkill(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "1.1.0", skill.LatestVersion)
	assert.Equal(t, "body 2", skill.SkillMDBody)
	require.Len(t, skill.Files, 1)
	assert.Equal(t, "references/v2.md", skill.Files[0].Path)
}

func TestUpdateSkillWithoutServeKeepsServingVersion(t *testing.T) {
	store := setupSkillTestStore(t)
	ctx := context.Background()

	created := &tables.TableSkill{
		Name:        "config-draft",
		Description: "first",
		SkillMDBody: "body 1",
		Files:       []tables.TableSkillFile{skillURLFile("references/v1.md")},
	}
	require.NoError(t, store.CreateSkill(ctx, created, "1.0.0", nil))

	updated := &tables.TableSkill{
		ID:          created.ID,
		Name:        created.Name,
		Description: "second",
		SkillMDBody: "body 2",
		Files:       []tables.TableSkillFile{skillURLFile("references/v2.md")},
	}
	require.NoError(t, store.UpdateSkill(ctx, updated, "1.1.0", false, nil))

	assert.Equal(t, "1.0.0", updated.LatestVersion)
	require.Len(t, updated.Files, 1)
	assert.Equal(t, "references/v1.md", updated.Files[0].Path)

	skill, err := store.GetSkill(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", skill.LatestVersion)
	assert.Equal(t, "body 1", skill.SkillMDBody)

	version, err := store.GetSkillVersion(ctx, created.ID, "1.1.0")
	require.NoError(t, err)
	assert.Equal(t, "body 2", version.SkillMDBody)
}

// =============================================================================
// Provider and Key Tests
// =============================================================================

func TestUpdateProvidersConfig_CreateNew(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{
					ID:     "key-uuid-1",
					Name:   "openai-primary",
					Value:  *schemas.NewSecretVar("sk-test-key"),
					Weight: 1.0,
				},
			},
		},
	}

	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Verify provider was created
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Contains(t, result, schemas.ModelProvider("openai"))
	assert.Len(t, result["openai"].Keys, 1)
	assert.Equal(t, "openai-primary", result["openai"].Keys[0].Name)
}

func TestUpdateProvidersConfig_UpdateExistingByKeyID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create initial provider with key
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{
					ID:     "key-uuid-1",
					Name:   "openai-primary",
					Value:  *schemas.NewSecretVar("sk-test-key-v1"),
					Weight: 1.0,
				},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Update with same KeyID but different value
	providers["openai"] = ProviderConfig{
		Keys: []schemas.Key{
			{
				ID:     "key-uuid-1", // Same KeyID
				Name:   "openai-primary",
				Value:  *schemas.NewSecretVar("sk-test-key-v2"), // Updated value
				Weight: 2.0,
			},
		},
	}
	err = store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Verify key was updated, not duplicated
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result["openai"].Keys, 1)
	assert.Equal(t, "sk-test-key-v2", result["openai"].Keys[0].Value.Val)
}

func TestUpdateProvidersConfig_UpdateExistingByName_FallbackFix(t *testing.T) {
	// This test verifies the fix for the unique constraint violation issue
	// when a new UUID is generated for a key that already exists by name
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create initial provider with key
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{
					ID:     "original-uuid",
					Name:   "openai-primary",
					Value:  *schemas.NewSecretVar("sk-test-key-v1"),
					Weight: 1.0,
				},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Simulate config reload with NEW UUID (as happens when loading from config file)
	providers["openai"] = ProviderConfig{
		Keys: []schemas.Key{
			{
				ID:     "new-uuid-from-config-reload", // Different UUID!
				Name:   "openai-primary",              // Same name
				Value:  *schemas.NewSecretVar("sk-test-key-v2"),
				Weight: 1.5,
			},
		},
	}
	err = store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err, "Should not fail with unique constraint violation")

	// Verify key was updated (not duplicated) and original KeyID preserved
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result["openai"].Keys, 1, "Should have exactly one key, not duplicated")
	assert.Equal(t, "sk-test-key-v2", result["openai"].Keys[0].Value.Val, "Value should be updated")
	assert.Equal(t, "original-uuid", result["openai"].Keys[0].ID, "Original KeyID should be preserved")
}

func TestUpdateProvidersConfig_MultipleKeys(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-1", Name: "openai-primary", Value: *schemas.NewSecretVar("sk-key-1"), Weight: 1.0},
				{ID: "key-2", Name: "openai-secondary", Value: *schemas.NewSecretVar("sk-key-2"), Weight: 0.5},
			},
		},
		"anthropic": {
			Keys: []schemas.Key{
				{ID: "key-3", Name: "anthropic-main", Value: *schemas.NewSecretVar("sk-key-3"), Weight: 1.0},
			},
		},
	}

	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 2)
	assert.Len(t, result["openai"].Keys, 2)
	assert.Len(t, result["anthropic"].Keys, 1)
}

func TestProviderKeyCRUD(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateProvidersConfig(ctx, map[schemas.ModelProvider]ProviderConfig{
		"openai": {},
	})
	require.NoError(t, err)

	keys, err := store.GetProviderKeys(ctx, "openai")
	require.NoError(t, err)
	assert.Empty(t, keys)

	key := schemas.Key{
		ID:     "key-uuid-1",
		Name:   "openai-primary",
		Value:  *schemas.NewSecretVar("sk-test-key-v1"),
		Weight: 1.0,
	}

	err = store.CreateProviderKey(ctx, "openai", key)
	require.NoError(t, err)

	keys, err = store.GetProviderKeys(ctx, "openai")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "openai-primary", keys[0].Name)

	storedKey, err := store.GetProviderKey(ctx, "openai", key.ID)
	require.NoError(t, err)
	require.NotNil(t, storedKey)
	assert.Equal(t, "sk-test-key-v1", storedKey.Value.Val)

	key.Value = *schemas.NewSecretVar("sk-test-key-v2")
	key.Weight = 2.0

	err = store.UpdateProviderKey(ctx, "openai", key.ID, key)
	require.NoError(t, err)

	storedKey, err = store.GetProviderKey(ctx, "openai", key.ID)
	require.NoError(t, err)
	require.NotNil(t, storedKey)
	assert.Equal(t, "sk-test-key-v2", storedKey.Value.Val)
	assert.Equal(t, 2.0, storedKey.Weight)

	err = store.DeleteProviderKey(ctx, "openai", key.ID)
	require.NoError(t, err)

	keys, err = store.GetProviderKeys(ctx, "openai")
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestProviderKeyCRUD_ProviderMustExist(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	key := schemas.Key{
		ID:     "key-uuid-1",
		Name:   "openai-primary",
		Value:  *schemas.NewSecretVar("sk-test-key-v1"),
		Weight: 1.0,
	}

	err := store.CreateProviderKey(ctx, "openai", key)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = store.GetProviderKeys(ctx, "openai")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = store.GetProviderKey(ctx, "openai", key.ID)
	require.ErrorIs(t, err, ErrNotFound)

	err = store.UpdateProviderKey(ctx, "openai", key.ID, key)
	require.ErrorIs(t, err, ErrNotFound)

	err = store.DeleteProviderKey(ctx, "openai", key.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

// =============================================================================
// Budget Tests
// =============================================================================

func TestCreateBudget(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	budget := &tables.TableBudget{
		ID:            "budget-test",
		MaxLimit:      100.0,
		ResetDuration: "1M",
	}

	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Verify budget was created
	result, err := store.GetBudget(ctx, "budget-test")
	require.NoError(t, err)
	assert.Equal(t, "budget-test", result.ID)
	assert.Equal(t, 100.0, result.MaxLimit)
	assert.Equal(t, "1M", result.ResetDuration)
}

func TestCreateBudget_InvalidDuration(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	budget := &tables.TableBudget{
		ID:            "budget-invalid",
		MaxLimit:      100.0,
		ResetDuration: "invalid",
	}

	err := store.CreateBudget(ctx, budget)
	assert.Error(t, err, "Should fail with invalid duration")
}

func TestCreateBudget_NegativeLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	budget := &tables.TableBudget{
		ID:            "budget-negative",
		MaxLimit:      -50.0,
		ResetDuration: "1h",
	}

	err := store.CreateBudget(ctx, budget)
	assert.Error(t, err, "Should fail with negative max limit")
}

func TestUpdateBudget(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create budget
	budget := &tables.TableBudget{
		ID:            "budget-update",
		MaxLimit:      100.0,
		ResetDuration: "1h",
	}
	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Update budget
	budget.MaxLimit = 200.0
	err = store.UpdateBudget(ctx, budget)
	require.NoError(t, err)

	// Verify update
	result, err := store.GetBudget(ctx, "budget-update")
	require.NoError(t, err)
	assert.Equal(t, 200.0, result.MaxLimit)
}

// TestUpdateBudgetPreservesRuntimeCounters verifies a configuration-shaped update
// cannot clobber the runtime-owned counters.
//
// CurrentUsage and LastReset are advanced by the governance dump path, never by
// config.json, which is why GenerateBudgetHash deliberately excludes them. A
// source_of_truth=config.json startup force-sync replays the file row through
// UpdateBudget with those fields at their Go zero values, so UpdateBudget must
// carry the persisted values forward the same way it already does for the
// override grant and the owner FKs.
func TestUpdateBudgetPreservesRuntimeCounters(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	lastReset := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, store.CreateBudget(ctx, &tables.TableBudget{
		ID:            "budget-runtime-counters",
		MaxLimit:      100.0,
		ResetDuration: "1h",
		CurrentUsage:  42.5,
		LastReset:     lastReset,
	}))

	// The shape config.json produces: configuration-owned fields only, with the
	// runtime counters absent and therefore zero.
	require.NoError(t, store.UpdateBudget(ctx, &tables.TableBudget{
		ID:            "budget-runtime-counters",
		MaxLimit:      200.0,
		ResetDuration: "1h",
	}))

	result, err := store.GetBudget(ctx, "budget-runtime-counters")
	require.NoError(t, err)
	assert.Equal(t, 200.0, result.MaxLimit, "configuration-owned max_limit must follow the file")
	assert.Equal(t, "1h", result.ResetDuration, "configuration-owned reset_duration must follow the file")
	assert.Equal(t, 42.5, result.CurrentUsage, "runtime-owned current_usage must survive a config-shaped update")
	assert.True(t, result.LastReset.Equal(lastReset),
		"runtime-owned last_reset must survive a config-shaped update: got %s, want %s",
		result.LastReset.UTC(), lastReset)
}

// TestCreateBudgetWithOverride verifies finite and permanent override state round-trips through the config store.
func TestCreateBudgetWithOverride(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	// A finite grant must carry its anchor and total, which is what the remaining
	// count is derived from. A cycles override without them is rejected by
	// validation rather than persisted as a lifecycle-less override.
	grantAnchor := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	tests := []*tables.TableBudget{
		{
			ID:                      "budget-override-cycles",
			MaxLimit:                100,
			ResetDuration:           "1h",
			LastReset:               grantAnchor,
			OverrideAmount:          25,
			OverrideMode:            tables.BudgetOverrideModeCycles,
			OverrideCyclesRemaining: 4,
			OverrideCyclesTotal:     4,
			OverrideAnchorReset:     &grantAnchor,
		},
		{
			ID:             "budget-override-forever",
			MaxLimit:       200,
			ResetDuration:  "1d",
			OverrideAmount: 50,
			OverrideMode:   tables.BudgetOverrideModeForever,
		},
	}

	for _, budget := range tests {
		require.NoError(t, store.CreateBudget(ctx, budget))
		got, err := store.GetBudget(ctx, budget.ID)
		require.NoError(t, err)
		assert.Equal(t, budget.OverrideAmount, got.OverrideAmount)
		assert.Equal(t, budget.OverrideMode, got.OverrideMode)
		assert.Equal(t, budget.OverrideCyclesRemaining, got.OverrideCyclesRemaining)
		assert.Equal(t, budget.OverrideCyclesTotal, got.OverrideCyclesTotal)
		if budget.OverrideAnchorReset == nil {
			assert.Nil(t, got.OverrideAnchorReset)
			continue
		}
		require.NotNil(t, got.OverrideAnchorReset)
		assert.True(t, got.OverrideAnchorReset.Equal(*budget.OverrideAnchorReset),
			"grant anchor did not round-trip: got %s, want %s", got.OverrideAnchorReset.UTC(), budget.OverrideAnchorReset.UTC())
	}
}

// TestUpdateBudgetOverridePreservesBudgetState verifies the partial update cannot clobber usage or base configuration.
func TestUpdateBudgetOverridePreservesBudgetState(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	budget := &tables.TableBudget{
		ID:            "budget-override-partial-update",
		MaxLimit:      100,
		ResetDuration: "1d",
		CurrentUsage:  40,
	}
	require.NoError(t, store.CreateBudget(ctx, budget))

	updated, err := store.UpdateBudgetOverride(ctx, budget.ID, 25, tables.BudgetOverrideModeCycles, 4, false)
	require.NoError(t, err)
	assert.Equal(t, 100.0, updated.MaxLimit)
	assert.Equal(t, "1d", updated.ResetDuration)
	assert.Equal(t, 40.0, updated.CurrentUsage)
	assert.Equal(t, 25.0, updated.OverrideAmount)
	assert.Equal(t, tables.BudgetOverrideModeCycles, updated.OverrideMode)
	assert.Equal(t, 4, updated.OverrideCyclesRemaining)
	assert.Equal(t, 4, updated.OverrideCyclesTotal)
	require.NotNil(t, updated.OverrideAnchorReset, "a finite grant must be anchored so its remaining count can be derived")

	cleared, err := store.UpdateBudgetOverride(ctx, budget.ID, 0, "", 0, false)
	require.NoError(t, err)
	assert.Equal(t, 100.0, cleared.MaxLimit)
	assert.Equal(t, 40.0, cleared.CurrentUsage)
	assert.Zero(t, cleared.OverrideAmount)
	assert.Empty(t, cleared.OverrideMode)
	assert.Zero(t, cleared.OverrideCyclesRemaining)
	assert.Zero(t, cleared.OverrideCyclesTotal)
	assert.Nil(t, cleared.OverrideAnchorReset, "clearing must drop the grant so it cannot be re-derived")
}

// TestUpdateBudgetOverrideAnchorsAtCurrentWindow verifies a rolling grant is
// anchored on the budget's CreatedAt lattice, so every node derives the same
// remaining count from it.
func TestUpdateBudgetOverrideAnchorsAtCurrentWindow(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	created := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)
	budget := &tables.TableBudget{
		ID:            "budget-override-rolling-anchor",
		MaxLimit:      100,
		ResetDuration: "1h",
		CreatedAt:     created,
		LastReset:     created,
	}
	require.NoError(t, store.CreateBudget(ctx, budget))

	updated, err := store.UpdateBudgetOverride(ctx, budget.ID, 25, tables.BudgetOverrideModeCycles, 2, false)
	require.NoError(t, err)
	require.NotNil(t, updated.OverrideAnchorReset)

	// The anchor must be a lattice point: an exact number of windows past the
	// budget's creation instant, not the wall clock at grant time.
	offset := updated.OverrideAnchorReset.Sub(updated.CreatedAt)
	assert.Zero(t, offset%time.Hour,
		"grant anchor is not lattice aligned: %s past CreatedAt is not a whole number of 1h windows", offset)
}

// TestUpdateBudgetOverrideAnchorsAtCalendarPeriodStart verifies a calendar-aligned
// grant is anchored on the calendar boundary rather than the rolling lattice.
func TestUpdateBudgetOverrideAnchorsAtCalendarPeriodStart(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	created := time.Now().UTC().AddDate(0, 0, -10).Truncate(time.Second)
	budget := &tables.TableBudget{
		ID:            "budget-override-calendar-anchor",
		MaxLimit:      100,
		ResetDuration: "1d",
		CreatedAt:     created,
		LastReset:     created,
	}
	require.NoError(t, store.CreateBudget(ctx, budget))

	updated, err := store.UpdateBudgetOverride(ctx, budget.ID, 25, tables.BudgetOverrideModeCycles, 2, true)
	require.NoError(t, err)
	require.NotNil(t, updated.OverrideAnchorReset)

	wantAnchor := tables.GetCalendarPeriodStart("1d", time.Now(), tables.QuarterStartNotApplicable)
	assert.True(t, updated.OverrideAnchorReset.Equal(wantAnchor),
		"calendar-aligned grant anchor should be midnight UTC today: got %s, want %s",
		updated.OverrideAnchorReset.UTC(), wantAnchor.UTC())
}

func TestGetBudgets(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create multiple budgets
	budgets := []*tables.TableBudget{
		{ID: "budget-1", MaxLimit: 100.0, ResetDuration: "1h"},
		{ID: "budget-2", MaxLimit: 200.0, ResetDuration: "1d"},
		{ID: "budget-3", MaxLimit: 300.0, ResetDuration: "1M"},
	}

	for _, b := range budgets {
		err := store.CreateBudget(ctx, b)
		require.NoError(t, err)
	}

	result, err := store.GetBudgets(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 3)
}

// =============================================================================
// Rate Limit Tests
// =============================================================================

func TestCreateRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	requestMax := int64(1000)
	tokenDuration := "1h"
	requestDuration := "1h"

	rateLimit := &tables.TableRateLimit{
		ID:                   "rate-limit-test",
		TokenMaxLimit:        &tokenMax,
		TokenResetDuration:   &tokenDuration,
		RequestMaxLimit:      &requestMax,
		RequestResetDuration: &requestDuration,
	}

	err := store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	result, err := store.GetRateLimit(ctx, "rate-limit-test")
	require.NoError(t, err)
	assert.Equal(t, "rate-limit-test", result.ID)
	assert.Equal(t, int64(100000), *result.TokenMaxLimit)
	assert.Equal(t, int64(1000), *result.RequestMaxLimit)
}

func TestCreateRateLimit_InvalidDuration(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	invalidDuration := "invalid"

	rateLimit := &tables.TableRateLimit{
		ID:                 "rate-limit-invalid",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &invalidDuration,
	}

	err := store.CreateRateLimit(ctx, rateLimit)
	assert.Error(t, err, "Should fail with invalid duration")
}

func TestCreateRateLimit_MissingDuration(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)

	rateLimit := &tables.TableRateLimit{
		ID:            "rate-limit-missing",
		TokenMaxLimit: &tokenMax,
		// Missing TokenResetDuration
	}

	err := store.CreateRateLimit(ctx, rateLimit)
	assert.Error(t, err, "Should fail when max limit set without duration")
}

func TestUpdateRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	tokenDuration := "1h"

	rateLimit := &tables.TableRateLimit{
		ID:                 "rate-limit-update",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &tokenDuration,
	}
	err := store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	// Update
	newMax := int64(200000)
	rateLimit.TokenMaxLimit = &newMax
	err = store.UpdateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	result, err := store.GetRateLimit(ctx, "rate-limit-update")
	require.NoError(t, err)
	assert.Equal(t, int64(200000), *result.TokenMaxLimit)
}

// TestUpdateRateLimitPreservesRuntimeCounters is the rate-limit counterpart of
// TestUpdateBudgetPreservesRuntimeCounters: the same startup force-sync replays
// the file-shaped rate limit, and the token/request counters are runtime-owned
// for the same reason (GenerateRateLimitHash excludes them).
func TestUpdateRateLimitPreservesRuntimeCounters(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	tokenMax := int64(100000)
	tokenDuration := "1h"
	requestMax := int64(500)
	requestDuration := "1h"
	tokenLastReset := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	requestLastReset := time.Date(2026, time.August, 1, 0, 30, 0, 0, time.UTC)

	require.NoError(t, store.CreateRateLimit(ctx, &tables.TableRateLimit{
		ID:                   "rate-limit-runtime-counters",
		TokenMaxLimit:        &tokenMax,
		TokenResetDuration:   &tokenDuration,
		TokenCurrentUsage:    1234,
		TokenLastReset:       tokenLastReset,
		RequestMaxLimit:      &requestMax,
		RequestResetDuration: &requestDuration,
		RequestCurrentUsage:  56,
		RequestLastReset:     requestLastReset,
	}))

	newTokenMax := int64(200000)
	require.NoError(t, store.UpdateRateLimit(ctx, &tables.TableRateLimit{
		ID:                   "rate-limit-runtime-counters",
		TokenMaxLimit:        &newTokenMax,
		TokenResetDuration:   &tokenDuration,
		RequestMaxLimit:      &requestMax,
		RequestResetDuration: &requestDuration,
	}))

	result, err := store.GetRateLimit(ctx, "rate-limit-runtime-counters")
	require.NoError(t, err)
	require.NotNil(t, result.TokenMaxLimit)
	assert.Equal(t, int64(200000), *result.TokenMaxLimit, "configuration-owned token_max_limit must follow the file")
	assert.Equal(t, int64(1234), result.TokenCurrentUsage, "runtime-owned token_current_usage must survive a config-shaped update")
	assert.Equal(t, int64(56), result.RequestCurrentUsage, "runtime-owned request_current_usage must survive a config-shaped update")
	assert.True(t, result.TokenLastReset.Equal(tokenLastReset),
		"runtime-owned token_last_reset must survive a config-shaped update: got %s, want %s",
		result.TokenLastReset.UTC(), tokenLastReset)
	assert.True(t, result.RequestLastReset.Equal(requestLastReset),
		"runtime-owned request_last_reset must survive a config-shaped update: got %s, want %s",
		result.RequestLastReset.UTC(), requestLastReset)
}

// =============================================================================
// Virtual Key Tests
// =============================================================================

// TestGetVirtualKeysPaginated_AssignmentFilters covers how the customer / team /
// user filters compose. A virtual key is assigned to at most one of the three, so
// supplying several ORs them rather than narrowing to nothing.
//
// UserID is unresolvable in the OSS build — the VK↔user link lives in an
// enterprise-only table — so it contributes a never-true disjunct here: a
// user-only filter matches nothing instead of silently returning every key. It
// deliberately does NOT suppress a customer/team disjunct supplied alongside it;
// those are still resolvable, and dropping them would diverge from the enterprise
// store, which returns exactly that union.
func TestGetVirtualKeysPaginated_AssignmentFilters(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateCustomer(ctx, &tables.TableCustomer{ID: "cust-1", Name: "Customer One"}))
	require.NoError(t, store.CreateTeam(ctx, &tables.TableTeam{ID: "team-1", Name: "Team One"}))

	custID, teamID := "cust-1", "team-1"
	seed := []*tables.TableVirtualKey{
		{ID: "vk-cust", Name: "Customer Key", Value: *schemas.NewSecretVar("vk-cust-val"), IsActive: schemas.Ptr(true), CustomerID: &custID},
		{ID: "vk-team", Name: "Team Key", Value: *schemas.NewSecretVar("vk-team-val"), IsActive: schemas.Ptr(true), TeamID: &teamID},
		{ID: "vk-none", Name: "Unassigned Key", Value: *schemas.NewSecretVar("vk-none-val"), IsActive: schemas.Ptr(true)},
	}
	for _, vk := range seed {
		require.NoError(t, store.CreateVirtualKey(ctx, vk))
	}

	tests := []struct {
		name    string
		params  VirtualKeyQueryParams
		wantIDs []string
	}{
		{
			name:    "no filters returns every key",
			params:  VirtualKeyQueryParams{},
			wantIDs: []string{"vk-cust", "vk-none", "vk-team"},
		},
		{
			name:    "customer only",
			params:  VirtualKeyQueryParams{CustomerID: "cust-1"},
			wantIDs: []string{"vk-cust"},
		},
		{
			name:    "team only",
			params:  VirtualKeyQueryParams{TeamID: "team-1"},
			wantIDs: []string{"vk-team"},
		},
		{
			name:    "customer or team",
			params:  VirtualKeyQueryParams{CustomerID: "cust-1", TeamID: "team-1"},
			wantIDs: []string{"vk-cust", "vk-team"},
		},
		{
			// Fail closed: OSS cannot resolve the assignment, so it matches nothing
			// rather than falling through to an unfiltered list.
			name:    "user only matches nothing in OSS",
			params:  VirtualKeyQueryParams{UserID: "user-1"},
			wantIDs: nil,
		},
		{
			name:    "user plus customer keeps the resolvable customer disjunct",
			params:  VirtualKeyQueryParams{UserID: "user-1", CustomerID: "cust-1"},
			wantIDs: []string{"vk-cust"},
		},
		{
			name:    "user plus team keeps the resolvable team disjunct",
			params:  VirtualKeyQueryParams{UserID: "user-1", TeamID: "team-1"},
			wantIDs: []string{"vk-team"},
		},
		{
			name:    "user plus customer and team keeps both resolvable disjuncts",
			params:  VirtualKeyQueryParams{UserID: "user-1", CustomerID: "cust-1", TeamID: "team-1"},
			wantIDs: []string{"vk-cust", "vk-team"},
		},
		{
			name:    "user filter never widens a non-matching search",
			params:  VirtualKeyQueryParams{UserID: "user-1", Search: "Unassigned"},
			wantIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vks, totalCount, err := store.GetVirtualKeysPaginated(ctx, tt.params)
			require.NoError(t, err)

			gotIDs := make([]string, 0, len(vks))
			for _, vk := range vks {
				gotIDs = append(gotIDs, vk.ID)
			}
			sort.Strings(gotIDs)
			assert.Equal(t, tt.wantIDs, nonEmptyIDs(gotIDs))
			// The count drives pagination, so it must agree with the page contents.
			assert.Equal(t, int64(len(tt.wantIDs)), totalCount)
		})
	}
}

// TestGetVirtualKeysPaginated_Search covers the fields a search term matches. The
// search box is the only free-text affordance on the virtual keys page, so it
// matches everything the "Assigned To" column can display - the key's own name,
// its team, and its customer - rather than the name alone. (The assigned user is
// the enterprise store's addition; the link table does not exist here.)
// TestGetVirtualKeysPaginated_BudgetSpentCountsWildcardModelConfigBudgets pins the
// budget_spent ordering across both owners of a VK's key-wide budgets: the VK itself and
// its provider-independent wildcard model config, where the standalone-limits migration
// (upstream #7291) moves them. A provider-specific budget is not key-wide and does not count.
func TestGetVirtualKeysPaginated_BudgetSpentCountsWildcardModelConfigBudgets(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	db := store.DB()

	for _, id := range []string{"vk-direct", "vk-mc", "vk-provider", "vk-none"} {
		require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{ID: id, Name: id, Value: *schemas.NewSecretVar(id + "-val"), IsActive: schemas.Ptr(true)}))
	}
	mcVK, providerVK, openai := "vk-mc", "vk-provider", "openai"
	require.NoError(t, db.Create(&tables.TableModelConfig{ID: "mc-wild", ModelName: tables.ModelConfigAllModels, Scope: tables.ModelConfigScopeVirtualKey, ScopeID: &mcVK}).Error)
	require.NoError(t, db.Create(&tables.TableModelConfig{ID: "mc-openai", ModelName: tables.ModelConfigAllModels, Scope: tables.ModelConfigScopeVirtualKey, ScopeID: &providerVK, Provider: &openai}).Error)
	for _, b := range []tables.TableBudget{
		{ID: "b-direct", MaxLimit: 1000, ResetDuration: "1M", CurrentUsage: 50, VirtualKeyID: schemas.Ptr("vk-direct")},
		{ID: "b-wild", MaxLimit: 1000, ResetDuration: "1M", CurrentUsage: 900, ModelConfigID: schemas.Ptr("mc-wild")},
		{ID: "b-openai", MaxLimit: 1000, ResetDuration: "1M", CurrentUsage: 999, ModelConfigID: schemas.Ptr("mc-openai")},
	} {
		require.NoError(t, db.Create(&b).Error)
	}

	vks, _, err := store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{SortBy: "budget_spent", Order: "desc"})
	require.NoError(t, err)
	ids := make([]string, len(vks))
	for i, vk := range vks {
		ids[i] = vk.ID
	}
	assert.Equal(t, []string{"vk-mc", "vk-direct", "vk-none", "vk-provider"}, ids)
}

func TestGetVirtualKeysPaginated_Search(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateCustomer(ctx, &tables.TableCustomer{ID: "cust-1", Name: "Acme Corp"}))
	require.NoError(t, store.CreateTeam(ctx, &tables.TableTeam{ID: "team-1", Name: "Platform Squad"}))

	custID, teamID := "cust-1", "team-1"
	seed := []*tables.TableVirtualKey{
		{ID: "vk-cust", Name: "billing key", Value: *schemas.NewSecretVar("vk-cust-val"), IsActive: schemas.Ptr(true), CustomerID: &custID},
		{ID: "vk-team", Name: "ingest key", Value: *schemas.NewSecretVar("vk-team-val"), IsActive: schemas.Ptr(true), TeamID: &teamID},
		{ID: "vk-none", Name: "Platform scratch", Value: *schemas.NewSecretVar("vk-none-val"), IsActive: schemas.Ptr(true)},
	}
	for _, vk := range seed {
		require.NoError(t, store.CreateVirtualKey(ctx, vk))
	}

	tests := []struct {
		name    string
		search  string
		wantIDs []string
	}{
		{name: "matches the key name", search: "billing", wantIDs: []string{"vk-cust"}},
		{name: "matches the key name case-insensitively", search: "BILLING", wantIDs: []string{"vk-cust"}},
		{name: "matches the customer name", search: "acme", wantIDs: []string{"vk-cust"}},
		{name: "matches the team name", search: "squad", wantIDs: []string{"vk-team"}},
		{
			// One term can hit a key by its own name and another by its team, and
			// both belong in the results.
			name:    "unions matches across fields",
			search:  "platform",
			wantIDs: []string{"vk-none", "vk-team"},
		},
		{name: "matches nothing when no field contains the term", search: "nonexistent", wantIDs: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vks, totalCount, err := store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Search: tt.search})
			require.NoError(t, err)

			gotIDs := make([]string, 0, len(vks))
			for _, vk := range vks {
				gotIDs = append(gotIDs, vk.ID)
			}
			sort.Strings(gotIDs)
			assert.Equal(t, tt.wantIDs, nonEmptyIDs(gotIDs))
			assert.Equal(t, int64(len(tt.wantIDs)), totalCount)
		})
	}
}

// A search must not widen an assignment filter: the two narrow together.
func TestGetVirtualKeysPaginated_SearchWithAssignmentFilter(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateTeam(ctx, &tables.TableTeam{ID: "team-1", Name: "Platform Squad"}))
	teamID := "team-1"
	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-team", Name: "ingest key", Value: *schemas.NewSecretVar("vk-team-val"), IsActive: schemas.Ptr(true), TeamID: &teamID,
	}))
	require.NoError(t, store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
		ID: "vk-none", Name: "ingest scratch", Value: *schemas.NewSecretVar("vk-none-val"), IsActive: schemas.Ptr(true),
	}))

	vks, totalCount, err := store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Search: "ingest", TeamID: "team-1"})
	require.NoError(t, err)
	require.Len(t, vks, 1)
	assert.Equal(t, "vk-team", vks[0].ID)
	assert.Equal(t, int64(1), totalCount)
}

// nonEmptyIDs normalizes an empty slice to nil so table cases can express
// "matches nothing" as a nil wantIDs.
func nonEmptyIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

func TestCreateVirtualKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-test",
		Name:     "Test Virtual Key",
		Value:    *schemas.NewSecretVar("vk-test-value-123"),
		IsActive: schemas.Ptr(true),
	}

	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-test")
	require.NoError(t, err)
	assert.Equal(t, "vk-test", result.ID)
	assert.Equal(t, "Test Virtual Key", result.Name)
	assert.Equal(t, "vk-test-value-123", result.Value.Val)
	assert.True(t, result.IsActiveValue())
}

func TestCreateVirtualKey_WithBudgetAndRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create budget first
	budget := &tables.TableBudget{
		ID:            "budget-for-vk",
		MaxLimit:      100.0,
		ResetDuration: "1M",
	}
	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Create rate limit
	tokenMax := int64(100000)
	tokenDuration := "1h"
	rateLimit := &tables.TableRateLimit{
		ID:                 "rate-limit-for-vk",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &tokenDuration,
	}
	err = store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	// Create virtual key with references
	rateLimitID := "rate-limit-for-vk"
	vkID := "vk-with-refs"
	vk := &tables.TableVirtualKey{
		ID:          vkID,
		Name:        "VK With References",
		Value:       *schemas.NewSecretVar("vk-refs-value"),
		IsActive:    schemas.Ptr(true),
		RateLimitID: &rateLimitID,
	}

	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Link the existing budget to the VK via FK
	budget.VirtualKeyID = &vkID
	err = store.UpdateBudget(ctx, budget)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-with-refs")
	require.NoError(t, err)
	assert.Len(t, result.Budgets, 1)
	assert.Equal(t, "budget-for-vk", result.Budgets[0].ID)
	assert.NotNil(t, result.RateLimitID)
	assert.Equal(t, "rate-limit-for-vk", *result.RateLimitID)
}

func TestCreateVirtualKey_DuplicateName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk1 := &tables.TableVirtualKey{
		ID:       "vk-1",
		Name:     "Same Name",
		Value:    *schemas.NewSecretVar("vk-value-1"),
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk1)
	require.NoError(t, err)

	vk2 := &tables.TableVirtualKey{
		ID:       "vk-2",
		Name:     "Same Name", // Duplicate name
		Value:    *schemas.NewSecretVar("vk-value-2"),
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk2)
	assert.Error(t, err, "Should fail with duplicate name")
}

func TestGetVirtualKeyByValue(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-lookup",
		Name:     "Lookup Key",
		Value:    *schemas.NewSecretVar("vk-unique-value-xyz"),
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	result, err := store.GetVirtualKeyByValue(ctx, "vk-unique-value-xyz")
	require.NoError(t, err)
	assert.Equal(t, "vk-lookup", result.ID)
}

func TestUpdateVirtualKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-update",
		Name:     "Original Name",
		Value:    *schemas.NewSecretVar("vk-update-value"),
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Update
	vk.Name = "Updated Name"
	vk.IsActive = schemas.Ptr(false)
	err = store.UpdateVirtualKey(ctx, vk)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-update")
	require.NoError(t, err)
	assert.Equal(t, "Updated Name", result.Name)
	assert.False(t, result.IsActiveValue())

	// Content logging is tri-state and every state must survive an update: the update path names
	// its columns explicitly, so a column left off that list is silently never written.
	vk.DisableContentLogging = new(true)
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, "vk-update")
	require.NoError(t, err)
	require.NotNil(t, result.DisableContentLogging, "forcing content off must persist")
	assert.True(t, *result.DisableContentLogging)

	vk.DisableContentLogging = new(false)
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, "vk-update")
	require.NoError(t, err)
	require.NotNil(t, result.DisableContentLogging, "forcing content on must persist")
	assert.False(t, *result.DisableContentLogging)

	vk.DisableContentLogging = nil
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, "vk-update")
	require.NoError(t, err)
	assert.Nil(t, result.DisableContentLogging, "clearing the override must write NULL, back to inherit")
}

// TestUpdateVirtualKey_PersistsBusinessUnitOwner verifies an update writes business_unit_id, so
// assigning an existing key to a business unit sticks and moving it elsewhere clears it.
func TestUpdateVirtualKey_PersistsBusinessUnitOwner(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-bu-owner",
		Name:     "BU Owner",
		Value:    *schemas.NewSecretVar("vk-bu-owner-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	vk.BusinessUnitID = schemas.Ptr("bu-1")
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err := store.GetVirtualKey(ctx, "vk-bu-owner")
	require.NoError(t, err)
	require.NotNil(t, result.BusinessUnitID, "assigning a business unit must persist")
	assert.Equal(t, "bu-1", *result.BusinessUnitID)

	vk.BusinessUnitID = nil
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, "vk-bu-owner")
	require.NoError(t, err)
	assert.Nil(t, result.BusinessUnitID, "clearing the business unit must write NULL")
}

// TestUpdateVirtualKey_KeepsDistinctIncomingRateLimit pins upstream #7291's config-sync
// guard to the limit the migration actually moved. A VK whose wildcard model config owns
// limit A drops an incoming direct reference to A, which would otherwise undo the
// migration, but keeps a reference to a different limit B the config file now declares.
// Dropping B would silently lose that limit and record the file's hash as applied.
func TestUpdateVirtualKey_KeepsDistinctIncomingRateLimit(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"rl-a", "rl-b"} {
		require.NoError(t, store.CreateRateLimit(ctx, &tables.TableRateLimit{ID: id, RequestMaxLimit: schemas.Ptr(int64(10)), RequestResetDuration: schemas.Ptr("1m")}))
	}
	vk := &tables.TableVirtualKey{ID: "vk-sync", Name: "vk-sync", Value: *schemas.NewSecretVar("vk-sync-value"), IsActive: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	vkID := "vk-sync"
	require.NoError(t, store.DB().Create(&tables.TableModelConfig{ID: "mc-sync", ModelName: tables.ModelConfigAllModels,
		Scope: tables.ModelConfigScopeVirtualKey, ScopeID: &vkID, RateLimitID: schemas.Ptr("rl-a")}).Error)

	vk.RateLimitID = schemas.Ptr("rl-a")
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err := store.GetVirtualKey(ctx, "vk-sync")
	require.NoError(t, err)
	assert.Nil(t, result.RateLimitID, "the limit the model config owns is not referenced again from the VK")

	vk.RateLimitID = schemas.Ptr("rl-b")
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, "vk-sync")
	require.NoError(t, err)
	require.NotNil(t, result.RateLimitID, "a distinct incoming limit is kept")
	assert.Equal(t, "rl-b", *result.RateLimitID)
}

// TestVirtualKeyMetadata_PersistsThroughCreateAndUpdate pins the metadata round trip: create
// stores it, update replaces it (the update path names its columns, so a missing "metadata" would
// silently drop the change), an empty map clears it to NULL, and invalid metadata is refused.
func TestVirtualKeyMetadata_PersistsThroughCreateAndUpdate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-metadata",
		Name:     "Metadata",
		Value:    *schemas.NewSecretVar("vk-metadata-value"),
		Metadata: map[string]string{"cost_center": "cc-42", "owner": "team-a@example.com"},
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	result, err := store.GetVirtualKey(ctx, vk.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"cost_center": "cc-42", "owner": "team-a@example.com"}, result.Metadata)

	vk.Metadata = map[string]string{"cost_center": "cc-7"}
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, vk.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"cost_center": "cc-7"}, result.Metadata, "an update must replace the metadata")

	// An update that does not set metadata (nil), such as a config.json sync of an entry without a
	// metadata field, keeps what is stored; only an explicit empty map clears it.
	vk.Metadata = nil
	vk.Description = "edited elsewhere"
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	result, err = store.GetVirtualKey(ctx, vk.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"cost_center": "cc-7"}, result.Metadata, "a nil metadata must keep the stored value")
	assert.Equal(t, "edited elsewhere", result.Description)

	vk.Metadata = map[string]string{}
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	var raw *string
	require.NoError(t, store.DB().Table("governance_virtual_keys").Select("metadata").Where("id = ?", vk.ID).Scan(&raw).Error)
	assert.Nil(t, raw, "clearing the metadata must write NULL, not an empty object")
	result, err = store.GetVirtualKey(ctx, vk.ID)
	require.NoError(t, err)
	assert.Empty(t, result.Metadata)

	vk.Metadata = map[string]string{"bad key": "x"}
	require.ErrorContains(t, store.UpdateVirtualKey(ctx, vk), "invalid metadata key")
	bad := &tables.TableVirtualKey{ID: "vk-metadata-bad", Name: "Bad", Value: *schemas.NewSecretVar("vk-metadata-bad-value"), Metadata: map[string]string{"bifrost_alb_provider": "x"}}
	require.ErrorContains(t, store.CreateVirtualKey(ctx, bad), "reserved")
}

func TestUpdateVirtualKey_PreservesRotationStateOnPlainUpdate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-grace",
		Name:     "Grace Key",
		Value:    *schemas.NewSecretVar("vk-old-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	// Rotation: RotatedAt set makes the grace-period fields authoritative.
	now := time.Now().UTC()
	exp := now.Add(5 * time.Minute)
	vk.Value = *schemas.NewSecretVar("vk-new-value")
	vk.PreviousValue = *schemas.NewSecretVar("vk-old-value")
	vk.PreviousValueExpiresAt = &exp
	vk.RotatedAt = &now
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))

	stored, err := store.GetVirtualKey(ctx, "vk-grace")
	require.NoError(t, err)
	assert.Equal(t, "vk-new-value", stored.Value.GetValue())
	assert.Equal(t, "vk-old-value", stored.PreviousValue.GetValue())
	assert.NotEmpty(t, stored.PreviousValueHash)
	require.NotNil(t, stored.PreviousValueExpiresAt)
	require.NotNil(t, stored.RotatedAt)
	assert.True(t, stored.HasActivePreviousValue(now))

	// Plain update (RotatedAt nil) must not wipe the in-flight grace window.
	plain := &tables.TableVirtualKey{
		ID:       "vk-grace",
		Name:     "Grace Key Renamed",
		Value:    *schemas.NewSecretVar("vk-new-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.UpdateVirtualKey(ctx, plain))

	stored, err = store.GetVirtualKey(ctx, "vk-grace")
	require.NoError(t, err)
	assert.Equal(t, "Grace Key Renamed", stored.Name)
	assert.Equal(t, "vk-old-value", stored.PreviousValue.GetValue())
	require.NotNil(t, stored.PreviousValueExpiresAt)
	require.NotNil(t, stored.RotatedAt)
}

func TestUpdateVirtualKey_RotationWithoutCooldownClearsPreviousValue(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-nograce",
		Name:     "No Grace Key",
		Value:    *schemas.NewSecretVar("vk-first-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	// First rotation with a grace window.
	now := time.Now().UTC()
	exp := now.Add(5 * time.Minute)
	vk.Value = *schemas.NewSecretVar("vk-second-value")
	vk.PreviousValue = *schemas.NewSecretVar("vk-first-value")
	vk.PreviousValueExpiresAt = &exp
	vk.RotatedAt = &now
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))

	// Second rotation with cooldown disabled clears the grace state.
	later := now.Add(time.Minute)
	second := &tables.TableVirtualKey{
		ID:        "vk-nograce",
		Name:      "No Grace Key",
		Value:     *schemas.NewSecretVar("vk-third-value"),
		IsActive:  schemas.Ptr(true),
		RotatedAt: &later,
	}
	second.ClearPreviousValue()
	require.NoError(t, store.UpdateVirtualKey(ctx, second))

	stored, err := store.GetVirtualKey(ctx, "vk-nograce")
	require.NoError(t, err)
	assert.Equal(t, "vk-third-value", stored.Value.GetValue())
	assert.False(t, stored.PreviousValue.IsSet())
	assert.Empty(t, stored.PreviousValueHash)
	assert.Nil(t, stored.PreviousValueExpiresAt)
	assert.False(t, stored.HasActivePreviousValue(later))
}

func TestUpdateVirtualKey_UnresolvablePreviousValueRejected(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-badprev",
		Name:     "Bad Prev Key",
		Value:    *schemas.NewSecretVar("vk-current-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	// A set-but-unresolvable PreviousValue must fail the save, mirroring the
	// Value validation: persisting it would leave an empty hash that
	// grace-period authentication can never match.
	now := time.Now().UTC()
	exp := now.Add(5 * time.Minute)
	vk.Value = *schemas.NewSecretVar("vk-rotated-value")
	vk.PreviousValue = *schemas.NewSecretVar("env.BIFROST_TEST_UNSET_PREVIOUS_VALUE")
	vk.PreviousValueExpiresAt = &exp
	vk.RotatedAt = &now
	err := store.UpdateVirtualKey(ctx, vk)
	require.Error(t, err, "unresolvable previous value must not persist")
	assert.Contains(t, err.Error(), "could not be resolved")
}

func TestVirtualKeyHasActivePreviousValue(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	vk := &tables.TableVirtualKey{}
	assert.False(t, vk.HasActivePreviousValue(now), "no previous value")

	vk.PreviousValue = *schemas.NewSecretVar("vk-prev")
	assert.False(t, vk.HasActivePreviousValue(now), "no expiry set")

	vk.PreviousValueExpiresAt = &future
	assert.True(t, vk.HasActivePreviousValue(now), "inside grace window")
	assert.False(t, vk.HasActivePreviousValue(future), "now == expiry is expired")

	vk.PreviousValueExpiresAt = &past
	assert.False(t, vk.HasActivePreviousValue(now), "past expiry")
}

// rotateTestVirtualKey simulates a value rotation with a grace window the way
// rotateVirtualKeyByID does: new current value, old value retired with an expiry.
// oldValue must be the plaintext the key was created with: the struct's Value is
// encrypted in place by BeforeSave, so it cannot be read back after create.
func rotateTestVirtualKey(t *testing.T, store *RDBConfigStore, vk *tables.TableVirtualKey, oldValue, newValue string, expiresAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	vk.Value = *schemas.NewSecretVar(newValue)
	vk.PreviousValue = *schemas.NewSecretVar(oldValue)
	vk.PreviousValueExpiresAt = &expiresAt
	vk.RotatedAt = &now
	require.NoError(t, store.UpdateVirtualKey(context.Background(), vk))
}

func TestGetVirtualKeyByValue_GraceValueWithinWindow(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-grace-lookup",
		Name:     "Grace Lookup Key",
		Value:    *schemas.NewSecretVar("vk-grace-old-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	rotateTestVirtualKey(t, store, vk, "vk-grace-old-value", "vk-grace-new-value", time.Now().UTC().Add(5*time.Minute))

	byOld, err := store.GetVirtualKeyByValue(ctx, "vk-grace-old-value")
	require.NoError(t, err, "grace value inside the window must resolve")
	assert.Equal(t, "vk-grace-lookup", byOld.ID)

	byNew, err := store.GetVirtualKeyByValue(ctx, "vk-grace-new-value")
	require.NoError(t, err, "current value must keep resolving")
	assert.Equal(t, "vk-grace-lookup", byNew.ID)
}

func TestGetVirtualKeyByValue_GraceValueExpired(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-grace-expired",
		Name:     "Expired Grace Key",
		Value:    *schemas.NewSecretVar("vk-expired-old-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	rotateTestVirtualKey(t, store, vk, "vk-expired-old-value", "vk-expired-new-value", time.Now().UTC().Add(-time.Minute))

	_, err := store.GetVirtualKeyByValue(ctx, "vk-expired-old-value")
	require.ErrorIs(t, err, ErrNotFound, "expired grace value must not resolve")

	byNew, err := store.GetVirtualKeyByValue(ctx, "vk-expired-new-value")
	require.NoError(t, err)
	assert.Equal(t, "vk-grace-expired", byNew.ID)
}

func TestGetVirtualKeyByValue_UnknownValueStillNotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-known",
		Name:     "Known Key",
		Value:    *schemas.NewSecretVar("vk-known-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	_, err := store.GetVirtualKeyByValue(ctx, "vk-never-issued")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestGetVirtualKeyByValue_CurrentValueWinsOverOtherVKsGraceValue(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// VK-A rotates away from the shared value; the value stays alive in A's
	// grace window.
	vkA := &tables.TableVirtualKey{
		ID:       "vk-shared-a",
		Name:     "Shared A",
		Value:    *schemas.NewSecretVar("vk-shared-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vkA))
	rotateTestVirtualKey(t, store, vkA, "vk-shared-value", "vk-shared-a-new", time.Now().UTC().Add(5*time.Minute))

	// VK-B claims the same value as its current value (the previous-value hash
	// index is intentionally non-unique, so this is legal).
	vkB := &tables.TableVirtualKey{
		ID:       "vk-shared-b",
		Name:     "Shared B",
		Value:    *schemas.NewSecretVar("vk-shared-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vkB))

	got, err := store.GetVirtualKeyByValue(ctx, "vk-shared-value")
	require.NoError(t, err)
	assert.Equal(t, "vk-shared-b", got.ID, "a live current value must beat another VK's grace value")
}

// TestGetVirtualKeyByValue_ZeroCooldownRotationRevokesActiveGraceValue covers
// the revocation path: a VK carrying a live grace value is rotated again while
// the cooldown is disabled, so both retired values must stop resolving at once.
// Without the clear, the stale previous_value_hash row would keep authenticating
// until its original expiry.
func TestGetVirtualKeyByValue_ZeroCooldownRotationRevokesActiveGraceValue(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-revoke-now",
		Name:     "Revoke Now Key",
		Value:    *schemas.NewSecretVar("vk-revoke-first"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	// First rotation with a long cooldown: the first value is live in its window.
	rotateTestVirtualKey(t, store, vk, "vk-revoke-first", "vk-revoke-second", time.Now().UTC().Add(30*time.Minute))
	found, err := store.GetVirtualKeyByValue(ctx, "vk-revoke-first")
	require.NoError(t, err, "grace value must resolve before the zero-cooldown rotation")
	assert.Equal(t, "vk-revoke-now", found.ID)

	// Second rotation with the cooldown disabled: mirrors rotateVirtualKeyByID's
	// else-branch, which calls ClearPreviousValue instead of retiring the value.
	now := time.Now().UTC()
	second := &tables.TableVirtualKey{
		ID:        "vk-revoke-now",
		Name:      "Revoke Now Key",
		Value:     *schemas.NewSecretVar("vk-revoke-third"),
		IsActive:  schemas.Ptr(true),
		RotatedAt: &now,
	}
	second.ClearPreviousValue()
	require.NoError(t, store.UpdateVirtualKey(ctx, second))

	_, err = store.GetVirtualKeyByValue(ctx, "vk-revoke-first")
	require.ErrorIs(t, err, ErrNotFound, "the older retired value must be revoked immediately")
	_, err = store.GetVirtualKeyByValue(ctx, "vk-revoke-second")
	require.ErrorIs(t, err, ErrNotFound, "the just-retired value must be revoked immediately")

	current, err := store.GetVirtualKeyByValue(ctx, "vk-revoke-third")
	require.NoError(t, err, "the current value must keep working")
	assert.Equal(t, "vk-revoke-now", current.ID)

	stored, err := store.GetVirtualKey(ctx, "vk-revoke-now")
	require.NoError(t, err)
	assert.Empty(t, stored.PreviousValueHash, "the retired hash must not linger in the row")
	assert.Nil(t, stored.PreviousValueExpiresAt)
}

func TestGetVirtualKeyQuotaByValue_GraceValueWithinWindow(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-quota-grace",
		Name:     "Quota Grace Key",
		Value:    *schemas.NewSecretVar("vk-quota-old-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	require.NoError(t, store.CreateBudget(ctx, &tables.TableBudget{
		ID:            "budget-quota-grace",
		MaxLimit:      10,
		ResetDuration: "1h",
		VirtualKeyID:  schemas.Ptr("vk-quota-grace"),
	}))
	rotateTestVirtualKey(t, store, vk, "vk-quota-old-value", "vk-quota-new-value", time.Now().UTC().Add(5*time.Minute))

	got, err := store.GetVirtualKeyQuotaByValue(ctx, "vk-quota-old-value")
	require.NoError(t, err, "grace value inside the window must resolve quota")
	assert.Equal(t, "vk-quota-grace", got.ID)
	require.Len(t, got.Budgets, 1, "budgets must be preloaded for grace lookups")
	assert.Equal(t, "budget-quota-grace", got.Budgets[0].ID)
}

func TestGetVirtualKeyQuotaByValue_GraceValueExpired(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-quota-expired",
		Name:     "Quota Expired Key",
		Value:    *schemas.NewSecretVar("vk-quota-exp-old"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	rotateTestVirtualKey(t, store, vk, "vk-quota-exp-old", "vk-quota-exp-new", time.Now().UTC().Add(-time.Minute))

	_, err := store.GetVirtualKeyQuotaByValue(ctx, "vk-quota-exp-old")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDeleteVirtualKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-delete",
		Name:     "Delete Me",
		Value:    *schemas.NewSecretVar("vk-delete-value"),
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	err = store.DeleteVirtualKey(ctx, "vk-delete")
	require.NoError(t, err)

	_, err = store.GetVirtualKey(ctx, "vk-delete")
	assert.Error(t, err, "Should not find deleted virtual key")
}

func TestDeleteVirtualKey_RevokesInboundVKGrants(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-grant",
		Name:     "Grant VK",
		Value:    *schemas.NewSecretVar("vk-grant-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	// An active vk-mode inbound grant bound to this VK (vk-mode rows key bf_sub
	// to the VK id).
	rt := &tables.TableOAuth2RefreshToken{
		ID:        "rt-vk-grant",
		TokenHash: "hash-vk-grant",
		FamilyID:  "fam-vk-grant",
		ClientID:  "client-1",
		BfMode:    string(schemas.MCPAuthModeVK),
		BfSub:     vk.ID,
		Scope:     "mcp",
		Resource:  "https://example.test/mcp",
		CreatedAt: time.Now(),
	}
	require.NoError(t, store.DB().WithContext(ctx).Create(rt).Error)

	require.NoError(t, store.DeleteVirtualKey(ctx, vk.ID))

	var got tables.TableOAuth2RefreshToken
	require.NoError(t, store.DB().WithContext(ctx).First(&got, "id = ?", "rt-vk-grant").Error)
	assert.NotNil(t, got.RevokedAt, "vk-mode grant should be revoked when its VK is deleted")
}

func TestDeleteVirtualKey_CleansUpScopedModelConfigs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-scoped",
		Name:     "Scoped VK",
		Value:    *schemas.NewSecretVar("vk-scoped-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	budget := &tables.TableBudget{ID: "b-scoped", MaxLimit: 100, ResetDuration: "1h"}
	require.NoError(t, store.CreateBudget(ctx, budget))
	rateLimit := &tables.TableRateLimit{
		ID:                 "rl-scoped",
		TokenMaxLimit:      schemas.Ptr(int64(1000)),
		TokenResetDuration: schemas.Ptr("1h"),
	}
	require.NoError(t, store.CreateRateLimit(ctx, rateLimit))

	mc := &tables.TableModelConfig{
		ID:          "mc-scoped",
		ModelName:   "gpt-4",
		Scope:       tables.ModelConfigScopeVirtualKey,
		ScopeID:     schemas.Ptr(vk.ID),
		BudgetID:    &budget.ID,
		RateLimitID: &rateLimit.ID,
	}
	require.NoError(t, store.CreateModelConfig(ctx, mc))

	// Sanity: the scoped config exists before deletion.
	_, err := store.GetModelConfigByID(ctx, "mc-scoped")
	require.NoError(t, err)

	// Deleting the VK must cascade-clean its scoped model config and owned budget/rate-limit.
	require.NoError(t, store.DeleteVirtualKey(ctx, vk.ID))

	_, err = store.GetModelConfigByID(ctx, "mc-scoped")
	assert.Error(t, err, "scoped model config should be deleted with the VK")

	var budgetCount int64
	require.NoError(t, store.DB().Model(&tables.TableBudget{}).Where("id = ?", "b-scoped").Count(&budgetCount).Error)
	assert.Equal(t, int64(0), budgetCount, "owned budget should be deleted")

	var rlCount int64
	require.NoError(t, store.DB().Model(&tables.TableRateLimit{}).Where("id = ?", "rl-scoped").Count(&rlCount).Error)
	assert.Equal(t, int64(0), rlCount, "owned rate limit should be deleted")
}

// TestDeleteVirtualKey_CleansUpMultiBudgetScopedModelConfigs is a regression test for a
// leak where DeleteVirtualKey cleaned only the legacy single BudgetID column and ignored
// the modern multi-budget rows owned via TableBudget.ModelConfigID. Those budgets carry no
// virtual_key_id, so the VK's own budget sweep didn't catch them either — they orphaned.
func TestDeleteVirtualKey_CleansUpMultiBudgetScopedModelConfigs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	vk := &tables.TableVirtualKey{
		ID:       "vk-multibudget",
		Name:     "MultiBudget VK",
		Value:    *schemas.NewSecretVar("vk-multibudget-value"),
		IsActive: schemas.Ptr(true),
	}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	rateLimit := &tables.TableRateLimit{
		ID:                 "rl-mb",
		TokenMaxLimit:      schemas.Ptr(int64(1000)),
		TokenResetDuration: schemas.Ptr("1h"),
	}
	require.NoError(t, store.CreateRateLimit(ctx, rateLimit))

	mc := &tables.TableModelConfig{
		ID:          "mc-multibudget",
		ModelName:   "gpt-4",
		Scope:       tables.ModelConfigScopeVirtualKey,
		ScopeID:     schemas.Ptr(vk.ID),
		RateLimitID: &rateLimit.ID,
	}
	require.NoError(t, store.CreateModelConfig(ctx, mc))

	// Modern budgets are owned via ModelConfigID (no virtual_key_id), mirroring how the
	// governance handler creates them.
	mbBudgetIDs := []string{"b-mb-1", "b-mb-2"}
	for _, id := range mbBudgetIDs {
		require.NoError(t, store.CreateBudget(ctx, &tables.TableBudget{
			ID:            id,
			MaxLimit:      100,
			ResetDuration: "1h",
			ModelConfigID: &mc.ID,
		}))
	}

	require.NoError(t, store.DeleteVirtualKey(ctx, vk.ID))

	_, err := store.GetModelConfigByID(ctx, "mc-multibudget")
	assert.Error(t, err, "scoped model config should be deleted with the VK")

	for _, id := range mbBudgetIDs {
		var count int64
		require.NoError(t, store.DB().Model(&tables.TableBudget{}).Where("id = ?", id).Count(&count).Error)
		assert.Equal(t, int64(0), count, "modern model-config budget %s should be deleted, not orphaned", id)
	}

	var rlCount int64
	require.NoError(t, store.DB().Model(&tables.TableRateLimit{}).Where("id = ?", "rl-mb").Count(&rlCount).Error)
	assert.Equal(t, int64(0), rlCount, "owned rate limit should be deleted")
}

// TestCreateModelConfig_RejectsMissingScopeOwner verifies the scope-owner lock in
// CreateModelConfig: a virtual_key-scoped config whose scope_id points at a non-existent
// VK is rejected (ErrNotFound) rather than created as an orphan. This is the guard that
// closes the CreateModelConfig↔DeleteVirtualKey race.
func TestCreateModelConfig_RejectsMissingScopeOwner(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	mc := &tables.TableModelConfig{
		ID:        "mc-orphan",
		ModelName: "gpt-4",
		Scope:     tables.ModelConfigScopeVirtualKey,
		ScopeID:   schemas.Ptr("vk-does-not-exist"),
	}
	err := store.CreateModelConfig(ctx, mc)
	assert.ErrorIs(t, err, ErrNotFound, "creating a VK-scoped config for a missing VK should be rejected")

	_, getErr := store.GetModelConfigByID(ctx, "mc-orphan")
	assert.Error(t, getErr, "rejected config must not have been persisted")
}

func TestDeleteProvider_CleansUpProviderModelConfigs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	now := time.Now()
	require.NoError(t, store.DB().Create(&tables.TableProvider{Name: "openai", CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, store.DB().Create(&tables.TableBudget{ID: "pb", MaxLimit: 100, ResetDuration: "1M", LastReset: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, store.DB().Create(&tables.TableRateLimit{ID: "prl", TokenMaxLimit: schemas.Ptr(int64(1000)), TokenResetDuration: schemas.Ptr("1h"), TokenLastReset: now, RequestLastReset: now, CreatedAt: now, UpdatedAt: now}).Error)

	providerName := "openai"
	mc := &tables.TableModelConfig{
		ID:          "mc-wildcard",
		ModelName:   tables.ModelConfigAllModels,
		Provider:    &providerName,
		Scope:       tables.ModelConfigScopeGlobal,
		BudgetID:    schemas.Ptr("pb"),
		RateLimitID: schemas.Ptr("prl"),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	require.NoError(t, store.CreateModelConfig(ctx, mc))

	require.NoError(t, store.DeleteProvider(ctx, schemas.ModelProvider("openai")))

	// The provider's wildcard model config and its owned budget/rate-limit are cleaned up.
	_, err := store.GetModelConfigByID(ctx, "mc-wildcard")
	assert.Error(t, err, "provider-scoped model config should be deleted with the provider")
	for _, q := range []struct {
		model any
		id    string
		label string
	}{
		{&tables.TableBudget{}, "pb", "budget"},
		{&tables.TableRateLimit{}, "prl", "rate limit"},
	} {
		var count int64
		require.NoError(t, store.DB().Model(q.model).Where("id = ?", q.id).Count(&count).Error)
		assert.Equal(t, int64(0), count, "owned "+q.label+" should be deleted")
	}
}

// =============================================================================
// Virtual Key Provider Config Tests
// =============================================================================

func TestCreateVirtualKeyProviderConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create virtual key first
	vk := &tables.TableVirtualKey{
		ID:       "vk-for-pc",
		Name:     "VK For Provider Config",
		Value:    *schemas.NewSecretVar("vk-pc-value"),
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Create provider config
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-for-pc",
		Provider:     "openai",
		Weight:       &weight,
	}

	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	// Verify
	configs, err := store.GetVirtualKeyProviderConfigs(ctx, "vk-for-pc")
	require.NoError(t, err)
	assert.Len(t, configs, 1)
	assert.Equal(t, "openai", configs[0].Provider)
}

func TestCreateVirtualKeyProviderConfig_WithKeys(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create provider with keys first
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-for-pc", Name: "openai-pc-key", Value: *schemas.NewSecretVar("sk-test"), Weight: 1.0},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Create virtual key
	vk := &tables.TableVirtualKey{
		ID:       "vk-with-keys",
		Name:     "VK With Keys",
		Value:    *schemas.NewSecretVar("vk-keys-value"),
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Create provider config with key reference
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-with-keys",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "openai-pc-key"}, // Reference by name
		},
	}

	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	// Verify keys are associated
	configs, err := store.GetVirtualKeyProviderConfigs(ctx, "vk-with-keys")
	require.NoError(t, err)
	assert.Len(t, configs, 1)

	// Load with keys
	var configWithKeys tables.TableVirtualKeyProviderConfig
	err = store.DB().Preload("Keys").First(&configWithKeys, "id = ?", configs[0].ID).Error
	require.NoError(t, err)
	assert.Len(t, configWithKeys.Keys, 1)
}

// TestReplaceVirtualKeyProviderConfigsPreservesParity verifies bulk replacement keeps nested ownership and key associations.
func TestReplaceVirtualKeyProviderConfigsPreservesParity(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.UpdateProvidersConfig(ctx, map[schemas.ModelProvider]ProviderConfig{
		"openai": {Keys: []schemas.Key{{ID: "replacement-key", Name: "replacement-key", Value: *schemas.NewSecretVar("secret"), Weight: 1}}},
	}))
	vk := &tables.TableVirtualKey{ID: "vk-replace-pcs", Name: "replace", Value: *schemas.NewSecretVar("vk-replace"), IsActive: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	oldRateLimit := &tables.TableRateLimit{ID: "old-pc-rate-limit"}
	require.NoError(t, store.CreateRateLimit(ctx, oldRateLimit))
	old := &tables.TableVirtualKeyProviderConfig{VirtualKeyID: vk.ID, Provider: "anthropic", RateLimitID: &oldRateLimit.ID, RateLimit: oldRateLimit, Budgets: []tables.TableBudget{{ID: "old-pc-budget", MaxLimit: 1, ResetDuration: "1h"}}}
	require.NoError(t, store.CreateVirtualKeyProviderConfig(ctx, old))

	newRateLimit := &tables.TableRateLimit{ID: "new-pc-rate-limit"}
	tx := store.DB().WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	err := store.ReplaceVirtualKeyProviderConfigs(ctx, vk.ID, []tables.TableVirtualKeyProviderConfig{{
		Provider: "openai", Keys: []tables.TableKey{{KeyID: "replacement-key"}},
		Budgets: []tables.TableBudget{{ID: "new-pc-budget", MaxLimit: 10, ResetDuration: "1d"}}, RateLimitID: &newRateLimit.ID, RateLimit: newRateLimit,
	}}, tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit().Error)

	var configs []tables.TableVirtualKeyProviderConfig
	require.NoError(t, store.DB().Preload("Keys").Preload("Budgets").Preload("RateLimit").Where("virtual_key_id = ?", vk.ID).Find(&configs).Error)
	require.Len(t, configs, 1)
	require.Equal(t, "openai", configs[0].Provider)
	require.Len(t, configs[0].Keys, 1)
	require.Equal(t, "replacement-key", configs[0].Keys[0].KeyID)
	require.Len(t, configs[0].Budgets, 1)
	require.Equal(t, "new-pc-budget", configs[0].Budgets[0].ID)
	require.NotNil(t, configs[0].RateLimit)
	require.Equal(t, newRateLimit.ID, configs[0].RateLimit.ID)
	for _, id := range []string{"old-pc-budget", "old-pc-rate-limit"} {
		var count int64
		table := (&tables.TableBudget{}).TableName()
		if id == "old-pc-rate-limit" {
			table = (&tables.TableRateLimit{}).TableName()
		}
		require.NoError(t, store.DB().Table(table).Where("id = ?", id).Count(&count).Error)
		require.Zero(t, count)
	}
}

// TestReplaceVirtualKeyProviderConfigsRollsBackOnUnresolvedKey verifies replacement remains atomic on resolution failure.
func TestReplaceVirtualKeyProviderConfigsRollsBackOnUnresolvedKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	vk := &tables.TableVirtualKey{ID: "vk-replace-rollback", Name: "rollback", Value: *schemas.NewSecretVar("vk-rollback"), IsActive: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	require.NoError(t, store.CreateVirtualKeyProviderConfig(ctx, &tables.TableVirtualKeyProviderConfig{VirtualKeyID: vk.ID, Provider: "anthropic"}))

	tx := store.DB().WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	err := store.ReplaceVirtualKeyProviderConfigs(ctx, vk.ID, []tables.TableVirtualKeyProviderConfig{{Provider: "openai", Keys: []tables.TableKey{{KeyID: "missing"}}}}, tx)
	require.Error(t, err)
	require.NoError(t, tx.Rollback().Error)
	configs, listErr := store.GetVirtualKeyProviderConfigs(ctx, vk.ID)
	require.NoError(t, listErr)
	require.Len(t, configs, 1)
	require.Equal(t, "anthropic", configs[0].Provider)
}

// TestReplaceVirtualKeyProviderConfigsResolvesKeysByName verifies bulk replacement
// accepts the config-file reference forms that CreateVirtualKeyProviderConfig
// accepts: a name with no KeyID, and a KeyID that misses but carries a usable name.
func TestReplaceVirtualKeyProviderConfigsResolvesKeysByName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.UpdateProvidersConfig(ctx, map[schemas.ModelProvider]ProviderConfig{
		"openai":    {Keys: []schemas.Key{{ID: "openai-key-id", Name: "openai-name", Value: *schemas.NewSecretVar("s1"), Weight: 1}}},
		"anthropic": {Keys: []schemas.Key{{ID: "anthropic-key-id", Name: "anthropic-name", Value: *schemas.NewSecretVar("s2"), Weight: 1}}},
	}))
	vk := &tables.TableVirtualKey{ID: "vk-replace-by-name", Name: "by-name", Value: *schemas.NewSecretVar("vk-by-name"), IsActive: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	tx := store.DB().WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	require.NoError(t, store.ReplaceVirtualKeyProviderConfigs(ctx, vk.ID, []tables.TableVirtualKeyProviderConfig{
		// Name only, no KeyID — the config-file form.
		{Provider: "openai", Keys: []tables.TableKey{{Name: "openai-name"}}},
		// KeyID that does not resolve, but the name does — the fallback path.
		{Provider: "anthropic", Keys: []tables.TableKey{{KeyID: "stale-id", Name: "anthropic-name"}}},
	}, tx))
	require.NoError(t, tx.Commit().Error)

	var configs []tables.TableVirtualKeyProviderConfig
	require.NoError(t, store.DB().Preload("Keys").Where("virtual_key_id = ?", vk.ID).Order("provider").Find(&configs).Error)
	require.Len(t, configs, 2)
	// Resolution is scoped per provider, so each config must land on its own
	// provider's key rather than whichever row happened to match the name first.
	require.Equal(t, "anthropic", configs[0].Provider)
	require.Len(t, configs[0].Keys, 1)
	require.Equal(t, "anthropic-key-id", configs[0].Keys[0].KeyID)
	require.Equal(t, "openai", configs[1].Provider)
	require.Len(t, configs[1].Keys, 1)
	require.Equal(t, "openai-key-id", configs[1].Keys[0].KeyID)
}

// TestReplaceVirtualKeyProviderConfigsReportsEveryUnresolvedKey verifies a blank
// reference is aggregated with the others rather than short-circuiting the scan.
func TestReplaceVirtualKeyProviderConfigsReportsEveryUnresolvedKey(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	vk := &tables.TableVirtualKey{ID: "vk-replace-unresolved", Name: "unresolved", Value: *schemas.NewSecretVar("vk-unresolved"), IsActive: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	tx := store.DB().WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	err := store.ReplaceVirtualKeyProviderConfigs(ctx, vk.ID, []tables.TableVirtualKeyProviderConfig{
		{Provider: "openai", Keys: []tables.TableKey{{}, {KeyID: "missing-id"}, {Name: "missing-name"}}},
	}, tx)
	require.NoError(t, tx.Rollback().Error)
	var unresolvedErr *ErrUnresolvedKeys
	require.ErrorAs(t, err, &unresolvedErr)
	require.ElementsMatch(t, []string{"key[0]", "key_id=missing-id", "name=missing-name"}, unresolvedErr.Identifiers)
}

// TestReplaceVirtualKeyProviderConfigsChunksInserts verifies replacement crosses both bounded insert sizes.
func TestReplaceVirtualKeyProviderConfigsChunksInserts(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	vk := &tables.TableVirtualKey{ID: "vk-replace-chunks", Name: "chunks", Value: *schemas.NewSecretVar("vk-chunks"), IsActive: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))
	configs := make([]tables.TableVirtualKeyProviderConfig, 101)
	for i := range configs {
		configs[i] = tables.TableVirtualKeyProviderConfig{Provider: fmt.Sprintf("provider-%03d", i)}
	}
	createStatements := 0
	callbackName := "test:count-provider-config-inserts"
	require.NoError(t, store.DB().Callback().Create().After("gorm:create").Register(callbackName, func(db *gorm.DB) {
		if db.Statement.Table == (&tables.TableVirtualKeyProviderConfig{}).TableName() {
			createStatements++
		}
	}))
	t.Cleanup(func() { _ = store.DB().Callback().Create().Remove(callbackName) })
	tx := store.DB().WithContext(ctx).Begin()
	require.NoError(t, tx.Error)
	require.NoError(t, store.ReplaceVirtualKeyProviderConfigs(ctx, vk.ID, configs, tx))
	require.NoError(t, tx.Commit().Error)
	var count int64
	require.NoError(t, store.DB().Model(&tables.TableVirtualKeyProviderConfig{}).Where("virtual_key_id = ?", vk.ID).Count(&count).Error)
	require.Equal(t, int64(101), count)
	require.Equal(t, 2, createStatements, "101 configs should use two bounded insert statements instead of 101 row inserts")
}

func TestCreateVirtualKeyProviderConfig_UnresolvedKeys(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create virtual key
	vk := &tables.TableVirtualKey{
		ID:       "vk-unresolved",
		Name:     "VK Unresolved",
		Value:    *schemas.NewSecretVar("vk-unresolved-value"),
		IsActive: schemas.Ptr(true),
	}
	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Try to create provider config with non-existent key
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-unresolved",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "non-existent-key"},
		},
	}

	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	assert.Error(t, err, "Should fail with unresolved keys")

	var unresolvedErr *ErrUnresolvedKeys
	assert.ErrorAs(t, err, &unresolvedErr, "Should be ErrUnresolvedKeys")
}

func TestUpdateProvider_RemovesStaleVirtualKeyProviderConfigKeyAssociations(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-a", Name: "openai-key-a", Value: *schemas.NewSecretVar("sk-a"), Weight: 1.0},
				{ID: "key-b", Name: "openai-key-b", Value: *schemas.NewSecretVar("sk-b"), Weight: 1.0},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	vk := &tables.TableVirtualKey{
		ID:       "vk-update-provider-cleanup",
		Name:     "VK Update Provider Cleanup",
		Value:    *schemas.NewSecretVar("vk-update-provider-cleanup-value"),
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-update-provider-cleanup",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "openai-key-b"},
		},
	}
	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	updatedProviderConfig := ProviderConfig{
		Keys: []schemas.Key{
			{ID: "key-a", Name: "openai-key-a", Value: *schemas.NewSecretVar("sk-a"), Weight: 1.0},
		},
	}
	err = store.UpdateProvider(ctx, "openai", updatedProviderConfig)
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-update-provider-cleanup")
	require.NoError(t, err)
	require.Len(t, result.ProviderConfigs, 1)
	assert.Equal(t, "openai", result.ProviderConfigs[0].Provider)
	assert.False(t, result.ProviderConfigs[0].AllowAllKeys)
	assert.Empty(t, result.ProviderConfigs[0].Keys)
}

func TestDeleteProvider_RemovesVirtualKeyProviderConfigs(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{{ID: "key-delete", Name: "openai-key-delete", Value: *schemas.NewSecretVar("sk-delete"), Weight: 1.0}},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	vk := &tables.TableVirtualKey{
		ID:       "vk-delete-provider-cleanup",
		Name:     "VK Delete Provider Cleanup",
		Value:    *schemas.NewSecretVar("vk-delete-provider-cleanup-value"),
		IsActive: schemas.Ptr(true),
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-delete-provider-cleanup",
		Provider:     "openai",
		Weight:       &weight,
	}
	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	err = store.DeleteProvider(ctx, "openai")
	require.NoError(t, err)

	result, err := store.GetVirtualKey(ctx, "vk-delete-provider-cleanup")
	require.NoError(t, err)
	assert.Empty(t, result.ProviderConfigs)
}

// =============================================================================
// Client Config Tests
// =============================================================================

func TestUpdateClientConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	config := &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
	}

	err := store.UpdateClientConfig(ctx, config)
	require.NoError(t, err)

	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.True(t, result.EnableLogging != nil && *result.EnableLogging)
	assert.Equal(t, 100, result.InitialPoolSize)
}

func TestUpdateClientConfig_VKRotationCooldownRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
		VKRotationCooldown:   schemas.Duration(5 * time.Minute),
	})
	require.NoError(t, err)

	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, result.VKRotationCooldown.D())
}

func TestUpdateClientConfig_EnforceVKObjectOwnershipRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// config.json without the field enforces ownership.
	var fromJSON ClientConfig
	require.NoError(t, json.Unmarshal([]byte(`{"initial_pool_size":100}`), &fromJSON))
	assert.True(t, fromJSON.EnforceVKObjectOwnership, "an omitted field keeps ownership enforced")
	require.NoError(t, json.Unmarshal([]byte(`{"enforce_vk_object_ownership":false}`), &fromJSON))
	assert.False(t, fromJSON.EnforceVKObjectOwnership, "an explicit false turns it off")

	on := ClientConfig{EnableLogging: new(true), InitialPoolSize: 100, LogRetentionDays: 30, MaxRequestBodySizeMB: 50, EnforceVKObjectOwnership: true}
	require.NoError(t, store.UpdateClientConfig(ctx, &on))
	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.True(t, result.EnforceVKObjectOwnership)

	// The column defaults to true, so the write must force an explicit false through.
	off := on
	off.EnforceVKObjectOwnership = false
	require.NoError(t, store.UpdateClientConfig(ctx, &off))
	result, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.False(t, result.EnforceVKObjectOwnership, "an explicit false survives the delete-and-create write")

	// Only the opt-out is hashed, so configs written before the field existed keep their hash.
	onHash, err := on.GenerateClientConfigHash()
	require.NoError(t, err)
	offHash, err := off.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, onHash, offHash)
}

func TestUpdateClientConfig_ExtraFieldsPlacementRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Unset reads back as the column default, so existing installs keep extra_fields in the body.
	base := ClientConfig{EnableLogging: new(true), InitialPoolSize: 100, LogRetentionDays: 30, MaxRequestBodySizeMB: 50, EnforceVKObjectOwnership: true}
	require.NoError(t, store.UpdateClientConfig(ctx, &base))
	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, tables.ExtraFieldsPlacementBody, result.ExtraFieldsPlacement)

	headers := base
	headers.ExtraFieldsPlacement = tables.ExtraFieldsPlacementHeaders
	require.NoError(t, store.UpdateClientConfig(ctx, &headers))
	result, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, tables.ExtraFieldsPlacementHeaders, result.ExtraFieldsPlacement)

	// Only the non-default value is hashed, so configs written before the field existed keep their hash.
	baseHash, err := base.GenerateClientConfigHash()
	require.NoError(t, err)
	body := base
	body.ExtraFieldsPlacement = tables.ExtraFieldsPlacementBody
	bodyHash, err := body.GenerateClientConfigHash()
	require.NoError(t, err)
	headersHash, err := headers.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.Equal(t, baseHash, bodyHash)
	assert.NotEqual(t, baseHash, headersHash)
}

func TestUpdateClientConfig_MCPCodeModeLimitsRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	base := func(limits *schemas.MCPCodeModeLimits) *ClientConfig {
		return &ClientConfig{
			EnableLogging:        new(true),
			InitialPoolSize:      100,
			LogRetentionDays:     30,
			MaxRequestBodySizeMB: 50,
			MCPCodeModeLimits:    limits,
		}
	}
	limits := &schemas.MCPCodeModeLimits{MaxSteps: 5_000_000, MaxToolCalls: 500, MaxNestingDepth: 128}

	require.NoError(t, store.UpdateClientConfig(ctx, base(limits)))
	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, limits, result.MCPCodeModeLimits)

	mcpConfig, err := store.GetMCPConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, mcpConfig)
	require.NotNil(t, mcpConfig.ToolManagerConfig)
	assert.Equal(t, limits, mcpConfig.ToolManagerConfig.CodeModeLimits, "the MCP config built from the store must carry the limits")

	require.NoError(t, store.UpdateClientConfig(ctx, base(nil)))
	result, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Nil(t, result.MCPCodeModeLimits, "clearing the limits must persist")
}

func TestGenerateClientConfigHash_MCPCodeModeLimits(t *testing.T) {
	hash := func(limits *schemas.MCPCodeModeLimits) string {
		h, err := (&ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, MCPCodeModeLimits: limits}).GenerateClientConfigHash()
		require.NoError(t, err)
		return h
	}
	base := hash(nil)
	baseHash, err := (&ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30}).GenerateClientConfigHash()
	require.NoError(t, err)
	assert.Equal(t, baseHash, base, "unset limits must not change existing hashes")
	assert.NotEqual(t, base, hash(&schemas.MCPCodeModeLimits{MaxSteps: 5}))
	assert.NotEqual(t, hash(&schemas.MCPCodeModeLimits{MaxSteps: 5}), hash(&schemas.MCPCodeModeLimits{MaxSteps: 6}))

	cc := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30}
	withoutLimits, err := cc.GenerateClientConfigHashWithToolManager(&schemas.MCPToolManagerConfig{MaxAgentDepth: 10})
	require.NoError(t, err)
	withLimits, err := cc.GenerateClientConfigHashWithToolManager(&schemas.MCPToolManagerConfig{MaxAgentDepth: 10, CodeModeLimits: &schemas.MCPCodeModeLimits{MaxToolCalls: 5}})
	require.NoError(t, err)
	assert.NotEqual(t, withoutLimits, withLimits, "tool_manager_config.code_mode_limits must take part in the file hash")
}

func TestGenerateClientConfigHash_OAuth2ServerConfigPolicyFields(t *testing.T) {
	hash := func(oc *tables.OAuth2ServerConfig) string {
		h, err := (&ClientConfig{InitialPoolSize: 100, MCPServerAuthMode: tables.MCPServerAuthModeBoth, OAuth2ServerConfig: oc}).GenerateClientConfigHash()
		require.NoError(t, err)
		return h
	}
	base := func() *tables.OAuth2ServerConfig {
		return &tables.OAuth2ServerConfig{IssuerURL: schemas.NewSecretVar("https://issuer.example.com"), AuthCodeTTL: 300, AccessTokenTTL: 600}
	}
	baseHash := hash(base())

	empty := base()
	empty.AllowedRedirectURIs = []string{}
	assert.Equal(t, baseHash, hash(empty), "an empty redirect list must not change existing hashes")

	withRedirect := base()
	withRedirect.AllowedRedirectURIs = []string{"https://www.cursor.com/agents/mcp/oauth/callback"}
	assert.NotEqual(t, baseHash, hash(withRedirect), "adding allowed_redirect_uris must change the hash")

	otherRedirect := base()
	otherRedirect.AllowedRedirectURIs = []string{"https://app.example.com/callback"}
	assert.NotEqual(t, hash(withRedirect), hash(otherRedirect), "changing allowed_redirect_uris must change the hash")

	disabledVK := base()
	disabledVK.DisableVKIdentity = true
	assert.NotEqual(t, baseHash, hash(disabledVK), "setting disable_vk_identity must change the hash")
}

func TestUpdateClientConfig_CompatAzureDeepseekRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	base := func(azureDeepseek bool) *ClientConfig {
		return &ClientConfig{
			EnableLogging:        new(true),
			InitialPoolSize:      100,
			LogRetentionDays:     30,
			MaxRequestBodySizeMB: 50,
			Compat:               CompatConfig{AzureDeepseek: azureDeepseek},
		}
	}

	require.NoError(t, store.UpdateClientConfig(ctx, base(false)))
	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.False(t, result.Compat.AzureDeepseek, "disabling the toggle must persist")

	require.NoError(t, store.UpdateClientConfig(ctx, base(true)))
	result, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.True(t, result.Compat.AzureDeepseek, "re-enabling the toggle must persist")
}

func TestUpdateClientConfig_CompatForceReasoningOnlyModelsToResponsesRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	base := func(enabled bool) *ClientConfig {
		return &ClientConfig{
			EnableLogging:        new(true),
			InitialPoolSize:      100,
			LogRetentionDays:     30,
			MaxRequestBodySizeMB: 50,
			Compat:               CompatConfig{ForceReasoningOnlyModelsToResponses: enabled},
		}
	}

	require.NoError(t, store.UpdateClientConfig(ctx, base(false)))
	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.False(t, result.Compat.ForceReasoningOnlyModelsToResponses, "disabling the toggle must persist")

	require.NoError(t, store.UpdateClientConfig(ctx, base(true)))
	result, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.True(t, result.Compat.ForceReasoningOnlyModelsToResponses, "re-enabling the toggle must persist")
}

func TestGenerateClientConfigHash_CompatForceReasoningOnlyModelsToResponses(t *testing.T) {
	on := &ClientConfig{InitialPoolSize: 100, Compat: CompatConfig{ForceReasoningOnlyModelsToResponses: true}}
	off := &ClientConfig{InitialPoolSize: 100}
	onHash, err := on.GenerateClientConfigHash()
	require.NoError(t, err)
	offHash, err := off.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, onHash, offHash, "toggling force_reasoning_only_models_to_responses must change the hash")
}

func TestGenerateClientConfigHash_VKRotationCooldown(t *testing.T) {
	base := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30}
	baseHash, err := base.GenerateClientConfigHash()
	require.NoError(t, err)

	// Zero cooldown must not change the hash: existing deployments see no
	// config drift after upgrade.
	zero := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, VKRotationCooldown: 0}
	zeroHash, err := zero.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.Equal(t, baseHash, zeroHash)

	// A non-zero cooldown is a meaningful config change.
	withCooldown := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, VKRotationCooldown: schemas.Duration(5 * time.Minute)}
	cooldownHash, err := withCooldown.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, cooldownHash)
}

func TestGenerateClientConfigHash_A2AExternalClientURL(t *testing.T) {
	base := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30}
	baseHash, err := base.GenerateClientConfigHash()
	require.NoError(t, err)

	// Explicit clearing must change the hash so file reconciliation disables
	// an A2A URL previously configured through the dashboard.
	unset := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, A2AExternalClientURL: schemas.NewSecretVar("")}
	unsetHash, err := unset.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, unsetHash)

	// A configured URL is a meaningful config change.
	withURL := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, A2AExternalClientURL: schemas.NewSecretVar("https://bifrost.example.com")}
	urlHash, err := withURL.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, urlHash)
}

func TestClientConfigVKRotationCooldown_UnmarshalDurationString(t *testing.T) {
	var cfg ClientConfig
	require.NoError(t, json.Unmarshal([]byte(`{"vk_rotation_cooldown": "5m"}`), &cfg))
	assert.Equal(t, 5*time.Minute, cfg.VKRotationCooldown.D())

	var cfgInt ClientConfig
	require.NoError(t, json.Unmarshal([]byte(`{"vk_rotation_cooldown": 300000000000}`), &cfgInt))
	assert.Equal(t, 5*time.Minute, cfgInt.VKRotationCooldown.D())

	var cfgAbsent ClientConfig
	require.NoError(t, json.Unmarshal([]byte(`{}`), &cfgAbsent))
	assert.Equal(t, time.Duration(0), cfgAbsent.VKRotationCooldown.D())
}

func TestUpdateClientConfig_PreservesMetadataAcrossSync(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpdateClientConfig(ctx, &ClientConfig{EnableLogging: new(true), LogRetentionDays: 30}))
	require.NoError(t, store.UpdateClientMetadata(ctx, map[string]any{"onboarding_dismissed": true, "onboarding_skipped": []any{"scim"}}))

	require.NoError(t, store.UpdateClientConfig(ctx, &ClientConfig{EnableLogging: new(false), LogRetentionDays: 7}))

	metadata, err := store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, true, metadata["onboarding_dismissed"])
	assert.Equal(t, []any{"scim"}, metadata["onboarding_skipped"])
	cfg, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.LogRetentionDays)
}

func TestUpdateClientMetadata(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding_dismissed": true,
		"theme":                "dark",
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{
		"theme": "light",
		"stale": nil,
	})
	require.NoError(t, err)

	metadata, err := store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, true, metadata["onboarding_dismissed"])
	assert.Equal(t, "light", metadata["theme"])
	assert.NotContains(t, metadata, "stale")

	err = store.UpdateClientMetadata(ctx, map[string]any{"theme": nil})
	require.NoError(t, err)

	metadata, err = store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.NotContains(t, metadata, "theme")
	assert.Equal(t, true, metadata["onboarding_dismissed"])

	// Nested objects must be merged recursively (RFC 7386), not replaced
	// wholesale, so sibling keys survive a partial nested patch.
	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding": map[string]any{"dismissed": true, "step": "a"},
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding": map[string]any{"step": "b"},
	})
	require.NoError(t, err)

	metadata, err = store.GetClientMetadata(ctx)
	require.NoError(t, err)
	onboarding, ok := metadata["onboarding"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, onboarding["dismissed"], "sibling key must survive nested patch")
	assert.Equal(t, "b", onboarding["step"])

	// A nil nested value deletes just that nested key.
	err = store.UpdateClientMetadata(ctx, map[string]any{
		"onboarding": map[string]any{"dismissed": nil},
	})
	require.NoError(t, err)

	metadata, err = store.GetClientMetadata(ctx)
	require.NoError(t, err)
	onboarding, ok = metadata["onboarding"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, onboarding, "dismissed")
	assert.Equal(t, "b", onboarding["step"])
}

func TestUpdateClientMetadataRequiresClientConfig(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientMetadata(ctx, map[string]any{"onboarding_dismissed": true})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotFound)

	var count int64
	err = store.DB().WithContext(ctx).Model(&tables.TableClientConfig{}).Count(&count).Error
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestUpdateClientConfigPreservesMetadata(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      100,
		LogRetentionDays:     30,
		MaxRequestBodySizeMB: 50,
	})
	require.NoError(t, err)

	err = store.UpdateClientMetadata(ctx, map[string]any{"onboarding_dismissed": true})
	require.NoError(t, err)

	err = store.UpdateClientConfig(ctx, &ClientConfig{
		EnableLogging:        new(true),
		InitialPoolSize:      200,
		LogRetentionDays:     60,
		MaxRequestBodySizeMB: 100,
	})
	require.NoError(t, err)

	config, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, 200, config.InitialPoolSize)

	metadata, err := store.GetClientMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, true, metadata["onboarding_dismissed"])
}

// =============================================================================
// Transaction Tests
// =============================================================================

func TestExecuteTransaction_Success(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Create budget in transaction
		budget := &tables.TableBudget{
			ID:            "tx-budget",
			MaxLimit:      100.0,
			ResetDuration: "1h",
		}
		return tx.Create(budget).Error
	})
	require.NoError(t, err)

	// Verify budget was created
	result, err := store.GetBudget(ctx, "tx-budget")
	require.NoError(t, err)
	assert.Equal(t, "tx-budget", result.ID)
}

func TestExecuteTransaction_Rollback(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.ExecuteTransaction(ctx, func(tx *gorm.DB) error {
		// Create budget
		budget := &tables.TableBudget{
			ID:            "tx-rollback-budget",
			MaxLimit:      100.0,
			ResetDuration: "1h",
		}
		if err := tx.Create(budget).Error; err != nil {
			return err
		}

		// Force error to trigger rollback
		return assert.AnError
	})
	assert.Error(t, err)

	// Verify budget was NOT created (rolled back)
	_, err = store.GetBudget(ctx, "tx-rollback-budget")
	assert.Error(t, err, "Budget should not exist after rollback")
}

// =============================================================================
// Customer and Team Tests
// =============================================================================

func TestCreateCustomer(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	customer := &tables.TableCustomer{
		ID:   "customer-test",
		Name: "Test Customer",
	}

	err := store.CreateCustomer(ctx, customer)
	require.NoError(t, err)

	result, err := store.GetCustomer(ctx, "customer-test")
	require.NoError(t, err)
	assert.Equal(t, "Test Customer", result.Name)
}

func TestCreateTeam(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create customer first
	customer := &tables.TableCustomer{
		ID:   "customer-for-team",
		Name: "Customer For Team",
	}
	err := store.CreateCustomer(ctx, customer)
	require.NoError(t, err)

	// Create team
	customerID := "customer-for-team"
	team := &tables.TableTeam{
		ID:         "team-test",
		Name:       "Test Team",
		CustomerID: &customerID,
	}

	err = store.CreateTeam(ctx, team)
	require.NoError(t, err)

	result, err := store.GetTeam(ctx, "team-test")
	require.NoError(t, err)
	assert.Equal(t, "Test Team", result.Name)
	assert.Equal(t, "customer-for-team", *result.CustomerID)
}

// =============================================================================
// Ping and Health Tests
// =============================================================================

func TestPing(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	err := store.Ping(ctx)
	assert.NoError(t, err)
}

// =============================================================================
// Error Handling Tests
// =============================================================================

func TestGetBudget_NotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetBudget(ctx, "non-existent-budget")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetVirtualKey_NotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetVirtualKey(ctx, "non-existent-vk")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetRateLimit_NotFound(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	_, err := store.GetRateLimit(ctx, "non-existent-rate-limit")
	assert.ErrorIs(t, err, ErrNotFound)
}

// =============================================================================
// Plugin Tests
// =============================================================================

func TestCreateAndGetPlugin(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	plugin := &tables.TablePlugin{
		Name:    "test-plugin",
		Enabled: true,
	}

	err := store.CreatePlugin(ctx, plugin)
	require.NoError(t, err)

	result, err := store.GetPlugin(ctx, "test-plugin")
	require.NoError(t, err)
	assert.Equal(t, "test-plugin", result.Name)
	assert.True(t, result.Enabled)
}

func TestUpsertPlugin(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create plugin
	plugin := &tables.TablePlugin{
		Name:    "upsert-plugin",
		Enabled: true,
	}
	err := store.UpsertPlugin(ctx, plugin)
	require.NoError(t, err)

	// Upsert with update
	plugin.Enabled = false
	err = store.UpsertPlugin(ctx, plugin)
	require.NoError(t, err)

	result, err := store.GetPlugin(ctx, "upsert-plugin")
	require.NoError(t, err)
	assert.False(t, result.Enabled)
}

// TestUpdatePluginPreservesPlacementAndOrder verifies that a UI/API edit, which sends neither
// field because the UI has no control over them, does not clear the stored placement/order.
func TestUpdatePluginPreservesPlacementAndOrder(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	preBuiltin := schemas.PluginPlacementPreBuiltin
	order7 := 7
	require.NoError(t, store.CreatePlugin(ctx, &tables.TablePlugin{
		Name: "ordering-plugin", Enabled: true, Placement: &preBuiltin, Order: &order7,
		Config: map[string]any{"setting": "old"},
	}))

	require.NoError(t, store.UpdatePlugin(ctx, &tables.TablePlugin{
		Name: "ordering-plugin", Enabled: true, Config: map[string]any{"setting": "new"},
	}))

	result, err := store.GetPlugin(ctx, "ordering-plugin")
	require.NoError(t, err)
	require.NotNil(t, result.Placement)
	assert.Equal(t, preBuiltin, *result.Placement)
	require.NotNil(t, result.Order)
	assert.Equal(t, 7, *result.Order)
	assert.Equal(t, map[string]any{"setting": "new"}, result.Config, "the edit itself must still apply")
}

// TestUpdatePluginPreservesSyncState verifies that a UI/API edit, which sets neither field,
// keeps the stored config hash and version. Clearing either makes the next startup read
// config.json as changed and revert the edit.
func TestUpdatePluginPreservesSyncState(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreatePlugin(ctx, &tables.TablePlugin{
		Name:       "sync-state-plugin",
		Enabled:    true,
		Version:    3,
		ConfigHash: "hash-from-last-file-sync",
		Config:     map[string]any{"setting": "file-value"},
	}))

	// A UI/API edit: only the fields the form knows about.
	require.NoError(t, store.UpdatePlugin(ctx, &tables.TablePlugin{
		Name:    "sync-state-plugin",
		Enabled: true,
		Config:  map[string]any{"setting": "ui-value"},
	}))

	result, err := store.GetPlugin(ctx, "sync-state-plugin")
	require.NoError(t, err)
	assert.Equal(t, "hash-from-last-file-sync", result.ConfigHash)
	assert.Equal(t, int16(3), result.Version)
	assert.Equal(t, map[string]any{"setting": "ui-value"}, result.Config)
}

// =============================================================================
// Integration Test: Full Virtual Key with Provider Config Flow
// =============================================================================

func TestFullVirtualKeyFlow(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Step 1: Create provider with keys
	providers := map[schemas.ModelProvider]ProviderConfig{
		"openai": {
			Keys: []schemas.Key{
				{ID: "key-1", Name: "openai-main", Value: *schemas.NewSecretVar("sk-main"), Weight: 1.0},
				{ID: "key-2", Name: "openai-backup", Value: *schemas.NewSecretVar("sk-backup"), Weight: 0.5},
			},
		},
	}
	err := store.UpdateProvidersConfig(ctx, providers)
	require.NoError(t, err)

	// Step 2: Create budget
	budget := &tables.TableBudget{
		ID:            "integration-budget",
		MaxLimit:      500.0,
		ResetDuration: "1M",
	}
	err = store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Step 3: Create rate limit
	tokenMax := int64(1000000)
	tokenDuration := "1d"
	rateLimit := &tables.TableRateLimit{
		ID:                 "integration-rate-limit",
		TokenMaxLimit:      &tokenMax,
		TokenResetDuration: &tokenDuration,
	}
	err = store.CreateRateLimit(ctx, rateLimit)
	require.NoError(t, err)

	// Step 4: Create virtual key
	rateLimitID := "integration-rate-limit"
	integrationVKID := "integration-vk"
	vk := &tables.TableVirtualKey{
		ID:          integrationVKID,
		Name:        "Integration Virtual Key",
		Value:       *schemas.NewSecretVar("vk-integration-xyz"),
		IsActive:    schemas.Ptr(true),
		RateLimitID: &rateLimitID,
	}
	err = store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Link the existing budget to the VK via FK
	budget.VirtualKeyID = &integrationVKID
	err = store.UpdateBudget(ctx, budget)
	require.NoError(t, err)

	// Step 5: Create provider config with key reference
	weight := 1.0
	pc := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "integration-vk",
		Provider:     "openai",
		Weight:       &weight,
		Keys: []tables.TableKey{
			{Name: "openai-main"},
		},
	}
	err = store.CreateVirtualKeyProviderConfig(ctx, pc)
	require.NoError(t, err)

	// Step 6: Verify complete setup
	result, err := store.GetVirtualKey(ctx, "integration-vk")
	require.NoError(t, err)
	assert.Equal(t, "Integration Virtual Key", result.Name)
	assert.Len(t, result.Budgets, 1)
	assert.NotNil(t, result.RateLimitID)

	configs, err := store.GetVirtualKeyProviderConfigs(ctx, "integration-vk")
	require.NoError(t, err)
	assert.Len(t, configs, 1)
	assert.Equal(t, "openai", configs[0].Provider)
}

// TestGetVirtualKeysUsesInternalPagination verifies that the unpaginated
// virtual-key API still returns every row when the result spans multiple
// internal preload pages.
func TestGetVirtualKeysUsesInternalPagination(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	totalVirtualKeys := virtualKeyInternalPageSize + 5
	createdAt := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < totalVirtualKeys; i++ {
		vk := &tables.TableVirtualKey{
			ID:        fmt.Sprintf("vk-page-%04d", i),
			Name:      fmt.Sprintf("Virtual Key %04d", i),
			Value:     *schemas.NewSecretVar(fmt.Sprintf("vk-value-%04d", i)),
			IsActive:  schemas.Ptr(true),
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}
		require.NoError(t, store.CreateVirtualKey(ctx, vk))
	}

	virtualKeys, err := store.GetVirtualKeys(ctx)
	require.NoError(t, err)
	require.Len(t, virtualKeys, totalVirtualKeys)
	require.Equal(t, "vk-page-0000", virtualKeys[0].ID)
	require.Equal(t, fmt.Sprintf("vk-page-%04d", totalVirtualKeys-1), virtualKeys[len(virtualKeys)-1].ID)
}

// =============================================================================
// Helper function tests
// =============================================================================

func TestGetWeight(t *testing.T) {
	// Test nil weight returns default
	assert.Equal(t, 1.0, getWeight(nil))

	// Test explicit weight
	w := 2.5
	assert.Equal(t, 2.5, getWeight(&w))

	// Test zero weight
	zero := 0.0
	assert.Equal(t, 0.0, getWeight(&zero))
}

// =============================================================================
// Concurrent Access Tests
// =============================================================================

func TestMultipleBudgetUpdates(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Create initial budget
	budget := &tables.TableBudget{
		ID:            "multi-update-budget",
		MaxLimit:      100.0,
		ResetDuration: "1h",
		CurrentUsage:  0,
	}
	err := store.CreateBudget(ctx, budget)
	require.NoError(t, err)

	// Simulate multiple sequential updates
	for i := 0; i < 10; i++ {
		b := &tables.TableBudget{
			ID:            "multi-update-budget",
			MaxLimit:      100.0 + float64(i),
			ResetDuration: "1h",
		}
		err := store.UpdateBudget(ctx, b)
		require.NoError(t, err)
	}

	// Verify budget exists and has the last value
	result, err := store.GetBudget(ctx, "multi-update-budget")
	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, 109.0, result.MaxLimit) // 100 + 9
}

// =============================================================================
// Duration Validation Tests (for budgets and rate limits)
// =============================================================================

func TestBudgetDurationFormats(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	validDurations := []string{"30s", "5m", "1h", "1d", "1w", "1M", "1Y"}

	for i, duration := range validDurations {
		budget := &tables.TableBudget{
			ID:            "budget-duration-" + string(rune('a'+i)),
			MaxLimit:      100.0,
			ResetDuration: duration,
		}
		err := store.CreateBudget(ctx, budget)
		assert.NoError(t, err, "Duration %s should be valid", duration)
	}
}

func TestRateLimitDurationFormats(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	validDurations := []string{"30s", "5m", "1h", "1d", "1w", "1M", "1Y"}

	for i, duration := range validDurations {
		tokenMax := int64(1000)
		rateLimit := &tables.TableRateLimit{
			ID:                 "rate-limit-duration-" + string(rune('a'+i)),
			TokenMaxLimit:      &tokenMax,
			TokenResetDuration: &duration,
		}
		err := store.CreateRateLimit(ctx, rateLimit)
		assert.NoError(t, err, "Duration %s should be valid", duration)
	}
}

// =============================================================================
// Prompt Deletion Tests
// =============================================================================

// testPromptTree holds IDs of entities created by createTestPromptTree for verification
type testPromptTree struct {
	FolderID   string
	PromptIDs  []string
	VersionIDs []uint
	SessionIDs []uint
}

// createTestPromptTree creates a folder with 2 prompts, each having 2 versions (with messages) and 1 session (with messages).
func createTestPromptTree(t *testing.T, store *RDBConfigStore, ctx context.Context) testPromptTree {
	t.Helper()

	tree := testPromptTree{}

	// Create folder
	folder := &tables.TableFolder{ID: "folder-1", Name: "Test Folder"}
	require.NoError(t, store.CreateFolder(ctx, folder))
	tree.FolderID = folder.ID

	for i, promptID := range []string{"prompt-1", "prompt-2"} {
		_ = i
		prompt := &tables.TablePrompt{ID: promptID, Name: "Prompt " + promptID, FolderID: &tree.FolderID}
		require.NoError(t, store.CreatePrompt(ctx, prompt))
		tree.PromptIDs = append(tree.PromptIDs, promptID)

		// Create 2 versions with messages
		for v := 0; v < 2; v++ {
			version := &tables.TablePromptVersion{
				PromptID:      promptID,
				CommitMessage: "version commit",
				Messages: []tables.TablePromptVersionMessage{
					{PromptID: promptID, Message: json.RawMessage(`{"role":"user","content":"hello"}`)},
				},
			}
			require.NoError(t, store.CreatePromptVersion(ctx, version))
			tree.VersionIDs = append(tree.VersionIDs, version.ID)
		}

		// Create 1 session with messages
		session := &tables.TablePromptSession{
			PromptID: promptID,
			Name:     "Session " + promptID,
			Messages: []tables.TablePromptSessionMessage{
				{PromptID: promptID, Message: json.RawMessage(`{"role":"user","content":"hi"}`)},
			},
		}
		require.NoError(t, store.CreatePromptSession(ctx, session))
		tree.SessionIDs = append(tree.SessionIDs, session.ID)
	}

	return tree
}

// countRows returns the number of rows in a table
func countRows(t *testing.T, store *RDBConfigStore, model interface{}) int64 {
	t.Helper()
	var count int64
	require.NoError(t, store.DB().Model(model).Count(&count).Error)
	return count
}

func TestDeleteFolder(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeleteFolder(ctx, "nonexistent")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("Empty", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		folder := &tables.TableFolder{ID: "folder-empty", Name: "Empty"}
		require.NoError(t, store.CreateFolder(ctx, folder))

		require.NoError(t, store.DeleteFolder(ctx, "folder-empty"))

		_, err := store.GetFolderByID(ctx, "folder-empty")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("CascadesAll", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		// Verify entities exist before deletion
		assert.Greater(t, countRows(t, store, &tables.TablePrompt{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptVersion{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptVersionMessage{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptSession{}), int64(0))
		assert.Greater(t, countRows(t, store, &tables.TablePromptSessionMessage{}), int64(0))

		require.NoError(t, store.DeleteFolder(ctx, tree.FolderID))

		// All child entities should be deleted
		assert.Equal(t, int64(0), countRows(t, store, &tables.TableFolder{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePrompt{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptVersion{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptVersionMessage{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptSession{}))
		assert.Equal(t, int64(0), countRows(t, store, &tables.TablePromptSessionMessage{}))
	})
}

func TestDeletePrompt(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeletePrompt(ctx, "nonexistent")
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("CascadesAll", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePrompt(ctx, tree.PromptIDs[0]))

		// First prompt and its children should be gone
		_, err := store.GetPromptByID(ctx, tree.PromptIDs[0])
		assert.ErrorIs(t, err, ErrNotFound)

		// Second prompt should still exist
		p2, err := store.GetPromptByID(ctx, tree.PromptIDs[1])
		require.NoError(t, err)
		assert.Equal(t, tree.PromptIDs[1], p2.ID)
	})

	t.Run("LeavesFolder", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePrompt(ctx, tree.PromptIDs[0]))

		// Folder should still exist
		folder, err := store.GetFolderByID(ctx, tree.FolderID)
		require.NoError(t, err)
		assert.Equal(t, tree.FolderID, folder.ID)
	})

	t.Run("LeavesSiblings", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePrompt(ctx, tree.PromptIDs[0]))

		// Sibling prompt's versions and sessions should be unaffected
		versions, err := store.GetPromptVersions(ctx, tree.PromptIDs[1])
		require.NoError(t, err)
		assert.Len(t, versions, 2)

		sessions, err := store.GetPromptSessions(ctx, tree.PromptIDs[1])
		require.NoError(t, err)
		assert.Len(t, sessions, 1)
	})
}

func TestDeletePromptVersion(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeletePromptVersion(ctx, 99999)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("NonLatest", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		// Version at index 0 is v1 (non-latest), index 1 is v2 (latest) for prompt-1
		nonLatestID := tree.VersionIDs[0]
		latestID := tree.VersionIDs[1]

		require.NoError(t, store.DeletePromptVersion(ctx, nonLatestID))

		// Non-latest version should be gone
		_, err := store.GetPromptVersionByID(ctx, nonLatestID)
		assert.ErrorIs(t, err, ErrNotFound)

		// Latest version should still be latest
		latest, err := store.GetPromptVersionByID(ctx, latestID)
		require.NoError(t, err)
		assert.True(t, latest.IsLatest)
	})

	t.Run("LatestPromotesPrevious", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		// Delete the latest version (index 1 = v2 for prompt-1)
		latestID := tree.VersionIDs[1]
		prevID := tree.VersionIDs[0]

		require.NoError(t, store.DeletePromptVersion(ctx, latestID))

		// Previous version should now be latest
		prev, err := store.GetPromptVersionByID(ctx, prevID)
		require.NoError(t, err)
		assert.True(t, prev.IsLatest)
	})

	t.Run("LeavesPrompt", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePromptVersion(ctx, tree.VersionIDs[0]))

		// Prompt should still exist
		prompt, err := store.GetPromptByID(ctx, tree.PromptIDs[0])
		require.NoError(t, err)
		assert.Equal(t, tree.PromptIDs[0], prompt.ID)
	})
}

func TestDeletePromptSession(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		err := store.DeletePromptSession(ctx, 99999)
		assert.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("CascadesMessages", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		sessionID := tree.SessionIDs[0]
		require.NoError(t, store.DeletePromptSession(ctx, sessionID))

		// Session should be gone
		_, err := store.GetPromptSessionByID(ctx, sessionID)
		assert.ErrorIs(t, err, ErrNotFound)

		// Session messages for that session should be gone
		var msgCount int64
		require.NoError(t, store.DB().Model(&tables.TablePromptSessionMessage{}).Where("session_id = ?", sessionID).Count(&msgCount).Error)
		assert.Equal(t, int64(0), msgCount)
	})

	t.Run("LeavesPrompt", func(t *testing.T) {
		store := setupRDBTestStore(t)
		ctx := context.Background()
		tree := createTestPromptTree(t, store, ctx)

		require.NoError(t, store.DeletePromptSession(ctx, tree.SessionIDs[0]))

		// Prompt and versions should still exist
		prompt, err := store.GetPromptByID(ctx, tree.PromptIDs[0])
		require.NoError(t, err)
		assert.Equal(t, tree.PromptIDs[0], prompt.ID)

		versions, err := store.GetPromptVersions(ctx, tree.PromptIDs[0])
		require.NoError(t, err)
		assert.Len(t, versions, 2)
	})
}

// TestUpsertModelPricesBatch_SQLite guards the pricing-sync write path on
// SQLite. Batching these rows into a multi-row INSERT makes GORM emit the
// DEFAULT keyword for the table's many default:null columns, which SQLite
// rejects ("near \"DEFAULT\": syntax error"), so UpsertModelPricesBatch must
// fall back to per-row writes on SQLite. The rows below intentionally leave
// cost columns nil to exercise exactly that case.
func TestUpsertModelPricesBatch_SQLite(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	pricing := []tables.TableModelPricing{
		{Model: "google/gemini-2.5-flash", Provider: "vertex", Mode: "chat", InputCostPerToken: cost(0.000001)},
		{Model: "openai/gpt-4o", Provider: "openai", Mode: "chat"}, // all costs nil
		{Model: "anthropic/claude-3", Provider: "anthropic", Mode: "chat", OutputCostPerToken: cost(0.000015)},
	}

	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 3)

	// Re-upsert with a changed cost to exercise the ON CONFLICT update path.
	pricing[1].InputCostPerToken = cost(0.000005)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err = s.GetModelPrices(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 3) // upsert, not duplicate insert

	var updated *tables.TableModelPricing
	for i := range got {
		if got[i].Model == "openai/gpt-4o" {
			updated = &got[i]
		}
	}
	require.NotNil(t, updated)
	require.NotNil(t, updated.InputCostPerToken)
	assert.InDelta(t, 0.000005, *updated.InputCostPerToken, 1e-9)
}

// TestUpsertModelPricesBatch_PriorityAbove272kCacheCreation_SurvivesResync guards
// the pricingSyncUpdateColumns entry for the priority >272k cache-write column:
// the first sync writes every column, only the ON CONFLICT DO UPDATE of the
// second sync reveals a column missing from the explicit update list.
func TestUpsertModelPricesBatch_PriorityAbove272kCacheCreation_SurvivesResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	pricing := []tables.TableModelPricing{{
		Model: "gpt-6-astra", Provider: "openai", Mode: "responses",
		CacheCreationInputTokenCostPriority:                cost(0.000025),
		CacheCreationInputTokenCostAbove272kTokensPriority: cost(0.00005),
	}}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	pricing[0].CacheCreationInputTokenCostAbove272kTokensPriority = cost(0.00006)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].CacheCreationInputTokenCostAbove272kTokensPriority)
	assert.InDelta(t, 0.00006, *got[0].CacheCreationInputTokenCostAbove272kTokensPriority, 1e-12)
}

func TestUpsertModelPricesBatch_WebSearchCostPerRequest_SurvivesResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))
	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	pricing := []tables.TableModelPricing{{Model: "claude-haiku-4-5", Provider: "anthropic", Mode: "chat", WebSearchCostPerRequest: cost(0.01)}}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))
	pricing[0].WebSearchCostPerRequest = cost(0.02)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].WebSearchCostPerRequest)
	assert.InDelta(t, 0.02, *got[0].WebSearchCostPerRequest, 1e-9)
}

func TestUpsertModelPricesBatch_MegapixelImageTierColumns_SurviveResync(t *testing.T) {
	// Regression test for pricingSyncUpdateColumns: a column present on
	// TableModelPricing but missing from that explicit update-column list
	// would insert fine on the first sync (Create writes every column) but
	// silently revert to null on the second sync (ON CONFLICT DO UPDATE only
	// touches listed columns).
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	pricing := []tables.TableModelPricing{
		{
			Model:                               "prunaai/p-image-upscale",
			Provider:                            "replicate",
			Mode:                                "image_generation",
			OutputCostPerImage:                  cost(0.005),
			OutputCostPerImageAbove4Megapixels:  cost(0.01),
			OutputCostPerImageAbove8Megapixels:  cost(0.02),
			OutputCostPerImageAbove16Megapixels: cost(0.04),
			OutputCostPerImageAbove32Megapixels: cost(0.06),
			OutputCostPerImageAbove64Megapixels: cost(0.12),
		},
	}

	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	// Re-upsert the same row (simulating the next scheduled datasheet sync)
	// with a changed tier value to exercise the ON CONFLICT update path.
	pricing[0].OutputCostPerImageAbove16Megapixels = cost(0.05)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)

	row := got[0]
	require.NotNil(t, row.OutputCostPerImageAbove4Megapixels)
	require.NotNil(t, row.OutputCostPerImageAbove8Megapixels)
	require.NotNil(t, row.OutputCostPerImageAbove16Megapixels)
	require.NotNil(t, row.OutputCostPerImageAbove32Megapixels)
	require.NotNil(t, row.OutputCostPerImageAbove64Megapixels)
	assert.InDelta(t, 0.01, *row.OutputCostPerImageAbove4Megapixels, 1e-9)
	assert.InDelta(t, 0.02, *row.OutputCostPerImageAbove8Megapixels, 1e-9)
	assert.InDelta(t, 0.05, *row.OutputCostPerImageAbove16Megapixels, 1e-9) // survived resync with the updated value
	assert.InDelta(t, 0.06, *row.OutputCostPerImageAbove32Megapixels, 1e-9)
	assert.InDelta(t, 0.12, *row.OutputCostPerImageAbove64Megapixels, 1e-9)
}

func TestUpsertModelPricesBatch_TimeOfDayColumns_SurviveResync(t *testing.T) {
	// Same pricingSyncUpdateColumns regression as the megapixel test above, for
	// the peak/off-peak columns. peak_hours additionally exercises the
	// serializer:json round-trip, which the plain *float64 columns do not.
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	pricing := []tables.TableModelPricing{
		{
			Model:                 "deepseek-v4-flash",
			Provider:              "deepseek",
			Mode:                  "chat",
			InputCostPerToken:     cost(0.00000044),
			OutputCostPerToken:    cost(0.00000132),
			OffPeakCostMultiplier: cost(0.5),
			PeakHours: &tables.PeakHoursSchedule{
				Timezone: "UTC",
				Windows: []tables.PeakHoursWindow{
					{Days: []int{1, 2, 3, 4, 5}, Start: "01:00", End: "04:00"},
					{Days: []int{1, 2, 3, 4, 5}, Start: "06:00", End: "10:00"},
				},
			},
		},
	}

	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	// Re-upsert the same row (the next scheduled datasheet sync) with BOTH
	// columns changed, to exercise the ON CONFLICT update path. peak_hours has
	// to change too: leaving it identical would let the row keep the value the
	// first insert wrote, so the assertions below would still pass even if
	// peak_hours were missing from pricingSyncUpdateColumns, which is the exact
	// regression this test exists to catch.
	pricing[0].OffPeakCostMultiplier = cost(0.6)
	pricing[0].PeakHours = &tables.PeakHoursSchedule{
		Timezone: "Asia/Shanghai",
		Windows: []tables.PeakHoursWindow{
			{Days: []int{1, 2, 3, 4, 5}, Start: "02:00", End: "05:00"},
		},
	}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)

	row := got[0]
	require.NotNil(t, row.OffPeakCostMultiplier)
	assert.InDelta(t, 0.6, *row.OffPeakCostMultiplier, 1e-9) // survived resync with the updated value

	// Every field below differs from the first insert, so a peak_hours column
	// that never got updated fails here rather than passing by coincidence.
	require.NotNil(t, row.PeakHours)
	assert.Equal(t, "Asia/Shanghai", row.PeakHours.Timezone)
	require.Len(t, row.PeakHours.Windows, 1)
	assert.Equal(t, []int{1, 2, 3, 4, 5}, row.PeakHours.Windows[0].Days)
	assert.Equal(t, "02:00", row.PeakHours.Windows[0].Start)
	assert.Equal(t, "05:00", row.PeakHours.Windows[0].End)
}

// TestUpsertModelPricesBatch_DecisionRateColumns_SurviveResync is the same
// pricingSyncUpdateColumns regression for the decision rate columns: both
// values change on the second upsert so a column missing from the list fails
// here instead of keeping its first value.
func TestUpsertModelPricesBatch_DecisionRateColumns_SurviveResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	pricing := []tables.TableModelPricing{{
		Model:                       "gpt-6-luna",
		Provider:                    "openai",
		Mode:                        "chat",
		InputCostPerToken:           cost(0.000002),
		OutputCostPerToken:          cost(0.000008),
		InputCostPerTokenDecisions:  cost(0.0000001),
		OutputCostPerTokenDecisions: cost(0.0000003),
	}}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	pricing[0].InputCostPerTokenDecisions = cost(0.0000002)
	pricing[0].OutputCostPerTokenDecisions = cost(0.0000004)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)

	row := got[0]
	require.NotNil(t, row.InputCostPerTokenDecisions, "input_cost_per_token_decisions missing from pricingSyncUpdateColumns")
	require.NotNil(t, row.OutputCostPerTokenDecisions, "output_cost_per_token_decisions missing from pricingSyncUpdateColumns")
	assert.InDelta(t, 0.0000002, *row.InputCostPerTokenDecisions, 1e-12)
	assert.InDelta(t, 0.0000004, *row.OutputCostPerTokenDecisions, 1e-12)
}

func TestUpsertModelParametersBatch_SQLite(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelParameters{}))

	ctx := context.Background()
	params := []tables.TableModelParameters{
		{Model: "model-a", Data: `{"max_output_tokens":100}`},
		{Model: "model-b", Data: `{"max_output_tokens":200}`},
		{Model: "model-c", Data: `{"max_output_tokens":300}`},
	}

	require.NoError(t, s.UpsertModelParametersBatch(ctx, params))

	got, err := s.GetModelParameters(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 3)

	params[1].Data = `{"max_output_tokens":250}`
	require.NoError(t, s.UpsertModelParametersBatch(ctx, params))

	updated, err := s.GetModelParametersByModel(ctx, "model-b")
	require.NoError(t, err)
	assert.Equal(t, `{"max_output_tokens":250}`, updated.Data)

	require.NoError(t, s.UpsertModelParametersBatch(ctx, []tables.TableModelParameters{
		{Model: "model-b", Data: `{"max_output_tokens":260}`},
		{Model: "model-b", Data: `{"max_output_tokens":270}`},
	}))
	updated, err = s.GetModelParametersByModel(ctx, "model-b")
	require.NoError(t, err)
	assert.Equal(t, `{"max_output_tokens":270}`, updated.Data)

	got, err = s.GetModelParameters(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func testWebhookEndpoint(name string) *tables.TableWebhookEndpoint {
	return &tables.TableWebhookEndpoint{
		Name:   name,
		URL:    "https://93.184.216.34/hook",
		Events: []tables.WebhookEvent{tables.WebhookEventAsyncJobCompleted},
	}
}

func TestWebhookEndpointsPaginated(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	billing := testWebhookEndpoint("billing-hook")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, billing))

	alerts := testWebhookEndpoint("alerts-hook")
	alerts.URL = "https://93.184.216.34/alerts"
	alerts.Events = []tables.WebhookEvent{tables.WebhookEventAsyncJobFailed}
	require.NoError(t, store.CreateWebhookEndpoint(ctx, alerts))

	paused := testWebhookEndpoint("paused-hook")
	paused.Disabled = true
	require.NoError(t, store.CreateWebhookEndpoint(ctx, paused))

	names := func(endpoints []tables.TableWebhookEndpoint) []string {
		out := make([]string, 0, len(endpoints))
		for _, e := range endpoints {
			out = append(out, e.Name)
		}
		return out
	}

	// No filters: everything, creation order, full count.
	all, total, err := store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, []string{"billing-hook", "alerts-hook", "paused-hook"}, names(all))

	// Paging: total stays the full match count.
	page, total, err := store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{Limit: 1, Offset: 1})
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	assert.Equal(t, []string{"alerts-hook"}, names(page))

	// Search matches name or URL, case-insensitively.
	found, total, err := store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{Search: "ALERTS", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, []string{"alerts-hook"}, names(found))

	// Disabled filter is tri-state.
	enabledOnly := false
	found, total, err = store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{Disabled: &enabledOnly, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	assert.Equal(t, []string{"billing-hook", "alerts-hook"}, names(found))

	// Event filter selects subscribers of any requested event.
	found, total, err = store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{
		Events: []string{string(tables.WebhookEventAsyncJobFailed)},
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, []string{"alerts-hook"}, names(found))

	// Filters compose with AND semantics.
	found, total, err = store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{
		Search:   "hook",
		Disabled: &enabledOnly,
		Events:   []string{string(tables.WebhookEventAsyncJobCompleted)},
		Limit:    10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, []string{"billing-hook"}, names(found))
}

func TestWebhookEndpointsSearchEscapesLikeWildcards(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("prod-hook")))
	require.NoError(t, store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("team_hook")))

	names := func(endpoints []tables.TableWebhookEndpoint) []string {
		out := make([]string, 0, len(endpoints))
		for _, e := range endpoints {
			out = append(out, e.Name)
		}
		return out
	}

	// A literal "_" must match only the name that contains one — not act as a
	// single-character LIKE wildcard, which would also match "prod-hook".
	found, total, err := store.GetWebhookEndpointsPaginated(ctx, WebhookEndpointsQueryParams{Search: "_", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Equal(t, []string{"team_hook"}, names(found))
}

func TestWebhookEndpointCreate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("create-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))

	// Server generates the ID and a Standard Webhooks style secret, and leaves
	// the plaintext on the struct for one-time display.
	assert.NotEmpty(t, endpoint.ID)
	require.NotNil(t, endpoint.Secret)
	secret := endpoint.Secret.GetValue()
	assert.True(t, strings.HasPrefix(secret, "whsec_"), "secret %q missing whsec_ prefix", secret)
	assert.Len(t, secret, len("whsec_")+44) // base64 of 32 bytes

	fetched, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, "create-test", fetched.Name)
	assert.Equal(t, "https://93.184.216.34/hook", fetched.URL)
	assert.Equal(t, []tables.WebhookEvent{tables.WebhookEventAsyncJobCompleted}, fetched.Events)
	assert.Equal(t, secret, fetched.Secret.GetValue())

	byName, err := store.GetWebhookEndpointByName(ctx, "create-test")
	require.NoError(t, err)
	assert.Equal(t, endpoint.ID, byName.ID)

	// Distinct endpoints get distinct generated secrets.
	second := testWebhookEndpoint("create-test-2")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, second))
	assert.NotEqual(t, secret, second.Secret.GetValue())
}

func TestWebhookEndpointCreateDuplicateName(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("dup-name")))
	err := store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("dup-name"))
	assert.ErrorIs(t, err, ErrAlreadyExists)
}

func TestWebhookEndpointCreateRejectsUnresolvedSecretRef(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// An env reference that resolves to nothing must never be persisted —
	// deliveries would sign with an empty key. The API never accepts a
	// secret, so a reference can only arrive from config.json.
	endpoint := testWebhookEndpoint("unresolved-ref")
	endpoint.Secret = schemas.NewSecretVar("env.E2E_WEBHOOK_SECRET_THAT_DOES_NOT_EXIST")
	require.True(t, endpoint.Secret.IsFromSecret())

	err := store.CreateWebhookEndpoint(ctx, endpoint)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not resolve")

	// A reference that resolves is kept as the signing key.
	t.Setenv("E2E_WEBHOOK_SECRET_SET", "whsec_from_env")
	resolved := testWebhookEndpoint("resolved-ref")
	resolved.Secret = schemas.NewSecretVar("env.E2E_WEBHOOK_SECRET_SET")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, resolved))
	assert.Equal(t, "whsec_from_env", resolved.Secret.GetValue())
}

func TestWebhookEndpointCreateFailureKeepsCallerPlaintext(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("plaintext-kept")))

	// A failed create (duplicate name here) must leave the caller's secret
	// as the plaintext it supplied — not the BeforeSave ciphertext — so a
	// retry cannot double-encrypt and persist unusable key material.
	retry := testWebhookEndpoint("plaintext-kept")
	retry.Secret = schemas.NewSecretVar("whsec_caller_supplied")
	err := store.CreateWebhookEndpoint(ctx, retry)
	require.ErrorIs(t, err, ErrAlreadyExists)
	assert.Equal(t, "whsec_caller_supplied", retry.Secret.GetValue())

	// And the retry (under a fresh name) persists a working secret.
	retry.Name = "plaintext-kept-2"
	retry.ID = ""
	require.NoError(t, store.CreateWebhookEndpoint(ctx, retry))
	fetched, err := store.GetWebhookEndpointByID(ctx, retry.ID)
	require.NoError(t, err)
	assert.Equal(t, "whsec_caller_supplied", fetched.Secret.GetValue())
}

func TestWebhookEndpointUpdate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("update-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))
	originalSecret := endpoint.Secret.GetValue()

	// Seed a failure streak without going through hooks.
	require.NoError(t, store.DB().Model(&tables.TableWebhookEndpoint{}).
		Where("id = ?", endpoint.ID).UpdateColumn("consecutive_failures", 7).Error)

	// A non-URL change keeps the failure counter and the stored secret.
	loaded, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	loaded.IncludeResponse = true
	loaded.Events = []tables.WebhookEvent{tables.WebhookEventAsyncJobCompleted, tables.WebhookEventAsyncJobFailed}
	require.NoError(t, store.UpdateWebhookEndpoint(ctx, loaded))

	fetched, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.True(t, fetched.IncludeResponse)
	assert.Len(t, fetched.Events, 2)
	assert.Equal(t, 7, fetched.ConsecutiveFailures)
	assert.Equal(t, originalSecret, fetched.Secret.GetValue(), "update must not touch the signing secret")

	// Changing the URL resets the failure counter.
	fetched.URL = "https://93.184.216.35/hook"
	require.NoError(t, store.UpdateWebhookEndpoint(ctx, fetched))
	fetched, err = store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, "https://93.184.216.35/hook", fetched.URL)
	assert.Equal(t, 0, fetched.ConsecutiveFailures)
}

func TestWebhookEndpointUpdateReenablePreservesFailureCounter(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("reenable-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))

	loaded, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	loaded.Disabled = true
	require.NoError(t, store.UpdateWebhookEndpoint(ctx, loaded))
	require.NoError(t, store.DB().Model(&tables.TableWebhookEndpoint{}).
		Where("id = ?", endpoint.ID).UpdateColumn("consecutive_failures", 25).Error)

	disabled, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.True(t, disabled.Disabled)

	disabled.Disabled = false
	require.NoError(t, store.UpdateWebhookEndpoint(ctx, disabled))

	// Re-enabling does not touch the failure counter — only a successful
	// delivery (or a URL change) resets it.
	reenabled, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.False(t, reenabled.Disabled)
	assert.Equal(t, 25, reenabled.ConsecutiveFailures)
}

func TestWebhookEndpointUpdateNotFound(t *testing.T) {
	store := setupRDBTestStore(t)

	endpoint := testWebhookEndpoint("ghost")
	endpoint.ID = "does-not-exist"
	assert.ErrorIs(t, store.UpdateWebhookEndpoint(context.Background(), endpoint), ErrNotFound)
}

func TestWebhookEndpointDelete(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("delete-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))

	require.NoError(t, store.DeleteWebhookEndpoint(ctx, endpoint.ID))
	_, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.DeleteWebhookEndpoint(ctx, endpoint.ID), ErrNotFound)
}

func TestWebhookEndpointRotateSecret(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("rotate-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))
	originalSecret := endpoint.Secret.GetValue()

	rotated, err := store.RotateWebhookEndpointSecret(ctx, endpoint.ID)
	require.NoError(t, err)

	newSecret := rotated.Secret.GetValue()
	assert.True(t, strings.HasPrefix(newSecret, "whsec_"))
	assert.NotEqual(t, originalSecret, newSecret)

	// Rotation is immediate: only the new secret is stored.
	fetched, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, newSecret, fetched.Secret.GetValue())

	_, err = store.RotateWebhookEndpointSecret(ctx, "does-not-exist")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestWebhookEndpointList(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoints, err := store.GetWebhookEndpoints(ctx)
	require.NoError(t, err)
	assert.Empty(t, endpoints)

	require.NoError(t, store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("list-a")))
	require.NoError(t, store.CreateWebhookEndpoint(ctx, testWebhookEndpoint("list-b")))

	endpoints, err = store.GetWebhookEndpoints(ctx)
	require.NoError(t, err)
	assert.Len(t, endpoints, 2)
}

func TestWebhookEndpointUpdatePersistsTuningAndHeaders(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("update-persist-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))

	loaded, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	loaded.MaxRetries = 2
	loaded.AttemptTimeoutSeconds = 7
	loaded.MaxConcurrentDeliveries = 3
	loaded.Headers = map[string]schemas.SecretVar{"Authorization": {Val: "Bearer tok"}}
	require.NoError(t, store.UpdateWebhookEndpoint(ctx, loaded))

	fetched, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, fetched.MaxRetries, "updates must persist tuning knobs")
	assert.Equal(t, 7, fetched.AttemptTimeoutSeconds)
	assert.Equal(t, 3, fetched.MaxConcurrentDeliveries)
	require.Len(t, fetched.Headers, 1)
	auth := fetched.Headers["Authorization"]
	assert.Equal(t, "Bearer tok", auth.GetValue())
}

func TestWebhookEndpointRecordFailureAndSuccess(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("counter-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))

	failures, err := store.RecordWebhookEndpointFailure(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, failures)
	failures, err = store.RecordWebhookEndpointFailure(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, failures)

	fetched, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, fetched.ConsecutiveFailures)
	assert.NotNil(t, fetched.LastFailureAt)
	assert.Nil(t, fetched.LastSuccessAt)

	require.NoError(t, store.RecordWebhookEndpointSuccess(ctx, endpoint.ID))
	fetched, err = store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, fetched.ConsecutiveFailures)
	assert.NotNil(t, fetched.LastSuccessAt)

	_, err = store.RecordWebhookEndpointFailure(ctx, "does-not-exist")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.RecordWebhookEndpointSuccess(ctx, "does-not-exist"), ErrNotFound)
}

func TestWebhookEndpointCounterUpdatesLeaveConfigUntouched(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("counter-config-test")
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))
	secret := endpoint.Secret.GetValue()

	before, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)

	_, err = store.RecordWebhookEndpointFailure(ctx, endpoint.ID)
	require.NoError(t, err)
	require.NoError(t, store.RecordWebhookEndpointSuccess(ctx, endpoint.ID))

	after, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, before.UpdatedAt.UnixNano(), after.UpdatedAt.UnixNano(),
		"operational counters must not touch updated_at")
	assert.Equal(t, before.URL, after.URL)
	assert.Equal(t, before.Events, after.Events)
	assert.Equal(t, secret, after.Secret.GetValue(),
		"operational counters must not touch the stored secret")
}

func testWebhookJob(id, endpointID string) *tables.TableWebhookJob {
	return &tables.TableWebhookJob{
		ID:         id,
		EndpointID: endpointID,
		AsyncJobID: "async-job-1",
		Event:      tables.WebhookEventAsyncJobCompleted,
	}
}

// getWebhookJob reads a queue row directly; the store deliberately exposes no
// single-row getter, since workers only ever list-and-claim.
func getWebhookJob(t *testing.T, store *RDBConfigStore, id string) *tables.TableWebhookJob {
	t.Helper()
	var job tables.TableWebhookJob
	require.NoError(t, store.DB().Where("id = ?", id).First(&job).Error)
	return &job
}

// setWebhookJobClaimedUntil forces a job's lease expiry to a fixed time so
// lease expiry can be exercised deterministically without sleeping.
func setWebhookJobClaimedUntil(t *testing.T, store *RDBConfigStore, id string, ts time.Time) {
	t.Helper()
	require.NoError(t, store.DB().Model(&tables.TableWebhookJob{}).
		Where("id = ?", id).UpdateColumn("claimed_until", ts.UTC()).Error)
}

func TestWebhookJobCreateValidation(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	assert.Error(t, store.CreateWebhookJob(ctx, nil))
	assert.Error(t, store.CreateWebhookJob(ctx, &tables.TableWebhookJob{EndpointID: "ep", AsyncJobID: "job", Event: tables.WebhookEventAsyncJobCompleted}))
	assert.Error(t, store.CreateWebhookJob(ctx, &tables.TableWebhookJob{ID: "id", AsyncJobID: "job", Event: tables.WebhookEventAsyncJobCompleted}))
	assert.Error(t, store.CreateWebhookJob(ctx, &tables.TableWebhookJob{ID: "id", EndpointID: "ep", Event: tables.WebhookEventAsyncJobCompleted}))
	assert.Error(t, store.CreateWebhookJob(ctx, &tables.TableWebhookJob{ID: "id", EndpointID: "ep", AsyncJobID: "job", Event: "bogus.event"}))
	// A new job must enter unattempted and unclaimed.
	assert.Error(t, store.CreateWebhookJob(ctx, &tables.TableWebhookJob{ID: "id", EndpointID: "ep", AsyncJobID: "job", Event: tables.WebhookEventAsyncJobCompleted, AttemptCount: 1}))
	assert.Error(t, store.CreateWebhookJob(ctx, &tables.TableWebhookJob{ID: "id", EndpointID: "ep", AsyncJobID: "job", Event: tables.WebhookEventAsyncJobCompleted, ClaimedBy: "node-a"}))
}

func TestWebhookJobCreateDefaultsAndDuplicate(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	job := testWebhookJob("job-1", "ep-1")
	require.NoError(t, store.CreateWebhookJob(ctx, job))
	assert.False(t, job.NextAttemptAt.IsZero(), "zero NextAttemptAt defaults to now (due immediately)")
	assert.False(t, job.CreatedAt.IsZero())

	stored := getWebhookJob(t, store, "job-1")
	assert.Equal(t, 0, stored.AttemptCount)
	assert.Empty(t, stored.ClaimedBy)
	assert.Nil(t, stored.ClaimedUntil)

	// The id doubles as the delivery's wire identifier and must stay unique
	// while a delivery is in flight.
	assert.ErrorIs(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")), ErrAlreadyExists)
}

func TestWebhookJobClaimRace(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Pin the pool to a single connection so both racers hit the same
	// in-memory database; SQLite serializes writes on that connection, which
	// is precisely the atomicity the conditional UPDATE relies on.
	sqlDB, err := store.DB().DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")))

	// Two nodes claim the same job simultaneously: both block on the barrier,
	// then fire together. Exactly one conditional UPDATE may match the still-due
	// row, so exactly one claimer must win.
	nodes := []string{"node-a", "node-b"}
	results := make([]bool, len(nodes))
	errs := make([]error, len(nodes))
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i, node := range nodes {
		done.Add(1)
		go func(i int, node string) {
			defer done.Done()
			start.Wait()
			results[i], errs[i] = store.ClaimWebhookJob(ctx, "job-1", node, time.Now().Add(time.Minute))
		}(i, node)
	}
	start.Done()
	done.Wait()

	for i, node := range nodes {
		require.NoErrorf(t, errs[i], "claim by %s errored", node)
	}
	wins := 0
	for _, won := range results {
		if won {
			wins++
		}
	}
	assert.Equal(t, 1, wins, "exactly one concurrent claimer must win")

	stored := getWebhookJob(t, store, "job-1")
	assert.Contains(t, nodes, stored.ClaimedBy, "the stored claim must belong to a racer")
	require.NotNil(t, stored.ClaimedUntil)
}

func TestWebhookJobClaimLeaseExpiryReclaim(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")))
	won, err := store.ClaimWebhookJob(ctx, "job-1", "node-a", time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, won)

	// The owner dies mid-attempt: its lease lapses and another node reclaims.
	setWebhookJobClaimedUntil(t, store, "job-1", time.Now().Add(-time.Second))
	won, err = store.ClaimWebhookJob(ctx, "job-1", "node-b", time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, won)
	assert.Equal(t, "node-b", getWebhookJob(t, store, "job-1").ClaimedBy)
}

func TestWebhookJobClaimNotDueRejected(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	job := testWebhookJob("job-1", "ep-1")
	job.NextAttemptAt = time.Now().Add(time.Hour)
	require.NoError(t, store.CreateWebhookJob(ctx, job))

	won, err := store.ClaimWebhookJob(ctx, "job-1", "node-a", time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, won)

	won, err = store.ClaimWebhookJob(ctx, "missing-job", "node-a", time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, won)

	// A non-future lease is rejected outright: winning with an already-expired
	// lease would leave the job instantly reclaimable by another worker.
	won, err = store.ClaimWebhookJob(ctx, "job-1", "node-a", time.Now().Add(-time.Second))
	assert.Error(t, err)
	assert.False(t, won)
}

func TestWebhookJobListDue(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Oldest due job, listed first.
	oldest := testWebhookJob("due-oldest", "ep-1")
	oldest.NextAttemptAt = time.Now().Add(-2 * time.Minute)
	require.NoError(t, store.CreateWebhookJob(ctx, oldest))

	newer := testWebhookJob("due-newer", "ep-1")
	newer.NextAttemptAt = time.Now().Add(-time.Minute)
	require.NoError(t, store.CreateWebhookJob(ctx, newer))

	future := testWebhookJob("future", "ep-1")
	future.NextAttemptAt = time.Now().Add(time.Hour)
	require.NoError(t, store.CreateWebhookJob(ctx, future))

	claimed := testWebhookJob("claimed-live", "ep-1")
	claimed.NextAttemptAt = time.Now().Add(-time.Minute)
	require.NoError(t, store.CreateWebhookJob(ctx, claimed))
	won, err := store.ClaimWebhookJob(ctx, "claimed-live", "node-a", time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, won)

	expired := testWebhookJob("claimed-expired", "ep-1")
	expired.NextAttemptAt = time.Now().Add(-3 * time.Minute)
	require.NoError(t, store.CreateWebhookJob(ctx, expired))
	won, err = store.ClaimWebhookJob(ctx, "claimed-expired", "node-a", time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, won)
	setWebhookJobClaimedUntil(t, store, "claimed-expired", time.Now().Add(-time.Second))

	due, err := store.ListDueWebhookJobs(ctx, 0)
	require.NoError(t, err)
	ids := make([]string, 0, len(due))
	for _, j := range due {
		ids = append(ids, j.ID)
	}
	assert.Equal(t, []string{"claimed-expired", "due-oldest", "due-newer"}, ids)

	due, err = store.ListDueWebhookJobs(ctx, 1)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, "claimed-expired", due[0].ID)
}

// TestWebhookJobUTCDueComparison guards the queue's time handling with the
// UTC timestamps the dispatcher actually writes: SQLite compares datetimes
// as strings, so a due check made with a local-zone now sees a future UTC
// next_attempt_at as already due on any machine east of UTC — which made
// retries fire on the next poll tick instead of after their backoff.
func TestWebhookJobUTCDueComparison(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	future := testWebhookJob("utc-future", "ep-1")
	future.NextAttemptAt = time.Now().UTC().Add(time.Hour)
	require.NoError(t, store.CreateWebhookJob(ctx, future))

	due, err := store.ListDueWebhookJobs(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, due, "a job due an hour from now must not be listed")

	won, err := store.ClaimWebhookJob(ctx, "utc-future", "node-a", time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, won, "a job due an hour from now must not be claimable")
}

func TestWebhookJobReschedule(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")))
	lease := time.Now().Add(time.Minute)
	won, err := store.ClaimWebhookJob(ctx, "job-1", "node-a", lease)
	require.NoError(t, err)
	require.True(t, won)

	// A non-owner cannot reschedule someone else's claim.
	assert.Error(t, store.RescheduleWebhookJob(ctx, "job-1", "node-b", lease, time.Now().Add(5*time.Minute)))

	next := time.Now().Add(5 * time.Minute)
	require.NoError(t, store.RescheduleWebhookJob(ctx, "job-1", "node-a", lease, next))

	stored := getWebhookJob(t, store, "job-1")
	assert.Equal(t, 1, stored.AttemptCount)
	assert.Empty(t, stored.ClaimedBy)
	assert.Nil(t, stored.ClaimedUntil)
	assert.WithinDuration(t, next, stored.NextAttemptAt, time.Second)

	// The claim was released, so a second reschedule has nothing to fence on.
	assert.Error(t, store.RescheduleWebhookJob(ctx, "job-1", "node-a", lease, next))
}

func TestWebhookJobDelete(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")))

	// Only the current claim holder can retire a job.
	assert.Error(t, store.DeleteWebhookJob(ctx, "job-1", "node-a", time.Now().Add(time.Minute)))

	lease := time.Now().Add(time.Minute)
	won, err := store.ClaimWebhookJob(ctx, "job-1", "node-a", lease)
	require.NoError(t, err)
	require.True(t, won)
	assert.Error(t, store.DeleteWebhookJob(ctx, "job-1", "node-b", lease))
	require.NoError(t, store.DeleteWebhookJob(ctx, "job-1", "node-a", lease))

	var count int64
	require.NoError(t, store.DB().Model(&tables.TableWebhookJob{}).Where("id = ?", "job-1").Count(&count).Error)
	assert.Zero(t, count)
}

func TestWebhookJobClaimCycleWithEmptyRunnerID(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Single-node mode claims with an empty runner id; the lease alone
	// fences, and fenced writes still match the empty claimed_by.
	require.NoError(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")))
	lease := time.Now().Add(time.Minute)
	won, err := store.ClaimWebhookJob(ctx, "job-1", "", lease)
	require.NoError(t, err)
	require.True(t, won)

	won, err = store.ClaimWebhookJob(ctx, "job-1", "", time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, won, "live lease must fence even without runner ids")

	require.NoError(t, store.RescheduleWebhookJob(ctx, "job-1", "", lease, time.Now().Add(time.Minute)))
	assert.Equal(t, 1, getWebhookJob(t, store, "job-1").AttemptCount)
}

func TestWebhookJobStaleOwnerAfterReclaimIsFenced(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	// Same runner reclaims a job after its first lease expired — the stale
	// attempt still holds the OLD lease value, and runner identity alone
	// cannot tell the two claims apart (the single-node runner id is empty).
	require.NoError(t, store.CreateWebhookJob(ctx, testWebhookJob("job-1", "ep-1")))
	staleLease := time.Now().Add(time.Minute)
	won, err := store.ClaimWebhookJob(ctx, "job-1", "", staleLease)
	require.NoError(t, err)
	require.True(t, won)
	setWebhookJobClaimedUntil(t, store, "job-1", time.Now().Add(-time.Second))

	// The expired-lease value in the DB, exactly as the reclaim will see it.
	expiredLease := *getWebhookJob(t, store, "job-1").ClaimedUntil

	newLease := time.Now().Add(time.Minute)
	won, err = store.ClaimWebhookJob(ctx, "job-1", "", newLease)
	require.NoError(t, err)
	require.True(t, won, "expired lease must be reclaimable")

	// The stale owner's terminal mutations must match nothing.
	assert.Error(t, store.RescheduleWebhookJob(ctx, "job-1", "", staleLease, time.Now().Add(time.Minute)))
	assert.Error(t, store.RescheduleWebhookJob(ctx, "job-1", "", expiredLease, time.Now().Add(time.Minute)))
	assert.Error(t, store.DeleteWebhookJob(ctx, "job-1", "", staleLease))
	assert.Error(t, store.DeleteWebhookJob(ctx, "job-1", "", expiredLease))

	stored := getWebhookJob(t, store, "job-1")
	assert.Zero(t, stored.AttemptCount, "stale reschedule must not move the attempt counter")
	require.NotNil(t, stored.ClaimedUntil, "stale delete must not release the live claim")

	// The live claim holder still owns the job.
	require.NoError(t, store.DeleteWebhookJob(ctx, "job-1", "", newLease))
}

func TestClientConfigWebhookConfigRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpdateClientConfig(ctx, &ClientConfig{
		WebhookConfig: &tables.WebhookConfig{DeliveryHistoryRetentionDays: 90},
	}))
	loaded, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded.WebhookConfig, "webhook settings must survive the database round-trip")
	assert.Equal(t, 90, loaded.WebhookConfig.DeliveryHistoryRetentionDays)

	// Absent settings stay absent rather than becoming a zero-value struct.
	require.NoError(t, store.UpdateClientConfig(ctx, &ClientConfig{}))
	loaded, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.Nil(t, loaded.WebhookConfig)
}

func TestWebhookEndpointTuningRoundTripAndHash(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	endpoint := testWebhookEndpoint("tuning-test")
	endpoint.MaxRetries = 2
	endpoint.RetryBackoffInitialSeconds = 5
	endpoint.AttemptTimeoutSeconds = 3
	require.NoError(t, endpoint.Validate())
	require.NoError(t, store.CreateWebhookEndpoint(ctx, endpoint))

	fetched, err := store.GetWebhookEndpointByID(ctx, endpoint.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, fetched.MaxRetries)
	assert.Equal(t, 5, fetched.RetryBackoffInitialSeconds)
	assert.Equal(t, 3, fetched.AttemptTimeoutSeconds)
	assert.Zero(t, fetched.MaxConcurrentDeliveries, "unset knobs stay zero (worker default)")

	// Tuning participates in change detection for config.json sync.
	baseHash, err := GenerateWebhookEndpointHash(fetched)
	require.NoError(t, err)
	fetched.MaxRetries = 7
	changedHash, err := GenerateWebhookEndpointHash(fetched)
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, changedHash)

	// Negative knobs are rejected at validation.
	invalid := testWebhookEndpoint("tuning-invalid")
	invalid.MaxRetries = -1
	assert.Error(t, invalid.Validate())
}

// TestRDBConfigStore_SyncRoutingRules covers the batch path used by config-file reloads.
// It defers the unique-priority-per-scope check to the final state, so a valid permutation
// (e.g. swapping two priorities) succeeds despite a transient intermediate collision, while a
// genuine end-state duplicate still errors and a missing update target returns ErrNotFound.
func TestRDBConfigStore_SyncRoutingRules(t *testing.T) {
	ctx := context.Background()

	rule := func(id string, priority int) tables.TableRoutingRule {
		return *routingRuleFixture(id, priority, "openai")
	}

	tests := []struct {
		name       string
		toAdd      []tables.TableRoutingRule
		toUpdate   []tables.TableRoutingRule
		wantErrIs  error
		wantErrSub string
		wantFinal  map[string]int // expected priority per rule ID after the call
	}{
		{
			name:      "valid priority swap succeeds",
			toUpdate:  []tables.TableRoutingRule{rule("rule-a", 1), rule("rule-b", 0)},
			wantFinal: map[string]int{"rule-a": 1, "rule-b": 0},
		},
		{
			name:      "batch add plus swap succeeds",
			toAdd:     []tables.TableRoutingRule{rule("rule-c", 2)},
			toUpdate:  []tables.TableRoutingRule{rule("rule-a", 1), rule("rule-b", 0)},
			wantFinal: map[string]int{"rule-a": 1, "rule-b": 0, "rule-c": 2},
		},
		{
			name:       "final-state duplicate priority fails and rolls back",
			toUpdate:   []tables.TableRoutingRule{rule("rule-a", 1), rule("rule-b", 1)},
			wantErrSub: "already exists",
			wantFinal:  map[string]int{"rule-a": 0, "rule-b": 1}, // unchanged (rolled back)
		},
		{
			name:      "missing update ID returns ErrNotFound",
			toUpdate:  []tables.TableRoutingRule{rule("rule-missing", 5)},
			wantErrIs: ErrNotFound,
			wantFinal: map[string]int{"rule-a": 0, "rule-b": 1}, // unchanged
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := setupRDBTestStore(t)
			require.NoError(t, store.CreateRoutingRule(ctx, routingRuleFixture("rule-a", 0, "openai")))
			require.NoError(t, store.CreateRoutingRule(ctx, routingRuleFixture("rule-b", 1, "openai")))

			err := store.SyncRoutingRules(ctx, tc.toAdd, tc.toUpdate)
			switch {
			case tc.wantErrIs != nil:
				require.ErrorIs(t, err, tc.wantErrIs)
			case tc.wantErrSub != "":
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErrSub)
			default:
				require.NoError(t, err)
			}

			for id, want := range tc.wantFinal {
				got, getErr := store.GetRoutingRule(ctx, id)
				require.NoError(t, getErr)
				require.Equalf(t, want, got.Priority, "priority for %s", id)
			}
		})
	}
}

// TestRDBConfigStore_RoutingRuleUpdateOmittedEnabled pins the update path for a rule whose
// enabled field is omitted, as every config.json rule without "enabled" is. Save writes every
// column, so a nil Enabled used to write NULL into the NOT NULL column and fail startup.
func TestRDBConfigStore_RoutingRuleUpdateOmittedEnabled(t *testing.T) {
	ctx := context.Background()

	updaters := map[string]func(store *RDBConfigStore, rule *tables.TableRoutingRule) error{
		"SyncRoutingRules": func(store *RDBConfigStore, rule *tables.TableRoutingRule) error {
			return store.SyncRoutingRules(ctx, nil, []tables.TableRoutingRule{*rule})
		},
		"UpdateRoutingRule": func(store *RDBConfigStore, rule *tables.TableRoutingRule) error {
			return store.UpdateRoutingRule(ctx, rule)
		},
	}

	for name, update := range updaters {
		for _, stored := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stored=%t", name, stored), func(t *testing.T) {
				store := setupRDBTestStore(t)
				created := routingRuleFixture("rule-a", 0, "openai")
				created.Enabled = new(stored)
				require.NoError(t, store.CreateRoutingRule(ctx, created))

				incoming := routingRuleFixture("rule-a", 0, "openai")
				incoming.Enabled = nil
				require.NoError(t, update(store, incoming))

				got, err := store.GetRoutingRule(ctx, "rule-a")
				require.NoError(t, err)
				require.NotNil(t, got.Enabled)
				require.Equal(t, stored, *got.Enabled, "omitted enabled must keep the stored value")
			})
		}
	}
}

// TestUpsertModelPricesBatch_InputCostPerQuerySurvivesResync guards the ON CONFLICT DO UPDATE
// column list. Create() writes every column, so a first sync looks correct even when a field is
// missing from pricingSyncUpdateColumns - the value only disappears on the next resync of an
// existing row, which is 24h later in production.
func TestUpsertModelPricesBatch_InputCostPerQuerySurvivesResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	row := tables.TableModelPricing{Model: "rerank-v3.5", Provider: "cohere", Mode: "rerank"}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{row}))

	row.InputCostPerQuery = cost(0.002)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{row}))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	var found *tables.TableModelPricing
	for i := range got {
		if got[i].Model == "rerank-v3.5" {
			found = &got[i]
			break
		}
	}
	require.NotNil(t, found)
	require.NotNil(t, found.InputCostPerQuery, "input_cost_per_query missing from pricingSyncUpdateColumns")
	assert.Equal(t, 0.002, *found.InputCostPerQuery)
}

// TestUpsertModelPricesBatch_SizeQualityImageColumnsSurviveResync pins the
// ON CONFLICT DO UPDATE path: a column missing from pricingSyncUpdateColumns is
// written on the initial Create but silently dropped on every later sync of an
// existing row.
func TestUpsertModelPricesBatch_SizeQualityImageColumnsSurviveResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	// First sync: the row exists with none of the new rates set.
	row := tables.TableModelPricing{Model: "gpt-image-1", Provider: "openai", Mode: "image_generation"}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{row}))

	// Second sync of the same row now carries them, exercising the update path.
	row.OutputCostPerImageAbove1024x1536Pixels = cost(0.001)
	row.OutputCostPerImageAbove1536x1024Pixels = cost(0.002)
	row.OutputCostPerImageAbove1024x1024PixelsLowQuality = cost(0.003)
	row.OutputCostPerImageAbove1024x1536PixelsLowQuality = cost(0.004)
	row.OutputCostPerImageAbove1536x1024PixelsLowQuality = cost(0.005)
	row.OutputCostPerImageAbove1024x1024PixelsMediumQuality = cost(0.006)
	row.OutputCostPerImageAbove1024x1536PixelsMediumQuality = cost(0.007)
	row.OutputCostPerImageAbove1536x1024PixelsMediumQuality = cost(0.008)
	row.OutputCostPerImageAbove1024x1024PixelsHighQuality = cost(0.009)
	row.OutputCostPerImageAbove1024x1536PixelsHighQuality = cost(0.010)
	row.OutputCostPerImageAbove1536x1024PixelsHighQuality = cost(0.011)
	row.OutputCostPerImageAbove1024x1024PixelsStandardQuality = cost(0.012)
	row.OutputCostPerImageAbove1024x1536PixelsStandardQuality = cost(0.013)
	row.OutputCostPerImageAbove1536x1024PixelsStandardQuality = cost(0.014)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{row}))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	updated := got[0]

	require.NotNil(t, updated.OutputCostPerImageAbove1024x1536Pixels, "OutputCostPerImageAbove1024x1536Pixels was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.001, *updated.OutputCostPerImageAbove1024x1536Pixels, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1536x1024Pixels, "OutputCostPerImageAbove1536x1024Pixels was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.002, *updated.OutputCostPerImageAbove1536x1024Pixels, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1024PixelsLowQuality, "OutputCostPerImageAbove1024x1024PixelsLowQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.003, *updated.OutputCostPerImageAbove1024x1024PixelsLowQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1536PixelsLowQuality, "OutputCostPerImageAbove1024x1536PixelsLowQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.004, *updated.OutputCostPerImageAbove1024x1536PixelsLowQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1536x1024PixelsLowQuality, "OutputCostPerImageAbove1536x1024PixelsLowQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.005, *updated.OutputCostPerImageAbove1536x1024PixelsLowQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1024PixelsMediumQuality, "OutputCostPerImageAbove1024x1024PixelsMediumQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.006, *updated.OutputCostPerImageAbove1024x1024PixelsMediumQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1536PixelsMediumQuality, "OutputCostPerImageAbove1024x1536PixelsMediumQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.007, *updated.OutputCostPerImageAbove1024x1536PixelsMediumQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1536x1024PixelsMediumQuality, "OutputCostPerImageAbove1536x1024PixelsMediumQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.008, *updated.OutputCostPerImageAbove1536x1024PixelsMediumQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1024PixelsHighQuality, "OutputCostPerImageAbove1024x1024PixelsHighQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.009, *updated.OutputCostPerImageAbove1024x1024PixelsHighQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1536PixelsHighQuality, "OutputCostPerImageAbove1024x1536PixelsHighQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.010, *updated.OutputCostPerImageAbove1024x1536PixelsHighQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1536x1024PixelsHighQuality, "OutputCostPerImageAbove1536x1024PixelsHighQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.011, *updated.OutputCostPerImageAbove1536x1024PixelsHighQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1024PixelsStandardQuality, "OutputCostPerImageAbove1024x1024PixelsStandardQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.012, *updated.OutputCostPerImageAbove1024x1024PixelsStandardQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1024x1536PixelsStandardQuality, "OutputCostPerImageAbove1024x1536PixelsStandardQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.013, *updated.OutputCostPerImageAbove1024x1536PixelsStandardQuality, 1e-9)
	require.NotNil(t, updated.OutputCostPerImageAbove1536x1024PixelsStandardQuality, "OutputCostPerImageAbove1536x1024PixelsStandardQuality was dropped by the ON CONFLICT update (missing from pricingSyncUpdateColumns)")
	assert.InDelta(t, 0.014, *updated.OutputCostPerImageAbove1536x1024PixelsStandardQuality, 1e-9)
}

// TestRDBConfigStore_RoutingRuleCreatedAtSurvivesUpdate guards created_at against GORM's Save,
// which selects every column: rules arriving from a config.json reload carry no created_at, so
// without an explicit carry-forward the original insert timestamp is overwritten with the zero
// time. Both the batch (SyncRoutingRules) and single (UpdateRoutingRule) paths are covered.
func TestRDBConfigStore_RoutingRuleCreatedAtSurvivesUpdate(t *testing.T) {
	ctx := context.Background()

	assertCreatedAt := func(t *testing.T, store *RDBConfigStore, id string, want time.Time) {
		t.Helper()
		got, err := store.GetRoutingRule(ctx, id)
		require.NoError(t, err)
		require.False(t, got.CreatedAt.IsZero(), "created_at must not be zeroed by an update")
		require.Equal(t, want, got.CreatedAt, "created_at must be preserved")
	}

	t.Run("SyncRoutingRules preserves created_at", func(t *testing.T) {
		store := setupRDBTestStore(t)
		require.NoError(t, store.CreateRoutingRule(ctx, routingRuleFixture("rule-a", 0, "openai")))
		created, err := store.GetRoutingRule(ctx, "rule-a")
		require.NoError(t, err)
		require.False(t, created.CreatedAt.IsZero())

		// Shape of a config-file rule: no created_at, changed payload.
		fileRule := routingRuleFixture("rule-a", 3, "anthropic")
		fileRule.Description = "edited in config.json"
		require.NoError(t, store.SyncRoutingRules(ctx, nil, []tables.TableRoutingRule{*fileRule}))

		assertCreatedAt(t, store, "rule-a", created.CreatedAt)
	})

	t.Run("UpdateRoutingRule preserves created_at", func(t *testing.T) {
		store := setupRDBTestStore(t)
		require.NoError(t, store.CreateRoutingRule(ctx, routingRuleFixture("rule-a", 0, "openai")))
		created, err := store.GetRoutingRule(ctx, "rule-a")
		require.NoError(t, err)
		require.False(t, created.CreatedAt.IsZero())

		partial := routingRuleFixture("rule-a", 3, "anthropic")
		partial.Description = "updated without reading the row first"
		require.NoError(t, store.UpdateRoutingRule(ctx, partial))

		assertCreatedAt(t, store, "rule-a", created.CreatedAt)
	})
}

// TestRDBConfigStore_RoutingRuleDuplicatePriority pins the conflict two rules sharing a priority in
// one scope raise on create and update: it is ErrAlreadyExists, so the API answers 409 rather than
// 500, and its message names the scope ID rather than the address of the pointer that holds it.
func TestRDBConfigStore_RoutingRuleDuplicatePriority(t *testing.T) {
	ctx := context.Background()
	store := setupRDBTestStore(t)
	scoped := func(id string, priority int, scopeID string) *tables.TableRoutingRule {
		rule := routingRuleFixture(id, priority, "openai")
		rule.Scope = "virtual_key"
		rule.ScopeID = &scopeID
		return rule
	}

	require.NoError(t, store.CreateRoutingRule(ctx, routingRuleFixture("global-a", 5, "openai")))
	err := store.CreateRoutingRule(ctx, routingRuleFixture("global-b", 5, "openai"))
	require.ErrorIs(t, err, ErrAlreadyExists)
	require.ErrorIs(t, err, ErrRoutingRulePriorityTaken, "a priority conflict is told apart from other conflicts")
	require.Contains(t, err.Error(), "priority 5")
	require.Contains(t, err.Error(), "scope 'global'")

	require.NoError(t, store.CreateRoutingRule(ctx, scoped("vk-a", 7, "vk-1")))
	err = store.CreateRoutingRule(ctx, scoped("vk-b", 7, "vk-1"))
	require.ErrorIs(t, err, ErrAlreadyExists)
	require.Contains(t, err.Error(), "'vk-1'")
	require.NotContains(t, err.Error(), "0x", "the scope ID must be printed, not its pointer")

	// The same priority in another scope, or under another scope ID, is no conflict.
	require.NoError(t, store.CreateRoutingRule(ctx, scoped("vk-c", 5, "vk-1")))
	require.NoError(t, store.CreateRoutingRule(ctx, scoped("vk-d", 7, "vk-2")))

	require.NoError(t, store.CreateRoutingRule(ctx, routingRuleFixture("global-c", 8, "openai")))
	err = store.UpdateRoutingRule(ctx, routingRuleFixture("global-c", 5, "openai"))
	require.ErrorIs(t, err, ErrAlreadyExists)
	err = store.UpdateRoutingRule(ctx, scoped("vk-d", 7, "vk-1"))
	require.ErrorIs(t, err, ErrAlreadyExists)
	require.Contains(t, err.Error(), "'vk-1'")
	require.NotContains(t, err.Error(), "0x", "the scope ID must be printed, not its pointer")
}

// TestRDBConfigStore_RoutingTargetTTFTTimeoutRoundTrip pins a target's
// ttft_timeout_ms through create, read, update and clearing it back to nil.
func TestRDBConfigStore_RoutingTargetTTFTTimeoutRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := setupRDBTestStore(t)

	rule := routingRuleFixture("rule-ttft", 0, "openai")
	require.NotEmpty(t, rule.Targets)
	rule.Targets[0].TTFTTimeoutMs = new(1500)
	require.NoError(t, store.CreateRoutingRule(ctx, rule))
	got, err := store.GetRoutingRule(ctx, "rule-ttft")
	require.NoError(t, err)
	require.NotEmpty(t, got.Targets)
	require.NotNil(t, got.Targets[0].TTFTTimeoutMs)
	require.Equal(t, 1500, *got.Targets[0].TTFTTimeoutMs)

	got.Targets[0].TTFTTimeoutMs = new(800)
	require.NoError(t, store.UpdateRoutingRule(ctx, got))
	got, err = store.GetRoutingRule(ctx, "rule-ttft")
	require.NoError(t, err)
	require.NotNil(t, got.Targets[0].TTFTTimeoutMs)
	require.Equal(t, 800, *got.Targets[0].TTFTTimeoutMs)

	got.Targets[0].TTFTTimeoutMs = nil
	require.NoError(t, store.UpdateRoutingRule(ctx, got))
	got, err = store.GetRoutingRule(ctx, "rule-ttft")
	require.NoError(t, err)
	require.Nil(t, got.Targets[0].TTFTTimeoutMs, "clearing the field must persist as NULL")
}

// TestGenerateRoutingRuleHash_TargetTTFTTimeout: a rule whose targets carry no
// deadline keeps the hash it had before the field existed, and setting one
// changes the hash.
func TestGenerateRoutingRuleHash_TargetTTFTTimeout(t *testing.T) {
	// Hash of this fixture from the implementation before ttft_timeout_ms
	// existed. Comparing two current hashes would not catch a change to it.
	const preTTFTHash = "f9808962adb5bd07a2da3521037890b40ebafbafc61881986c3830f6d362e053"

	rule := *routingRuleFixture("rule-hash", 0, "openai")
	require.NotEmpty(t, rule.Targets)
	rule.Targets = append([]tables.TableRoutingTarget(nil), rule.Targets...)
	unset, err := GenerateRoutingRuleHash(rule)
	require.NoError(t, err)
	require.Equal(t, preTTFTHash, unset, "a target without ttft_timeout_ms must keep its pre-upgrade hash")

	rule.Targets[0].TTFTTimeoutMs = new(1500)
	set, err := GenerateRoutingRuleHash(rule)
	require.NoError(t, err)
	require.NotEqual(t, unset, set, "setting ttft_timeout_ms must change the config hash")

	rule.Targets[0].TTFTTimeoutMs = new(2000)
	changed, err := GenerateRoutingRuleHash(rule)
	require.NoError(t, err)
	require.NotEqual(t, set, changed, "changing ttft_timeout_ms must change the config hash")

	rule.Targets[0].TTFTTimeoutMs = nil
	cleared, err := GenerateRoutingRuleHash(rule)
	require.NoError(t, err)
	require.Equal(t, unset, cleared, "a target without ttft_timeout_ms must keep its pre-upgrade hash")
}

// TestUpsertModelPricesBatch_VideoResolutionColumnsSurviveResync pins the
// ON CONFLICT DO UPDATE path: a column missing from pricingSyncUpdateColumns is
// written on the initial Create but silently dropped on every later sync of an
// existing row.
func TestUpsertModelPricesBatch_VideoResolutionColumnsSurviveResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	cost := func(f float64) *float64 { return &f }

	row := tables.TableModelPricing{Model: "sora-2-pro", Provider: "openai", Mode: "video_generation"}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{row}))

	row.OutputCostPerVideoPerSecond480p = cost(0.10)
	row.OutputCostPerVideoPerSecond720p = cost(0.30)
	row.OutputCostPerVideoPerSecond1024p = cost(0.50)
	row.OutputCostPerVideoPerSecond1080p = cost(0.70)
	row.OutputCostPerVideoPerSecond4k = cost(0.60)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, []tables.TableModelPricing{row}))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	var found *tables.TableModelPricing
	for i := range got {
		if got[i].Model == "sora-2-pro" {
			found = &got[i]
			break
		}
	}
	require.NotNil(t, found)
	require.NotNil(t, found.OutputCostPerVideoPerSecond480p, "output_cost_per_video_per_second_480p missing from pricingSyncUpdateColumns")
	require.NotNil(t, found.OutputCostPerVideoPerSecond720p, "output_cost_per_video_per_second_720p missing from pricingSyncUpdateColumns")
	require.NotNil(t, found.OutputCostPerVideoPerSecond1024p, "output_cost_per_video_per_second_1024p missing from pricingSyncUpdateColumns")
	require.NotNil(t, found.OutputCostPerVideoPerSecond1080p, "output_cost_per_video_per_second_1080p missing from pricingSyncUpdateColumns")
	require.NotNil(t, found.OutputCostPerVideoPerSecond4k, "output_cost_per_video_per_second_4k missing from pricingSyncUpdateColumns")
	assert.Equal(t, 0.10, *found.OutputCostPerVideoPerSecond480p)
	assert.Equal(t, 0.30, *found.OutputCostPerVideoPerSecond720p)
	assert.Equal(t, 0.50, *found.OutputCostPerVideoPerSecond1024p)
	assert.Equal(t, 0.70, *found.OutputCostPerVideoPerSecond1080p)
	assert.Equal(t, 0.60, *found.OutputCostPerVideoPerSecond4k)
}

// TestRDBConfigStore_CreatedAtSurvivesConfigSync is the general form of
// TestRDBConfigStore_RoutingRuleCreatedAtSurvivesUpdate. GORM's Save selects every
// column, so any entity rebuilt from config.json — which carries no created_at —
// has its original insert timestamp overwritten with the zero time unless the
// store carries the persisted value forward.
func TestRDBConfigStore_CreatedAtSurvivesConfigSync(t *testing.T) {
	ctx := context.Background()

	// seed inserts the entity and returns its persisted created_at. sync re-saves it
	// shaped exactly the way the config.json reconciler builds one: same ID, changed
	// payload, zero CreatedAt. verify asserts the changed payload actually landed
	// before returning the created_at that survived, so an Update that quietly did
	// nothing cannot pass a case just by leaving the row alone.
	tests := []struct {
		name   string
		seed   func(t *testing.T, store *RDBConfigStore) time.Time
		sync   func(t *testing.T, store *RDBConfigStore)
		verify func(t *testing.T, store *RDBConfigStore) time.Time
	}{
		{
			name: "RateLimit",
			seed: func(t *testing.T, store *RDBConfigStore) time.Time {
				max := int64(1000)
				require.NoError(t, store.CreateRateLimit(ctx, &tables.TableRateLimit{
					ID: "rl-a", TokenMaxLimit: &max, TokenResetDuration: strPtr("1h"),
				}))
				created, err := store.GetRateLimit(ctx, "rl-a")
				require.NoError(t, err)
				return created.CreatedAt
			},
			sync: func(t *testing.T, store *RDBConfigStore) {
				newMax := int64(2000)
				require.NoError(t, store.UpdateRateLimit(ctx, &tables.TableRateLimit{
					ID: "rl-a", TokenMaxLimit: &newMax, TokenResetDuration: strPtr("1h"),
				}))
			},
			verify: func(t *testing.T, store *RDBConfigStore) time.Time {
				got, err := store.GetRateLimit(ctx, "rl-a")
				require.NoError(t, err)
				require.Equal(t, int64(2000), *got.TokenMaxLimit)
				return got.CreatedAt
			},
		},
		{
			name: "Team",
			seed: func(t *testing.T, store *RDBConfigStore) time.Time {
				require.NoError(t, store.CreateTeam(ctx, &tables.TableTeam{ID: "team-a", Name: "Team A"}))
				created, err := store.GetTeam(ctx, "team-a")
				require.NoError(t, err)
				return created.CreatedAt
			},
			sync: func(t *testing.T, store *RDBConfigStore) {
				require.NoError(t, store.UpdateTeam(ctx, &tables.TableTeam{ID: "team-a", Name: "Team A renamed"}))
			},
			verify: func(t *testing.T, store *RDBConfigStore) time.Time {
				got, err := store.GetTeam(ctx, "team-a")
				require.NoError(t, err)
				require.Equal(t, "Team A renamed", got.Name)
				return got.CreatedAt
			},
		},
		{
			name: "Customer",
			seed: func(t *testing.T, store *RDBConfigStore) time.Time {
				require.NoError(t, store.CreateCustomer(ctx, &tables.TableCustomer{ID: "cust-a", Name: "Customer A"}))
				created, err := store.GetCustomer(ctx, "cust-a")
				require.NoError(t, err)
				return created.CreatedAt
			},
			sync: func(t *testing.T, store *RDBConfigStore) {
				require.NoError(t, store.UpdateCustomer(ctx, &tables.TableCustomer{ID: "cust-a", Name: "Customer A renamed"}))
			},
			verify: func(t *testing.T, store *RDBConfigStore) time.Time {
				got, err := store.GetCustomer(ctx, "cust-a")
				require.NoError(t, err)
				require.Equal(t, "Customer A renamed", got.Name)
				return got.CreatedAt
			},
		},
		{
			name: "ModelConfig",
			seed: func(t *testing.T, store *RDBConfigStore) time.Time {
				require.NoError(t, store.CreateModelConfig(ctx, &tables.TableModelConfig{
					ID: "mc-a", ModelName: "gpt-4o", Scope: "global",
				}))
				created, err := store.GetModelConfig(ctx, "global", nil, "gpt-4o", nil)
				require.NoError(t, err)
				return created.CreatedAt
			},
			sync: func(t *testing.T, store *RDBConfigStore) {
				require.NoError(t, store.UpdateModelConfig(ctx, &tables.TableModelConfig{
					ID: "mc-a", ModelName: "gpt-4o", Scope: "global", CalendarAligned: true,
				}))
			},
			verify: func(t *testing.T, store *RDBConfigStore) time.Time {
				got, err := store.GetModelConfig(ctx, "global", nil, "gpt-4o", nil)
				require.NoError(t, err)
				require.True(t, got.CalendarAligned)
				return got.CreatedAt
			},
		},
		{
			name: "PricingOverride",
			seed: func(t *testing.T, store *RDBConfigStore) time.Time {
				require.NoError(t, store.CreatePricingOverride(ctx, &tables.TablePricingOverride{
					ID: "po-a", Name: "Override A", ScopeKind: "global", MatchType: "exact", Pattern: "gpt-4o",
				}))
				created, err := store.GetPricingOverrideByID(ctx, "po-a")
				require.NoError(t, err)
				return created.CreatedAt
			},
			sync: func(t *testing.T, store *RDBConfigStore) {
				require.NoError(t, store.UpdatePricingOverride(ctx, &tables.TablePricingOverride{
					ID: "po-a", Name: "Override A renamed", ScopeKind: "global", MatchType: "exact", Pattern: "gpt-4o",
				}))
			},
			verify: func(t *testing.T, store *RDBConfigStore) time.Time {
				got, err := store.GetPricingOverrideByID(ctx, "po-a")
				require.NoError(t, err)
				require.Equal(t, "Override A renamed", got.Name)
				return got.CreatedAt
			},
		},
		{
			// UpdatePlugin is a delete-and-reinsert, so without a carry-forward
			// created_at is re-stamped to now() rather than zeroed.
			name: "Plugin",
			seed: func(t *testing.T, store *RDBConfigStore) time.Time {
				require.NoError(t, store.CreatePlugin(ctx, &tables.TablePlugin{Name: "plug-a", Enabled: true}))
				created, err := store.GetPlugin(ctx, "plug-a")
				require.NoError(t, err)
				return created.CreatedAt
			},
			sync: func(t *testing.T, store *RDBConfigStore) {
				require.NoError(t, store.UpdatePlugin(ctx, &tables.TablePlugin{Name: "plug-a", Enabled: false}))
			},
			verify: func(t *testing.T, store *RDBConfigStore) time.Time {
				got, err := store.GetPlugin(ctx, "plug-a")
				require.NoError(t, err)
				require.False(t, got.Enabled)
				return got.CreatedAt
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := setupRDBTestStore(t)

			createdAt := tt.seed(t, store)
			require.False(t, createdAt.IsZero())

			tt.sync(t, store)

			syncedAt := tt.verify(t, store)
			require.False(t, syncedAt.IsZero())
			require.Equal(t, createdAt, syncedAt, "created_at must survive a config sync")
		})
	}
}

func TestListExpiredVirtualKeysForDeletion(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	keys := []*tables.TableVirtualKey{
		{ID: "vk-expired-flagged", Name: "expired flagged", Value: *schemas.NewSecretVar("v1"), ExpiresAt: &past, DeleteAfterExpire: schemas.Ptr(true)},
		{ID: "vk-expired-inactive-flagged", Name: "expired inactive flagged", Value: *schemas.NewSecretVar("v2"), ExpiresAt: &past, DeleteAfterExpire: schemas.Ptr(true), IsActive: schemas.Ptr(false)},
		{ID: "vk-expired-unset", Name: "expired unset", Value: *schemas.NewSecretVar("v3"), ExpiresAt: &past},
		{ID: "vk-expired-opt-out", Name: "expired opt out", Value: *schemas.NewSecretVar("v7"), ExpiresAt: &past, DeleteAfterExpire: schemas.Ptr(false)},
		{ID: "vk-future-flagged", Name: "future flagged", Value: *schemas.NewSecretVar("v4"), ExpiresAt: &future, DeleteAfterExpire: schemas.Ptr(true)},
		{ID: "vk-never-flagged", Name: "never flagged", Value: *schemas.NewSecretVar("v5"), DeleteAfterExpire: schemas.Ptr(true)},
	}
	for _, vk := range keys {
		require.NoError(t, store.CreateVirtualKey(ctx, vk))
	}
	// An omitted flag must round-trip as NULL, not the column default.
	unset, err := store.GetVirtualKey(ctx, "vk-expired-unset")
	require.NoError(t, err)
	assert.Nil(t, unset.DeleteAfterExpire)

	listIDs := func(includeUnset bool) []string {
		got, err := store.ListExpiredVirtualKeysForDeletion(ctx, now, includeUnset)
		require.NoError(t, err)
		var ids []string
		for _, vk := range got {
			ids = append(ids, vk.ID)
			assert.NotEmpty(t, vk.Name)
		}
		sort.Strings(ids)
		return ids
	}

	// Client default off: only explicitly flagged keys.
	assert.Equal(t, []string{"vk-expired-flagged", "vk-expired-inactive-flagged"}, listIDs(false))
	// Client default on: unset keys join, explicit false stays out.
	assert.Equal(t, []string{"vk-expired-flagged", "vk-expired-inactive-flagged", "vk-expired-unset"}, listIDs(true))

	// A key whose expiry is exactly now counts as expired, matching IsExpiredAt.
	boundary := &tables.TableVirtualKey{ID: "vk-boundary", Name: "boundary", Value: *schemas.NewSecretVar("v6"), ExpiresAt: &now, DeleteAfterExpire: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, boundary))
	assert.Len(t, listIDs(false), 3)
}

func TestVirtualKeyDeleteAfterExpireUpdateRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	future := time.Now().UTC().Add(time.Hour)
	vk := &tables.TableVirtualKey{ID: "vk-rt", Name: "rt", Value: *schemas.NewSecretVar("rt"), ExpiresAt: &future, DeleteAfterExpire: schemas.Ptr(true)}
	require.NoError(t, store.CreateVirtualKey(ctx, vk))

	// UpdateVirtualKey uses an explicit column list; NULL must persist through it.
	vk.DeleteAfterExpire = nil
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	got, err := store.GetVirtualKey(ctx, vk.ID)
	require.NoError(t, err)
	assert.Nil(t, got.DeleteAfterExpire)

	vk.DeleteAfterExpire = schemas.Ptr(false)
	require.NoError(t, store.UpdateVirtualKey(ctx, vk))
	got, err = store.GetVirtualKey(ctx, vk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeleteAfterExpire)
	assert.False(t, *got.DeleteAfterExpire)
}

func TestDeleteExpiredVirtualKey_RechecksEligibility(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	keys := []*tables.TableVirtualKey{
		{ID: "vk-eligible", Name: "eligible", Value: *schemas.NewSecretVar("e1"), ExpiresAt: &past, DeleteAfterExpire: schemas.Ptr(true)},
		// Updated after the scan: the operator cleared the flag or extended the expiry.
		{ID: "vk-opted-out", Name: "opted out", Value: *schemas.NewSecretVar("e2"), ExpiresAt: &past, DeleteAfterExpire: schemas.Ptr(false)},
		{ID: "vk-extended", Name: "extended", Value: *schemas.NewSecretVar("e3"), ExpiresAt: &future, DeleteAfterExpire: schemas.Ptr(true)},
		{ID: "vk-unset", Name: "unset", Value: *schemas.NewSecretVar("e4"), ExpiresAt: &past},
	}
	for _, vk := range keys {
		require.NoError(t, store.CreateVirtualKey(ctx, vk))
	}

	for _, id := range []string{"vk-opted-out", "vk-extended", "vk-unset"} {
		deleted, err := store.DeleteExpiredVirtualKey(ctx, id, now, false)
		require.NoError(t, err)
		assert.Nil(t, deleted, "%s no longer qualifies and must survive", id)
		_, err = store.GetVirtualKey(ctx, id)
		assert.NoError(t, err, "%s was deleted despite being ineligible", id)
	}

	deleted, err := store.DeleteExpiredVirtualKey(ctx, "vk-eligible", now, false)
	require.NoError(t, err)
	require.NotNil(t, deleted)
	assert.Equal(t, "eligible", deleted.Name)
	_, err = store.GetVirtualKey(ctx, "vk-eligible")
	assert.ErrorIs(t, err, ErrNotFound)

	// The job scanned with the default on, but an admin turned it off before the delete:
	// the unset key must follow the current default and survive.
	require.NoError(t, store.UpdateClientConfig(ctx, &ClientConfig{DeleteExpiredVirtualKeys: false}))
	deleted, err = store.DeleteExpiredVirtualKey(ctx, "vk-unset", now, true)
	require.NoError(t, err)
	assert.Nil(t, deleted, "unset key was deleted after the client default was turned off")
	_, err = store.GetVirtualKey(ctx, "vk-unset")
	require.NoError(t, err)

	// An unset flag follows the client default when it is on.
	require.NoError(t, store.UpdateClientConfig(ctx, &ClientConfig{DeleteExpiredVirtualKeys: true}))
	deleted, err = store.DeleteExpiredVirtualKey(ctx, "vk-unset", now, true)
	require.NoError(t, err)
	require.NotNil(t, deleted)

	_, err = store.DeleteExpiredVirtualKey(ctx, "vk-missing", now, true)
	assert.ErrorIs(t, err, ErrNotFound)
}

// followupsMaxVarsRecorder registers query callbacks on db that record the
// largest number of bind parameters any SELECT carried, so a test can pin that
// a read never binds one parameter per id or per result row.
func followupsMaxVarsRecorder(t *testing.T, db *gorm.DB) func() int {
	t.Helper()
	var mu sync.Mutex
	maxVars := 0
	name := fmt.Sprintf("followups:maxvars:%s", t.Name())
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		if n := len(tx.Statement.Vars); n > maxVars {
			maxVars = n
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return maxVars
	}
}

// followupsSeedScopedModelConfigs inserts count virtual_key-scoped model configs,
// perScope per scope id vk-s-<n>, each owning one multi budget (bm-i) and pointing
// at one legacy budget (bl-i) and one rate limit (rl-i). Even-numbered configs are
// calendar aligned.
func followupsSeedScopedModelConfigs(t *testing.T, db *gorm.DB, count, perScope int) {
	t.Helper()
	inListSeedSeries(t, db, count, `INSERT INTO governance_budgets
		(id, max_limit, reset_duration, last_reset, current_usage, override_amount, override_mode, override_cycles_remaining, override_cycles_total, created_at, updated_at)
		SELECT 'bl-' || i, 1, '1d', CURRENT_TIMESTAMP, 0, 0, '', 0, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM n`)
	inListSeedSeries(t, db, count, `INSERT INTO governance_rate_limits
		(id, created_at, updated_at)
		SELECT 'rl-' || i, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM n`)
	inListSeedSeries(t, db, count, fmt.Sprintf(`INSERT INTO governance_model_configs
		(id, model_name, provider, scope, scope_id, calendar_aligned, budget_id, rate_limit_id, config_hash, created_at, updated_at)
		SELECT 'mc-' || i, 'model-' || i, 'openai', 'virtual_key', 'vk-s-' || ((i - 1) / %d), (i %% 2 = 0), 'bl-' || i, 'rl-' || i, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM n`, perScope))
	inListSeedSeries(t, db, count, `INSERT INTO governance_budgets
		(id, max_limit, reset_duration, last_reset, current_usage, override_amount, override_mode, override_cycles_remaining, override_cycles_total, model_config_id, created_at, updated_at)
		SELECT 'bm-' || i, 1, '1M', CURRENT_TIMESTAMP, 0, 0, '', 0, 0, 'mc-' || i, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM n`)
}

// followupsAssertModelConfigs checks that got holds exactly the configs mc-1..count
// with every relation attached and calendar alignment stamped onto owned budgets.
func followupsAssertModelConfigs(t *testing.T, got []tables.TableModelConfig, count int) {
	t.Helper()
	require.Len(t, got, count)
	seen := make(map[string]struct{}, count)
	for _, mc := range got {
		var n int
		_, err := fmt.Sscanf(mc.ID, "mc-%d", &n)
		require.NoError(t, err)
		seen[mc.ID] = struct{}{}
		require.Len(t, mc.Budgets, 1, mc.ID)
		require.Equal(t, fmt.Sprintf("bm-%d", n), mc.Budgets[0].ID)
		require.Equal(t, mc.CalendarAligned, mc.Budgets[0].IsCalendarAligned, mc.ID)
		require.Equal(t, n%2 == 0, mc.CalendarAligned, mc.ID)
		require.NotNil(t, mc.Budget, mc.ID)
		require.Equal(t, fmt.Sprintf("bl-%d", n), mc.Budget.ID)
		require.NotNil(t, mc.RateLimit, mc.ID)
		require.Equal(t, fmt.Sprintf("rl-%d", n), mc.RateLimit.ID)
	}
	require.Len(t, seen, count)
}

// TestFollowupsGetModelConfigsByScopeAndScopeIDsBindsFewParams pins that loading
// the model configs of more scope ids than the bind-parameter limit, whose result
// rows also outnumber that limit, succeeds with every relation attached and never
// binds more than a handful of parameters per statement.
func TestFollowupsGetModelConfigsByScopeAndScopeIDsBindsFewParams(t *testing.T) {
	const scopes = 1000
	const perScope = 40
	const count = scopes * perScope
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			followupsSeedScopedModelConfigs(t, ns.db, count, perScope)
			ids := inListSyntheticIDs("missing", inListOverLimit)
			for i := range scopes {
				ids = append(ids, fmt.Sprintf("vk-s-%d", i))
			}
			maxVars := followupsMaxVarsRecorder(t, ns.db)

			got, err := ns.store.GetModelConfigsByScopeAndScopeIDs(context.Background(), tables.ModelConfigScopeVirtualKey, ids)
			require.NoError(t, err)
			followupsAssertModelConfigs(t, got, count)
			require.LessOrEqual(t, maxVars(), 4, "a statement bound one parameter per id or per row")
		})
	}
}

// TestFollowupsGetModelConfigsByScopeAndScopeIDsSubBatches pins that when a
// chunk of scope ids yields more configs than one preload batch, relations are
// still attached to every config, and that the result keeps the order of a
// single unbatched read.
func TestFollowupsGetModelConfigsByScopeAndScopeIDsSubBatches(t *testing.T) {
	restoreChunk, restoreBatch := modelConfigScopeIDChunkSize, modelConfigPreloadBatchSize
	modelConfigScopeIDChunkSize, modelConfigPreloadBatchSize = 3, 7
	t.Cleanup(func() { modelConfigScopeIDChunkSize, modelConfigPreloadBatchSize = restoreChunk, restoreBatch })

	const scopes = 10
	const perScope = 5
	const count = scopes * perScope
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			followupsSeedScopedModelConfigs(t, ns.db, count, perScope)
			ids := make([]string, 0, scopes)
			for i := range scopes {
				ids = append(ids, fmt.Sprintf("vk-s-%d", i))
			}
			got, err := ns.store.GetModelConfigsByScopeAndScopeIDs(context.Background(), tables.ModelConfigScopeVirtualKey, ids)
			require.NoError(t, err)
			followupsAssertModelConfigs(t, got, count)

			// A single scope id is one chunk; its rows come back in the same order
			// as the plain read the old implementation issued.
			var want []tables.TableModelConfig
			require.NoError(t, ns.db.Where("scope = ? AND scope_id = ?", tables.ModelConfigScopeVirtualKey, "vk-s-4").Find(&want).Error)
			one, err := ns.store.GetModelConfigsByScopeAndScopeIDs(context.Background(), tables.ModelConfigScopeVirtualKey, []string{"vk-s-4"})
			require.NoError(t, err)
			require.Len(t, one, len(want))
			for i := range want {
				require.Equal(t, want[i].ID, one[i].ID)
			}
		})
	}
}

// TestFollowupsGetModelConfigsByScopeAndScopeIDsDedupesAcrossChunks pins that a
// scope id repeated on both sides of a chunk boundary returns its configs once,
// as the single IN query did before the id list was chunked.
func TestFollowupsGetModelConfigsByScopeAndScopeIDsDedupesAcrossChunks(t *testing.T) {
	restoreChunk := modelConfigScopeIDChunkSize
	modelConfigScopeIDChunkSize = 2
	t.Cleanup(func() { modelConfigScopeIDChunkSize = restoreChunk })

	const scopes = 3
	const perScope = 2
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			followupsSeedScopedModelConfigs(t, ns.db, scopes*perScope, perScope)
			// vk-s-0 is at index 0 (first chunk) and index 2 (second chunk).
			ids := []string{"vk-s-0", "vk-s-1", "vk-s-0", "vk-s-2"}
			got, err := ns.store.GetModelConfigsByScopeAndScopeIDs(context.Background(), tables.ModelConfigScopeVirtualKey, ids)
			require.NoError(t, err)
			followupsAssertModelConfigs(t, got, scopes*perScope)
		})
	}
}

// highFixPGSchema isolates these tests from other Postgres-backed tests in this package.
const highFixPGSchema = "configstore_highfix_test"

// namedStore pairs a backend name with a store for dialect subtests.
type namedStore struct {
	name  string
	store *RDBConfigStore
}

// highFixStores returns an in-memory SQLite store and, when the local test
// Postgres is reachable, a Postgres store in a dedicated schema that went through
// the real migration chain.
func highFixStores(t *testing.T) []namedStore {
	t.Helper()
	stores := []namedStore{{name: "sqlite", store: setupRDBTestStore(t)}}

	dsn := strings.Replace(postgresDSN, "search_path="+pgTestSchema, "search_path="+highFixPGSchema, 1)
	if dsn == postgresDSN {
		return stores
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return stores
	}
	sqlDB, err := db.DB()
	if err != nil || sqlDB.Ping() != nil {
		return stores
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.Exec("DROP SCHEMA IF EXISTS "+highFixPGSchema+" CASCADE").Error)
	require.NoError(t, db.Exec("CREATE SCHEMA "+highFixPGSchema).Error)
	require.NoError(t, triggerMigrations(context.Background(), db, testMigrationLogger))

	store := &RDBConfigStore{logger: bifrost.NewDefaultLogger(schemas.LogLevelError)}
	store.db.Store(db)
	store.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, store.DB())
	}
	store.refreshPoolFn = func(ctx context.Context) error { return nil }
	return append(stores, namedStore{name: "postgres", store: store})
}

// statementCounter counts executed SQL statements that match a predicate, so a
// test can prove a path issues a constant number of statements instead of one per row.
type statementCounter struct {
	n     atomic.Int64
	match func(sql string) bool
}

// count returns the number of matching statements seen so far.
func (c *statementCounter) count() int64 { return c.n.Load() }

// countStatements registers after-callbacks on every GORM processor and counts the
// statements whose SQL satisfies match. The callbacks are removed on cleanup.
func countStatements(t *testing.T, db *gorm.DB, match func(sql string) bool) *statementCounter {
	t.Helper()
	c := &statementCounter{match: match}
	name := fmt.Sprintf("highfix:count:%p", c)
	hook := func(tx *gorm.DB) {
		if c.match(strings.ToUpper(tx.Statement.SQL.String())) {
			c.n.Add(1)
		}
	}
	cb := db.Callback()
	require.NoError(t, cb.Create().After("gorm:create").Register(name, hook))
	require.NoError(t, cb.Query().After("gorm:query").Register(name, hook))
	require.NoError(t, cb.Update().After("gorm:update").Register(name, hook))
	require.NoError(t, cb.Delete().After("gorm:delete").Register(name, hook))
	require.NoError(t, cb.Row().After("gorm:row").Register(name, hook))
	require.NoError(t, cb.Raw().After("gorm:raw").Register(name, hook))
	t.Cleanup(func() {
		_ = cb.Create().Remove(name)
		_ = cb.Query().Remove(name)
		_ = cb.Update().Remove(name)
		_ = cb.Delete().Remove(name)
		_ = cb.Row().Remove(name)
		_ = cb.Raw().Remove(name)
	})
	return c
}

// deleteTarget returns the table a DELETE statement deletes from (upper-cased,
// unquoted), or "" when sql is not a DELETE. Tables named only in a subquery do not count.
func deleteTarget(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) < 3 || fields[0] != "DELETE" || fields[1] != "FROM" {
		return ""
	}
	return strings.Trim(fields[2], "`\"")
}

// createTestVK inserts a minimal virtual key with the given id and created_at.
func createTestVK(t *testing.T, db *gorm.DB, id string, createdAt time.Time) {
	t.Helper()
	vk := tables.TableVirtualKey{ID: id, Name: "name-" + id, Value: *schemas.NewSecretVar("value-" + id), CreatedAt: createdAt, UpdatedAt: createdAt}
	require.NoError(t, db.Omit("ProviderConfigs", "MCPConfigs").Create(&vk).Error)
}

// createTestMCPClient inserts a minimal MCP client row and returns it.
func createTestMCPClient(t *testing.T, db *gorm.DB, clientID string, allowOnAll bool) tables.TableMCPClient {
	t.Helper()
	c := tables.TableMCPClient{ClientID: clientID, Name: clientID, EndpointSlug: clientID, ConnectionType: "http", AllowByDefault: allowOnAll}
	require.NoError(t, db.Create(&c).Error)
	return c
}

// TestHighFixDeleteMCPClientConfigSetBased pins H13: deleting an MCP client removes
// every VK assignment for it with one statement, and leaves other clients' rows alone.
func TestHighFixDeleteMCPClientConfigSetBased(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.store.DB()
			target := createTestMCPClient(t, db, "mcp-target", false)
			other := createTestMCPClient(t, db, "mcp-other", false)
			now := time.Now().UTC()
			for i := 0; i < 12; i++ {
				vkID := fmt.Sprintf("vk-%02d", i)
				createTestVK(t, db, vkID, now)
				require.NoError(t, db.Create(&tables.TableVirtualKeyMCPConfig{VirtualKeyID: vkID, MCPClientID: target.ID}).Error)
				require.NoError(t, db.Create(&tables.TableVirtualKeyMCPConfig{VirtualKeyID: vkID, MCPClientID: other.ID}).Error)
			}

			deletes := countStatements(t, db, func(sql string) bool {
				return deleteTarget(sql) == "GOVERNANCE_VIRTUAL_KEY_MCP_CONFIGS"
			})
			require.NoError(t, ns.store.DeleteMCPClientConfig(ctx, target.ClientID))
			require.EqualValues(t, 1, deletes.count(), "VK assignments must be removed with one statement, not one per row")

			var remainingTarget, remainingOther int64
			require.NoError(t, db.Model(&tables.TableVirtualKeyMCPConfig{}).Where("mcp_client_id = ?", target.ID).Count(&remainingTarget).Error)
			require.NoError(t, db.Model(&tables.TableVirtualKeyMCPConfig{}).Where("mcp_client_id = ?", other.ID).Count(&remainingOther).Error)
			require.Zero(t, remainingTarget)
			require.EqualValues(t, 12, remainingOther)
			var clients int64
			require.NoError(t, db.Model(&tables.TableMCPClient{}).Where("client_id = ?", target.ClientID).Count(&clients).Error)
			require.Zero(t, clients)
		})
	}
}

// providerFixture is the state built by buildProviderFixture.
type providerFixture struct {
	keyIDs      map[string]uint // provider key_id -> config_keys.id
	targetVKPCs []uint          // provider configs for the provider under test
	otherVKPCs  []uint          // provider configs for another provider
}

// buildProviderFixture creates providers "openai" (keys k1, k2) and "anthropic"
// (key a1), and n VKs that each hold one provider config per provider with a
// budget, a rate limit and join rows to every key of that provider.
func buildProviderFixture(t *testing.T, s *RDBConfigStore, n int) providerFixture {
	t.Helper()
	ctx := context.Background()
	db := s.DB()
	require.NoError(t, s.UpdateProvidersConfig(ctx, map[schemas.ModelProvider]ProviderConfig{
		"openai": {Keys: []schemas.Key{
			{ID: "k1", Name: "k1", Value: *schemas.NewSecretVar("sk-1"), Weight: 1},
			{ID: "k2", Name: "k2", Value: *schemas.NewSecretVar("sk-2"), Weight: 1},
		}},
		"anthropic": {Keys: []schemas.Key{
			{ID: "a1", Name: "a1", Value: *schemas.NewSecretVar("sk-a1"), Weight: 1},
		}},
	}))
	var keys []tables.TableKey
	require.NoError(t, db.Find(&keys).Error)
	fx := providerFixture{keyIDs: map[string]uint{}}
	for _, k := range keys {
		fx.keyIDs[k.KeyID] = k.ID
	}
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		vkID := fmt.Sprintf("vk-p-%02d", i)
		createTestVK(t, db, vkID, now)
		for _, p := range []struct {
			provider string
			keys     []string
		}{{"openai", []string{"k1", "k2"}}, {"anthropic", []string{"a1"}}} {
			rlID := fmt.Sprintf("rl-%s-%s", p.provider, vkID)
			require.NoError(t, db.Create(&tables.TableRateLimit{ID: rlID}).Error)
			pc := tables.TableVirtualKeyProviderConfig{VirtualKeyID: vkID, Provider: p.provider, RateLimitID: &rlID}
			require.NoError(t, db.Omit("Keys", "Budgets", "RateLimit").Create(&pc).Error)
			pcID := pc.ID
			require.NoError(t, db.Create(&tables.TableBudget{ID: fmt.Sprintf("b-%s-%s", p.provider, vkID), MaxLimit: 10, ResetDuration: "1d", ProviderConfigID: &pcID}).Error)
			for _, k := range p.keys {
				require.NoError(t, db.Create(&tables.TableVirtualKeyProviderConfigKey{TableVirtualKeyProviderConfigID: pc.ID, TableKeyID: fx.keyIDs[k]}).Error)
			}
			if p.provider == "openai" {
				fx.targetVKPCs = append(fx.targetVKPCs, pc.ID)
			} else {
				fx.otherVKPCs = append(fx.otherVKPCs, pc.ID)
			}
		}
	}
	return fx
}

// countRows counts rows of table matching where.
func countTableRows(t *testing.T, db *gorm.DB, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	q := db.Table(table)
	if where != "" {
		q = q.Where(where, args...)
	}
	require.NoError(t, q.Count(&n).Error)
	return n
}

// TestHighFixDeleteProviderCleansProviderConfigsSetBased pins H14: deleting a
// provider removes its VK provider configs, their join rows, budgets and rate
// limits with a constant number of statements, and leaves other providers intact.
func TestHighFixDeleteProviderCleansProviderConfigsSetBased(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.store.DB()
			fx := buildProviderFixture(t, ns.store, 10)

			vkpcDeletes := countStatements(t, db, func(sql string) bool {
				return deleteTarget(sql) == "GOVERNANCE_VIRTUAL_KEY_PROVIDER_CONFIGS"
			})
			require.NoError(t, ns.store.DeleteProvider(ctx, "openai"))
			require.LessOrEqual(t, vkpcDeletes.count(), int64(1), "provider configs must be deleted set-based, not one statement per row")

			require.Zero(t, countTableRows(t, db, "governance_virtual_key_provider_configs", "provider = ?", "openai"))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_virtual_key_provider_configs", "provider = ?", "anthropic"))
			require.Zero(t, countTableRows(t, db, "governance_budgets", "id LIKE ?", "b-openai-%"))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_budgets", "id LIKE ?", "b-anthropic-%"))
			require.Zero(t, countTableRows(t, db, "governance_rate_limits", "id LIKE ?", "rl-openai-%"))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_rate_limits", "id LIKE ?", "rl-anthropic-%"))
			require.Zero(t, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id IN ?", []uint{fx.keyIDs["k1"], fx.keyIDs["k2"]}))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id = ?", fx.keyIDs["a1"]))
			if ns.name == "postgres" {
				// Provider keys go by ON DELETE CASCADE, which the SQLite test store does not enforce.
				require.Zero(t, countTableRows(t, db, "config_keys", "key_id IN ?", []string{"k1", "k2"}))
			}
		})
	}
}

// TestHighFixUpdateProviderRemovedKeysSetBased pins H14's UpdateProvider path:
// dropping a key removes its join rows from every VK provider config of that
// provider in one statement, and keeps the join rows of surviving keys.
func TestHighFixUpdateProviderRemovedKeysSetBased(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.store.DB()
			fx := buildProviderFixture(t, ns.store, 10)

			joinDeletes := countStatements(t, db, func(sql string) bool {
				return deleteTarget(sql) == "GOVERNANCE_VIRTUAL_KEY_PROVIDER_CONFIG_KEYS"
			})
			require.NoError(t, ns.store.UpdateProvider(ctx, "openai", ProviderConfig{Keys: []schemas.Key{
				{ID: "k1", Name: "k1", Value: *schemas.NewSecretVar("sk-1"), Weight: 1},
			}}))
			require.LessOrEqual(t, joinDeletes.count(), int64(1), "join rows must be deleted set-based, not one statement per provider config")

			require.Zero(t, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id = ?", fx.keyIDs["k2"]))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id = ?", fx.keyIDs["k1"]))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id = ?", fx.keyIDs["a1"]))
			require.EqualValues(t, 20, countTableRows(t, db, "governance_virtual_key_provider_configs", ""))
			require.Zero(t, countTableRows(t, db, "config_keys", "key_id = ?", "k2"))

			require.NoError(t, ns.store.DeleteProviderKey(ctx, "openai", "k1"))
			require.Zero(t, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id = ?", fx.keyIDs["k1"]))
			require.EqualValues(t, 10, countTableRows(t, db, "governance_virtual_key_provider_config_keys", "table_key_id = ?", fx.keyIDs["a1"]))
		})
	}
}

// TestHighFixVirtualKeyKeysetPaging pins H27: the row-comparison keyset returns
// exactly the rows, in exactly the order, of one ordered scan, including runs of
// identical created_at values that straddle page boundaries.
func TestHighFixVirtualKeyKeysetPaging(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.store.DB()
			base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			for i := 0; i < 23; i++ {
				// Groups of 4 share a created_at, with ids inserted out of order.
				createTestVK(t, db, fmt.Sprintf("vk-%02d", (i*7)%23), base.Add(time.Duration(i/4)*time.Second))
			}

			var want []tables.TableVirtualKey
			require.NoError(t, db.Order("created_at ASC, id ASC").Find(&want).Error)
			wantIDs := make([]string, 0, len(want))
			for _, vk := range want {
				wantIDs = append(wantIDs, vk.ID)
			}

			var gotIDs []string
			var lastCreatedAt time.Time
			var lastID string
			hasCursor := false
			for {
				page, err := ns.store.getVirtualKeysPage(ctx, 3, lastCreatedAt, lastID, hasCursor)
				require.NoError(t, err)
				if len(page) == 0 {
					break
				}
				for _, vk := range page {
					gotIDs = append(gotIDs, vk.ID)
				}
				lastCreatedAt, lastID, hasCursor = page[len(page)-1].CreatedAt, page[len(page)-1].ID, true
			}
			require.Equal(t, wantIDs, gotIDs)

			all, err := ns.store.GetVirtualKeys(ctx)
			require.NoError(t, err)
			allIDs := make([]string, 0, len(all))
			for _, vk := range all {
				allIDs = append(allIDs, vk.ID)
			}
			require.Equal(t, wantIDs, allIDs)

			gov, err := ns.store.getGovernanceConfigVirtualKeys(ctx)
			require.NoError(t, err)
			govIDs := make([]string, 0, len(gov))
			for _, vk := range gov {
				govIDs = append(govIDs, vk.ID)
			}
			require.Equal(t, wantIDs, govIDs)
		})
	}
}

// TestHighFixVirtualKeyKeysetUsesCompositeIndex pins that on Postgres the keyset
// predicate is an index condition on idx_virtual_keys_created_at_id rather than a
// filter applied after reading from the start of the created_at index.
func TestHighFixVirtualKeyKeysetUsesCompositeIndex(t *testing.T) {
	var pg *RDBConfigStore
	for _, ns := range highFixStores(t) {
		if ns.name == "postgres" {
			pg = ns.store
		}
	}
	if pg == nil {
		t.Skip("postgres not available")
	}
	db := pg.DB()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 50; i++ {
		createTestVK(t, db, fmt.Sprintf("vk-%03d", i), base.Add(time.Duration(i/5)*time.Second))
	}
	require.NoError(t, db.Exec("ANALYZE governance_virtual_keys").Error)
	var plan []string
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL enable_seqscan = off").Error; err != nil {
			return err
		}
		stmt := tx.Session(&gorm.Session{DryRun: true}).Model(&tables.TableVirtualKey{}).
			Where(virtualKeyKeysetCondition, base, "vk-010").
			Order("governance_virtual_keys.created_at ASC, governance_virtual_keys.id ASC").Limit(3).Find(&[]tables.TableVirtualKey{}).Statement
		return tx.Raw("EXPLAIN "+stmt.SQL.String(), stmt.Vars...).Scan(&plan).Error
	}))
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "idx_virtual_keys_created_at_id", joined)
	require.Contains(t, joined, "Index Cond", joined)
}

// TestHighFixProviderJobTerminalClearsNextCheckAt pins H28: a job that reaches a
// terminal accounting status no longer carries next_check_at, a job released with
// an error keeps it, re-opening an unpriceable job makes it due again, and
// ListDueProviderJobs returns the same non-terminal jobs as before.
func TestHighFixProviderJobTerminalClearsNextCheckAt(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			s := ns.store
			db := s.DB()
			require.NoError(t, ensureProviderJobTable(t, db))
			due := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
			for _, id := range []string{"j-accounted", "j-unpriceable", "j-error", "j-pending"} {
				require.NoError(t, s.UpsertProviderJob(ctx, &tables.TableProviderJob{ID: id, Provider: "openai", JobID: id, NextCheckAt: &due}))
			}
			stale := time.Now().UTC().Add(-time.Hour)
			for _, id := range []string{"j-accounted", "j-unpriceable", "j-error"} {
				ok, err := s.ClaimProviderJob(ctx, id, "runner", stale, false)
				require.NoError(t, err)
				require.True(t, ok)
			}
			require.NoError(t, s.CompleteProviderJob(ctx, "j-accounted", "runner"))
			require.NoError(t, s.MarkProviderJobUnpriceable(ctx, "j-unpriceable", "runner", "max_poll_attempts", nil))
			require.NoError(t, s.FailProviderJob(ctx, "j-error", "runner", fmt.Errorf("boom")))

			nextOf := func(id string) *time.Time {
				job, err := s.GetProviderJob(ctx, id)
				require.NoError(t, err)
				return job.NextCheckAt
			}
			require.Nil(t, nextOf("j-accounted"))
			require.Nil(t, nextOf("j-unpriceable"))
			require.NotNil(t, nextOf("j-error"))
			require.NotNil(t, nextOf("j-pending"))

			jobs, err := s.ListDueProviderJobs(ctx, "", "", time.Now().UTC(), 10)
			require.NoError(t, err)
			ids := make([]string, 0, len(jobs))
			for _, j := range jobs {
				ids = append(ids, j.ID)
			}
			sort.Strings(ids)
			require.Equal(t, []string{"j-error", "j-pending"}, ids)

			// Settling an unpriceable job with results in hand re-opens it; if that
			// attempt fails the sweeper must still see it, as it did before the
			// terminal transition cleared next_check_at.
			ok, err := s.ClaimProviderJob(ctx, "j-unpriceable", "runner-2", stale, true)
			require.NoError(t, err)
			require.True(t, ok)
			require.NotNil(t, nextOf("j-unpriceable"))
			require.NoError(t, s.FailProviderJob(ctx, "j-unpriceable", "runner-2", fmt.Errorf("again")))
			jobs, err = s.ListDueProviderJobs(ctx, "", "", time.Now().UTC().Add(time.Second), 10)
			require.NoError(t, err)
			ids = ids[:0]
			for _, j := range jobs {
				ids = append(ids, j.ID)
			}
			sort.Strings(ids)
			require.Equal(t, []string{"j-error", "j-pending", "j-unpriceable"}, ids)
		})
	}
}

// ensureProviderJobTable creates batch_jobs for stores built without the full
// migration chain (the SQLite test store), matching the fresh-install shape.
func ensureProviderJobTable(t *testing.T, db *gorm.DB) error {
	t.Helper()
	if db.Migrator().HasTable(&tables.TableProviderJob{}) {
		return nil
	}
	return db.Migrator().CreateTable(&tables.TableProviderJob{})
}

// credRow is the comparable projection of one credential or flow row.
type credRow struct {
	Tbl    string
	ID     string
	Status string
}

// snapshotCredentialState returns every OAuth and header credential/flow row as
// (table, id, status), sorted, so two reconcile strategies can be compared.
func snapshotCredentialState(t *testing.T, db *gorm.DB) []credRow {
	t.Helper()
	var out []credRow
	for _, table := range []string{"mcp_oauth_tokens", "mcp_oauth_flows", "mcp_per_user_header_credentials", "mcp_per_user_header_flows"} {
		var rows []credRow
		require.NoError(t, db.Table(table).Select("? AS tbl, id, status", table).Scan(&rows).Error)
		out = append(out, rows...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tbl != out[j].Tbl {
			return out[i].Tbl < out[j].Tbl
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// seedReconcileFixture builds MCP clients, VKs, assignments and credential/flow
// rows covering every reconcile branch: allowed and disallowed clients, explicit
// and allow-on-all grants, VKs with no grants at all, orphaned rows that must
// come back, pending and non-pending flows, non-VK modes, and rows of other MCPs.
func seedReconcileFixture(t *testing.T, db *gorm.DB, everyone bool) {
	t.Helper()
	target := createTestMCPClient(t, db, "mcp-target", false)
	granted := createTestMCPClient(t, db, "mcp-granted", false)
	createTestMCPClient(t, db, "mcp-everyone", everyone)
	createTestMCPClient(t, db, "mcp-revoked", false)
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	for i := 0; i < 6; i++ {
		createTestVK(t, db, fmt.Sprintf("vk-r-%d", i), now)
	}
	// vk-r-0: target granted; vk-r-1: granted only; vk-r-2..: no explicit grants.
	require.NoError(t, db.Create(&tables.TableVirtualKeyMCPConfig{VirtualKeyID: "vk-r-0", MCPClientID: target.ID}).Error)
	require.NoError(t, db.Create(&tables.TableVirtualKeyMCPConfig{VirtualKeyID: "vk-r-1", MCPClientID: granted.ID}).Error)
	require.NoError(t, db.Create(&tables.TableVirtualKeyMCPConfig{VirtualKeyID: "vk-r-3", MCPClientID: granted.ID}).Error)

	strp := func(s string) *string { return &s }
	n := 0
	token := func(vk *string, client, mode, status string) {
		n++
		require.NoError(t, db.Create(&tables.TableMCPOauthToken{ID: fmt.Sprintf("tok-%02d", n), AuthMode: mode, MCPClientID: client, VirtualKeyID: vk,
			Status: status, AccessToken: "at", TokenType: "Bearer", CreatedAt: now, UpdatedAt: now}).Error)
	}
	flow := func(vk *string, client, mode, status string) {
		n++
		require.NoError(t, db.Create(&tables.TableMCPOauthFlow{ID: fmt.Sprintf("flow-%02d", n), MCPClientID: client, OauthConfigID: "oc", State: fmt.Sprintf("s-%d", n),
			VirtualKeyID: vk, FlowMode: mode, Status: status, ExpiresAt: exp, CreatedAt: now, UpdatedAt: now}).Error)
	}
	header := func(vk *string, client, mode, status string) {
		n++
		require.NoError(t, db.Create(&tables.TableMCPPerUserHeaderCredential{ID: fmt.Sprintf("hdr-%02d", n), VirtualKeyID: vk, MCPClientID: client, AuthMode: mode,
			Status: status, HeadersJSON: "{}", CreatedAt: now, UpdatedAt: now}).Error)
	}
	hflow := func(vk *string, client, mode, status string) {
		n++
		require.NoError(t, db.Create(&tables.TableMCPPerUserHeaderFlow{ID: fmt.Sprintf("hflow-%02d", n), MCPClientID: client, VirtualKeyID: vk,
			FlowMode: mode, Status: status, ExpiresAt: exp, CreatedAt: now, UpdatedAt: now}).Error)
	}
	statuses := []string{"active", "orphaned", "needs_reauth"}
	for i := 0; i < 6; i++ {
		vk := strp(fmt.Sprintf("vk-r-%d", i))
		for ci, client := range []string{"mcp-target", "mcp-granted", "mcp-everyone", "mcp-revoked", ""} {
			// One vk-mode credential per (VK, client): the partial unique indexes allow no more.
			token(vk, client, "vk", statuses[(i+ci)%len(statuses)])
			for _, status := range []string{"pending", "authorized", "expired"} {
				flow(vk, client, "vk", status)
			}
			token(vk, client, "user", "active")
			flow(vk, client, "user", "pending")
			// Header rows carry Postgres FKs to the client, so only real clients get them.
			if client == "" {
				continue
			}
			header(vk, client, "vk", statuses[(i+ci+1)%len(statuses)])
			for _, status := range []string{"pending", "completed", "expired"} {
				hflow(vk, client, "vk", status)
			}
			header(vk, client, "user", "active")
			hflow(vk, client, "user", "pending")
		}
	}
	// NULL client ids: kept by a VK with grants (NOT IN is unknown for NULL), and
	// orphaned by a VK whose allowlist is empty (no NOT IN filter at all).
	for _, vk := range []string{"vk-r-0", "vk-r-2"} {
		token(strp(vk), "placeholder-"+vk, "vk", "active")
		require.NoError(t, db.Exec("UPDATE mcp_oauth_tokens SET mcp_client_id = NULL WHERE mcp_client_id = ?", "placeholder-"+vk).Error)
	}
	// A VK that holds nothing for the target MCP must not be reconciled at all.
	createTestVK(t, db, "vk-untouched", now)
	token(strp("vk-untouched"), "mcp-revoked", "vk", "active")
	header(strp("vk-untouched"), "mcp-revoked", "vk", "active")
	// Rows with no VK id.
	token(nil, "mcp-target", "vk", "active")
	header(nil, "mcp-target", "vk", "active")
}

// legacyReconcileAfterMCPChange is the per-VK loop the set-based rewrite replaced,
// kept verbatim as the reference the new statements are compared against.
func legacyReconcileAfterMCPChange(t *testing.T, db *gorm.DB, mcpClientID string, headers bool) {
	t.Helper()
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		read, reconcile := readVKsHoldingOauthCredsForMCP, reconcileVKDirectTokensDB
		if headers {
			read, reconcile = readVKsHoldingHeaderCredsForMCP, reconcileVKDirectHeaderRowsDB
		}
		vkIDs, err := read(tx, mcpClientID)
		if err != nil {
			return err
		}
		sort.Strings(vkIDs)
		for _, vkID := range vkIDs {
			if err := reconcile(tx, vkID); err != nil {
				return err
			}
		}
		return nil
	}))
}

// TestHighFixReconcileAfterMCPChangeMatchesLegacy pins H12: the set-based
// ReconcileOauthAfterMCPChange / ReconcileMCPHeadersAfterMCPChange leave exactly
// the same rows, with the same statuses, as the per-VK loop they replaced, and
// issue a constant number of statements.
func TestHighFixReconcileAfterMCPChangeMatchesLegacy(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.store.DB()
			for _, variant := range []struct {
				name     string
				clientID string
				everyone bool
			}{
				{"target", "mcp-target", true}, {"target-no-allow-all", "mcp-target", false},
				{"revoked", "mcp-revoked", true}, {"revoked-no-allow-all", "mcp-revoked", false},
				{"everyone", "mcp-everyone", true}, {"granted", "mcp-granted", false},
			} {
				clientID := variant.clientID
				t.Run(variant.name, func(t *testing.T) {
					resetReconcileTables(t, db)
					seedReconcileFixture(t, db, variant.everyone)
					before := snapshotCredentialState(t, db)
					legacyReconcileAfterMCPChange(t, db, clientID, false)
					legacyReconcileAfterMCPChange(t, db, clientID, true)
					want := snapshotCredentialState(t, db)
					require.NotEqual(t, before, want, "fixture must exercise at least one change")

					resetReconcileTables(t, db)
					seedReconcileFixture(t, db, variant.everyone)
					runSetBasedReconcile(t, ctx, ns.store, clientID)
					require.Equal(t, want, snapshotCredentialState(t, db))
				})
			}
		})
	}
}

// runSetBasedReconcile runs both MCP-change reconciles through the store and
// asserts the statement count does not grow with the number of VKs.
func runSetBasedReconcile(t *testing.T, ctx context.Context, s *RDBConfigStore, clientID string) {
	t.Helper()
	stmts := countStatements(t, s.DB(), func(sql string) bool {
		return strings.Contains(sql, "MCP_OAUTH_") || strings.Contains(sql, "MCP_PER_USER_HEADER_")
	})
	require.NoError(t, s.ReconcileOauthAfterMCPChange(ctx, clientID))
	require.NoError(t, s.ReconcileMCPHeadersAfterMCPChange(ctx, clientID))
	require.LessOrEqual(t, stmts.count(), int64(14), "reconcile must be set-based, not ~5 statements per VK")
}

// resetReconcileTables empties the tables seedReconcileFixture writes.
func resetReconcileTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{"mcp_oauth_tokens", "mcp_oauth_flows", "mcp_per_user_header_credentials", "mcp_per_user_header_flows",
		"governance_virtual_key_mcp_configs", "governance_virtual_keys", "config_mcp_clients"} {
		require.NoError(t, db.Exec("DELETE FROM "+table).Error)
	}
}

// TestHighFixGetRedactedVirtualKeysAppliesQueryScope pins that a QueryScope on ctx
// restricts GetRedactedVirtualKeys on both the id-list and the list-all paths, as
// it does for GetVirtualKey, so callers that resolve VK names through it cannot see
// keys outside the caller's scope.
func TestHighFixGetRedactedVirtualKeysAppliesQueryScope(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			db := ns.store.DB()
			now := time.Now().UTC()
			createTestVK(t, db, "vk-visible", now)
			createTestVK(t, db, "vk-hidden", now)
			ctx := queryscope.WithQueryScope(context.Background(), func(db *gorm.DB) *gorm.DB {
				return db.Where("governance_virtual_keys.id = ?", "vk-visible")
			})

			byID, err := ns.store.GetRedactedVirtualKeys(ctx, []string{"vk-visible", "vk-hidden"})
			require.NoError(t, err)
			require.Len(t, byID, 1)
			require.Equal(t, "vk-visible", byID[0].ID)

			all, err := ns.store.GetRedactedVirtualKeys(ctx, nil)
			require.NoError(t, err)
			require.Len(t, all, 1)
			require.Equal(t, "vk-visible", all[0].ID)

			unscoped, err := ns.store.GetRedactedVirtualKeys(context.Background(), nil)
			require.NoError(t, err)
			require.Len(t, unscoped, 2)
		})
	}
}

// TestGetRedactedTeamsAndCustomers pins the batch name lookups rankings use to
// show each team's and customer's current name: exactly the listed ids come
// back with their names, an unknown id is simply absent, and an empty list
// returns nothing rather than every row.
func TestGetRedactedTeamsAndCustomers(t *testing.T) {
	for _, ns := range highFixStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.store.DB()
			require.NoError(t, db.Create(&tables.TableCustomer{ID: "cust-1", Name: "Acme"}).Error)
			require.NoError(t, db.Create(&tables.TableCustomer{ID: "cust-2", Name: "Globex"}).Error)
			require.NoError(t, db.Create(&tables.TableTeam{ID: "team-1", Name: "Platform"}).Error)
			require.NoError(t, db.Create(&tables.TableTeam{ID: "team-2", Name: "Search"}).Error)

			teams, err := ns.store.GetRedactedTeams(ctx, []string{"team-2", "team-missing"})
			require.NoError(t, err)
			require.Len(t, teams, 1)
			require.Equal(t, "team-2", teams[0].ID)
			require.Equal(t, "Search", teams[0].Name)

			customers, err := ns.store.GetRedactedCustomers(ctx, []string{"cust-1"})
			require.NoError(t, err)
			require.Len(t, customers, 1)
			require.Equal(t, "Acme", customers[0].Name)

			none, err := ns.store.GetRedactedTeams(ctx, nil)
			require.NoError(t, err)
			require.Empty(t, none, "an empty id list must not return every team")
			noCustomers, err := ns.store.GetRedactedCustomers(ctx, nil)
			require.NoError(t, err)
			require.Empty(t, noCustomers, "an empty id list must not return every customer")
		})
	}
}

// TestHighFixReconcileLockOrderMatchesPerVKPath pins that the bulk MCP-change
// reconcile takes row locks in the same order as the per-VK path (active rows,
// then orphaned rows), so concurrent MCP and VK edits cannot deadlock. The test
// forces the bad interleave: the per-VK transaction holds the active row while
// the bulk reconcile runs, then reaches for the orphaned row the bulk reconcile
// would already hold if it locked in (virtual_key_id, id) order.
func TestHighFixReconcileLockOrderMatchesPerVKPath(t *testing.T) {
	var pg *RDBConfigStore
	for _, ns := range highFixStores(t) {
		if ns.name == "postgres" {
			pg = ns.store
		}
	}
	if pg == nil {
		t.Skip("Postgres not available, skipping lock-order test")
	}
	ctx := context.Background()
	db := pg.DB()
	resetReconcileTables(t, db)
	t.Cleanup(func() { resetReconcileTables(t, db) })

	createTestMCPClient(t, db, "mcp-granted", false)
	granted := createTestMCPClient(t, db, "mcp-granted-2", false)
	createTestMCPClient(t, db, "mcp-revoked", false)
	now := time.Now().UTC()
	createTestVK(t, db, "vk-lock", now)
	require.NoError(t, db.Create(&tables.TableVirtualKeyMCPConfig{VirtualKeyID: "vk-lock", MCPClientID: granted.ID}).Error)
	vk := "vk-lock"
	// The orphaned row sorts first by id, so a (virtual_key_id, id) ordered lock
	// reaches it before the active row.
	for _, row := range []struct{ id, client, status string }{
		{"tok-a-orphaned", "mcp-granted-2", "orphaned"},
		{"tok-b-active", "mcp-revoked", "active"},
	} {
		require.NoError(t, db.Create(&tables.TableMCPOauthToken{ID: row.id, AuthMode: "vk", MCPClientID: row.client, VirtualKeyID: &vk,
			Status: row.status, AccessToken: "at", TokenType: "Bearer", CreatedAt: now, UpdatedAt: now}).Error)
	}

	perVK := db.Begin()
	require.NoError(t, perVK.Error)
	defer perVK.Rollback()
	// The per-VK path's first step locks the active row.
	require.NoError(t, perVK.Exec("UPDATE mcp_oauth_tokens SET status = status WHERE id = ?", "tok-b-active").Error)

	bulkDone := make(chan error, 1)
	go func() { bulkDone <- pg.ReconcileOauthAfterMCPChange(ctx, "mcp-granted-2") }()

	require.Eventually(t, func() bool {
		var waiting int64
		require.NoError(t, db.Raw(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%mcp_oauth_tokens%'`).Scan(&waiting).Error)
		return waiting > 0
	}, 10*time.Second, 50*time.Millisecond, "the bulk reconcile should block on the row the per-VK transaction holds")

	require.NoError(t, reconcileVKDirectTokensDB(perVK, "vk-lock"), "per-VK reconcile must not be the deadlock victim")
	require.NoError(t, perVK.Commit().Error)
	select {
	case err := <-bulkDone:
		require.NoError(t, err, "bulk reconcile must not be the deadlock victim")
	case <-time.After(15 * time.Second):
		t.Fatal("bulk reconcile did not finish")
	}

	var statuses []string
	require.NoError(t, db.Model(&tables.TableMCPOauthToken{}).Order("id").Pluck("status", &statuses).Error)
	require.Equal(t, []string{"active", "orphaned"}, statuses, "the orphaned grant comes back and the revoked one is orphaned")
}

// inListLimitsPGSchema isolates these tests from the other Postgres-backed
// tests in this package, which drop and recreate shared schemas.
const inListLimitsPGSchema = "configstore_inlist_test"

// inListOverLimit is larger than both Postgres's 65,535 bind-parameter limit and
// SQLite's 32,766, so a query that binds one parameter per id fails on either.
const inListOverLimit = 70000

// inListNamedStore pairs a migrated RDBConfigStore with the dialect it runs on.
type inListNamedStore struct {
	name  string
	store *RDBConfigStore
	db    *gorm.DB
}

// inListLimitStores returns a fully migrated store on a dedicated Postgres
// schema, when the local test Postgres is reachable, and one on a fresh SQLite
// file.
func inListLimitStores(t *testing.T) []inListNamedStore {
	t.Helper()
	ctx := context.Background()
	var stores []inListNamedStore

	dsn := strings.Replace(postgresDSN, "search_path="+pgTestSchema, "search_path="+inListLimitsPGSchema, 1)
	if pgDB, err := gorm.Open(postgres.Open(dsn), inListGormConfig()); err == nil {
		if sqlDB, err := pgDB.DB(); err == nil && sqlDB.Ping() == nil {
			// Drop the schema afterwards: postgresIndexIsValid is not schema-scoped,
			// so indexes left here would satisfy other tests' existence checks.
			t.Cleanup(func() {
				_ = pgDB.Exec("DROP SCHEMA IF EXISTS " + inListLimitsPGSchema + " CASCADE").Error
				_ = sqlDB.Close()
			})
			require.NoError(t, pgDB.Exec("DROP SCHEMA IF EXISTS "+inListLimitsPGSchema+" CASCADE").Error)
			require.NoError(t, pgDB.Exec("CREATE SCHEMA "+inListLimitsPGSchema).Error)
			require.NoError(t, triggerMigrations(ctx, pgDB, testMigrationLogger))
			stores = append(stores, inListNamedStore{name: "postgres", store: newInListTestStore(pgDB), db: pgDB})
		}
	}

	sqliteDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inlist.db")), inListGormConfig())
	require.NoError(t, err)
	require.NoError(t, triggerMigrations(ctx, sqliteDB, testMigrationLogger))
	return append(stores, inListNamedStore{name: "sqlite", store: newInListTestStore(sqliteDB), db: sqliteDB})
}

// inListGormConfig returns a fresh silent GORM config. Each connection needs
// its own: gorm.Open writes the dialector into the config it is given, so a
// shared config would switch an earlier Postgres handle to SQLite quoting.
func inListGormConfig() *gorm.Config {
	return &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
}

// newInListTestStore wraps db in an RDBConfigStore with no-op pool hooks.
func newInListTestStore(db *gorm.DB) *RDBConfigStore {
	s := &RDBConfigStore{logger: newMockLogger()}
	s.db.Store(db)
	s.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, s.DB())
	}
	s.refreshPoolFn = func(ctx context.Context) error { return nil }
	return s
}

// inListSeedSeries runs "INSERT ... SELECT ... FROM n" with n bound to the integers
// 1..count through a recursive CTE, which both SQLite and Postgres accept, so
// tens of thousands of rows are seeded in one statement without bind parameters.
func inListSeedSeries(t *testing.T, db *gorm.DB, count int, insert string) {
	t.Helper()
	sql := fmt.Sprintf("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d) %s", count, insert)
	require.NoError(t, db.Exec(sql).Error)
}

// inListSyntheticIDs returns count ids of the form prefix-000001 that match no row.
func inListSyntheticIDs(prefix string, count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%06d", prefix, i+1)
	}
	return ids
}

// inListCountRows returns the row count of table matching where.
func inListCountRows(t *testing.T, db *gorm.DB, table, where string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Table(table).Where(where, args...).Count(&n).Error)
	return n
}

// TestInListLimitDeleteModelConfigsWhere pins that tearing down a provider with
// more model configs than the bind-parameter limit succeeds and still removes
// every owned budget (multi-budget and legacy) and rate limit, while leaving
// another provider's configs untouched.
func TestInListLimitDeleteModelConfigsWhere(t *testing.T) {
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.db
			inListSeedSeries(t, db, inListOverLimit, `INSERT INTO governance_model_configs
				(id, model_name, provider, scope, calendar_aligned, config_hash, created_at, updated_at)
				SELECT 'mc-' || i, 'model-' || i, 'openai', 'global', false, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM n`)

			now := time.Now()
			mc1, mc2, mc3 := "mc-1", "mc-2", "mc-3"
			for _, b := range []tables.TableBudget{
				{ID: "b-multi-1", MaxLimit: 1, ResetDuration: "1d", LastReset: now, ModelConfigID: &mc1},
				{ID: "b-multi-2", MaxLimit: 1, ResetDuration: "1w", LastReset: now, ModelConfigID: &mc1},
				{ID: "b-legacy", MaxLimit: 1, ResetDuration: "1d", LastReset: now},
				{ID: "b-keep", MaxLimit: 1, ResetDuration: "1d", LastReset: now},
			} {
				require.NoError(t, db.Create(&b).Error)
			}
			require.NoError(t, db.Create(&tables.TableRateLimit{ID: "rl-1", TokenLastReset: now, RequestLastReset: now}).Error)
			require.NoError(t, db.Create(&tables.TableRateLimit{ID: "rl-keep", TokenLastReset: now, RequestLastReset: now}).Error)
			require.NoError(t, db.Exec("UPDATE governance_model_configs SET budget_id = ? WHERE id = ?", "b-legacy", mc2).Error)
			require.NoError(t, db.Exec("UPDATE governance_model_configs SET rate_limit_id = ? WHERE id = ?", "rl-1", mc3).Error)

			otherProvider := "anthropic"
			keepBudgetID, keepRateLimitID := "b-keep", "rl-keep"
			require.NoError(t, db.Create(&tables.TableModelConfig{
				ID: "mc-keep", ModelName: "model-keep", Provider: &otherProvider, Scope: "global",
				BudgetID: &keepBudgetID, RateLimitID: &keepRateLimitID,
			}).Error)

			err := db.Transaction(func(tx *gorm.DB) error {
				return ns.store.deleteModelConfigsWhere(ctx, tx, "provider = ?", "openai")
			})
			require.NoError(t, err)

			require.Zero(t, inListCountRows(t, db, "governance_model_configs", "provider = ?", "openai"))
			require.Equal(t, int64(1), inListCountRows(t, db, "governance_model_configs", "id = ?", "mc-keep"))
			require.Zero(t, inListCountRows(t, db, "governance_budgets", "id IN ?", []string{"b-multi-1", "b-multi-2", "b-legacy"}))
			require.Equal(t, int64(1), inListCountRows(t, db, "governance_budgets", "id = ?", "b-keep"))
			require.Zero(t, inListCountRows(t, db, "governance_rate_limits", "id = ?", "rl-1"))
			require.Equal(t, int64(1), inListCountRows(t, db, "governance_rate_limits", "id = ?", "rl-keep"))
		})
	}
}

// TestInListLimitMCPSessionFilters pins that the MCP sessions list accepts
// search-resolved, user-filter and virtual-key-filter id lists larger than the
// bind-parameter limit and still matches the one real row inside them.
func TestInListLimitMCPSessionFilters(t *testing.T) {
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			db := ns.db
			client := tables.TableMCPClient{ClientID: "client-inlist", Name: "inlist-client", ConnectionType: "http", AuthType: "per_user_oauth"}
			require.NoError(t, db.Create(&client).Error)
			userID := "user-real"
			require.NoError(t, db.Create(&tables.TableMCPOauthToken{
				ID: "tok-1", AuthMode: "user", MCPClientID: client.ClientID, OauthConfigID: "cfg-1",
				UserID: &userID, Status: "active", AccessToken: "x", TokenType: "Bearer", CreatedAt: time.Now(),
			}).Error)

			ids := append(inListSyntheticIDs("user", inListOverLimit), userID)

			tokens, err := ns.store.ListOauthUserTokens(ctx, MCPSessionsFilterParams{Search: "no-such-needle", MatchedUserIDs: ids})
			require.NoError(t, err)
			require.Len(t, tokens, 1)
			require.Equal(t, "tok-1", tokens[0].ID)

			tokens, err = ns.store.ListOauthUserTokens(ctx, MCPSessionsFilterParams{UserIDs: ids})
			require.NoError(t, err)
			require.Len(t, tokens, 1)

			tokens, err = ns.store.ListOauthUserTokens(ctx, MCPSessionsFilterParams{VirtualKeyIDs: inListSyntheticIDs("vk", inListOverLimit)})
			require.NoError(t, err)
			require.Empty(t, tokens)
		})
	}
}

// inListSeedVirtualKeys inserts count virtual keys with ids vk-1000001.. sharing one
// created_at, so the default (created_at, id) order is the id order.
func inListSeedVirtualKeys(t *testing.T, db *gorm.DB, count int) {
	t.Helper()
	inListSeedSeries(t, db, count, `INSERT INTO governance_virtual_keys
		(id, name, description, value, is_active, calendar_aligned, allow_all_providers, created_at, updated_at)
		SELECT 'vk-' || (1000000 + i), 'vk-name-' || i, '', 'sk-bf-' || i, true, false, false, '2026-01-01 00:00:00', '2026-01-01 00:00:00' FROM n`)
}

// TestInListLimitGetRedactedVirtualKeys pins that looking up redacted virtual
// keys by an id list larger than the bind-parameter limit succeeds and returns
// only the keys in that list.
func TestInListLimitGetRedactedVirtualKeys(t *testing.T) {
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			inListSeedVirtualKeys(t, ns.db, 3)
			ids := append(inListSyntheticIDs("missing", inListOverLimit), "vk-1000001", "vk-1000003")
			vks, err := ns.store.GetRedactedVirtualKeys(context.Background(), ids)
			require.NoError(t, err)
			got := make([]string, 0, len(vks))
			for _, vk := range vks {
				got = append(got, vk.ID)
			}
			sort.Strings(got)
			require.Equal(t, []string{"vk-1000001", "vk-1000003"}, got)
		})
	}
}

// TestInListLimitGetVirtualKeysPaginatedExport pins that an export page of
// 10,000 virtual keys whose provider configs outnumber the bind-parameter limit
// loads successfully, in the same order and with the same total as before, and
// that an offset export window returns exactly the matching slice.
func TestInListLimitGetVirtualKeysPaginatedExport(t *testing.T) {
	const vkCount = 10000
	const configsPerVK = 7
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			inListSeedVirtualKeys(t, ns.db, vkCount)
			inListSeedSeries(t, ns.db, configsPerVK, `INSERT INTO governance_virtual_key_provider_configs
				(virtual_key_id, provider, allow_all_keys)
				SELECT vk.id, 'provider-' || n.i, false FROM governance_virtual_keys vk CROSS JOIN n`)
			expected := make([]string, vkCount)
			for i := range expected {
				expected[i] = fmt.Sprintf("vk-%d", 1000000+i+1)
			}

			vks, total, err := ns.store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Export: true})
			require.NoError(t, err)
			require.Equal(t, int64(vkCount), total)
			require.Len(t, vks, vkCount)
			for i, vk := range vks {
				require.Equal(t, expected[i], vk.ID)
				require.Len(t, vk.ProviderConfigs, configsPerVK)
			}

			vks, total, err = ns.store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Export: true, Offset: 500, Limit: 2500})
			require.NoError(t, err)
			require.Equal(t, int64(vkCount), total)
			require.Len(t, vks, 2500)
			for i, vk := range vks {
				require.Equal(t, expected[500+i], vk.ID)
			}

			vks, _, err = ns.store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Export: true, Offset: 9500, SortBy: "name", Order: "desc"})
			require.NoError(t, err)
			require.Len(t, vks, 500)

			vks, _, err = ns.store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Offset: 10, Limit: 5})
			require.NoError(t, err)
			require.Len(t, vks, 5)
			require.Equal(t, expected[10], vks[0].ID)
		})
	}
}

// TestInListLimitGetVirtualKeysPaginatedExportSnapshot pins that a chunked export
// reads every chunk from one snapshot. A virtual key created between chunks that
// sorts ahead of the current offset must not shift a row from the first chunk into
// the second (a duplicate), and the total must match the rows returned.
func TestInListLimitGetVirtualKeysPaginatedExportSnapshot(t *testing.T) {
	const vkCount = virtualKeyInternalPageSize + 10
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			if ns.name == "sqlite" {
				// Production runs SQLite in WAL mode, where a writer can commit while a
				// read transaction holds its snapshot.
				require.NoError(t, ns.db.Exec("PRAGMA journal_mode=WAL").Error)
			}
			inListSeedVirtualKeys(t, ns.db, vkCount)

			// After the first chunk's Find, create a key that sorts first, from a
			// connection outside any export transaction.
			var inserted bool
			callbackName := "test:insert-vk-between-export-chunks"
			require.NoError(t, ns.db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				if inserted || tx.Error != nil || tx.Statement.Table != "governance_virtual_keys" {
					return
				}
				if _, ok := tx.Statement.Dest.(*[]tables.TableVirtualKey); !ok {
					return
				}
				inserted = true
				require.NoError(t, ns.db.Session(&gorm.Session{NewDB: true}).Exec(`INSERT INTO governance_virtual_keys
					(id, name, description, value, is_active, calendar_aligned, allow_all_providers, created_at, updated_at)
					VALUES ('vk-0000000', 'vk-name-early', '', 'sk-bf-early', true, false, false, '2025-01-01 00:00:00', '2025-01-01 00:00:00')`).Error)
			}))
			t.Cleanup(func() { _ = ns.db.Callback().Query().Remove(callbackName) })

			vks, total, err := ns.store.GetVirtualKeysPaginated(ctx, VirtualKeyQueryParams{Export: true})
			require.NoError(t, err)
			require.True(t, inserted, "the mid-export insert never ran")

			seen := make(map[string]bool, len(vks))
			for _, vk := range vks {
				require.False(t, seen[vk.ID], "export returned %s twice", vk.ID)
				seen[vk.ID] = true
			}
			require.False(t, seen["vk-0000000"], "export mixed in a key created after it started")
			require.Len(t, vks, vkCount)
			require.Equal(t, int64(len(vks)), total)
		})
	}
}

// TestGetVirtualKeysByIDsMatchesGetVirtualKey pins that the batched load used to
// reload many keys at once returns, for every id, exactly what GetVirtualKey
// returns (same relations), across a chunk boundary; that a missing id is
// absent; and that an empty id list returns nothing.
func TestGetVirtualKeysByIDsMatchesGetVirtualKey(t *testing.T) {
	const vkCount = virtualKeyInternalPageSize + 5
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			inListSeedVirtualKeys(t, ns.db, vkCount)
			inListSeedSeries(t, ns.db, 2, `INSERT INTO governance_virtual_key_provider_configs
				(virtual_key_id, provider, allow_all_keys)
				SELECT vk.id, 'provider-' || n.i, false FROM governance_virtual_keys vk CROSS JOIN n`)
			ids := make([]string, 0, vkCount+1)
			for i := 1; i <= vkCount; i++ {
				ids = append(ids, fmt.Sprintf("vk-%d", 1000000+i))
			}
			ids = append(ids, "vk-missing")

			got, err := ns.store.GetVirtualKeysByIDs(ctx, ids)
			require.NoError(t, err)
			require.Len(t, got, vkCount)
			byID := make(map[string]tables.TableVirtualKey, len(got))
			for _, vk := range got {
				byID[vk.ID] = vk
			}
			for _, id := range []string{"vk-1000001", "vk-1000500", fmt.Sprintf("vk-%d", 1000000+vkCount)} {
				want, err := ns.store.GetVirtualKey(ctx, id)
				require.NoError(t, err)
				wantJSON, err := json.Marshal(want)
				require.NoError(t, err)
				have := byID[id]
				gotJSON, err := json.Marshal(&have)
				require.NoError(t, err)
				require.JSONEq(t, string(wantJSON), string(gotJSON), "batched load of %s must match GetVirtualKey", id)
			}

			none, err := ns.store.GetVirtualKeysByIDs(ctx, nil)
			require.NoError(t, err)
			require.Empty(t, none)
		})
	}
}

// TestUpsertModelPricesBatch_Above100kColumns_SurviveResync guards the
// pricingSyncUpdateColumns entries for the >100k tier columns.
func TestUpsertModelPricesBatch_Above100kColumns_SurviveResync(t *testing.T) {
	s := setupRDBTestStore(t)
	require.NoError(t, s.DB().AutoMigrate(&tables.TableModelPricing{}))

	ctx := context.Background()
	pricing := []tables.TableModelPricing{{
		Model: "claude-haiku-5-5", Provider: "anthropic", Mode: "chat",
		InputCostPerToken:                                  new(1e-07),
		InputCostPerTokenAbove100kTokens:                   new(4e-07),
		OutputCostPerTokenAbove100kTokens:                  new(2e-06),
		CacheCreationInputTokenCostAbove100kTokens:         new(5e-07),
		CacheReadInputTokenCostAbove100kTokens:             new(4e-08),
		CacheCreationInputTokenCostAbove1hrAbove100kTokens: new(8e-07),
	}}
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	pricing[0].InputCostPerTokenAbove100kTokens = new(5e-07)
	pricing[0].OutputCostPerTokenAbove100kTokens = new(2.5e-06)
	pricing[0].CacheCreationInputTokenCostAbove100kTokens = new(6.25e-07)
	pricing[0].CacheReadInputTokenCostAbove100kTokens = new(5e-08)
	pricing[0].CacheCreationInputTokenCostAbove1hrAbove100kTokens = new(1e-06)
	require.NoError(t, s.UpsertModelPricesBatch(ctx, pricing))

	got, err := s.GetModelPrices(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].InputCostPerTokenAbove100kTokens)
	assert.InDelta(t, 5e-07, *got[0].InputCostPerTokenAbove100kTokens, 1e-15)
	require.NotNil(t, got[0].OutputCostPerTokenAbove100kTokens)
	assert.InDelta(t, 2.5e-06, *got[0].OutputCostPerTokenAbove100kTokens, 1e-15)
	require.NotNil(t, got[0].CacheCreationInputTokenCostAbove100kTokens)
	assert.InDelta(t, 6.25e-07, *got[0].CacheCreationInputTokenCostAbove100kTokens, 1e-15)
	require.NotNil(t, got[0].CacheReadInputTokenCostAbove100kTokens)
	assert.InDelta(t, 5e-08, *got[0].CacheReadInputTokenCostAbove100kTokens, 1e-15)
	require.NotNil(t, got[0].CacheCreationInputTokenCostAbove1hrAbove100kTokens)
	assert.InDelta(t, 1e-06, *got[0].CacheCreationInputTokenCostAbove1hrAbove100kTokens, 1e-15)
}

// TestProviderInjectedToolsRoundTrip pins injected_tools through every provider write
// path (bulk upsert, add, update) and both read paths, including clearing it.
func TestProviderInjectedToolsRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	webSearch := []schemas.InjectedTool{
		{Name: "web_search", MCPClientName: "tavily", ToolName: "search"},
		{Name: "read_page", MCPClientName: "fetcher", ToolName: "fetch"},
	}

	require.NoError(t, store.UpdateProvidersConfig(ctx, map[schemas.ModelProvider]ProviderConfig{
		schemas.OpenAI: {InjectedTools: webSearch},
	}))
	all, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, webSearch, all[schemas.OpenAI].InjectedTools)

	require.NoError(t, store.AddProvider(ctx, schemas.Anthropic, ProviderConfig{InjectedTools: webSearch}))
	got, err := store.GetProviderConfig(ctx, schemas.Anthropic)
	require.NoError(t, err)
	assert.Equal(t, webSearch, got.InjectedTools)

	require.NoError(t, store.UpdateProvider(ctx, schemas.Anthropic, ProviderConfig{}))
	got, err = store.GetProviderConfig(ctx, schemas.Anthropic)
	require.NoError(t, err)
	assert.Nil(t, got.InjectedTools, "updating with a nil block must clear the stored column")

	require.NoError(t, store.UpdateProvider(ctx, schemas.Anthropic, ProviderConfig{InjectedTools: webSearch}))
	require.NoError(t, store.UpdateProvider(ctx, schemas.Anthropic, ProviderConfig{InjectedTools: []schemas.InjectedTool{}}))
	got, err = store.GetProviderConfig(ctx, schemas.Anthropic)
	require.NoError(t, err)
	assert.Empty(t, got.InjectedTools, "an empty list clears the stored column too")
}

func TestUpdateClientConfig_CompatConvertDecisionToResponsesRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()

	base := func(enabled bool) *ClientConfig {
		return &ClientConfig{
			EnableLogging:        new(true),
			InitialPoolSize:      100,
			LogRetentionDays:     30,
			MaxRequestBodySizeMB: 50,
			Compat:               CompatConfig{ConvertDecisionToResponses: enabled},
		}
	}

	require.NoError(t, store.UpdateClientConfig(ctx, base(true)))
	result, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.True(t, result.Compat.ConvertDecisionToResponses, "enabling the toggle must persist")

	require.NoError(t, store.UpdateClientConfig(ctx, base(false)))
	result, err = store.GetClientConfig(ctx)
	require.NoError(t, err)
	assert.False(t, result.Compat.ConvertDecisionToResponses, "disabling the toggle must persist")
}

func TestGenerateClientConfigHash_CompatConvertDecisionToResponses(t *testing.T) {
	on := &ClientConfig{InitialPoolSize: 100, Compat: CompatConfig{ConvertDecisionToResponses: true}}
	off := &ClientConfig{InitialPoolSize: 100}
	onHash, err := on.GenerateClientConfigHash()
	require.NoError(t, err)
	offHash, err := off.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, onHash, offHash, "toggling convert_decision_to_responses must change the hash")
}

// TestClientConfigUnmarshal_CompatConvertDecisionToResponsesDefaultsOff pins that,
// unlike the other compat toggles, decision emulation stays off when config.json
// omits it - both when compat is absent and when it is present without the field.
func TestClientConfigUnmarshal_CompatConvertDecisionToResponsesDefaultsOff(t *testing.T) {
	for name, raw := range map[string]string{
		"compat absent":      `{}`,
		"field absent":       `{"compat": {"convert_text_to_chat": true}}`,
		"explicitly enabled": `{"compat": {"convert_decision_to_responses": true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var c ClientConfig
			require.NoError(t, json.Unmarshal([]byte(raw), &c))
			assert.Equal(t, name == "explicitly enabled", c.Compat.ConvertDecisionToResponses)
		})
	}
}

// TestA2ARawCaptureConfigRoundTrip verifies persistence and hash parity for raw capture.
func TestA2ARawCaptureConfigRoundTrip(t *testing.T) {
	store := setupRDBTestStore(t)
	ctx := context.Background()
	config := &ClientConfig{}
	require.NoError(t, store.UpdateClientConfig(ctx, config))
	config, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	config.A2AStoreRawRequestResponse = true
	require.NoError(t, store.UpdateClientConfig(ctx, config))
	found, err := store.GetClientConfig(ctx)
	require.NoError(t, err)
	require.True(t, found.A2AStoreRawRequestResponse)
	want, err := config.GenerateClientConfigHash()
	require.NoError(t, err)
	got, err := found.GenerateClientConfigHash()
	require.NoError(t, err)
	require.Equal(t, want, got)
	config.A2AStoreRawRequestResponse = false
	other, err := config.GenerateClientConfigHash()
	require.NoError(t, err)
	require.NotEqual(t, want, other)
}

// TestInListLimitProviderObjectsAndJobsByIDs pins that the ownership lookups a
// file, batch or video list page runs accept id lists larger than the
// bind-parameter limit: a 10,000-item page fanned out across every provider
// name that shares the provider's API reaches it. Only the real rows inside the
// list come back.
func TestInListLimitProviderObjectsAndJobsByIDs(t *testing.T) {
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			objectID := tables.ProviderObjectID(tables.ProviderObjectKindFile, "openai", "file-real")
			require.NoError(t, ns.store.UpsertProviderObject(ctx, &tables.TableProviderObject{
				ID: objectID, Kind: tables.ProviderObjectKindFile, Provider: "openai", ObjectID: "file-real", VirtualKeyID: "vk-a",
			}))
			out := "file-out"
			jobID := tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "batch-real")
			require.NoError(t, ns.store.UpsertProviderJob(ctx, &tables.TableProviderJob{
				ID: jobID, Provider: "openai", JobID: "batch-real", InputFileID: "file-in", OutputFileID: &out,
				AccountingStatus: tables.ProviderJobAccountingStatusPending,
			}))

			t.Run("GetProviderObjectsByIDs", func(t *testing.T) {
				objects, err := ns.store.GetProviderObjectsByIDs(ctx, append(inListSyntheticIDs("file:openai:missing", inListOverLimit), objectID))
				require.NoError(t, err)
				require.Len(t, objects, 1)
				require.Equal(t, objectID, objects[0].ID)
			})
			t.Run("GetProviderJobsByIDs", func(t *testing.T) {
				jobs, err := ns.store.GetProviderJobsByIDs(ctx, append(inListSyntheticIDs("batch:openai:missing", inListOverLimit), jobID))
				require.NoError(t, err)
				require.Len(t, jobs, 1)
				require.Equal(t, jobID, jobs[0].ID)
			})
			// Three columns share one id list, so a third of the limit is already too many.
			t.Run("GetProviderJobsByFileIDs", func(t *testing.T) {
				jobs, err := ns.store.GetProviderJobsByFileIDs(ctx, "openai", append(inListSyntheticIDs("file-missing", inListOverLimit/3), out))
				require.NoError(t, err)
				require.Len(t, jobs, 1)
				require.Equal(t, jobID, jobs[0].ID)
			})
		})
	}
}

// TestInListLimitProviderObjectsAndJobsBySuffixes pins that the short-id lookups a
// Vertex list page runs accept more suffixes than one statement can hold: every
// suffix is its own LIKE term, and SQLite caps both the bound parameters and the
// depth of the OR chain they form. Only the full names ending in a listed suffix
// come back.
func TestInListLimitProviderObjectsAndJobsBySuffixes(t *testing.T) {
	const suffixCount = 10000
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			ctx := context.Background()
			fullName := "projects/p/locations/l/batchPredictionJobs/real"
			require.NoError(t, ns.store.UpsertProviderObject(ctx, &tables.TableProviderObject{
				ID: tables.ProviderObjectID(tables.ProviderObjectKindBatch, "vertex", fullName), Kind: tables.ProviderObjectKindBatch,
				Provider: "vertex", ObjectID: fullName, VirtualKeyID: "vk-a",
			}))
			require.NoError(t, ns.store.UpsertProviderJob(ctx, &tables.TableProviderJob{
				ID: tables.ProviderJobID(tables.ProviderJobKindBatch, "vertex", fullName), Kind: tables.ProviderJobKindBatch,
				Provider: "vertex", JobID: fullName, AccountingStatus: tables.ProviderJobAccountingStatusPending,
			}))
			suffixes := append(inListSyntheticIDs("missing", suffixCount), "real")

			t.Run("GetProviderObjectsByObjectIDSuffixes", func(t *testing.T) {
				objects, err := ns.store.GetProviderObjectsByObjectIDSuffixes(ctx, tables.ProviderObjectKindBatch, "vertex", suffixes)
				require.NoError(t, err)
				require.Len(t, objects, 1)
				require.Equal(t, fullName, objects[0].ObjectID)
			})
			t.Run("GetProviderJobsByJobIDSuffixes", func(t *testing.T) {
				jobs, err := ns.store.GetProviderJobsByJobIDSuffixes(ctx, tables.ProviderJobKindBatch, "vertex", suffixes)
				require.NoError(t, err)
				require.Len(t, jobs, 1)
				require.Equal(t, fullName, jobs[0].JobID)
			})
		})
	}
}

// TestGenerateClientConfigHash_SkillsExternalClientURL keeps unset hashes stable and distinguishes configured URLs.
func TestGenerateClientConfigHash_SkillsExternalClientURL(t *testing.T) {
	base := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30}
	baseHash, err := base.GenerateClientConfigHash()
	require.NoError(t, err)

	// An explicit empty URL must differ from omission so file reconciliation
	// can disable a URL previously configured through the dashboard or migration.
	unset := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, SkillsExternalClientURL: schemas.NewSecretVar("")}
	unsetHash, err := unset.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, unsetHash)

	// A configured URL is a meaningful config change.
	withURL := &ClientConfig{InitialPoolSize: 100, LogRetentionDays: 30, SkillsExternalClientURL: schemas.NewSecretVar("https://bifrost.example.com")}
	urlHash, err := withURL.GenerateClientConfigHash()
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, urlHash)
	t.Setenv("BIFROST_SKILLS_URL_TEST", "https://skills.example")
	store := setupRDBTestStore(t)
	require.NoError(t, store.UpdateClientConfig(context.Background(), base))
	withEnv, err := store.GetClientConfig(context.Background())
	require.NoError(t, err)
	withEnv.SkillsExternalClientURL = schemas.NewSecretVar("env.BIFROST_SKILLS_URL_TEST")
	require.NoError(t, store.UpdateClientConfig(context.Background(), withEnv))
	stored, err := store.GetClientConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "env.BIFROST_SKILLS_URL_TEST", stored.SkillsExternalClientURL.GetRawRef())
	require.Equal(t, "https://skills.example", stored.SkillsExternalClientURL.GetValue())
	wantHash, err := withEnv.GenerateClientConfigHash()
	require.NoError(t, err)
	gotHash, err := stored.GenerateClientConfigHash()
	require.NoError(t, err)
	require.Equal(t, wantHash, gotHash)
	redacted := stored.Redacted()
	require.NotEqual(t, "https://skills.example", redacted.SkillsExternalClientURL.GetValue())
	require.Equal(t, "env.BIFROST_SKILLS_URL_TEST", redacted.SkillsExternalClientURL.GetRawRef())
}

// TestValidatePublicBaseURL permits public base paths and rejects credential-bearing components without echoing input.
func TestValidatePublicBaseURL(t *testing.T) {
	for _, value := range []string{"", "https://skills.example/path", "http://localhost:8080", "https://example.com/path%23section", "http://[::1]:8080"} {
		require.NoError(t, ValidatePublicBaseURL(value))
	}
	for _, value := range []string{"https://user:password@example.com", "https://example.com?token=private", "https://example.com?", "https://example.com#private", "ftp://example.com", "not a URL", "https://:8080", "https://example.com#"} {
		err := ValidatePublicBaseURL(value)
		require.Error(t, err, value)
		require.NotContains(t, err.Error(), value)
	}
}

// vkMetadataPGSchema keeps these tests' tables apart from the shared test schema, so the whole
// schema can be dropped afterwards.
const vkMetadataPGSchema = "configstore_vk_metadata_test"

// vkMetadataStoreBackends returns the SQLite store and, when the local test Postgres is reachable,
// a Postgres store with the virtual key tables. The metadata filter is dialect-specific SQL, so
// both must agree.
func vkMetadataStoreBackends(t *testing.T) map[string]*RDBConfigStore {
	t.Helper()
	backends := map[string]*RDBConfigStore{"sqlite": setupRDBTestStore(t)}
	if pg := trySetupPostgresVKMetadataStore(t); pg != nil {
		backends["postgres"] = pg
	}
	return backends
}

// trySetupPostgresVKMetadataStore returns a Postgres store in its own schema, or nil when the
// local test Postgres is not reachable. Once the server answers, any setup error (schema
// creation, migration) fails the test instead of quietly dropping the Postgres backend, so the
// dialect-specific metadata filter cannot go untested while the run still passes on SQLite.
func trySetupPostgresVKMetadataStore(t *testing.T) *RDBConfigStore {
	t.Helper()
	db, err := gorm.Open(postgres.Open(strings.Replace(postgresDSN, pgTestSchema, vkMetadataPGSchema, 1)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil
	}
	if sqlDB.Ping() != nil {
		_ = sqlDB.Close()
		return nil
	}
	// Registered before the schema statements so the connection is closed even if they fail.
	t.Cleanup(func() {
		_ = db.Exec("DROP SCHEMA IF EXISTS " + vkMetadataPGSchema + " CASCADE").Error
		_ = sqlDB.Close()
	})
	require.NoError(t, db.Exec("DROP SCHEMA IF EXISTS "+vkMetadataPGSchema+" CASCADE").Error)
	require.NoError(t, db.Exec("CREATE SCHEMA "+vkMetadataPGSchema).Error)
	require.NoError(t, db.AutoMigrate(
		&tables.TableCustomer{},
		&tables.TableTeam{},
		&tables.TableBudget{},
		&tables.TableRateLimit{},
		&tables.TableProvider{},
		&tables.TableKey{},
		&tables.TableMCPClient{},
		&tables.TableVirtualKey{},
		&tables.TableVirtualKeyProviderConfig{},
		&tables.TableVirtualKeyProviderConfigKey{},
		&tables.TableVirtualKeyMCPConfig{},
		&tables.TableAgentRegistration{},
		&tables.TableVirtualKeyAgentGrant{},
	))
	require.NoError(t, db.SetupJoinTable(&tables.TableVirtualKeyProviderConfig{}, "Keys", &tables.TableVirtualKeyProviderConfigKey{}))
	s := &RDBConfigStore{logger: nil}
	s.db.Store(db)
	s.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, s.DB())
	}
	s.refreshPoolFn = func(ctx context.Context) error { return nil }
	return s
}

func TestGetVirtualKeysPaginated_MetadataFiltersAndSearch(t *testing.T) {
	for name, store := range vkMetadataStoreBackends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			seed := []*tables.TableVirtualKey{
				{ID: "vk-md-a", Name: "alpha", Value: *schemas.NewSecretVar("vk-md-a-val"), Metadata: map[string]string{"cost_center": "cc-42", "env": "prod"}},
				{ID: "vk-md-b", Name: "bravo", Value: *schemas.NewSecretVar("vk-md-b-val"), Metadata: map[string]string{"cost_center": "cc-42", "env": "dev"}},
				{ID: "vk-md-c", Name: "charlie", Value: *schemas.NewSecretVar("vk-md-c-val"), Metadata: map[string]string{"cost_center": "cc-7", "owner.email": "Ops@Example.com", "quote": `say "hi"`}},
				{ID: "vk-md-none", Name: "delta", Value: *schemas.NewSecretVar("vk-md-none-val")},
				{ID: "vk-md-amp", Name: "echo", Value: *schemas.NewSecretVar("vk-md-amp-val"), Metadata: map[string]string{"vendor": "AT&T <us>"}},
			}
			for _, vk := range seed {
				require.NoError(t, store.CreateVirtualKey(ctx, vk))
			}

			tests := []struct {
				name    string
				params  VirtualKeyQueryParams
				wantIDs []string
			}{
				{name: "one pair", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{"cost_center": "cc-42"}}, wantIDs: []string{"vk-md-a", "vk-md-b"}},
				{name: "pairs AND together", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{"cost_center": "cc-42", "env": "prod"}}, wantIDs: []string{"vk-md-a"}},
				{name: "dotted key", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{"owner.email": "Ops@Example.com"}}, wantIDs: []string{"vk-md-c"}},
				{name: "value with quotes", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{"quote": `say "hi"`}}, wantIDs: []string{"vk-md-c"}},
				{name: "value match is exact", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{"cost_center": "cc-4"}}, wantIDs: nil},
				{name: "unknown key", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{"team": "x"}}, wantIDs: nil},
				{name: "invalid key matches nothing", params: VirtualKeyQueryParams{MetadataFilters: map[string]string{`a" OR 1=1 --`: "x"}}, wantIDs: nil},
				{name: "filter narrows search", params: VirtualKeyQueryParams{Search: "a", MetadataFilters: map[string]string{"env": "dev"}}, wantIDs: []string{"vk-md-b"}},
				{name: "search matches metadata value", params: VirtualKeyQueryParams{Search: "cc-7"}, wantIDs: []string{"vk-md-c"}},
				{name: "search on metadata is case-insensitive", params: VirtualKeyQueryParams{Search: "ops@example"}, wantIDs: []string{"vk-md-c"}},
				// The stored JSON escapes &, < and >, so the search term must be matched in that encoding.
				{name: "search matches metadata values with & and <", params: VirtualKeyQueryParams{Search: "at&t <us"}, wantIDs: []string{"vk-md-amp"}},
				// LIKE metacharacters in the term match literally: "cc_7" must not match "cc-7".
				{name: "search treats _ literally in metadata", params: VirtualKeyQueryParams{Search: "cc_7"}, wantIDs: nil},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					vks, total, err := store.GetVirtualKeysPaginated(ctx, tt.params)
					require.NoError(t, err)
					var got []string
					for _, vk := range vks {
						got = append(got, vk.ID)
					}
					sort.Strings(got)
					assert.Equal(t, tt.wantIDs, got)
					assert.Equal(t, int64(len(tt.wantIDs)), total)
				})
			}
		})
	}
}
