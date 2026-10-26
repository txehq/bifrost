package configstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/capsohq/bifrost/framework/migrator"

	bifrost "github.com/capsohq/bifrost/core"
	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/configstore/tables"
	"github.com/capsohq/bifrost/framework/encrypt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// testMigrationLogger is shared by migration tests that exercise migration
// functions directly.
var testMigrationLogger = bifrost.NewDefaultLogger(schemas.LogLevelInfo)

// pgTestSchema is this package's dedicated Postgres schema. Test packages
// (configstore, configstore/tables, logstore) run in parallel against the same
// database, so each one works in its own schema to avoid clobbering the
// others' tables and rows.
const pgTestSchema = "configstore_test"

// postgresDSN matches the postgres service in tests/docker-compose.yml and
// framework/docker-compose.yml.
const postgresDSN = "host=localhost user=bifrost password=bifrost_password dbname=bifrost port=5432 sslmode=disable search_path=" + pgTestSchema

// namedDB pairs a backend name with its GORM connection for use in subtests.
type namedDB struct {
	name string
	db   *gorm.DB
}

// setupTestDB creates an in-memory SQLite database for testing
func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err, "Failed to create test database")

	// Create the MCP clients table
	err = db.AutoMigrate(&tables.TableMCPClient{})
	require.NoError(t, err, "Failed to migrate test database")

	return db
}

// captureLogOutput captures log output during a function execution
func captureLogOutput(fn func()) string {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	fn()
	return buf.String()
}

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "hyphen to underscore",
			input:    "my-tool",
			expected: "my_tool",
		},
		{
			name:     "space to underscore",
			input:    "my tool",
			expected: "my_tool",
		},
		{
			name:     "multiple hyphens",
			input:    "my-super-tool",
			expected: "my_super_tool",
		},
		{
			name:     "multiple spaces",
			input:    "my super tool",
			expected: "my_super_tool",
		},
		{
			name:     "leading digits removed",
			input:    "123tool",
			expected: "tool",
		},
		{
			name:     "leading digits with hyphen",
			input:    "123my-tool",
			expected: "my_tool",
		},
		{
			name:     "empty after normalization",
			input:    "123",
			expected: "mcp_client",
		},
		{
			name:     "no change needed",
			input:    "my_tool",
			expected: "my_tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized := normalizeMCPClientName(tt.input)
			assert.Equal(t, tt.expected, normalized, "normalizeMCPClientName should produce expected output")
		})
	}
}

func TestFindUniqueName_NoCollision(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create a test client with a unique name
	client := &tables.TableMCPClient{
		Name:           "existing_client",
		ClientID:       "client-1",
		EndpointSlug:   "client-1",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err := db.WithContext(ctx).Create(client).Error
	require.NoError(t, err)

	// Test findUniqueName with a different base name (no collision)
	logOutput := captureLogOutput(func() {
		uniqueName, err := findUniqueNameForTest("new_client", "new_client", 999, db.WithContext(ctx))
		require.NoError(t, err)
		assert.Equal(t, "new_client", uniqueName, "Should return base name when no collision")
	})

	// Should not log anything when there's no collision
	assert.Empty(t, logOutput, "Should not log when name is available without suffix")
}

func TestFindUniqueName_WithCollision(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create existing clients that will cause collisions
	// First client with base name
	client1 := &tables.TableMCPClient{
		Name:           "my_tool",
		ClientID:       "client-1",
		EndpointSlug:   "client-1",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err := db.WithContext(ctx).Create(client1).Error
	require.NoError(t, err)

	// Second client with first suffix
	client2 := &tables.TableMCPClient{
		Name:           "my_tool1",
		ClientID:       "client-2",
		EndpointSlug:   "client-2",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err = db.WithContext(ctx).Create(client2).Error
	require.NoError(t, err)

	// Test findUniqueName with collision - should find "my_tool2"
	// excludeID is set to a non-existent ID (999) so all existing clients are considered
	var uniqueName string
	logOutput := captureLogOutput(func() {
		uniqueName, err = findUniqueNameForTest("my_tool", "my-tool", 999, db.WithContext(ctx))
	})

	require.NoError(t, err)
	assert.Equal(t, "my_tool2", uniqueName, "Should return name with suffix when collision occurs")
	assert.Contains(t, logOutput, "MCP Client Name Normalized: 'my-tool' -> 'my_tool2'", "Should log the transformation")
}

func TestFindUniqueName_MultipleCollisions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create existing clients that will cause multiple collisions
	client1 := &tables.TableMCPClient{
		Name:           "test_tool",
		ClientID:       "client-1",
		EndpointSlug:   "client-1",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err := db.WithContext(ctx).Create(client1).Error
	require.NoError(t, err)

	client2 := &tables.TableMCPClient{
		Name:           "test_tool1",
		ClientID:       "client-2",
		EndpointSlug:   "client-2",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err = db.WithContext(ctx).Create(client2).Error
	require.NoError(t, err)

	client3 := &tables.TableMCPClient{
		Name:           "test_tool2",
		ClientID:       "client-3",
		EndpointSlug:   "client-3",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err = db.WithContext(ctx).Create(client3).Error
	require.NoError(t, err)

	// Test findUniqueName with multiple collisions - should find "test_tool3"
	var uniqueName string
	logOutput := captureLogOutput(func() {
		uniqueName, err = findUniqueNameForTest("test_tool", "test tool", 999, db.WithContext(ctx))
	})

	require.NoError(t, err)
	assert.Equal(t, "test_tool3", uniqueName, "Should return name with correct suffix after multiple collisions")
	assert.Contains(t, logOutput, "MCP Client Name Normalized: 'test tool' -> 'test_tool3'", "Should log the transformation")
}

func TestFindUniqueName_NormalizationAndCollision(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create existing client with normalized name
	client := &tables.TableMCPClient{
		Name:           "my_tool",
		ClientID:       "client-1",
		EndpointSlug:   "client-1",
		ConnectionType: "stdio",
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	err := db.WithContext(ctx).Create(client).Error
	require.NoError(t, err)

	// Test that "my-tool" normalizes to "my_tool" and then collides, requiring suffix
	var uniqueName string
	logOutput := captureLogOutput(func() {
		uniqueName, err = findUniqueNameForTest("my_tool", "my-tool", 999, db.WithContext(ctx))
	})

	require.NoError(t, err)
	assert.Equal(t, "my_tool2", uniqueName, "Should handle normalization and collision")
	assert.Contains(t, logOutput, "MCP Client Name Normalized: 'my-tool' -> 'my_tool2'", "Should log the full transformation")
}

func TestFindUniqueName_MultipleNormalizationsToSameBase(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Test case: 3 entries that normalize to the same base name:
	// "mcp client" -> "mcp_client"
	// "mcp-client" -> "mcp_client" (collision, becomes "mcp_client2")
	// "1mcp-client" -> "mcp_client" (collision, becomes "mcp_client3")
	// Note: In the actual migration, names are processed sequentially and each checks
	// against all previously created names. To simulate this, we need to create clients
	// with the original names first, then normalize them in sequence.

	// Helper function to normalize (same logic as in migrations.go)
	normalizeName := func(name string) string {
		normalized := strings.ReplaceAll(name, "-", "_")
		normalized = strings.ReplaceAll(normalized, " ", "_")
		normalized = strings.TrimLeftFunc(normalized, func(r rune) bool {
			return r >= '0' && r <= '9'
		})
		if normalized == "" {
			normalized = "mcp_client"
		}
		return normalized
	}

	// Create three clients with original names (simulating pre-migration state)
	clients := []*tables.TableMCPClient{
		{
			Name:           "mcp client",
			ClientID:       "client-1",
			EndpointSlug:   "client-1",
			ConnectionType: "stdio",
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
		{
			Name:           "mcp-client",
			ClientID:       "client-2",
			EndpointSlug:   "client-2",
			ConnectionType: "stdio",
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
		{
			Name:           "1mcp-client",
			ClientID:       "client-3",
			EndpointSlug:   "client-3",
			ConnectionType: "stdio",
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
	}

	for _, client := range clients {
		err := db.WithContext(ctx).Create(client).Error
		require.NoError(t, err)
	}

	// Now simulate the migration: process each client sequentially
	// First: "mcp client" -> "mcp_client" (no collision)
	client1 := clients[0]
	normalizedName1 := normalizeName(client1.Name)
	var uniqueName1 string
	var err error
	logOutput1 := captureLogOutput(func() {
		uniqueName1, err = findUniqueNameForTest(normalizedName1, client1.Name, client1.ID, db.WithContext(ctx))
	})
	require.NoError(t, err)
	assert.Equal(t, "mcp_client", uniqueName1, "First normalization should use base name")
	assert.Empty(t, logOutput1, "Should not log when name is available without suffix")

	// Update first client
	err = db.WithContext(ctx).Model(client1).Update("name", uniqueName1).Error
	require.NoError(t, err)

	// Second: "mcp-client" -> "mcp_client" (collision with "mcp_client", becomes "mcp_client2")
	// Note: We need to check that "mcp_client" exists (from client1), so it should skip to "mcp_client2"
	client2 := clients[1]
	normalizedName2 := normalizeName(client2.Name)
	var uniqueName2 string
	logOutput2 := captureLogOutput(func() {
		uniqueName2, err = findUniqueNameForTest(normalizedName2, client2.Name, client2.ID, db.WithContext(ctx))
	})
	require.NoError(t, err)
	// With the updated implementation, suffixes start from 2 when base name exists
	// So "mcp-client" normalizes to "mcp_client" which collides, becomes "mcp_client2"
	assert.Equal(t, "mcp_client2", uniqueName2, "Second normalization should get suffix 2 (skipping 1)")
	assert.Contains(t, logOutput2, "MCP Client Name Normalized: 'mcp-client' -> 'mcp_client2'", "Should log the transformation")

	// Update second client
	err = db.WithContext(ctx).Model(client2).Update("name", uniqueName2).Error
	require.NoError(t, err)

	// Third: "1mcp-client" -> "mcp_client" (collision with "mcp_client" and "mcp_client2", becomes "mcp_client3")
	client3 := clients[2]
	normalizedName3 := normalizeName(client3.Name)
	var uniqueName3 string
	logOutput3 := captureLogOutput(func() {
		uniqueName3, err = findUniqueNameForTest(normalizedName3, client3.Name, client3.ID, db.WithContext(ctx))
	})
	require.NoError(t, err)
	// Third normalization finds "mcp_client" and "mcp_client2" exist, so becomes "mcp_client3"
	assert.Equal(t, "mcp_client3", uniqueName3, "Third normalization should get suffix 3")
	assert.Contains(t, logOutput3, "MCP Client Name Normalized: '1mcp-client' -> 'mcp_client3'", "Should log the transformation")

	// Update third client
	err = db.WithContext(ctx).Model(client3).Update("name", uniqueName3).Error
	require.NoError(t, err)

	// Final verification: all three should exist with correct names
	var finalClients []tables.TableMCPClient
	err = db.WithContext(ctx).Find(&finalClients).Error
	require.NoError(t, err)
	assert.Len(t, finalClients, 3, "Should have all 3 clients")

	names := make([]string, len(finalClients))
	for i, c := range finalClients {
		names[i] = c.Name
	}
	assert.Contains(t, names, "mcp_client", "Should contain mcp_client")
	assert.Contains(t, names, "mcp_client2", "Should contain mcp_client2")
	assert.Contains(t, names, "mcp_client3", "Should contain mcp_client3")
}

func TestFindUniqueName_MigrationScenarioWithInMemoryTracking(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// This test simulates the exact migration scenario where clients are processed in a loop
	// and we need to track assigned names in memory to avoid transaction visibility issues

	// Create three clients with original names (simulating pre-migration state)
	clients := []*tables.TableMCPClient{
		{
			Name:           "mcp client",
			ClientID:       "client-1",
			EndpointSlug:   "client-1",
			ConnectionType: "stdio",
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
		{
			Name:           "mcp-client",
			ClientID:       "client-2",
			EndpointSlug:   "client-2",
			ConnectionType: "stdio",
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
		{
			Name:           "1mcp-client",
			ClientID:       "client-3",
			EndpointSlug:   "client-3",
			ConnectionType: "stdio",
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
	}

	for _, client := range clients {
		err := db.WithContext(ctx).Create(client).Error
		require.NoError(t, err)
	}

	// Simulate the migration: process clients in a loop with in-memory tracking
	assignedNames := make(map[string]bool)
	normalizeName := func(name string) string {
		normalized := strings.ReplaceAll(name, "-", "_")
		normalized = strings.ReplaceAll(normalized, " ", "_")
		normalized = strings.TrimLeftFunc(normalized, func(r rune) bool {
			return r >= '0' && r <= '9'
		})
		if normalized == "" {
			normalized = "mcp_client"
		}
		return normalized
	}

	var logOutputs []string
	for _, client := range clients {
		originalName := client.Name
		needsUpdate := strings.Contains(originalName, "-") || strings.Contains(originalName, " ") ||
			(len(originalName) > 0 && originalName[0] >= '0' && originalName[0] <= '9')

		if needsUpdate {
			normalizedName := normalizeName(originalName)
			uniqueName, err := findUniqueNameForTestWithTracking(normalizedName, originalName, client.ID, db.WithContext(ctx), assignedNames)
			require.NoError(t, err)

			// Capture log output
			logOutput := captureLogOutput(func() {
				// Log if name changed
				if originalName != uniqueName {
					log.Printf("MCP Client Name Normalized: '%s' -> '%s'", originalName, uniqueName)
				}
			})
			if logOutput != "" {
				logOutputs = append(logOutputs, logOutput)
			}

			// Update client
			err = db.WithContext(ctx).Model(client).Update("name", uniqueName).Error
			require.NoError(t, err)
		}
	}

	// Verify all three clients have correct names
	var finalClients []tables.TableMCPClient
	err := db.WithContext(ctx).Find(&finalClients).Error
	require.NoError(t, err)
	assert.Len(t, finalClients, 3, "Should have all 3 clients")

	names := make([]string, len(finalClients))
	for i, c := range finalClients {
		names[i] = c.Name
	}
	assert.Contains(t, names, "mcp_client", "Should contain mcp_client")
	assert.Contains(t, names, "mcp_client2", "Should contain mcp_client2")
	assert.Contains(t, names, "mcp_client3", "Should contain mcp_client3")

	// Verify logging: should log all three transformations
	allLogs := strings.Join(logOutputs, "")
	assert.Contains(t, allLogs, "MCP Client Name Normalized: 'mcp client' -> 'mcp_client'", "Should log first normalization")
	assert.Contains(t, allLogs, "MCP Client Name Normalized: 'mcp-client' -> 'mcp_client2'", "Should log second normalization")
	assert.Contains(t, allLogs, "MCP Client Name Normalized: '1mcp-client' -> 'mcp_client3'", "Should log third normalization")
}

// findUniqueNameForTestWithTracking is a test helper that tracks assigned names in memory
func findUniqueNameForTestWithTracking(baseName string, originalName string, excludeID uint, tx *gorm.DB, assignedNames map[string]bool) (string, error) {
	// First check if base name is already assigned in this migration
	if !assignedNames[baseName] {
		// Also check database for existing names (excluding current client)
		var count int64
		err := tx.Model(&tables.TableMCPClient{}).Where("name = ? AND id != ?", baseName, excludeID).Count(&count).Error
		if err != nil {
			return "", fmt.Errorf("failed to check name availability: %w", err)
		}
		if count == 0 {
			// Name is available
			assignedNames[baseName] = true
			// Log normalization even when no collision
			if originalName != baseName {
				log.Printf("MCP Client Name Normalized: '%s' -> '%s'", originalName, baseName)
			}
			return baseName, nil
		}
	}

	// Name exists (either assigned in this migration or in database), try with number suffix starting from 2
	suffix := 2
	const maxSuffix = 1000
	for {
		if suffix > maxSuffix {
			return "", fmt.Errorf("could not find unique name after %d attempts for base name: %s", maxSuffix, baseName)
		}
		candidateName := baseName + strconv.Itoa(suffix)

		// Check both in-memory map and database
		if !assignedNames[candidateName] {
			var count int64
			err := tx.Model(&tables.TableMCPClient{}).Where("name = ? AND id != ?", candidateName, excludeID).Count(&count).Error
			if err != nil {
				return "", fmt.Errorf("failed to check name availability: %w", err)
			}
			if count == 0 {
				// Found available name
				assignedNames[candidateName] = true
				log.Printf("MCP Client Name Normalized: '%s' -> '%s'", originalName, candidateName)
				return candidateName, nil
			}
		}
		suffix++
	}
}

// findUniqueNameForTest is a test helper that extracts the findUniqueName logic
// This mirrors the implementation in migrations.go for testing
func findUniqueNameForTest(baseName string, originalName string, excludeID uint, tx *gorm.DB) (string, error) {
	// First, try the base name
	var count int64
	err := tx.Model(&tables.TableMCPClient{}).Where("name = ? AND id != ?", baseName, excludeID).Count(&count).Error
	if err != nil {
		return "", fmt.Errorf("failed to check name availability: %w", err)
	}
	if count == 0 {
		// Name is available
		return baseName, nil
	}

	// Name exists, try with number suffix starting from 2
	// (base name is conceptually "1", so collisions start from "2")
	suffix := 2
	const maxSuffix = 1000 // Safety limit to prevent infinite loops
	for {
		if suffix > maxSuffix {
			return "", fmt.Errorf("could not find unique name after %d attempts for base name: %s", maxSuffix, baseName)
		}
		candidateName := baseName + strconv.Itoa(suffix)
		err := tx.Model(&tables.TableMCPClient{}).Where("name = ? AND id != ?", candidateName, excludeID).Count(&count).Error
		if err != nil {
			return "", fmt.Errorf("failed to check name availability: %w", err)
		}
		if count == 0 {
			// Found available name - log the transformation
			log.Printf("MCP Client Name Normalized: '%s' -> '%s'", originalName, candidateName)
			return candidateName, nil
		}
		suffix++
	}
}

// setupProviderTestDBWithoutStoreRawColumn creates an in-memory SQLite database
// with the config_providers table but WITHOUT the store_raw_request_response column,
// simulating the pre-migration state.
func setupProviderTestDBWithoutStoreRawColumn(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err, "Failed to create test database")

	// Create the config_providers table manually without store_raw_request_response column
	// This simulates the pre-migration state
	err = db.Exec(`
		CREATE TABLE config_providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name VARCHAR(50) NOT NULL UNIQUE,
			network_config_json TEXT,
			concurrency_buffer_json TEXT,
			proxy_config_json TEXT,
			custom_provider_config_json TEXT,
			pricing_overrides_json TEXT,
			send_back_raw_request BOOLEAN DEFAULT 0,
			send_back_raw_response BOOLEAN DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			budget_id VARCHAR(255),
			rate_limit_id VARCHAR(255),
			config_hash VARCHAR(255),
			status VARCHAR(50) DEFAULT 'unknown',
			description TEXT,
			encryption_status VARCHAR(20) DEFAULT 'plain_text'
		)
	`).Error
	require.NoError(t, err, "Failed to create config_providers table")

	// Create the migrations table for the migrator (matches migrator.DefaultOptions.TableName)
	err = db.Exec(`
		CREATE TABLE IF NOT EXISTS migrations (
			id VARCHAR(255) PRIMARY KEY
		)
	`).Error
	require.NoError(t, err, "Failed to create migrations table")

	return db
}

// trySetupPostgresDBWithoutStoreRawColumn attempts to connect to Postgres and creates
// the config_providers table WITHOUT the store_raw_request_response column.
// Returns nil (without skipping the test) if Postgres is unavailable.
func trySetupPostgresDBWithoutStoreRawColumn(t *testing.T, testSuffix string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(postgresDSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil
	}

	// Verify the connection is actually live before proceeding.
	sqlDB, err := db.DB()
	if err != nil {
		return nil
	}
	if err := sqlDB.Ping(); err != nil {
		return nil
	}

	// All objects live in this package's dedicated schema (via search_path in
	// the DSN), isolated from other test packages sharing the same database.
	if err := db.Exec("CREATE SCHEMA IF NOT EXISTS " + pgTestSchema).Error; err != nil {
		return nil
	}

	// Drop config_providers to start fresh (for this specific test).
	// Use CASCADE to drop dependent objects (composite types, sequences, etc.).
	db.Exec("DROP TABLE IF EXISTS config_providers CASCADE")

	// Clear migration tracking so the migration under test always runs.
	db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		id VARCHAR(255) PRIMARY KEY
	)`)
	db.Exec("DELETE FROM migrations")

	// Create the config_providers table manually without store_raw_request_response column
	// This simulates the pre-migration state (PostgreSQL syntax)
	err = db.Exec(`
		CREATE TABLE config_providers (
			id SERIAL PRIMARY KEY,
			name VARCHAR(50) NOT NULL UNIQUE,
			network_config_json TEXT,
			concurrency_buffer_json TEXT,
			proxy_config_json TEXT,
			custom_provider_config_json TEXT,
			pricing_overrides_json TEXT,
			send_back_raw_request BOOLEAN DEFAULT false,
			send_back_raw_response BOOLEAN DEFAULT false,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			budget_id VARCHAR(255),
			rate_limit_id VARCHAR(255),
			config_hash VARCHAR(255),
			status VARCHAR(50) DEFAULT 'unknown',
			description TEXT,
			encryption_status VARCHAR(20) DEFAULT 'plain_text'
		)
	`).Error
	if err != nil {
		return nil
	}

	// Clean up after the test so later tests in this package start fresh.
	t.Cleanup(func() {
		db.Exec("DELETE FROM migrations")
		db.Exec("DROP TABLE IF EXISTS config_providers CASCADE")
	})

	return db
}

// forEachProviderMigrationDB returns backends for provider migration tests.
// Always includes SQLite; includes Postgres when available.
func forEachProviderMigrationDB(t *testing.T, testSuffix string) []namedDB {
	t.Helper()
	dbs := []namedDB{{"sqlite", setupProviderTestDBWithoutStoreRawColumn(t)}}
	if pgDB := trySetupPostgresDBWithoutStoreRawColumn(t, testSuffix); pgDB != nil {
		dbs = append(dbs, namedDB{"postgres", pgDB})
	}
	return dbs
}

// setupVKTestDBWithoutRotationColumns creates an in-memory SQLite database with
// governance_virtual_keys in its pre-rotation-migration shape: none of the
// previous_value*/rotated_at columns and no idx_virtual_key_previous_value_hash
// index, simulating an upgraded (not fresh) installation.
func setupVKTestDBWithoutRotationColumns(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err, "Failed to create test database")

	err = db.Exec(`
		CREATE TABLE governance_virtual_keys (
			id VARCHAR(255) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			value TEXT,
			value_hash VARCHAR(64),
			encryption_status VARCHAR(20) DEFAULT 'plain_text',
			config_hash VARCHAR(255),
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)
	`).Error
	require.NoError(t, err, "Failed to create governance_virtual_keys table")

	err = db.Exec(`
		CREATE TABLE IF NOT EXISTS migrations (
			id VARCHAR(255) PRIMARY KEY
		)
	`).Error
	require.NoError(t, err, "Failed to create migrations table")

	return db
}

func TestMigrationMovePricingOverrideSearchContextToWebSearch(t *testing.T) {
	db := setupRDBTestStore(t).DB()
	ctx := context.Background()
	table := tables.TablePricingOverride{}.TableName()
	now := time.Now()
	for id, patch := range map[string]string{
		"legacy":   `{"input_cost_per_token":0.001,"search_context_cost_per_query":0.02}`,
		"explicit": `{"search_context_cost_per_query":0.02,"web_search_cost_per_request":0.05}`,
		"none":     `{"input_cost_per_token":0.001}`,
	} {
		require.NoError(t, db.Table(table).Create(map[string]any{
			"id": id, "name": id, "scope_kind": "global", "match_type": "exact", "pattern": "gpt-4o",
			"request_types_json": "[]", "pricing_patch_json": patch, "created_at": now, "updated_at": now,
		}).Error)
	}

	require.NoError(t, migrationMovePricingOverrideSearchContextToWebSearch(ctx, db, testMigrationLogger))

	patchOf := func(id string) string {
		var patch string
		require.NoError(t, db.Table(table).Select("pricing_patch_json").Where("id = ?", id).Scan(&patch).Error)
		return patch
	}
	assert.JSONEq(t, `{"input_cost_per_token":0.001,"web_search_cost_per_request":0.02}`, patchOf("legacy"))
	assert.JSONEq(t, `{"web_search_cost_per_request":0.05}`, patchOf("explicit"))
	assert.JSONEq(t, `{"input_cost_per_token":0.001}`, patchOf("none"))
}

func TestMigrationAddWebSearchCostPerRequestColumn_BackfillsFromSearchContext(t *testing.T) {
	db := setupRDBTestStore(t).DB()
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(&tables.TableModelPricing{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableModelPricing{}, "web_search_cost_per_request"))

	table := tables.TableModelPricing{}.TableName()
	require.NoError(t, db.Table(table).Create(map[string]any{"model": "claude-haiku-4-5", "provider": "anthropic", "mode": "chat", "search_context_cost_per_query": 0.01}).Error)
	require.NoError(t, db.Table(table).Create(map[string]any{"model": "gpt-4o", "provider": "openai", "mode": "chat"}).Error)

	require.NoError(t, migrationAddWebSearchCostPerRequestColumn(ctx, db, testMigrationLogger))

	var got []tables.TableModelPricing
	require.NoError(t, db.Order("model").Find(&got).Error)
	require.Len(t, got, 2)
	require.NotNil(t, got[0].WebSearchCostPerRequest)
	assert.InDelta(t, 0.01, *got[0].WebSearchCostPerRequest, 1e-12)
	assert.Nil(t, got[1].WebSearchCostPerRequest)
}

func TestMigrationAddVKRotationCooldownColumns_CreatesIndex(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	mg := db.Migrator()

	require.False(t, mg.HasColumn(&tables.TableVirtualKey{}, "previous_value_hash"),
		"previous_value_hash column must not exist before migration")
	require.False(t, mg.HasIndex(&tables.TableVirtualKey{}, "idx_virtual_key_previous_value_hash"),
		"previous_value_hash index must not exist before migration")

	require.NoError(t, migrationAddVKRotationCooldownColumns(ctx, db, testMigrationLogger))

	for _, column := range []string{"previous_value", "previous_value_hash", "previous_value_expires_at", "rotated_at"} {
		assert.True(t, mg.HasColumn(&tables.TableVirtualKey{}, column),
			"%s column should exist after migration", column)
	}
	assert.True(t, mg.HasIndex(&tables.TableVirtualKey{}, "idx_virtual_key_previous_value_hash"),
		"upgrade path must create the previous_value_hash index declared in struct tags")

	// Idempotent: a re-run with the index already present must not fail.
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_vk_rotation_cooldown_columns").Error)
	require.NoError(t, migrationAddVKRotationCooldownColumns(ctx, db, testMigrationLogger))
}

func TestMigrationAddStoreRawRequestResponseColumn(t *testing.T) {
	tests := []struct {
		name                            string
		sendBackRawRequest              bool
		sendBackRawResponse             bool
		expectedStoreRawRequestResponse bool
	}{
		{
			name:                            "both false - store should be false",
			sendBackRawRequest:              false,
			sendBackRawResponse:             false,
			expectedStoreRawRequestResponse: false,
		},
		{
			name:                            "request true response false - store should be true",
			sendBackRawRequest:              true,
			sendBackRawResponse:             false,
			expectedStoreRawRequestResponse: true,
		},
		{
			name:                            "request false response true - store should be true",
			sendBackRawRequest:              false,
			sendBackRawResponse:             true,
			expectedStoreRawRequestResponse: true,
		},
		{
			name:                            "both true - store should be true",
			sendBackRawRequest:              true,
			sendBackRawResponse:             true,
			expectedStoreRawRequestResponse: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			for _, ndb := range forEachProviderMigrationDB(t, tt.name) {
				ndb := ndb
				t.Run(ndb.name, func(t *testing.T) {
					db := ndb.db
					ctx := context.Background()

					providerName := "test_provider"
					staleHash := "stale_hash_before_migration"
					now := time.Now()

					// Insert a provider with the old schema (no store_raw_request_response column)
					err := db.Exec(`
						INSERT INTO config_providers (
							name, send_back_raw_request, send_back_raw_response,
							config_hash, created_at, updated_at, encryption_status
						) VALUES (?, ?, ?, ?, ?, ?, ?)
					`, providerName, tt.sendBackRawRequest, tt.sendBackRawResponse, staleHash, now, now, "plain_text").Error
					require.NoError(t, err, "Failed to insert test provider")

					// Verify column does not exist before migration
					hasColumn := db.Migrator().HasColumn(&tables.TableProvider{}, "store_raw_request_response")
					assert.False(t, hasColumn, "store_raw_request_response column should not exist before migration")

					// Run the migration
					err = migrationAddStoreRawRequestResponseColumn(ctx, db, testMigrationLogger)
					require.NoError(t, err, "Migration should succeed")

					// Verify column exists after migration
					hasColumn = db.Migrator().HasColumn(&tables.TableProvider{}, "store_raw_request_response")
					assert.True(t, hasColumn, "store_raw_request_response column should exist after migration")

					// Fetch the provider and verify values
					var result struct {
						Name                    string
						SendBackRawRequest      bool
						SendBackRawResponse     bool
						StoreRawRequestResponse bool
						ConfigHash              string
					}
					err = db.Table("config_providers").
						Select("name, send_back_raw_request, send_back_raw_response, store_raw_request_response, config_hash").
						Where("name = ?", providerName).
						Scan(&result).Error
					require.NoError(t, err, "Failed to fetch provider after migration")

					// Verify store_raw_request_response was set correctly
					assert.Equal(t, tt.expectedStoreRawRequestResponse, result.StoreRawRequestResponse,
						"store_raw_request_response should be set based on send_back_raw_request OR send_back_raw_response")

					// Verify config_hash was updated (not the stale hash)
					assert.NotEqual(t, staleHash, result.ConfigHash,
						"config_hash should be updated after migration")

					// Verify the hash matches what GenerateConfigHash would produce
					expectedConfig := ProviderConfig{
						SendBackRawRequest:      tt.sendBackRawRequest,
						SendBackRawResponse:     tt.sendBackRawResponse,
						StoreRawRequestResponse: tt.expectedStoreRawRequestResponse,
					}
					expectedHash, err := expectedConfig.GenerateConfigHash(providerName)
					require.NoError(t, err, "Failed to generate expected hash")
					assert.Equal(t, expectedHash, result.ConfigHash,
						"config_hash should match the expected hash from GenerateConfigHash")
				})
			}
		})
	}
}

func TestMigrationAddStoreRawRequestResponseColumn_MultipleProviders(t *testing.T) {
	for _, ndb := range forEachProviderMigrationDB(t, "multiple") {
		ndb := ndb
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			ctx := context.Background()

			now := time.Now()

			// Insert multiple providers with different configurations
			providers := []struct {
				name                string
				sendBackRawRequest  bool
				sendBackRawResponse bool
			}{
				{"provider_neither", false, false},
				{"provider_request_only", true, false},
				{"provider_response_only", false, true},
				{"provider_both", true, true},
			}

			for _, p := range providers {
				err := db.Exec(`
					INSERT INTO config_providers (
						name, send_back_raw_request, send_back_raw_response,
						config_hash, created_at, updated_at, encryption_status
					) VALUES (?, ?, ?, ?, ?, ?, ?)
				`, p.name, p.sendBackRawRequest, p.sendBackRawResponse, "stale_hash", now, now, "plain_text").Error
				require.NoError(t, err, "Failed to insert provider %s", p.name)
			}

			// Run the migration
			err := migrationAddStoreRawRequestResponseColumn(ctx, db, testMigrationLogger)
			require.NoError(t, err, "Migration should succeed")

			// Verify each provider
			for _, p := range providers {
				var result struct {
					StoreRawRequestResponse bool
					ConfigHash              string
				}
				err := db.Table("config_providers").
					Select("store_raw_request_response, config_hash").
					Where("name = ?", p.name).
					Scan(&result).Error
				require.NoError(t, err, "Failed to fetch provider %s", p.name)

				expectedStore := p.sendBackRawRequest || p.sendBackRawResponse
				assert.Equal(t, expectedStore, result.StoreRawRequestResponse,
					"Provider %s: store_raw_request_response mismatch", p.name)
				assert.NotEqual(t, "stale_hash", result.ConfigHash,
					"Provider %s: config_hash should be updated", p.name)
			}
		})
	}
}

func TestMigrationAddStoreRawRequestResponseColumn_Idempotent(t *testing.T) {
	for _, ndb := range forEachProviderMigrationDB(t, "idempotent") {
		ndb := ndb
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			ctx := context.Background()

			now := time.Now()
			providerName := "idempotent_test_provider"

			// Insert a provider
			err := db.Exec(`
				INSERT INTO config_providers (
					name, send_back_raw_request, send_back_raw_response,
					config_hash, created_at, updated_at, encryption_status
				) VALUES (?, ?, ?, ?, ?, ?, ?)
			`, providerName, true, false, "stale_hash", now, now, "plain_text").Error
			require.NoError(t, err, "Failed to insert test provider")

			// Run the migration first time
			err = migrationAddStoreRawRequestResponseColumn(ctx, db, testMigrationLogger)
			require.NoError(t, err, "First migration should succeed")

			// Get the hash after first migration
			var firstHash string
			err = db.Table("config_providers").
				Select("config_hash").
				Where("name = ?", providerName).
				Scan(&firstHash).Error
			require.NoError(t, err)

			// Run the migration second time (should be idempotent)
			err = migrationAddStoreRawRequestResponseColumn(ctx, db, testMigrationLogger)
			require.NoError(t, err, "Second migration should succeed (idempotent)")

			// Verify hash is unchanged after second run
			var secondHash string
			err = db.Table("config_providers").
				Select("config_hash").
				Where("name = ?", providerName).
				Scan(&secondHash).Error
			require.NoError(t, err)

			assert.Equal(t, firstHash, secondHash, "Hash should remain unchanged after idempotent migration run")
		})
	}
}

// setupKeyDBWithLegacyDeploymentColumns creates an in-memory SQLite database
// with the config_keys table including legacy deployment columns and NO aliases_json.
// This simulates the pre-aliases, post-encryption database state.
func setupKeyDBWithLegacyDeploymentColumns(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	// Create migrations table (required by the migrator)
	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)

	// Start from the current key schema so this fixture stays aligned as new
	// provider key fields are added. Then restore only the pre-alias shape that
	// this migration is responsible for.
	require.NoError(t, db.AutoMigrate(&tables.TableKey{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableKey{}, "aliases_json"))
	for _, column := range []string{
		"azure_deployments_json",
		"vertex_deployments_json",
		"bedrock_deployments_json",
		"replicate_deployments_json",
	} {
		require.NoError(t, db.Exec("ALTER TABLE config_keys ADD COLUMN "+column+" TEXT").Error)
	}

	t.Cleanup(func() {
		db.Exec("DELETE FROM migrations")
		db.Exec("DROP TABLE IF EXISTS config_keys")
	})

	return db
}

// TestMigrationDropDeploymentColumnsAndAddAliases_EncryptedRows tests that the
// aliases migration correctly handles rows where encryption_status is 'encrypted'
// but deployment columns contain plaintext JSON (because those columns were never
// in the encryption list). This was the root cause of the base64 decode crash.
func TestMigrationDropDeploymentColumnsAndAddAliases_EncryptedRows(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled, skipping encrypted rows test")
	}

	db := setupKeyDBWithLegacyDeploymentColumns(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Encrypt the key values (simulating what EncryptPlaintextRows would have done)
	encVal1, err := encrypt.Encrypt("sk-azure-key-1")
	require.NoError(t, err)
	encVal2, err := encrypt.Encrypt("sk-vertex-key-1")
	require.NoError(t, err)
	encVal3, err := encrypt.Encrypt("sk-replicate-key-1")
	require.NoError(t, err)

	// Insert encrypted rows with PLAINTEXT deployment JSON.
	// azure_deployments_json, vertex_deployments_json, and replicate_deployments_json
	// were never in the encryption list, so they remain plaintext even though the row
	// is marked as encrypted.
	err = db.Exec(`
		INSERT INTO config_keys (
			name, provider_id, provider, key_id, value, models_json,
			azure_deployments_json, encryption_status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'encrypted', ?, ?)`,
		"azure-key-1", 1, "azure", "ak-1", encVal1, `["*"]`,
		`{"gpt-4":"dep-gpt4","gpt-3.5":"dep-gpt35"}`, now, now,
	).Error
	require.NoError(t, err)

	err = db.Exec(`
		INSERT INTO config_keys (
			name, provider_id, provider, key_id, value, models_json,
			vertex_deployments_json, encryption_status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'encrypted', ?, ?)`,
		"vertex-key-1", 2, "vertex", "vk-1", encVal2, `["*"]`,
		`{"gemini-pro":"dep-gemini"}`, now, now,
	).Error
	require.NoError(t, err)

	err = db.Exec(`
		INSERT INTO config_keys (
			name, provider_id, provider, key_id, value, models_json,
			replicate_deployments_json, encryption_status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'encrypted', ?, ?)`,
		"replicate-key-1", 3, "replicate", "rk-1", encVal3, `["*"]`,
		`{"llama":"dep-llama"}`, now, now,
	).Error
	require.NoError(t, err)

	// Run the aliases migration — this should NOT crash with base64 decode error.
	err = migrationDropDeploymentColumnsAndAddAliases(ctx, db, testMigrationLogger)
	require.NoError(t, err, "migration should not crash on encrypted rows with plaintext deployment data")

	// Verify aliases_json was populated and is readable via GORM hooks
	var keys []tables.TableKey
	err = db.Order("name").Find(&keys).Error
	require.NoError(t, err)
	require.Len(t, keys, 3)

	// Azure key should have its deployment data migrated to aliases
	assert.Equal(t, "azure-key-1", keys[0].Name)
	assert.NotNil(t, keys[0].Aliases)

	// Vertex key
	assert.Equal(t, "vertex-key-1", keys[2].Name)
	assert.NotNil(t, keys[2].Aliases)

	// Replicate key — sorted alphabetically, it's between azure and vertex
	assert.Equal(t, "replicate-key-1", keys[1].Name)
	assert.NotNil(t, keys[1].Aliases)
}

// TestMigrationDropDeploymentColumnsAndAddAliases_BedrockEncrypted tests the branch
// where bedrock_deployments_json is already encrypted before migration. The migration
// should detect it's already encrypted (Decrypt succeeds) and NOT double-encrypt it.
func TestMigrationDropDeploymentColumnsAndAddAliases_BedrockEncrypted(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled, skipping bedrock encrypted test")
	}

	db := setupKeyDBWithLegacyDeploymentColumns(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Encrypt the key value
	encVal, err := encrypt.Encrypt("sk-bedrock-key-1")
	require.NoError(t, err)

	// Encrypt the deployments JSON (simulating bedrock which WAS in the encryption list)
	bedrockDeployments := `{"claude":"dep-claude","claude-instant":"dep-instant"}`
	encDeployments, err := encrypt.Encrypt(bedrockDeployments)
	require.NoError(t, err)

	// Insert a bedrock row where bedrock_deployments_json is already encrypted
	err = db.Exec(`
		INSERT INTO config_keys (
			name, provider_id, provider, key_id, value, models_json,
			bedrock_deployments_json, encryption_status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'encrypted', ?, ?)`,
		"bedrock-key-1", 4, "bedrock", "bk-1", encVal, `["*"]`,
		encDeployments, now, now,
	).Error
	require.NoError(t, err)

	// Run the aliases migration — should detect already-encrypted data and skip re-encryption
	err = migrationDropDeploymentColumnsAndAddAliases(ctx, db, testMigrationLogger)
	require.NoError(t, err, "migration should handle already-encrypted bedrock deployments")

	// Verify aliases_json was populated and is readable via GORM hooks (AfterFind decrypts)
	var keys []tables.TableKey
	err = db.Find(&keys).Error
	require.NoError(t, err)
	require.Len(t, keys, 1)

	assert.Equal(t, "bedrock-key-1", keys[0].Name)
	assert.NotNil(t, keys[0].Aliases)

	// Verify the aliases contain the original deployment data (not double-encrypted)
	aliases := keys[0].Aliases
	assert.Contains(t, aliases, "claude")
	assert.Equal(t, "dep-claude", aliases["claude"].ModelID)
	assert.Equal(t, "dep-instant", aliases["claude-instant"].ModelID)
}

func TestMigrationDropDeploymentColumnsAndAddAliases_NewWritesUseAliasesJSON(t *testing.T) {
	db := setupKeyDBWithLegacyDeploymentColumns(t)
	ctx := context.Background()

	err := migrationDropDeploymentColumnsAndAddAliases(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	assert.False(t, db.Migrator().HasColumn(&tables.TableKey{}, "azure_deployments_json"))

	key := &tables.TableKey{
		Name:       "azure-post-migration-write",
		ProviderID: 1,
		Provider:   "azure",
		KeyID:      "azure-post-migration-write",
		Value:      *schemas.NewSecretVar("sk-azure-post-migration"),
		Aliases: schemas.KeyAliases{
			"gpt-4o": {ModelID: "azure-post-migration-deployment"},
		},
		AzureKeyConfig: &schemas.AzureKeyConfig{},
	}

	require.NoError(t, db.Create(key).Error, "post-migration writes must not target dropped deployment columns")

	var raw map[string]any
	require.NoError(t, db.Table("config_keys").Where("id = ?", key.ID).Take(&raw).Error)
	require.NotNil(t, raw["aliases_json"], "aliases_json should store post-migration deployments")

	var found tables.TableKey
	require.NoError(t, db.Where("id = ?", key.ID).First(&found).Error)
	assert.Equal(t, schemas.KeyAliases{"gpt-4o": {ModelID: "azure-post-migration-deployment"}}, found.Aliases)
}

// ============================================================================
// Helper: full migration DB setup
// ============================================================================

// setupFullMigrationDB creates a fresh in-memory SQLite database and runs the
// full triggerMigrations chain (the same code path as production startup).
// Returns an RDBConfigStore for CRUD verification and the raw *gorm.DB for
// low-level assertions.
// testDBCounter ensures each test gets a unique shared-cache in-memory SQLite URI.
var testDBCounter int64

func setupFullMigrationDB(t *testing.T) (*RDBConfigStore, *gorm.DB) {
	t.Helper()
	// Use a unique shared-cache URI so all connections see the same in-memory DB
	// without requiring MaxOpenConns(1) (which deadlocks when code opens transactions
	// and queries on s.db concurrently).
	n := time.Now().UnixNano() + testDBCounter
	testDBCounter++
	dsn := fmt.Sprintf("file:testdb_%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err, "Failed to create test database")

	ctx := context.Background()
	err = triggerMigrations(ctx, db, testMigrationLogger)
	require.NoError(t, err, "triggerMigrations should succeed on a fresh DB")

	store := &RDBConfigStore{logger: bifrost.NewDefaultLogger(schemas.LogLevelInfo)}
	store.db.Store(db)
	store.migrateOnFreshFn = func(ctx context.Context, fn func(context.Context, *gorm.DB) error) error {
		return fn(ctx, store.DB())
	}
	store.refreshPoolFn = func(ctx context.Context) error { return nil }
	return store, db
}

// ============================================================================
// Part 2: Full-chain integration tests
// ============================================================================

func TestTriggerMigrations_FreshDB(t *testing.T) {
	_, db := setupFullMigrationDB(t)

	// Every critical table should exist after the full migration chain.
	criticalTables := []interface{}{
		&tables.TableProvider{},
		&tables.TableKey{},
		&tables.TableVirtualKey{},
		&tables.TableMCPClient{},
		&tables.TableBudget{},
		&tables.TableRateLimit{},
		&tables.TablePlugin{},
		&tables.TableCustomer{},
		&tables.TableTeam{},
		&tables.SessionsTable{},
		&tables.TableOauthConfig{},
		&tables.TableOauthToken{},
		&tables.TableModelPricing{},
		&tables.TableGovernanceConfig{},
		&tables.TableClientConfig{},
		&tables.TableVirtualKeyProviderConfig{},
		&tables.TableVirtualKeyMCPConfig{},
		&tables.TableNotification{},
		&tables.TableWarpConfig{},
		&tables.TableAgentRegistration{},
		&tables.TableVirtualKeyAgentGrant{},
	}

	migrator := db.Migrator()
	for _, table := range criticalTables {
		assert.True(t, migrator.HasTable(table), "table should exist: %T", table)
	}
	assert.True(t, migrator.HasColumn(&tables.TableModelPricing{}, "is_deprecated"), "model pricing is_deprecated column should exist")
	assert.False(t, migrator.HasColumn(&tables.TableVirtualKey{}, "agent_grants"), "agent_grants is a has-many association, not a database column")
}

func TestMigrationAddAgentGatewayTables_UpgradesExistingDatabase(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// The shipped foundational migration must not create Agent Gateway tables.
	require.NoError(t, migrationInit(ctx, db, testMigrationLogger))
	require.False(t, db.Migrator().HasTable(&tables.TableAgentRegistration{}), "init must not create Agent Gateway tables")
	require.False(t, db.Migrator().HasTable(&tables.TableVirtualKeyAgentGrant{}), "init must not create Agent Gateway tables")

	require.NoError(t, migrationAddAgentGatewayTables(ctx, db, testMigrationLogger))
	require.True(t, db.Migrator().HasTable(&tables.TableAgentRegistration{}))
	require.True(t, db.Migrator().HasTable(&tables.TableVirtualKeyAgentGrant{}))
	require.True(t, db.Migrator().HasTable(&tables.TableAgentPushConfig{}))
	require.True(t, db.Migrator().HasTable(&tables.TableAgentPushDelivery{}))
	require.True(t, db.Migrator().HasColumn(&tables.TableAgentRegistration{}, "extension_uris"))
	require.True(t, db.Migrator().HasColumn(&tables.TableAgentRegistration{}, "forward_accepted_credential_overrides_auth"))

	var grantDDL string
	require.NoError(t, db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", tables.TableVirtualKeyAgentGrant{}.TableName()).Scan(&grantDDL).Error)
	assert.Contains(t, grantDDL, "REFERENCES", "grant table should carry foreign key constraints")
	assert.Contains(t, grantDDL, "ON DELETE CASCADE", "grant table foreign keys should cascade on delete")

	// Re-running is a no-op on an already upgraded database.
	require.NoError(t, migrationAddAgentGatewayTables(ctx, db, testMigrationLogger))
	require.True(t, db.Migrator().HasTable(&tables.TableVirtualKeyAgentGrant{}))
}

func TestTriggerMigrations_Idempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	ctx := context.Background()

	// First run
	err = triggerMigrations(ctx, db, testMigrationLogger)
	require.NoError(t, err, "first triggerMigrations should succeed")

	// Second run – must be a no-op, not an error.
	err = triggerMigrations(ctx, db, testMigrationLogger)
	require.NoError(t, err, "second triggerMigrations should succeed (idempotent)")

	// Tables should still be intact
	assert.True(t, db.Migrator().HasTable(&tables.TableProvider{}), "TableProvider should still exist")
	assert.True(t, db.Migrator().HasTable(&tables.TableKey{}), "TableKey should still exist")
	assert.True(t, db.Migrator().HasTable(&tables.TableVirtualKey{}), "TableVirtualKey should still exist")
}

func TestFullMigration_ProviderAndKeyCRUD(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled")
	}

	store, db := setupFullMigrationDB(t)
	ctx := context.Background()

	config := ProviderConfig{
		Keys: []schemas.Key{
			{
				ID:     "key-uuid-1",
				Name:   "openai-primary",
				Value:  *schemas.NewSecretVar("sk-test-secret-key-12345"),
				Models: schemas.WhiteList{"*"},
				Weight: 1.0,
			},
		},
	}

	err := store.AddProvider(ctx, "openai", config)
	require.NoError(t, err)

	// Read back through store (AfterFind hooks decrypt)
	result, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.Contains(t, result, schemas.ModelProvider("openai"))

	openai := result[schemas.ModelProvider("openai")]
	require.Len(t, openai.Keys, 1)
	assert.Equal(t, "openai-primary", openai.Keys[0].Name)
	assert.Equal(t, "sk-test-secret-key-12345", openai.Keys[0].Value.GetValue())

	// Verify encryption at the raw DB level
	var rawValue string
	var rawStatus string
	err = db.Table("config_keys").Select("value").Where("key_id = ?", "key-uuid-1").Scan(&rawValue).Error
	require.NoError(t, err)
	err = db.Table("config_keys").Select("encryption_status").Where("key_id = ?", "key-uuid-1").Scan(&rawStatus).Error
	require.NoError(t, err)

	assert.NotEqual(t, "sk-test-secret-key-12345", rawValue, "key value should be encrypted at rest")
	assert.Equal(t, "encrypted", rawStatus)
}

func TestFullMigration_VirtualKeyCRUD(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled")
	}

	store, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	vk := &tables.TableVirtualKey{
		ID:        "vk-test-001",
		Name:      "test-virtual-key",
		Value:     *schemas.NewSecretVar("vk-secret-value-12345"),
		IsActive:  bifrost.Ptr(true),
		CreatedAt: now,
		UpdatedAt: now,
	}

	err := store.CreateVirtualKey(ctx, vk)
	require.NoError(t, err)

	// Read back
	vks, err := store.GetVirtualKeys(ctx)
	require.NoError(t, err)
	require.Len(t, vks, 1)

	assert.Equal(t, "vk-test-001", vks[0].ID)
	assert.Equal(t, "test-virtual-key", vks[0].Name)
	assert.Equal(t, "vk-secret-value-12345", vks[0].Value.GetValue()) // AfterFind decrypts
	assert.True(t, vks[0].IsActiveValue())

	// Verify encryption at raw DB level
	var rawValue, rawStatus, rawHash string
	err = db.Table("governance_virtual_keys").
		Select("value").Where("id = ?", "vk-test-001").Scan(&rawValue).Error
	require.NoError(t, err)
	err = db.Table("governance_virtual_keys").
		Select("encryption_status").Where("id = ?", "vk-test-001").Scan(&rawStatus).Error
	require.NoError(t, err)
	err = db.Table("governance_virtual_keys").
		Select("value_hash").Where("id = ?", "vk-test-001").Scan(&rawHash).Error
	require.NoError(t, err)

	assert.NotEqual(t, "vk-secret-value-12345", rawValue, "VK value should be encrypted at rest")
	assert.Equal(t, "encrypted", rawStatus)
	assert.NotEmpty(t, rawHash, "value_hash should be populated")
}

func TestFullMigration_MCPClientCRUD(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled")
	}

	store, db := setupFullMigrationDB(t)
	ctx := context.Background()

	clientConfig := &schemas.MCPClientConfig{
		ID:               "mcp-client-001",
		Name:             "test_mcp_server",
		ConnectionType:   schemas.MCPConnectionTypeSSE,
		ConnectionString: schemas.NewSecretVar("https://mcp.example.com/sse"),
		ToolsToExecute:   schemas.WhiteList{"*"},
	}

	err := store.CreateMCPClientConfig(ctx, clientConfig)
	require.NoError(t, err)

	// Read back through store
	mcpClient, err := store.GetMCPClientByName(ctx, "test_mcp_server")
	require.NoError(t, err)
	require.NotNil(t, mcpClient)

	assert.Equal(t, "mcp-client-001", mcpClient.ClientID)
	assert.Equal(t, "test_mcp_server", mcpClient.Name)
	assert.Equal(t, "sse", mcpClient.ConnectionType)
	assert.Equal(t, "https://mcp.example.com/sse", mcpClient.ConnectionString.GetValue())

	// Verify encryption at raw DB level
	var rawConnStr, rawStatus string
	err = db.Table("config_mcp_clients").
		Select("connection_string").Where("client_id = ?", "mcp-client-001").Scan(&rawConnStr).Error
	require.NoError(t, err)
	err = db.Table("config_mcp_clients").
		Select("encryption_status").Where("client_id = ?", "mcp-client-001").Scan(&rawStatus).Error
	require.NoError(t, err)

	assert.NotEqual(t, "https://mcp.example.com/sse", rawConnStr, "connection string should be encrypted at rest")
	assert.Equal(t, "encrypted", rawStatus)
}

func TestFullMigration_EncryptPlaintextRows(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled")
	}

	store, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Insert a provider first (FK for config_keys)
	err := db.Exec(`INSERT INTO config_providers (name, encryption_status, created_at, updated_at)
		VALUES (?, 'plain_text', ?, ?)`, "openai", now, now).Error
	require.NoError(t, err)

	// Get the provider ID
	var providerID uint
	err = db.Table("config_providers").Select("id").Where("name = ?", "openai").Scan(&providerID).Error
	require.NoError(t, err)

	// Insert plaintext key (bypassing GORM hooks)
	err = db.Exec(`INSERT INTO config_keys (name, provider_id, provider, key_id, value, models_json,
		encryption_status, created_at, updated_at)
		VALUES (?, ?, 'openai', ?, ?, '["*"]', 'plain_text', ?, ?)`,
		"plaintext-key", providerID, "pk-1", "sk-plaintext-secret", now, now).Error
	require.NoError(t, err)

	// Insert plaintext virtual key
	err = db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, encryption_status, created_at, updated_at)
		VALUES (?, ?, ?, true, 'plain_text', ?, ?)`,
		"vk-plain-1", "plaintext-vk", "vk-plain-secret", now, now).Error
	require.NoError(t, err)

	// Verify they are plaintext before encryption
	var preStatus string
	err = db.Table("config_keys").Select("encryption_status").Where("key_id = ?", "pk-1").Scan(&preStatus).Error
	require.NoError(t, err)
	assert.Equal(t, "plain_text", preStatus)

	// Run the encryption pass
	err = store.EncryptPlaintextRows(ctx)
	require.NoError(t, err)

	// Verify raw DB: encryption_status changed, values differ
	var rawKeyValue, rawKeyStatus string
	err = db.Table("config_keys").Select("value").Where("key_id = ?", "pk-1").Scan(&rawKeyValue).Error
	require.NoError(t, err)
	err = db.Table("config_keys").Select("encryption_status").Where("key_id = ?", "pk-1").Scan(&rawKeyStatus).Error
	require.NoError(t, err)
	assert.NotEqual(t, "sk-plaintext-secret", rawKeyValue, "key should be encrypted after EncryptPlaintextRows")
	assert.Equal(t, "encrypted", rawKeyStatus)

	var rawVKValue, rawVKStatus string
	err = db.Table("governance_virtual_keys").Select("value").Where("id = ?", "vk-plain-1").Scan(&rawVKValue).Error
	require.NoError(t, err)
	err = db.Table("governance_virtual_keys").Select("encryption_status").Where("id = ?", "vk-plain-1").Scan(&rawVKStatus).Error
	require.NoError(t, err)
	assert.NotEqual(t, "vk-plain-secret", rawVKValue, "VK should be encrypted after EncryptPlaintextRows")
	assert.Equal(t, "encrypted", rawVKStatus)

	// Verify GORM read decrypts correctly
	var key tables.TableKey
	err = db.Where("key_id = ?", "pk-1").First(&key).Error
	require.NoError(t, err)
	assert.Equal(t, "sk-plaintext-secret", key.Value.Val)

	var vk tables.TableVirtualKey
	err = db.Where("id = ?", "vk-plain-1").First(&vk).Error
	require.NoError(t, err)
	assert.Equal(t, "vk-plain-secret", vk.Value.Val)
}

func TestFullMigration_EndToEnd(t *testing.T) {
	if !encrypt.IsEnabled() {
		t.Skip("encryption not enabled")
	}

	store, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Add two providers with keys
	for _, p := range []struct {
		provider string
		keyID    string
		keyName  string
		keyValue string
	}{
		{"openai", "key-oa-1", "openai-key", "sk-openai-secret"},
		{"anthropic", "key-ant-1", "anthropic-key", "sk-anthropic-secret"},
	} {
		err := store.AddProvider(ctx, schemas.ModelProvider(p.provider), ProviderConfig{
			Keys: []schemas.Key{{
				ID:     p.keyID,
				Name:   p.keyName,
				Value:  *schemas.NewSecretVar(p.keyValue),
				Models: schemas.WhiteList{"*"},
				Weight: 1.0,
			}},
		})
		require.NoError(t, err, "AddProvider %s", p.provider)
	}

	// Add virtual keys
	for _, vk := range []struct {
		id, name, value string
	}{
		{"vk-1", "vk-alpha", "vk-alpha-secret"},
		{"vk-2", "vk-beta", "vk-beta-secret"},
	} {
		err := store.CreateVirtualKey(ctx, &tables.TableVirtualKey{
			ID: vk.id, Name: vk.name, Value: *schemas.NewSecretVar(vk.value),
			IsActive: bifrost.Ptr(true), CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err, "CreateVirtualKey %s", vk.name)
	}

	// Add MCP client
	err := store.CreateMCPClientConfig(ctx, &schemas.MCPClientConfig{
		ID:               "mcp-e2e-1",
		Name:             "e2e_mcp_client",
		ConnectionType:   schemas.MCPConnectionTypeSSE,
		ConnectionString: schemas.NewSecretVar("https://mcp.e2e.test/sse"),
		ToolsToExecute:   schemas.WhiteList{"*"},
	})
	require.NoError(t, err)

	// Verify providers
	providers, err := store.GetProvidersConfig(ctx)
	require.NoError(t, err)
	assert.Len(t, providers, 2)
	assert.Contains(t, providers, schemas.ModelProvider("openai"))
	assert.Contains(t, providers, schemas.ModelProvider("anthropic"))
	assert.Equal(t, "sk-openai-secret", providers["openai"].Keys[0].Value.GetValue())
	assert.Equal(t, "sk-anthropic-secret", providers["anthropic"].Keys[0].Value.GetValue())

	// Verify virtual keys
	vks, err := store.GetVirtualKeys(ctx)
	require.NoError(t, err)
	assert.Len(t, vks, 2)

	// Verify MCP client
	mcpClient, err := store.GetMCPClientByName(ctx, "e2e_mcp_client")
	require.NoError(t, err)
	assert.Equal(t, "https://mcp.e2e.test/sse", mcpClient.ConnectionString.GetValue())

	// Verify all sensitive data is encrypted at the raw DB level
	type encCheck struct {
		table, column, whereCol, whereVal, plaintext string
	}
	checks := []encCheck{
		{"config_keys", "value", "key_id", "key-oa-1", "sk-openai-secret"},
		{"config_keys", "value", "key_id", "key-ant-1", "sk-anthropic-secret"},
		{"governance_virtual_keys", "value", "id", "vk-1", "vk-alpha-secret"},
		{"governance_virtual_keys", "value", "id", "vk-2", "vk-beta-secret"},
		{"config_mcp_clients", "connection_string", "client_id", "mcp-e2e-1", "https://mcp.e2e.test/sse"},
	}
	for _, c := range checks {
		var rawVal string
		err := db.Table(c.table).Select(c.column).Where(fmt.Sprintf("%s = ?", c.whereCol), c.whereVal).Scan(&rawVal).Error
		require.NoError(t, err)
		assert.NotEqual(t, c.plaintext, rawVal, "raw %s.%s for %s=%s should be encrypted",
			c.table, c.column, c.whereCol, c.whereVal)
	}
}

// ============================================================================
// Part 3: Individual complex migration tests
// ============================================================================

// setupPreEncryptionDB runs all migrations up to (but not including) the
// encryption columns migration. This is approximated by running migrationInit
// plus the essential early migrations on a fresh SQLite DB.
func setupPreEncryptionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	ctx := context.Background()
	// Run the initial migration to create core tables
	err = migrationInit(ctx, db, testMigrationLogger)
	require.NoError(t, err)
	// Sessions table is created by a later migration, but required by the
	// encryption columns migration.
	err = migrationAddSessionsTable(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	return db
}

func TestMigrationAddEncryptionColumns(t *testing.T) {
	db := setupPreEncryptionDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Insert rows BEFORE encryption columns exist (they won't have encryption_status)
	err := db.Exec(`INSERT INTO config_providers (name, created_at, updated_at) VALUES (?, ?, ?)`,
		"openai", now, now).Error
	require.NoError(t, err)

	var providerID uint
	err = db.Table("config_providers").Select("id").Where("name = ?", "openai").Scan(&providerID).Error
	require.NoError(t, err)

	err = db.Exec(`INSERT INTO config_keys (name, provider_id, provider, key_id, value, created_at, updated_at)
		VALUES (?, ?, 'openai', ?, ?, ?, ?)`,
		"test-key", providerID, "ek-1", "sk-test", now, now).Error
	require.NoError(t, err)

	err = db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, created_at, updated_at)
		VALUES (?, ?, ?, true, ?, ?)`,
		"vk-enc-1", "enc-vk", "vk-value", now, now).Error
	require.NoError(t, err)

	err = db.Exec(`INSERT INTO sessions (token, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?)`,
		"sess-token", now, now, now).Error
	require.NoError(t, err)

	// Run the encryption columns migration
	err = migrationAddEncryptionColumns(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify encryption_status column exists and is backfilled on all 9 tables
	encTables := []string{
		"config_keys", "governance_virtual_keys", "sessions",
		"oauth_configs", "oauth_tokens", "config_mcp_clients",
		"config_providers", "config_vector_store", "config_plugins",
	}
	for _, table := range encTables {
		var count int64
		err := db.Table(table).Where("encryption_status = 'plain_text'").Count(&count).Error
		if err != nil {
			// Table might be empty, just verify column exists
			assert.True(t, db.Migrator().HasColumn(table, "encryption_status"),
				"encryption_status column should exist on %s", table)
			continue
		}
	}

	// Verify pre-existing rows have encryption_status = 'plain_text'
	var keyStatus string
	err = db.Table("config_keys").Select("encryption_status").Where("key_id = ?", "ek-1").Scan(&keyStatus).Error
	require.NoError(t, err)
	assert.Equal(t, "plain_text", keyStatus)

	var providerStatus string
	err = db.Table("config_providers").Select("encryption_status").Where("name = ?", "openai").Scan(&providerStatus).Error
	require.NoError(t, err)
	assert.Equal(t, "plain_text", providerStatus)

	var vkStatus string
	err = db.Table("governance_virtual_keys").Select("encryption_status").Where("id = ?", "vk-enc-1").Scan(&vkStatus).Error
	require.NoError(t, err)
	assert.Equal(t, "plain_text", vkStatus)

	// Verify value_hash on governance_virtual_keys is NULL (not empty string)
	var rawHash *string
	err = db.Table("governance_virtual_keys").Select("value_hash").Where("id = ?", "vk-enc-1").Scan(&rawHash).Error
	require.NoError(t, err)
	assert.Nil(t, rawHash, "value_hash should be NULL, not empty string")

	// Verify token_hash on sessions is NULL
	var tokenHash *string
	err = db.Table("sessions").Select("token_hash").Where("token = ?", "sess-token").Scan(&tokenHash).Error
	require.NoError(t, err)
	assert.Nil(t, tokenHash, "token_hash should be NULL, not empty string")

	// Idempotency: running again should not error
	err = migrationAddEncryptionColumns(ctx, db, testMigrationLogger)
	require.NoError(t, err)
}

func TestMigrationCleanupMCPClientToolsConfig(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	// Create the MCP clients table using AutoMigrate
	err = db.AutoMigrate(&tables.TableMCPClient{})
	require.NoError(t, err)

	// Create the migrations tracking table
	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)

	now := time.Now()

	// Insert clients with various tools_to_execute_json states
	clients := []struct {
		name  string
		tools string
	}{
		{"client_empty_array", "[]"},
		{"client_empty_string", ""},
		{"client_wildcard", `["*"]`},
		{"client_specific", `["tool1","tool2"]`},
	}

	for i, c := range clients {
		err := db.Exec(`INSERT INTO config_mcp_clients (client_id, name, connection_type, tools_to_execute_json, created_at, updated_at, encryption_status)
			VALUES (?, ?, 'stdio', ?, ?, ?, 'plain_text')`,
			fmt.Sprintf("client-%d", i), c.name, c.tools, now, now).Error
		require.NoError(t, err)
	}

	// Also insert one with NULL tools
	err = db.Exec(`INSERT INTO config_mcp_clients (client_id, name, connection_type, tools_to_execute_json, created_at, updated_at, encryption_status)
		VALUES (?, ?, 'stdio', NULL, ?, ?, 'plain_text')`,
		"client-null", "client_null_tools", now, now).Error
	require.NoError(t, err)

	// Run the cleanup migration
	err = migrationCleanupMCPClientToolsConfig(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify: empty/null → ["*"], existing values preserved
	for _, tc := range []struct {
		name     string
		expected string
	}{
		{"client_empty_array", `["*"]`},
		{"client_empty_string", `["*"]`},
		{"client_null_tools", `["*"]`},
		{"client_wildcard", `["*"]`},
		{"client_specific", `["tool1","tool2"]`},
	} {
		var tools string
		err := db.Table("config_mcp_clients").Select("tools_to_execute_json").
			Where("name = ?", tc.name).Scan(&tools).Error
		require.NoError(t, err)
		assert.Equal(t, tc.expected, tools, "tools for %s", tc.name)
	}
}

func TestMigrationAddConfigHashColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	// Create migrations table
	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)

	// Create tables WITHOUT config_hash column to simulate pre-migration state
	err = db.Exec(`CREATE TABLE config_providers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name VARCHAR(50) NOT NULL UNIQUE,
		network_config_json TEXT,
		concurrency_buffer_json TEXT,
		proxy_config_json TEXT,
		custom_provider_config_json TEXT,
		send_back_raw_request BOOLEAN DEFAULT 0,
		send_back_raw_response BOOLEAN DEFAULT 0,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		encryption_status VARCHAR(20) DEFAULT 'plain_text'
	)`).Error
	require.NoError(t, err)

	err = db.Exec(`CREATE TABLE config_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name VARCHAR(255) NOT NULL,
		key_id VARCHAR(255) NOT NULL UNIQUE,
		provider_id INTEGER NOT NULL,
		provider VARCHAR(50),
		value TEXT NOT NULL,
		models_json TEXT,
		weight REAL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		encryption_status VARCHAR(20) DEFAULT 'plain_text'
	)`).Error
	require.NoError(t, err)

	now := time.Now()

	// Insert provider and key without config_hash
	err = db.Exec(`INSERT INTO config_providers (name, created_at, updated_at, encryption_status)
		VALUES (?, ?, ?, 'plain_text')`, "openai", now, now).Error
	require.NoError(t, err)

	var providerID uint
	err = db.Table("config_providers").Select("id").Where("name = ?", "openai").Scan(&providerID).Error
	require.NoError(t, err)

	err = db.Exec(`INSERT INTO config_keys (name, key_id, provider_id, provider, value, models_json, created_at, updated_at, encryption_status)
		VALUES (?, ?, ?, 'openai', ?, '["*"]', ?, ?, 'plain_text')`,
		"test-key", "ck-1", providerID, "sk-test-value", now, now).Error
	require.NoError(t, err)

	// Run the migration
	err = migrationAddConfigHashColumn(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify config_hash column exists
	assert.True(t, db.Migrator().HasColumn(&tables.TableProvider{}, "config_hash"))
	assert.True(t, db.Migrator().HasColumn(&tables.TableKey{}, "config_hash"))

	// Verify hashes are non-empty
	var providerHash string
	err = db.Table("config_providers").Select("config_hash").Where("name = ?", "openai").Scan(&providerHash).Error
	require.NoError(t, err)
	assert.NotEmpty(t, providerHash, "provider config_hash should be backfilled")

	var keyHash string
	err = db.Table("config_keys").Select("config_hash").Where("key_id = ?", "ck-1").Scan(&keyHash).Error
	require.NoError(t, err)
	assert.NotEmpty(t, keyHash, "key config_hash should be backfilled")

	// Verify provider hash matches expected computation
	expectedProviderConfig := ProviderConfig{}
	expectedHash, err := expectedProviderConfig.GenerateConfigHash("openai")
	require.NoError(t, err)
	assert.Equal(t, expectedHash, providerHash, "provider hash should match GenerateConfigHash output")
}

func TestMigrationBackfillEmptyVirtualKeyConfigs(t *testing.T) {
	// Use full migration DB to get all tables, then insert test data
	store, db := setupFullMigrationDB(t)
	_ = store
	ctx := context.Background()
	now := time.Now()

	// Clear the migration tracking for the specific migration so it runs again
	db.Exec(`DELETE FROM migrations WHERE id = 'backfill_empty_virtual_key_configs'`)

	// Create a provider
	err := db.Exec(`INSERT INTO config_providers (name, encryption_status, created_at, updated_at)
		VALUES ('openai', 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	// Create a virtual key with NO provider configs
	err = db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, encryption_status, created_at, updated_at)
		VALUES ('vk-backfill-1', 'backfill-vk', 'vk-value', true, 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	// Verify no provider configs exist
	var provConfigCount int64
	db.Table("governance_virtual_key_provider_configs").Where("virtual_key_id = ?", "vk-backfill-1").Count(&provConfigCount)
	assert.Equal(t, int64(0), provConfigCount, "should have no provider configs before migration")

	// Run the migration
	err = migrationBackfillEmptyVirtualKeyConfigs(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify provider configs were created for the VK
	db.Table("governance_virtual_key_provider_configs").Where("virtual_key_id = ?", "vk-backfill-1").Count(&provConfigCount)
	assert.Equal(t, int64(1), provConfigCount, "should have 1 provider config after backfill (one for openai)")

	// Verify the provider config has correct defaults
	var allowAllKeys bool
	err = db.Table("governance_virtual_key_provider_configs").
		Select("allow_all_keys").Where("virtual_key_id = ?", "vk-backfill-1").Scan(&allowAllKeys).Error
	require.NoError(t, err)
	assert.True(t, allowAllKeys, "backfilled provider config should have AllowAllKeys=true")

	// Verify config_hash was recomputed (non-empty)
	var vkHash string
	err = db.Table("governance_virtual_keys").Select("config_hash").
		Where("id = ?", "vk-backfill-1").Scan(&vkHash).Error
	require.NoError(t, err)
	assert.NotEmpty(t, vkHash, "VK config_hash should be recomputed after backfill")
}

func TestMigrationBackfillVirtualKeyAllowAllProvidersHash(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Clear the migration tracking so the backfill runs against our seeded row.
	db.Exec(`DELETE FROM migrations WHERE id = 'backfill_vk_allow_all_providers_hash'`)

	// Existing VK with allow_all_providers=false and a stale config_hash from before the field
	// joined GenerateVirtualKeyHash.
	err := db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, allow_all_providers, config_hash, encryption_status, created_at, updated_at)
		VALUES ('vk-aap-hash-1', 'aap-vk', 'vk-value', true, false, 'stale_hash', 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	err = migrationBackfillVirtualKeyAllowAllProvidersHash(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	var vkHash string
	err = db.Table("governance_virtual_keys").Select("config_hash").
		Where("id = ?", "vk-aap-hash-1").Scan(&vkHash).Error
	require.NoError(t, err)
	assert.NotEqual(t, "stale_hash", vkHash, "config_hash should be recomputed, not the stale value")

	// The recomputed hash must match GenerateVirtualKeyHash for the row as read back.
	var vk tables.TableVirtualKey
	err = db.Preload("ProviderConfigs").Preload("ProviderConfigs.Keys").Preload("MCPConfigs").
		Where("id = ?", "vk-aap-hash-1").First(&vk).Error
	require.NoError(t, err)
	expected, err := GenerateVirtualKeyHash(vk)
	require.NoError(t, err)
	assert.Equal(t, expected, vkHash, "config_hash should match GenerateVirtualKeyHash output")
}

func TestTriggerMigrationsAddsVKProviderConfigBlacklistColumnBeforeBackfill(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Simulate an existing DB that has provider configs but predates the
	// provider-config blacklisted_models migration. This matches upgrades where
	// backfill_empty_virtual_key_configs has not run yet.
	require.NoError(t, db.Exec(`DELETE FROM migrations WHERE id IN (
		'add_vk_provider_config_blacklisted_models_column',
		'backfill_vk_provider_config_blacklisted_models',
		'backfill_empty_virtual_key_configs'
	)`).Error)

	if db.Migrator().HasColumn(&tables.TableVirtualKeyProviderConfig{}, "blacklisted_models") {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableVirtualKeyProviderConfig{}, "blacklisted_models"))
	}
	require.False(t, db.Migrator().HasColumn(&tables.TableVirtualKeyProviderConfig{}, "blacklisted_models"))

	err := db.Exec(`INSERT INTO config_providers (name, encryption_status, created_at, updated_at)
		VALUES ('legacy-openai', 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	err = db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, encryption_status, created_at, updated_at)
		VALUES ('vk-legacy-missing-blacklist', 'legacy-vk', 'vk-value', true, 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	err = triggerMigrations(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	require.True(t, db.Migrator().HasColumn(&tables.TableVirtualKeyProviderConfig{}, "blacklisted_models"))

	var providerConfigCount int64
	err = db.Table("governance_virtual_key_provider_configs").
		Where("virtual_key_id = ?", "vk-legacy-missing-blacklist").
		Count(&providerConfigCount).Error
	require.NoError(t, err)
	assert.Equal(t, int64(1), providerConfigCount)

	var blacklistedModels string
	err = db.Table("governance_virtual_key_provider_configs").
		Select("blacklisted_models").
		Where("virtual_key_id = ?", "vk-legacy-missing-blacklist").
		Scan(&blacklistedModels).Error
	require.NoError(t, err)
	assert.Equal(t, "[]", blacklistedModels)
}

func TestMigrationBackfillAllowedModelsWildcard(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Clear migration tracking so it runs again
	db.Exec(`DELETE FROM migrations WHERE id = 'backfill_allowed_models_wildcard'`)

	// Create a provider
	err := db.Exec(`INSERT INTO config_providers (name, encryption_status, created_at, updated_at)
		VALUES ('openai', 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	var providerID uint
	err = db.Table("config_providers").Select("id").Where("name = ?", "openai").Scan(&providerID).Error
	require.NoError(t, err)

	// Create a key with empty models_json
	err = db.Exec(`INSERT INTO config_keys (name, key_id, provider_id, provider, value, models_json,
		encryption_status, created_at, updated_at)
		VALUES ('empty-models-key', 'emk-1', ?, 'openai', 'sk-test', '[]', 'plain_text', ?, ?)`,
		providerID, now, now).Error
	require.NoError(t, err)

	// Create a VK
	err = db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, encryption_status, created_at, updated_at)
		VALUES ('vk-wildcard-1', 'wildcard-vk', 'vk-val', true, 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	// Create a provider config with empty allowed_models
	err = db.Exec(`INSERT INTO governance_virtual_key_provider_configs (virtual_key_id, provider, allowed_models, allow_all_keys)
		VALUES ('vk-wildcard-1', 'openai', '[]', true)`).Error
	require.NoError(t, err)

	// Run the migration
	err = migrationBackfillAllowedModelsWildcard(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify provider config allowed_models changed to ["*"]
	var allowedModels string
	err = db.Table("governance_virtual_key_provider_configs").
		Select("allowed_models").Where("virtual_key_id = ?", "vk-wildcard-1").Scan(&allowedModels).Error
	require.NoError(t, err)
	assert.Equal(t, `["*"]`, allowedModels, "allowed_models should be backfilled to wildcard")

	// Verify key models_json changed to ["*"]
	var modelsJSON string
	err = db.Table("config_keys").Select("models_json").Where("key_id = ?", "emk-1").Scan(&modelsJSON).Error
	require.NoError(t, err)
	assert.Equal(t, `["*"]`, modelsJSON, "models_json should be backfilled to wildcard")

	// Verify key config_hash was recomputed
	var keyHash string
	err = db.Table("config_keys").Select("config_hash").Where("key_id = ?", "emk-1").Scan(&keyHash).Error
	require.NoError(t, err)
	assert.NotEmpty(t, keyHash, "key config_hash should be recomputed")
}

// TestProviderConfigWildcardRoundTrip verifies the current write path persists a
// WhiteList/BlackList wildcard as the JSON array '["*"]' (never the bare byte '*')
// and reads it back intact — the round-trip that issue #4318's corrupted rows broke.
func TestProviderConfigWildcardRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualKeyProviderConfig{}))

	pc := tables.TableVirtualKeyProviderConfig{
		VirtualKeyID:      "vk-roundtrip",
		Provider:          "zai",
		AllowedModels:     schemas.WhiteList{"*"},
		BlacklistedModels: schemas.BlackList{"*"},
	}
	require.NoError(t, db.Create(&pc).Error)

	// Raw column bytes must be the JSON array, not the bare character.
	var allowedRaw, blacklistedRaw string
	require.NoError(t, db.Table("governance_virtual_key_provider_configs").
		Select("allowed_models").Where("virtual_key_id = ?", "vk-roundtrip").Scan(&allowedRaw).Error)
	require.NoError(t, db.Table("governance_virtual_key_provider_configs").
		Select("blacklisted_models").Where("virtual_key_id = ?", "vk-roundtrip").Scan(&blacklistedRaw).Error)
	assert.Equal(t, `["*"]`, allowedRaw)
	assert.Equal(t, `["*"]`, blacklistedRaw)

	// Model read must deserialize cleanly back to the wildcard slice.
	var got tables.TableVirtualKeyProviderConfig
	require.NoError(t, db.Where("virtual_key_id = ?", "vk-roundtrip").First(&got).Error)
	assert.True(t, got.AllowedModels.IsUnrestricted())
	assert.True(t, got.BlacklistedModels.IsBlockAll())
}

// TestMigrationRepairBareWildcardAllowedModels verifies the repair migration heals
// legacy rows whose allowed_models / blacklisted_models column holds the bare byte
// '*' (issue #4318). Without the repair, loading such a row aborts the GORM json
// deserializer and poisons the provider admin surface.
func TestMigrationRepairBareWildcardAllowedModels(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Clear migration tracking so it runs again against the seeded rows.
	db.Exec(`DELETE FROM migrations WHERE id = 'repair_bare_wildcard_allowed_models'`)

	err := db.Exec(`INSERT INTO governance_virtual_keys (id, name, value, is_active, encryption_status, created_at, updated_at)
		VALUES ('vk-bare-1', 'bare-vk', 'vk-val', true, 'plain_text', ?, ?)`, now, now).Error
	require.NoError(t, err)

	// Row A: corrupted allowed_models, valid blacklisted_models.
	err = db.Exec(`INSERT INTO governance_virtual_key_provider_configs (virtual_key_id, provider, allowed_models, blacklisted_models, allow_all_keys)
		VALUES ('vk-bare-1', 'zai', '*', '[]', true)`).Error
	require.NoError(t, err)
	// Row B: valid allowed_models, corrupted blacklisted_models.
	err = db.Exec(`INSERT INTO governance_virtual_key_provider_configs (virtual_key_id, provider, allowed_models, blacklisted_models, allow_all_keys)
		VALUES ('vk-bare-1', 'openai', '["*"]', '*', true)`).Error
	require.NoError(t, err)

	// Pre-condition: the bare '*' rows cannot be loaded through the GORM model.
	var pre []tables.TableVirtualKeyProviderConfig
	preErr := db.Where("virtual_key_id = ?", "vk-bare-1").Find(&pre).Error
	require.Error(t, preErr, "bare '*' rows should fail to deserialize before repair")

	require.NoError(t, migrationRepairBareWildcardAllowedModels(ctx, db, testMigrationLogger))

	// Both columns are now canonical JSON arrays.
	var allowedA, blacklistedB string
	require.NoError(t, db.Table("governance_virtual_key_provider_configs").
		Select("allowed_models").Where("virtual_key_id = ? AND provider = ?", "vk-bare-1", "zai").Scan(&allowedA).Error)
	require.NoError(t, db.Table("governance_virtual_key_provider_configs").
		Select("blacklisted_models").Where("virtual_key_id = ? AND provider = ?", "vk-bare-1", "openai").Scan(&blacklistedB).Error)
	assert.Equal(t, `["*"]`, allowedA, "bare '*' allowed_models should be repaired to wildcard array")
	assert.Equal(t, `["*"]`, blacklistedB, "bare '*' blacklisted_models should be repaired to wildcard array")

	// Post-condition: the rows now load cleanly through the GORM model.
	var post []tables.TableVirtualKeyProviderConfig
	require.NoError(t, db.Where("virtual_key_id = ?", "vk-bare-1").Find(&post).Error,
		"repaired rows should deserialize without error")
	require.Len(t, post, 2)
	byProvider := map[string]tables.TableVirtualKeyProviderConfig{}
	for _, pc := range post {
		byProvider[pc.Provider] = pc
	}
	assert.True(t, byProvider["zai"].AllowedModels.IsUnrestricted(), "repaired allowed_models should be unrestricted")
	assert.True(t, byProvider["openai"].BlacklistedModels.IsBlockAll(), "repaired blacklisted_models should block all")
}

func TestMigrationRemoveServerPrefixFromMCPTools(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Clear migration tracking so it runs again
	db.Exec(`DELETE FROM migrations WHERE id = 'remove_server_prefix_from_mcp_tools'`)

	// Insert an MCP client with prefixed tool names
	toolsJSON, _ := json.Marshal([]string{"my_server_tool1", "my_server_tool2", "standalone_tool"})
	autoToolsJSON, _ := json.Marshal([]string{"my_server_auto1"})

	err := db.Exec(`INSERT INTO config_mcp_clients (client_id, name, connection_type,
		tools_to_execute_json, tools_to_auto_execute_json, encryption_status, created_at, updated_at)
		VALUES (?, ?, 'stdio', ?, ?, 'plain_text', ?, ?)`,
		"mcp-prefix-1", "my_server", string(toolsJSON), string(autoToolsJSON), now, now).Error
	require.NoError(t, err)

	// Run the migration
	err = migrationRemoveServerPrefixFromMCPTools(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify tools had prefixes stripped
	var resultToolsJSON string
	err = db.Table("config_mcp_clients").Select("tools_to_execute_json").
		Where("client_id = ?", "mcp-prefix-1").Scan(&resultToolsJSON).Error
	require.NoError(t, err)

	var resultTools []string
	err = json.Unmarshal([]byte(resultToolsJSON), &resultTools)
	require.NoError(t, err)

	assert.Contains(t, resultTools, "tool1", "should have stripped my_server_ prefix")
	assert.Contains(t, resultTools, "tool2", "should have stripped my_server_ prefix")
	assert.Contains(t, resultTools, "standalone_tool", "should preserve non-prefixed tool")
	assert.NotContains(t, resultTools, "my_server_tool1", "original prefixed name should be gone")

	// Verify auto-execute tools
	var resultAutoToolsJSON string
	err = db.Table("config_mcp_clients").Select("tools_to_auto_execute_json").
		Where("client_id = ?", "mcp-prefix-1").Scan(&resultAutoToolsJSON).Error
	require.NoError(t, err)

	var resultAutoTools []string
	err = json.Unmarshal([]byte(resultAutoToolsJSON), &resultAutoTools)
	require.NoError(t, err)
	assert.Contains(t, resultAutoTools, "auto1", "should have stripped my_server_ prefix from auto tools")
}

func TestMigrationRemoveServerPrefixFromMCPTools_Collision(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	ctx := context.Background()
	now := time.Now()

	// Clear migration tracking
	db.Exec(`DELETE FROM migrations WHERE id = 'remove_server_prefix_from_mcp_tools'`)

	// Client where stripping the prefix would cause a collision:
	// "srv_read" (prefixed) → "read", but "read" already exists in the list
	toolsJSON, _ := json.Marshal([]string{"srv_read", "read"})
	err := db.Exec(`INSERT INTO config_mcp_clients (client_id, name, connection_type,
		tools_to_execute_json, encryption_status, created_at, updated_at)
		VALUES (?, ?, 'stdio', ?, 'plain_text', ?, ?)`,
		"mcp-collision", "srv", string(toolsJSON), now, now).Error
	require.NoError(t, err)

	// Run the migration — should not error, collision is handled
	err = migrationRemoveServerPrefixFromMCPTools(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// The collision drops the duplicate, keeping "read" once
	var resultJSON string
	err = db.Table("config_mcp_clients").Select("tools_to_execute_json").
		Where("client_id = ?", "mcp-collision").Scan(&resultJSON).Error
	require.NoError(t, err)

	var resultTools []string
	err = json.Unmarshal([]byte(resultJSON), &resultTools)
	require.NoError(t, err)
	assert.Contains(t, resultTools, "read")
	// Should not have duplicates
	readCount := 0
	for _, tool := range resultTools {
		if tool == "read" {
			readCount++
		}
	}
	assert.Equal(t, 1, readCount, "should deduplicate on collision")
}

func TestMigrationReplaceEnableLiteLLMWithCompatColumns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	// Create migrations table
	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)

	// Create config_client from the GORM model so DropColumn works (SQLite recreates
	// the table from model columns). Then add the legacy column on top.
	err = db.AutoMigrate(&tables.TableClientConfig{})
	require.NoError(t, err)

	// Add the legacy column that the migration will read and drop
	err = db.Exec(`ALTER TABLE config_client ADD COLUMN enable_litellm_fallbacks BOOLEAN DEFAULT 0`).Error
	require.NoError(t, err)

	now := time.Now()

	// Insert a row with enable_litellm_fallbacks = true
	err = db.Exec(`INSERT INTO config_client (enable_litellm_fallbacks, created_at, updated_at)
		VALUES (1, ?, ?)`, now, now).Error
	require.NoError(t, err)

	// Insert a row with enable_litellm_fallbacks = false
	err = db.Exec(`INSERT INTO config_client (enable_litellm_fallbacks, created_at, updated_at)
		VALUES (0, ?, ?)`, now, now).Error
	require.NoError(t, err)

	// Run the migration
	err = migrationReplaceEnableLiteLLMWithCompatColumns(ctx, db, testMigrationLogger)
	require.NoError(t, err)

	// Verify new columns exist
	mgr := db.Migrator()
	assert.True(t, mgr.HasColumn(&tables.TableClientConfig{}, "compat_convert_text_to_chat"))
	assert.True(t, mgr.HasColumn(&tables.TableClientConfig{}, "compat_convert_chat_to_responses"))
	assert.True(t, mgr.HasColumn(&tables.TableClientConfig{}, "compat_should_drop_params"))
	assert.True(t, mgr.HasColumn(&tables.TableClientConfig{}, "compat_should_convert_params"))

	// Verify data migration: row 1 had litellm=true → compat_convert_text_to_chat=true
	type compatRow struct {
		ID                        uint
		CompatConvertTextToChat   bool `gorm:"column:compat_convert_text_to_chat"`
		CompatShouldConvertParams bool `gorm:"column:compat_should_convert_params"`
	}
	var rows []compatRow
	err = db.Table("config_client").
		Select("id, compat_convert_text_to_chat, compat_should_convert_params").
		Order("id").Find(&rows).Error
	require.NoError(t, err)
	require.Len(t, rows, 2)

	assert.True(t, rows[0].CompatConvertTextToChat, "row with litellm=true should have compat_convert_text_to_chat=true")
	assert.False(t, rows[0].CompatShouldConvertParams, "compat_should_convert_params should default to false")

	assert.False(t, rows[1].CompatConvertTextToChat, "row with litellm=false should have compat_convert_text_to_chat=false")
	assert.False(t, rows[1].CompatShouldConvertParams, "compat_should_convert_params should default to false")
}

func TestMigrationAddCompatAzureDeepseekColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)

	err = db.AutoMigrate(&tables.TableClientConfig{})
	require.NoError(t, err)

	// Simulate the pre-migration schema
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "compat_azure_deepseek"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "compat_azure_deepseek"))

	now := time.Now()
	err = db.Exec(`INSERT INTO config_client (created_at, updated_at) VALUES (?, ?)`, now, now).Error
	require.NoError(t, err)

	require.NoError(t, migrationAddCompatAzureDeepseekColumn(ctx, db, testMigrationLogger))

	assert.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "compat_azure_deepseek"))

	// The conversion was unconditional before the toggle existed, so existing rows are
	// backfilled to true rather than picking up the column's false default.
	type row struct {
		CompatAzureDeepseek bool `gorm:"column:compat_azure_deepseek"`
	}
	var rows []row
	err = db.Table("config_client").Select("compat_azure_deepseek").Order("id").Find(&rows).Error
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].CompatAzureDeepseek, "existing rows must keep the conversion enabled")
}

func TestMigrationAddMCPCodeModeLimitsClientColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error)
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))

	// Simulate the pre-migration schema
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "mcp_code_mode_limits_json"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "mcp_code_mode_limits_json"))

	require.NoError(t, migrationAddMCPCodeModeLimitsClientColumn(ctx, db, testMigrationLogger))
	assert.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "mcp_code_mode_limits_json"))

	// Idempotent: a second run on an already-migrated schema is a no-op.
	require.NoError(t, migrationAddMCPCodeModeLimitsClientColumn(ctx, db, testMigrationLogger))
}

func TestMigrationAddCompatForceReasoningOnlyModelsToResponsesColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))

	// Simulate the pre-migration schema
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "compat_force_reasoning_only_models_to_responses"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "compat_force_reasoning_only_models_to_responses"))

	now := time.Now()
	require.NoError(t, db.Exec(`INSERT INTO config_client (created_at, updated_at) VALUES (?, ?)`, now, now).Error)

	require.NoError(t, migrationAddCompatForceReasoningOnlyModelsToResponsesColumn(ctx, db, testMigrationLogger))
	assert.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "compat_force_reasoning_only_models_to_responses"))
	require.NoError(t, migrationBackfillCompatForceReasoningOnlyModelsToResponses(ctx, db, testMigrationLogger))

	type row struct {
		CompatForceReasoningOnlyModelsToResponses bool `gorm:"column:compat_force_reasoning_only_models_to_responses"`
	}
	var rows []row
	require.NoError(t, db.Table("config_client").Select("compat_force_reasoning_only_models_to_responses").Order("id").Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].CompatForceReasoningOnlyModelsToResponses, "existing rows must have the toggle on by default")
}

func TestMigrationAddCompatConvertDecisionToResponsesColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	ctx := context.Background()

	err = db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))

	// Simulate the pre-migration schema
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "compat_convert_decision_to_responses"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "compat_convert_decision_to_responses"))

	now := time.Now()
	require.NoError(t, db.Exec(`INSERT INTO config_client (created_at, updated_at) VALUES (?, ?)`, now, now).Error)

	require.NoError(t, migrationAddCompatConvertDecisionToResponsesColumn(ctx, db, testMigrationLogger))
	assert.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "compat_convert_decision_to_responses"))

	type row struct {
		CompatConvertDecisionToResponses bool `gorm:"column:compat_convert_decision_to_responses"`
	}
	var rows []row
	require.NoError(t, db.Table("config_client").Select("compat_convert_decision_to_responses").Order("id").Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].CompatConvertDecisionToResponses, "existing rows must keep decision emulation off")
}

// setupCalendarAlignedPreMigrationDB creates a SQLite DB with governance_virtual_keys,
// governance_budgets, and governance_rate_limits tables, then drops the calendar_aligned
// column from budgets and rate_limits to simulate the pre-migration schema state.
func setupCalendarAlignedPreMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err, "Failed to create test database")

	require.NoError(t, db.AutoMigrate(
		&tables.TableBudget{},
		&tables.TableRateLimit{},
		&tables.TableVirtualKey{},
	), "Failed to auto-migrate governance tables")

	// Simulate pre-migration state: drop calendar_aligned from budgets and rate_limits.
	mig := db.Migrator()
	if mig.HasColumn(&tables.TableBudget{}, "calendar_aligned") {
		require.NoError(t, mig.DropColumn(&tables.TableBudget{}, "calendar_aligned"))
	}
	if mig.HasColumn(&tables.TableRateLimit{}, "calendar_aligned") {
		require.NoError(t, mig.DropColumn(&tables.TableRateLimit{}, "calendar_aligned"))
	}

	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error)
	return db
}

// insertBudgetRaw inserts a budget via raw SQL so the pre-migration table (without
// calendar_aligned) accepts the row without tripping BeforeSave hooks.
func insertBudgetRaw(t *testing.T, db *gorm.DB, id string, vkID *string) {
	t.Helper()
	now := time.Now()
	err := db.Exec(`
		INSERT INTO governance_budgets
		  (id, max_limit, reset_duration, last_reset, current_usage, virtual_key_id, config_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, '', ?, ?)
	`, id, 100.0, "1d", now, vkID, now, now).Error
	require.NoError(t, err, "Failed to insert budget %s", id)
}

// insertRateLimitRaw inserts a rate_limit via raw SQL mirroring insertBudgetRaw.
func insertRateLimitRaw(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	now := time.Now()
	tokenMax := int64(1000)
	tokenDur := "1h"
	err := db.Exec(`
		INSERT INTO governance_rate_limits
		  (id, token_max_limit, token_reset_duration, token_current_usage, token_last_reset,
		   request_current_usage, request_last_reset, config_hash, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, 0, ?, '', ?, ?)
	`, id, tokenMax, tokenDur, now, now, now, now).Error
	require.NoError(t, err, "Failed to insert rate limit %s", id)
}

// insertVKRaw inserts a virtual key via raw SQL so we can set rate_limit_id without
// running the VK BeforeSave hook (which handles encryption).
func insertVKRaw(t *testing.T, db *gorm.DB, id, name, value string, rateLimitID *string, calendarAligned bool) {
	t.Helper()
	now := time.Now()
	err := db.Exec(`
		INSERT INTO governance_virtual_keys
		  (id, name, value, is_active, rate_limit_id, calendar_aligned, encryption_status, value_hash, config_hash, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?, 'plain_text', ?, '', ?, ?)
	`, id, name, value, rateLimitID, calendarAligned, id+"-hash", now, now).Error
	require.NoError(t, err, "Failed to insert virtual key %s", id)
}

func setupLegacyBudgetOwnerMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err, "Failed to create test database")

	require.NoError(t, db.AutoMigrate(
		&tables.TableBudget{},
		&tables.TableVirtualKey{},
		&tables.TableVirtualKeyProviderConfig{},
		&tables.TableTeam{},
	), "Failed to auto-migrate legacy budget owner test tables")

	// Add legacy budget_id columns WITH foreign key constraints, matching production
	// schema. The FK definitions are what caused SQLite to reject ALTER TABLE DROP
	// COLUMN and what triggered FK reference corruption via rename propagation.
	require.NoError(t, db.Exec(`ALTER TABLE governance_virtual_keys ADD COLUMN budget_id VARCHAR(255) REFERENCES governance_budgets(id)`).Error)
	require.NoError(t, db.Exec(`ALTER TABLE governance_virtual_key_provider_configs ADD COLUMN budget_id VARCHAR(255) REFERENCES governance_budgets(id)`).Error)
	require.NoError(t, db.Exec(`ALTER TABLE governance_teams ADD COLUMN budget_id VARCHAR(255) REFERENCES governance_budgets(id)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`).Error)
	return db
}

func insertProviderConfigRaw(t *testing.T, db *gorm.DB, id uint, virtualKeyID, provider string) {
	t.Helper()
	err := db.Exec(`
		INSERT INTO governance_virtual_key_provider_configs
		  (id, virtual_key_id, provider, allowed_models, allow_all_keys)
		VALUES (?, ?, ?, '[]', 1)
	`, id, virtualKeyID, provider).Error
	require.NoError(t, err, "Failed to insert provider config %d", id)
}

func insertTeamRaw(t *testing.T, db *gorm.DB, id, name string) {
	t.Helper()
	now := time.Now()
	err := db.Exec(`
		INSERT INTO governance_teams
		  (id, name, config_hash, created_at, updated_at)
		VALUES (?, ?, '', ?, ?)
	`, id, name, now, now).Error
	require.NoError(t, err, "Failed to insert team %s", id)
}

// TestMigrationCalendarAligned_AddColumnsAndBackfill exercises the full migration:
// column addition on governance_budgets and governance_rate_limits, plus backfill
// of calendar_aligned=true for rows attached to any virtual key. Rows NOT attached
// to a VK must remain at the default (false).
func TestMigrationCalendarAligned_AddColumnsAndBackfill(t *testing.T) {
	db := setupCalendarAlignedPreMigrationDB(t)
	ctx := context.Background()

	// Pre-condition: columns should be absent before running the migration.
	mig := db.Migrator()
	assert.False(t, mig.HasColumn(&tables.TableBudget{}, "calendar_aligned"),
		"calendar_aligned should NOT exist on budgets before migration")
	assert.False(t, mig.HasColumn(&tables.TableRateLimit{}, "calendar_aligned"),
		"calendar_aligned should NOT exist on rate_limits before migration")

	// Seed: VK-aligned carries the legacy calendar_aligned=true flag; its rate limit
	// and both attached budgets SHOULD be backfilled.
	insertRateLimitRaw(t, db, "rl-aligned")
	vkAlignedID := "vk-aligned"
	rlAlignedID := "rl-aligned"
	insertVKRaw(t, db, vkAlignedID, "vk-aligned", "vk-aligned-value", &rlAlignedID, true)
	insertBudgetRaw(t, db, "budget-aligned-1", &vkAlignedID)
	insertBudgetRaw(t, db, "budget-aligned-2", &vkAlignedID)

	// Seed: VK-unaligned carries calendar_aligned=false; its rate limit and
	// budgets MUST NOT be backfilled (preserves legacy per-VK semantics).
	insertRateLimitRaw(t, db, "rl-unaligned")
	vkUnalignedID := "vk-unaligned"
	rlUnalignedID := "rl-unaligned"
	insertVKRaw(t, db, vkUnalignedID, "vk-unaligned", "vk-unaligned-value", &rlUnalignedID, false)
	insertBudgetRaw(t, db, "budget-unaligned", &vkUnalignedID)

	// Seed: VK-aligned-no-rl has calendar_aligned=true but no rate limit — its
	// single budget should still be backfilled.
	vkAlignedNoRLID := "vk-aligned-no-rl"
	insertVKRaw(t, db, vkAlignedNoRLID, "vk-aligned-no-rl", "vk-aligned-no-rl-value", nil, true)
	insertBudgetRaw(t, db, "budget-aligned-no-rl", &vkAlignedNoRLID)

	// Seed: orphaned rows (no VK owner) — these must NOT be backfilled.
	insertRateLimitRaw(t, db, "rl-orphan")
	insertBudgetRaw(t, db, "budget-orphan", nil)

	// Run the migration.
	require.NoError(t, migrateCalendarAlignedToBudgetsAndRateLimitsTable(ctx, db, testMigrationLogger),
		"migration should succeed")

	// Post-condition: columns exist.
	assert.True(t, mig.HasColumn(&tables.TableBudget{}, "calendar_aligned"),
		"calendar_aligned should exist on budgets after migration")
	assert.True(t, mig.HasColumn(&tables.TableRateLimit{}, "calendar_aligned"),
		"calendar_aligned should exist on rate_limits after migration")

	// Verify per-row state.
	type budgetRow struct {
		ID              string
		CalendarAligned bool
	}
	type rlRow struct {
		ID              string
		CalendarAligned bool
	}

	assertBudget := func(id string, expected bool) {
		t.Helper()
		var row budgetRow
		err := db.Table("governance_budgets").
			Select("id, calendar_aligned").
			Where("id = ?", id).Scan(&row).Error
		require.NoError(t, err)
		assert.Equal(t, expected, row.CalendarAligned,
			"budget %s calendar_aligned mismatch", id)
	}
	assertRateLimit := func(id string, expected bool) {
		t.Helper()
		var row rlRow
		err := db.Table("governance_rate_limits").
			Select("id, calendar_aligned").
			Where("id = ?", id).Scan(&row).Error
		require.NoError(t, err)
		assert.Equal(t, expected, row.CalendarAligned,
			"rate_limit %s calendar_aligned mismatch", id)
	}

	// Rows attached to VKs with calendar_aligned=true should be backfilled.
	assertBudget("budget-aligned-1", true)
	assertBudget("budget-aligned-2", true)
	assertBudget("budget-aligned-no-rl", true)
	assertRateLimit("rl-aligned", true)

	// Rows attached to VKs with calendar_aligned=false must remain false —
	// the migration must preserve the legacy per-VK semantic.
	assertBudget("budget-unaligned", false)
	assertRateLimit("rl-unaligned", false)

	// Orphaned rows should retain the default (false).
	assertBudget("budget-orphan", false)
	assertRateLimit("rl-orphan", false)
}

// TestMigrationCalendarAligned_StaleRateLimitID verifies that a VK pointing at a
// deleted rate_limit row is skipped rather than aborting the migration (the FK
// is intentionally not DB-enforced on TableVirtualKey).
func TestMigrationCalendarAligned_StaleRateLimitID(t *testing.T) {
	db := setupCalendarAlignedPreMigrationDB(t)
	ctx := context.Background()

	// VK-stale carries calendar_aligned=true (so the migration touches it) and
	// references a rate_limit_id that doesn't exist in the table.
	stale := "rl-does-not-exist"
	insertVKRaw(t, db, "vk-stale", "vk-stale", "vk-stale-value", &stale, true)
	insertBudgetRaw(t, db, "budget-stale-vk", strPtr("vk-stale"))

	// VK-ok has calendar_aligned=true and a valid rate_limit so we can verify
	// the loop keeps processing after the stale skip.
	insertRateLimitRaw(t, db, "rl-ok")
	vkOKID := "vk-ok"
	rlOK := "rl-ok"
	insertVKRaw(t, db, vkOKID, "vk-ok", "vk-ok-value", &rlOK, true)
	insertBudgetRaw(t, db, "budget-ok", &vkOKID)

	// Migration must NOT return an error despite the stale reference.
	require.NoError(t, migrateCalendarAlignedToBudgetsAndRateLimitsTable(ctx, db, testMigrationLogger),
		"stale rate_limit_id should be skipped, not abort the migration")

	// The VK with the stale ref still has its budgets backfilled.
	var staleVKBudget bool
	require.NoError(t, db.Table("governance_budgets").
		Select("calendar_aligned").
		Where("id = ?", "budget-stale-vk").Scan(&staleVKBudget).Error)
	assert.True(t, staleVKBudget, "budget attached to the stale-ref VK should still be migrated")

	// Non-stale VK should have been fully processed.
	var okRL bool
	require.NoError(t, db.Table("governance_rate_limits").
		Select("calendar_aligned").
		Where("id = ?", "rl-ok").Scan(&okRL).Error)
	assert.True(t, okRL, "valid rate_limit should be migrated even when a prior VK had a stale ref")
}

// TestMigrationCalendarAligned_Idempotent ensures the migration can run twice
// without error — the migrator library must record the migration ID so the
// second invocation is a no-op.
func TestMigrationCalendarAligned_Idempotent(t *testing.T) {
	db := setupCalendarAlignedPreMigrationDB(t)
	ctx := context.Background()

	insertRateLimitRaw(t, db, "rl-1")
	vkID := "vk-1"
	rl := "rl-1"
	insertVKRaw(t, db, vkID, "vk-1", "vk-1-value", &rl, true)
	insertBudgetRaw(t, db, "budget-1", &vkID)

	require.NoError(t, migrateCalendarAlignedToBudgetsAndRateLimitsTable(ctx, db, testMigrationLogger),
		"first run should succeed")
	require.NoError(t, migrateCalendarAlignedToBudgetsAndRateLimitsTable(ctx, db, testMigrationLogger),
		"second run should be a no-op")
}

func TestMigrationAddMultiBudgetTables_DropsLegacyBudgetColumnsAndBackfillsOwners(t *testing.T) {
	db := setupLegacyBudgetOwnerMigrationDB(t)
	ctx := context.Background()
	mig := db.Migrator()

	vkID := "vk-legacy"
	insertVKRaw(t, db, vkID, "vk-legacy", "vk-legacy-value", nil, false)
	insertBudgetRaw(t, db, "budget-vk-legacy", nil)
	require.NoError(t, db.Exec(`UPDATE governance_virtual_keys SET budget_id = ? WHERE id = ?`, "budget-vk-legacy", vkID).Error)

	insertProviderConfigRaw(t, db, 1001, vkID, "openai")
	insertBudgetRaw(t, db, "budget-pc-legacy", nil)
	require.NoError(t, db.Exec(`UPDATE governance_virtual_key_provider_configs SET budget_id = ? WHERE id = ?`, "budget-pc-legacy", 1001).Error)

	require.True(t, mig.HasColumn("governance_virtual_keys", "budget_id"))
	require.True(t, mig.HasColumn("governance_virtual_key_provider_configs", "budget_id"))

	require.NoError(t, migrationAddMultiBudgetTables(ctx, db, testMigrationLogger))

	assert.False(t, mig.HasColumn("governance_virtual_keys", "budget_id"), "vk budget_id should be dropped by migration")
	assert.False(t, mig.HasColumn("governance_virtual_key_provider_configs", "budget_id"), "provider config budget_id should be dropped by migration")

	var vkBudgetOwner string
	require.NoError(t, db.Table("governance_budgets").Select("virtual_key_id").Where("id = ?", "budget-vk-legacy").Scan(&vkBudgetOwner).Error)
	assert.Equal(t, vkID, vkBudgetOwner, "migration should backfill governance_budgets.virtual_key_id")

	var providerConfigBudgetOwner uint
	require.NoError(t, db.Table("governance_budgets").Select("provider_config_id").Where("id = ?", "budget-pc-legacy").Scan(&providerConfigBudgetOwner).Error)
	assert.Equal(t, uint(1001), providerConfigBudgetOwner, "migration should backfill governance_budgets.provider_config_id")

	// Verify FK references in governance_budgets still point at the correct
	// table names — not corrupted backup/temp names from SQLite rename propagation.
	assertNoCorruptedFKReferences(t, db)
}

func TestMigrationAddTeamBudgetsToBudgetsTable_DropsLegacyBudgetColumnAndBackfillsOwners(t *testing.T) {
	db := setupLegacyBudgetOwnerMigrationDB(t)
	ctx := context.Background()
	mig := db.Migrator()

	insertTeamRaw(t, db, "team-legacy", "Legacy Team")
	insertBudgetRaw(t, db, "budget-team-legacy", nil)
	require.NoError(t, db.Exec(`UPDATE governance_teams SET budget_id = ? WHERE id = ?`, "budget-team-legacy", "team-legacy").Error)

	require.True(t, mig.HasColumn("governance_teams", "budget_id"))

	require.NoError(t, migrationAddTeamBudgetsToBudgetsTable(ctx, db, testMigrationLogger))

	assert.False(t, mig.HasColumn("governance_teams", "budget_id"), "team budget_id should be dropped by migration")

	var teamBudgetOwner string
	require.NoError(t, db.Table("governance_budgets").Select("team_id").Where("id = ?", "budget-team-legacy").Scan(&teamBudgetOwner).Error)
	assert.Equal(t, "team-legacy", teamBudgetOwner, "migration should backfill governance_budgets.team_id")

	// Verify FK references in governance_budgets still point at the correct
	// table names — not corrupted backup/temp names from SQLite rename propagation.
	assertNoCorruptedFKReferences(t, db)
}

// After the full migration chain, deleting a model config row must cascade-delete
// its model_config_id-owned budgets at the DB level (foreign_keys on) — no
// application code involved. Regression guard for migrationAddModelConfigBudgetsFKConstraint.
func TestMigrationAddModelConfigBudgetsFKConstraint_CascadesOnDelete(t *testing.T) {
	ctx := context.Background()
	n := time.Now().UnixNano() + testDBCounter
	testDBCounter++
	// Shared-cache so the pool sees one DB; _foreign_keys=on so the cascade is enforced
	// (mirrors the production DSN in sqlite.go).
	dsn := fmt.Sprintf("file:fkcascade_%d?mode=memory&cache=shared&_foreign_keys=on", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, triggerMigrations(ctx, db, testMigrationLogger))

	require.True(t, db.Migrator().HasConstraint(&tables.TableModelConfig{}, "Budgets"),
		"model_config -> budgets FK should exist after the migration chain")

	now := time.Now()
	mc := tables.TableModelConfig{ID: "mc-cascade", ModelName: "gpt-4", Scope: tables.ModelConfigScopeGlobal, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(&mc).Error)
	b := tables.TableBudget{ID: "b-cascade", MaxLimit: 10, ResetDuration: "1h", LastReset: now, ModelConfigID: &mc.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(&b).Error)

	// Delete the model config DIRECTLY, bypassing application-level cleanup.
	require.NoError(t, db.Exec("DELETE FROM governance_model_configs WHERE id = ?", mc.ID).Error)

	var cnt int64
	require.NoError(t, db.Model(&tables.TableBudget{}).Where("id = ?", "b-cascade").Count(&cnt).Error)
	assert.Equal(t, int64(0), cnt, "owned budget should be cascade-deleted with its model config")
}

// The migration must pre-clean budgets whose model_config_id already references a
// missing config (existing orphans) — otherwise the FK creation would fail on a DB
// that already has the leak. Valid budgets must be retained.
func TestMigrationAddModelConfigBudgetsFKConstraint_PreCleansOrphans(t *testing.T) {
	ctx := context.Background()
	n := time.Now().UnixNano() + testDBCounter
	testDBCounter++
	// foreign_keys OFF (default) so we can plant an orphan and drop the auto-created FK
	// to reproduce the production "column present, constraint missing" starting state.
	dsn := fmt.Sprintf("file:fkpreclean_%d?mode=memory&cache=shared", n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableModelConfig{}, &tables.TableBudget{}, &tables.TableRateLimit{}))
	if db.Migrator().HasConstraint(&tables.TableModelConfig{}, "Budgets") {
		require.NoError(t, db.Migrator().DropConstraint(&tables.TableModelConfig{}, "Budgets"))
	}

	now := time.Now()
	live := tables.TableModelConfig{ID: "mc-live", ModelName: "gpt-4", Scope: tables.ModelConfigScopeGlobal, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(&live).Error)
	require.NoError(t, db.Create(&tables.TableBudget{ID: "b-live", MaxLimit: 1, ResetDuration: "1h", LastReset: now, ModelConfigID: &live.ID, CreatedAt: now, UpdatedAt: now}).Error)
	ghost := "mc-ghost"
	require.NoError(t, db.Create(&tables.TableBudget{ID: "b-orphan", MaxLimit: 1, ResetDuration: "1h", LastReset: now, ModelConfigID: &ghost, CreatedAt: now, UpdatedAt: now}).Error)

	require.NoError(t, migrationAddModelConfigBudgetsFKConstraint(ctx, db, testMigrationLogger))

	var orphan, liveB int64
	require.NoError(t, db.Model(&tables.TableBudget{}).Where("id = ?", "b-orphan").Count(&orphan).Error)
	require.NoError(t, db.Model(&tables.TableBudget{}).Where("id = ?", "b-live").Count(&liveB).Error)
	assert.Equal(t, int64(0), orphan, "orphaned model-config budget should be pre-cleaned by the migration")
	assert.Equal(t, int64(1), liveB, "valid model-config budget must be retained")
	assert.True(t, db.Migrator().HasConstraint(&tables.TableModelConfig{}, "Budgets"),
		"FK constraint should exist after the migration")
}

// migration is part of the startup chain so a fresh DB emerges with
// calendar_aligned on its current owners — the virtual key and the team —
// and the legacy per-budget / per-rate-limit columns cleaned up.
func TestMigrationCalendarAligned_WiredIntoTriggerMigrations(t *testing.T) {
	_, db := setupFullMigrationDB(t)
	mig := db.Migrator()
	// Calendar alignment is now a VK-level (and team-level) setting; budget and
	// rate-limit reset logic derives the value from the owning VK/team. The
	// legacy governance_budgets and governance_rate_limits columns are added by
	// migrate_calendar_aligned and then intentionally removed by
	// migrationDropLegacyCalendarAlignedColumns later in the same chain.
	assert.True(t, mig.HasColumn(&tables.TableVirtualKey{}, "calendar_aligned"),
		"triggerMigrations should add calendar_aligned to governance_virtual_keys")
	assert.True(t, mig.HasColumn(&tables.TableTeam{}, "calendar_aligned"),
		"triggerMigrations should add calendar_aligned to governance_teams")
	assert.False(t, mig.HasColumn(&tables.TableBudget{}, "calendar_aligned"),
		"triggerMigrations should drop legacy calendar_aligned from governance_budgets")
	assert.False(t, mig.HasColumn(&tables.TableRateLimit{}, "calendar_aligned"),
		"triggerMigrations should drop legacy calendar_aligned from governance_rate_limits")
}

// assertNoCorruptedFKReferences checks that no table in the database has FK
// references pointing at temporary/backup table names left behind by a
// SQLite table rebuild. This is the actual failure mode from the production
// bug: ALTER TABLE RENAME propagated into FK definitions in other tables,
// leaving references like governance_teams__legacy_budget_backup.
func assertNoCorruptedFKReferences(t *testing.T, db *gorm.DB) {
	t.Helper()
	type masterRow struct {
		Name string `gorm:"column:name"`
		SQL  string `gorm:"column:sql"`
	}
	var rows []masterRow
	require.NoError(t, db.Raw(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND sql IS NOT NULL`).Scan(&rows).Error)
	for _, row := range rows {
		assert.NotContains(t, row.SQL, "__dump",
			"table %s has FK referencing a dump table: %s", row.Name, row.SQL)
		assert.NotContains(t, row.SQL, "__legacy_budget_backup",
			"table %s has FK referencing a legacy backup table: %s", row.Name, row.SQL)
		assert.NotContains(t, row.SQL, "__new",
			"table %s has FK referencing a temp table: %s", row.Name, row.SQL)
	}
}

func strPtr(s string) *string { return &s }

// TestMigrationAddModelConfigScopeColumns verifies the existing-install transition:
// adding scope/scope_id columns, backfilling existing rows to "global", and swapping the
// (model_name, provider) unique index for the composite (scope, scope_id, model_name, provider) one.
func TestMigrationAddModelConfigScopeColumns(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create the OLD governance_model_configs schema: no scope/scope_id columns,
	// with a unique index on (model_name, provider).
	require.NoError(t, db.Exec(`
		CREATE TABLE governance_model_configs (
			id varchar(255) PRIMARY KEY,
			model_name varchar(255) NOT NULL,
			provider varchar(50),
			budget_id varchar(255),
			rate_limit_id varchar(255),
			config_hash varchar(255),
			created_at datetime NOT NULL,
			updated_at datetime NOT NULL
		)
	`).Error)
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX idx_model_provider ON governance_model_configs (model_name, provider)`).Error)

	now := time.Now()
	require.NoError(t, db.Exec(`
		INSERT INTO governance_model_configs (id, model_name, created_at, updated_at)
		VALUES (?, ?, ?, ?)
	`, "mc-existing", "gpt-4", now, now).Error)

	mc := &tables.TableModelConfig{}

	// Pre-migration state.
	assert.False(t, db.Migrator().HasColumn(mc, "scope"), "scope column should not exist yet")
	assert.False(t, db.Migrator().HasColumn(mc, "scope_id"), "scope_id column should not exist yet")
	assert.True(t, db.Migrator().HasIndex(mc, "idx_model_provider"), "old index should exist before migration")

	require.NoError(t, migrationAddModelConfigScopeColumns(ctx, db, testMigrationLogger))

	// Post-migration state.
	assert.True(t, db.Migrator().HasColumn(mc, "scope"), "scope column should exist after migration")
	assert.True(t, db.Migrator().HasColumn(mc, "scope_id"), "scope_id column should exist after migration")
	assert.True(t, db.Migrator().HasIndex(mc, "idx_model_scope_provider"), "new composite index should exist after migration")
	assert.False(t, db.Migrator().HasIndex(mc, "idx_model_provider"), "old index should be dropped after migration")

	// Existing row should be backfilled to the global scope.
	var scope string
	require.NoError(t, db.Table("governance_model_configs").Select("scope").Where("id = ?", "mc-existing").Scan(&scope).Error)
	assert.Equal(t, tables.ModelConfigScopeGlobal, scope, "existing row should be backfilled to the global scope")

	// Global-scope rows must have NULL scope_id for the composite unique index to work correctly.
	var scopeID *string
	require.NoError(t, db.Table("governance_model_configs").Select("scope_id").Where("id = ?", "mc-existing").Scan(&scopeID).Error)
	assert.Nil(t, scopeID, "global scope rows must have NULL scope_id")

	// Idempotency: running again must be a no-op (no error, state unchanged).
	require.NoError(t, migrationAddModelConfigScopeColumns(ctx, db, testMigrationLogger))
	assert.True(t, db.Migrator().HasColumn(mc, "scope"))
	assert.True(t, db.Migrator().HasIndex(mc, "idx_model_scope_provider"))
	assert.False(t, db.Migrator().HasIndex(mc, "idx_model_provider"))
}

// TestMigrationMigrateProviderGovernanceToModelConfigs verifies provider-level governance is
// folded into a (global, provider, '*') model_config reusing the same budget/rate-limit rows,
// and the provider FKs are cleared. Idempotent on re-run.
func TestMigrationMigrateProviderGovernanceToModelConfigs(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(
		&tables.TableProvider{}, &tables.TableModelConfig{}, &tables.TableBudget{}, &tables.TableRateLimit{},
	))

	now := time.Now()
	require.NoError(t, db.Create(&tables.TableBudget{ID: "b1", MaxLimit: 100, ResetDuration: "1M", LastReset: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&tables.TableRateLimit{ID: "rl1", TokenMaxLimit: schemas.Ptr(int64(1000)), TokenResetDuration: schemas.Ptr("1h"), TokenLastReset: now, RequestLastReset: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&tables.TableProvider{Name: "openai", BudgetID: schemas.Ptr("b1"), RateLimitID: schemas.Ptr("rl1"), CreatedAt: now, UpdatedAt: now}).Error)

	require.NoError(t, migrationMigrateProviderGovernanceToModelConfigs(ctx, db, testMigrationLogger))

	// A (global, openai, '*') model config now exists reusing the same budget/rate-limit IDs.
	var mc tables.TableModelConfig
	require.NoError(t, db.Where("scope = ? AND model_name = ? AND provider = ?", tables.ModelConfigScopeGlobal, tables.ModelConfigAllModels, "openai").First(&mc).Error)
	require.NotNil(t, mc.BudgetID)
	assert.Equal(t, "b1", *mc.BudgetID)
	require.NotNil(t, mc.RateLimitID)
	assert.Equal(t, "rl1", *mc.RateLimitID)

	// Provider governance FKs are cleared (old path now inert).
	var prov tables.TableProvider
	require.NoError(t, db.Where("name = ?", "openai").First(&prov).Error)
	assert.Nil(t, prov.BudgetID, "provider budget_id should be cleared")
	assert.Nil(t, prov.RateLimitID, "provider rate_limit_id should be cleared")

	// Idempotency: re-run creates no duplicate wildcard row.
	require.NoError(t, migrationMigrateProviderGovernanceToModelConfigs(ctx, db, testMigrationLogger))
	var count int64
	require.NoError(t, db.Model(&tables.TableModelConfig{}).
		Where("scope = ? AND model_name = ? AND provider = ?", tables.ModelConfigScopeGlobal, tables.ModelConfigAllModels, "openai").
		Count(&count).Error)
	assert.Equal(t, int64(1), count, "re-run must not duplicate the wildcard config")
}

// TestMigrationRefreshConfigHash_ColumnAheadOfTable is the focused regression test
// for issue #4797: the config-hash recompute must not fail when the
// TableClientConfig struct declares a column the physical table does not have yet.
//
// On upgrade from a pre-1.6 schema, config_client lacks dump_errors_in_console_logs
// (added by a later migration step). The old recompute derived its SELECT projection
// from the struct, so it named the missing column and failed with 42703 on Postgres
// (or "no such column" on SQLite). The fix intersects the struct columns with the
// live table columns before selecting.
func TestMigrationRefreshConfigHash_ColumnAheadOfTable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create config_client with the full current schema, then seed a row before
	// dropping the column (GORM's struct-derived INSERT would fail once the column
	// is gone). PrometheusLabels / AllowedOrigins exercise the AfterFind path the
	// recompute depends on to compute the hash.
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))
	seed := &tables.TableClientConfig{
		ConfigHash:       "stale-hash",
		PrometheusLabels: []string{"team", "env"},
		AllowedOrigins:   []string{"https://example.com"},
	}
	require.NoError(t, db.Create(seed).Error)

	// Simulate the pre-1.6 table: struct is ahead of the applied DDL.
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "dump_errors_in_console_logs"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "dump_errors_in_console_logs"),
		"precondition: dump_errors_in_console_logs must be absent to reproduce the bug")

	// Pre-fix this returned the 42703 / "no such column" error; post-fix the
	// projection skips the absent column and the recompute succeeds.
	require.NoError(t, migrationRefreshConfigHashAfterMCPExternalServerURLRemoval(ctx, db, testMigrationLogger))

	// The stale hash was recomputed. Read via raw SQL to avoid the same
	// struct-vs-table drift on the read path.
	var gotHash string
	require.NoError(t, db.Raw("SELECT config_hash FROM config_client WHERE id = ?", seed.ID).Scan(&gotHash).Error)
	assert.NotEmpty(t, gotHash, "config_hash should have been recomputed")
	assert.NotEqual(t, "stale-hash", gotHash, "config_hash should differ from the seeded stale value")
}

// TestFullMigration_UpgradeFromPreDumpErrorsSchema runs the entire ordered chain
// against a config_client that predates dump_errors_in_console_logs, reproducing
// the real upgrade path from issue #4797.
//
// A fresh triggerMigrations run cannot reproduce the bug because init's CreateTable
// builds config_client from the current struct (with the column). We instead
// pre-create the table and drop the column so the chain hits the recompute step
// (which runs before add_dump_errors_in_console_logs_column) while the column is
// still missing.
func TestFullMigration_UpgradeFromPreDumpErrorsSchema(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))
	seed := &tables.TableClientConfig{
		ConfigHash:       "stale-hash",
		PrometheusLabels: []string{"team"},
	}
	require.NoError(t, db.Create(seed).Error)
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "dump_errors_in_console_logs"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "dump_errors_in_console_logs"))

	// The full chain must complete: init skips CreateTable (table exists), the
	// recompute runs against the column-less table, and add_dump_errors re-adds it.
	require.NoError(t, triggerMigrations(ctx, db, testMigrationLogger),
		"full migration chain should complete on a pre-dump-errors config_client")

	assert.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "dump_errors_in_console_logs"),
		"chain should have re-added dump_errors_in_console_logs")

	var gotHash string
	require.NoError(t, db.Raw("SELECT config_hash FROM config_client WHERE id = ?", seed.ID).Scan(&gotHash).Error)
	assert.NotEmpty(t, gotHash)
	assert.NotEqual(t, "stale-hash", gotHash, "config_hash should have been recomputed by the chain")
}

// TestMigrationAddDualCredentialConflictBehaviorColumn verifies that upgrading a
// config_client that predates dual_credential_conflict_behavior re-adds the column
// and backfills existing rows with the NOT NULL default ('prefer_idp'). Without the
// migration, an upgraded deployment would carry a struct column the physical table
// lacks and fail the config save/sync path.
func TestMigrationAddDualCredentialConflictBehaviorColumn(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Build the current schema, seed a row, then drop the column to simulate a
	// pre-feature config_client.
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))
	seed := &tables.TableClientConfig{ConfigHash: "seed-hash"}
	require.NoError(t, db.Create(seed).Error)

	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "dual_credential_conflict_behavior"))
	require.False(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "dual_credential_conflict_behavior"),
		"precondition: dual_credential_conflict_behavior must be absent to reproduce the upgrade path")

	require.NoError(t, migrationAddDualCredentialConflictBehaviorColumn(ctx, db, testMigrationLogger),
		"migration should re-add the column")

	require.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "dual_credential_conflict_behavior"),
		"migration should have added dual_credential_conflict_behavior")

	// The pre-existing row must be backfilled with the prefer_idp default so upgraded
	// deployments retain the pre-feature behavior.
	var got string
	require.NoError(t, db.Raw("SELECT dual_credential_conflict_behavior FROM config_client WHERE id = ?", seed.ID).Scan(&got).Error)
	assert.Equal(t, "prefer_idp", got, "existing rows should default to prefer_idp after migration")

	// Idempotency: re-running the migration is a no-op and must not error.
	require.NoError(t, migrationAddDualCredentialConflictBehaviorColumn(ctx, db, testMigrationLogger),
		"re-running the migration should be idempotent")
}

// TestMigrationAddBudgetOverrideColumns verifies legacy budgets receive inactive override defaults.
func TestMigrationAddBudgetOverrideColumns(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableBudget{}))
	seed := &tables.TableBudget{
		ID:            "legacy-budget",
		MaxLimit:      100,
		ResetDuration: "1h",
	}
	require.NoError(t, db.Create(seed).Error)

	for _, column := range []string{"override_cycles_remaining", "override_mode", "override_amount"} {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableBudget{}, column))
		require.False(t, db.Migrator().HasColumn(&tables.TableBudget{}, column),
			"precondition: %s must be absent to reproduce the upgrade path", column)
	}

	require.NoError(t, migrationAddBudgetOverrideColumns(ctx, db, testMigrationLogger))

	for _, column := range []string{"override_amount", "override_mode", "override_cycles_remaining"} {
		require.True(t, db.Migrator().HasColumn(&tables.TableBudget{}, column),
			"migration should have added %s", column)
	}

	var got struct {
		OverrideAmount          float64
		OverrideMode            tables.BudgetOverrideMode
		OverrideCyclesRemaining int
	}
	require.NoError(t, db.Table("governance_budgets").
		Select("override_amount, override_mode, override_cycles_remaining").
		Where("id = ?", seed.ID).
		Scan(&got).Error)
	assert.Zero(t, got.OverrideAmount)
	assert.Empty(t, got.OverrideMode)
	assert.Zero(t, got.OverrideCyclesRemaining)

	require.NoError(t, migrationAddBudgetOverrideColumns(ctx, db, testMigrationLogger),
		"re-running the migration should be idempotent")
}

// TestMigrationAddBudgetOverrideAnchorColumns verifies the grant columns are added
// and that every already-active finite override is adopted into the derived model.
// Adoption is required rather than cosmetic: validateOverride rejects a cycles
// override with no anchor, so an unadopted row would be treated as having no
// lifecycle at all.
func TestMigrationAddBudgetOverrideAnchorColumns(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableBudget{}))
	lastReset := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// An active finite override, a forever override, and a plain budget with no
	// override. Only the first should be adopted.
	seeds := []*tables.TableBudget{
		{ID: "cycles-budget", MaxLimit: 100, ResetDuration: "1h", LastReset: lastReset},
		{ID: "forever-budget", MaxLimit: 100, ResetDuration: "1h", LastReset: lastReset},
		{ID: "plain-budget", MaxLimit: 100, ResetDuration: "1h", LastReset: lastReset},
	}
	for _, seed := range seeds {
		require.NoError(t, db.Session(&gorm.Session{SkipHooks: true}).Create(seed).Error)
	}
	// Set override state directly so the pre-migration shape is reproduced without
	// going through validation, which now requires a grant.
	require.NoError(t, db.Exec(`UPDATE governance_budgets
		SET override_amount = 25, override_mode = 'cycles', override_cycles_remaining = 2
		WHERE id = 'cycles-budget'`).Error)
	require.NoError(t, db.Exec(`UPDATE governance_budgets
		SET override_amount = 50, override_mode = 'forever'
		WHERE id = 'forever-budget'`).Error)

	for _, column := range []string{"override_anchor_reset", "override_cycles_total"} {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableBudget{}, column))
		require.False(t, db.Migrator().HasColumn(&tables.TableBudget{}, column),
			"precondition: %s must be absent to reproduce the upgrade path", column)
	}

	require.NoError(t, migrationAddBudgetOverrideAnchorColumns(ctx, db, testMigrationLogger))

	for _, column := range []string{"override_cycles_total", "override_anchor_reset"} {
		require.True(t, db.Migrator().HasColumn(&tables.TableBudget{}, column),
			"migration should have added %s", column)
	}

	readGrant := func(id string) (int, *time.Time) {
		var got struct {
			OverrideCyclesTotal int
			OverrideAnchorReset *time.Time
		}
		require.NoError(t, db.Table("governance_budgets").
			Select("override_cycles_total, override_anchor_reset").
			Where("id = ?", id).
			Scan(&got).Error)
		return got.OverrideCyclesTotal, got.OverrideAnchorReset
	}

	// The active finite override is adopted: granted its remaining count from its
	// current boundary, which can only be generous by less than one window.
	total, anchor := readGrant("cycles-budget")
	assert.Equal(t, 2, total, "adopted grant total should match the remaining count")
	require.NotNil(t, anchor, "an active finite override must end up anchored")
	assert.True(t, anchor.UTC().Equal(lastReset), "adopted grant should anchor at the budget's current boundary")

	// A forever override has no cycle lifecycle, so it must stay unanchored.
	total, anchor = readGrant("forever-budget")
	assert.Zero(t, total)
	assert.Nil(t, anchor)

	total, anchor = readGrant("plain-budget")
	assert.Zero(t, total)
	assert.Nil(t, anchor)

	require.NoError(t, migrationAddBudgetOverrideAnchorColumns(ctx, db, testMigrationLogger),
		"re-running the migration should be idempotent")

	// Idempotence must not re-adopt: the backfill is scoped to rows with no anchor,
	// so a row that already had one keeps its original grant.
	total, anchor = readGrant("cycles-budget")
	assert.Equal(t, 2, total, "re-running must not change an already adopted grant")
	require.NotNil(t, anchor)
	assert.True(t, anchor.UTC().Equal(lastReset))
}

// TestMigrationAddBudgetResetConfigColumn_NonRollbackable pins that rolling the
// fiscal-quarter column back is refused rather than performed. reset_config_json
// is the only home for a budget's quarter definition, and QuarterStartMonth
// reads a nil ResetConfig as January, so dropping the column would not merely
// lose data: it would silently re-window every fiscal-year budget onto the
// calendar year. The rollback must fail loudly and leave the column in place.
func TestMigrationAddBudgetResetConfigColumn_NonRollbackable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableBudget{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableBudget{}, "reset_config_json"))
	require.False(t, db.Migrator().HasColumn(&tables.TableBudget{}, "reset_config_json"),
		"precondition: the column must be absent to reproduce the upgrade path")

	require.NoError(t, migrationAddBudgetResetConfigColumn(ctx, db, testMigrationLogger))
	require.True(t, db.Migrator().HasColumn(&tables.TableBudget{}, "reset_config_json"),
		"migration should have added reset_config_json")

	// A budget carrying a non-default fiscal quarter is exactly the state a
	// rollback would destroy.
	seed := &tables.TableBudget{
		ID:            "fiscal-budget",
		MaxLimit:      100,
		ResetDuration: "1Q",
		ResetConfig:   &tables.BudgetResetConfig{QuarterStartMonth: int(time.April)},
	}
	require.NoError(t, db.Create(seed).Error)

	err := rollbackBudgetResetConfigColumn(db)
	require.Error(t, err, "rollback must refuse: dropping the column destroys unrecoverable fiscal-quarter state")
	assert.Contains(t, err.Error(), "non-rollbackable")
	assert.True(t, db.Migrator().HasColumn(&tables.TableBudget{}, "reset_config_json"),
		"a refused rollback must leave the column intact")

	var got tables.TableBudget
	require.NoError(t, db.Where("id = ?", seed.ID).First(&got).Error)
	assert.Equal(t, time.April, got.QuarterStartMonth(),
		"the fiscal quarter definition must survive the refused rollback")
}

func setupComplexityExemplarMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TableGovernanceConfig{}))
	require.NoError(t, db.Exec(`CREATE TABLE migrations (id VARCHAR(255) PRIMARY KEY)`).Error)
	return db
}

func seedComplexityAnalyzerConfig(t *testing.T, db *gorm.DB, value string) {
	t.Helper()
	require.NoError(t, db.Create(&tables.TableGovernanceConfig{
		Key:   tables.ConfigComplexityAnalyzerConfigKey,
		Value: value,
	}).Error)
}

func seedComplexitySemanticConfig(t *testing.T, db *gorm.DB, value string) {
	t.Helper()
	require.NoError(t, db.Create(&tables.TableGovernanceConfig{
		Key:   tables.ConfigComplexitySemanticConfigKey,
		Value: value,
	}).Error)
}

func readRawComplexityAnalyzerConfig(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var entry tables.TableGovernanceConfig
	require.NoError(t, db.First(&entry, "key = ?", tables.ConfigComplexityAnalyzerConfigKey).Error)
	return entry.Value
}

func readRawComplexitySemanticConfig(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var entry tables.TableGovernanceConfig
	require.NoError(t, db.First(&entry, "key = ?", tables.ConfigComplexitySemanticConfigKey).Error)
	return entry.Value
}

func TestMigrationBackfillDefaultComplexityExemplars(t *testing.T) {
	ctx := context.Background()
	defaults := legacyComplexityExemplarsV2()

	t.Run("backfills defaults without replacing administrator state", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		config := testSemanticAnalyzerConfig()
		config.Keywords = ComplexityEditableKeywordConfig{
			SimpleKeywords:  []string{"custom simple"},
			MediumKeywords:  []string{"custom medium", defaults.SimpleKeywords[0]},
			ComplexKeywords: []string{"custom complex"},
		}
		config.ConfigHashes = ComplexityAnalyzerConfigHashes{
			TierBoundaries:   "tier-hash",
			SimpleKeywords:   "simple-hash",
			MediumKeywords:   "medium-hash",
			ComplexKeywords:  "complex-hash",
			SemanticSettings: "semantic-hash",
		}
		config.EmbeddingFingerprint = "stale-fingerprint"

		raw, err := encodeComplexityAnalyzerConfig(config.Normalized())
		require.NoError(t, err)
		seedComplexityAnalyzerConfig(t, db, string(raw))
		semanticRaw, err := encodeComplexitySemanticConfigRow(config.Normalized())
		require.NoError(t, err)
		seedComplexitySemanticConfig(t, db, string(semanticRaw))

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		// Exemplars live in the semantic row, so that is where the backfill lands.
		semanticRow, err := decodeComplexitySemanticConfigRow([]byte(readRawComplexitySemanticConfig(t, db)))
		require.NoError(t, err)
		require.NotNil(t, semanticRow)
		got := &ComplexityAnalyzerConfig{Keywords: semanticRow.Keywords}

		// The semantic settings survive, but the fingerprint recorded against the
		// old exemplars must not: the exemplars just changed.
		assert.Equal(t, config.Normalized().Semantic, semanticRow.Semantic)
		assert.Equal(t, "semantic-hash", semanticRow.ConfigHashes.SemanticSettings)
		assert.Empty(t, semanticRow.EmbeddingFingerprint, "changed exemplars must invalidate the stored embedding fingerprint")

		// The analyzer row is untouched: it belongs to the lexical classifier,
		// and nothing about exemplars concerns it.
		assert.Equal(t, string(raw), readRawComplexityAnalyzerConfig(t, db))

		assert.Contains(t, got.Keywords.SimpleKeywords, "custom simple")
		assert.Contains(t, got.Keywords.MediumKeywords, "custom medium")
		assert.Contains(t, got.Keywords.ComplexKeywords, "custom complex")

		// An administrator already assigned this shipped phrase to MEDIUM. The
		// migration must not duplicate or move it back to its default SIMPLE tier.
		crossTierDefault := normalizeComplexityExemplarKey(defaults.SimpleKeywords[0])
		assert.NotContains(t, got.Keywords.SimpleKeywords, crossTierDefault)
		assert.Contains(t, got.Keywords.MediumKeywords, crossTierDefault)

		for _, phrase := range defaults.SimpleKeywords[1:] {
			assert.Contains(t, got.Keywords.SimpleKeywords, normalizeComplexityExemplarKey(phrase))
		}
		for _, phrase := range defaults.MediumKeywords {
			assert.Contains(t, got.Keywords.MediumKeywords, normalizeComplexityExemplarKey(phrase))
		}
		for _, phrase := range defaults.ComplexKeywords {
			assert.Contains(t, got.Keywords.ComplexKeywords, normalizeComplexityExemplarKey(phrase))
		}
	})

	t.Run("keeps an already complete config byte-for-byte unchanged", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		config := testComplexityAnalyzerConfig()
		config.Keywords = defaults
		config.EmbeddingFingerprint = "keep-fingerprint"
		raw, err := encodeComplexityAnalyzerConfig(config.Normalized())
		require.NoError(t, err)
		seedComplexityAnalyzerConfig(t, db, string(raw))
		semanticRaw, err := encodeComplexitySemanticConfigRow(config.Normalized())
		require.NoError(t, err)
		seedComplexitySemanticConfig(t, db, string(semanticRaw))

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		assert.Equal(t, string(raw), readRawComplexityAnalyzerConfig(t, db))
		// Nothing was added, so the fingerprint stays valid and untouched.
		assert.Equal(t, string(semanticRaw), readRawComplexitySemanticConfig(t, db))
	})

	// A pre-split build of this version kept the semantic block inside the
	// analyzer row. It has to travel to the semantic row with the exemplars,
	// otherwise the split itself is what loses it.
	t.Run("carries a pre-split semantic block into the semantic row", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		seedComplexityAnalyzerConfig(t, db, `{
			"tier_boundaries": {"simple_medium": 0.2, "medium_complex": 0.4},
			"keywords": {
				"simple_keywords": ["custom simple"],
				"medium_keywords": ["custom medium"],
				"complex_keywords": ["custom complex"]
			},
			"semantic": {
				"provider": "openai",
				"embedding_model": "text-embedding-3-small",
				"min_similarity": 0.3
			},
			"_config_hashes": {"semantic_settings": "carried-hash"}
		}`)

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		semanticRow, err := decodeComplexitySemanticConfigRow([]byte(readRawComplexitySemanticConfig(t, db)))
		require.NoError(t, err)
		require.NotNil(t, semanticRow)
		require.NotNil(t, semanticRow.Semantic, "the semantic block must survive the split")
		assert.Equal(t, schemas.ModelProvider("openai"), semanticRow.Semantic.Provider)
		assert.Equal(t, 0.3, semanticRow.Semantic.MinSimilarity)
		assert.Equal(t, "carried-hash", semanticRow.ConfigHashes.SemanticSettings)
		assert.Contains(t, semanticRow.Keywords.SimpleKeywords, "custom simple")
	})

	// A build further up the stack can write semantic fields this version has no
	// name for. That must cost the unreadable section, not the whole migration.
	t.Run("skips an unreadable semantic block without failing", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		seedComplexityAnalyzerConfig(t, db, `{
			"tier_boundaries": {"simple_medium": 0.2, "medium_complex": 0.4},
			"keywords": {
				"simple_keywords": ["custom simple"],
				"medium_keywords": ["custom medium"],
				"complex_keywords": ["custom complex"]
			},
			"semantic": {
				"provider": "openai",
				"embedding_model": "text-embedding-3-small",
				"a_field_from_a_higher_branch": "llm"
			}
		}`)

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		semanticRow, err := decodeComplexitySemanticConfigRow([]byte(readRawComplexitySemanticConfig(t, db)))
		require.NoError(t, err)
		require.NotNil(t, semanticRow)
		assert.Nil(t, semanticRow.Semantic)
		// The exemplars still made it across.
		assert.Contains(t, semanticRow.Keywords.SimpleKeywords, "custom simple")
	})

	// The fallback selector lives in the semantic block and the classifier it
	// names lives in the llm block, so an unreadable llm block leaves the
	// selector pointing at nothing -- which Validate rejects, costing the whole
	// backfill rather than just the block that could not be read.
	t.Run("disables the llm fallback when the llm block is unreadable", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		seedComplexityAnalyzerConfig(t, db, `{
			"tier_boundaries": {"simple_medium": 0.2, "medium_complex": 0.4},
			"keywords": {
				"simple_keywords": ["custom simple"],
				"medium_keywords": ["custom medium"],
				"complex_keywords": ["custom complex"]
			},
			"semantic": {
				"provider": "openai",
				"embedding_model": "text-embedding-3-small",
				"min_similarity": 0.3,
				"fallback": "llm"
			},
			"llm": {
				"provider": "openai",
				"model": "gpt-4o-mini",
				"a_field_from_a_higher_branch": true
			},
			"_config_hashes": {"semantic_settings": "semantic-hash", "llm_settings": "llm-hash"}
		}`)

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		semanticRow, err := decodeComplexitySemanticConfigRow([]byte(readRawComplexitySemanticConfig(t, db)))
		require.NoError(t, err)
		require.NotNil(t, semanticRow)
		require.NotNil(t, semanticRow.Semantic, "the readable semantic block still carries over")
		assert.Nil(t, semanticRow.LLM, "an unreadable llm block cannot be carried")
		assert.Equal(t, ComplexitySemanticFallbackNone, semanticRow.Semantic.Fallback,
			"the fallback must not name a classifier that was dropped")
		// Both section hashes go, so a config.json still asking for the llm
		// fallback re-applies both blocks on the next boot.
		assert.Empty(t, semanticRow.ConfigHashes.SemanticSettings)
		assert.Empty(t, semanticRow.ConfigHashes.LLMSettings)
		// The settings that had nothing to do with the fallback survive.
		assert.Equal(t, schemas.ModelProvider("openai"), semanticRow.Semantic.Provider)
		assert.Equal(t, 0.3, semanticRow.Semantic.MinSimilarity)
		// And the backfill this migration exists for still happened.
		assert.Contains(t, semanticRow.Keywords.SimpleKeywords, "custom simple")
		for _, phrase := range defaults.ComplexKeywords {
			assert.Contains(t, semanticRow.Keywords.ComplexKeywords, normalizeComplexityExemplarKey(phrase))
		}
	})

	// The semantic row itself can be unreadable for the same reason: a build
	// further up the stack writes a field this version has no name for, and
	// ComplexitySemanticConfig rejects unknown fields. Failing takes every
	// migration queued behind this one down with it, and rebuilding the row from
	// the pre-split analyzer row would overwrite content this version simply
	// cannot see. The backfill is dropped and the row is left byte-for-byte alone.
	t.Run("leaves an unreadable semantic row untouched without failing", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		seedComplexityAnalyzerConfig(t, db, `{
			"tier_boundaries": {"simple_medium": 0.2, "medium_complex": 0.4},
			"keywords": {
				"simple_keywords": ["custom simple"],
				"medium_keywords": ["custom medium"],
				"complex_keywords": ["custom complex"]
			}
		}`)
		unreadable := `{"keywords":{"simple_keywords":["from a higher branch"]},` +
			`"semantic":{"provider":"openai","embedding_model":"text-embedding-3-small","a_field_from_a_higher_branch":"llm"}}`
		seedComplexitySemanticConfig(t, db, unreadable)

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		assert.JSONEq(t, unreadable, readRawComplexitySemanticConfig(t, db),
			"a row this version cannot decode must not be rewritten from the pre-split analyzer row")
	})

	// The upgrade path: an installation that predates the split has phrases in
	// its analyzer row and no semantic row at all. Those phrases are what it has
	// been routing with, so they seed the semantic row instead of being replaced
	// by the defaults.
	t.Run("seeds the semantic row from a pre-split analyzer row", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		seedComplexityAnalyzerConfig(t, db, `{
			"tier_boundaries": {"simple_medium": 0.15, "medium_complex": 0.35, "complex_reasoning": 0.6},
			"keywords": {
				"code_keywords": ["function"],
				"reasoning_keywords": ["step by step"],
				"technical_keywords": ["architecture"],
				"simple_keywords": ["hello"]
			}
		}`)

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		semanticRow, err := decodeComplexitySemanticConfigRow([]byte(readRawComplexitySemanticConfig(t, db)))
		require.NoError(t, err)
		require.NotNil(t, semanticRow)

		// The operator's own phrases carry over, mapped onto the three tiers.
		assert.Nil(t, semanticRow.Semantic, "the seeded row had no semantic block to carry")
		assert.Contains(t, semanticRow.Keywords.SimpleKeywords, "hello")
		assert.Contains(t, semanticRow.Keywords.MediumKeywords, "function")
		assert.Contains(t, semanticRow.Keywords.MediumKeywords, "architecture")
		assert.Contains(t, semanticRow.Keywords.ComplexKeywords, "step by step")

		// And the curated defaults are appended alongside them.
		for _, phrase := range defaults.ComplexKeywords {
			assert.Contains(t, semanticRow.Keywords.ComplexKeywords, normalizeComplexityExemplarKey(phrase))
		}
	})

	// The section hashes record which config-file sections have already been
	// applied. The semantic row owns the keyword sections after the split, so
	// seeding it has to carry their hashes across too: losing them would make an
	// unchanged config file look newly applied and reapply its keyword sections
	// over the phrases this installation has been routing with.
	t.Run("carries the pre-split section hashes onto the semantic row", func(t *testing.T) {
		db := setupComplexityExemplarMigrationDB(t)
		seedComplexityAnalyzerConfig(t, db, `{
			"tier_boundaries": {"simple_medium": 0.15, "medium_complex": 0.35, "complex_reasoning": 0.6},
			"keywords": {
				"simple_keywords": ["hello"],
				"medium_keywords": ["function"],
				"complex_keywords": ["step by step"]
			},
			"_config_hashes": {
				"tier_boundaries": "hash-tiers",
				"simple_keywords": "hash-simple",
				"medium_keywords": "hash-medium",
				"complex_keywords": "hash-complex",
				"semantic_settings": "hash-semantic"
			}
		}`)

		require.NoError(t, migrationBackfillDefaultComplexityExemplars(ctx, db, testMigrationLogger))

		semanticRow, err := decodeComplexitySemanticConfigRow([]byte(readRawComplexitySemanticConfig(t, db)))
		require.NoError(t, err)
		require.NotNil(t, semanticRow)

		assert.Equal(t, "hash-simple", semanticRow.ConfigHashes.SimpleKeywords)
		assert.Equal(t, "hash-medium", semanticRow.ConfigHashes.MediumKeywords)
		assert.Equal(t, "hash-complex", semanticRow.ConfigHashes.ComplexKeywords)
		assert.Equal(t, "hash-semantic", semanticRow.ConfigHashes.SemanticSettings)
	})
}

// TestMigrationAddBatchJobsAttributionColumns verifies the upgrade path: an existing
// batch_jobs table gains the requester-identity columns without disturbing the rows
// already in it (which simply have no identity to recover).
func TestMigrationAddBatchJobsAttributionColumns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err, "Failed to create test database")
	ctx := context.Background()
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)

	// Pre-migration shape: the table as migrationAddBatchJobsTable left it.
	require.NoError(t, db.Exec(`
		CREATE TABLE batch_jobs (
			id VARCHAR(512) PRIMARY KEY,
			provider VARCHAR(255) NOT NULL,
			batch_id VARCHAR(255) NOT NULL,
			model VARCHAR(255),
			endpoint VARCHAR(255),
			provider_status VARCHAR(50),
			input_file_id VARCHAR(255),
			output_file_id VARCHAR(255),
			error_file_id VARCHAR(255),
			results_url TEXT,
			next_check_at DATETIME,
			poll_attempts INTEGER DEFAULT 0,
			accounting_status VARCHAR(50) NOT NULL,
			runner_id VARCHAR(255),
			claimed_at DATETIME,
			unpriceable_reason VARCHAR(255),
			last_error TEXT,
			aggregate_log_written_at DATETIME,
			governance_reported_at DATETIME,
			selected_key_id VARCHAR(255),
			virtual_key_id VARCHAR(255),
			budget_ids TEXT,
			rate_limit_ids TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`).Error)

	now := time.Now().UTC()
	require.NoError(t, db.Exec(`
		INSERT INTO batch_jobs (id, provider, batch_id, accounting_status, selected_key_id, virtual_key_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "batch-legacy"), "openai", "batch-legacy",
		tables.ProviderJobAccountingStatusPending, "key-1", "vk-1", now, now).Error)

	require.NoError(t, migrationAddBatchJobsAttributionColumns(ctx, db, logger))

	mig := db.Migrator()
	for _, column := range []string{"user_id", "team_id", "customer_id", "source_log_id"} {
		assert.True(t, mig.HasColumn(&tables.TableProviderJob{}, column), "expected column %s", column)
	}

	var job tables.TableProviderJob
	require.NoError(t, db.Where("id = ?", tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "batch-legacy")).First(&job).Error)
	assert.Equal(t, "key-1", job.SelectedKeyID, "pre-existing attribution must survive")
	require.NotNil(t, job.VirtualKeyID)
	assert.Equal(t, "vk-1", *job.VirtualKeyID)
	assert.Nil(t, job.UserID, "a batch created before the column has no user to recover")
	assert.Nil(t, job.SourceLogID)

	// Migrations are re-run on every boot; the second pass must be a no-op.
	require.NoError(t, migrationAddBatchJobsAttributionColumns(ctx, db, logger))
}

// TestMigrationAddVideoResolutionPricingColumns exercises the upgrade path the
// fresh-DB migration tests cannot: on a fresh database the pricing table is
// created complete, so addColumnIfNotExists no-ops and never proves it can add
// the columns to a table that predates them.
func TestMigrationAddVideoResolutionPricingColumns(t *testing.T) {
	s := setupRDBTestStore(t)
	db := s.DB()
	ctx := context.Background()

	// A pre-upgrade pricing table: no resolution-banded columns.
	require.NoError(t, db.Exec(`CREATE TABLE IF NOT EXISTS governance_model_pricing (
		id TEXT PRIMARY KEY, model TEXT, provider TEXT, mode TEXT,
		output_cost_per_video_per_second REAL
	)`).Error)
	for _, column := range videoResolutionPricingColumns {
		require.False(t, db.Migrator().HasColumn("governance_model_pricing", column), "column %s should not exist yet", column)
	}

	require.NoError(t, migrationAddVideoResolutionPricingColumns(ctx, db, testMigrationLogger))
	for _, column := range videoResolutionPricingColumns {
		require.True(t, db.Migrator().HasColumn("governance_model_pricing", column), "column %s missing after migration", column)
	}

	// Rolling deploys re-run migrations; a second pass must be a no-op.
	require.NoError(t, migrationAddVideoResolutionPricingColumns(ctx, db, testMigrationLogger))
}

// providerJobPreMigrationDDL is batch_jobs as the attribution migration left it:
// no kind, no params, and the narrow identity index this change replaces.
func providerJobPreMigrationDDL(dialect string) []string {
	if dialect == "postgres" {
		return []string{`
			CREATE TABLE batch_jobs (
				id VARCHAR(512) PRIMARY KEY,
				provider VARCHAR(255) NOT NULL,
				batch_id VARCHAR(255) NOT NULL,
				model VARCHAR(255),
				endpoint VARCHAR(255),
				provider_status VARCHAR(50),
				input_file_id VARCHAR(255),
				output_file_id VARCHAR(255),
				error_file_id VARCHAR(255),
				results_url TEXT,
				next_check_at TIMESTAMPTZ,
				poll_attempts INTEGER NOT NULL DEFAULT 0,
				accounting_status VARCHAR(50) NOT NULL,
				runner_id VARCHAR(255),
				claimed_at TIMESTAMPTZ,
				unpriceable_reason VARCHAR(255),
				last_error TEXT,
				aggregate_log_written_at TIMESTAMPTZ,
				governance_reported_at TIMESTAMPTZ,
				selected_key_id VARCHAR(255),
				virtual_key_id VARCHAR(255),
				user_id VARCHAR(255),
				team_id VARCHAR(255),
				customer_id VARCHAR(255),
				budget_ids TEXT,
				rate_limit_ids TEXT,
				source_log_id VARCHAR(255),
				created_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE UNIQUE INDEX idx_batch_jobs_identity ON batch_jobs (provider, batch_id)`,
			`CREATE INDEX idx_batch_jobs_sweeper ON batch_jobs (provider, accounting_status, next_check_at)`,
		}
	}
	return []string{`
		CREATE TABLE batch_jobs (
			id VARCHAR(512) PRIMARY KEY,
			provider VARCHAR(255) NOT NULL,
			batch_id VARCHAR(255) NOT NULL,
			model VARCHAR(255),
			endpoint VARCHAR(255),
			provider_status VARCHAR(50),
			input_file_id VARCHAR(255),
			output_file_id VARCHAR(255),
			error_file_id VARCHAR(255),
			results_url TEXT,
			next_check_at DATETIME,
			poll_attempts INTEGER DEFAULT 0,
			accounting_status VARCHAR(50) NOT NULL,
			runner_id VARCHAR(255),
			claimed_at DATETIME,
			unpriceable_reason VARCHAR(255),
			last_error TEXT,
			aggregate_log_written_at DATETIME,
			governance_reported_at DATETIME,
			selected_key_id VARCHAR(255),
			virtual_key_id VARCHAR(255),
			user_id VARCHAR(255),
			team_id VARCHAR(255),
			customer_id VARCHAR(255),
			budget_ids TEXT,
			rate_limit_ids TEXT,
			source_log_id VARCHAR(255),
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE UNIQUE INDEX idx_batch_jobs_identity ON batch_jobs (provider, batch_id)`,
		`CREATE INDEX idx_batch_jobs_sweeper ON batch_jobs (provider, accounting_status, next_check_at)`,
	}
}

// forEachProviderJobMigrationDB returns a pre-migration batch_jobs on every backend
// available. Postgres matters here specifically: the properties this migration
// leans on — a NOT NULL DEFAULT add being metadata-only, DROP INDEX cascading off a
// dropped column — are postgres semantics that sqlite does not exercise.
func forEachProviderJobMigrationDB(t *testing.T) []namedDB {
	t.Helper()
	dbs := []namedDB{}

	sqliteDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err, "Failed to create test database")
	for _, stmt := range providerJobPreMigrationDDL("sqlite") {
		require.NoError(t, sqliteDB.Exec(stmt).Error)
	}
	dbs = append(dbs, namedDB{"sqlite", sqliteDB})

	pgDB, err := gorm.Open(postgres.Open(postgresDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return dbs
	}
	sqlDB, err := pgDB.DB()
	if err != nil || sqlDB.Ping() != nil {
		return dbs
	}
	if pgDB.Exec("CREATE SCHEMA IF NOT EXISTS "+pgTestSchema).Error != nil {
		return dbs
	}
	pgDB.Exec("DROP TABLE IF EXISTS batch_jobs CASCADE")
	pgDB.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`)
	pgDB.Exec("DELETE FROM migrations")
	for _, stmt := range providerJobPreMigrationDDL("postgres") {
		if err := pgDB.Exec(stmt).Error; err != nil {
			return dbs
		}
	}
	t.Cleanup(func() {
		pgDB.Exec("DELETE FROM migrations")
		pgDB.Exec("DROP TABLE IF EXISTS batch_jobs CASCADE")
	})
	return append(dbs, namedDB{"postgres", pgDB})
}

// TestMigrationAddProviderJobKindColumns verifies the upgrade path that matters
// most in this change: an existing batch_jobs table, holding both in-flight and
// already-settled rows, gains kind/params and the widened identity index without
// a single existing row being altered, re-keyed, or losing its place in the
// sweeper's queue.
func TestMigrationAddProviderJobKindColumns(t *testing.T) {
	ctx := context.Background()
	logr := bifrost.NewDefaultLogger(schemas.LogLevelError)

	for _, backend := range forEachProviderJobMigrationDB(t) {
		t.Run(backend.name, func(t *testing.T) {
			db := backend.db
			now := time.Now().UTC().Truncate(time.Second)
			due := now.Add(-time.Minute)

			// One job still being polled and one already settled: the migration must not
			// disturb either, and the in-flight one must stay visible to the sweeper.
			require.NoError(t, db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, model, accounting_status, next_check_at, poll_attempts, selected_key_id, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "batch-inflight"), "openai", "batch-inflight",
				"gpt-4o-mini", tables.ProviderJobAccountingStatusPending, due, 3, "key-1", now, now).Error)
			require.NoError(t, db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, model, accounting_status, poll_attempts, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				tables.ProviderJobID(tables.ProviderJobKindBatch, "anthropic", "batch-settled"), "anthropic", "batch-settled",
				"claude-sonnet-5", tables.ProviderJobAccountingStatusAccounted, 0, now, now).Error)

			require.NoError(t, migrationAddProviderJobKindColumns(ctx, db, logr))

			mig := db.Migrator()
			assert.True(t, mig.HasColumn(&tables.TableProviderJob{}, "kind"))
			assert.True(t, mig.HasColumn(&tables.TableProviderJob{}, "params"))

			// Every pre-existing row *becomes* a batch row, with no UPDATE having touched
			// it: the column default is what backfills them.
			var inflight tables.TableProviderJob
			require.NoError(t, db.Where("batch_id = ?", "batch-inflight").First(&inflight).Error)
			assert.Equal(t, tables.ProviderJobKindBatch, inflight.Kind)
			assert.Nil(t, inflight.Params, "a batch created before the column has no params to recover")
			assert.Equal(t, tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "batch-inflight"), inflight.ID,
				"the primary key must survive verbatim, or the sweeper settles the job a second time under a new id")
			assert.Equal(t, "gpt-4o-mini", inflight.Model)
			assert.Equal(t, 3, inflight.PollAttempts, "poll progress must not reset")
			assert.Equal(t, "key-1", inflight.SelectedKeyID)
			require.NotNil(t, inflight.NextCheckAt)
			assert.WithinDuration(t, due, inflight.NextCheckAt.UTC(), time.Second, "the job must stay due at the same moment")

			var settled tables.TableProviderJob
			require.NoError(t, db.Where("batch_id = ?", "batch-settled").First(&settled).Error)
			assert.Equal(t, tables.ProviderJobKindBatch, settled.Kind)
			assert.Equal(t, tables.ProviderJobAccountingStatusAccounted, settled.AccountingStatus,
				"a settled batch must stay settled; re-opening it would bill it twice")

			// The identity index widened, and the narrow one it replaces is gone.
			assert.True(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_identity_v2"))
			assert.False(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_identity"))
			assert.True(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_sweeper_v2"))
			assert.False(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_sweeper"))

			// The widened index is what lets a video job reuse a provider's batch id.
			require.NoError(t, db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, kind, accounting_status, poll_attempts, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				tables.ProviderJobID(tables.ProviderJobKindVideo, "openai", "batch-inflight"), "openai", "batch-inflight",
				tables.ProviderJobKindVideo, tables.ProviderJobAccountingStatusPending, 0, now, now).Error)

			// ...but two rows of the same kind still cannot share one provider job id.
			err := db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, kind, accounting_status, poll_attempts, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"batch-job:openai:duplicate", "openai", "batch-inflight",
				tables.ProviderJobKindBatch, tables.ProviderJobAccountingStatusPending, 0, now, now).Error
			require.Error(t, err, "the widened index must still enforce one row per (provider, kind, batch_id)")

			// Re-run the DDL itself, not just the migrator: clearing the recorded id is
			// what forces the HasColumn/IF NOT EXISTS guards to actually execute against
			// an already-migrated table. Without this the migrator short-circuits and the
			// guards are never exercised at all.
			require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_provider_job_kind_columns").Error)
			require.NoError(t, migrationAddProviderJobKindColumns(ctx, db, logr),
				"the migration's own SQL must be idempotent, not merely skipped by the migrator")
			assert.True(t, mig.HasColumn(&tables.TableProviderJob{}, "kind"))
			assert.True(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_identity_v2"))
		})
	}
}

// params is the pricing basis captured at submission and is reconstructible from
// nothing else, so a rollback must refuse while any job still carries it — checked
// on the column's own contents, not on kind. Only video writes params today, so the
// kind guard happens to cover it; that is a fact about the current writers, not
// about the column.
func TestMigrationAddProviderJobKindColumns_RollbackRefusesWhileParamsExist(t *testing.T) {
	ctx := context.Background()
	logr := bifrost.NewDefaultLogger(schemas.LogLevelError)

	for _, backend := range forEachProviderJobMigrationDB(t) {
		t.Run(backend.name, func(t *testing.T) {
			db := backend.db
			now := time.Now().UTC().Truncate(time.Second)
			require.NoError(t, migrationAddProviderJobKindColumns(ctx, db, logr))

			// A batch-kind row carrying params: the kind guard passes it through.
			require.NoError(t, db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, kind, params, accounting_status, poll_attempts, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "job-with-params"), "openai", "job-with-params",
				tables.ProviderJobKindBatch, `{"seconds":8,"size":"1920x1080"}`,
				tables.ProviderJobAccountingStatusPending, 0, now, now).Error)

			err := rollbackProviderJobKindColumns(ctx, db)
			require.Error(t, err, "rollback must refuse while captured params exist")
			assert.Contains(t, err.Error(), "non-rollbackable")
			assert.Contains(t, err.Error(), "params")
			assert.True(t, db.Migrator().HasColumn(&tables.TableProviderJob{}, "params"),
				"a refused rollback must leave the column intact")

			// Cleared, it rolls back cleanly — the common case right after deploying,
			// before any job has captured anything.
			require.NoError(t, db.Exec("UPDATE batch_jobs SET params = NULL").Error)
			require.NoError(t, rollbackProviderJobKindColumns(ctx, db))
			assert.False(t, db.Migrator().HasColumn(&tables.TableProviderJob{}, "params"))
		})
	}
}

// TestMigrationAddProviderJobKindColumns_Rollback verifies the down path drops the
// columns and restores the narrow indexes — and, more importantly, that it refuses
// outright once the table holds a kind the narrow index cannot represent.
func TestMigrationAddProviderJobKindColumns_Rollback(t *testing.T) {
	ctx := context.Background()
	logr := bifrost.NewDefaultLogger(schemas.LogLevelError)

	for _, backend := range forEachProviderJobMigrationDB(t) {
		t.Run(backend.name, func(t *testing.T) {
			db := backend.db
			mig := db.Migrator()
			now := time.Now().UTC().Truncate(time.Second)

			require.NoError(t, db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, accounting_status, poll_attempts, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				tables.ProviderJobID(tables.ProviderJobKindBatch, "openai", "shared-id"), "openai", "shared-id",
				tables.ProviderJobAccountingStatusAccounted, 0, now, now).Error)
			require.NoError(t, migrationAddProviderJobKindColumns(ctx, db, logr))

			// A video job may legitimately carry the same provider-side id as a batch.
			require.NoError(t, db.Exec(`
				INSERT INTO batch_jobs (id, provider, batch_id, kind, accounting_status, poll_attempts, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				tables.ProviderJobID(tables.ProviderJobKindVideo, "openai", "shared-id"), "openai", "shared-id",
				tables.ProviderJobKindVideo, tables.ProviderJobAccountingStatusPending, 0, now, now).Error)

			// Restoring a UNIQUE (provider, batch_id) over that data is impossible, and
			// the column drop happens first — so a rollback that discovered the problem
			// at the index would already have erased the only thing telling the two rows
			// apart. It has to refuse before touching anything.
			err := rollbackProviderJobKindColumns(ctx, db)
			require.Error(t, err, "rollback must refuse while a non-batch job exists")
			assert.Contains(t, err.Error(), "non-rollbackable")
			assert.True(t, mig.HasColumn(&tables.TableProviderJob{}, "kind"),
				"a refused rollback must leave the schema exactly as it was")
			assert.True(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_identity_v2"))

			// With the video rows gone the narrow index is representable again.
			require.NoError(t, db.Exec("DELETE FROM batch_jobs WHERE kind <> ?", tables.ProviderJobKindBatch).Error)
			require.NoError(t, rollbackProviderJobKindColumns(ctx, db))

			assert.False(t, mig.HasColumn(&tables.TableProviderJob{}, "kind"))
			assert.False(t, mig.HasColumn(&tables.TableProviderJob{}, "params"))
			assert.True(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_identity"),
				"the narrow identity index must come back, or nothing enforces batch uniqueness")
			assert.True(t, mig.HasIndex(&tables.TableProviderJob{}, "idx_batch_jobs_sweeper"))

			var count int64
			require.NoError(t, db.Table("batch_jobs").Where("batch_id = ?", "shared-id").Count(&count).Error)
			assert.Equal(t, int64(1), count, "rolling back the schema must not drop the batch rows")
		})
	}
}

// TestMigrationClearPluginConfigHashes verifies the migration wipes every stored plugin
// config hash, including one the released config_hash migration populated from the row.
// That is what puts the rows in the "no config.json baseline" state startup relies on, so
// UI/API edits are not reverted on the first boot after upgrade.
func TestMigrationClearPluginConfigHashes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tables.TablePlugin{}))

	ctx := context.Background()
	require.NoError(t, db.Create(&tables.TablePlugin{
		Name: "with-hash", Enabled: true, ConfigHash: "hash-from-released-migration",
	}).Error)
	require.NoError(t, db.Create(&tables.TablePlugin{
		Name: "without-hash", Enabled: true,
	}).Error)

	require.NoError(t, migrationClearPluginConfigHashes(ctx, db, testMigrationLogger))

	var plugins []tables.TablePlugin
	require.NoError(t, db.Order("name").Find(&plugins).Error)
	require.Len(t, plugins, 2)
	for _, plugin := range plugins {
		require.Empty(t, plugin.ConfigHash, "plugin %s should have no baseline after the migration", plugin.Name)
	}

	// Running it again must be a no-op rather than an error (gormigrate skips by ID, but the
	// statement itself has to stay safe to replay).
	require.NoError(t, migrationClearPluginConfigHashes(ctx, db, testMigrationLogger))
}

// TestMigrationAddGithubCopilotConfigColumns_NonRollbackable pins that rolling these
// columns back is refused rather than performed. github_copilot_private_key holds a GitHub
// App private key, which GitHub lets you download exactly once: dropping the column does
// not merely lose a config value, it forces the operator to generate a new key on GitHub
// and re-install it. The columns are additive, so an older binary ignores them safely and
// there is nothing a rollback needs to undo.
func TestMigrationAddGithubCopilotConfigColumns_NonRollbackable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	columns := []string{
		"github_copilot_app_id",
		"github_copilot_installation_id",
		"github_copilot_repository_id",
		"github_copilot_private_key",
		"github_copilot_github_domain",
	}

	require.NoError(t, db.AutoMigrate(&tables.TableKey{}))
	for _, column := range columns {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableKey{}, column))
	}

	require.NoError(t, migrationAddGithubCopilotConfigColumns(ctx, db, testMigrationLogger))
	for _, column := range columns {
		require.True(t, db.Migrator().HasColumn(&tables.TableKey{}, column),
			"migration should have added %s", column)
	}

	// A key carrying App credentials is exactly the state a rollback would destroy.
	seed := &tables.TableKey{
		Name:       "copilot-rollback",
		ProviderID: 1,
		Provider:   "github-copilot",
		KeyID:      "copilot-rollback-1",
		GithubCopilotKeyConfig: &schemas.GithubCopilotKeyConfig{
			AppID:          *schemas.NewSecretVar("123456"),
			InstallationID: *schemas.NewSecretVar("87654321"),
			RepositoryID:   *schemas.NewSecretVar("999000111"),
			PrivateKey:     *schemas.NewSecretVar("-----BEGIN RSA PRIVATE KEY-----"),
		},
	}
	require.NoError(t, db.Create(seed).Error)

	err := rollbackGithubCopilotConfigColumns(db)
	require.Error(t, err, "rollback must refuse: dropping the columns destroys an unrecoverable App private key")
	assert.Contains(t, err.Error(), "non-rollbackable")
	for _, column := range columns {
		assert.True(t, db.Migrator().HasColumn(&tables.TableKey{}, column),
			"a refused rollback must leave %s intact", column)
	}

	var got tables.TableKey
	require.NoError(t, db.Where("key_id = ?", seed.KeyID).First(&got).Error)
	require.NotNil(t, got.GithubCopilotKeyConfig)
	assert.Equal(t, "-----BEGIN RSA PRIVATE KEY-----", got.GithubCopilotKeyConfig.PrivateKey.GetValue(),
		"the private key must survive the refused rollback")
}

// TestMigrationMigrateVKStandaloneLimitsKeepsDifferingRateLimits pins that the
// standalone-limits migration (upstream #7291) only drops a VK-scoped model config's
// rate limit when it duplicates the VK's own config.json row in both limits and live
// window. Before the migration both rows are enforced, so deleting a stricter UI-made
// limit (10 req/min against 1000 req/hour), or an identical one whose window is
// exhausted or aligned differently, would admit traffic that was being refused.
func TestMigrationMigrateVKStandaloneLimitsKeepsDifferingRateLimits(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	for _, ddl := range []string{
		`CREATE TABLE governance_virtual_keys (id TEXT PRIMARY KEY, rate_limit_id TEXT, calendar_aligned BOOLEAN DEFAULT false)`,
		`CREATE TABLE governance_rate_limits (id TEXT PRIMARY KEY, token_max_limit INTEGER, token_reset_duration TEXT,
			token_current_usage INTEGER DEFAULT 0, token_last_reset DATETIME,
			request_max_limit INTEGER, request_reset_duration TEXT, request_current_usage INTEGER DEFAULT 0, request_last_reset DATETIME,
			created_at DATETIME)`,
		`CREATE TABLE governance_model_configs (id TEXT PRIMARY KEY, model_name TEXT NOT NULL, scope TEXT NOT NULL,
			scope_id TEXT, provider TEXT, rate_limit_id TEXT, calendar_aligned BOOLEAN DEFAULT false, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE governance_budgets (id TEXT PRIMARY KEY, virtual_key_id TEXT, model_config_id TEXT, max_limit REAL,
			reset_duration TEXT, current_usage REAL, last_reset DATETIME, config_hash TEXT)`,
	} {
		require.NoError(t, db.Exec(ddl).Error)
	}
	type window struct {
		requests int64
		reset    string
		usage    int64
		lastSet  time.Time
	}
	epoch := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	seed := func(vkID string, cfg, ui window) {
		for id, w := range map[string]window{"rl-cfg-" + vkID: cfg, "rl-ui-" + vkID: ui} {
			require.NoError(t, db.Exec(`INSERT INTO governance_rate_limits (id, request_max_limit, request_reset_duration, request_current_usage,
				request_last_reset, token_current_usage, token_last_reset, created_at) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
				id, w.requests, w.reset, w.usage, w.lastSet, epoch, epoch).Error)
		}
		require.NoError(t, db.Exec(`INSERT INTO governance_virtual_keys (id, rate_limit_id) VALUES (?, ?)`, vkID, "rl-cfg-"+vkID).Error)
		require.NoError(t, db.Exec(`INSERT INTO governance_model_configs (id, model_name, scope, scope_id, rate_limit_id) VALUES (?, '*', 'virtual_key', ?, ?)`, "mc-"+vkID, vkID, "rl-ui-"+vkID).Error)
	}
	// vk-same: the UI copy repeats the config.json limit and has counted the same window.
	seed("vk-same", window{100, "1m", 7, epoch}, window{100, "1m", 7, epoch})
	// vk-differ: the UI limit is stricter than the config.json one.
	seed("vk-differ", window{1000, "1h", 7, epoch}, window{10, "1m", 7, epoch})
	// vk-exhausted: same limit, but the UI row is spent while the canonical one has just reset.
	seed("vk-exhausted", window{10, "1m", 0, epoch.Add(50 * time.Second)}, window{10, "1m", 10, epoch})
	// vk-aligned: same limit and usage, windows reset at different moments.
	seed("vk-aligned", window{10, "1m", 3, epoch}, window{10, "1m", 3, epoch.Add(30 * time.Second)})
	// vk-calendar: identical rows, but the VK resets on calendar boundaries and its model config does not.
	seed("vk-calendar", window{10, "1d", 3, epoch}, window{10, "1d", 3, epoch})
	require.NoError(t, db.Exec(`UPDATE governance_virtual_keys SET calendar_aligned = true WHERE id = 'vk-calendar'`).Error)

	require.NoError(t, migrationMigrateVKStandaloneLimitsToModelConfigs(context.Background(), db, testMigrationLogger))

	refs := func(vkID string) (vkRL, mcRL *string) {
		require.NoError(t, db.Raw(`SELECT rate_limit_id FROM governance_virtual_keys WHERE id = ?`, vkID).Scan(&vkRL).Error)
		require.NoError(t, db.Raw(`SELECT rate_limit_id FROM governance_model_configs WHERE id = ?`, "mc-"+vkID).Scan(&mcRL).Error)
		return vkRL, mcRL
	}
	exists := func(id string) bool {
		var n int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM governance_rate_limits WHERE id = ?`, id).Scan(&n).Error)
		return n == 1
	}

	vkRL, mcRL := refs("vk-same")
	require.Nil(t, vkRL, "the duplicate's VK reference moves onto the model config")
	require.NotNil(t, mcRL)
	require.Equal(t, "rl-cfg-vk-same", *mcRL, "the model config takes the canonical config.json row")
	require.False(t, exists("rl-ui-vk-same"), "the identical UI copy is removed")
	require.True(t, exists("rl-cfg-vk-same"))

	for _, vkID := range []string{"vk-differ", "vk-exhausted", "vk-aligned", "vk-calendar"} {
		vkRL, mcRL = refs(vkID)
		require.NotNil(t, vkRL, vkID)
		require.Equal(t, "rl-cfg-"+vkID, *vkRL, "%s: a pair that is not a duplicate keeps the VK's own limit", vkID)
		require.NotNil(t, mcRL, vkID)
		require.Equal(t, "rl-ui-"+vkID, *mcRL, "%s: a pair that is not a duplicate keeps the model-config limit", vkID)
		require.True(t, exists("rl-cfg-"+vkID), vkID)
		require.True(t, exists("rl-ui-"+vkID), "%s: a limit that is not a duplicate is never deleted", vkID)
	}
}

// TestMigrationMigrateVKStandaloneLimitsKeepsCalendarAlignment pins that moving a VK's
// standalone budgets under a model config keeps their reset boundary: a new model config
// takes the VK's calendar alignment, and a VK whose existing model config is aligned
// differently keeps its standalone budgets instead of having them re-anchored.
func TestMigrationMigrateVKStandaloneLimitsKeepsCalendarAlignment(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	for _, ddl := range []string{
		`CREATE TABLE governance_virtual_keys (id TEXT PRIMARY KEY, rate_limit_id TEXT, calendar_aligned BOOLEAN DEFAULT false)`,
		`CREATE TABLE governance_rate_limits (id TEXT PRIMARY KEY, token_max_limit INTEGER, token_reset_duration TEXT,
			token_current_usage INTEGER DEFAULT 0, token_last_reset DATETIME, request_max_limit INTEGER, request_reset_duration TEXT,
			request_current_usage INTEGER DEFAULT 0, request_last_reset DATETIME, created_at DATETIME)`,
		`CREATE TABLE governance_model_configs (id TEXT PRIMARY KEY, model_name TEXT NOT NULL, scope TEXT NOT NULL,
			scope_id TEXT, provider TEXT, rate_limit_id TEXT, calendar_aligned BOOLEAN DEFAULT false, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE governance_budgets (id TEXT PRIMARY KEY, virtual_key_id TEXT, model_config_id TEXT, max_limit REAL,
			reset_duration TEXT, current_usage REAL, last_reset DATETIME, config_hash TEXT)`,
		// vk-new: calendar-aligned, no model config yet.
		`INSERT INTO governance_virtual_keys (id, calendar_aligned) VALUES ('vk-new', true)`,
		`INSERT INTO governance_budgets (id, virtual_key_id, max_limit, reset_duration, current_usage) VALUES ('b-new', 'vk-new', 10, '1d', 10)`,
		// vk-mismatch: calendar-aligned, with an existing model config that is not.
		`INSERT INTO governance_virtual_keys (id, calendar_aligned) VALUES ('vk-mismatch', true)`,
		`INSERT INTO governance_model_configs (id, model_name, scope, scope_id, calendar_aligned) VALUES ('mc-mismatch', '*', 'virtual_key', 'vk-mismatch', false)`,
		`INSERT INTO governance_budgets (id, virtual_key_id, max_limit, reset_duration, current_usage) VALUES ('b-mismatch', 'vk-mismatch', 10, '1d', 10)`,
	} {
		require.NoError(t, db.Exec(ddl).Error)
	}

	require.NoError(t, migrationMigrateVKStandaloneLimitsToModelConfigs(context.Background(), db, testMigrationLogger))

	var created struct {
		ID              string
		CalendarAligned bool
	}
	require.NoError(t, db.Raw(`SELECT id, calendar_aligned FROM governance_model_configs WHERE scope_id = 'vk-new'`).Scan(&created).Error)
	require.NotEmpty(t, created.ID, "the VK gets a top-level model config")
	require.True(t, created.CalendarAligned, "the new model config resets by the VK's calendar alignment")
	var owner *string
	require.NoError(t, db.Raw(`SELECT model_config_id FROM governance_budgets WHERE id = 'b-new'`).Scan(&owner).Error)
	require.NotNil(t, owner)
	require.Equal(t, created.ID, *owner)

	var budget struct {
		VirtualKeyID  *string
		ModelConfigID *string
	}
	require.NoError(t, db.Raw(`SELECT virtual_key_id, model_config_id FROM governance_budgets WHERE id = 'b-mismatch'`).Scan(&budget).Error)
	require.NotNil(t, budget.VirtualKeyID, "a budget whose destination is aligned differently stays with its VK")
	require.Equal(t, "vk-mismatch", *budget.VirtualKeyID)
	require.Nil(t, budget.ModelConfigID)
}

// oauth2PreWidenDDL returns the three OAuth2 AS tables as
// migrationAddOAuth2IssuanceTables created them before the client-controlled
// columns were widened to text: state varchar(512), client_name and every scope
// column varchar(255). Those bounds are the upgrade path the widen migration
// exists for; everything else matches the structs of that time.
func oauth2PreWidenDDL(dialect string) []string {
	ts := "DATETIME"
	if dialect == "postgres" {
		ts = "TIMESTAMPTZ"
	}
	return []string{
		`CREATE TABLE oauth2_clients (
			id VARCHAR(255) PRIMARY KEY,
			client_id VARCHAR(255) NOT NULL UNIQUE,
			client_name VARCHAR(255),
			redirect_uris_json TEXT NOT NULL,
			grant_types_json TEXT NOT NULL,
			scope VARCHAR(255),
			created_at ` + ts + ` NOT NULL
		)`,
		`CREATE TABLE oauth2_authorize_requests (
			id VARCHAR(255) PRIMARY KEY,
			client_id VARCHAR(255) NOT NULL,
			redirect_uri TEXT NOT NULL,
			state VARCHAR(512) NOT NULL,
			scope VARCHAR(255),
			resource TEXT NOT NULL,
			code_challenge VARCHAR(512) NOT NULL,
			code_challenge_method VARCHAR(10) NOT NULL,
			status VARCHAR(20) NOT NULL,
			bf_mode VARCHAR(20),
			bf_sub VARCHAR(255),
			code_hash VARCHAR(255) UNIQUE,
			expires_at ` + ts + ` NOT NULL,
			created_at ` + ts + ` NOT NULL,
			updated_at ` + ts + ` NOT NULL
		)`,
		`CREATE TABLE oauth2_refresh_tokens (
			id VARCHAR(255) PRIMARY KEY,
			token_hash VARCHAR(255) NOT NULL UNIQUE,
			family_id VARCHAR(255) NOT NULL,
			client_id VARCHAR(255) NOT NULL,
			bf_mode VARCHAR(20) NOT NULL,
			bf_sub VARCHAR(255) NOT NULL,
			scope VARCHAR(255),
			resource TEXT NOT NULL,
			revoked_at ` + ts + `,
			last_used_at ` + ts + `,
			created_at ` + ts + ` NOT NULL
		)`,
	}
}

// oauth2WidenedColumns lists every (table, column) the widen migration targets.
var oauth2WidenedColumns = []struct{ table, column string }{
	{"oauth2_authorize_requests", "state"},
	{"oauth2_authorize_requests", "scope"},
	{"oauth2_clients", "client_name"},
	{"oauth2_clients", "scope"},
	{"oauth2_refresh_tokens", "scope"},
}

// forEachOAuth2PreWidenDB returns the pre-widen tables on every available
// backend. Postgres is the one that matters: it enforces varchar(n), SQLite does
// not, so only Postgres can show the bounds and their removal.
func forEachOAuth2PreWidenDB(t *testing.T) []namedDB {
	t.Helper()
	sqliteDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err, "Failed to create test database")
	for _, stmt := range oauth2PreWidenDDL("sqlite") {
		require.NoError(t, sqliteDB.Exec(stmt).Error)
	}
	dbs := []namedDB{{"sqlite", sqliteDB}}

	pgDB, err := gorm.Open(postgres.Open(postgresDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return dbs
	}
	sqlDB, err := pgDB.DB()
	if err != nil || sqlDB.Ping() != nil {
		return dbs
	}
	if pgDB.Exec("CREATE SCHEMA IF NOT EXISTS "+pgTestSchema).Error != nil {
		return dbs
	}
	dropAll := func() {
		pgDB.Exec("DROP TABLE IF EXISTS oauth2_refresh_tokens CASCADE")
		pgDB.Exec("DROP TABLE IF EXISTS oauth2_authorize_requests CASCADE")
		pgDB.Exec("DROP TABLE IF EXISTS oauth2_clients CASCADE")
	}
	dropAll()
	pgDB.Exec(`CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)`)
	pgDB.Exec("DELETE FROM migrations")
	for _, stmt := range oauth2PreWidenDDL("postgres") {
		if err := pgDB.Exec(stmt).Error; err != nil {
			return dbs
		}
	}
	t.Cleanup(func() {
		pgDB.Exec("DELETE FROM migrations")
		dropAll()
	})
	return append(dbs, namedDB{"postgres", pgDB})
}

// TestMigrationWidenOAuth2ClientControlledColumns verifies the upgrade path: an
// existing deployment whose client-controlled OAuth2 columns are still varchar
// accepts arbitrarily long values in each once the migration has run. On
// Postgres every pre-migration insert is required to fail first, so the
// post-migration success is proof of the widening rather than of a bound that
// was never enforced.
func TestMigrationWidenOAuth2ClientControlledColumns(t *testing.T) {
	ctx := context.Background()
	logr := bifrost.NewDefaultLogger(schemas.LogLevelError)
	long := strings.Repeat("s", 4096)
	insertClient := func(db *gorm.DB, id string) error {
		return db.Exec(`
			INSERT INTO oauth2_clients (id, client_id, client_name, redirect_uris_json, grant_types_json, scope, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, id, long, `["https://client.example/cb"]`, `["authorization_code"]`, long, time.Now()).Error
	}
	insertRequest := func(db *gorm.DB, id string) error {
		now := time.Now()
		return db.Exec(`
			INSERT INTO oauth2_authorize_requests (id, client_id, redirect_uri, state, scope, resource, code_challenge, code_challenge_method, status, expires_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "client-1", "https://client.example/cb", long, long, "https://bifrost.test/mcp",
			"challenge", "S256", "pending", now.Add(time.Minute), now, now).Error
	}
	insertRefreshToken := func(db *gorm.DB, id string) error {
		return db.Exec(`
			INSERT INTO oauth2_refresh_tokens (id, token_hash, family_id, client_id, bf_mode, bf_sub, scope, resource, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "hash-"+id, "req-1", "client-1", "vk", "vk-1", long, "https://bifrost.test/mcp", time.Now()).Error
	}

	for _, backend := range forEachOAuth2PreWidenDB(t) {
		t.Run(backend.name, func(t *testing.T) {
			db := backend.db
			if backend.name == "postgres" {
				require.Error(t, insertClient(db, "pre-widen"), "varchar(255) must reject the long client_name/scope before migration")
				require.Error(t, insertRequest(db, "pre-widen"), "varchar(512) must reject the long state before migration")
				require.Error(t, insertRefreshToken(db, "pre-widen"), "varchar(255) must reject the long scope before migration")
			}

			require.NoError(t, migrationWidenOAuth2ClientControlledColumns(ctx, db, logr))

			require.NoError(t, insertClient(db, "post-widen"))
			require.NoError(t, insertRequest(db, "post-widen"))
			require.NoError(t, insertRefreshToken(db, "post-widen"))
			var got string
			require.NoError(t, db.Raw("SELECT state FROM oauth2_authorize_requests WHERE id = ?", "post-widen").Scan(&got).Error)
			assert.Equal(t, long, got)
			require.NoError(t, db.Raw("SELECT client_name FROM oauth2_clients WHERE id = ?", "post-widen").Scan(&got).Error)
			assert.Equal(t, long, got)
			require.NoError(t, db.Raw("SELECT scope FROM oauth2_refresh_tokens WHERE id = ?", "post-widen").Scan(&got).Error)
			assert.Equal(t, long, got)

			if backend.name == "postgres" {
				for _, c := range oauth2WidenedColumns {
					var dataType string
					require.NoError(t, db.Raw(`SELECT data_type FROM information_schema.columns
						WHERE table_schema = ? AND table_name = ? AND column_name = ?`, pgTestSchema, c.table, c.column).Scan(&dataType).Error)
					assert.Equalf(t, "text", dataType, "%s.%s", c.table, c.column)
				}
			}

			// Re-running is a no-op: the migrator has recorded the ID.
			require.NoError(t, migrationWidenOAuth2ClientControlledColumns(ctx, db, logr))
		})
	}
}

// TestMigrationAddVirtualKeyBusinessUnitColumn covers the upgrade path for business-unit key
// ownership: an installation whose governance_virtual_keys predates the column gets it and its
// index, and a re-run over an already-migrated table is a no-op rather than an error.
func TestMigrationAddVirtualKeyBusinessUnitColumn(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	mg := db.Migrator()

	require.False(t, mg.HasColumn(&tables.TableVirtualKey{}, "business_unit_id"),
		"business_unit_id must not exist before the migration")

	require.NoError(t, migrationAddVirtualKeyBusinessUnitColumn(ctx, db, testMigrationLogger))

	assert.True(t, mg.HasColumn(&tables.TableVirtualKey{}, "business_unit_id"),
		"business_unit_id column should exist after migration")
	assert.True(t, mg.HasIndex(&tables.TableVirtualKey{}, "idx_governance_virtual_keys_business_unit_id"),
		"business_unit_id must be indexed: it is walked per request to find the key's owner")

	// Idempotent: the column and index already being there is the normal case on every pod that
	// is not the one that ran the migration first.
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id IN (?, ?)",
		"add_virtual_key_business_unit_column", "add_virtual_key_business_unit_column_index").Error)
	require.NoError(t, migrationAddVirtualKeyBusinessUnitColumn(ctx, db, testMigrationLogger))

	// A key owned by a business unit hashes that owner, and config synchronization compares the hash
	// it stores against the one it computes. Seeded here so the two cannot drift apart and report a
	// business-unit-owned key as changed on every sync.
	// This fixture's table is the pre-migration skeleton; fill in the rest of the columns the
	// struct declares so a row can be written through it.
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualKey{}))
	buID := "bu-1"
	vk := tables.TableVirtualKey{
		ID: "vk-bu-hash", Name: "vk-bu-hash", Value: *schemas.NewSecretVar("sk-bf-vk-bu-hash"), BusinessUnitID: &buID,
	}
	hash, err := GenerateVirtualKeyHash(vk)
	require.NoError(t, err)
	vk.ConfigHash = hash
	require.NoError(t, db.Create(&vk).Error)

	var stored tables.TableVirtualKey
	require.NoError(t, db.First(&stored, "id = ?", vk.ID).Error)
	require.NotNil(t, stored.BusinessUnitID)
	assert.Equal(t, buID, *stored.BusinessUnitID)
	recomputed, err := GenerateVirtualKeyHash(stored)
	require.NoError(t, err)
	assert.Equal(t, stored.ConfigHash, recomputed, "the stored hash must match what synchronization recomputes")
}

// TestMigrationAddVirtualKeyDisableContentLoggingColumn pins the tri-state column: the migration
// adds it and is idempotent, NULL survives a write and a read (it means "inherit"), and the config
// hash only moves when the key actually says something, so every pre-existing key keeps the hash
// synchronization already stored for it.
func TestMigrationAddVirtualKeyDisableContentLoggingColumn(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	mg := db.Migrator()

	require.False(t, mg.HasColumn(&tables.TableVirtualKey{}, "disable_content_logging"),
		"disable_content_logging column must not exist before migration")

	require.NoError(t, migrationAddVirtualKeyDisableContentLoggingColumn(ctx, db, testMigrationLogger))
	assert.True(t, mg.HasColumn(&tables.TableVirtualKey{}, "disable_content_logging"))

	// Idempotent: a re-run with the column already present must not fail.
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_virtual_key_disable_content_logging_column").Error)
	require.NoError(t, migrationAddVirtualKeyDisableContentLoggingColumn(ctx, db, testMigrationLogger))

	// This fixture's table is the pre-migration skeleton; fill in the rest of the columns the
	// struct declares so rows can be written through it.
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualKey{}))

	inherit := tables.TableVirtualKey{ID: "vk-dcl-inherit", Name: "vk-dcl-inherit", Value: *schemas.NewSecretVar("sk-bf-vk-dcl-inherit")}
	off := tables.TableVirtualKey{ID: "vk-dcl-off", Name: "vk-dcl-off", Value: *schemas.NewSecretVar("sk-bf-vk-dcl-off"), DisableContentLogging: new(true)}
	on := tables.TableVirtualKey{ID: "vk-dcl-on", Name: "vk-dcl-on", Value: *schemas.NewSecretVar("sk-bf-vk-dcl-on"), DisableContentLogging: new(false)}
	for _, vk := range []*tables.TableVirtualKey{&inherit, &off, &on} {
		hash, err := GenerateVirtualKeyHash(*vk)
		require.NoError(t, err)
		vk.ConfigHash = hash
		require.NoError(t, db.Create(vk).Error)
	}

	// Fresh destination per read: GORM folds a populated primary key into the WHERE clause.
	read := func(id string) tables.TableVirtualKey {
		var stored tables.TableVirtualKey
		require.NoError(t, db.First(&stored, "id = ?", id).Error)
		return stored
	}
	assert.Nil(t, read(inherit.ID).DisableContentLogging, "a key that never said anything must read back as inherit, not false")
	storedOff := read(off.ID)
	require.NotNil(t, storedOff.DisableContentLogging)
	assert.True(t, *storedOff.DisableContentLogging)
	storedOn := read(on.ID)
	require.NotNil(t, storedOn.DisableContentLogging)
	assert.False(t, *storedOn.DisableContentLogging, "an explicit false is a decision, not an absence")

	// Hash: one key, hashed with each decision and with none, so the only thing that can move the
	// hash is the field itself. nil contributes nothing, so a key from before the column keeps its
	// hash; true and false are each a real config change, distinct from inherit and from each other.
	// Built with a fresh secret: Create encrypts a SecretVar in place, so reusing inherit.Value
	// here would hash ciphertext against the plaintext hash stored on inherit.
	same := tables.TableVirtualKey{ID: inherit.ID, Name: inherit.Name, Value: *schemas.NewSecretVar("sk-bf-vk-dcl-inherit")}
	hashWith := func(decision *bool) string {
		same.DisableContentLogging = decision
		hash, err := GenerateVirtualKeyHash(same)
		require.NoError(t, err)
		return hash
	}
	inheritHash, offHash, onHash := hashWith(nil), hashWith(new(true)), hashWith(new(false))
	assert.Equal(t, inheritHash, inherit.ConfigHash, "inherit must hash exactly like a key from before the column existed")
	assert.NotEqual(t, inheritHash, offHash, "forcing content off must change the hash")
	assert.NotEqual(t, inheritHash, onHash, "forcing content on must change the hash")
	assert.NotEqual(t, offHash, onHash, "off and on must not collide")
	recomputed, err := GenerateVirtualKeyHash(storedOff)
	require.NoError(t, err)
	assert.Equal(t, storedOff.ConfigHash, recomputed, "the stored hash must match what synchronization recomputes")
}

// TestMigrationAddVirtualKeyDisableContentLoggingColumn_NonRollbackable pins that rolling the
// column back is refused rather than performed. The column is the only home for a key's own
// content-logging decision, and a nil reads as "inherit": dropping it would not merely lose
// data, it would silently start logging content for every key that had turned it off.
func TestMigrationAddVirtualKeyDisableContentLoggingColumn_NonRollbackable(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	mg := db.Migrator()

	require.NoError(t, migrationAddVirtualKeyDisableContentLoggingColumn(ctx, db, testMigrationLogger))
	require.True(t, mg.HasColumn(&tables.TableVirtualKey{}, "disable_content_logging"))
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualKey{}))

	// A key that turned content off is exactly the state a rollback would strand.
	seed := tables.TableVirtualKey{ID: "vk-dcl-rollback", Name: "vk-dcl-rollback", Value: *schemas.NewSecretVar("sk-bf-vk-dcl-rollback"), DisableContentLogging: new(true)}
	require.NoError(t, db.Create(&seed).Error)

	var got tables.TableVirtualKey
	require.NoError(t, db.First(&got, "id = ?", seed.ID).Error)
	require.NotNil(t, got.DisableContentLogging)
	assert.True(t, *got.DisableContentLogging, "the surviving decision must be untouched")
}

func TestMigrationAddAgentGatewayTables_NonRollbackable(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))
	require.NoError(t, migrationAddAgentGatewayTables(context.Background(), db, testMigrationLogger))

	err := rollbackAgentGatewayTables(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-rollbackable")
	assert.True(t, db.Migrator().HasTable(&tables.TableAgentRegistration{}))
	assert.True(t, db.Migrator().HasTable(&tables.TableVirtualKeyAgentGrant{}))
	assert.True(t, db.Migrator().HasTable(&tables.TableAgentPushConfig{}))
	assert.True(t, db.Migrator().HasTable(&tables.TableAgentPushDelivery{}))
	assert.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "A2AExternalClientURL"))
}

// TestMigrationAddWarpAPIKeyIDColumn_NonRollbackable pins that rolling Warp's
// move to a key reference back is refused rather than performed. The forward
// migration deliberately NULLs api_key - clearing the credential is the step
// that matters, since GORM's SQLite driver reports a successful DropColumn
// without dropping anything. A rollback therefore cannot reconstruct api_key,
// so dropping api_key_id would leave Warp with neither a usable credential nor
// the reference that replaced it. The rollback must fail loudly and leave the
// column in place.
func TestMigrationAddWarpAPIKeyIDColumn_NonRollbackable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableWarpConfig{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableWarpConfig{}, "api_key_id"))
	require.False(t, db.Migrator().HasColumn(&tables.TableWarpConfig{}, "api_key_id"),
		"precondition: the column must be absent to reproduce the upgrade path")

	require.NoError(t, migrationAddWarpAPIKeyIDColumn(ctx, db, testMigrationLogger))
	require.True(t, db.Migrator().HasColumn(&tables.TableWarpConfig{}, "api_key_id"),
		"migration should have added api_key_id")

	// A configured Warp row is exactly the state a rollback would strand: its
	// api_key is already gone, so losing api_key_id too leaves no credential.
	seed := &tables.TableWarpConfig{
		ID: tables.WarpConfigRowID, Enabled: true, Provider: "openai", Model: "gpt-4o", APIKeyID: "key-abc",
	}
	require.NoError(t, db.Create(seed).Error)

	err := rollbackWarpAPIKeyIDColumn(ctx, db, testMigrationLogger)
	require.Error(t, err, "rollback must refuse: the forward migration destroyed the api_key it replaced")
	assert.Contains(t, err.Error(), "non-rollbackable")
	assert.True(t, db.Migrator().HasColumn(&tables.TableWarpConfig{}, "api_key_id"),
		"a refused rollback must leave the column intact")

	var got tables.TableWarpConfig
	require.NoError(t, db.Where("id = ?", seed.ID).First(&got).Error)
	assert.Equal(t, "key-abc", got.APIKeyID, "the surviving key reference must be untouched")
}

// TestMigrationAddWarpHistoryRetentionDaysColumn pins the upgrade path for a
// deployment whose warp_config row predates the setting.
//
// The column has to arrive with a zero, and zero has to keep meaning "not set"
// all the way through to schemas.WarpConfig.EffectiveHistoryRetentionDays. If
// it were ever read literally, the first sweep after an upgrade would delete
// every saved chat on the deployment.
func TestMigrationAddWarpHistoryRetentionDaysColumn(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableWarpConfig{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableWarpConfig{}, "history_retention_days"))
	require.False(t, db.Migrator().HasColumn(&tables.TableWarpConfig{}, "history_retention_days"),
		"precondition: the column must be absent to reproduce the upgrade path")
	// Raw SQL, not Create: the model now carries the column this test just
	// dropped, so GORM would name it in the INSERT and fail before the migration
	// gets a chance to add it.
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		"INSERT INTO warp_config (id, enabled, provider, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		tables.WarpConfigRowID, true, "openai", "gpt-4o", now, now).Error)

	require.NoError(t, migrationAddWarpHistoryRetentionDaysColumn(ctx, db, testMigrationLogger))
	require.True(t, db.Migrator().HasColumn(&tables.TableWarpConfig{}, "history_retention_days"),
		"migration should have added history_retention_days")

	var got tables.TableWarpConfig
	require.NoError(t, db.Where("id = ?", tables.WarpConfigRowID).First(&got).Error)
	assert.Zero(t, got.HistoryRetentionDays, "an existing row must come back unset, not expired")

	config := schemas.WarpConfig{HistoryRetentionDays: got.HistoryRetentionDays}
	assert.Equal(t, schemas.WarpDefaultHistoryRetentionDays, config.EffectiveHistoryRetentionDays(),
		"unset must resolve to the default rather than deleting everything")
}

// The embedding columns must not arrive NULL on an existing row.
//
// migrationAddWarpLogEmbeddingColumns adds them with AutoMigrate, and without a
// default the existing warp_config row gets NULL. GetWarpConfig scans them into
// plain Go strings, where database/sql reports "converting NULL to string is
// unsupported" - so reading the configuration fails outright on any deployment
// that had Warp set up before this migration.
func TestMigrationAddWarpLogEmbeddingColumnsBackfillsExistingRows(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableWarpConfig{}))
	for _, column := range []string{"embedding_provider", "embedding_model", "embedding_api_key_id", "log_vector_store_namespace"} {
		require.NoError(t, db.Migrator().DropColumn(&tables.TableWarpConfig{}, column))
	}
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		"INSERT INTO warp_config (id, enabled, provider, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		tables.WarpConfigRowID, true, "openai", "gpt-4o", now, now).Error)

	require.NoError(t, migrationAddWarpLogEmbeddingColumns(ctx, db, testMigrationLogger))

	// The read that used to fail: scanning into non-pointer strings.
	var got tables.TableWarpConfig
	require.NoError(t, db.Where("id = ?", tables.WarpConfigRowID).First(&got).Error,
		"reading the configuration must not fail on a row that predates these columns")
	require.Empty(t, got.EmbeddingProvider)
	require.Empty(t, got.LogVectorStoreNamespace)

	// And the columns must hold '' rather than NULL. SQLite will scan a NULL
	// into a string without complaint, so the assertion above passes either way;
	// Postgres is where database/sql refuses, and this is the check that would
	// have caught it.
	var nulls int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM warp_config WHERE embedding_provider IS NULL
		OR embedding_model IS NULL OR embedding_api_key_id IS NULL OR log_vector_store_namespace IS NULL`).Scan(&nulls).Error)
	require.Zero(t, nulls, "these columns are scanned into plain strings, so NULL breaks the read on Postgres")
}

func TestMigrationAddClientConfigDeleteExpiredVirtualKeysColumn(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "delete_expired_virtual_keys"))
	mg := db.Migrator()
	require.False(t, mg.HasColumn(&tables.TableClientConfig{}, "delete_expired_virtual_keys"))

	require.NoError(t, migrationAddClientConfigDeleteExpiredVirtualKeysColumn(ctx, db, testMigrationLogger))
	assert.True(t, mg.HasColumn(&tables.TableClientConfig{}, "delete_expired_virtual_keys"))

	// Idempotent: a re-run with the column already present must not fail.
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_client_config_delete_expired_virtual_keys_column").Error)
	require.NoError(t, migrationAddClientConfigDeleteExpiredVirtualKeysColumn(ctx, db, testMigrationLogger))
}

func TestMigrationAddClientConfigEnforceVKObjectOwnershipColumn(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(&tables.TableClientConfig{}))
	require.NoError(t, db.Create(&tables.TableClientConfig{}).Error)
	require.NoError(t, db.Migrator().DropColumn(&tables.TableClientConfig{}, "enforce_vk_object_ownership"))
	mg := db.Migrator()
	require.False(t, mg.HasColumn(&tables.TableClientConfig{}, "enforce_vk_object_ownership"))

	require.NoError(t, migrationAddClientConfigEnforceVKObjectOwnershipColumn(ctx, db, testMigrationLogger))
	assert.True(t, mg.HasColumn(&tables.TableClientConfig{}, "enforce_vk_object_ownership"))

	// Existing rows read back as on: an upgrade keeps provider objects bound to their creator.
	var cfg tables.TableClientConfig
	require.NoError(t, db.First(&cfg).Error)
	assert.True(t, cfg.EnforceVKObjectOwnership)

	// Idempotent: a re-run with the column already present must not fail.
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_client_config_enforce_vk_object_ownership_column").Error)
	require.NoError(t, migrationAddClientConfigEnforceVKObjectOwnershipColumn(ctx, db, testMigrationLogger))
}

func TestMigrationAddVirtualKeyDeleteAfterExpireColumn(t *testing.T) {
	db := setupVKTestDBWithoutRotationColumns(t)
	ctx := context.Background()
	mg := db.Migrator()

	require.False(t, mg.HasColumn(&tables.TableVirtualKey{}, "delete_after_expire"),
		"delete_after_expire column must not exist before migration")

	require.NoError(t, migrationAddVirtualKeyDeleteAfterExpireColumn(ctx, db, testMigrationLogger))
	assert.True(t, mg.HasColumn(&tables.TableVirtualKey{}, "delete_after_expire"),
		"delete_after_expire column should exist after migration")

	// Idempotent: a re-run with the column already present must not fail.
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_virtual_key_delete_after_expire_column").Error)
	require.NoError(t, migrationAddVirtualKeyDeleteAfterExpireColumn(ctx, db, testMigrationLogger))
}

// oauthFlowStateMigrationID is the id the state-uniqueness migration records.
const oauthFlowStateMigrationID = "make_mcp_oauth_flows_state_unique"

// createUpgradedOauthFlowsTable creates the mcp_oauth_flows shape an upgraded
// database has: the state column carries the NON-unique idx_mcp_oauth_flows_state
// the old struct tag built, which made the later CREATE UNIQUE INDEX IF NOT EXISTS
// a no-op.
func createUpgradedOauthFlowsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`CREATE TABLE mcp_oauth_flows (
		id VARCHAR(255) PRIMARY KEY, state VARCHAR(255) NOT NULL, status VARCHAR(50) NOT NULL,
		expires_at TIMESTAMP NOT NULL)`).Error)
	require.NoError(t, db.Exec(`CREATE INDEX idx_mcp_oauth_flows_state ON mcp_oauth_flows (state)`).Error)
}

// insertOauthFlow inserts one flow row with the given state, status and expiry.
func insertOauthFlow(t *testing.T, db *gorm.DB, id, state, status string, expiresAt time.Time) {
	t.Helper()
	require.NoError(t, db.Exec("INSERT INTO mcp_oauth_flows (id, state, status, expires_at) VALUES (?, ?, ?, ?)",
		id, state, status, expiresAt).Error)
}

// oauthFlowIDs returns every flow id, sorted.
func oauthFlowIDs(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var ids []string
	require.NoError(t, db.Raw("SELECT id FROM mcp_oauth_flows ORDER BY id").Scan(&ids).Error)
	return ids
}

// runOauthFlowStateMigration runs migrationMakeMCPOauthFlowsStateUnique once.
func runOauthFlowStateMigration(ctx context.Context, db *gorm.DB) error {
	return migrationMakeMCPOauthFlowsStateUnique(ctx, db, testMigrationLogger)
}

// TestFollowupsOauthFlowStateTagIsUnique pins that a fresh install, which builds
// mcp_oauth_flows from struct tags, gets a UNIQUE idx_mcp_oauth_flows_state.
func TestFollowupsOauthFlowStateTagIsUnique(t *testing.T) {
	db := freshSQLiteDB(t)
	require.NoError(t, db.Migrator().CreateTable(&tables.TableMCPOauthFlow{}))
	require.True(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
	require.Equal(t, []string{"state"}, indexColumns(t, db, "idx_mcp_oauth_flows_state"))
}

// TestFollowupsOauthFlowStateUniqueAfterAllMigrations pins that the full
// migration chain leaves state UNIQUE on both dialects.
func TestFollowupsOauthFlowStateUniqueAfterAllMigrations(t *testing.T) {
	for _, ns := range inListLimitStores(t) {
		t.Run(ns.name, func(t *testing.T) {
			require.True(t, indexIsUnique(t, ns.db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
			require.False(t, indexExists(t, ns.db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state_unique"))
		})
	}
}

// TestFollowupsOauthFlowStateConvertsToUnique pins the upgrade path: a
// non-unique idx_mcp_oauth_flows_state becomes UNIQUE under the same name. An
// expired pending or claiming copy of a duplicated state, which the expiry sweep
// would delete anyway, is removed first; every other row, including an expired
// row whose state is not duplicated, is kept. Re-running is a no-op.
func TestFollowupsOauthFlowStateConvertsToUnique(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			createUpgradedOauthFlowsTable(t, db)
			past := time.Now().Add(-time.Hour)
			future := time.Now().Add(time.Hour)
			insertOauthFlow(t, db, "f1", "s1", "pending", future)
			insertOauthFlow(t, db, "f2", "s1", "pending", past)  // expired dup: swept
			insertOauthFlow(t, db, "f3", "s2", "claiming", past) // expired dup: swept
			insertOauthFlow(t, db, "f4", "s2", "authorized", past)
			insertOauthFlow(t, db, "f5", "s3", "pending", past) // expired, not a dup: kept
			require.False(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))

			require.NoError(t, runOauthFlowStateMigration(context.Background(), db))
			require.True(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
			require.False(t, indexExists(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state_unique"))
			require.Equal(t, []string{"f1", "f4", "f5"}, oauthFlowIDs(t, db))
			require.Error(t, db.Exec("INSERT INTO mcp_oauth_flows (id, state, status, expires_at) VALUES ('f6', 's1', 'pending', ?)", future).Error)

			require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", oauthFlowStateMigrationID).Error)
			require.NoError(t, runOauthFlowStateMigration(context.Background(), db))
			require.True(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
		})
	}
}

// TestFollowupsOauthFlowStateKeepsNonUniqueOnLiveDuplicates pins the safety
// path: duplicated states the sweep would not delete make UNIQUE impossible, so
// the migration keeps the non-unique index, deletes nothing, and still succeeds.
func TestFollowupsOauthFlowStateKeepsNonUniqueOnLiveDuplicates(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			createUpgradedOauthFlowsTable(t, db)
			future := time.Now().Add(time.Hour)
			insertOauthFlow(t, db, "f1", "dup", "pending", future)
			insertOauthFlow(t, db, "f2", "dup", "authorized", future)
			insertOauthFlow(t, db, "f3", "s3", "pending", future)

			require.NoError(t, runOauthFlowStateMigration(context.Background(), db))
			require.True(t, indexExists(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
			require.False(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
			require.False(t, indexExists(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state_unique"))
			require.Equal(t, []string{"f1", "f2", "f3"}, oauthFlowIDs(t, db))

			// The recovery the fallback warning describes: remove the duplicate,
			// delete the migration's ledger row, and the next run enforces uniqueness.
			require.NoError(t, db.Exec("DELETE FROM mcp_oauth_flows WHERE id = ?", "f2").Error)
			require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", oauthFlowStateMigrationID).Error)
			require.NoError(t, runOauthFlowStateMigration(context.Background(), db))
			require.True(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"), "the retried migration must make state unique")
		})
	}
}

// TestFollowupsOauthFlowStateResumesAfterInterruptedSwap pins that a run which
// stopped after dropping the old index but before the rename finishes the swap
// on the next start instead of rebuilding.
func TestFollowupsOauthFlowStateResumesAfterInterruptedSwap(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			require.NoError(t, db.Exec(`CREATE TABLE mcp_oauth_flows (
				id VARCHAR(255) PRIMARY KEY, state VARCHAR(255) NOT NULL, status VARCHAR(50) NOT NULL,
				expires_at TIMESTAMP NOT NULL)`).Error)
			require.NoError(t, db.Exec(`CREATE UNIQUE INDEX idx_mcp_oauth_flows_state_unique ON mcp_oauth_flows (state)`).Error)

			require.NoError(t, runOauthFlowStateMigration(context.Background(), db))
			require.True(t, indexIsUnique(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state"))
			require.False(t, indexExists(t, db, "mcp_oauth_flows", "idx_mcp_oauth_flows_state_unique"))
		})
	}
}

// TestFollowupsOauthFlowStateRollbackRefuses pins that rolling back the state
// uniqueness migration fails instead of reporting success: the migration deletes
// expired duplicate flows and swaps in a UNIQUE index, neither of which can be
// undone, and a nil rollback would let the runner drop the migration record.
func TestFollowupsOauthFlowStateRollbackRefuses(t *testing.T) {
	m := mcpOauthFlowsStateUniqueMigration(context.Background(), oauthFlowStateMigrationID, testMigrationLogger)
	err := m.Rollback(nil)
	require.Error(t, err, "an irreversible migration must refuse to roll back")
	require.Contains(t, err.Error(), "non-rollbackable")
}

// indexIsUnique reports whether the named index on table is a UNIQUE index, on either dialect.
func indexIsUnique(t *testing.T, db *gorm.DB, table, name string) bool {
	t.Helper()
	if db.Dialector.Name() == "postgres" {
		var unique bool
		require.NoError(t, db.Raw(`
			SELECT COALESCE(bool_and(pi.indisunique), false)
			FROM pg_class pc
			JOIN pg_index pi ON pi.indrelid = pc.oid
			JOIN pg_class ic ON ic.oid = pi.indexrelid
			WHERE pc.relname = ? AND ic.relname = ? AND pg_catalog.pg_table_is_visible(pc.oid)
		`, table, name).Scan(&unique).Error)
		return unique
	}
	type indexRow struct {
		Name   string
		Unique int
	}
	var rows []indexRow
	require.NoError(t, db.Raw(fmt.Sprintf("PRAGMA index_list(%s)", table)).Scan(&rows).Error)
	for _, r := range rows {
		if r.Name == name {
			return r.Unique == 1
		}
	}
	return false
}

// indexColumns returns the ordered column list of a SQLite index.
func indexColumns(t *testing.T, db *gorm.DB, name string) []string {
	t.Helper()
	type colRow struct {
		Seqno int
		Name  string
	}
	var rows []colRow
	require.NoError(t, db.Raw(fmt.Sprintf("PRAGMA index_info(%s)", name)).Scan(&rows).Error)
	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		cols = append(cols, r.Name)
	}
	return cols
}

// sqliteIndexSQL returns the CREATE INDEX statement SQLite stored for an index.
func sqliteIndexSQL(t *testing.T, db *gorm.DB, name string) string {
	t.Helper()
	var sql string
	require.NoError(t, db.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", name).Scan(&sql).Error)
	return sql
}

// freshSQLiteDB opens an empty file-backed SQLite database for tag tests.
func freshSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "fresh.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

// createHashTables creates minimal pre-tag versions of governance_virtual_keys and
// sessions, the shape an upgraded database has after migrationAddEncryptionColumns
// added the hash columns without their indexes.
func createHashTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`CREATE TABLE governance_virtual_keys (id VARCHAR(255) PRIMARY KEY, value_hash VARCHAR(64))`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE sessions (id INTEGER PRIMARY KEY, token_hash VARCHAR(64))`).Error)
}

// runHashIndexMigration runs migrationAddValueHashAndTokenHashIndexes once.
func runHashIndexMigration(ctx context.Context, db *gorm.DB) error {
	return migrationAddValueHashAndTokenHashIndexes(ctx, db, testMigrationLogger)
}

// TestHighFixHashIndexesBuildUnique pins H1: an upgraded database without the tag
// indexes gets UNIQUE idx_virtual_key_value_hash and idx_session_token_hash. Many
// NULL and empty-string hashes must not block the unique build, and the migration re-runs cleanly.
func TestHighFixHashIndexesBuildUnique(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			createHashTables(t, db)
			require.NoError(t, db.Exec(`INSERT INTO governance_virtual_keys (id, value_hash) VALUES
				('vk-1', 'h1'), ('vk-2', 'h2'), ('vk-3', NULL), ('vk-4', NULL), ('vk-5', ''), ('vk-6', '')`).Error)
			require.NoError(t, db.Exec(`INSERT INTO sessions (id, token_hash) VALUES
				(1, 't1'), (2, 't2'), (3, NULL), (4, ''), (5, '')`).Error)

			runIndexMigrationTwice(t, db, "add_value_hash_and_token_hash_indexes", runHashIndexMigration,
				"governance_virtual_keys", "idx_virtual_key_value_hash")
			require.True(t, indexIsUnique(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash"))
			require.True(t, indexExists(t, db, "sessions", "idx_session_token_hash"))
			require.True(t, indexIsUnique(t, db, "sessions", "idx_session_token_hash"))
			require.False(t, indexExists(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash_nonunique"))

			// '' hashes are normalized to NULL, the value the encryption migration uses.
			var empties int64
			require.NoError(t, db.Raw("SELECT COUNT(*) FROM governance_virtual_keys WHERE value_hash = ''").Scan(&empties).Error)
			require.Zero(t, empties)
			require.NoError(t, db.Raw("SELECT COUNT(*) FROM sessions WHERE token_hash = ''").Scan(&empties).Error)
			require.Zero(t, empties)
		})
	}
}

// TestHighFixHashIndexesFallBackOnDuplicates pins H1's safety path: duplicate
// hashes make a unique build impossible, so the migration builds a non-unique
// index under a separate name instead of failing startup.
func TestHighFixHashIndexesFallBackOnDuplicates(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			createHashTables(t, db)
			require.NoError(t, db.Exec(`INSERT INTO governance_virtual_keys (id, value_hash) VALUES
				('vk-1', 'dup'), ('vk-2', 'dup'), ('vk-3', 'h3')`).Error)
			require.NoError(t, db.Exec(`INSERT INTO sessions (id, token_hash) VALUES (1, 'td'), (2, 'td'), (3, 'td'), (4, 't4')`).Error)

			require.NoError(t, runHashIndexMigration(context.Background(), db))
			require.False(t, indexExists(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash"))
			require.True(t, indexExists(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash_nonunique"))
			require.False(t, indexIsUnique(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash_nonunique"))
			require.False(t, indexExists(t, db, "sessions", "idx_session_token_hash"))
			require.True(t, indexExists(t, db, "sessions", "idx_session_token_hash_nonunique"))

			// Re-running is safe.
			require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "add_value_hash_and_token_hash_indexes").Error)
			require.NoError(t, runHashIndexMigration(context.Background(), db))
			require.True(t, indexExists(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash_nonunique"))
		})
	}
}

// TestHighFixSetBasedIndexMigrations pins H13, H14 and H27: upgraded databases get
// the lookup indexes that the new struct tags build on fresh installs.
func TestHighFixSetBasedIndexMigrations(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			require.NoError(t, db.Exec(`CREATE TABLE governance_virtual_key_mcp_configs (
				id INTEGER PRIMARY KEY, virtual_key_id VARCHAR(255) NOT NULL, mcp_client_id INTEGER NOT NULL)`).Error)
			require.NoError(t, db.Exec(`CREATE TABLE governance_virtual_key_provider_configs (
				id INTEGER PRIMARY KEY, virtual_key_id VARCHAR(255) NOT NULL, provider VARCHAR(50) NOT NULL)`).Error)
			require.NoError(t, db.Exec(`CREATE TABLE governance_virtual_key_provider_config_keys (
				table_virtual_key_provider_config_id INTEGER NOT NULL, table_key_id INTEGER NOT NULL,
				PRIMARY KEY (table_virtual_key_provider_config_id, table_key_id))`).Error)
			require.NoError(t, db.Exec(`CREATE TABLE governance_virtual_keys (
				id VARCHAR(255) PRIMARY KEY, created_at TIMESTAMP NOT NULL)`).Error)

			runIndexMigrationTwice(t, db, "add_vk_mcp_configs_mcp_client_id_index",
				func(ctx context.Context, db *gorm.DB) error {
					return migrationAddVKMCPConfigsMCPClientIDIndex(ctx, db, testMigrationLogger)
				},
				"governance_virtual_key_mcp_configs", "idx_vk_mcp_configs_mcp_client_id")
			runIndexMigrationTwice(t, db, "add_vk_provider_config_provider_and_key_indexes",
				func(ctx context.Context, db *gorm.DB) error {
					return migrationAddVKProviderConfigProviderAndKeyIndexes(ctx, db, testMigrationLogger)
				},
				"governance_virtual_key_provider_configs", "idx_vk_provider_configs_provider")
			require.True(t, indexExists(t, db, "governance_virtual_key_provider_config_keys", "idx_vkpc_keys_table_key_id"))
			runIndexMigrationTwice(t, db, "add_virtual_keys_created_at_id_index",
				func(ctx context.Context, db *gorm.DB) error {
					return migrationAddVirtualKeysCreatedAtIDIndex(ctx, db, testMigrationLogger)
				},
				"governance_virtual_keys", "idx_virtual_keys_created_at_id")
		})
	}
}

// TestHighFixBatchJobsDueMigration pins H28's upgrade path: terminal rows keep no
// next_check_at (backfilled in primary-key batches), non-terminal rows are
// untouched, and the partial due index exists.
func TestHighFixBatchJobsDueMigration(t *testing.T) {
	prev := batchJobsDueBackfillBatchSize
	batchJobsDueBackfillBatchSize = 3
	t.Cleanup(func() { batchJobsDueBackfillBatchSize = prev })

	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			require.NoError(t, db.Exec(`CREATE TABLE batch_jobs (
				id VARCHAR(512) PRIMARY KEY, kind VARCHAR(50) NOT NULL DEFAULT 'batch', provider VARCHAR(255) NOT NULL,
				accounting_status VARCHAR(50) NOT NULL, next_check_at TIMESTAMP)`).Error)
			due := time.Now().UTC().Add(-time.Hour)
			statuses := []string{
				tables.ProviderJobAccountingStatusAccounted, tables.ProviderJobAccountingStatusUnpriceable,
				tables.ProviderJobAccountingStatusPending, tables.ProviderJobAccountingStatusError,
				tables.ProviderJobAccountingStatusProcessing,
			}
			for i := 0; i < 20; i++ {
				status := statuses[i%len(statuses)]
				require.NoError(t, db.Exec("INSERT INTO batch_jobs (id, kind, provider, accounting_status, next_check_at) VALUES (?, 'batch', 'openai', ?, ?)",
					fmt.Sprintf("job-%02d", i), status, due).Error)
			}

			runIndexMigrationTwice(t, db, "add_batch_jobs_due_index",
				func(ctx context.Context, db *gorm.DB) error {
					return migrationAddBatchJobsDueIndex(ctx, db, testMigrationLogger)
				},
				"batch_jobs", "idx_batch_jobs_due")

			var terminalWithNext, nonTerminalWithNext int64
			require.NoError(t, db.Raw("SELECT COUNT(*) FROM batch_jobs WHERE accounting_status IN ('accounted','unpriceable') AND next_check_at IS NOT NULL").Scan(&terminalWithNext).Error)
			require.NoError(t, db.Raw("SELECT COUNT(*) FROM batch_jobs WHERE accounting_status NOT IN ('accounted','unpriceable') AND next_check_at IS NOT NULL").Scan(&nonTerminalWithNext).Error)
			require.Zero(t, terminalWithNext)
			require.EqualValues(t, 12, nonTerminalWithNext)

			if db.Dialector.Name() == "postgres" {
				var def string
				require.NoError(t, db.Raw("SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_batch_jobs_due' AND schemaname = current_schema()").Scan(&def).Error)
				require.Contains(t, def, "WHERE (next_check_at IS NOT NULL)")
			} else {
				require.Contains(t, sqliteIndexSQL(t, db, "idx_batch_jobs_due"), "WHERE next_check_at IS NOT NULL")
			}
		})
	}
}

// TestHighFixTagsBuildIndexes pins that fresh installs, which build tables from
// struct tags through CreateTable, get the same indexes the upgrade migrations add.
func TestHighFixTagsBuildIndexes(t *testing.T) {
	db := freshSQLiteDB(t)
	mg := db.Migrator()
	require.NoError(t, mg.CreateTable(&tables.TableVirtualKey{}))
	require.NoError(t, mg.CreateTable(&tables.SessionsTable{}))
	require.NoError(t, mg.CreateTable(&tables.TableVirtualKeyMCPConfig{}))
	require.NoError(t, mg.CreateTable(&tables.TableVirtualKeyProviderConfig{}))
	require.NoError(t, mg.CreateTable(&tables.TableVirtualKeyProviderConfigKey{}))
	require.NoError(t, mg.CreateTable(&tables.TableProviderJob{}))

	require.True(t, indexIsUnique(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash"))
	require.True(t, indexIsUnique(t, db, "sessions", "idx_session_token_hash"))

	require.Equal(t, []string{"mcp_client_id"}, indexColumns(t, db, "idx_vk_mcp_configs_mcp_client_id"))
	require.Equal(t, []string{"virtual_key_id", "mcp_client_id"}, indexColumns(t, db, "idx_vk_mcpclient"))
	require.True(t, indexIsUnique(t, db, "governance_virtual_key_mcp_configs", "idx_vk_mcpclient"))

	require.Equal(t, []string{"provider"}, indexColumns(t, db, "idx_vk_provider_configs_provider"))
	require.Equal(t, []string{"table_key_id"}, indexColumns(t, db, "idx_vkpc_keys_table_key_id"))

	require.Equal(t, []string{"created_at", "id"}, indexColumns(t, db, "idx_virtual_keys_created_at_id"))
	require.True(t, mg.HasIndex(&tables.TableVirtualKey{}, "idx_governance_virtual_keys_created_at"), "the single-column created_at index must stay")

	require.Equal(t, []string{"kind", "next_check_at"}, indexColumns(t, db, "idx_batch_jobs_due"))
	require.True(t, strings.Contains(sqliteIndexSQL(t, db, "idx_batch_jobs_due"), "WHERE next_check_at IS NOT NULL"))
	require.Equal(t, []string{"input_file_id"}, indexColumns(t, db, "idx_batch_jobs_input_file_id"))
	require.Equal(t, []string{"output_file_id"}, indexColumns(t, db, "idx_batch_jobs_output_file_id"))
	require.Equal(t, []string{"error_file_id"}, indexColumns(t, db, "idx_batch_jobs_error_file_id"))
}

// TestHighFixHashNormalizationIsBatched pins that normalizing empty hashes to
// NULL runs in bounded, separately committed batches rather than one UPDATE over
// the whole table, which would hold row locks on every matching row at once on
// a large sessions table. Five empty hashes with a batch size of two need at
// least three statements per table, and none may be left afterwards.
func TestHighFixHashNormalizationIsBatched(t *testing.T) {
	restore := hashNormalizeBatchSize
	hashNormalizeBatchSize = 2
	t.Cleanup(func() { hashNormalizeBatchSize = restore })
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			db := ndb.db
			createHashTables(t, db)
			require.NoError(t, db.Exec(`INSERT INTO governance_virtual_keys (id, value_hash) VALUES
				('vk-1', ''), ('vk-2', ''), ('vk-3', 'h3'), ('vk-4', ''), ('vk-5', ''), ('vk-6', '')`).Error)
			require.NoError(t, db.Exec(`INSERT INTO sessions (id, token_hash) VALUES
				(1, ''), (2, ''), (3, 't3'), (4, ''), (5, ''), (6, '')`).Error)

			updates := map[string]int{}
			name := "test:count-hash-normalize:" + t.Name()
			require.NoError(t, db.Callback().Raw().After("gorm:raw").Register(name, func(tx *gorm.DB) {
				sql := tx.Statement.SQL.String()
				for _, col := range []string{"value_hash", "token_hash"} {
					if strings.HasPrefix(sql, "UPDATE") && strings.Contains(sql, "SET "+col+" = NULL") {
						updates[col]++
					}
				}
			}))
			t.Cleanup(func() { _ = db.Callback().Raw().Remove(name) })

			require.NoError(t, runHashIndexMigration(context.Background(), db))
			require.GreaterOrEqual(t, updates["value_hash"], 3, "empty value hashes must be normalized in bounded batches")
			require.GreaterOrEqual(t, updates["token_hash"], 3, "empty token hashes must be normalized in bounded batches")
			var empties int64
			require.NoError(t, db.Raw("SELECT COUNT(*) FROM governance_virtual_keys WHERE value_hash = ''").Scan(&empties).Error)
			require.Zero(t, empties)
			require.NoError(t, db.Raw("SELECT COUNT(*) FROM sessions WHERE token_hash = ''").Scan(&empties).Error)
			require.Zero(t, empties)
			require.True(t, indexIsUnique(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash"))
		})
	}
}

// TestHighFixHashIndexRollbackRefuses pins that rolling back the hash-index
// migration fails instead of dropping the unique lookup indexes: on a fresh
// install the struct tags built them before this migration ran, and the ledger
// cannot tell whether the migration or the tags created them.
func TestHighFixHashIndexRollbackRefuses(t *testing.T) {
	db := freshSQLiteDB(t)
	require.NoError(t, db.Migrator().CreateTable(&tables.TableVirtualKey{}, &tables.SessionsTable{}))
	require.True(t, indexExists(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash"))

	err := valueHashAndTokenHashIndexesMigration(context.Background(), "add_value_hash_and_token_hash_indexes", testMigrationLogger).Rollback(db)
	require.Error(t, err, "the hash-index migration must refuse to roll back")
	require.Contains(t, err.Error(), "non-rollbackable")
	require.True(t, indexExists(t, db, "governance_virtual_keys", "idx_virtual_key_value_hash"), "a refused rollback must keep the index")
}

// TestHighFixBatchJobsDueRollbackRefuses pins that rolling back the batch-jobs
// migration fails: it cleared next_check_at on finished jobs, which dropping the
// index cannot restore.
func TestHighFixBatchJobsDueRollbackRefuses(t *testing.T) {
	err := batchJobsDueIndexMigration(context.Background(), "add_batch_jobs_due_index").Rollback(freshSQLiteDB(t))
	require.Error(t, err, "the batch-jobs migration must refuse to roll back")
	require.Contains(t, err.Error(), "non-rollbackable")
}

// indexMigrationPGSchema isolates the index migration tests from the other
// Postgres-backed tests in this package, which drop and recreate shared tables.
const indexMigrationPGSchema = "configstore_idxmig_test"

// indexMigrationDBs returns a fresh SQLite database and, when the local test
// Postgres is reachable, a Postgres connection in a dedicated schema. Both start
// empty so each test creates exactly the tables it needs.
func indexMigrationDBs(t *testing.T) []namedDB {
	t.Helper()
	sqliteDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "idx.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	dbs := []namedDB{{name: "sqlite", db: sqliteDB}}

	dsn := strings.Replace(postgresDSN, "search_path="+pgTestSchema, "search_path="+indexMigrationPGSchema, 1)
	pgDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return dbs
	}
	sqlDB, err := pgDB.DB()
	if err != nil || sqlDB.Ping() != nil {
		return dbs
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, pgDB.Exec("DROP SCHEMA IF EXISTS "+indexMigrationPGSchema+" CASCADE").Error)
	require.NoError(t, pgDB.Exec("CREATE SCHEMA "+indexMigrationPGSchema).Error)
	return append(dbs, namedDB{name: "postgres", db: pgDB})
}

// indexExists reports whether index name exists on table, on either dialect.
func indexExists(t *testing.T, db *gorm.DB, table, name string) bool {
	t.Helper()
	if db.Dialector.Name() == "postgres" {
		valid, err := postgresIndexIsValid(db, table, name)
		require.NoError(t, err)
		return valid
	}
	return db.Migrator().HasIndex(table, name)
}

// runIndexMigrationTwice runs an index migration, asserts the index exists, then
// clears the migration record and runs it again to prove the migration is idempotent.
func runIndexMigrationTwice(t *testing.T, db *gorm.DB, id string, run func(context.Context, *gorm.DB) error, table, index string) {
	t.Helper()
	ctx := context.Background()
	require.False(t, indexExists(t, db, table, index), "index %s must not exist before the migration", index)
	require.NoError(t, run(ctx, db))
	require.True(t, indexExists(t, db, table, index), "index %s must exist after the migration", index)
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", id).Error)
	require.NoError(t, run(ctx, db))
	require.True(t, indexExists(t, db, table, index))
}

// TestMigrationAddVKProviderConfigVirtualKeyIDIndex pins that upgraded databases,
// whose provider-config table was created before the struct tag existed, get the
// virtual_key_id index, that the migration is safe to re-run, and that its
// rollback drops the index (concurrently on Postgres) and is safe to re-run.
func TestMigrationAddVKProviderConfigVirtualKeyIDIndex(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			require.NoError(t, ndb.db.Exec(`CREATE TABLE governance_virtual_key_provider_configs (
				id INTEGER PRIMARY KEY, virtual_key_id VARCHAR(255) NOT NULL, provider VARCHAR(50) NOT NULL)`).Error)
			runIndexMigrationTwice(t, ndb.db, "add_vk_provider_config_virtual_key_id_index",
				func(ctx context.Context, db *gorm.DB) error {
					return migrationAddVKProviderConfigVirtualKeyIDIndex(ctx, db, testMigrationLogger)
				},
				"governance_virtual_key_provider_configs", "idx_vk_provider_configs_virtual_key_id")

			rollback := vkProviderConfigVirtualKeyIDIndexMigration(context.Background(), "add_vk_provider_config_virtual_key_id_index").Rollback
			require.NoError(t, rollback(ndb.db))
			require.False(t, indexExists(t, ndb.db, "governance_virtual_key_provider_configs", "idx_vk_provider_configs_virtual_key_id"),
				"index must not exist after the rollback")
			require.NoError(t, rollback(ndb.db))
		})
	}
}

// TestVKProviderConfigTagsBuildVirtualKeyIDIndex pins that a fresh install, which
// creates the table from struct tags, gets the same index the upgrade migration adds.
func TestVKProviderConfigTagsBuildVirtualKeyIDIndex(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "fresh.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Migrator().CreateTable(&tables.TableVirtualKeyProviderConfig{}))
	require.True(t, db.Migrator().HasIndex(&tables.TableVirtualKeyProviderConfig{}, "idx_vk_provider_configs_virtual_key_id"))
}

// TestIndexHelpersNeverDropAnotherTablesIndex pins that the concurrent index
// helpers only ever drop the index on their own table. With the search path set
// to the test schema and then another schema, and only the other schema holding
// an index of the same name, neither building the index on the test schema's
// table nor rolling the migration back may drop the other schema's index: an
// unqualified DROP INDEX resolves through search_path and would find it.
func TestIndexHelpersNeverDropAnotherTablesIndex(t *testing.T) {
	const otherSchema = "configstore_idxmig_other"
	const table = "governance_virtual_key_provider_configs"
	const index = "idx_vk_provider_configs_virtual_key_id"
	for _, ndb := range indexMigrationDBs(t) {
		if ndb.name != "postgres" {
			continue
		}
		t.Run(ndb.name, func(t *testing.T) {
			ctx := context.Background()
			sqlDB, err := ndb.db.DB()
			require.NoError(t, err)
			// One connection, so the search_path set below applies to every statement.
			sqlDB.SetMaxOpenConns(1)
			db := ndb.db
			require.NoError(t, db.Exec("DROP SCHEMA IF EXISTS "+otherSchema+" CASCADE").Error)
			t.Cleanup(func() { _ = db.Exec("DROP SCHEMA IF EXISTS " + otherSchema + " CASCADE").Error })
			require.NoError(t, db.Exec("CREATE SCHEMA "+otherSchema).Error)
			require.NoError(t, db.Exec("CREATE TABLE "+otherSchema+"."+table+" (id INTEGER PRIMARY KEY, virtual_key_id VARCHAR(255))").Error)
			require.NoError(t, db.Exec("CREATE INDEX "+index+" ON "+otherSchema+"."+table+" (virtual_key_id)").Error)
			require.NoError(t, db.Exec(`CREATE TABLE `+table+` (
				id INTEGER PRIMARY KEY, virtual_key_id VARCHAR(255) NOT NULL, provider VARCHAR(50) NOT NULL)`).Error)
			require.NoError(t, db.Exec("SET search_path TO "+indexMigrationPGSchema+", "+otherSchema).Error)

			otherIndexExists := func() bool {
				var n int64
				require.NoError(t, db.Raw(`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = ? AND indexname = ?`, otherSchema, index).Scan(&n).Error)
				return n == 1
			}

			require.NoError(t, ensureIndexConcurrently(db, table, index, "virtual_key_id", false))
			require.True(t, otherIndexExists(), "building the index on this schema's table dropped another schema's index")
			require.True(t, indexExists(t, db, table, index), "the index must be built on this schema's table")

			require.NoError(t, db.Exec("DROP INDEX "+indexMigrationPGSchema+"."+index).Error)
			require.NoError(t, vkProviderConfigVirtualKeyIDIndexMigration(ctx, "add_vk_provider_config_virtual_key_id_index").Rollback(db))
			require.True(t, otherIndexExists(), "rolling back with no index on this table dropped another schema's index")
		})
	}
}

// newRunStepsTestDB opens a file-backed SQLite database so every pooled connection sees the same migrations table.
func newRunStepsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "steps.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	return db
}

// countingStep returns a step that records one gormigrate row per ID and bumps calls[id] each time it is invoked.
func countingStep(calls map[string]int, ids ...string) migrationStep {
	return migrationStep{
		IDs: ids,
		run: func(ctx context.Context, db *gorm.DB, _ schemas.Logger) error {
			for _, id := range ids {
				calls[id]++
				m := migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
					ID:      id,
					Migrate: func(*gorm.DB) error { return nil },
				}})
				if err := m.Migrate(); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// TestRunMigrationSteps_FreshDBRunsEveryStep verifies an empty migrations table makes every step run.
func TestRunMigrationSteps_FreshDBRunsEveryStep(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	steps := []migrationStep{countingStep(calls, "a"), countingStep(calls, "b"), countingStep(calls, "c")}

	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), steps))

	assert.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1}, calls)
}

// TestRunMigrationSteps_SkipsAppliedStepsAndRunsPending verifies applied steps are not invoked when a new step is added.
func TestRunMigrationSteps_SkipsAppliedStepsAndRunsPending(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	steps := []migrationStep{countingStep(calls, "a"), countingStep(calls, "b"), countingStep(calls, "c")}
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), steps))

	steps = append(steps, countingStep(calls, "d"))
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), steps))

	assert.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1, "d": 1}, calls)
}

// TestRunMigrationSteps_AllAppliedRunsNothing verifies a fully applied list invokes no step.
func TestRunMigrationSteps_AllAppliedRunsNothing(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	steps := []migrationStep{countingStep(calls, "a"), countingStep(calls, "b")}
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), steps))
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), steps))

	assert.Equal(t, map[string]int{"a": 1, "b": 1}, calls)
}

// TestRunMigrationSteps_GroupedStepRunsWhenOneIDPending verifies a multi-ID step runs if any of its IDs is missing.
func TestRunMigrationSteps_GroupedStepRunsWhenOneIDPending(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), []migrationStep{countingStep(calls, "g1")}))
	calls["g1"] = 0

	grouped := countingStep(calls, "g1", "g2")
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), []migrationStep{grouped}))

	assert.Equal(t, 1, calls["g2"])
	pending, err := pendingMigrationStepIDs(context.Background(), db, []migrationStep{grouped})
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// TestRunMigrationSteps_PreflightErrorRunsEveryStep verifies a failing pending read falls back to running all steps.
func TestRunMigrationSteps_PreflightErrorRunsEveryStep(t *testing.T) {
	db := newRunStepsTestDB(t)
	// A migrations table without the id column passes the column checks in PendingIDs, then fails the read.
	require.NoError(t, db.Exec("CREATE TABLE migrations (sequence INTEGER, applied_at DATETIME, status TEXT)").Error)
	calls := map[string]int{}
	steps := []migrationStep{countingStep(calls, "a"), countingStep(calls, "b")}

	_, err := pendingMigrationStepIDs(context.Background(), db, steps)
	require.Error(t, err)

	// The steps themselves cannot use this broken table, so they only record that they were called.
	stub := func(id string) migrationStep {
		return migrationStep{IDs: []string{id}, run: func(context.Context, *gorm.DB, schemas.Logger) error {
			calls[id]++
			return nil
		}}
	}
	calls = map[string]int{}
	require.NoError(t, runMigrationSteps(context.Background(), db, newMockLogger(), []migrationStep{stub("a"), stub("b")}))

	assert.Equal(t, map[string]int{"a": 1, "b": 1}, calls)
}

// Rolling back add_ignore_provider_cost_column is refused rather than performed:
// the column holds an operator's per-provider choice, and dropping it would
// silently send those providers back to trusting their reported usage.cost.
func TestMigrationAddIgnoreProviderCostColumn_NonRollbackable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&tables.TableProvider{}))
	require.NoError(t, db.Migrator().DropColumn(&tables.TableProvider{}, "ignore_provider_cost"))
	require.NoError(t, migrationAddIgnoreProviderCostColumn(ctx, db, testMigrationLogger))
	require.True(t, db.Migrator().HasColumn(&tables.TableProvider{}, "ignore_provider_cost"))

	require.NoError(t, db.Create(&tables.TableProvider{Name: "cortecs", IgnoreProviderCost: true}).Error)

	err := rollbackIgnoreProviderCostColumn(db, testMigrationLogger)
	require.Error(t, err, "rollback must refuse: dropping the column discards the operator's setting")
	assert.Contains(t, err.Error(), "non-rollbackable")
	assert.True(t, db.Migrator().HasColumn(&tables.TableProvider{}, "ignore_provider_cost"),
		"a refused rollback must leave the column in place")
}

// Directly owned virtual key limits move onto the key's model config tiers.
func TestMigrationMoveConfigJSONVKLimitsToModelConfigs(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualKey{}, &tables.TableVirtualKeyProviderConfig{},
		&tables.TableBudget{}, &tables.TableRateLimit{}, &tables.TableModelConfig{}))

	tokenMax, tokenDur := int64(100), "1d"
	rateLimit := func(id string) *tables.TableRateLimit {
		return &tables.TableRateLimit{ID: id, TokenMaxLimit: &tokenMax, TokenResetDuration: &tokenDur}
	}
	for _, id := range []string{"rl-key", "rl-pc", "rl-file", "rl-tier"} {
		require.NoError(t, db.Create(rateLimit(id)).Error)
	}

	// The shape config.json reconciliation wrote.
	require.NoError(t, db.Create(&tables.TableVirtualKey{
		ID: "vk-cal", Name: "vk-cal", Value: *schemas.NewSecretVar("sk-bf-cal"),
		CalendarAligned: true, RateLimitID: schemas.Ptr("rl-key"),
	}).Error)
	providerConfig := &tables.TableVirtualKeyProviderConfig{
		VirtualKeyID: "vk-cal", Provider: "openai", RateLimitID: schemas.Ptr("rl-pc"),
	}
	require.NoError(t, db.Create(providerConfig).Error)

	// Tier already holds an API-set rate limit while the key still references the file's.
	require.NoError(t, db.Create(&tables.TableVirtualKey{
		ID: "vk-tiered", Name: "vk-tiered", Value: *schemas.NewSecretVar("sk-bf-tiered"),
		RateLimitID: schemas.Ptr("rl-file"),
	}).Error)
	require.NoError(t, db.Create(&tables.TableModelConfig{
		ID: "mc-tiered", ModelName: tables.ModelConfigAllModels, Scope: tables.ModelConfigScopeVirtualKey,
		ScopeID: schemas.Ptr("vk-tiered"), RateLimitID: schemas.Ptr("rl-tier"),
	}).Error)

	lastReset := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	require.NoError(t, db.Create(&tables.TableBudget{
		ID: "b-key", MaxLimit: 10, ResetDuration: "1M", CurrentUsage: 7, LastReset: lastReset,
		VirtualKeyID: schemas.Ptr("vk-cal"),
	}).Error)
	require.NoError(t, db.Create(&tables.TableBudget{
		ID: "b-pc", MaxLimit: 20, ResetDuration: "1M", CurrentUsage: 3, LastReset: lastReset,
		ProviderConfigID: &providerConfig.ID,
	}).Error)
	// Already on a model config.
	require.NoError(t, db.Create(&tables.TableModelConfig{
		ID: "mc-done", ModelName: "gpt-4o", Scope: tables.ModelConfigScopeVirtualKey, ScopeID: schemas.Ptr("vk-cal"),
	}).Error)
	require.NoError(t, db.Create(&tables.TableBudget{
		ID: "b-done", MaxLimit: 5, ResetDuration: "1d", ModelConfigID: schemas.Ptr("mc-done"),
	}).Error)
	// Orphan: its key is gone.
	require.NoError(t, db.Exec(
		`INSERT INTO governance_budgets (id, max_limit, reset_duration, virtual_key_id, created_at, updated_at)
		 VALUES ('b-orphan', 1, '1d', 'vk-gone', ?, ?)`, time.Now(), time.Now()).Error)

	require.NoError(t, migrationMoveConfigJSONVKLimitsToModelConfigs(ctx, db, testMigrationLogger))

	tier := func(vkID string, provider *string) tables.TableModelConfig {
		t.Helper()
		q := db.Where("scope = ? AND scope_id = ? AND model_name = ?",
			tables.ModelConfigScopeVirtualKey, vkID, tables.ModelConfigAllModels)
		if provider == nil {
			q = q.Where("provider IS NULL")
		} else {
			q = q.Where("provider = ?", *provider)
		}
		var rows []tables.TableModelConfig
		require.NoError(t, q.Find(&rows).Error)
		require.Len(t, rows, 1, "expected exactly one all-models tier for %s provider %v", vkID, provider)
		return rows[0]
	}
	budget := func(id string) tables.TableBudget {
		t.Helper()
		var b tables.TableBudget
		require.NoError(t, db.First(&b, "id = ?", id).Error)
		return b
	}

	openai := "openai"
	keyTier, providerTier := tier("vk-cal", nil), tier("vk-cal", &openai)

	// Only the owning FK moves.
	moved := budget("b-key")
	require.NotNil(t, moved.ModelConfigID)
	assert.Equal(t, keyTier.ID, *moved.ModelConfigID)
	assert.Nil(t, moved.VirtualKeyID)
	assert.EqualValues(t, 7, moved.CurrentUsage)
	assert.WithinDuration(t, lastReset, moved.LastReset, time.Second)

	movedPC := budget("b-pc")
	require.NotNil(t, movedPC.ModelConfigID, "a budget owned by a provider config was never migrated before")
	assert.Equal(t, providerTier.ID, *movedPC.ModelConfigID)
	assert.Nil(t, movedPC.ProviderConfigID)
	assert.EqualValues(t, 3, movedPC.CurrentUsage)

	// The tiers carry the key's calendar alignment.
	assert.True(t, keyTier.CalendarAligned)
	assert.True(t, providerTier.CalendarAligned)

	// Rate limits move to their tier; direct references dropped.
	require.NotNil(t, keyTier.RateLimitID)
	assert.Equal(t, "rl-key", *keyTier.RateLimitID)
	require.NotNil(t, providerTier.RateLimitID)
	assert.Equal(t, "rl-pc", *providerTier.RateLimitID)
	var vkCal tables.TableVirtualKey
	require.NoError(t, db.Select("id", "rate_limit_id").First(&vkCal, "id = ?", "vk-cal").Error)
	assert.Nil(t, vkCal.RateLimitID)
	var reloadedPC tables.TableVirtualKeyProviderConfig
	require.NoError(t, db.First(&reloadedPC, "id = ?", providerConfig.ID).Error)
	assert.Nil(t, reloadedPC.RateLimitID)

	// A tier with a rate limit keeps it, and the key keeps the reference this migration did not move.
	tiered := tier("vk-tiered", nil)
	require.NotNil(t, tiered.RateLimitID)
	assert.Equal(t, "rl-tier", *tiered.RateLimitID, "the tier's own rate limit is the one in force")
	var vkTiered tables.TableVirtualKey
	require.NoError(t, db.Select("id", "rate_limit_id").First(&vkTiered, "id = ?", "vk-tiered").Error)
	require.NotNil(t, vkTiered.RateLimitID, "a reference the migration did not relocate was dropped, orphaning the row")
	assert.Equal(t, "rl-file", *vkTiered.RateLimitID)

	// Untouched.
	done := budget("b-done")
	require.NotNil(t, done.ModelConfigID)
	assert.Equal(t, "mc-done", *done.ModelConfigID)
	orphan := budget("b-orphan")
	assert.Nil(t, orphan.ModelConfigID, "a budget whose key is gone was moved onto a model config")
	var orphanTiers int64
	require.NoError(t, db.Model(&tables.TableModelConfig{}).Where("scope_id = ?", "vk-gone").Count(&orphanTiers).Error)
	assert.Zero(t, orphanTiers, "a model config was minted for a key that no longer exists")

	// Idempotent.
	var tiersBefore int64
	require.NoError(t, db.Model(&tables.TableModelConfig{}).Count(&tiersBefore).Error)
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "move_config_json_vk_limits_to_model_configs").Error)
	require.NoError(t, migrationMoveConfigJSONVKLimitsToModelConfigs(ctx, db, testMigrationLogger))
	var tiersAfter int64
	require.NoError(t, db.Model(&tables.TableModelConfig{}).Count(&tiersAfter).Error)
	assert.Equal(t, tiersBefore, tiersAfter, "a re-run created another tier")
}

// TestEnsureVKLimitModelConfigSkipsAMissingKey: the tier is scoped to the key by a column with no
// foreign key, so a key that is gone must yield no tier rather than an orphan the delete cannot reach.
func TestEnsureVKLimitModelConfigSkipsAMissingKey(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&tables.TableVirtualKey{}, &tables.TableModelConfig{}))

	for _, provider := range []*string{nil, schemas.Ptr("openai")} {
		mcID, err := ensureVKLimitModelConfig(db, "vk-gone", provider)
		require.NoError(t, err, "a missing key is not an error, it is nothing to move")
		assert.Empty(t, mcID, "a tier was minted for a key that does not exist")
	}
	var tiers int64
	require.NoError(t, db.Model(&tables.TableModelConfig{}).Where("scope_id = ?", "vk-gone").Count(&tiers).Error)
	assert.Zero(t, tiers, "an orphaned tier was left behind")
}

// TestMigrationAddOAuthKeyConfigColumns_BoundsTheLockWait pins the rolling-upgrade behaviour
// of the oauth column-add on Postgres: ADD COLUMN takes ACCESS EXCLUSIVE on config_keys, so
// while another session holds the table the migration must fail fast with lock_not_available
// and leave the step unapplied, instead of parking at the head of the lock queue where every
// later read and write on the table waits behind it. Skips when Postgres is not reachable.
func TestMigrationAddOAuthKeyConfigColumns_BoundsTheLockWait(t *testing.T) {
	const schema = pgTestSchema + "_oauth_lockwait"
	dsn := strings.Replace(postgresDSN, "search_path="+pgTestSchema, "search_path="+schema, 1)
	pgDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	sqlDB, err := pgDB.DB()
	require.NoError(t, err)
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	require.NoError(t, pgDB.Exec("DROP SCHEMA IF EXISTS "+schema+" CASCADE").Error)
	require.NoError(t, pgDB.Exec("CREATE SCHEMA "+schema).Error)
	t.Cleanup(func() { pgDB.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE") })
	// The pre-upgrade shape: a key table without any oauth_* column, and an empty ledger.
	require.NoError(t, pgDB.Exec(`CREATE TABLE config_keys (id VARCHAR(255) PRIMARY KEY)`).Error)
	require.NoError(t, pgDB.Exec(`CREATE TABLE migrations (id VARCHAR(255) PRIMARY KEY)`).Error)

	// A second session holds the table, as a long transaction would during a rolling upgrade.
	ctx := context.Background()
	holder, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	holderTx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = holderTx.ExecContext(ctx, "LOCK TABLE config_keys IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	released := false
	release := func() {
		if !released {
			released = true
			_ = holderTx.Rollback()
			_ = holder.Close()
		}
	}
	t.Cleanup(release)

	migrationCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	err = migrationAddOAuthKeyConfigColumns(migrationCtx, pgDB, testMigrationLogger)
	waited := time.Since(start)
	require.Error(t, err, "the migration must not succeed while config_keys is locked")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a lock_not_available failure, got %v after %s", err, waited)
	assert.Equal(t, "55P03", pgErr.Code, "expected SQLSTATE 55P03 lock_not_available, got %s: %s", pgErr.Code, pgErr.Message)
	assert.Less(t, waited, 15*time.Second, "the lock wait must be bounded, took %s", waited)

	// Nothing was recorded for the failed attempt, so the next boot retries and succeeds.
	var applied int64
	require.NoError(t, pgDB.Table("migrations").Where("id = ?", "add_oauth_key_config_columns").Count(&applied).Error)
	assert.Zero(t, applied, "a failed column-add must not be recorded as applied")
	release()
	require.NoError(t, migrationAddOAuthKeyConfigColumns(ctx, pgDB, testMigrationLogger))
	for _, column := range oauthKeyConfigColumns {
		assert.True(t, pgDB.Migrator().HasColumn(&tables.TableKey{}, column), "column %s missing after the retry", column)
	}
}

// TestA2ARawCaptureSettingMigration defaults existing deployments to capture disabled.
func TestA2ARawCaptureSettingMigration(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.Exec("CREATE TABLE config_client (id INTEGER PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO config_client (id) VALUES (1)").Error)
	for range 2 {
		require.NoError(t, migrationAddA2ARawCaptureSetting(context.Background(), db, testMigrationLogger))
	}
	require.True(t, db.Migrator().HasColumn(&tables.TableClientConfig{}, "A2AStoreRawRequestResponse"))
	var enabled bool
	require.NoError(t, db.Table("config_client").Select("a2a_store_raw_request_response").Where("id = 1").Scan(&enabled).Error)
	require.False(t, enabled)
}

// TestMigrationAddBatchJobsFileIDIndexes pins that upgraded databases get the indexes the
// file-ownership fallback (GetProviderJobsByFileIDs) filters on, that the migration is safe to
// re-run, and that its rollback drops them.
func TestMigrationAddBatchJobsFileIDIndexes(t *testing.T) {
	for _, ndb := range indexMigrationDBs(t) {
		t.Run(ndb.name, func(t *testing.T) {
			require.NoError(t, ndb.db.Exec(`CREATE TABLE batch_jobs (
				id VARCHAR(512) PRIMARY KEY, kind VARCHAR(50) NOT NULL DEFAULT 'batch', provider VARCHAR(255) NOT NULL,
				input_file_id VARCHAR(255), output_file_id VARCHAR(255), error_file_id VARCHAR(255))`).Error)
			runIndexMigrationTwice(t, ndb.db, "add_batch_jobs_file_id_indexes",
				func(ctx context.Context, db *gorm.DB) error {
					return migrationAddBatchJobsFileIDIndexes(ctx, db, testMigrationLogger)
				},
				"batch_jobs", "idx_batch_jobs_input_file_id")
			require.True(t, indexExists(t, ndb.db, "batch_jobs", "idx_batch_jobs_output_file_id"))
			require.True(t, indexExists(t, ndb.db, "batch_jobs", "idx_batch_jobs_error_file_id"))

			rollback := batchJobsFileIDIndexesMigration(context.Background(), "add_batch_jobs_file_id_indexes").Rollback
			require.NoError(t, rollback(ndb.db))
			for _, index := range []string{"idx_batch_jobs_input_file_id", "idx_batch_jobs_output_file_id", "idx_batch_jobs_error_file_id"} {
				require.False(t, indexExists(t, ndb.db, "batch_jobs", index), "rollback drops %s", index)
			}
			require.NoError(t, rollback(ndb.db), "rollback is safe to re-run")
		})
	}
}

// TestProviderObjectsTableRollbackRefuses pins that rolling back the provider_objects
// migration fails instead of dropping the table: its rows are the only record of which
// virtual key created each file, container and cached content, and without them every
// such object becomes reachable to every key allowed on the provider. The table is
// additive, so an older binary runs fine with it in place.
func TestProviderObjectsTableRollbackRefuses(t *testing.T) {
	db := freshSQLiteDB(t)
	require.NoError(t, db.Migrator().CreateTable(&tables.TableProviderObject{}))

	err := providerObjectsTableMigration(context.Background(), "add_provider_objects_table").Rollback(db)
	require.Error(t, err, "the provider_objects migration must refuse to roll back")
	require.Contains(t, err.Error(), "non-rollbackable")
	require.True(t, db.Migrator().HasTable(&tables.TableProviderObject{}), "a refused rollback must keep the table")
}
