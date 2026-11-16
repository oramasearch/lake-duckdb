// SPDX-License-Identifier: MIT

package server

import (
	"errors"
	"sync"

	"github.com/duckdb/duckdb-go/v2/mapping"
)

// splitter counts the statements in a sql with DuckDB's own parser, before anything runs.
//
// The driver prepares a sql holding several statements by executing every one but the last, so a
// check made after preparing (the statement type: COPY, EXPORT, DETACH, a transaction statement in a
// batch) would see only the last one. The service therefore accepts exactly one statement per sql,
// and counts them first. Counting needs a DuckDB connection; it uses its own in-memory database,
// on which nothing is ever executed or prepared (extracting statements only parses), so it holds no
// data and opens no file.
type splitter struct {
	mu   sync.Mutex
	db   mapping.Database
	conn mapping.Connection
}

func newSplitter() (*splitter, error) {
	s := &splitter{}
	if mapping.Open(":memory:", &s.db) == mapping.StateError {
		return nil, errors.New("cannot open the statement parser")
	}
	if mapping.Connect(s.db, &s.conn) == mapping.StateError {
		mapping.Close(&s.db)
		return nil, errors.New("cannot connect the statement parser")
	}
	return s, nil
}

// count returns how many statements sql holds, or DuckDB's parser error when it holds none it can
// read (an empty message means sql holds nothing but whitespace or comments).
func (s *splitter) count(sql string) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var extracted mapping.ExtractedStatements
	n := mapping.ExtractStatements(s.conn, sql, &extracted)
	defer mapping.DestroyExtracted(&extracted)
	if n == 0 {
		return 0, mapping.ExtractStatementsError(extracted)
	}
	return int(n), ""
}

func (s *splitter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	mapping.Disconnect(&s.conn)
	mapping.Close(&s.db)
}
