package logstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCHConn and fakeCHRows stand in for clickhouse-go's database/sql types:
// rows that yield n values, and a connection that never reports itself bad.
type fakeCHConn struct{}

func (fakeCHConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (fakeCHConn) Close() error                        { return nil }
func (fakeCHConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (fakeCHConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if query == "SLOW" {
		// Blocks inside the driver until the query's context ends.
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &fakeCHRows{left: 3}, nil
}

type fakeCHRows struct{ left int }

func (r *fakeCHRows) Columns() []string { return []string{"v"} }
func (r *fakeCHRows) Close() error      { return nil }
func (r *fakeCHRows) Next(dest []driver.Value) error {
	if r.left == 0 {
		return io.EOF
	}
	r.left--
	dest[0] = int64(r.left)
	return nil
}

// A connection is retired when its query's context had ended by the time the
// rows ended or closed, or the query failed because it had: clickhouse-go ends
// a cut-off read with a plain io.EOF, so that is the only sign. Rows read and
// closed under a live context, to the end or not, keep the connection.
func TestCutOffSafeConnRetiresOnlyCutOffConnections(t *testing.T) {
	query := func(c *cutOffSafeConn, ctx context.Context) driver.Rows {
		t.Helper()
		rows, err := c.QueryContext(ctx, "SELECT v", nil)
		require.NoError(t, err)
		return rows
	}
	requireValid := func(c *cutOffSafeConn, want bool) {
		t.Helper()
		require.Equal(t, want, c.IsValid())
		if want {
			require.NoError(t, c.ResetSession(context.Background()))
		} else {
			require.ErrorIs(t, c.ResetSession(context.Background()), driver.ErrBadConn)
			_, err := c.QueryContext(context.Background(), "SELECT v", nil)
			require.ErrorIs(t, err, driver.ErrBadConn)
		}
	}

	t.Run("read to the end and closed, context ended afterwards", func(t *testing.T) {
		c := &cutOffSafeConn{conn: fakeCHConn{}}
		ctx, cancel := context.WithCancel(context.Background())
		rows := query(c, ctx)
		dest := make([]driver.Value, 1)
		for rows.Next(dest) == nil {
		}
		require.NoError(t, rows.Close())
		cancel()
		requireValid(c, true)
	})

	t.Run("ended with io.EOF after the context ended", func(t *testing.T) {
		c := &cutOffSafeConn{conn: fakeCHConn{}}
		ctx, cancel := context.WithCancel(context.Background())
		rows := query(c, ctx)
		dest := make([]driver.Value, 1)
		require.NoError(t, rows.Next(dest))
		cancel()
		for rows.Next(dest) == nil {
		}
		require.NoError(t, rows.Close())
		requireValid(c, false)
	})

	t.Run("closed early under a live context", func(t *testing.T) {
		c := &cutOffSafeConn{conn: fakeCHConn{}}
		rows := query(c, context.Background())
		require.NoError(t, rows.Next(make([]driver.Value, 1)))
		require.NoError(t, rows.Close())
		requireValid(c, true)
	})

	t.Run("closed unread after the context ended", func(t *testing.T) {
		c := &cutOffSafeConn{conn: fakeCHConn{}}
		ctx, cancel := context.WithCancel(context.Background())
		rows := query(c, ctx)
		require.NoError(t, rows.Next(make([]driver.Value, 1)))
		cancel()
		require.NoError(t, rows.Close())
		requireValid(c, false)
	})

	t.Run("query failed because the context ended", func(t *testing.T) {
		c := &cutOffSafeConn{conn: fakeCHConn{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.QueryContext(ctx, "SELECT v", nil)
		require.ErrorIs(t, err, context.Canceled)
		requireValid(c, false)
	})
}

// TestClickHousePoolNeverHandsOutACutOffResult runs queries whose contexts end
// mid-read next to probes that check they get their own result set back.
// Without the cut-off guard the native protocol hands a probe the remaining
// blocks of a cut-off query (TXE-5255).
func TestClickHousePoolNeverHandsOutACutOffResult(t *testing.T) {
	store := trySetupClickHouseStore(t)
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	pool, err := store.db.DB()
	require.NoError(t, err)

	// Every cut-off query retires its connection, so the cutters are few: the
	// churn must not use up the client's ports for the ClickHouse tests that
	// run after this one. Probes outnumber them to catch a tainted connection.
	const cutters, probers = 4, 12
	stop := time.Now().Add(10 * time.Second)
	var probes, crossed atomic.Int64
	var wg sync.WaitGroup
	for range cutters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+rand.Intn(5))*time.Millisecond)
				if rows, err := pool.QueryContext(ctx, "SELECT number AS cut_off_col FROM system.numbers LIMIT 200000"); err == nil {
					for rows.Next() {
						var n uint64
						_ = rows.Scan(&n)
					}
					_ = rows.Close()
				}
				cancel()
			}
		}()
	}
	for range probers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				rows, err := pool.QueryContext(context.Background(), "SELECT toUnixTimestamp64Nano(now64(9)) AS probe_col")
				if err != nil {
					continue
				}
				cols, _ := rows.Columns()
				probes.Add(1)
				if len(cols) != 1 || cols[0] != "probe_col" {
					crossed.Add(1)
				}
				_ = rows.Close()
			}
		}()
	}
	wg.Wait()
	require.Positive(t, probes.Load())
	require.Zero(t, crossed.Load(), "%d of %d probes read a cut-off query's result set", crossed.Load(), probes.Load())
}

// fakeCHConnector counts the connections database/sql opens through the
// cut-off guard; fakeCHTxConn adds transactions to fakeCHConn.
type fakeCHConnector struct {
	opened    atomic.Int64
	committed atomic.Int64
}

func (c *fakeCHConnector) Connect(context.Context) (driver.Conn, error) {
	c.opened.Add(1)
	return fakeCHTxConn{connector: c}, nil
}

func (c *fakeCHConnector) Driver() driver.Driver { return nil }

type fakeCHTxConn struct {
	fakeCHConn
	connector *fakeCHConnector
}

func (c fakeCHTxConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeCHTx{connector: c.connector}, nil
}

type fakeCHTx struct{ connector *fakeCHConnector }

func (t fakeCHTx) Commit() error   { t.connector.committed.Add(1); return nil }
func (t fakeCHTx) Rollback() error { return nil }

// TestCutOffSafePoolReusesOnlyUncutConnections drives the guard through a real
// database/sql pool: ordinary queries, rows closed early under a live context,
// and transactions keep reusing one pooled connection; a query cut off before
// it returned, or mid-iteration, makes the pool open a fresh one.
func TestCutOffSafePoolReusesOnlyUncutConnections(t *testing.T) {
	connector := &fakeCHConnector{}
	pool := sql.OpenDB(&cutOffSafeConnector{base: connector})
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(1)

	readAll := func(ctx context.Context) error {
		rows, err := pool.QueryContext(ctx, "SELECT v")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	}

	for range 20 {
		require.NoError(t, readAll(context.Background()))
	}
	require.EqualValues(t, 1, connector.opened.Load(), "ordinary queries reuse the pooled connection")

	rows, err := pool.QueryContext(context.Background(), "SELECT v")
	require.NoError(t, err)
	require.True(t, rows.Next())
	require.NoError(t, rows.Close())
	require.NoError(t, readAll(context.Background()))
	require.EqualValues(t, 1, connector.opened.Load(), "rows closed early under a live context keep the connection")

	tx, err := pool.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	txRows, err := tx.QueryContext(context.Background(), "SELECT v")
	require.NoError(t, err)
	for txRows.Next() {
	}
	require.NoError(t, txRows.Close())
	require.NoError(t, tx.Commit())
	require.EqualValues(t, 1, connector.committed.Load())
	require.EqualValues(t, 1, connector.opened.Load(), "a transaction runs on the pooled connection and returns it")

	ctx, cancel := context.WithCancel(context.Background())
	rows, err = pool.QueryContext(ctx, "SELECT v")
	require.NoError(t, err)
	require.True(t, rows.Next())
	cancel()
	for rows.Next() {
	}
	_ = rows.Close()
	require.NoError(t, readAll(context.Background()))
	require.EqualValues(t, 2, connector.opened.Load(), "a read cut off mid-iteration retires its connection")

	slow, cancelSlow := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSlow()
	_, err = pool.QueryContext(slow, "SLOW")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, readAll(context.Background()))
	require.EqualValues(t, 3, connector.opened.Load(), "a query cut off before it returned retires its connection")
}
