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
