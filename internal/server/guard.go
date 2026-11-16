// SPDX-License-Identifier: MIT

package server

import (
	"fmt"
	"net/http"
	"strings"
)

// lock_configuration (the last lockdown setting) stops every SET, RESET and PRAGMA of a
// configuration option, but DuckDB v1.5 also changes state through functions and pragmas that are
// not configuration options, and those it does not stop:
//
//   - enable_logging() switches logging on for the whole database. With storage 'stdout' every
//     statement's SQL is written to the pod's log; with storage 'file' every later statement fails
//     until the process restarts. Upstream: https://github.com/duckdb/duckdb/issues/28110, fixed
//     after v1.5.
//   - enable_profiling(), PRAGMA enable_profiling and friends print query trees, or point the
//     connection at a file so that every later statement on it fails.
//   - PRAGMA enable_progress_bar / enable_print_progress_bar print to stdout; enable_verification,
//     verify_*, disable_optimizer, enable_object_cache, *_checkpoint_on_shutdown and the PEG parser
//     switches change how the engine runs, and stay on the pooled connection after the request.
//
// So the service refuses any statement that names one of them. A name is refused wherever it
// appears (a call, a macro or view body, a PREPARE), quoted or not, in any case; a refused name
// inside a string literal is refused too, which costs nothing real.
//
// query() runs a SQL string, and the string can be built from pieces ('FROM enable_' ||
// 'logging()'), so no name check can see through it: a call of query() is refused as well. A
// column or alias named query is still fine. json_execute_serialized_sql runs a serialized plan
// built the same way, and is refused by name.
//
// guard_test.go enumerates every PRAGMA and table function the engine has, so a DuckDB upgrade that
// adds one fails the tests until it is classified here.
var refusedNames = map[string]bool{
	// Logging.
	"enable_logging":       true,
	"disable_logging":      true,
	"truncate_duckdb_logs": true,
	"write_log":            true,
	// Profiling.
	"enable_profiling":     true,
	"disable_profiling":    true,
	"enable_profile":       true,
	"disable_profile":      true,
	"all_profiling_output": true,
	// Output to stdout.
	"enable_progress_bar":        true,
	"disable_progress_bar":       true,
	"enable_print_progress_bar":  true,
	"disable_print_progress_bar": true,
	// Engine behaviour that would outlive the request.
	"enable_verification":            true,
	"disable_verification":           true,
	"verify_external":                true,
	"disable_verify_external":        true,
	"verify_fetch_row":               true,
	"disable_verify_fetch_row":       true,
	"verify_parallelism":             true,
	"disable_verify_parallelism":     true,
	"verify_serializer":              true,
	"disable_verify_serializer":      true,
	"enable_optimizer":               true,
	"disable_optimizer":              true,
	"enable_object_cache":            true,
	"disable_object_cache":           true,
	"enable_checkpoint_on_shutdown":  true,
	"disable_checkpoint_on_shutdown": true,
	"enable_peg_parser":              true,
	"disable_peg_parser":             true,
	"check_peg_parser":               true,
	// Running SQL or plans built at run time, and raw memory pointers.
	"json_execute_serialized_sql": true,
	"arrow_scan":                  true,
	"arrow_scan_dumb":             true,
}

// refusedCalls are names refused only when called: a column or alias may carry them.
var refusedCalls = map[string]bool{
	"query": true,
}

// guard returns a 403 when sql names something the lockdown cannot lock (refusedNames, or a call
// of a refusedCalls function), and nil otherwise.
func guard(sql string) *apiError {
	// Double quotes only delimit identifiers: "Enable_Logging" is enable_logging.
	text := strings.ToLower(strings.ReplaceAll(sql, `"`, ""))
	for i := 0; i < len(text); {
		if !isWordByte(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && isWordByte(text[i]) {
			i++
		}
		word := text[start:i]
		if refusedNames[word] || (refusedCalls[word] && nextIsOpenParen(text[i:])) {
			return &apiError{http.StatusForbidden, "forbidden", fmt.Sprintf(
				"%s is not allowed: it changes engine state that the lockdown cannot lock", word)}
		}
	}
	return nil
}

func isWordByte(b byte) bool {
	return b == '_' || b == '$' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b >= 0x80
}

// nextIsOpenParen: after whitespace, the next character is "(" or starts a comment. A comment is
// treated as if a "(" may follow it rather than parsed (comments nest), so "query /* x */ (" is
// refused, and so is a column named query followed by a comment, which is rare and harmless to
// refuse.
func nextIsOpenParen(rest string) bool {
	rest = strings.TrimLeft(rest, " \t\n\r\f\v")
	return strings.HasPrefix(rest, "(") || strings.HasPrefix(rest, "/*") || strings.HasPrefix(rest, "--")
}
