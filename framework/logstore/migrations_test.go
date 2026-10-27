package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/migrator"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestMigrationAddMCPRedactionMappingColumn verifies the MCP mapping column is additive, idempotent, and preserves existing rows.
func TestMigrationAddMCPRedactionMappingColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE mcp_tool_logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO mcp_tool_logs (id) VALUES (?)", "mcp-existing").Error)

	ctx := context.Background()
	require.NoError(t, migrationAddMCPRedactionMappingColumn(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasColumn(&MCPToolLog{}, "RedactionMapping"))
	require.NoError(t, migrationAddMCPRedactionMappingColumn(ctx, db, testLogger{}))

	var count int64
	require.NoError(t, db.Table("mcp_tool_logs").Where("id = ?", "mcp-existing").Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// TestMigrationAddMCPPluginLogsColumn verifies the MCP plugin-log column is additive, idempotent, and preserves existing rows.
func TestMigrationAddMCPPluginLogsColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE mcp_tool_logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO mcp_tool_logs (id) VALUES (?)", "mcp-existing").Error)

	ctx := context.Background()
	require.NoError(t, migrationAddMCPPluginLogsColumn(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasColumn(&MCPToolLog{}, "PluginLogs"))
	require.NoError(t, migrationAddMCPPluginLogsColumn(ctx, db, testLogger{}))

	var count int64
	require.NoError(t, db.Table("mcp_tool_logs").Where("id = ?", "mcp-existing").Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// TestMigrationAddSessionIDColumn verifies that the session lookup
// column is additive, indexed, and preserves pre-existing request logs.
func TestMigrationAddSessionIDColumn(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO logs (id) VALUES (?)", "existing-log").Error)

	ctx := context.Background()
	require.NoError(t, migrationAddSessionIDColumn(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasColumn(&Log{}, "SessionID"))
	require.True(t, db.Migrator().HasIndex(&Log{}, "idx_logs_session_id"))
	require.NoError(t, migrationAddSessionIDColumn(ctx, db, testLogger{}))

	var count int64
	require.NoError(t, db.Table("logs").Where("id = ?", "existing-log").Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// pgTestSchema is this package's dedicated Postgres schema. Test packages
// (configstore, configstore/tables, logstore) run in parallel against the same
// database, so each one works in its own schema to avoid clobbering the
// others' tables and rows.
const pgTestSchema = "logstore_test"

// postgresDSN matches the postgres service in tests/docker-compose.yml and
// framework/docker-compose.yml.
const postgresDSN = "host=localhost user=bifrost password=bifrost_password dbname=bifrost port=5432 sslmode=disable search_path=" + pgTestSchema

// trySetupPostgresDB attempts to connect to Postgres and returns the connection.
// Returns nil if Postgres is unavailable.
func trySetupPostgresDB(t *testing.T) *gorm.DB {
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
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.Ping(); err != nil {
		return nil
	}

	// All objects live in this package's dedicated schema (via search_path in
	// the DSN), isolated from other test packages sharing the same database.
	if err := db.Exec("CREATE SCHEMA IF NOT EXISTS " + pgTestSchema).Error; err != nil {
		return nil
	}

	return db
}

// setupLogsTableForGINIndexTest creates the logs table in a pre-migration state
// (with metadata column but without the GIN index) for testing the GIN index migration.
func setupLogsTableForGINIndexTest(t *testing.T, db *gorm.DB) {
	t.Helper()

	// Drop existing tables and migration tracking in the correct order.
	// Preserve the shared migrations table — only clear its rows.
	dropAllManagedMatViews(db)
	db.Exec("DROP INDEX IF EXISTS idx_logs_metadata_gin")
	db.Exec("DROP TABLE IF EXISTS logs")
	db.Exec("CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)")
	db.Exec("DELETE FROM migrations")

	// Create a minimal logs table with only the columns needed for the test
	err := db.Exec(`
		CREATE TABLE logs (
			id VARCHAR(255) PRIMARY KEY,
			timestamp TIMESTAMP NOT NULL,
			object_type VARCHAR(255) NOT NULL,
			provider VARCHAR(255) NOT NULL,
			model VARCHAR(255) NOT NULL,
			status VARCHAR(50) NOT NULL,
			metadata TEXT,
			created_at TIMESTAMP NOT NULL
		)
	`).Error
	require.NoError(t, err, "Failed to create logs table")

	// The migrator will create the migrations table automatically when it runs

	// Clean up tables after the test
	t.Cleanup(func() {
		dropAllManagedMatViews(db)
		db.Exec("DROP INDEX IF EXISTS idx_logs_metadata_gin")
		db.Exec("DROP TABLE IF EXISTS logs")
		db.Exec("DELETE FROM migrations")
	})
}

func dropAllManagedMatViews(db *gorm.DB) {
	for _, view := range allMatViewNames() {
		db.Exec("DROP MATERIALIZED VIEW IF EXISTS " + view + " CASCADE")
	}
	for _, view := range legacyMatViewNames {
		db.Exec("DROP MATERIALIZED VIEW IF EXISTS " + view + " CASCADE")
	}
}

// insertTestLog inserts a test log entry with the given metadata value.
func insertTestLog(t *testing.T, db *gorm.DB, id string, metadata *string) {
	t.Helper()
	now := time.Now()

	var metadataVal interface{}
	if metadata != nil {
		metadataVal = *metadata
	}

	err := db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, now, "chat_completion", "openai", "gpt-4", "success", metadataVal, now).Error
	require.NoError(t, err, "Failed to insert test log %s", id)
}

// getMetadataValue retrieves the metadata value for a given log ID.
func getMetadataValue(t *testing.T, db *gorm.DB, id string) *string {
	t.Helper()
	var result struct {
		Metadata *string
	}
	err := db.Table("logs").Select("metadata").Where("id = ?", id).Scan(&result).Error
	require.NoError(t, err, "Failed to get metadata for log %s", id)
	return result.Metadata
}

// indexExists checks if the GIN index exists on the logs table.
func indexExists(t *testing.T, db *gorm.DB, indexName string) bool {
	t.Helper()
	var count int64
	err := db.Raw(`
		SELECT COUNT(*) FROM pg_indexes 
		WHERE tablename = 'logs' AND indexname = ?
	`, indexName).Scan(&count).Error
	require.NoError(t, err, "Failed to check index existence")
	return count > 0
}

func TestMigrationAddMetadataGINIndex_ValidJSON(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}

	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()

	// Insert logs with valid JSON object metadata (arrays are not supported)
	validJSON1 := `{"key": "value"}`
	validJSON2 := `{"nested": {"foo": "bar"}, "array": [1, 2, 3]}`
	validJSON3 := `{"empty": {}}`
	validJSON4 := `{"number": 42, "bool": true, "null": null}`

	insertTestLog(t, db, "log-valid-1", &validJSON1)
	insertTestLog(t, db, "log-valid-2", &validJSON2)
	insertTestLog(t, db, "log-valid-3", &validJSON3)
	insertTestLog(t, db, "log-valid-4", &validJSON4)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("Failed to get SQL DB: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Failed to get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Run the migration (cleanup only) then ensure the index is built.
	err = migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "Migration should succeed")
	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "GIN index creation should succeed")

	// Verify all valid JSON object values are preserved
	meta1 := getMetadataValue(t, db, "log-valid-1")
	assert.NotNil(t, meta1, "Valid JSON object should be preserved")
	assert.Equal(t, validJSON1, *meta1)

	meta2 := getMetadataValue(t, db, "log-valid-2")
	assert.NotNil(t, meta2, "Valid JSON object should be preserved")
	assert.Equal(t, validJSON2, *meta2)

	meta3 := getMetadataValue(t, db, "log-valid-3")
	assert.NotNil(t, meta3, "Valid JSON object with nested empty object should be preserved")
	assert.Equal(t, validJSON3, *meta3)

	meta4 := getMetadataValue(t, db, "log-valid-4")
	assert.NotNil(t, meta4, "Valid JSON object with various types should be preserved")
	assert.Equal(t, validJSON4, *meta4)

	// Verify the GIN index was created
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should be created")
}

func TestMigrationAddMetadataGINIndex_InvalidJSON(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}

	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()

	// Insert logs with invalid JSON metadata (not valid JSON objects)
	invalid1 := `{"key": invalid}`     // Unquoted value
	invalid2 := `{key: "value"}`       // Unquoted key
	invalid3 := `{"key": "value",}`    // Trailing comma
	invalid4 := `just a string`        // Plain text
	invalid5 := ``                     // Empty string
	invalid6 := `{"unclosed": "brace"` // Unclosed brace
	invalid7 := `{"key": undefined}`   // JavaScript undefined
	invalid8 := `{'single': 'quotes'}` // Single quotes
	invalid9 := `[NULL]`               // Literal string [NULL] (not valid JSON)
	invalid10 := `NULL`                // Literal string NULL (not valid JSON)
	invalid11 := `null`                // Valid JSON but not a JSON object
	invalid12 := `[1, 2, 3]`           // Valid JSON array but not a JSON object

	insertTestLog(t, db, "log-invalid-1", &invalid1)
	insertTestLog(t, db, "log-invalid-2", &invalid2)
	insertTestLog(t, db, "log-invalid-3", &invalid3)
	insertTestLog(t, db, "log-invalid-4", &invalid4)
	insertTestLog(t, db, "log-invalid-5", &invalid5)
	insertTestLog(t, db, "log-invalid-6", &invalid6)
	insertTestLog(t, db, "log-invalid-7", &invalid7)
	insertTestLog(t, db, "log-invalid-8", &invalid8)
	insertTestLog(t, db, "log-invalid-9", &invalid9)
	insertTestLog(t, db, "log-invalid-10", &invalid10)
	insertTestLog(t, db, "log-invalid-11", &invalid11)
	insertTestLog(t, db, "log-invalid-12", &invalid12)
	insertTestLog(t, db, "log-actual-null", nil) // Actual SQL NULL
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("Failed to get SQL DB: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Failed to get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// Run the migration (cleanup only) then ensure the index is built.
	err = migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "Migration should succeed even with invalid JSON")
	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "GIN index creation should succeed after invalid JSON cleanup")

	// Verify all non-object values were set to NULL (only JSON objects are supported)
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("log-invalid-%d", i)
		meta := getMetadataValue(t, db, id)
		assert.Nil(t, meta, "Non-object JSON for %s should be set to NULL", id)
	}

	// Verify actual SQL NULL remains NULL
	metaActualNull := getMetadataValue(t, db, "log-actual-null")
	assert.Nil(t, metaActualNull, "Actual NULL should remain NULL")

	// Verify the GIN index was created
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should be created")
}

func TestMigrationAddMetadataGINIndex_MixedData(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}

	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()

	// Insert a mix of valid JSON, invalid JSON, and NULL metadata
	validJSON := `{"environment": "production", "version": "1.0.0"}`
	invalidJSON := `{"broken": invalid_value}`

	insertTestLog(t, db, "log-mixed-valid", &validJSON)
	insertTestLog(t, db, "log-mixed-invalid", &invalidJSON)
	insertTestLog(t, db, "log-mixed-null", nil)

	// Run the migration (cleanup only) then ensure the index is built.
	err := migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "Migration should succeed")

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("Failed to get SQL DB: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Failed to get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "GIN index creation should succeed")

	// Verify valid JSON is preserved
	metaValid := getMetadataValue(t, db, "log-mixed-valid")
	assert.NotNil(t, metaValid, "Valid JSON should be preserved")
	assert.Equal(t, validJSON, *metaValid)

	// Verify invalid JSON is cleaned to NULL
	metaInvalid := getMetadataValue(t, db, "log-mixed-invalid")
	assert.Nil(t, metaInvalid, "Invalid JSON should be set to NULL")

	// Verify NULL remains NULL
	metaNull := getMetadataValue(t, db, "log-mixed-null")
	assert.Nil(t, metaNull, "NULL metadata should remain NULL")

	// Verify the GIN index was created
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should be created")
}

func TestMigrationAddMetadataGINIndex_Idempotent(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}

	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()

	// Insert a log with valid JSON
	validJSON := `{"test": "idempotent"}`
	insertTestLog(t, db, "log-idempotent", &validJSON)

	// Run the migration (cleanup only) then ensure the index is built.
	err := migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "First migration should succeed")

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("Failed to get SQL DB: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Failed to get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "GIN index creation should succeed")

	// Verify index exists
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should exist after first migration")

	// Verify metadata is preserved
	meta1 := getMetadataValue(t, db, "log-idempotent")
	assert.NotNil(t, meta1)
	assert.Equal(t, validJSON, *meta1)

	// Run the migration second time (should be idempotent due to gomigrate tracking)
	err = migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "Second migration should succeed (idempotent)")
	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "ensureMetadataGINIndex should be a no-op when index already exists")

	// Verify index still exists
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should exist after second migration")

	// Verify metadata is still preserved
	meta2 := getMetadataValue(t, db, "log-idempotent")
	assert.NotNil(t, meta2)
	assert.Equal(t, validJSON, *meta2)
}

func TestMigrationAddMetadataGINIndex_EmptyTable(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}

	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()

	// Run the migration (cleanup only) then ensure the index is built.
	err := migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "Migration should succeed on empty table")

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("Failed to get SQL DB: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Failed to get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "GIN index creation should succeed on empty table")

	// Verify the GIN index was created
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should be created even on empty table")
}

func TestMigrationAddMetadataGINIndex_EdgeCases(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}

	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()

	// Test edge cases that might be tricky (only JSON objects are supported)
	emptyObject := `{}`
	emptyArray := `[]`                       // Not a JSON object, should be nullified
	whitespaceJSON := `  {"key": "value"}  ` // Valid JSON with surrounding whitespace
	unicodeJSON := `{"emoji": "🎉", "chinese": "中文"}`
	largeNumber := `{"bignum": 99999999999999999999}`
	scientificNotation := `{"sci": 1.23e10}`

	insertTestLog(t, db, "log-edge-empty-obj", &emptyObject)
	insertTestLog(t, db, "log-edge-empty-arr", &emptyArray)
	insertTestLog(t, db, "log-edge-whitespace", &whitespaceJSON)
	insertTestLog(t, db, "log-edge-unicode", &unicodeJSON)
	insertTestLog(t, db, "log-edge-large-num", &largeNumber)
	insertTestLog(t, db, "log-edge-scientific", &scientificNotation)

	// Run the migration (cleanup only) then ensure the index is built.
	err := migrationAddMetadataGINIndex(ctx, db, testLogger{})
	require.NoError(t, err, "Migration should succeed")

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("Failed to get SQL DB: %v", err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("Failed to get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	err = ensureMetadataGINIndex(ctx, conn)
	require.NoError(t, err, "GIN index creation should succeed")

	// Verify all edge cases are handled correctly
	// Empty object should be preserved, but empty array is not a JSON object
	assert.NotNil(t, getMetadataValue(t, db, "log-edge-empty-obj"), "Empty object should be preserved")
	assert.Nil(t, getMetadataValue(t, db, "log-edge-empty-arr"), "Empty array should be nullified (not a JSON object)")

	// Whitespace JSON should be preserved (Postgres handles it)
	meta := getMetadataValue(t, db, "log-edge-whitespace")
	assert.NotNil(t, meta, "Whitespace JSON object should be preserved")

	// Unicode should be preserved
	assert.NotNil(t, getMetadataValue(t, db, "log-edge-unicode"), "Unicode JSON object should be preserved")

	// Large numbers and scientific notation should be preserved
	assert.NotNil(t, getMetadataValue(t, db, "log-edge-large-num"), "Large number JSON object should be preserved")
	assert.NotNil(t, getMetadataValue(t, db, "log-edge-scientific"), "Scientific notation JSON object should be preserved")

	// Verify the GIN index was created
	assert.True(t, indexExists(t, db, "idx_logs_metadata_gin"), "GIN index should be created")
}

// TestCorrelationColumnsMigration verifies that all correlation columns are additive,
// indexed, idempotent, and preserve rows written before the migration.
func TestCorrelationColumnsMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("CREATE TABLE mcp_tool_logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO logs (id) VALUES (?)", "log-existing").Error)
	require.NoError(t, db.Exec("INSERT INTO mcp_tool_logs (id) VALUES (?)", "mcp-existing").Error)

	ctx := context.Background()
	require.NoError(t, migrationCreateAgentLogsTable(ctx, db, testLogger{}))
	for _, column := range []struct {
		model any
		field string
		index string
	}{
		{&Log{}, "AgentCorrelationID", "idx_logs_agent_correlation_id"},
		{&MCPToolLog{}, "SessionID", "idx_mcp_logs_session_id"},
		{&MCPToolLog{}, "AgentCorrelationID", "idx_mcp_logs_agent_correlation_id"},
	} {
		require.True(t, db.Migrator().HasColumn(column.model, column.field), "missing %s", column.field)
		require.True(t, db.Migrator().HasIndex(column.model, column.index), "missing %s", column.index)
	}
	require.NoError(t, migrationCreateAgentLogsTable(ctx, db, testLogger{}))

	var logCount, mcpCount int64
	require.NoError(t, db.Table("logs").Where("id = ?", "log-existing").Count(&logCount).Error)
	require.NoError(t, db.Table("mcp_tool_logs").Where("id = ?", "mcp-existing").Count(&mcpCount).Error)
	assert.Equal(t, int64(1), logCount)
	assert.Equal(t, int64(1), mcpCount)
}

func TestAgentLogsMigrationRollbackGuardsRecordedHistory(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, db.Exec("CREATE TABLE logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("CREATE TABLE mcp_tool_logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, migrationCreateAgentLogsTable(ctx, db, testLogger{}))
	require.NoError(t, rollbackAgentLogsMigration(db, testLogger{}))
	assert.False(t, db.Migrator().HasTable(&AgentLog{}), "empty Agent history is safe to drop")

	require.NoError(t, db.AutoMigrate(&AgentLog{}))
	require.NoError(t, db.Create(&AgentLog{
		ID:         "agent-log-1",
		Timestamp:  time.Now(),
		RecordKind: "request",
		Status:     "completed",
		AgentName:  "test-agent",
		RequestID:  "request-1",
	}).Error)

	err = rollbackAgentLogsMigration(db, testLogger{})
	require.Error(t, err, "rollback must refuse while recorded Agent history exists")
	assert.Contains(t, err.Error(), "agent_logs_init is non-rollbackable")
	assert.True(t, db.Migrator().HasTable(&AgentLog{}), "a refused rollback must leave the table intact")

	var surviving int64
	require.NoError(t, db.Model(&AgentLog{}).Count(&surviving).Error)
	assert.EqualValues(t, 1, surviving, "the recorded Agent history must survive")
}

func TestAgentLogsMigrationRollbackGuardsCorrelationData(t *testing.T) {
	tests := []struct {
		name   string
		table  string
		column string
	}{
		{name: "LLM Agent correlation", table: "logs", column: "agent_correlation_id"},
		{name: "MCP session", table: "mcp_tool_logs", column: "session_id"},
		{name: "MCP Agent correlation", table: "mcp_tool_logs", column: "agent_correlation_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			require.NoError(t, db.Exec("CREATE TABLE logs (id TEXT PRIMARY KEY)").Error)
			require.NoError(t, db.Exec("CREATE TABLE mcp_tool_logs (id TEXT PRIMARY KEY)").Error)
			require.NoError(t, migrationCreateAgentLogsTable(context.Background(), db, testLogger{}))
			require.NoError(t, db.Table(tt.table).Create(map[string]interface{}{"id": "correlated-log", tt.column: "correlation-1"}).Error)

			err = rollbackAgentLogsMigration(db, testLogger{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "agent_logs_init is non-rollbackable")
			assert.Contains(t, err.Error(), tt.table+"."+tt.column)
			assert.True(t, db.Migrator().HasTable(&AgentLog{}), "a refused rollback must leave the Agent log table intact")
			assert.True(t, db.Migrator().HasColumn(tt.table, tt.column), "a refused rollback must leave the populated correlation column intact")
		})
	}
}

// TestPerformanceIndexesCoverProjectIDs pins the project indexes to the background builders: the
// project column migrations add only the columns (addColumnIfNotExists never creates a field's
// index), so an upgraded deployment scans logs by project_id unindexed unless a builder carries
// them. logs gets the (project_id, timestamp) composite from ensureOwnerTimestampIndexes;
// mcp_tool_logs keeps its single-column entry in ensurePerformanceIndexes. A fresh database gets
// them from the model's index tags at table creation.
func TestPerformanceIndexesCoverProjectIDs(t *testing.T) {
	tables := map[string]string{}
	for _, idx := range performanceIndexes {
		tables[idx.name] = idx.table
	}
	for _, idx := range rankingNameTimestampIndexes {
		tables[idx.name] = idx.table
	}
	assert.Equal(t, "logs", tables["idx_logs_project_ts"])
	assert.Equal(t, "mcp_tool_logs", tables["idx_mcp_logs_project_id"])
}

// TestPerformanceIndexesHaveNoDuplicateColumns keeps the background builder from building the
// same index twice under two names: a second index on the same columns costs a full build on
// every upgraded deployment and a write on every insert, and the planner still picks one. A
// partial index counts as a duplicate of a full one on the same columns, since an equality
// filter on the column already implies its IS NOT NULL predicate.
func TestPerformanceIndexesHaveNoDuplicateColumns(t *testing.T) {
	onColumns := regexp.MustCompile(`(?i)\bON\s+(\w+)\s*(USING\s+\w+\s*)?\(([^)]*)\)`)
	seen := map[string]string{}
	for _, idx := range performanceIndexes {
		match := onColumns.FindStringSubmatch(idx.sql)
		require.NotNil(t, match, "cannot read the columns of %s from %q", idx.name, idx.sql)
		key := strings.ToLower(strings.TrimSpace(match[1]+" "+strings.TrimSpace(match[2])) + "(" + strings.Join(strings.Fields(match[3]), " ") + ")")
		if other, ok := seen[key]; ok {
			t.Errorf("%s and %s both index %s", other, idx.name, key)
			continue
		}
		seen[key] = idx.name
	}
}

func TestPerformanceIndexesCoverCorrelationIDs(t *testing.T) {
	tables := map[string]string{}
	for _, idx := range performanceIndexes {
		tables[idx.name] = idx.table
	}
	assert.Equal(t, "logs", tables["idx_logs_agent_correlation_id"])
	assert.Equal(t, "mcp_tool_logs", tables["idx_mcp_logs_session_id"])
	assert.Equal(t, "mcp_tool_logs", tables["idx_mcp_logs_agent_correlation_id"])
}

// TestMigrationAddMCPGovernanceSnapshots verifies the attribution columns are
// additive, idempotent, and leave rows written before them intact — those rows
// keep their bare ids, which is the accepted cost of not rewriting history.
func TestMigrationAddMCPGovernanceSnapshots(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE mcp_tool_logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO mcp_tool_logs (id) VALUES (?)", "mcp-existing").Error)

	ctx := context.Background()
	require.NoError(t, migrationAddMCPGovernanceSnapshots(ctx, db, testLogger{}))
	for _, field := range []string{
		"UserName", "TeamName", "CustomerName", "BusinessUnitName",
		"TeamIDs", "TeamNames", "CustomerIDs", "CustomerNames",
		"BusinessUnitIDs", "BusinessUnitNames", "BudgetIDs", "RateLimitIDs",
	} {
		require.True(t, db.Migrator().HasColumn(&MCPToolLog{}, field), "missing column for %s", field)
	}
	require.NoError(t, migrationAddMCPGovernanceSnapshots(ctx, db, testLogger{}))

	var count int64
	require.NoError(t, db.Table("mcp_tool_logs").Where("id = ?", "mcp-existing").Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// TestMCPGovernanceSnapshotsMigrationIsRegistered keeps the migration reachable:
// an unregistered step leaves the columns missing on every real deployment while
// every unit test that calls it directly still passes.
func TestMCPGovernanceSnapshotsMigrationIsRegistered(t *testing.T) {
	for _, step := range logstoreMigrationSteps {
		for _, id := range step.IDs {
			if id == "mcp_tool_logs_add_governance_snapshots" {
				return
			}
		}
	}
	t.Fatal("mcp_tool_logs_add_governance_snapshots is not registered in logstoreMigrationSteps")
}

// TestMigrationAddWarpConversationTables_NonRollbackable pins that rolling the
// history tables back is refused while they hold anything. warp_conversations
// and warp_messages are persistent user content - saved chats someone can
// reopen - so dropping them is not a schema reversal, it is deleting the data.
// An empty pair is still safe to drop, which keeps a failed upgrade reversible.
func TestMigrationAddWarpConversationTables_NonRollbackable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, migrationAddWarpConversationTables(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasTable(&WarpConversation{}))
	require.True(t, db.Migrator().HasTable(&WarpMessage{}))

	// Empty: the rollback is a genuine reversal and must be allowed.
	require.NoError(t, rollbackWarpConversationTables(db))
	require.False(t, db.Migrator().HasTable(&WarpConversation{}),
		"an empty history is safe to drop")

	// Re-create via AutoMigrate, not the migration: migration ids are write-once,
	// so a second run of the same id is recorded as already applied and does
	// nothing. Then seed a saved conversation, so the drop would destroy content.
	require.NoError(t, db.AutoMigrate(&WarpConversation{}, &WarpMessage{}))
	require.NoError(t, db.Create(&WarpConversation{
		ID: "c-1", OwnerID: "u-1", Title: "how much did we spend?",
	}).Error)

	err = rollbackWarpConversationTables(db)
	require.Error(t, err, "rollback must refuse while saved conversations exist")
	assert.Contains(t, err.Error(), "non-rollbackable")
	assert.True(t, db.Migrator().HasTable(&WarpConversation{}),
		"a refused rollback must leave the table intact")

	var surviving int64
	require.NoError(t, db.Model(&WarpConversation{}).Count(&surviving).Error)
	assert.EqualValues(t, 1, surviving, "the saved conversation must survive")
}

// TestMigrationAddWarpMessageOutcomeColumns_RollbackGuardsRecordedUsage pins
// that rolling the outcome columns back is refused once they hold anything.
//
// These are not pure schema. finish_reason is what marks an answer partial, and
// total_tokens/cost are the recorded spend for a saved chat, so dropping a
// populated set destroys a record rather than reversing a migration. Empty is
// still reversible, which keeps a failed upgrade recoverable.
func TestMigrationAddWarpMessageOutcomeColumns_RollbackGuardsRecordedUsage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, db.AutoMigrate(&WarpConversation{}, &WarpMessage{}))
	require.NoError(t, migrationAddWarpMessageOutcomeColumns(ctx, db, testLogger{}))
	for _, column := range warpMessageOutcomeColumns {
		require.True(t, db.Migrator().HasColumn(&WarpMessage{}, column.column))
	}

	// A message with no recorded outcome: the rollback is a genuine reversal.
	require.NoError(t, db.Create(&WarpMessage{ID: "m-plain", ConversationID: "c-1", Role: "user", Content: "q"}).Error)
	require.NoError(t, rollbackWarpMessageOutcomeColumns(db))
	require.False(t, db.Migrator().HasColumn(&WarpMessage{}, "cost"),
		"columns holding nothing are safe to drop")

	// Re-add them, then record a turn's spend. Now the drop would destroy it.
	require.NoError(t, db.AutoMigrate(&WarpMessage{}))
	require.NoError(t, db.Create(&WarpMessage{
		ID: "m-answer", ConversationID: "c-1", Role: "assistant", Content: "a",
		FinishReason: "partial", TotalTokens: 1200, Cost: 0.042,
	}).Error)

	err = rollbackWarpMessageOutcomeColumns(db)
	require.Error(t, err, "rollback must refuse while a recorded outcome exists")
	assert.Contains(t, err.Error(), "non-rollbackable")
	assert.True(t, db.Migrator().HasColumn(&WarpMessage{}, "cost"),
		"a refused rollback must leave the columns intact")

	var got WarpMessage
	require.NoError(t, db.Where("id = ?", "m-answer").First(&got).Error)
	assert.Equal(t, 1200, got.TotalTokens, "the recorded usage must survive")
	assert.InDelta(t, 0.042, got.Cost, 1e-9)
}

// The cross-owner sweep needs an index it can actually use.
//
// DeleteWarpConversationsOlderThan filters on updated_at alone, and the
// composite index leads with owner_id, so it cannot serve a bare range lookup -
// the sweep scans the whole table every hour on a deployment where most rows
// are not stale. AutoMigrate only creates the composite one, and the table
// migration is write-once, so existing installs never get this without a
// migration of its own.
func TestMigrationAddWarpConversationsUpdatedAtIndex(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migrations.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, migrationAddWarpConversationTables(ctx, db, testLogger{}))
	require.NoError(t, migrationAddWarpConversationsUpdatedAtIndex(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasIndex(&WarpConversation{}, "idx_warp_conversations_updated_at"),
		"the retention sweep filters on updated_at alone and needs it leading")

	// Idempotent: the helper runs on every boot and must not fail on an index
	// that is already there.
	require.NoError(t, migrationAddWarpConversationsUpdatedAtIndex(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasIndex(&WarpConversation{}, "idx_warp_conversations_updated_at"))
}

// sqlRecorder captures every statement gorm executes, so a test can assert on
// statement order - here, that rollback takes its locks in writer order.
type sqlRecorder struct {
	logger.Interface
	statements []string
}

func (r *sqlRecorder) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sql, _ := fc()
	r.statements = append(r.statements, sql)
}

// The rollback must lock warp_conversations before warp_messages.
//
// AppendWarpMessages locks the conversation row first and then inserts
// messages, so a rollback that grabs ACCESS EXCLUSIVE on warp_messages first
// acquires the same two tables in the opposite order - a textbook PostgreSQL
// deadlock, with one side aborted by the server. The drop order stays
// child-before-parent; only the lock order follows the writers.
func TestWarpRollbackLocksTablesInWriterOrder(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	ctx := context.Background()

	// The table migration is ID-gated in the shared migrations table, and this
	// test calls the rollback function directly (no RollbackLast), so the ID row
	// must be cleared or a repeat run skips creation and finds no tables.
	db.Exec("DROP TABLE IF EXISTS warp_messages")
	db.Exec("DROP TABLE IF EXISTS warp_conversations")
	db.Exec("DELETE FROM migrations WHERE id = 'logs_add_warp_conversation_tables'")
	require.NoError(t, migrationAddWarpConversationTables(ctx, db, testLogger{}))
	t.Cleanup(func() {
		db.Exec("DROP TABLE IF EXISTS warp_messages")
		db.Exec("DROP TABLE IF EXISTS warp_conversations")
		db.Exec("DELETE FROM migrations WHERE id = 'logs_add_warp_conversation_tables'")
	})

	recorder := &sqlRecorder{Interface: logger.Default.LogMode(logger.Silent)}
	session := db.Session(&gorm.Session{Logger: recorder})
	require.NoError(t, session.Transaction(rollbackWarpConversationTables))

	conversationsLock, messagesLock := -1, -1
	for i, sql := range recorder.statements {
		switch sql {
		case "LOCK TABLE warp_conversations IN ACCESS EXCLUSIVE MODE":
			conversationsLock = i
		case "LOCK TABLE warp_messages IN ACCESS EXCLUSIVE MODE":
			messagesLock = i
		}
	}
	require.NotEqual(t, -1, conversationsLock, "rollback must lock warp_conversations")
	require.NotEqual(t, -1, messagesLock, "rollback must lock warp_messages")
	require.Less(t, conversationsLock, messagesLock,
		"locks must follow writer order (conversation first), or a concurrent append can deadlock the rollback")
}

// ========== Embedding Input Column Migration Tests ==========

// runEmbeddingInputColumnCases pins that the migration only adds the column and leaves existing rows NULL.
func runEmbeddingInputColumnCases(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Exec("DROP TABLE IF EXISTS logs")
	db.Exec("CREATE TABLE IF NOT EXISTS migrations (id VARCHAR(255) PRIMARY KEY)")
	db.Exec("DELETE FROM migrations WHERE id = 'logs_add_embedding_input_column'")
	require.NoError(t, db.Exec(`CREATE TABLE logs (id VARCHAR(255) PRIMARY KEY, object_type VARCHAR(255) NOT NULL, input_history TEXT)`).Error)
	t.Cleanup(func() {
		db.Exec("DROP TABLE IF EXISTS logs")
		db.Exec("DELETE FROM migrations WHERE id = 'logs_add_embedding_input_column'")
	})

	history := `[{"role":"user","content":[{"type":"text","text":"hello"}]}]`
	require.NoError(t, db.Exec("INSERT INTO logs (id, object_type, input_history) VALUES (?, ?, ?)", "emb-1", "embedding", history).Error)

	ctx := context.Background()
	require.NoError(t, migrationAddEmbeddingInputColumn(ctx, db, testLogger{}))
	require.True(t, db.Migrator().HasColumn(&Log{}, "embedding_input"))

	var result struct {
		EmbeddingInput *string `gorm:"column:embedding_input"`
	}
	require.NoError(t, db.Table("logs").Select("embedding_input").Where("id = ?", "emb-1").Scan(&result).Error)
	assert.Nil(t, result.EmbeddingInput, "historical rows are not backfilled; the UI falls back to input_history")

	require.NoError(t, migrationAddEmbeddingInputColumn(ctx, db, testLogger{}), "re-run should be a no-op")
}

func TestMigrationAddEmbeddingInputColumn_Postgres(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	runEmbeddingInputColumnCases(t, db)
}

func TestMigrationAddEmbeddingInputColumn_SQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	runEmbeddingInputColumnCases(t, db)
}

// hfPGIndexValid reports whether a named index exists on table and is valid.
func hfPGIndexValid(t *testing.T, db *gorm.DB, table, name string) bool {
	t.Helper()
	var valid bool
	require.NoError(t, db.Raw(`
		SELECT COALESCE(bool_and(pi.indisvalid), false)
		FROM pg_class pc
		JOIN pg_index pi ON pi.indrelid = pc.oid
		JOIN pg_class ic ON ic.oid = pi.indexrelid
		JOIN pg_namespace n ON n.oid = pc.relnamespace
		WHERE n.nspname = current_schema() AND pc.relname = ? AND ic.relname = ?
	`, table, name).Scan(&valid).Error)
	return valid
}

// hfPGIndexExists reports whether a named index exists in the test schema.
func hfPGIndexExists(t *testing.T, db *gorm.DB, name string) bool {
	t.Helper()
	var n int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?`, name).Scan(&n).Error)
	return n > 0
}

// TestHighFixesPGOwnerTimestampIndexesReplaceSingles simulates an upgraded
// database (single-column owner indexes, no composites) and runs the
// background builder: the composites are built valid and the replaced
// single-column indexes are dropped only afterwards.
func TestHighFixesPGOwnerTimestampIndexesReplaceSingles(t *testing.T) {
	_, db := setupPerfTestDB(t)
	ctx := context.Background()

	for table, names := range hfOwnerTimestampIndexes {
		for _, name := range names {
			require.NoError(t, db.Exec("DROP INDEX IF EXISTS "+name).Error)
		}
		for i, name := range hfReplacedSingleIndexes[table] {
			col := []string{"virtual_key_id", "user_id", "team_id"}[i]
			require.NoError(t, db.Exec("CREATE INDEX IF NOT EXISTS "+name+" ON "+table+"("+col+")").Error)
		}
	}

	conn := acquirePerfTestSQLConn(t, ctx, db)
	require.NoError(t, ensureOwnerTimestampIndexes(ctx, conn, testLogger{}))
	// Idempotent: a second pass with everything already in place is a no-op.
	require.NoError(t, ensureOwnerTimestampIndexes(ctx, conn, testLogger{}))

	for table, names := range hfOwnerTimestampIndexes {
		for _, name := range names {
			assert.True(t, hfPGIndexValid(t, db, table, name), "%s must be built and valid", name)
		}
		for _, name := range hfReplacedSingleIndexes[table] {
			assert.False(t, hfPGIndexExists(t, db, name), "%s must be dropped once its composite is valid", name)
		}
	}

	// The performance-index pass must not rebuild the dropped single-column indexes.
	require.NoError(t, ensurePerformanceIndexes(ctx, conn, testLogger{}))
	for _, names := range hfReplacedSingleIndexes {
		for _, name := range names {
			assert.False(t, hfPGIndexExists(t, db, name), "%s must stay dropped after ensurePerformanceIndexes", name)
		}
	}
}

// TestHighFixesPGRankingNameIndexesReplaceSingles simulates an upgraded
// database (single-column id indexes, no composites) and runs the background
// builder: the ranking-name composites are built valid, the replaced singles
// are dropped only afterwards, and the performance-index pass does not bring
// them back.
func TestHighFixesPGRankingNameIndexesReplaceSingles(t *testing.T) {
	_, db := setupPerfTestDB(t)
	ctx := context.Background()
	for _, idx := range hfRankingNameIndexes {
		require.NoError(t, db.Exec("DROP INDEX IF EXISTS "+idx.name).Error)
		require.NoError(t, db.Exec("CREATE INDEX IF NOT EXISTS "+idx.replaces+" ON logs("+idx.column+")").Error)
	}

	conn := acquirePerfTestSQLConn(t, ctx, db)
	require.NoError(t, ensureOwnerTimestampIndexes(ctx, conn, testLogger{}))
	require.NoError(t, ensureOwnerTimestampIndexes(ctx, conn, testLogger{}))
	require.NoError(t, ensurePerformanceIndexes(ctx, conn, testLogger{}))

	for _, idx := range hfRankingNameIndexes {
		assert.True(t, hfPGIndexValid(t, db, "logs", idx.name), "%s must be built and valid", idx.name)
		assert.False(t, hfPGIndexExists(t, db, idx.replaces), "%s must be dropped once its composite is valid", idx.replaces)
	}
}

// TestHighFixesPGRankingNameFallbackWalksIndexPerID pins that the raw-table
// name fallback reads one row per id through the (id, timestamp) composite,
// newest first, instead of every row the id ever logged: on a planner-realistic
// table its plan has a per-id Limit over idx_logs_selected_key_ts. It still
// returns the latest name across all history. 2,000 keys over 50k rows give the
// planner the per-key selectivity of a real fleet of keys or rules.
func TestHighFixesPGRankingNameFallbackWalksIndexPerID(t *testing.T) {
	store, db := setupPerfTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at, selected_key_id, selected_key_name)
		SELECT 'rn-' || g, ?::timestamp - (g || ' seconds')::interval, 'chat_completion', 'openai', 'gpt-4o', 'success',
		       ?::timestamp, 'key-' || (g % 2000), 'Key old ' || (g % 2000)
		FROM generate_series(1, 50000) AS g
	`, now, now).Error)
	hfInsertNamedLogWithKey(t, db, "rn-latest", now.Add(time.Minute), "key-3", "Key three")
	require.NoError(t, db.Exec("ANALYZE logs").Error)

	capture := hfCaptureQueries(t, db)
	names := store.resolveRankingNames(ctx, "selected_key_id", "selected_key_name", []string{"key-3", "key-missing"})
	assert.Equal(t, map[string]string{"key-3": "Key three"}, names, "the fallback returns the latest name across all history")

	var lookup string
	for _, st := range capture.snapshot() {
		if strings.Contains(st.sql, "selected_key_name") {
			lookup = st.rendered
		}
	}
	require.NotEmpty(t, lookup, "the fallback query was not captured")
	plan := hfExplain(t, db, lookup)
	assert.Contains(t, plan, "Limit", "each id must stop at its newest named row; plan:\n%s", plan)
	assert.Contains(t, plan, "idx_logs_selected_key_ts", "the lookup must walk the (selected_key_id, timestamp) composite; plan:\n%s", plan)
}

// hfInsertNamedLogWithKey writes one logs row carrying a provider key id and name.
func hfInsertNamedLogWithKey(t *testing.T, db *gorm.DB, id string, ts time.Time, keyID, keyName string) {
	t.Helper()
	require.NoError(t, db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at, selected_key_id, selected_key_name)
		VALUES (?, ?, 'chat_completion', 'openai', 'gpt-4o', 'success', ?, ?, ?)
	`, id, ts, ts, keyID, keyName).Error)
}

// hfExplain returns the text plan of query.
func hfExplain(t *testing.T, db *gorm.DB, query string, args ...any) string {
	t.Helper()
	var lines []string
	require.NoError(t, db.Raw("EXPLAIN "+query, args...).Scan(&lines).Error)
	return strings.Join(lines, "\n")
}

// TestHighFixesPGOwnerListUsesComposite pins H4 on a planner-realistic table:
// with 50k rows spread over 50 virtual keys and users, the logs list query
// (owner filter, time window, ORDER BY timestamp DESC, id DESC LIMIT) walks
// the (owner, timestamp) composite instead of a single-column index plus sort.
func TestHighFixesPGOwnerListUsesComposite(t *testing.T) {
	_, db := setupPerfTestDB(t)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at, virtual_key_id, user_id, team_id)
		SELECT 'hf-' || g, ?::timestamp - (g || ' seconds')::interval, 'chat_completion', 'openai', 'gpt-4o', 'success',
		       ?::timestamp, 'vk-' || (g % 50), 'user-' || (g % 50), 'team-' || (g % 50)
		FROM generate_series(1, 50000) AS g
	`, now, now).Error)
	require.NoError(t, db.Exec("ANALYZE logs").Error)

	start := now.Add(-30 * 24 * time.Hour)
	for _, c := range []struct{ col, value, idx string }{
		{"virtual_key_id", "vk-7", "idx_logs_vk_ts"},
		{"user_id", "user-7", "idx_logs_user_ts"},
	} {
		plan := hfExplain(t, db,
			"SELECT id FROM logs WHERE "+c.col+" IN (?) AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp DESC, id DESC LIMIT 50",
			c.value, start, now)
		assert.Contains(t, plan, c.idx, "list filtered by %s must use %s; plan:\n%s", c.col, c.idx, plan)
	}

	require.NoError(t, db.Exec(`
		INSERT INTO mcp_tool_logs (id, request_id, timestamp, tool_name, status, created_at, virtual_key_id, user_id)
		SELECT 'hfm-' || g, 'hfm-' || g, ?::timestamp - (g || ' seconds')::interval, 'search', 'success', ?::timestamp,
		       'vk-' || (g % 50), 'user-' || (g % 50)
		FROM generate_series(1, 50000) AS g
	`, now, now).Error)
	require.NoError(t, db.Exec("ANALYZE mcp_tool_logs").Error)
	plan := hfExplain(t, db,
		"SELECT id FROM mcp_tool_logs WHERE virtual_key_id IN (?) AND timestamp >= ? AND timestamp <= ? ORDER BY timestamp DESC LIMIT 50",
		"vk-7", start, now)
	assert.Contains(t, plan, "idx_mcp_logs_vk_ts", "MCP list filtered by virtual key must use the composite; plan:\n%s", plan)
}

// hfInsertNamedLog writes one logs row with owner ids and names for ranking tests.
func hfInsertNamedLog(t *testing.T, db *gorm.DB, id string, ts time.Time, vkID, vkName, app string) {
	t.Helper()
	require.NoError(t, db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at,
			virtual_key_id, virtual_key_name, app, cost, prompt_tokens, completion_tokens, total_tokens)
		VALUES (?, ?, 'chat_completion', 'openai', 'gpt-4o', 'success', ?, ?, ?, ?, 0.01, 10, 5, 15)
	`, id, ts, ts, vkID, vkName, app).Error)
}

// TestHighFixesPGRankingNamesAvoidFullHistoryScan pins H7: dimension rankings
// served from mv_logs_hourly resolve display names from the dimension's
// mv_filter_* view (or skip the lookup when the id is the name) instead of a
// DISTINCT ON over the whole logs history, and return the same names.
func TestHighFixesPGRankingNamesAvoidFullHistoryScan(t *testing.T) {
	store, db := setupPerfTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	hfInsertNamedLog(t, db, "r1", now.Add(-3*time.Hour), "vk-a", "Alpha", "claude-code")
	hfInsertNamedLog(t, db, "r2", now.Add(-2*time.Hour), "vk-a", "Alpha", "claude-code")
	hfInsertNamedLog(t, db, "r3", now.Add(-2*time.Hour), "vk-b", "Beta", "codex")
	// vk-c was renamed: the latest name wins, as before.
	hfInsertNamedLog(t, db, "r4", now.Add(-5*time.Hour), "vk-c", "Gamma old", "codex")
	hfInsertNamedLog(t, db, "r5", now.Add(-1*time.Hour), "vk-c", "Gamma", "codex")

	refreshTestMatViews(t, db)
	store.matViewsReady.Store(true)

	start := now.Add(-24 * time.Hour)
	end := now
	filters := SearchFilters{StartTime: &start, EndTime: &end}

	capture := hfCaptureQueries(t, db)
	res, err := store.getDimensionRankingsFromMatView(ctx, filters, RankingDimensionVirtualKey)
	require.NoError(t, err)
	names := map[string]string{}
	for _, r := range res.Rankings {
		names[r.ID] = r.Name
	}
	assert.Equal(t, map[string]string{"vk-a": "Alpha", "vk-b": "Beta", "vk-c": "Gamma"}, names)

	// Only the id the view could not name (vk-c, renamed inside the view's
	// window) reaches the raw-table lookup, and it runs exactly once as the
	// unnest ... CROSS JOIN LATERAL walk; the ids the view resolved never do.
	var fallbacks int
	for _, s := range capture.snapshot() {
		if !strings.Contains(s.sql, "CROSS JOIN LATERAL") {
			continue
		}
		fallbacks++
		assert.Contains(t, s.rendered, "vk-c", "the raw lookup is for the id the view could not name: %s", s.rendered)
		assert.NotContains(t, s.rendered, "vk-a", "an id the view named must not reach the raw lookup: %s", s.rendered)
		assert.NotContains(t, s.rendered, "vk-b", "an id the view named must not reach the raw lookup: %s", s.rendered)
	}
	assert.Equal(t, 1, fallbacks, "the raw-table fallback must run exactly once, for vk-c")
	var viewLookups int
	for _, s := range capture.snapshot() {
		if strings.Contains(s.sql, "mv_filter_virtual_keys") {
			viewLookups++
		}
	}
	assert.Equal(t, 1, viewLookups, "names must be read from mv_filter_virtual_keys")

	// app: the id is the name, so there is no name lookup at all.
	capture = hfCaptureQueries(t, db)
	res, err = store.getDimensionRankingsFromMatView(ctx, filters, RankingDimensionApp)
	require.NoError(t, err)
	for _, r := range res.Rankings {
		assert.Equal(t, r.ID, r.Name)
	}
	require.Len(t, res.Rankings, 2)
	for _, s := range capture.snapshot() {
		assert.NotContains(t, s.sql, "CROSS JOIN LATERAL", "app rankings must not look names up: %s", s.sql)
	}
}

// TestHighFixesPGRankingNamesCoverWholeStartHour pins that the raw-table name
// fallback covers the same hour buckets the matview counts. The count includes
// the whole bucket holding StartTime, so an id whose only in-window rows fall
// in [date_trunc('hour', StartTime), StartTime) is ranked and must still get a
// name. vk-d was renamed within the filter view's window, so its name cannot be
// read from mv_filter_virtual_keys and comes from the fallback.
func TestHighFixesPGRankingNamesCoverWholeStartHour(t *testing.T) {
	store, db := setupPerfTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	hourStart := now.Truncate(time.Hour).Add(-3 * time.Hour)

	hfInsertNamedLog(t, db, "d-old", now.Add(-48*time.Hour), "vk-d", "Delta old", "codex")
	hfInsertNamedLog(t, db, "d-new", hourStart.Add(5*time.Minute), "vk-d", "Delta", "codex")

	refreshTestMatViews(t, db)
	store.matViewsReady.Store(true)

	start := hourStart.Add(30 * time.Minute)
	end := now
	res, err := store.getDimensionRankingsFromMatView(ctx, SearchFilters{StartTime: &start, EndTime: &end}, RankingDimensionVirtualKey)
	require.NoError(t, err)
	names := map[string]string{}
	for _, r := range res.Rankings {
		names[r.ID] = r.Name
	}
	require.Contains(t, names, "vk-d", "vk-d is counted through the bucket holding StartTime")
	assert.Equal(t, "Delta", names["vk-d"], "the name fallback must cover the whole StartTime hour the count covers")
}

// TestHighFixesPGRankingNamesFallbackUsesAllHistory pins that when the filter
// view cannot name an id (renamed within its window, so it holds two names), the
// raw-table fallback returns the latest name across all history, as dev did,
// rather than the name inside the requested window. This is the name a deleted
// entity keeps; the handler shows a live entity's current config name instead.
func TestHighFixesPGRankingNamesFallbackUsesAllHistory(t *testing.T) {
	store, db := setupPerfTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	hfInsertNamedLog(t, db, "e-old", now.Add(-10*24*time.Hour), "vk-e", "Echo old", "codex")
	hfInsertNamedLog(t, db, "e-new", now.Add(-1*time.Hour), "vk-e", "Echo", "codex")

	refreshTestMatViews(t, db)
	store.matViewsReady.Store(true)

	start := now.Add(-12 * 24 * time.Hour)
	end := now.Add(-5 * 24 * time.Hour)
	res, err := store.getDimensionRankingsFromMatView(ctx, SearchFilters{StartTime: &start, EndTime: &end}, RankingDimensionVirtualKey)
	require.NoError(t, err)
	names := map[string]string{}
	for _, r := range res.Rankings {
		names[r.ID] = r.Name
	}
	require.Contains(t, names, "vk-e")
	assert.Equal(t, "Echo", names["vk-e"], "the fallback must return the latest name across all history")
}

// hfInsertMetadataLog writes a logs row with raw metadata text at ts.
func hfInsertMetadataLog(t *testing.T, db *gorm.DB, id string, ts time.Time, metadata string) {
	t.Helper()
	require.NoError(t, db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, metadata, created_at)
		VALUES (?, ?, 'chat_completion', 'openai', 'gpt-4', 'success', ?, ?)
	`, id, ts, metadata, ts).Error)
}

// TestHighFixesPGMetadataCleanupRunsOnce pins H8 for the invalid-metadata
// cleanup: one run clears invalid rows across many hours, records completion
// in the migrations ledger, and later boots skip the scan entirely.
func TestHighFixesPGMetadataCleanupRunsOnce(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	setupLogsTableForGINIndexTest(t, db)
	ctx := context.Background()
	now := time.Now().UTC()

	hfInsertMetadataLog(t, db, "bad-old", now.Add(-50*time.Hour), `not json`)
	hfInsertMetadataLog(t, db, "bad-new", now, `[1,2]`)
	hfInsertMetadataLog(t, db, "good", now.Add(-3*time.Hour), `{"k":"v"}`)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	conn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, ensureMetadataGINIndex(ctx, conn))
	assert.Nil(t, getMetadataValue(t, db, "bad-old"))
	assert.Nil(t, getMetadataValue(t, db, "bad-new"))
	require.NotNil(t, getMetadataValue(t, db, "good"))

	var marked int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM migrations WHERE id = ?", invalidMetadataCleanupJobID).Scan(&marked).Error)
	assert.Equal(t, int64(1), marked, "completion must be recorded in the migrations ledger")

	// A row that would have been cleaned is left alone: the second boot does no scan.
	hfInsertMetadataLog(t, db, "bad-after", now, `nope`)
	require.NoError(t, ensureMetadataGINIndex(ctx, conn))
	got := getMetadataValue(t, db, "bad-after")
	require.NotNil(t, got, "a recorded cleanup must not scan logs again")
}

// TestHighFixesPGCachedReadTokensBackfillRunsOnce pins H8 for the
// cached_read_tokens backfill: keyset hour windows cover old and new rows in
// one run, completion is recorded, and a second run does not scan.
func TestHighFixesPGCachedReadTokensBackfillRunsOnce(t *testing.T) {
	_, db := setupPerfTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	insert := func(id string, ts time.Time) {
		require.NoError(t, db.Exec(`
			INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at, token_usage, cached_read_tokens)
			VALUES (?, ?, 'chat_completion', 'openai', 'gpt-4', 'success', ?, '{"prompt_tokens_details":{"cached_read_tokens":7}}', 0)
		`, id, ts, ts).Error)
	}
	insert("c-old", now.Add(-72*time.Hour))
	insert("c-new", now)

	conn := acquirePerfTestSQLConn(t, ctx, db)
	require.NoError(t, backfillCachedReadTokens(ctx, conn))
	read := func(id string) int {
		var v int
		require.NoError(t, db.Raw("SELECT cached_read_tokens FROM logs WHERE id = ?", id).Scan(&v).Error)
		return v
	}
	assert.Equal(t, 7, read("c-old"))
	assert.Equal(t, 7, read("c-new"))

	insert("c-after", now)
	require.NoError(t, backfillCachedReadTokens(ctx, conn))
	assert.Equal(t, 0, read("c-after"), "a recorded backfill must not scan logs again")
}

// TestHighFixesPGLongJobsLiftPoolStatementTimeout pins that the long-running
// jobs on dedicated connections (matview creation and refresh, the background
// index builds and backfills) are not cut by the runtime pool's
// statement_timeout, while the pool itself keeps the timeout: every connection
// handed back afterwards still reports it.
func TestHighFixesPGLongJobsLiftPoolStatementTimeout(t *testing.T) {
	_, db := setupPerfTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, db.Exec(`
		INSERT INTO logs (id, timestamp, object_type, provider, model, status, created_at, virtual_key_id, virtual_key_name, cost, total_tokens)
		SELECT 'hft-' || g, ?::timestamp - ((g % 86400) || ' seconds')::interval, 'chat_completion', 'openai', 'gpt-4o-' || (g % 7), 'success',
		       ?::timestamp, 'vk-' || (g % 97), 'VK ' || (g % 97), 0.01, 15
		FROM generate_series(1, 300000) AS g
	`, now, now).Error)
	require.NoError(t, db.Exec("ANALYZE logs").Error)

	// pgx passes unknown DSN keys as run-time parameters, so every connection of
	// this pool starts with a 40ms statement_timeout, like a production runtime pool.
	tight, err := gorm.Open(postgres.Open(postgresDSN+" statement_timeout=40"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	tightSQL, err := tight.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tightSQL.Close() })

	require.Error(t, tight.Exec("SELECT pg_sleep(0.2)").Error, "the pool timeout must be live")
	require.Error(t, tight.Exec("REFRESH MATERIALIZED VIEW mv_logs_hourly").Error,
		"a refresh over 300k rows must exceed the pool timeout, or this test proves nothing")

	resetTestMatViewRefreshGate()
	require.NoError(t, refreshMatViews(ctx, tight, 0), "refreshMatViews must lift the pool timeout on its connection")

	require.NoError(t, db.Exec("DROP MATERIALIZED VIEW IF EXISTS mv_logs_hourly CASCADE").Error)
	require.NoError(t, ensureMatViews(ctx, tight), "ensureMatViews must lift the pool timeout on its connection")

	require.NoError(t, db.Exec("DROP INDEX IF EXISTS idx_logs_vk_ts").Error)
	buildBackgroundIndexes(ctx, tight, testLogger{})
	assert.True(t, hfPGIndexValid(t, db, "logs", "idx_logs_vk_ts"), "the background builder must finish its index builds")

	// Connections returned to the pool keep the pool's timeout.
	conns := make([]interface{ Close() error }, 0, 3)
	for range 3 {
		c, err := tightSQL.Conn(ctx)
		require.NoError(t, err)
		conns = append(conns, c)
		var setting string
		require.NoError(t, c.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&setting))
		assert.Equal(t, "40ms", setting, "a pooled connection must keep the runtime statement_timeout")
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// TestHighFixesPGOwnerTimestampIndexIgnoresOtherSchemas pins that the builder
// judges an index by the current schema only: a same-named index on a same-named
// table in another schema must not make it skip the build, and the replaced
// single-column index must still be dropped only after the current schema's
// composite is valid.
func TestHighFixesPGOwnerTimestampIndexIgnoresOtherSchemas(t *testing.T) {
	_, db := setupPerfTestDB(t)
	ctx := context.Background()

	const otherSchema = "hf_other_schema"
	require.NoError(t, db.Exec("DROP SCHEMA IF EXISTS "+otherSchema+" CASCADE").Error)
	require.NoError(t, db.Exec("CREATE SCHEMA "+otherSchema).Error)
	t.Cleanup(func() { db.Exec("DROP SCHEMA IF EXISTS " + otherSchema + " CASCADE") })
	require.NoError(t, db.Exec("CREATE TABLE "+otherSchema+".logs (virtual_key_id varchar(255), timestamp timestamp)").Error)
	require.NoError(t, db.Exec("CREATE INDEX idx_logs_vk_ts ON "+otherSchema+".logs(virtual_key_id, timestamp)").Error)

	require.NoError(t, db.Exec("DROP INDEX IF EXISTS idx_logs_vk_ts").Error)
	require.NoError(t, db.Exec("CREATE INDEX IF NOT EXISTS idx_logs_virtual_key_id ON logs(virtual_key_id)").Error)

	conn := acquirePerfTestSQLConn(t, ctx, db)
	require.NoError(t, ensureOwnerTimestampIndexes(ctx, conn, testLogger{}))

	assert.True(t, hfPGIndexValid(t, db, "logs", "idx_logs_vk_ts"), "the composite must be built in the current schema")
	assert.False(t, hfPGIndexExists(t, db, "idx_logs_virtual_key_id"), "the single-column index is dropped once the composite is valid")
	var other int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = ? AND indexname = 'idx_logs_vk_ts'`, otherSchema).Scan(&other).Error)
	assert.EqualValues(t, 1, other, "the other schema's index must be left alone")
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

	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, steps))

	assert.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1}, calls)
}

// TestRunMigrationSteps_SkipsAppliedStepsAndRunsPending verifies applied steps are not invoked when a new step is added.
func TestRunMigrationSteps_SkipsAppliedStepsAndRunsPending(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	steps := []migrationStep{countingStep(calls, "a"), countingStep(calls, "b"), countingStep(calls, "c")}
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, steps))

	steps = append(steps, countingStep(calls, "d"))
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, steps))

	assert.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1, "d": 1}, calls)
}

// TestRunMigrationSteps_AllAppliedRunsNothing verifies a fully applied list invokes no step.
func TestRunMigrationSteps_AllAppliedRunsNothing(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	steps := []migrationStep{countingStep(calls, "a"), countingStep(calls, "b")}
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, steps))
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, steps))

	assert.Equal(t, map[string]int{"a": 1, "b": 1}, calls)
}

// TestRunMigrationSteps_GroupedStepRunsWhenOneIDPending verifies a multi-ID step runs if any of its IDs is missing.
func TestRunMigrationSteps_GroupedStepRunsWhenOneIDPending(t *testing.T) {
	db := newRunStepsTestDB(t)
	calls := map[string]int{}
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, []migrationStep{countingStep(calls, "g1")}))
	calls["g1"] = 0

	grouped := countingStep(calls, "g1", "g2")
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, []migrationStep{grouped}))

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
	require.NoError(t, runMigrationSteps(context.Background(), db, testLogger{}, []migrationStep{stub("a"), stub("b")}))

	assert.Equal(t, map[string]int{"a": 1, "b": 1}, calls)
}

// TestAdvisoryKeyHalves pins how a bigint advisory key maps onto the
// (classid, objid) pair pg_locks reports, which the migration probe queries by.
func TestAdvisoryKeyHalves(t *testing.T) {
	classid, objid := advisoryKeyHalves(migrationAdvisoryLockKey)
	assert.Equal(t, int64(0), classid)
	assert.Equal(t, int64(migrationAdvisoryLockKey), objid)

	classid, objid = advisoryKeyHalves(int64(7)<<32 | 42)
	assert.Equal(t, int64(7), classid)
	assert.Equal(t, int64(42), objid)
}

// TestMigrationLockProbeNonPostgresNeverReportsMigration pins the fail-open
// defaults: no store, no database, or a non-Postgres dialect all read as
// "not migrating" without issuing any query.
func TestMigrationLockProbeNonPostgresNeverReportsMigration(t *testing.T) {
	var nilProbe *migrationLockProbe
	assert.False(t, nilProbe.inProgress(context.Background()))
	assert.False(t, newMigrationLockProbe(nil, testLogger{}).inProgress(context.Background()))

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "probe.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	store := &RDBLogStore{db: db, logger: testLogger{}}
	assert.False(t, store.MigrationInProgress(context.Background()))
	assert.False(t, MigrationInProgressFor(context.Background(), store))
	assert.False(t, MigrationInProgressFor(context.Background(), struct{}{}), "a value without the gate reads as not migrating")
}

// TestMigrationLockProbeCachesWithinTTL pins that one answer is reused for
// maintenanceCheckTTL so many pollers cost the database one query per TTL.
func TestMigrationLockProbeCachesWithinTTL(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	probe := &migrationLockProbe{now: func() time.Time { return now }, checkedAt: now, held: true}
	assert.True(t, probe.inProgress(context.Background()), "a fresh cached answer is served without a database")

	now = now.Add(maintenanceCheckTTL)
	assert.False(t, probe.inProgress(context.Background()), "an expired cache falls through to the (absent) database and fails open")
}

// TestMigrationInProgressTracksAdvisoryLockPostgres pins the live probe: it
// sees the migration lock held by another session, ignores other keys, clears
// when the holder goes away, and fails open when the database is unreachable.
func TestMigrationInProgressTracksAdvisoryLockPostgres(t *testing.T) {
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	store := &RDBLogStore{db: db, logger: testLogger{}}
	fresh := func() bool {
		probe := store.migrationProbe()
		probe.mu.Lock()
		probe.checkedAt = time.Time{}
		probe.mu.Unlock()
		return store.MigrationInProgress(context.Background())
	}

	require.False(t, fresh(), "no lock held yet")

	holderDB, holder := acquireTestAdvisoryLockOnIsolatedPool(t, migrationAdvisoryLockKey)
	require.True(t, fresh(), "migration lock held by another session must be visible")
	assert.True(t, store.MigrationInProgress(context.Background()), "cached answer within the TTL")
	closeTestAdvisoryLockSession(t, holderDB, holder)
	require.Eventually(t, probeReports(store, false), 5*time.Second, 50*time.Millisecond, "lock must clear once the holder's session ends")

	indexDB, indexHolder := acquireTestAdvisoryLockOnIsolatedPool(t, indexAdvisoryLockKey)
	require.False(t, fresh(), "a different advisory key (background index build) must not read as a migration")
	closeTestAdvisoryLockSession(t, indexDB, indexHolder)

	closedDB := trySetupPostgresDB(t)
	require.NotNil(t, closedDB)
	closedSQL, err := closedDB.DB()
	require.NoError(t, err)
	require.NoError(t, closedSQL.Close())
	assert.False(t, newMigrationLockProbe(closedDB, testLogger{}).inProgress(context.Background()), "an unreachable database fails open")
}

// probeReports returns a poll function that resets the probe cache and reports
// whether MigrationInProgress equals want.
func probeReports(store *RDBLogStore, want bool) func() bool {
	return func() bool {
		probe := store.migrationProbe()
		probe.mu.Lock()
		probe.checkedAt = time.Time{}
		probe.mu.Unlock()
		return store.MigrationInProgress(context.Background()) == want
	}
}

// TestMigrationLockProbeCachesFromQueryCompletion pins that the TTL starts when
// the answer arrives, not when the query was issued: a slow probe must not leave
// an already-expired answer for the callers queued behind it, or each of them
// would run its own slow query in turn.
func TestMigrationLockProbeCachesFromQueryCompletion(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	queries := 0
	probe := &migrationLockProbe{
		now: func() time.Time { return now },
		query: func(context.Context) (bool, error) {
			queries++
			now = now.Add(maintenanceCheckTTL + 500*time.Millisecond) // a slow round trip
			return true, nil
		},
	}
	assert.True(t, probe.inProgress(context.Background()))
	assert.True(t, probe.inProgress(context.Background()), "the answer that just arrived is served from cache")
	assert.Equal(t, 1, queries, "a slow probe must not make the next caller query again")
}

// TestMigrationLockProbeWaitersHonourContext pins that a caller whose context
// ends while another caller's query is in flight returns at once (fail open)
// instead of waiting for that probe, so shutdown never waits on the database.
func TestMigrationLockProbeWaitersHonourContext(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()
	probe := &migrationLockProbe{
		now: time.Now,
		query: func(context.Context) (bool, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return true, nil
		},
	}
	first := make(chan bool, 1)
	go func() { first <- probe.inProgress(context.Background()) }()
	<-entered

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan bool, 1)
	go func() { second <- probe.inProgress(cancelled) }()
	select {
	case got := <-second:
		assert.False(t, got, "a caller whose context ended must fail open")
	case <-time.After(time.Second):
		t.Fatal("a caller with an ended context waited for another caller's probe")
	}
	releaseOnce()
	assert.True(t, <-first, "the in-flight probe still completes normally")
}

// TestAgentRawCaptureMigration upgrades existing Agent log tables without replacing rows.
func TestAgentRawCaptureMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "raw-migration.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec("CREATE TABLE agent_logs (id TEXT PRIMARY KEY)").Error)
	require.NoError(t, db.Exec("INSERT INTO agent_logs (id) VALUES (?)", "existing").Error)
	for range 2 {
		require.NoError(t, migrationAddAgentRawPayloadColumns(context.Background(), db, testLogger{}))
	}
	require.True(t, db.Migrator().HasColumn(&AgentLog{}, "RawRequest"))
	require.True(t, db.Migrator().HasColumn(&AgentLog{}, "RawResponse"))
	var count int64
	require.NoError(t, db.Table("agent_logs").Where("id = ?", "existing").Count(&count).Error)
	require.Equal(t, int64(1), count)
}

// TestIndexBuildersIgnoreSameNamedIndexInOtherSchema pins that the index
// builders look for their index on this schema's table. A shared database can
// hold another schema with its own logs tables (another deployment, or a test
// package), and a valid same-named index there made the builders skip their
// build, leaving this schema's table unindexed (so the cost and latency sorts
// seq-scanned). The other schema is also on the search_path after this one, so
// an unqualified DROP INDEX of a missing index would resolve to its copy: the
// builders must leave it alone.
func TestIndexBuildersIgnoreSameNamedIndexInOtherSchema(t *testing.T) {
	const shadow = "logstore_test_shadow"
	_, db := setupPerfTestDB(t)
	ctx := context.Background()

	require.NoError(t, db.Exec("DROP SCHEMA IF EXISTS "+shadow+" CASCADE").Error)
	require.NoError(t, db.Exec("CREATE SCHEMA "+shadow).Error)
	t.Cleanup(func() { _ = db.Exec("DROP SCHEMA IF EXISTS " + shadow + " CASCADE").Error })

	type index struct{ table, name string }
	indexes := []index{
		{"logs", "idx_logs_metadata_gin"},
		{"logs", "idx_logs_team_ids_gin"},
		{"logs", "idx_logs_business_unit_ids_gin"},
		{"logs", "idx_logs_customer_ids_gin"},
		{"logs", "idx_logs_histogram_cover"},
		{"mcp_tool_logs", "idx_mcp_logs_histogram_cover"},
	}
	for _, idx := range performanceIndexes {
		indexes = append(indexes, index{idx.table, idx.name})
	}

	// A valid same-named index in the shadow schema, and none in this one.
	for _, idx := range indexes {
		require.NoError(t, db.Exec(fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.%s (id text)", shadow, idx.table)).Error)
		require.NoError(t, db.Exec(fmt.Sprintf("CREATE INDEX %s ON %s.%s (id)", idx.name, shadow, idx.table)).Error)
		require.NoError(t, db.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s.%s", pgTestSchema, idx.name)).Error)
	}

	conn := acquirePerfTestSQLConn(t, ctx, db)
	_, err := conn.ExecContext(ctx, fmt.Sprintf("SET search_path TO %s, %s", pgTestSchema, shadow))
	require.NoError(t, err)
	// Runs before the connection returns to the pool.
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), "SET search_path TO "+pgTestSchema) })

	require.NoError(t, ensureMetadataGINIndex(ctx, conn))
	require.NoError(t, ensureMultiTeamBusinessUnitGINIndexes(ctx, conn))
	require.NoError(t, ensureDashboardEnhancements(ctx, conn))
	require.NoError(t, ensurePerformanceIndexes(ctx, conn, testLogger{}))

	// Read pg_index directly rather than through pgIndexValid, which is the
	// check under test: the index must be valid on this schema's table.
	for _, idx := range indexes {
		var built bool
		require.NoError(t, db.Raw(`
			SELECT COALESCE(bool_and(pi.indisvalid), false)
			FROM pg_index pi
			JOIN pg_class ic ON ic.oid = pi.indexrelid
			WHERE pi.indrelid = to_regclass(?) AND ic.relname = ?
		`, pgTestSchema+"."+idx.table, idx.name).Scan(&built).Error)
		assert.True(t, built, "%s on %s.%s must be built despite a same-named index in %s", idx.name, pgTestSchema, idx.table, shadow)

		var shadowKept bool
		require.NoError(t, db.Raw("SELECT to_regclass(?) IS NOT NULL", shadow+"."+idx.name).Scan(&shadowKept).Error)
		assert.True(t, shadowKept, "the builders must not drop %s.%s", shadow, idx.name)
	}
}

// parkedLogIndexBuild is a CREATE INDEX CONCURRENTLY that pod 1 started and that
// is still running, as the build of a killed pod keeps running in Postgres.
type parkedLogIndexBuild struct {
	pod2    *sql.Conn  // a connection from a second pool, standing in for the next pod
	done    chan error // pod 1's build result
	pid     int64      // backend running pod 1's build
	oid     uint32     // pg_class oid of the index pod 1 is building
	release func()     // commits the transaction the build is waiting on
}

// parkLogIndexBuild starts pod 1's build of idx_race_logs_v behind an open
// REPEATABLE READ transaction (an old pod serving traffic), so the build parks in
// "waiting for old snapshots" with its index already in pg_index as INVALID: the
// state the next pod finds after a pod is killed mid-migration.
func parkLogIndexBuild(t *testing.T) parkedLogIndexBuild {
	t.Helper()
	db := trySetupPostgresDB(t)
	if db == nil {
		t.Skip("Postgres not available, skipping test")
	}
	ctx := context.Background()
	require.NoError(t, db.Exec("DROP TABLE IF EXISTS race_logs").Error)
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS race_logs").Error })
	require.NoError(t, db.Exec(`CREATE TABLE race_logs (id INTEGER PRIMARY KEY, v TEXT NOT NULL)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO race_logs SELECT g, 'v' || g FROM generate_series(1, 5000) g`).Error)

	pod2DB, err := gorm.Open(postgres.Open(postgresDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pod2SQL, err := pod2DB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pod2SQL.Close() })
	pod2 := acquirePerfTestSQLConn(t, ctx, pod2DB)

	blocker := acquirePerfTestSQLConn(t, ctx, db)
	_, err = blocker.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	require.NoError(t, err)
	_, err = blocker.ExecContext(ctx, "SELECT 1")
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(func() { _, _ = blocker.ExecContext(ctx, "COMMIT") }) }
	t.Cleanup(release)

	pod1 := acquirePerfTestSQLConn(t, ctx, db)
	build := parkedLogIndexBuild{pod2: pod2, done: make(chan error, 1), release: release}
	go func() {
		_, err := pod1.ExecContext(ctx, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_race_logs_v ON race_logs (v)")
		build.done <- err
	}()
	require.Eventually(t, func() bool {
		return db.Raw(`SELECT COALESCE((SELECT pid FROM pg_stat_progress_create_index
			WHERE relid = to_regclass('race_logs') AND phase = 'waiting for old snapshots'), 0)`).Scan(&build.pid).Error == nil && build.pid != 0
	}, 20*time.Second, 50*time.Millisecond, "pod 1's build must park in 'waiting for old snapshots'")
	require.NoError(t, db.Raw(`SELECT to_regclass('idx_race_logs_v')::oid`).Scan(&build.oid).Error)
	valid, err := pgIndexValid(ctx, pod2, "race_logs", "idx_race_logs_v")
	require.NoError(t, err)
	require.False(t, valid, "a build still in progress must read as INVALID in pg_index")
	return build
}

// rebuildRaceLogIndex runs the sequence every pgDropIndexConcurrently caller runs:
// an INVALID index is dropped and rebuilt with CREATE INDEX CONCURRENTLY IF NOT EXISTS.
func rebuildRaceLogIndex(ctx context.Context, conn *sql.Conn) error {
	valid, err := pgIndexValid(ctx, conn, "race_logs", "idx_race_logs_v")
	if err != nil || valid {
		return err
	}
	if err := pgDropIndexConcurrently(ctx, conn, "idx_race_logs_v"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_race_logs_v ON race_logs (v)")
	return err
}

// TestPgDropIndexConcurrentlyWaitsForLiveBuild pins that the next pod waits for a
// concurrent build another backend is still running instead of dropping it. A
// killed pod's idle advisory-lock session ends at once but its busy build keeps
// running, so the next pod holds the migration lock while that build is alive.
// Dropping it blocked behind the build and then failed one side with
// "tuple concurrently updated".
func TestPgDropIndexConcurrentlyWaitsForLiveBuild(t *testing.T) {
	ctx := context.Background()
	build := parkLogIndexBuild(t)
	pod2Done := make(chan error, 1)
	go func() { pod2Done <- rebuildRaceLogIndex(ctx, build.pod2) }()

	db := trySetupPostgresDB(t)
	var drops int64
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && drops == 0; time.Sleep(20 * time.Millisecond) {
		require.NoError(t, db.Raw(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE query ILIKE 'DROP INDEX CONCURRENTLY%idx_race_logs_v%'`).Scan(&drops).Error)
	}
	build.release()
	pod1Err, pod2Err := <-build.done, <-pod2Done

	require.Zero(t, drops, "pod 2 issued DROP INDEX CONCURRENTLY against a build another backend was still running")
	require.NoError(t, pod1Err)
	require.NoError(t, pod2Err)
	valid, err := pgIndexValid(ctx, build.pod2, "race_logs", "idx_race_logs_v")
	require.NoError(t, err)
	require.True(t, valid)
	var finalOID uint32
	require.NoError(t, db.Raw(`SELECT to_regclass('idx_race_logs_v')::oid`).Scan(&finalOID).Error)
	require.Equal(t, build.oid, finalOID, "pod 1's finished build must be the index that remains, not a second rebuild")
}

// TestPgDropIndexConcurrentlyRebuildsAfterPeerBuildFails pins that waiting never
// keeps a broken index: when the build pod 2 waits on dies (cancelled here, as a
// terminated orphan would be), pod 2 drops the INVALID leftover and rebuilds.
func TestPgDropIndexConcurrentlyRebuildsAfterPeerBuildFails(t *testing.T) {
	ctx := context.Background()
	build := parkLogIndexBuild(t)
	pod2Done := make(chan error, 1)
	go func() { pod2Done <- rebuildRaceLogIndex(ctx, build.pod2) }()
	time.Sleep(500 * time.Millisecond)
	db := trySetupPostgresDB(t)
	require.NoError(t, db.Exec("SELECT pg_cancel_backend(?)", build.pid).Error)
	require.Error(t, <-build.done, "the cancelled build must fail and leave an INVALID index")

	build.release()
	require.NoError(t, <-pod2Done)
	valid, err := pgIndexValid(ctx, build.pod2, "race_logs", "idx_race_logs_v")
	require.NoError(t, err)
	require.True(t, valid)
}

// dropMatViewsForColumnDDL drops the materialized views, which select every logs column and so block
// a DROP COLUMN; setupPerfTestDB recreates them for the next test.
func dropMatViewsForColumnDDL(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, view := range append(allMatViewNames(), legacyMatViewNames...) {
		require.NoError(t, db.Exec("DROP MATERIALIZED VIEW IF EXISTS "+view+" CASCADE").Error)
	}
}

// TestIsPassthroughMigrationBoundsLockWait pins that adding is_passthrough cannot park at the head of
// the logs lock queue behind a long-running read (where every later query would wait on it): with a
// read holding the table, the migration fails fast on its own lock timeout, and succeeds once the
// read ends.
func TestIsPassthroughMigrationBoundsLockWait(t *testing.T) {
	_, db := setupPerfTestDB(t)
	dropMatViewsForColumnDDL(t, db)
	require.NoError(t, db.Exec("ALTER TABLE logs DROP COLUMN IF EXISTS is_passthrough").Error)
	require.NoError(t, db.Exec("DELETE FROM migrations WHERE id = ?", "logs_add_is_passthrough_column").Error)

	holder := db.Begin()
	require.NoError(t, holder.Error)
	require.NoError(t, holder.Exec("SELECT 1 FROM logs LIMIT 1").Error)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	err := migrationAddIsPassthroughColumn(ctx, db, testLogger{})
	elapsed := time.Since(started)
	require.NoError(t, holder.Rollback().Error)

	require.Error(t, err, "the migration must not wait out a held table lock")
	require.Contains(t, err.Error(), "lock timeout", "it must fail on its own lock_timeout, not the caller's deadline")
	require.Less(t, elapsed, 15*time.Second)

	require.NoError(t, migrationAddIsPassthroughColumn(context.Background(), db, testLogger{}))
	require.True(t, db.Migrator().HasColumn(&Log{}, "is_passthrough"))
}

// TestIsPassthroughMigrationRollbackKeepsMarker pins that rolling the migration back does not drop
// is_passthrough: it is the only thing that tells a passthrough row stored under its inferred type
// apart from a typed request, so dropping it would silently erase that history.
func TestIsPassthroughMigrationRollbackKeepsMarker(t *testing.T) {
	_, db := setupPerfTestDB(t)
	require.NoError(t, db.Create(&Log{ID: "typed-passthrough", Timestamp: time.Now(), Object: "chat_completion", IsPassthrough: true, Provider: "openai", Status: "success"}).Error)
	dropMatViewsForColumnDDL(t, db)

	require.NoError(t, isPassthroughColumnMigration(context.Background(), testLogger{}).Rollback(db))

	require.True(t, db.Migrator().HasColumn(&Log{}, "is_passthrough"))
	var row Log
	require.NoError(t, db.First(&row, "id = ?", "typed-passthrough").Error)
	require.True(t, row.IsPassthrough)
}
