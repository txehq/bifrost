package logstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// openClickHousePool opens the database/sql pool for dsn through a connector
// that retires a connection once a query on it was cut off by its context.
//
// clickhouse-go's native protocol keeps a connection whose query context ended
// before the result was read to the end: database/sql closes the rows and hands
// the connection to the next caller, which then reads the cut-off query's
// remaining blocks as its own result (TXE-5255; reproduced with v2.32.0 and
// v2.50.0, not over HTTP). Marking such a connection bad makes database/sql
// close it instead of reusing it.
func openClickHousePool(dsn string) (*sql.DB, error) {
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(&cutOffSafeConnector{base: clickhouse.Connector(opts)}), nil
}

type cutOffSafeConnector struct {
	base driver.Connector
}

func (c *cutOffSafeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &cutOffSafeConn{conn: conn}, nil
}

func (c *cutOffSafeConnector) Driver() driver.Driver { return c.base.Driver() }

// cutOffSafeConn forwards every interface clickhouse-go's database/sql
// connection implements and reports itself bad after a cut-off query.
type cutOffSafeConn struct {
	conn   driver.Conn
	cutOff atomic.Bool
}

// noteErr marks the connection bad when a call failed because ctx ended.
func (c *cutOffSafeConn) noteErr(ctx context.Context, err error) {
	if err != nil && ctxEnded(ctx) {
		c.cutOff.Store(true)
	}
}

// ctxEnded reports whether ctx is done or its deadline has passed. clickhouse-go
// sets the deadline on the socket, which can expire a moment before ctx's own
// timer marks it done, so a passed deadline counts even while Err is nil.
func ctxEnded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

func (c *cutOffSafeConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *cutOffSafeConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if c.cutOff.Load() {
		return nil, driver.ErrBadConn
	}
	var stmt driver.Stmt
	var err error
	if p, ok := c.conn.(driver.ConnPrepareContext); ok {
		stmt, err = p.PrepareContext(ctx, query)
	} else {
		stmt, err = c.conn.Prepare(query)
	}
	c.noteErr(ctx, err)
	if err != nil {
		return nil, err
	}
	return &cutOffSafeStmt{stmt: stmt, conn: c}, nil
}

func (c *cutOffSafeConn) Close() error { return c.conn.Close() }

func (c *cutOffSafeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *cutOffSafeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.cutOff.Load() {
		return nil, driver.ErrBadConn
	}
	if b, ok := c.conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.conn.Begin() //nolint:staticcheck // fallback for drivers without BeginTx
}

func (c *cutOffSafeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.cutOff.Load() {
		return nil, driver.ErrBadConn
	}
	q, ok := c.conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := q.QueryContext(ctx, query, args)
	c.noteErr(ctx, err)
	if err != nil {
		return nil, err
	}
	return &cutOffSafeRows{rows: rows, conn: c, ctx: ctx}, nil
}

func (c *cutOffSafeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.cutOff.Load() {
		return nil, driver.ErrBadConn
	}
	e, ok := c.conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	res, err := e.ExecContext(ctx, query, args)
	c.noteErr(ctx, err)
	return res, err
}

func (c *cutOffSafeConn) Ping(ctx context.Context) error {
	if c.cutOff.Load() {
		return driver.ErrBadConn
	}
	if p, ok := c.conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *cutOffSafeConn) ResetSession(ctx context.Context) error {
	if c.cutOff.Load() {
		return driver.ErrBadConn
	}
	if r, ok := c.conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

// IsValid lets database/sql drop a cut-off connection when it is returned to
// the pool, before anyone else can take it.
func (c *cutOffSafeConn) IsValid() bool {
	if c.cutOff.Load() {
		return false
	}
	if v, ok := c.conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *cutOffSafeConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// cutOffSafeRows marks its connection bad when its query's context has ended
// (see ctxEnded) by the time the rows end or are closed. clickhouse-go ends a cut-off read
// with a plain io.EOF, so a read that looks complete may still have blocks on
// the wire; only a context that is still live proves it was not cut off.
type cutOffSafeRows struct {
	rows driver.Rows
	conn *cutOffSafeConn
	ctx  context.Context
}

func (r *cutOffSafeRows) Columns() []string { return r.rows.Columns() }

func (r *cutOffSafeRows) Next(dest []driver.Value) error {
	err := r.rows.Next(dest)
	r.conn.noteErr(r.ctx, err)
	return err
}

func (r *cutOffSafeRows) Close() error {
	if ctxEnded(r.ctx) {
		r.conn.cutOff.Store(true)
	}
	return r.rows.Close()
}

func (r *cutOffSafeRows) HasNextResultSet() bool {
	if n, ok := r.rows.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *cutOffSafeRows) NextResultSet() error {
	if n, ok := r.rows.(driver.RowsNextResultSet); ok {
		return n.NextResultSet()
	}
	return io.EOF
}

func (r *cutOffSafeRows) ColumnTypeScanType(idx int) reflect.Type {
	if t, ok := r.rows.(driver.RowsColumnTypeScanType); ok {
		return t.ColumnTypeScanType(idx)
	}
	return reflect.TypeOf(new(any)).Elem()
}

func (r *cutOffSafeRows) ColumnTypeDatabaseTypeName(idx int) string {
	if t, ok := r.rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return t.ColumnTypeDatabaseTypeName(idx)
	}
	return ""
}

func (r *cutOffSafeRows) ColumnTypeNullable(idx int) (nullable, ok bool) {
	if t, has := r.rows.(driver.RowsColumnTypeNullable); has {
		return t.ColumnTypeNullable(idx)
	}
	return false, false
}

func (r *cutOffSafeRows) ColumnTypePrecisionScale(idx int) (precision, scale int64, ok bool) {
	if t, has := r.rows.(driver.RowsColumnTypePrecisionScale); has {
		return t.ColumnTypePrecisionScale(idx)
	}
	return 0, 0, false
}

// cutOffSafeStmt is a prepared statement (clickhouse-go's batch insert) whose
// cut-off execution marks the connection bad.
type cutOffSafeStmt struct {
	stmt driver.Stmt
	conn *cutOffSafeConn
}

func (s *cutOffSafeStmt) Close() error  { return s.stmt.Close() }
func (s *cutOffSafeStmt) NumInput() int { return s.stmt.NumInput() }

func (s *cutOffSafeStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.stmt.Exec(args) //nolint:staticcheck // database/sql only calls it when ExecContext is absent
}

func (s *cutOffSafeStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.stmt.Query(args) //nolint:staticcheck // database/sql only calls it when QueryContext is absent
}

func (s *cutOffSafeStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	e, ok := s.stmt.(driver.StmtExecContext)
	if !ok {
		values := make([]driver.Value, len(args))
		for i, a := range args {
			values[i] = a.Value
		}
		res, err := s.stmt.Exec(values) //nolint:staticcheck // fallback for statements without ExecContext
		s.conn.noteErr(ctx, err)
		return res, err
	}
	res, err := e.ExecContext(ctx, args)
	s.conn.noteErr(ctx, err)
	return res, err
}

var (
	_ driver.Connector          = (*cutOffSafeConnector)(nil)
	_ driver.ConnPrepareContext = (*cutOffSafeConn)(nil)
	_ driver.ConnBeginTx        = (*cutOffSafeConn)(nil)
	_ driver.QueryerContext     = (*cutOffSafeConn)(nil)
	_ driver.ExecerContext      = (*cutOffSafeConn)(nil)
	_ driver.Pinger             = (*cutOffSafeConn)(nil)
	_ driver.SessionResetter    = (*cutOffSafeConn)(nil)
	_ driver.Validator          = (*cutOffSafeConn)(nil)
	_ driver.NamedValueChecker  = (*cutOffSafeConn)(nil)
	_ driver.RowsNextResultSet  = (*cutOffSafeRows)(nil)
	_ driver.StmtExecContext    = (*cutOffSafeStmt)(nil)
)
