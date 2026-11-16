// SPDX-License-Identifier: MIT

// Package server is lake-duckdb's HTTP API over one DuckDB database: POST /v1/query, /v1/exec and
// /v1/batch behind a bearer token, and an unauthenticated GET /healthz. The API is lake-sqlite's
// (lake docs/control-plane/services.md: "lake-duckdb has the same API as lake-sqlite"); docs/api.md
// documents this implementation.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

const (
	// DefaultMaxRows caps the rows one statement returns.
	DefaultMaxRows = 10000
	// DefaultMaxResponseBytes caps the values one request returns, over all its statements: 32 MiB
	// (counted as the bytes of strings and blobs, plus a few for every other value). The row cap
	// alone does not bound memory: 10,000 rows of 10 MB strings are 100 GB.
	DefaultMaxResponseBytes = 32 << 20
	// DefaultMaxBodyBytes caps a request body: 16 MiB.
	DefaultMaxBodyBytes = 16 << 20
	// DefaultRequestTimeout bounds every request, the SQL included: the statement timeout.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultConcurrency is how many API requests use the database at once; more wait for a slot
	// (within their own time limit). One more connection is kept for /healthz, so a busy service
	// still answers its probes.
	DefaultConcurrency = 4

	healthTimeout = 2 * time.Second
)

// Config configures a Server.
type Config struct {
	// Path is the database file. Its directory must exist and be writable.
	Path string
	// Token is the bearer token every route but /healthz requires.
	Token string
	// MemoryLimit is DuckDB's memory_limit in bytes; zero leaves DuckDB's default (80% of RAM).
	MemoryLimit int64
	// Threads is DuckDB's worker thread count; zero leaves DuckDB's default (the CPU count).
	Threads int
	// MaxRows, MaxResponseBytes, MaxBodyBytes, RequestTimeout and Concurrency default to the
	// constants above when zero.
	MaxRows          int
	MaxResponseBytes int64
	MaxBodyBytes     int64
	RequestTimeout   time.Duration
	Concurrency      int
	// Logger receives one line per request; nil discards them.
	Logger *slog.Logger
}

// Server serves the API over one database.
type Server struct {
	db        *sql.DB
	slots     chan struct{}
	tokenHash [sha256.Size]byte
	maxRows   int
	maxAnswer int64
	maxBody   int64
	timeout   time.Duration
	log       *slog.Logger
}

// Open opens the database, locks the engine down (Lockdown in engine.go) and checks it answers
// SELECT 1.
func Open(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Path == "" {
		return nil, errors.New("database path is empty")
	}
	if cfg.Token == "" {
		return nil, errors.New("token is empty")
	}
	s := &Server{
		tokenHash: sha256.Sum256([]byte(cfg.Token)),
		maxRows:   cfg.MaxRows,
		maxAnswer: cfg.MaxResponseBytes,
		maxBody:   cfg.MaxBodyBytes,
		timeout:   cfg.RequestTimeout,
		log:       cfg.Logger,
	}
	if s.maxRows <= 0 {
		s.maxRows = DefaultMaxRows
	}
	if s.maxAnswer <= 0 {
		s.maxAnswer = DefaultMaxResponseBytes
	}
	if s.maxBody <= 0 {
		s.maxBody = DefaultMaxBodyBytes
	}
	if s.timeout <= 0 {
		s.timeout = DefaultRequestTimeout
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	s.slots = make(chan struct{}, concurrency)
	if s.log == nil {
		s.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	db, err := openEngine(ctx, cfg.Path, cfg.MemoryLimit, cfg.Threads)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(concurrency + 1)
	db.SetMaxIdleConns(concurrency + 1)
	s.db = db
	if err := s.check(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database, which checkpoints its WAL into the file.
func (s *Server) Close() error { return s.db.Close() }

func (s *Server) check(ctx context.Context) error {
	var one int64
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return fmt.Errorf("SELECT 1 returned %d", one)
	}
	return nil
}

// Handler is the whole API with request logging.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/v1/query", s.api(s.handleQuery))
	mux.Handle("/v1/exec", s.api(s.handleExec))
	mux.Handle("/v1/batch", s.api(s.handleBatch))
	mux.Handle("/", s.authorized(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})))
	return s.logged(mux)
}

// apiError is the one error shape: {"error": {"code", "message"}}.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, status, struct {
		Error detail `json:"error"`
	}{detail{code, message}})
}

// authorized checks Authorization: Bearer <token>. Both sides are hashed first so the comparison
// takes the same time whatever the presented token's length.
func (s *Server) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		hash := sha256.Sum256([]byte(presented))
		if !ok || presented == "" || subtle.ConstantTimeCompare(hash[:], s.tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lake-duckdb"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// api wraps a POST JSON route: the token, the method, the body cap and the request timeout.
func (s *Server) api(handle func(ctx context.Context, body []byte) (interface{}, *apiError)) http.Handler {
	return s.authorized(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			return
		}
		if r.ContentLength > s.maxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("the body is over %d bytes", s.maxBody))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("the body is over %d bytes", s.maxBody))
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_request", "cannot read the body")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		result, apiErr := handle(ctx, body)
		if apiErr != nil {
			writeError(w, apiErr.status, apiErr.code, apiErr.message)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := s.check(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the database does not answer SELECT 1")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// statement is one {sql, params}.
type statement struct {
	SQL    string        `json:"sql"`
	Params []interface{} `json:"params"`
}

type batchRequest struct {
	Statements []statement `json:"statements"`
}

// queryResult answers /v1/query and a batch statement that returns data.
type queryResult struct {
	Columns   []string        `json:"columns"`
	Rows      [][]interface{} `json:"rows"`
	Truncated bool            `json:"truncated,omitempty"`
}

// execResult answers /v1/exec and a batch statement that returns no data. DuckDB has no rowid a
// client could use, so last_insert_id is always null; it is kept so a lake-sqlite client reads the
// same shape.
type execResult struct {
	RowsAffected int64  `json:"rows_affected"`
	LastInsertID *int64 `json:"last_insert_id"`
}

// decode reads a JSON body strictly: unknown fields and trailing data are refused, and numbers stay
// json.Number so an integer parameter binds as an integer.
func decode(body []byte, into interface{}) *apiError {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return &apiError{http.StatusBadRequest, "invalid_request", fmt.Sprintf("invalid JSON body: %v", err)}
	}
	if decoder.More() {
		return &apiError{http.StatusBadRequest, "invalid_request", "invalid JSON body: trailing data"}
	}
	return nil
}

func (st statement) args() ([]interface{}, *apiError) {
	if strings.TrimSpace(st.SQL) == "" {
		return nil, &apiError{http.StatusBadRequest, "invalid_request", "sql is required"}
	}
	if apiErr := guard(st.SQL); apiErr != nil {
		return nil, apiErr
	}
	args, err := decodeParams(st.Params)
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, "invalid_params", err.Error()}
	}
	return args, nil
}

func (s *Server) handleOne(ctx context.Context, body []byte, how mode) (interface{}, *apiError) {
	var st statement
	if err := decode(body, &st); err != nil {
		return nil, err
	}
	args, apiErr := st.args()
	if apiErr != nil {
		return nil, apiErr
	}
	var answer result
	budget := s.maxAnswer
	apiErr = s.withConn(ctx, func(conn *duckdb.Conn) *apiError {
		var runErr *apiError
		answer, runErr = s.run(ctx, conn, st.SQL, args, how, &budget)
		return runErr
	})
	if apiErr != nil {
		return nil, apiErr
	}
	return answer.value(), nil
}

func (s *Server) handleQuery(ctx context.Context, body []byte) (interface{}, *apiError) {
	return s.handleOne(ctx, body, modeQuery)
}

func (s *Server) handleExec(ctx context.Context, body []byte) (interface{}, *apiError) {
	return s.handleOne(ctx, body, modeExec)
}

// handleBatch runs every statement in one transaction, in order. A statement that returns data
// answers like /v1/query, any other like /v1/exec. The first error rolls the whole batch back.
func (s *Server) handleBatch(ctx context.Context, body []byte) (interface{}, *apiError) {
	var req batchRequest
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if len(req.Statements) == 0 {
		return nil, &apiError{http.StatusBadRequest, "invalid_request", "statements is empty"}
	}
	args := make([][]interface{}, len(req.Statements))
	for i, st := range req.Statements {
		a, apiErr := st.args()
		if apiErr != nil {
			apiErr.message = fmt.Sprintf("statements[%d]: %s", i, apiErr.message)
			return nil, apiErr
		}
		args[i] = a
	}
	results := make([]interface{}, 0, len(req.Statements))
	budget := s.maxAnswer // shared by every statement in the batch
	apiErr := s.withConn(ctx, func(conn *duckdb.Conn) *apiError {
		if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION", nil); err != nil {
			return sqlError(ctx, err)
		}
		// withConn rolls back whatever is still open when this returns without committing.
		for i, st := range req.Statements {
			answer, apiErr := s.run(ctx, conn, st.SQL, args[i], modeAuto, &budget)
			if apiErr != nil {
				apiErr.message = fmt.Sprintf("statements[%d]: %s", i, apiErr.message)
				return apiErr
			}
			results = append(results, answer.value())
		}
		if _, err := conn.ExecContext(ctx, "COMMIT", nil); err != nil {
			return sqlError(ctx, err)
		}
		return nil
	})
	if apiErr != nil {
		return nil, apiErr
	}
	return struct {
		Results []interface{} `json:"results"`
	}{results}, nil
}

// statusRecorder captures what a handler wrote, for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// logged writes one JSON line per request: method, path, status, response size and duration. It
// never logs a header, the body, the SQL or its parameters, so neither the token nor data reaches
// the log.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		s.log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Int("bytes", recorder.bytes),
			slog.Int64("request_bytes", r.ContentLength),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}
