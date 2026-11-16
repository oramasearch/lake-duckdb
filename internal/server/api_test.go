// SPDX-License-Identifier: MIT

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (h *harness) query(sql string, params ...interface{}) (int, map[string]interface{}) {
	h.t.Helper()
	if params == nil {
		params = []interface{}{}
	}
	return h.post("/v1/query", map[string]interface{}{"sql": sql, "params": params})
}

func TestAuth(t *testing.T) {
	h := newHarness(t, Config{})
	query := map[string]interface{}{"sql": "SELECT 1", "params": []interface{}{}}
	cases := []struct {
		name   string
		method string
		path   string
		header string
		body   interface{}
		want   int
		code   string
	}{
		{"no header", http.MethodPost, "/v1/query", "", query, http.StatusUnauthorized, "unauthorized"},
		{"wrong token", http.MethodPost, "/v1/query", "Bearer nope", query, http.StatusUnauthorized, "unauthorized"},
		{"token prefix", http.MethodPost, "/v1/query", "Bearer " + testToken[:10], query, http.StatusUnauthorized, "unauthorized"},
		{"token with suffix", http.MethodPost, "/v1/query", "Bearer " + testToken + "x", query, http.StatusUnauthorized, "unauthorized"},
		{"basic scheme", http.MethodPost, "/v1/query", "Basic " + testToken, query, http.StatusUnauthorized, "unauthorized"},
		{"empty bearer", http.MethodPost, "/v1/query", "Bearer ", query, http.StatusUnauthorized, "unauthorized"},
		{"exec without token", http.MethodPost, "/v1/exec", "", query, http.StatusUnauthorized, "unauthorized"},
		{"batch without token", http.MethodPost, "/v1/batch", "", map[string]interface{}{"statements": []interface{}{query}}, http.StatusUnauthorized, "unauthorized"},
		{"unknown route without token", http.MethodGet, "/v1/tables", "", nil, http.StatusUnauthorized, "unauthorized"},
		{"unknown route with token", http.MethodGet, "/v1/tables", "Bearer " + testToken, nil, http.StatusNotFound, "not_found"},
		{"good token", http.MethodPost, "/v1/query", "Bearer " + testToken, query, http.StatusOK, ""},
		{"wrong method", http.MethodGet, "/v1/query", "Bearer " + testToken, nil, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"healthz needs no token", http.MethodGet, "/healthz", "", nil, http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader
			if tc.body != nil {
				raw, _ := json.Marshal(tc.body)
				reader = bytes.NewReader(raw)
			}
			req, _ := http.NewRequest(tc.method, h.http.URL+tc.path, reader)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := h.http.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.want || errorCode(body) != tc.code {
				t.Fatalf("got %d %v, want %d %q", resp.StatusCode, body, tc.want, tc.code)
			}
		})
	}
}

func TestQueryTypes(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE t (i BIGINT, r DOUBLE, s VARCHAR, b BLOB, n VARCHAR, ok BOOLEAN)")
	h.mustExec("INSERT INTO t VALUES (?, ?, ?, ?, ?, ?)",
		9007199254740993, 1.0, "héllo", map[string]interface{}{"$base64": "AAEC/w=="}, nil, true)
	cases := []struct {
		name   string
		sql    string
		params []interface{}
		want   string
	}{
		{"every column type", "SELECT i, r, s, b, n, ok FROM t", nil,
			`{"columns":["i","r","s","b","n","ok"],"rows":[[9007199254740993,1.0,"héllo",{"$base64":"AAEC/w=="},null,true]]}`},
		{"integer parameter binds as BIGINT", "SELECT typeof(?) AS t", []interface{}{42},
			`{"columns":["t"],"rows":[["BIGINT"]]}`},
		{"float parameter binds as DOUBLE", "SELECT typeof(?) AS t", []interface{}{2.5},
			`{"columns":["t"],"rows":[["DOUBLE"]]}`},
		{"boolean parameter binds as BOOLEAN", "SELECT typeof(?) AS t", []interface{}{true},
			`{"columns":["t"],"rows":[["BOOLEAN"]]}`},
		{"null parameter", "SELECT ? IS NULL AS v", []interface{}{nil},
			`{"columns":["v"],"rows":[[true]]}`},
		{"blob parameter", "SELECT octet_length(?::BLOB) AS v", []interface{}{map[string]interface{}{"$base64": "AAEC"}},
			`{"columns":["v"],"rows":[[3]]}`},
		{"hugeint is an exact integer", "SELECT 170141183460469231731687303715884105727::HUGEINT AS v", nil,
			`{"columns":["v"],"rows":[[170141183460469231731687303715884105727]]}`},
		{"decimal is an exact string", "SELECT 12345.67::DECIMAL(10,2) AS v", nil,
			`{"columns":["v"],"rows":[["12345.67"]]}`},
		{"float keeps its own precision", "SELECT 0.1::FLOAT AS f, 2::DOUBLE AS d", nil,
			`{"columns":["f","d"],"rows":[[0.1,2.0]]}`},
		{"infinity and NaN are strings", "SELECT 'inf'::DOUBLE AS a, '-inf'::DOUBLE AS b, 'nan'::DOUBLE AS c", nil,
			`{"columns":["a","b","c"],"rows":[["Infinity","-Infinity","NaN"]]}`},
		{"uuid", "SELECT '0e0f9a4e-1c2d-4b5a-8f00-123456789abc'::UUID AS u", nil,
			`{"columns":["u"],"rows":[["0e0f9a4e-1c2d-4b5a-8f00-123456789abc"]]}`},
		{"dates and times", "SELECT DATE '2026-09-26' AS d, TIME '10:11:12.5' AS t, TIMESTAMP '2026-09-26 10:11:12' AS ts, TIMESTAMPTZ '2026-09-26 10:11:12+00' AS tz", nil,
			`{"columns":["d","t","ts","tz"],"rows":[["2026-09-26","10:11:12.5","2026-09-26 10:11:12","2026-09-26T10:11:12Z"]]}`},
		{"interval", "SELECT INTERVAL '1 month 2 days 3 seconds' AS v", nil,
			`{"columns":["v"],"rows":[[{"days":2,"micros":3000000,"months":1}]]}`},
		{"list, struct and map", "SELECT [1, 2] AS l, {'a': 1, 'b': 'x'} AS s, MAP {'k': 1} AS m", nil,
			`{"columns":["l","s","m"],"rows":[[[1,2],{"a":1,"b":"x"},[{"key":"k","value":1}]]]}`},
		{"no rows", "SELECT i FROM t WHERE false", nil,
			`{"columns":["i"],"rows":[]}`},
		{"insert through query answers its count", "INSERT INTO t (i) VALUES (1), (2)", nil,
			`{"columns":["Count"],"rows":[[2]]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.query(tc.sql, tc.params...)
			if status != http.StatusOK {
				t.Fatalf("status %d: %v", status, body)
			}
			if got := canonical(t, body); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRequestErrors(t *testing.T) {
	h := newHarness(t, Config{MaxBodyBytes: 1024})
	cases := []struct {
		name string
		path string
		body interface{}
		want int
		code string
	}{
		{"SQL error", "/v1/query", map[string]interface{}{"sql": "SELECT * FROM missing", "params": []interface{}{}}, http.StatusBadRequest, "sql_error"},
		{"syntax error", "/v1/exec", map[string]interface{}{"sql": "CREATE TABL x", "params": []interface{}{}}, http.StatusBadRequest, "sql_error"},
		{"empty sql", "/v1/query", map[string]interface{}{"sql": " ", "params": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
		{"comment only", "/v1/query", map[string]interface{}{"sql": "-- nothing", "params": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
		{"not JSON", "/v1/query", "{", http.StatusBadRequest, "invalid_request"},
		{"unknown field", "/v1/query", map[string]interface{}{"sql": "SELECT 1", "args": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
		{"nested array parameter", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{[]interface{}{1}}}, http.StatusBadRequest, "invalid_params"},
		{"object parameter", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{map[string]interface{}{"x": 1}}}, http.StatusBadRequest, "invalid_params"},
		{"bad base64", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{map[string]interface{}{"$base64": "***"}}}, http.StatusBadRequest, "invalid_params"},
		{"integer overflow", "/v1/query", `{"sql":"SELECT ?","params":[99999999999999999999]}`, http.StatusBadRequest, "invalid_params"},
		{"body over the cap", "/v1/query", map[string]interface{}{"sql": "SELECT ?", "params": []interface{}{strings.Repeat("x", 2048)}}, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"empty batch", "/v1/batch", map[string]interface{}{"statements": []interface{}{}}, http.StatusBadRequest, "invalid_request"},
		{"transaction statement in a batch", "/v1/batch", map[string]interface{}{"statements": []interface{}{map[string]interface{}{"sql": "COMMIT"}}}, http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.post(tc.path, tc.body)
			if status != tc.want || errorCode(body) != tc.code {
				t.Fatalf("got %d %v, want %d %q", status, body, tc.want, tc.code)
			}
			if msg, _ := body["error"].(map[string]interface{})["message"].(string); msg == "" {
				t.Fatalf("error without a message: %v", body)
			}
		})
	}
}

func TestBodyCapIsSixteenMiB(t *testing.T) {
	h := newHarness(t, Config{})
	big := `{"sql":"SELECT 1","params":["` + strings.Repeat("x", DefaultMaxBodyBytes) + `"]}`
	status, body := h.post("/v1/query", big)
	if status != http.StatusRequestEntityTooLarge || errorCode(body) != "body_too_large" {
		t.Fatalf("got %d %v", status, body)
	}
}

func TestExec(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE t (id INTEGER PRIMARY KEY, v VARCHAR)")
	cases := []struct {
		name   string
		sql    string
		params []interface{}
		want   string
	}{
		{"insert", "INSERT INTO t VALUES (?, ?)", []interface{}{1, "a"}, `{"last_insert_id":null,"rows_affected":1}`},
		{"insert two", "INSERT INTO t VALUES (?, ?), (?, ?)", []interface{}{2, "b", 3, "c"}, `{"last_insert_id":null,"rows_affected":2}`},
		{"update", "UPDATE t SET v = upper(v) WHERE id > ?", []interface{}{1}, `{"last_insert_id":null,"rows_affected":2}`},
		{"delete", "DELETE FROM t WHERE v = ?", []interface{}{"a"}, `{"last_insert_id":null,"rows_affected":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonical(t, h.mustExec(tc.sql, tc.params...)); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	_, body := h.query("SELECT id, v FROM t ORDER BY id")
	if got := canonical(t, body["rows"]); got != `[[2,"B"],[3,"C"]]` {
		t.Fatalf("rows after exec: %s", got)
	}
	status, body := h.post("/v1/exec", map[string]interface{}{"sql": "INSERT INTO t VALUES (2, 'dup')"})
	if status != http.StatusBadRequest || errorCode(body) != "sql_error" {
		t.Fatalf("a primary key violation was accepted: %d %v", status, body)
	}
}

// TestNoTransactionOutlivesItsRequest: a BEGIN sent alone is rolled back when its request ends, so
// the next request on the same pooled connection is not inside it.
func TestNoTransactionOutlivesItsRequest(t *testing.T) {
	h := newHarness(t, Config{Concurrency: 1})
	h.mustExec("CREATE TABLE t (v INTEGER)")
	h.mustExec("BEGIN TRANSACTION")
	h.mustExec("INSERT INTO t VALUES (1)")
	h.mustExec("BEGIN TRANSACTION") // fails inside an open transaction
	_, body := h.query("SELECT count(*) FROM t")
	if got := canonical(t, body["rows"]); got != `[[1]]` {
		t.Fatalf("count = %s", got)
	}
}

func TestBatch(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE SEQUENCE ids")
	h.mustExec("CREATE TABLE t (id INTEGER PRIMARY KEY DEFAULT nextval('ids'), v VARCHAR NOT NULL)")
	count := func() string {
		_, body := h.query("SELECT count(*) FROM t")
		return canonical(t, body["rows"])
	}
	stmt := func(sql string, params ...interface{}) map[string]interface{} {
		if params == nil {
			params = []interface{}{}
		}
		return map[string]interface{}{"sql": sql, "params": params}
	}
	cases := []struct {
		name       string
		statements []interface{}
		wantStatus int
		wantCode   string
		wantBody   string
		wantCount  string
	}{
		{
			name: "every statement commits",
			statements: []interface{}{
				stmt("INSERT INTO t (v) VALUES (?)", "a"),
				stmt("INSERT INTO t (v) VALUES (?)", "b"),
				stmt("CREATE TABLE other (x INTEGER)"),
				stmt("SELECT v FROM t ORDER BY id"),
			},
			wantStatus: http.StatusOK,
			wantBody:   `{"results":[{"last_insert_id":null,"rows_affected":1},{"last_insert_id":null,"rows_affected":1},{"last_insert_id":null,"rows_affected":0},{"columns":["v"],"rows":[["a"],["b"]]}]}`,
			wantCount:  `[[2]]`,
		},
		{
			name: "an error rolls every statement back",
			statements: []interface{}{
				stmt("INSERT INTO t (v) VALUES (?)", "c"),
				stmt("DELETE FROM t"),
				stmt("INSERT INTO t (v) VALUES (NULL)"),
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "sql_error",
			wantCount:  `[[2]]`,
		},
		{
			name: "a bad parameter runs nothing",
			statements: []interface{}{
				stmt("DELETE FROM t"),
				stmt("INSERT INTO t (v) VALUES (?)", []interface{}{1}),
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_params",
			wantCount:  `[[2]]`,
		},
		{
			name:       "returning answers rows",
			statements: []interface{}{stmt("INSERT INTO t (v) VALUES (?) RETURNING v", "d")},
			wantStatus: http.StatusOK,
			wantBody:   `{"results":[{"columns":["v"],"rows":[["d"]]}]}`,
			wantCount:  `[[3]]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.post("/v1/batch", map[string]interface{}{"statements": tc.statements})
			if status != tc.wantStatus || errorCode(body) != tc.wantCode {
				t.Fatalf("got %d %v, want %d %q", status, body, tc.wantStatus, tc.wantCode)
			}
			if tc.wantBody != "" {
				if got := canonical(t, body); got != tc.wantBody {
					t.Fatalf("got  %s\nwant %s", got, tc.wantBody)
				}
			}
			if tc.wantCode != "" {
				if msg := body["error"].(map[string]interface{})["message"].(string); !strings.HasPrefix(msg, "statements[") {
					t.Fatalf("the error does not name its statement: %q", msg)
				}
			}
			if got := count(); got != tc.wantCount {
				t.Fatalf("count after batch = %s, want %s", got, tc.wantCount)
			}
		})
	}
}

func TestTruncation(t *testing.T) {
	cases := []struct {
		name      string
		maxRows   int
		rows      int
		wantRows  int
		truncated bool
	}{
		{"under the cap", 5, 4, 4, false},
		{"at the cap", 5, 5, 5, false},
		{"over the cap", 5, 6, 5, true},
		{"the default cap is 10000", 0, DefaultMaxRows + 1, DefaultMaxRows, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{MaxRows: tc.maxRows})
			sql := fmt.Sprintf("SELECT range AS x FROM range(%d)", tc.rows)
			for _, path := range []string{"/v1/query", "/v1/batch"} {
				var status int
				var body map[string]interface{}
				if path == "/v1/query" {
					status, body = h.query(sql)
				} else {
					status, body = h.post(path, map[string]interface{}{"statements": []interface{}{map[string]interface{}{"sql": sql, "params": []interface{}{}}}})
					body = body["results"].([]interface{})[0].(map[string]interface{})
				}
				if status != http.StatusOK {
					t.Fatalf("%s: %d %v", path, status, body)
				}
				rows := body["rows"].([]interface{})
				truncated, _ := body["truncated"].(bool)
				if len(rows) != tc.wantRows || truncated != tc.truncated {
					t.Fatalf("%s: %d rows, truncated %v; want %d, %v", path, len(rows), truncated, tc.wantRows, tc.truncated)
				}
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, Config{})
	status, body := h.call(http.MethodGet, "/healthz", "", nil)
	if status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthy: %d %v", status, body)
	}
	status, body = h.call(http.MethodPost, "/healthz", "", nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz: %d %v", status, body)
	}
	// A closed database no longer answers SELECT 1.
	_ = h.server.Close()
	status, body = h.call(http.MethodGet, "/healthz", "", nil)
	if status != http.StatusServiceUnavailable || errorCode(body) != "unavailable" {
		t.Fatalf("closed database: %d %v", status, body)
	}
}

func TestOpenRefusesAnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing directory", Config{Path: filepath.Join(dir, "missing", "main.duckdb"), Token: testToken}},
		{"no token", Config{Path: filepath.Join(dir, "main.duckdb")}},
		{"no path", Config{Token: testToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if srv, err := Open(context.Background(), tc.cfg); err == nil {
				_ = srv.Close()
				t.Fatal("Open succeeded")
			}
		})
	}
}

func TestLogsNeverCarryTokenSQLOrParams(t *testing.T) {
	h := newHarness(t, Config{})
	secret := "parameter-that-must-not-be-logged"
	h.mustExec("CREATE TABLE secrets_table_name (v VARCHAR)")
	h.mustExec("INSERT INTO secrets_table_name VALUES (?)", secret)
	h.call(http.MethodPost, "/v1/query", "wrong-token-value", map[string]interface{}{"sql": "SELECT 1", "params": []interface{}{}})
	lines := strings.Split(strings.TrimSpace(h.logs.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one log line per request, got %d:\n%s", len(lines), h.logs.String())
	}
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		for _, forbidden := range []string{testToken, "wrong-token-value", secret, "secrets_table_name"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("log line carries %q: %s", forbidden, line)
			}
		}
		if entry["path"] == nil || entry["status"] == nil {
			t.Fatalf("log line lacks path or status: %s", line)
		}
	}
}

// TestStatementTimeout: a statement running past the request timeout is interrupted and answers
// 504, and the service keeps answering.
func TestStatementTimeout(t *testing.T) {
	h := newHarness(t, Config{RequestTimeout: 300 * time.Millisecond, Threads: 2})
	start := time.Now()
	status, body := h.query("SELECT count(*) FROM range(1000000) a, range(1000000) b WHERE a.range * b.range = 7")
	if status != http.StatusGatewayTimeout || errorCode(body) != "timeout" {
		t.Fatalf("got %d %v", status, body)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the statement was not interrupted: it ran %v", elapsed)
	}
	if status, body := h.call(http.MethodGet, "/healthz", "", nil); status != http.StatusOK {
		t.Fatalf("healthz after a timeout: %d %v", status, body)
	}
	if status, body := h.query("SELECT 1"); status != http.StatusOK {
		t.Fatalf("a query after a timeout: %d %v", status, body)
	}
}

func TestDatabaseIsTheFileAtPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.duckdb")
	h := newHarness(t, Config{Path: path})
	h.mustExec("CREATE TABLE t (v INTEGER)")
	h.mustExec("INSERT INTO t VALUES (42)")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// Reopen: the data is in the file.
	h.http.Close()
	if err := h.server.Close(); err != nil {
		t.Fatal(err)
	}
	again := newHarness(t, Config{Path: path})
	_, body := again.query("SELECT v FROM t")
	if got := canonical(t, body["rows"]); got != `[[42]]` {
		t.Fatalf("after reopening: %s", got)
	}
}
