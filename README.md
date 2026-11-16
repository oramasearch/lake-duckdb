# lake-duckdb

**lake-duckdb, powered by DuckDB**: the web service behind lake's **DuckDB service kind**. One
DuckDB database behind a small HTTP API with a bearer token, the same API as
[lake-sqlite](https://github.com/oramasearch/lake-sqlite). A customer creates a DuckDB service on
lake's Services page; the Orama operator runs one `lake-duckdb` pod per service with its own volume,
and lake hands the customer its address and token.

```sh
make test            # go test ./... (cgo: a C/C++ toolchain is needed)
make run             # serves ./local.duckdb on :8080 with token local-dev-token
curl -s -H 'Authorization: Bearer local-dev-token' -d '{"sql":"SELECT 42 AS answer"}' localhost:8080/v1/query
```

| Route | Body | Answer |
|---|---|---|
| `POST /v1/query` | `{sql, params}` | `{columns, rows}` (at most 10,000 rows; `truncated` when cut) |
| `POST /v1/exec` | `{sql, params}` | `{rows_affected, last_insert_id}` (`last_insert_id` is always `null`: DuckDB has no rowid to return) |
| `POST /v1/batch` | `{statements: [{sql, params}]}` | `{results: [...]}`, all in one transaction |
| `GET /healthz` | — | 200 when the database answers `SELECT 1` (no token) |

The engine is **locked down** before it answers anything: no file or network access beyond its own
database (`enable_external_access=false`), no extension install or autoload, no community
extensions, memory and thread limits, then `lock_configuration=true` so no query can change any of
it; every statement is bounded by the request timeout. The whole API, configuration, lockdown,
limits and errors: [`docs/api.md`](docs/api.md).

| | |
|---|---|
| Language | Go 1.25, cgo |
| Driver | [`github.com/duckdb/duckdb-go/v2`](https://github.com/duckdb/duckdb-go) v2.10505.0 — **MIT**. It is the continuation of `github.com/marcboeker/go-duckdb` (moved to the DuckDB Foundation; the old path stopped at v2.4.3 with DuckDB 1.4.1) |
| Engine | DuckDB **v1.5.5**, statically linked from `github.com/duckdb/duckdb-go-bindings` v0.10505.0 — **MIT** (Stichting DuckDB Foundation) |
| Runtime | `gcr.io/distroless/cc-debian12:nonroot` (glibc, no shell), user `65532:65532` |

DuckDB is a trademark of the DuckDB Foundation. This service is described as "powered by DuckDB";
it is not, and is never called, "DuckDB Cloud" or an official DuckDB product.

The image is `oramasearch/lake-duckdb` on Docker Hub.
The operator side (StatefulSet, Service, credential) is orama-operator
`internal/controller/lake_service_duckdb.go`.

## License

MIT, see [LICENSE](LICENSE). Third-party components and their licenses are in
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES). Contributions are welcome under the
[Contributor License Agreement](CLA.md); see [CONTRIBUTING.md](CONTRIBUTING.md).
