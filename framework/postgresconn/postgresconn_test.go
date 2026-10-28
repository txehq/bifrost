package postgresconn

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestValidatePasswordCommandAllowsEmptyPassword(t *testing.T) {
	cfg := validConfig()
	cfg.Password = schemas.NewSecretVar("")
	cfg.PasswordCommand = &PasswordCommandConfig{Command: "printf", Args: []string{"secret"}}

	require.NoError(t, Validate(cfg, true))
}

func TestValidatePasswordAndPasswordCommandAreExclusive(t *testing.T) {
	cfg := validConfig()
	cfg.PasswordCommand = &PasswordCommandConfig{Command: "printf", Args: []string{"secret"}}

	require.ErrorContains(t, Validate(cfg, true), "mutually exclusive")
}

func TestValidateRequiresStaticPasswordWhenConfigured(t *testing.T) {
	cfg := validConfig()
	cfg.Password = schemas.NewSecretVar("")

	require.ErrorContains(t, Validate(cfg, true), "postgres password is required")
}

func TestValidateRejectsInvalidConnMaxLifetime(t *testing.T) {
	cfg := validConfig()
	cfg.ConnMaxLifetime = "sometimes"

	require.ErrorContains(t, Validate(cfg, true), "invalid postgres conn_max_lifetime")
}

func TestValidateRejectsNonPositiveConnMaxLifetime(t *testing.T) {
	cfg := validConfig()
	cfg.ConnMaxLifetime = "0s"

	require.ErrorContains(t, Validate(cfg, true), "postgres conn_max_lifetime must be positive")
}

func TestBuildDSNQuotesValuesForPasswordCommandParsing(t *testing.T) {
	cfg := validConfig()
	cfg.Host = schemas.NewSecretVar("127.0.0.1")
	cfg.User = schemas.NewSecretVar("service-account@example-project.iam")
	cfg.Password = schemas.NewSecretVar("")
	cfg.PasswordCommand = &PasswordCommandConfig{Command: "printf", Args: []string{"unused-iam-auth"}}

	pgxConfig, err := pgx.ParseConfig(BuildDSN(cfg))

	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", pgxConfig.Host)
	require.Equal(t, "service-account@example-project.iam", pgxConfig.User)
	require.Equal(t, "", pgxConfig.Password)
	require.Equal(t, "bifrost", pgxConfig.Database)
}

func TestBuildDSNQuotesSpecialCharacters(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Config)
		validate func(*testing.T, *pgx.ConnConfig)
	}{
		{
			name: "single quote",
			mutate: func(cfg *Config) {
				cfg.User = schemas.NewSecretVar("service'account")
			},
			validate: func(t *testing.T, pgxConfig *pgx.ConnConfig) {
				require.Equal(t, "service'account", pgxConfig.User)
			},
		},
		{
			name: "backslash",
			mutate: func(cfg *Config) {
				cfg.Host = schemas.NewSecretVar(`C:\postgres\socket`)
			},
			validate: func(t *testing.T, pgxConfig *pgx.ConnConfig) {
				require.Equal(t, `C:\postgres\socket`, pgxConfig.Host)
			},
		},
		{
			name: "backslash and single quote",
			mutate: func(cfg *Config) {
				cfg.DBName = schemas.NewSecretVar(`bifrost\tenant's`)
			},
			validate: func(t *testing.T, pgxConfig *pgx.ConnConfig) {
				require.Equal(t, `bifrost\tenant's`, pgxConfig.Database)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)

			pgxConfig, err := pgx.ParseConfig(BuildDSN(cfg))

			require.NoError(t, err)
			tt.validate(t, pgxConfig)
		})
	}
}

func TestCloseNilDBDoesNotPanic(t *testing.T) {
	require.NotPanics(t, func() {
		Close(nil, nil)
	})
}

func TestRunPasswordCommand(t *testing.T) {
	password, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "printf",
		Args:    []string{"secret\n"},
	})

	require.NoError(t, err)
	require.Equal(t, "secret", password)
}

func TestRunPasswordCommandRequiresConfig(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), nil)

	require.ErrorContains(t, err, "postgres password_command config is required")
}

func TestRunPasswordCommandRequiresCommand(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: " ",
	})

	require.ErrorContains(t, err, "postgres password_command.command is required")
}

func TestRunPasswordCommandRejectsCommandWithInlineArgs(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "printf secret",
	})

	require.ErrorContains(t, err, "single executable")
}

func TestRunPasswordCommandRejectsShellInterpreter(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "sh",
		Args:    []string{"-c", "printf secret"},
	})

	require.ErrorContains(t, err, "must not invoke a shell interpreter")
}

func TestRunPasswordCommandRejectsEmptyOutput(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "printf",
		Args:    []string{""},
	})

	require.ErrorContains(t, err, "empty stdout")
}

func TestRunPasswordCommandIncludesStderr(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "ls",
		Args:    []string{"/definitely/not/a/real/postgres/password/file"},
	})

	require.ErrorContains(t, err, "exit status")
	require.ErrorContains(t, err, "definitely/not/a/real/postgres/password/file")
}

func TestRunPasswordCommandTimeout(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "sleep",
		Args:    []string{"1"},
		Timeout: "1ms",
	})

	require.ErrorContains(t, err, "timed out")
}

func TestRunPasswordCommandStartErrorIsNotTimeout(t *testing.T) {
	_, err := RunPasswordCommand(context.Background(), &PasswordCommandConfig{
		Command: "/definitely/not/a/real/postgres/password/command",
		Timeout: "1ns",
	})

	require.ErrorContains(t, err, "failed to start")
	require.NotContains(t, err.Error(), "timed out")
}

func validConfig() *Config {
	return &Config{
		Host:     schemas.NewSecretVar("localhost"),
		Port:     schemas.NewSecretVar("5432"),
		User:     schemas.NewSecretVar("bifrost"),
		Password: schemas.NewSecretVar("password"),
		DBName:   schemas.NewSecretVar("bifrost"),
		SSLMode:  schemas.NewSecretVar("disable"),
	}
}

// migrationDSNSuffix is what configstore and logstore append to build their
// migration-pool DSN.
const migrationDSNSuffix = " default_query_exec_mode=simple_protocol"

// localPostgresConfig points at the local dev Postgres used by these tests.
func localPostgresConfig() *Config {
	return &Config{
		Host:     schemas.NewSecretVar(getenvDefault("BIFROST_TEST_PG_HOST", "localhost")),
		Port:     schemas.NewSecretVar(getenvDefault("BIFROST_TEST_PG_PORT", "5432")),
		User:     schemas.NewSecretVar(getenvDefault("BIFROST_TEST_PG_USER", "bifrost")),
		Password: schemas.NewSecretVar(getenvDefault("BIFROST_TEST_PG_PASSWORD", "bifrost_password")),
		DBName:   schemas.NewSecretVar(getenvDefault("BIFROST_TEST_PG_DB", "bifrost")),
		SSLMode:  schemas.NewSecretVar("disable"),
	}
}

// openLocalPostgres opens dsn, skipping the test when no server is reachable.
func openLocalPostgres(t *testing.T, dsn string, cfg *Config) *gorm.DB {
	t.Helper()
	db, err := Open(dsn+" connect_timeout=2", cfg, gormlogger.Discard)
	if err == nil {
		err = db.Exec("SELECT 1").Error
	}
	if err != nil {
		t.Skipf("local Postgres unavailable: %v", err)
	}
	t.Cleanup(func() { Close(db, nil) })
	return db
}

// showSetting reads a session setting on one pooled connection.
func showSetting(t *testing.T, db *gorm.DB, name string) string {
	t.Helper()
	var value string
	require.NoError(t, db.Raw("SELECT current_setting(?)", name).Scan(&value).Error)
	return value
}

// TestSessionTimeoutDefaults pins the defaults and the disable/invalid forms.
func TestSessionTimeoutDefaults(t *testing.T) {
	cfg := validConfig()
	st, err := parseStatementTimeout(cfg)
	require.NoError(t, err)
	require.Equal(t, 120*time.Second, st)
	it, err := parseIdleInTransactionSessionTimeout(cfg)
	require.NoError(t, err)
	require.Equal(t, 120*time.Second, it)

	cfg.StatementTimeout = "0"
	cfg.IdleInTransactionSessionTimeout = "-1s"
	st, err = parseStatementTimeout(cfg)
	require.NoError(t, err)
	require.Zero(t, st)
	it, err = parseIdleInTransactionSessionTimeout(cfg)
	require.NoError(t, err)
	require.Zero(t, it)

	cfg.StatementTimeout = "soon"
	require.ErrorContains(t, Validate(cfg, true), "invalid postgres statement_timeout")
	cfg.StatementTimeout = ""
	cfg.IdleInTransactionSessionTimeout = "later"
	require.ErrorContains(t, Validate(cfg, true), "invalid postgres idle_in_transaction_session_timeout")
}

// TestSessionTimeoutStatementIsRuntimeOnly pins that the session settings are
// built for the runtime DSN only; the migration DSN (simple protocol) gets none,
// since migrations run long index builds and backfills.
func TestSessionTimeoutStatementIsRuntimeOnly(t *testing.T) {
	cfg := validConfig()
	dsn := BuildDSN(cfg)

	stmt, err := runtimeSessionSettingsSQL(dsn, cfg)
	require.NoError(t, err)
	require.Contains(t, stmt, "'statement_timeout', '120000'")
	require.Contains(t, stmt, "'idle_in_transaction_session_timeout', '120000'")

	stmt, err = runtimeSessionSettingsSQL(dsn+migrationDSNSuffix, cfg)
	require.NoError(t, err)
	require.Empty(t, stmt)

	cfg.StatementTimeout = "0"
	cfg.IdleInTransactionSessionTimeout = "0"
	stmt, err = runtimeSessionSettingsSQL(dsn, cfg)
	require.NoError(t, err)
	require.Empty(t, stmt)
}

// TestRuntimePoolStatementTimeoutCancelsLongQuery pins H22 against a real
// server: a runtime pool built with a short statement_timeout cancels
// pg_sleep(2), while the migration pool from the same config is unaffected.
func TestRuntimePoolStatementTimeoutCancelsLongQuery(t *testing.T) {
	cfg := localPostgresConfig()
	cfg.StatementTimeout = "500ms"
	cfg.IdleInTransactionSessionTimeout = "2s"
	dsn := BuildDSN(cfg)

	runtime := openLocalPostgres(t, dsn, cfg)
	require.NoError(t, ApplyPoolTuning(runtime, cfg))
	require.Equal(t, "500ms", showSetting(t, runtime, "statement_timeout"))
	require.Equal(t, "2s", showSetting(t, runtime, "idle_in_transaction_session_timeout"))

	started := time.Now()
	err := runtime.Exec("SELECT pg_sleep(2)").Error
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a server error, got %v", err)
	require.Equal(t, "57014", pgErr.Code, "statement_timeout cancels with query_canceled")
	require.Less(t, time.Since(started), 1900*time.Millisecond)

	migration := openLocalPostgres(t, dsn+migrationDSNSuffix, cfg)
	require.NoError(t, ApplyMigrationPoolTuning(migration))
	require.Equal(t, "0", showSetting(t, migration, "statement_timeout"))
	require.Equal(t, "0", showSetting(t, migration, "idle_in_transaction_session_timeout"))
	require.NoError(t, migration.Exec("SELECT pg_sleep(1)").Error)
}

// TestRuntimePoolDefaultsAndDisable pins the defaults on a real runtime pool and
// that "0" leaves the server default in place.
func TestRuntimePoolDefaultsAndDisable(t *testing.T) {
	cfg := localPostgresConfig()
	db := openLocalPostgres(t, BuildDSN(cfg), cfg)
	require.Equal(t, "2min", showSetting(t, db, "statement_timeout"))
	require.Equal(t, "2min", showSetting(t, db, "idle_in_transaction_session_timeout"))

	off := localPostgresConfig()
	off.StatementTimeout = "0"
	off.IdleInTransactionSessionTimeout = "0"
	db = openLocalPostgres(t, BuildDSN(off), off)
	require.Equal(t, "0", showSetting(t, db, "statement_timeout"))
	require.Equal(t, "0", showSetting(t, db, "idle_in_transaction_session_timeout"))
}

// TestDisableStatementTimeoutOnDedicatedConn pins the escape hatch for runtime
// work that legitimately runs long on a dedicated connection.
func TestDisableStatementTimeoutOnDedicatedConn(t *testing.T) {
	cfg := localPostgresConfig()
	cfg.StatementTimeout = "300ms"
	db := openLocalPostgres(t, BuildDSN(cfg), cfg)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	restore, err := DisableStatementTimeout(ctx, conn)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "SELECT pg_sleep(0.6)")
	require.NoError(t, err)
	require.NoError(t, restore(ctx))
	var value string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT current_setting('statement_timeout')").Scan(&value))
	require.Equal(t, "300ms", value)
	_, err = conn.ExecContext(ctx, "SELECT pg_sleep(0.6)")
	require.Error(t, err, "restored connection must enforce the pool timeout again")
}

// fakeRawConn records whether the caller asked database/sql to drop the
// physical connection (a Raw callback returning driver.ErrBadConn).
type fakeRawConn struct {
	discarded bool
}

// Raw runs f and records a discard when f reports driver.ErrBadConn.
func (c *fakeRawConn) Raw(f func(driverConn any) error) error {
	err := f(nil)
	if errors.Is(err, driver.ErrBadConn) {
		c.discarded = true
	}
	return err
}

// TestRestoreStatementTimeoutOrDiscard pins the DisableStatementTimeout
// contract at the restore site: a connection whose restore failed must never go
// back to the pool with statement_timeout lifted, and the restore itself is
// bounded even when the caller's ctx is already cancelled.
func TestRestoreStatementTimeoutOrDiscard(t *testing.T) {
	t.Run("failed restore discards the connection", func(t *testing.T) {
		conn := &fakeRawConn{}
		err := RestoreStatementTimeoutOrDiscard(context.Background(), conn, func(context.Context) error {
			return errors.New("set_config failed")
		}, time.Second)
		require.Error(t, err)
		require.True(t, conn.discarded, "a failed restore must discard the connection instead of pooling it with statement_timeout=0")
	})
	t.Run("successful restore keeps the connection", func(t *testing.T) {
		conn := &fakeRawConn{}
		require.NoError(t, RestoreStatementTimeoutOrDiscard(context.Background(), conn, func(context.Context) error { return nil }, time.Second))
		require.False(t, conn.discarded)
	})
	t.Run("restore is bounded and survives a cancelled caller ctx", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		var (
			deadline    time.Time
			hasDeadline bool
			ctxErr      error
		)
		require.NoError(t, RestoreStatementTimeoutOrDiscard(parent, &fakeRawConn{}, func(ctx context.Context) error {
			deadline, hasDeadline = ctx.Deadline()
			ctxErr = ctx.Err()
			return nil
		}, time.Second))
		require.True(t, hasDeadline, "the restore must run under a deadline")
		require.WithinDuration(t, time.Now().Add(time.Second), deadline, time.Second)
		require.NoError(t, ctxErr, "the caller's cancellation must not abort the restore")
	})
}

// TestWithApplicationName covers the opt-in default, the role suffix, quoting,
// and the 63-byte cap keeping the role intact.
func TestWithApplicationName(t *testing.T) {
	t.Cleanup(func() { SetApplicationName("") })
	const dsn = "host='h' port='5432'"

	SetApplicationName("")
	if got := WithApplicationName(dsn, "config"); got != dsn {
		t.Fatalf("unset base changed the DSN: %q", got)
	}

	SetApplicationName("bifrost-node-1")
	if got := WithApplicationName(dsn, "logs"); got != dsn+" application_name='bifrost-node-1-logs'" {
		t.Fatalf("got %q", got)
	}

	SetApplicationName("bifrost-" + strings.Repeat("x", 80))
	got := WithApplicationName(dsn, "config")
	name := strings.TrimSuffix(strings.TrimPrefix(got, dsn+" application_name='"), "'")
	if len(name) != maxApplicationNameLen || !strings.HasSuffix(name, "-config") {
		t.Fatalf("long base not capped with role kept: %q (%d bytes)", name, len(name))
	}

	// A multibyte rune at the cut point: 57 ASCII bytes leave room for only the
	// first byte of "é" before "-logs". Postgres hex-escapes any non-printable-
	// ASCII byte server-side, which would also push the name past 63 and drop
	// the role, so the name must come out as printable ASCII within the cap.
	SetApplicationName(strings.Repeat("a", 57) + "é")
	got = WithApplicationName(dsn, "logs")
	name = strings.TrimSuffix(strings.TrimPrefix(got, dsn+" application_name='"), "'")
	if !utf8.ValidString(name) || len(name) > maxApplicationNameLen || !strings.HasSuffix(name, "-logs") {
		t.Fatalf("multibyte base not cut cleanly: %q (%d bytes)", name, len(name))
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7e {
			t.Fatalf("non-printable-ASCII byte %#x at %d would be escaped by Postgres: %q", name[i], i, name)
		}
	}

	SetApplicationName("bifrost-hôte")
	if got := WithApplicationName(dsn, "config"); got != dsn+" application_name='bifrost-h_te-config'" {
		t.Fatalf("non-ASCII base not sanitized: %q", got)
	}
}
