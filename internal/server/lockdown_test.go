// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestLockdownRefusesEveryEscape tries each way customer SQL could reach past its own database —
// the host's files, another database file, the network, extensions — and each way it could undo the
// lockdown. Every one must fail, through every route that runs SQL.
func TestLockdownRefusesEveryEscape(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, Config{Path: filepath.Join(dir, "main.duckdb")})
	// Files the engine must not reach: one beside the database, one elsewhere on the host.
	outside := filepath.Join(t.TempDir(), "secret.csv")
	if err := os.WriteFile(outside, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.duckdb")
	copyTarget := filepath.Join(dir, "out.csv")
	cases := []struct {
		name string
		sql  string
	}{
		// The host's files.
		{"read_csv of /etc/passwd", "SELECT * FROM read_csv('/etc/passwd')"},
		{"read_csv_auto of a host file", "SELECT * FROM read_csv_auto('" + outside + "')"},
		{"a file path as a table", "SELECT * FROM '" + outside + "'"},
		{"read_text", "SELECT * FROM read_text('/etc/hosts')"},
		{"read_blob", "SELECT * FROM read_blob('/etc/hosts')"},
		{"read_parquet", "SELECT * FROM read_parquet('/tmp/x.parquet')"},
		{"read_json", "SELECT * FROM read_json('/etc/hosts')"},
		{"glob", "SELECT * FROM glob('/etc/*')"},
		// Writing outside the database.
		{"COPY to a path", "COPY (SELECT 42) TO '" + copyTarget + "'"},
		{"COPY a table to a path", "COPY (SELECT 'x' AS v) TO '/tmp/lake-duckdb-escape.parquet' (FORMAT parquet)"},
		{"EXPORT DATABASE", "EXPORT DATABASE '" + filepath.Join(dir, "export") + "'"},
		// Another database file.
		{"ATTACH another file", "ATTACH '" + other + "' AS other"},
		{"ATTACH a sqlite file", "ATTACH '" + filepath.Join(dir, "x.db") + "' AS x (TYPE sqlite)"},
		// Extensions and the network.
		{"INSTALL httpfs", "INSTALL httpfs"},
		{"FORCE INSTALL from a URL", "FORCE INSTALL httpfs FROM 'http://127.0.0.1:1'"},
		{"LOAD httpfs", "LOAD httpfs"},
		{"LOAD a file", "LOAD '" + filepath.Join(dir, "evil.duckdb_extension") + "'"},
		{"an http URL as a table", "SELECT * FROM 'https://example.com/data.csv'"},
		{"CREATE SECRET", "CREATE SECRET s (TYPE s3, KEY_ID 'a', SECRET 'b')"},
		// Extensions linked into the binary but not loaded: tpch and tpcds stay unloaded.
		{"LOAD tpch", "LOAD tpch"},
		{"LOAD tpcds", "LOAD tpcds"},
		{"tpch dbgen", "CALL dbgen(sf = 0.01)"},
		{"tpcds dsdgen", "CALL dsdgen(sf = 0.01)"},
		// Undoing the lockdown after it is locked.
		{"SET enable_external_access=true", "SET enable_external_access = true"},
		{"RESET enable_external_access", "RESET enable_external_access"},
		{"SET GLOBAL enable_external_access", "SET GLOBAL enable_external_access = true"},
		{"PRAGMA enable_external_access", "PRAGMA enable_external_access = true"},
		{"SET autoinstall_known_extensions", "SET autoinstall_known_extensions = true"},
		{"SET autoload_known_extensions", "SET autoload_known_extensions = true"},
		{"SET allow_community_extensions", "SET allow_community_extensions = true"},
		{"SET lock_configuration=false", "SET lock_configuration = false"},
		{"RESET lock_configuration", "RESET lock_configuration"},
		{"SET memory_limit", "SET memory_limit = '64GB'"},
		{"SET threads", "SET threads = 64"},
		{"RESET ALL", "RESET ALL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []string{"/v1/query", "/v1/exec", "/v1/batch"} {
				body := map[string]interface{}{"sql": tc.sql}
				if route == "/v1/batch" {
					body = map[string]interface{}{"statements": []interface{}{body}}
				}
				status, answer := h.post(route, body)
				if status == http.StatusOK {
					t.Fatalf("%s accepted %q: %v", route, tc.sql, answer)
				}
				if code := errorCode(answer); code != "forbidden" && code != "sql_error" {
					t.Fatalf("%s %q: unexpected %d %v", route, tc.sql, status, answer)
				}
			}
		})
	}
	// Nothing reached the disk outside the database.
	for _, path := range []string{other, copyTarget, filepath.Join(dir, "export"), "/tmp/lake-duckdb-escape.parquet"} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s was written", path)
		}
	}
	// And the settings still hold.
	_, answer := h.query("SELECT current_setting('enable_external_access'), current_setting('lock_configuration'), " +
		"current_setting('autoinstall_known_extensions'), current_setting('autoload_known_extensions'), current_setting('allow_community_extensions')")
	if got := canonical(t, answer["rows"]); got != `[[false,true,false,false,false]]` {
		t.Fatalf("settings after the attempts: %s", got)
	}
}

// TestFileAccessIsForbidden: a refused file access answers 403 forbidden with DuckDB's reason.
func TestFileAccessIsForbidden(t *testing.T) {
	h := newHarness(t, Config{})
	status, body := h.query("SELECT * FROM read_csv('/etc/passwd')")
	if status != http.StatusForbidden || errorCode(body) != "forbidden" {
		t.Fatalf("got %d %v", status, body)
	}
	message := body["error"].(map[string]interface{})["message"].(string)
	if !strings.Contains(message, "disabled by configuration") {
		t.Fatalf("message %q does not say why", message)
	}
}

// TestLockdownHoldsOnEveryConnection runs the check on many connections at once, so the pool opens
// several: the settings are the database's, not the first connection's.
func TestLockdownHoldsOnEveryConnection(t *testing.T) {
	h := newHarness(t, Config{Concurrency: 4})
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := h.query("SELECT current_setting('enable_external_access') AS e, current_setting('lock_configuration') AS l FROM range(20000) LIMIT 1")
			if status != http.StatusOK || canonical(t, body["rows"]) != `[[false,true]]` {
				errs <- canonical(t, body)
			}
			if status, _ := h.query("SELECT * FROM read_csv('/etc/passwd')"); status != http.StatusForbidden {
				errs <- "read_csv was not forbidden"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestLimitsAreApplied: memory_limit and threads come from the configuration.
func TestLimitsAreApplied(t *testing.T) {
	h := newHarness(t, Config{MemoryLimit: 256 << 20, Threads: 2})
	_, body := h.query("SELECT current_setting('memory_limit'), current_setting('threads')")
	if got := canonical(t, body["rows"]); got != `[["256.0 MiB",2]]` {
		t.Fatalf("limits: %s", got)
	}
}

// TestOpenFailsClosed: a lockdown statement that fails stops Open, so an unlocked engine is never
// served.
func TestOpenFailsClosed(t *testing.T) {
	if _, err := openEngine(context.Background(), filepath.Join(t.TempDir(), "main.duckdb"), 0, -5); err != nil {
		t.Fatalf("negative threads are skipped, not applied: %v", err)
	}
	for _, setting := range lockdown(1<<30, 2) {
		if !strings.HasPrefix(setting, "SET ") {
			t.Fatalf("unexpected lockdown statement %q", setting)
		}
	}
	settings := lockdown(0, 0)
	if last := settings[len(settings)-1]; last != "SET lock_configuration = true" {
		t.Fatalf("the lock is not last: %q", last)
	}
}

// TestOwnFilesAreNotWritable: enable_external_access leaves DuckDB its own files, so COPY and
// EXPORT are refused by statement type. Overwriting main.duckdb or its WAL would leave a pod that
// never starts again. Afterwards the data is still there, and the file reopens.
func TestOwnFilesAreNotWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.duckdb")
	h := newHarness(t, Config{Path: path})
	h.mustExec("CREATE TABLE t (v INTEGER)")
	h.mustExec("INSERT INTO t VALUES (42)")
	for _, sql := range []string{
		"COPY (SELECT 'x' AS g) TO '" + path + "' (HEADER false, USE_TMP_FILE false)",
		"COPY (SELECT 'x' AS g) TO '" + path + ".wal' (HEADER false)",
		"COPY (SELECT 'x' AS g) TO '" + path + ".tmp/x.csv'",
		"COPY t TO '" + path + "'",
		"COPY t FROM '" + path + ".wal'",
		"EXPORT DATABASE '" + path + ".tmp'",
		"DETACH main_db",
		"USE memory; DETACH main_db",
	} {
		for _, route := range []string{"/v1/query", "/v1/exec", "/v1/batch"} {
			body := map[string]interface{}{"sql": sql}
			if route == "/v1/batch" {
				body = map[string]interface{}{"statements": []interface{}{body}}
			}
			status, answer := h.post(route, body)
			if status == http.StatusOK {
				t.Fatalf("%s accepted %q: %v", route, sql, answer)
			}
		}
	}
	if _, body := h.query("SELECT v FROM t"); canonical(t, body["rows"]) != `[[42]]` {
		t.Fatalf("data after the attempts: %v", body)
	}
	if err := h.server.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), Config{Path: path, Token: testToken})
	if err != nil {
		t.Fatalf("the database does not reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOneStatementPerSQL: the driver would run every statement but the last while preparing, past
// every check made on the prepared statement, so a sql holding more than one is refused before
// anything runs. "COMMIT; SELECT 1" inside a batch used to commit the batch half-way.
func TestOneStatementPerSQL(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE bt (v INTEGER)")
	for _, sql := range []string{"INSERT INTO bt VALUES (1); SELECT 1", "SELECT 1; SELECT 2", "SELECT 1;;SELECT 2"} {
		for _, route := range []string{"/v1/query", "/v1/exec"} {
			status, body := h.post(route, map[string]interface{}{"sql": sql})
			if status != http.StatusBadRequest || errorCode(body) != "invalid_request" {
				t.Fatalf("%s %q: %d %v", route, sql, status, body)
			}
		}
	}
	status, body := h.post("/v1/batch", map[string]interface{}{"statements": []interface{}{
		map[string]interface{}{"sql": "INSERT INTO bt VALUES (2)"},
		map[string]interface{}{"sql": "COMMIT; SELECT 1"},
		map[string]interface{}{"sql": "INSERT INTO bt VALUES (3)"},
	}})
	if status != http.StatusBadRequest {
		t.Fatalf("batch with a hidden COMMIT: %d %v", status, body)
	}
	if _, body := h.query("SELECT count(*) FROM bt"); canonical(t, body["rows"]) != `[[0]]` {
		t.Fatalf("rows after the refused statements: %v", body)
	}
	// One statement with a trailing semicolon or comment is still one.
	for _, sql := range []string{"SELECT 1;", "SELECT 1; -- done", "SELECT ';' AS s"} {
		if status, body := h.query(sql); status != http.StatusOK {
			t.Fatalf("%q: %d %v", sql, status, body)
		}
	}
	for _, sql := range []string{"", "  ", "-- only a comment", ";"} {
		if status, body := h.query(sql); status != http.StatusBadRequest || errorCode(body) != "invalid_request" {
			t.Fatalf("%q: %d %v", sql, status, body)
		}
	}
}

// TestNoConnectionStateOutlivesItsRequest: each request gets a fresh connection, so nothing a
// client sets on one (USE, search_path, temporary tables, prepared statements, variables) reaches
// the next request, even with one connection in the pool.
func TestNoConnectionStateOutlivesItsRequest(t *testing.T) {
	h := newHarness(t, Config{Concurrency: 1})
	h.mustExec("ATTACH ':memory:' AS m")
	for _, sql := range []string{
		"USE m", "SET search_path = 'm'", "CREATE TEMP TABLE tt (v INTEGER)", "PREPARE p AS SELECT 1",
		"SET VARIABLE x = 1",
	} {
		h.mustExec(sql)
	}
	_, body := h.query("SELECT current_database(), current_setting('search_path'), getvariable('x')")
	if got := canonical(t, body["rows"]); got != `[["main_db","",null]]` {
		t.Fatalf("state after the request that set it: %s", canonical(t, body))
	}
	if status, _ := h.query("SELECT * FROM tt"); status == http.StatusOK {
		t.Fatal("a temporary table outlived its request")
	}
	if status, _ := h.query("EXECUTE p"); status == http.StatusOK {
		t.Fatal("a prepared statement outlived its request")
	}
}
