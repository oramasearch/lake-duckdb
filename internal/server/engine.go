/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// Lockdown is the settings applied once, before the service answers anything, in this order. The
// last one locks the configuration, so no statement a client sends can change any of them back.
// They are global to the database, so every connection in the pool has them.
//
//   - enable_external_access=false: no reading or writing any file but the database's own (no
//     read_csv, read_parquet, glob, COPY ... TO, EXPORT DATABASE, ATTACH of a file), and no network.
//   - autoinstall_known_extensions / autoload_known_extensions=false: a query never downloads or
//     loads an extension; INSTALL and LOAD of anything not built in fail.
//   - allow_community_extensions=false: no community extension, even if one were on disk.
//   - memory_limit / threads (when configured) bound DuckDB's buffer manager and its worker threads.
//   - lock_configuration=true: every later SET, RESET or PRAGMA of a configuration option fails.
func lockdown(memoryLimit int64, threads int) []string {
	settings := []string{
		"SET enable_external_access = false",
		"SET autoinstall_known_extensions = false",
		"SET autoload_known_extensions = false",
		"SET allow_community_extensions = false",
	}
	if memoryLimit > 0 {
		// DuckDB accepts a byte count with a unit; KiB keeps it exact enough and never zero.
		settings = append(settings, fmt.Sprintf("SET memory_limit = '%dKiB'", max(memoryLimit/1024, 1)))
	}
	if threads > 0 {
		settings = append(settings, fmt.Sprintf("SET threads = %d", threads))
	}
	return append(settings, "SET lock_configuration = true")
}

// openEngine opens the database file, applies Lockdown, and checks from a fresh connection that
// the lock holds. Any failure closes the database: the service never serves an unlocked engine.
func openEngine(ctx context.Context, path string, memoryLimit int64, threads int) (*sql.DB, error) {
	connector, err := duckdb.NewConnector(path, nil)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	fail := func(err error) (*sql.DB, error) {
		_ = db.Close()
		return nil, err
	}
	for _, setting := range lockdown(memoryLimit, threads) {
		if _, err := db.ExecContext(ctx, setting); err != nil {
			return fail(fmt.Errorf("%s: %w", setting, err))
		}
	}
	// A second connection held open beside the first proves the settings are the database's, not
	// one connection's.
	first, err := db.Conn(ctx)
	if err != nil {
		return fail(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		return fail(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		var external, locked bool
		if err := conn.QueryRowContext(ctx,
			"SELECT current_setting('enable_external_access'), current_setting('lock_configuration')").Scan(&external, &locked); err != nil {
			return fail(err)
		}
		if external || !locked {
			return fail(fmt.Errorf("lockdown did not hold: enable_external_access=%v lock_configuration=%v", external, locked))
		}
	}
	return db, nil
}

// result is one statement's answer: rows (like /v1/query) or a count (like /v1/exec).
type result struct {
	query *queryResult
	exec  *execResult
}

func (r result) value() interface{} {
	if r.query != nil {
		return r.query
	}
	return r.exec
}

// mode says how a statement answers.
type mode int

const (
	modeQuery mode = iota // columns and rows
	modeExec              // rows affected
	modeAuto              // rows when the statement returns data, a count otherwise (batch)
)

// withConn runs fn on one pooled connection, under an API slot, and rolls back any transaction a
// client statement left open before the connection goes back to the pool: no transaction outlives
// its request.
func (s *Server) withConn(ctx context.Context, fn func(conn *duckdb.Conn) *apiError) *apiError {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return contextError(ctx.Err())
	}
	pooled, err := s.db.Conn(ctx)
	if err != nil {
		return sqlError(ctx, err)
	}
	defer pooled.Close()
	var apiErr *apiError
	err = pooled.Raw(func(driverConn interface{}) error {
		conn, ok := driverConn.(*duckdb.Conn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %T", driverConn)
		}
		apiErr = fn(conn)
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Fails with "no transaction is active" in the usual case, which is the point.
		_, _ = conn.ExecContext(cleanup, "ROLLBACK", nil)
		return nil
	})
	if err != nil && apiErr == nil {
		return &apiError{http.StatusInternalServerError, "internal", "cannot use a database connection"}
	}
	return apiErr
}

// run prepares and runs one client statement on conn. A sql holding several statements runs all
// of them; the driver executes every one but the last while preparing, and the last answers.
func (s *Server) run(ctx context.Context, conn *duckdb.Conn, query string, args []interface{}, how mode) (result, *apiError) {
	prepared, err := conn.PrepareContext(ctx, query)
	if err != nil {
		return result{}, sqlError(ctx, err)
	}
	defer prepared.Close()
	stmt, ok := prepared.(*duckdb.Stmt)
	if !ok {
		return result{}, &apiError{http.StatusInternalServerError, "internal", "unexpected prepared statement"}
	}
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	if how == modeAuto {
		kind, err := stmt.StatementType()
		if err != nil {
			return result{}, sqlError(ctx, err)
		}
		if kind == duckdb.STATEMENT_TYPE_TRANSACTION {
			return result{}, &apiError{http.StatusBadRequest, "invalid_request",
				"a batch is already one transaction; BEGIN, COMMIT and ROLLBACK are not allowed in it"}
		}
		how = modeQuery
		if countsOnly(stmt, kind) {
			how = modeExec
		}
	}
	if how == modeExec {
		res, err := stmt.ExecContext(ctx, named)
		if err != nil {
			return result{}, sqlError(ctx, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return result{}, &apiError{http.StatusInternalServerError, "internal", "cannot read rows affected"}
		}
		return result{exec: &execResult{RowsAffected: affected}}, nil
	}
	rows, err := stmt.QueryContext(ctx, named)
	if err != nil {
		return result{}, sqlError(ctx, err)
	}
	answer, err := s.readRows(rows)
	if err != nil {
		return result{}, sqlError(ctx, err)
	}
	return result{query: &answer}, nil
}

// countsOnly: DuckDB answers a statement that returns no data with a single column, "Count"
// (INSERT, UPDATE, DELETE, CREATE) or "Success" (DROP, ALTER, CHECKPOINT, SET, ...). Those answer
// like /v1/exec in a batch. A SELECT-like statement, and INSERT/UPDATE/DELETE ... RETURNING, answer
// rows.
func countsOnly(stmt *duckdb.Stmt, kind duckdb.StmtType) bool {
	switch kind {
	case duckdb.STATEMENT_TYPE_SELECT, duckdb.STATEMENT_TYPE_EXPLAIN, duckdb.STATEMENT_TYPE_CALL,
		duckdb.STATEMENT_TYPE_EXECUTE, duckdb.STATEMENT_TYPE_PRAGMA, duckdb.STATEMENT_TYPE_RELATION:
		return false
	}
	count, err := stmt.ColumnCount()
	if err != nil || count != 1 {
		return false
	}
	name, err := stmt.ColumnName(0)
	if err != nil {
		return false
	}
	return name == "Count" || name == "Success"
}

// readRows reads up to maxRows rows and closes rows. When there are more, it stops and marks the
// result truncated.
func (s *Server) readRows(rows driver.Rows) (queryResult, error) {
	defer rows.Close()
	columns := rows.Columns()
	types := make([]string, len(columns))
	if typed, ok := rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		for i := range columns {
			types[i] = typed.ColumnTypeDatabaseTypeName(i)
		}
	}
	result := queryResult{Columns: columns, Rows: [][]interface{}{}}
	if result.Columns == nil {
		result.Columns = []string{}
	}
	values := make([]driver.Value, len(columns))
	for {
		err := rows.Next(values)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return queryResult{}, err
		}
		if len(result.Rows) == s.maxRows {
			result.Truncated = true
			break
		}
		row := make([]interface{}, len(columns))
		for i, value := range values {
			row[i] = encodeValue(value, types[i])
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func contextError(err error) *apiError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &apiError{http.StatusGatewayTimeout, "timeout", "the request ran past its time limit"}
	}
	return &apiError{499, "canceled", "the request was canceled"}
}

// sqlError maps a database error to its status: a timeout is 504, a refused file or extension
// access 403, a transaction conflict 409, memory exhaustion 503, a full disk 507, and any other SQL
// error 400 with DuckDB's message.
func sqlError(ctx context.Context, err error) *apiError {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return contextError(context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return contextError(context.Canceled)
	}
	var duckErr *duckdb.Error
	if errors.As(err, &duckErr) {
		switch duckErr.Type {
		case duckdb.ErrorTypeInterrupt:
			return contextError(context.DeadlineExceeded)
		case duckdb.ErrorTypePermission:
			return &apiError{http.StatusForbidden, "forbidden", duckErr.Msg}
		case duckdb.ErrorTypeTransaction:
			return &apiError{http.StatusConflict, "conflict", duckErr.Msg}
		case duckdb.ErrorTypeOutOfMemory:
			return &apiError{http.StatusServiceUnavailable, "out_of_memory", duckErr.Msg}
		case duckdb.ErrorTypeIO:
			if strings.Contains(strings.ToLower(duckErr.Msg), "no space left") {
				return &apiError{http.StatusInsufficientStorage, "disk_full", duckErr.Msg}
			}
			return &apiError{http.StatusInternalServerError, "io_error", duckErr.Msg}
		case duckdb.ErrorTypeFatal, duckdb.ErrorTypeInternal:
			return &apiError{http.StatusInternalServerError, "internal", duckErr.Msg}
		}
		return &apiError{http.StatusBadRequest, "sql_error", duckErr.Msg}
	}
	if err.Error() == "empty query" {
		return &apiError{http.StatusBadRequest, "invalid_request", "sql holds no statement"}
	}
	return &apiError{http.StatusBadRequest, "sql_error", err.Error()}
}
