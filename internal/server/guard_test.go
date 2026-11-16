// SPDX-License-Identifier: MIT

package server

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestGuardRefusesEngineStateChanges sends every statement that changes engine state past
// lock_configuration (guard.go), in the shapes a client could hide it in, through every route that
// runs SQL. Each must be refused with 403 before it reaches DuckDB, and afterwards logging and
// profiling must still be off and the service must still answer.
func TestGuardRefusesEngineStateChanges(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, Config{Path: filepath.Join(dir, "main.duckdb")})
	logFile := filepath.Join(dir, "log")
	cases := []string{
		// Logging: to the pod's stdout, or to a file, which wedges every later statement.
		"CALL enable_logging(storage='stdout', level='trace')",
		"CALL enable_logging(storage='file', storage_path='" + logFile + "')",
		"CALL enable_logging(level='debug')",
		"SELECT * FROM enable_logging(storage='stdout')",
		"CALL disable_logging()",
		"CALL truncate_duckdb_logs()",
		"SELECT write_log('x', level := 'error')",
		// The same, hidden.
		`SELECT * FROM "ENABLE_LOGGING"(storage='stdout')`,
		"SELECT * FROM system.main.enable_logging(storage='stdout')",
		"CALL Enable_Logging(storage='stdout')",
		"CREATE MACRO m() AS TABLE FROM enable_logging(storage='stdout')",
		"PREPARE p AS CALL enable_logging(storage='stdout')",
		"CREATE VIEW v AS FROM enable_logging(storage='stdout')",
		// Dynamic SQL: the name is built at run time, so query() itself is refused.
		"FROM query('FROM enable_' || 'logging(storage=''stdout'')')",
		"FROM query ('SELECT 1')",
		"FROM QUERY('SELECT 1')",
		`FROM "query"('SELECT 1')`,
		"FROM main.query('SELECT 1')",
		"FROM query/* a comment */('SELECT 1')",
		"FROM query -- a comment\n('SELECT 1')",
		"SELECT * FROM json_execute_serialized_sql(json_serialize_sql('SELECT 1'))",
		// Profiling: stdout, or a file that wedges the connection.
		"PRAGMA enable_profiling",
		"PRAGMA enable_profiling = 'json'",
		"PRAGMA enable_profile",
		"CALL enable_profiling(format='json', save_location='" + filepath.Join(dir, "p.json") + "')",
		"PRAGMA disable_profiling",
		"PRAGMA all_profiling_output",
		// Output to stdout and engine behaviour that outlives the request.
		"PRAGMA enable_progress_bar",
		"PRAGMA enable_print_progress_bar",
		"PRAGMA enable_verification",
		"PRAGMA verify_external",
		"PRAGMA verify_parallelism",
		"PRAGMA verify_serializer",
		"PRAGMA verify_fetch_row",
		"PRAGMA disable_optimizer",
		"PRAGMA enable_object_cache",
		"PRAGMA disable_checkpoint_on_shutdown",
		"CALL enable_peg_parser()",
		"CALL check_peg_parser('SELECT 1')",
		"FROM arrow_scan(NULL, NULL, NULL)",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			for _, route := range []string{"/v1/query", "/v1/exec", "/v1/batch"} {
				body := map[string]interface{}{"sql": sql}
				if route == "/v1/batch" {
					body = map[string]interface{}{"statements": []interface{}{body}}
				}
				status, answer := h.post(route, body)
				if status != http.StatusForbidden || errorCode(answer) != "forbidden" {
					t.Fatalf("%s %q: got %d %v, want 403 forbidden", route, sql, status, answer)
				}
			}
		})
	}
	// The setting names are built with || because the guard refuses them even as string literals.
	_, answer := h.query("SELECT current_setting('enable_' || 'logging'), current_setting('logging_storage'), " +
		"current_setting('enable_' || 'profiling'), current_setting('profile_output'), " +
		"current_setting('enable_' || 'progress_bar')")
	if got := canonical(t, answer["rows"]); got != `[[0,"memory",null,"",false]]` {
		t.Fatalf("engine state after the attempts: %s", canonical(t, answer))
	}
	if got := strings.TrimSpace(h.logs.String()); strings.Contains(got, "QueryLog") || strings.Contains(got, "enable_logging") {
		t.Fatalf("the log carries DuckDB output or SQL: %s", got)
	}
}

// TestGuardAllowsOrdinarySQL: a column or alias named query, query_table, a string that only
// spells a refused name at run time, and everyday SQL all still run.
func TestGuardAllowsOrdinarySQL(t *testing.T) {
	h := newHarness(t, Config{})
	h.mustExec("CREATE TABLE searches (id INTEGER, query VARCHAR)")
	h.mustExec("INSERT INTO searches VALUES (1, 'duckdb')")
	for _, sql := range []string{
		"SELECT query FROM searches",
		"SELECT id AS query FROM searches",
		"SELECT count(query) FROM searches",
		"SELECT s.query, length(s.query) FROM searches s WHERE query = 'duckdb'",
		"FROM query_table('searches')",
		"SELECT 'enable' || '_logging' AS word",
		"PRAGMA table_info('searches')",
		"PRAGMA database_size",
		"PRAGMA force_checkpoint",
		"CHECKPOINT",
		"SELECT * FROM sql_auto_complete('SEL') LIMIT 1",
	} {
		if status, body := h.query(sql); status != http.StatusOK {
			t.Errorf("%q: %d %v", sql, status, body)
		}
	}
}

// allowedFunctions are the PRAGMA and table functions DuckDB v1.5.5 has that the guard lets
// through: they read catalog or engine information, read or write only the database itself, or are
// already refused by the lockdown (file and extension access). A function not here and not refused
// by guard.go fails TestGuardCoversEveryFunction, so a DuckDB upgrade is classified on purpose.
var allowedFunctions = map[string]bool{
	// PRAGMA functions.
	"add_parquet_key": true, "collations": true, "copy_database": true, "database_list": true,
	"database_size": true, "extension_versions": true, "force_checkpoint": true, "functions": true,
	"import_database": true, "metadata_info": true, "platform": true, "show": true,
	"show_databases": true, "show_tables": true, "show_tables_expanded": true, "storage_info": true,
	"table_info": true, "user_agent": true, "version": true,
	// Table functions and table macros.
	"checkpoint": true, "duckdb_approx_database_count": true, "duckdb_columns": true,
	"duckdb_connection_count": true, "duckdb_constraints": true, "duckdb_coordinate_systems": true,
	"duckdb_databases": true, "duckdb_dependencies": true, "duckdb_extensions": true,
	"duckdb_external_file_cache": true, "duckdb_functions": true, "duckdb_indexes": true,
	"duckdb_keywords": true, "duckdb_log_contexts": true, "duckdb_logs": true,
	"duckdb_logs_parsed": true, "duckdb_memory": true, "duckdb_optimizers": true,
	"duckdb_prepared_statements": true, "duckdb_profiling_settings": true, "duckdb_schemas": true,
	"duckdb_secret_types": true, "duckdb_secrets": true, "duckdb_sequences": true,
	"duckdb_settings": true, "duckdb_table_sample": true, "duckdb_tables": true,
	"duckdb_temporary_files": true, "duckdb_types": true, "duckdb_variables": true,
	"duckdb_views": true, "generate_series": true, "glob": true, "histogram": true,
	"histogram_values": true, "icu_calendar_names": true, "json_each": true, "json_tree": true,
	"parquet_bloom_probe": true, "parquet_file_metadata": true, "parquet_full_metadata": true,
	"parquet_kv_metadata": true, "parquet_metadata": true, "parquet_scan": true,
	"parquet_schema": true, "pg_timezone_names": true, "pragma_collations": true,
	"pragma_database_size": true, "pragma_metadata_info": true, "pragma_platform": true,
	"pragma_show": true, "pragma_storage_info": true, "pragma_table_info": true,
	"pragma_user_agent": true, "pragma_version": true, "query_table": true, "range": true,
	"read_blob": true, "read_csv": true, "read_csv_auto": true, "read_duckdb": true,
	"read_json": true, "read_json_auto": true, "read_json_objects": true,
	"read_json_objects_auto": true, "read_ndjson": true, "read_ndjson_auto": true,
	"read_ndjson_objects": true, "read_ndjson_objects_auto": true, "read_parquet": true,
	"read_text": true, "repeat": true, "repeat_row": true, "seq_scan": true, "sniff_csv": true,
	"sql_auto_complete": true, "summary": true, "test_all_types": true, "test_vector_types": true,
	"unnest": true, "which_secret": true,
}

// TestGuardCoversEveryFunction lists every PRAGMA and table function the engine has and requires
// each to be either refused by the guard or in allowedFunctions.
func TestGuardCoversEveryFunction(t *testing.T) {
	h := newHarness(t, Config{})
	status, body := h.query("SELECT DISTINCT function_name FROM duckdb_functions() " +
		"WHERE function_type IN ('pragma', 'table', 'table_macro') ORDER BY 1")
	if status != http.StatusOK {
		t.Fatalf("listing functions: %d %v", status, body)
	}
	var unclassified []string
	for _, row := range body["rows"].([]interface{}) {
		name := row.([]interface{})[0].(string)
		if !allowedFunctions[name] && !refusedNames[name] && !refusedCalls[name] {
			unclassified = append(unclassified, name)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("functions neither refused (guard.go) nor allowed (allowedFunctions): %v", unclassified)
	}
}

func TestGuardMatching(t *testing.T) {
	refused := []string{
		"call enable_logging()", "CALL ENABLE_LOGGING()", `"enable_logging"()`,
		"x.enable_logging()", "query('x')", "query\n\t('x')", "query /* c */ ('x')", "query--c\n('x')",
		"'enable_logging'", // a refused name in a string literal is refused too
	}
	for _, sql := range refused {
		if guard(sql) == nil {
			t.Errorf("not refused: %q", sql)
		}
	}
	allowed := []string{
		"select query from t", "select query, x from t", "select x as query from t",
		"select my_enable_logging_flag from t", "select enable_logging_x()", "query_table('t')",
		"select queryx('a')", "select subquery(1)",
	}
	for _, sql := range allowed {
		if err := guard(sql); err != nil {
			t.Errorf("refused %q: %s", sql, err.message)
		}
	}
}
