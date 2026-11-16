# lake-duckdb

`lake-duckdb`, powered by DuckDB, is the small web service behind lake's **`duckdb` service kind**:
one DuckDB database behind an HTTP API with a bearer token. DuckDB has no upstream Kubernetes
operator we would use, so the Orama operator runs it itself, one StatefulSet per service
(orama-operator [`docs/lake/services.md`](https://github.com/oramasearch/orama-operator/blob/main/docs/lake/services.md)). The contract is the `duckdb` row of lake's
[`docs/control-plane/services.md`](https://github.com/oramasearch/lake/blob/aee5cda/docs/control-plane/services.md)
and the paragraph after its table ("`lake-duckdb` has the same API as `lake-sqlite`"); this page is
the whole API as built, and says where DuckDB makes it differ from lake-sqlite.

| | |
|---|---|
| Source | this repository: [`cmd/lake-duckdb/main.go`](../cmd/lake-duckdb/main.go) and the HTTP API in [`internal/server/`](../internal/server/) |
| Image | `oramasearch/lake-duckdb:<tag>` (Docker Hub), built from [`Dockerfile`](../Dockerfile) for `linux/amd64` |
| Driver | [`github.com/duckdb/duckdb-go/v2`](https://github.com/duckdb/duckdb-go) v2.10505.0, **MIT** — the DuckDB Foundation's continuation of `github.com/marcboeker/go-duckdb/v2` (which stopped at v2.4.3, DuckDB 1.4.1). cgo; DuckDB is linked statically |
| Engine | DuckDB v1.5.5, from `github.com/duckdb/duckdb-go-bindings` v0.10505.0, **MIT** |
| Runtime | `FROM scratch`, user `65532:65532`: one static binary (musl, LLVM libc++, DuckDB built from source), no shell, no libc package |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LAKE_DUCKDB_TOKEN` | — (required) | the bearer token; the operator mounts it from Secret `<name>-credentials`, key `token` |
| `LAKE_DUCKDB_PATH` | `/data/main.duckdb` | the database file; its directory must exist and be writable. DuckDB keeps its WAL (`main.duckdb.wal`) and spills large operations (`main.duckdb.tmp/`) beside it, on the same volume |
| `LAKE_DUCKDB_ADDR` | `:8080` | the listen address |
| `LAKE_DUCKDB_MEMORY_LIMIT` | unset | the container memory limit in bytes (the operator passes `limits.memory`). DuckDB's `memory_limit` is set to **half** of it, the Go heap to a quarter (`debug.SetMemoryLimit`), and the last quarter is headroom for what neither accounts for. Unset, DuckDB uses its own default (80% of the RAM it sees) |
| `LAKE_DUCKDB_THREADS` | unset | DuckDB's worker threads (the operator passes 2). Unset, DuckDB uses the CPU count it sees |

At startup the service opens the database, applies **the lockdown** below, checks from two
connections held open together that it holds (`enable_external_access` false and
`lock_configuration` true on both), runs `SELECT 1`, and exits non-zero if any of that fails or the
token is empty. It never serves an engine that is not locked.

## The lockdown

A customer's SQL is untrusted code. Before the service answers anything, it runs, in order:

| Setting | What it closes |
|---|---|
| `SET enable_external_access = false` | every file but the database's own — `read_csv`, `read_parquet`, `read_json`, `read_text`, `read_blob`, `glob`, a path used as a table, `COPY ... TO`, `EXPORT DATABASE`, `ATTACH` of any file, `CREATE SECRET` (persistent secrets) — and all network access |
| `SET autoinstall_known_extensions = false` | a query never downloads an extension |
| `SET autoload_known_extensions = false` | a query never loads one on its own; `INSTALL` and `LOAD` of anything not built in fail |
| `SET allow_community_extensions = false` | no community extension |
| `SET memory_limit = '<n>KiB'` (when configured) | DuckDB's buffer manager stays within its share of the container |
| `SET threads = <n>` (when configured) | CPU per query |
| `SET lock_configuration = true` | **last**: from here every `SET`, `RESET`, `SET GLOBAL` or `PRAGMA` of a configuration option fails (`Cannot change configuration option ... the configuration has been locked`) |

The settings are the database's, not one connection's, so every pooled connection has them. A
refused file, extension or network access answers **`403 forbidden`** with DuckDB's reason; an
attempt to change a locked setting answers `400 sql_error`. The statement timeout is the request
timeout (30 s): when it runs out the driver interrupts DuckDB (`duckdb_interrupt`) and the request
answers `504 timeout`.

[`internal/server/lockdown_test.go`](../internal/server/lockdown_test.go) tries every escape through
`/v1/query`, `/v1/exec` and `/v1/batch` and requires each to fail, then checks nothing was written
and the settings still hold: `read_csv('/etc/passwd')`, `read_csv_auto`, a file path as a table,
`read_text`, `read_blob`, `read_parquet`, `read_json`, `glob`, `COPY` of a query to a CSV and to a
Parquet path, `EXPORT DATABASE`, `ATTACH` of another DuckDB file and of a SQLite file, `INSTALL
httpfs`, `FORCE INSTALL ... FROM` a URL, `LOAD httpfs`, `LOAD` of a file, an `https://` URL as a
table, `CREATE SECRET`, and `SET`/`RESET`/`SET GLOBAL`/`PRAGMA` of `enable_external_access`, of
each extension setting, of `lock_configuration`, `memory_limit` and `threads`, and `RESET ALL`. It
also runs the check on many connections at once.

What stays allowed: everything inside the one database (tables, views, macros, sequences,
`CHECKPOINT`), `ATTACH ':memory:'` (an in-memory database, bounded by the memory limit and gone on
restart), session settings that are not configuration options (`SET search_path`), and the
extensions loaded in the binary (`core_functions`, `parquet`, `json`, `icu`, `autocomplete`), which
read nothing from outside while external access is off. The image's DuckDB is built without
`tpch` and `tpcds`; the prebuilt library a local `go build` links carries them unloaded. Either way
`LOAD tpch`, `LOAD tpcds`, `CALL dbgen(...)` and `CALL dsdgen(...)` fail (tested).

### Statements the lockdown cannot lock

`lock_configuration` stops configuration options, but DuckDB v1.5 also changes state through
functions and pragmas that are not options, and it does not stop those
([duckdb#28110](https://github.com/duckdb/duckdb/issues/28110), fixed after v1.5):
`CALL enable_logging(storage := 'stdout')` would write every statement's SQL to the pod's log, and
`storage := 'file'` would make every later statement fail and the process abort when it closes.
Profiling, progress bars, verification modes, optimizer and object-cache switches,
checkpoint-on-shutdown and the PEG parser switches change state the same way and stay on the pooled
connection after the request.

So, before any statement reaches DuckDB, the service refuses with **`403 forbidden`** every
statement that names one of these, quoted or not, in any case, schema-qualified or inside a macro,
view or `PREPARE` ([`internal/server/guard.go`](../internal/server/guard.go) has the list):
`enable_logging`, `disable_logging`, `truncate_duckdb_logs`, `write_log`, `enable_profiling`,
`disable_profiling`, `enable_profile`, `disable_profile`, `all_profiling_output`,
`enable_progress_bar`, `disable_progress_bar`, `enable_print_progress_bar`,
`disable_print_progress_bar`, `enable_verification`, `disable_verification`, `verify_external`,
`verify_fetch_row`, `verify_parallelism`, `verify_serializer` (and their `disable_` forms),
`enable_optimizer`, `disable_optimizer`, `enable_object_cache`, `disable_object_cache`,
`enable_checkpoint_on_shutdown`, `disable_checkpoint_on_shutdown`, `enable_peg_parser`,
`disable_peg_parser`, `check_peg_parser`, `json_execute_serialized_sql`, `arrow_scan` and
`arrow_scan_dumb`. A **call of `query(...)`** is refused too, because it runs a SQL string that can be
built from pieces (`'FROM enable_' || 'logging()'`); a column or alias named `query`, and
`query_table`, are fine. The names are matched as words anywhere in the text, so one inside a string
literal is refused as well (`current_setting('enable_logging')`; write `'enable_' || 'logging'`).

[`internal/server/guard_test.go`](../internal/server/guard_test.go) sends each of them, in each of
those shapes, through every route and checks that logging and profiling are still off afterwards. It
also lists every PRAGMA and table function the engine has and fails on any that is neither refused
nor on its reviewed allow list, so a DuckDB upgrade that adds one is classified on purpose.

As a second line, the service points file descriptor 1 at `/dev/null` at startup and writes its own
request log to a duplicate of the original stdout ([`cmd/lake-duckdb/stdout.go`](../cmd/lake-duckdb/stdout.go)):
whatever DuckDB prints to stdout never reaches the pod's log. Standard error is left as it is, so the
Go runtime can still report a crash; the only DuckDB output found there (the profiler's query trees)
comes from the profiling pragmas, which the guard refuses.

### One statement, and the statement types the service never runs

**Each `sql` holds exactly one statement** (a trailing `;` or comment is fine). The service counts
the statements with DuckDB's own parser before anything runs
([`internal/server/split.go`](../internal/server/split.go), on a separate in-memory database where
nothing is executed) and refuses more than one with `400 invalid_request`. The driver would run
every statement but the last while preparing, past every check made on the prepared statement: a
hidden `COMMIT; SELECT 1` used to commit a batch half-way. Send several statements through
`/v1/batch`.

The one statement is then prepared (which binds it; it does not run) and refused with
**`403 forbidden`** when its type is `COPY` (to or from anywhere), `EXPORT DATABASE` or `DETACH`.
`enable_external_access` leaves DuckDB its own files (the database, its WAL and the spill
directory), so `COPY ... TO 'main.duckdb'` could overwrite the database with a CSV, after which the
pod never starts again; `DETACH` of the main database would leave every later request without its
tables while `/healthz` still answers.

Every request also gets **a fresh connection**, discarded when the request ends, so nothing a client
sets on a connection outlives its request: `USE`, `SET search_path`, temporary tables, prepared
statements and variables are gone at the next request. What is the database's, not one
connection's, does last: tables and other objects in the database, and an attached `:memory:`
database until the pod restarts.

## API

Every route but `GET /healthz` needs **`Authorization: Bearer <token>`**. The presented and the
configured token are both hashed with SHA-256 and compared in constant time; a missing, malformed
or wrong token is `401` (with `WWW-Authenticate: Bearer`), before the route or method is looked at.

A statement is `{"sql": "...", "params": [...]}`. `params` is positional (`?` or `$1`) and may be
omitted. Each parameter binds as:

| JSON | Binds as |
|---|---|
| integer literal (`42`, `-7`) | `BIGINT` (a literal that does not fit in 64 bits is refused) |
| number with a fraction or exponent (`2.5`, `1e3`) | `DOUBLE` |
| string | `VARCHAR` (DuckDB casts it where the statement needs another type: a date, a UUID, ...) |
| `true` / `false` | `BOOLEAN` (lake-sqlite binds 1 / 0; DuckDB has a boolean) |
| `null` | `NULL` |
| `{"$base64": "<standard base64>"}` | `BLOB` |

Any other array or object is refused with `400 invalid_params`. Request bodies are strict JSON:
unknown fields and trailing data are refused.

### Values in answers

| DuckDB type | JSON |
|---|---|
| `BOOLEAN` | `true` / `false` |
| `TINYINT` … `BIGINT`, `UTINYINT` … `UBIGINT` | integer |
| `HUGEINT`, `UHUGEINT` | integer, exact (a JavaScript client loses precision above 2^53, as for `BIGINT`) |
| `FLOAT`, `DOUBLE` | a number that always has a fraction or exponent (`2.0`, never `2`); `FLOAT` is written at its own precision (`0.1`, not `0.10000000149`); infinities and NaN are the strings `"Infinity"`, `"-Infinity"`, `"NaN"` |
| `DECIMAL(p,s)` | **string**, exact (`"12345.67"`) |
| `VARCHAR`, `ENUM` | string |
| `BLOB` | `{"$base64": "..."}` |
| `UUID` | string, `8-4-4-4-12` hex |
| `DATE` | `"2026-09-26"` |
| `TIME` | `"10:11:12[.ffffff]"`; `TIMETZ` adds the offset |
| `TIMESTAMP`, `TIMESTAMP_S/_MS/_NS` | `"2026-09-26 10:11:12[.fraction]"` |
| `TIMESTAMPTZ` | RFC 3339 in UTC, `"2026-09-26T10:11:12Z"` |
| `INTERVAL` | `{"months": m, "days": d, "micros": µs}` |
| `LIST`, `ARRAY` | array (elements mapped by their type) |
| `STRUCT` | object |
| `MAP` | array of `{"key": k, "value": v}` (keys need not be strings, and order is kept) |
| `UNION` | `{"tag": "...", "value": v}` |
| `NULL` | `null` |

Inside a `STRUCT`, `MAP` or `UNION` the driver does not name member types, so a `UUID` member comes
back as `{"$base64": ...}` (its 16 bytes), a time as RFC 3339, and a `DECIMAL` still as a string.

### `POST /v1/query`

`{"sql", "params"}` answers `200` `{"columns": [...], "rows": [[...]], "truncated": true}`. At most
**10,000 rows** and **32 MiB of values** (strings counted as JSON writes them, escapes included,
blobs as their base64, plus a margin for every other value); when there were more, the answer stops there and carries `"truncated": true` (the key is
absent otherwise). A single value larger than 32 MiB answers no rows and `"truncated": true`. A statement that returns no rows still answers `columns` and an empty `rows`.
DuckDB answers a statement without data with one column: an `INSERT`, `UPDATE` or `DELETE` sent to
`/v1/query` answers `{"columns": ["Count"], "rows": [[n]]}`.

### `POST /v1/exec`

`{"sql", "params"}` answers `200` `{"rows_affected": 2, "last_insert_id": null}`. `rows_affected` is
DuckDB's changed-row count (0 for DDL). **`last_insert_id` is always `null`**: DuckDB has no rowid of
the last insert to report (lake-sqlite answers SQLite's `last_insert_rowid()`). Use `INSERT ...
RETURNING` through `/v1/query` or `/v1/batch` to read generated values.

### `POST /v1/batch`

`{"statements": [{"sql", "params"}, ...]}` runs every statement **in order, in one transaction**,
and answers `200` `{"results": [...]}`, one result per statement: a statement that returns data (a
`SELECT`, `EXPLAIN`, `PRAGMA`, `CALL`, or `INSERT/UPDATE/DELETE ... RETURNING`) answers like
`/v1/query` (with the same 10,000-row cap; the 32 MiB cap is shared by every statement in the batch), any other — DuckDB's single `Count` or `Success` column —
like `/v1/exec`. If any statement fails, the whole transaction is rolled back and the answer is that
statement's error, its message prefixed with `statements[<i>]: `. Parameters are checked before the
transaction begins, so a bad parameter runs nothing. An empty `statements` is `400
invalid_request`, and so is a `BEGIN`, `COMMIT` or `ROLLBACK` inside a batch (the batch is already
one transaction).

### Transactions

**No transaction outlives its request.** After every request the connection is rolled back and
closed, so a `BEGIN` sent alone to `/v1/exec` is undone when that request ends. Use `/v1/batch` for
statements that must commit together.

### `GET /healthz`

No token. `200` `{"status": "ok"}` only when the database answers `SELECT 1` within two seconds;
otherwise `503` `{"error": {"code": "unavailable", ...}}`. `HEAD` is accepted too. API requests use at
most four connections at a time (more wait for a slot within their own time limit); a fifth is kept
for `/healthz`, so a service busy with long queries still answers its probes. The operator's
startup, readiness and liveness probes all call it.

### Errors

Every error is `{"error": {"code": "...", "message": "..."}}`:

| Status | `code` | When |
|---|---|---|
| 400 | `sql_error` | DuckDB refused the statement (syntax, missing table, constraint, a locked setting, ...); the message is DuckDB's |
| 400 | `invalid_request` | the body is not the expected JSON, `sql` is empty or only a comment, `sql` holds more than one statement, `statements` is empty, or a batch holds a transaction statement |
| 400 | `invalid_params` | a parameter cannot be bound (see the table above) |
| 401 | `unauthorized` | no or a wrong bearer token |
| 403 | `forbidden` | the statement reached for a file, the network or an extension (the lockdown), names a function the lockdown cannot lock, or is a `COPY`, `EXPORT DATABASE` or `DETACH` (above) |
| 404 | `not_found` | an unknown route (with a valid token) |
| 405 | `method_not_allowed` | a route called with the wrong method |
| 409 | `conflict` | a transaction conflict with a concurrent writer; retry |
| 413 | `body_too_large` | a body over **16 MiB** |
| 500 | `io_error` / `internal` | DuckDB could not read or write its file, or failed internally (after a fatal error `/healthz` fails and the pod restarts) |
| 503 | `out_of_memory` | the statement needed more than DuckDB's memory limit (and could not spill) |
| 503 | `unavailable` | `/healthz`: the database does not answer |
| 504 | `timeout` | the request ran past its **30-second** limit; the statement is interrupted |
| 507 | `disk_full` | the volume is full |

### Limits and lifecycle

- Every request is bounded to 30 s, the SQL included. The server reads headers within 10 s and closes
  idle connections after 2 minutes.
- On `SIGTERM` (or `SIGINT`) the server stops accepting connections, lets in-flight requests finish
  for up to 30 s, then closes the database, which checkpoints the WAL into `main.duckdb`. The pod's
  `terminationGracePeriodSeconds` is 45.
- One JSON line per request on stdout: `method`, `path`, `status`, `bytes`, `request_bytes`,
  `duration_ms`. The SQL, its parameters, the body, headers and the token are **never** logged, and
  DuckDB's own output never reaches the log (descriptor 1 is `/dev/null`; the log is written to a
  duplicate of the original stdout).

## Build

The image is built from the repository root for `linux/amd64`, on an amd64 builder: the build uses
cgo, so build it natively, not under emulation. It is **`FROM scratch`** and holds no GPL or LGPL
code: the [`Dockerfile`](../Dockerfile) builds DuckDB v1.5.5 from its source release (checked
against its SHA-256) with clang on Alpine, against musl and LLVM's libc++, libc++abi, libunwind and
compiler-rt, then links the service fully statically through duckdb-go-bindings'
`duckdb_use_static_lib` tag. The image holds the binary, `/etc/passwd` and `/etc/group` for user
`65532`, and an empty `/data` owned by it. The binary is linked with an 8 MiB default thread
stack (`-z stack-size`): musl's own default is 128 KiB, which a query of about 100 nested
parentheses overflowed, killing the process. The full test suite runs inside the build, against that
static DuckDB, so a build that does not pass it produces no image. The build stage is
`golang:1.27.4-alpine`, pinned by digest. `go.mod` keeps `go 1.25.0` as the oldest Go that builds
the source; release images are built with a Go release that still gets security fixes.

A local `go build` (no tag) links the bindings' prebuilt DuckDB libraries instead, which is fast and
needs only a C/C++ toolchain; the image's build is the one that ships.

```sh
docker buildx build --platform linux/amd64 -t oramasearch/lake-duckdb:<tag> .
# or: make docker-build IMG=oramasearch/lake-duckdb:<tag>
```

The operator chart's default (`lakeServices.duckdbImage`) names a released tag, for example
`oramasearch/lake-duckdb:0.2.0`, so that tag must be published before a DuckDB service can start; a Lake overrides it with `spec.services.duckdbImage`. Changing the image rolls every DuckDB
service's pod once.

## Run locally

```sh
go test ./...
mkdir -p /tmp/lake-duckdb
LAKE_DUCKDB_TOKEN=dev LAKE_DUCKDB_PATH=/tmp/lake-duckdb/main.duckdb LAKE_DUCKDB_ADDR=127.0.0.1:8080 \
  go run ./cmd/lake-duckdb

curl -s localhost:8080/healthz
curl -s -H 'Authorization: Bearer dev' -d '{"sql":"CREATE TABLE t (id INTEGER, v VARCHAR)"}' localhost:8080/v1/exec
curl -s -H 'Authorization: Bearer dev' \
  -d '{"statements":[{"sql":"INSERT INTO t VALUES (?, ?)","params":[1,"a"]},{"sql":"SELECT v, count(*) FROM t GROUP BY v"}]}' \
  localhost:8080/v1/batch
```

The tests (`internal/server/*_test.go`, table-driven over an `httptest` server and a temporary
directory) cover the token (missing, wrong, prefix, suffix, other schemes, and `/healthz` without
one), every value type, request errors and their codes, the 16 MiB cap, exec results, a primary-key
violation, batch commit, rollback on a SQL error and on a bad parameter, `RETURNING` in a batch,
that no transaction outlives its request, the 10,000-row cap on `/v1/query` and in a batch, the
32 MiB response cap (counting JSON escapes, and shared by a batch), one statement per `sql`, the
refused statement types, that no connection state outlives its request, the
statement timeout (and that the service answers afterwards), `/healthz` on an open and a closed
database, refusing to start without a token or a writable path, data surviving a reopen, the memory
and thread limits, the lockdown and the statement guard (above), and that the request log never
carries the token, the SQL or a parameter.

## Known limits

- **The statement guard is a list of names.** It holds for DuckDB v1.5.5, whose every PRAGMA and
  table function the tests classify, until a DuckDB with the upstream fix
  ([duckdb#28110](https://github.com/duckdb/duckdb/issues/28110)) replaces it. It also refuses a
  refused name inside a string literal, and `query(...)` calls.
- **The response cap counts values after DuckDB has produced them.** One huge value (DuckDB allows
  strings up to 4 GB) is held in memory before the cap drops it; DuckDB's `memory_limit` and the Go
  heap limit bound that.

- **One copy.** One pod on one `ReadWriteOnce` volume, no replication and no backup yet: a lost
  volume loses the database, and the service is down while its pod restarts or moves. DuckDB has
  one writer process, so `instances` is always 1.
- **No bring-your-own files.** With external access off, a customer cannot `read_parquet` or
  `COPY FROM` a file or URL; data arrives through `INSERT` (parameters, or `VALUES`). Bulk loading
  from object storage would need a deliberate, allow-listed design (`allowed_directories`), not a
  relaxed lockdown.
- **One statement per `sql`**: a `sql` holding several is refused (above); lake-sqlite runs them
  all. Use `/v1/batch`.
- **No `COPY`, `EXPORT DATABASE` or `DETACH`**, and no `query(...)` calls (above).
- **Plain HTTP, and DuckDB's messages as they are.** The service does not terminate TLS: the bearer
  token crosses the pod network in clear unless a mesh or the cluster's network policy protects it
  (the operator's concern). Error messages are DuckDB's, so they can name paths inside the pod (the
  database file, its spill directory, the home directory); only a client holding the token sees
  them.
- The memory guard is DuckDB's `memory_limit` plus the Go heap limit; a query that needs more and
  cannot spill answers `503 out_of_memory` rather than the pod being killed, but DuckDB allocations
  outside its buffer manager are not counted, which is what the unassigned quarter of the container
  limit is for.
