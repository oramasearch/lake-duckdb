# lake-duckdb

**One DuckDB database behind a small, locked-down HTTP API: lake's DuckDB service, powered by DuckDB.**

lake-duckdb is one Go binary that serves one DuckDB database file over HTTP, behind a bearer token.
It is what runs when someone creates a DuckDB service in lake: the operator starts one lake-duckdb
pod per service, with its own volume, and lake hands out its address and token. The client's SQL is
treated as untrusted code: the engine is locked to its own database file before the service answers
anything, so a query can't read the host's files, reach the network or load an extension.

Part of [Orama Lake](https://github.com/oramasearch/lake-platform), the sovereign AI stack.
MIT-licensed.

## What it does

- **SQL over HTTP**: `POST /v1/query` (rows, at most 10,000 and 32 MiB per answer, marked
  `truncated` when cut), `POST /v1/exec` (rows affected) and `POST /v1/batch` (several statements
  in one transaction, rolled back on the first error). [API](docs/api.md#api)
- **The same API as [lake-sqlite](https://github.com/oramasearch/lake-sqlite)**, so a client works
  against either; `last_insert_id` is always `null`, because DuckDB has no rowid to return.
  [Differences](docs/api.md#post-v1exec)
- **Typed values**: every DuckDB type maps to JSON, including `DECIMAL` as an exact string, `BLOB`
  as `{"$base64": ...}`, `LIST`, `STRUCT`, `MAP` and `UNION`. [Values](docs/api.md#values-in-answers)
- **The lockdown**: no file or network access beyond the database's own file, no extension install
  or load, memory and thread limits, then the configuration locked. Statements that change engine
  state past that lock (logging, profiling and similar) are refused before they reach DuckDB.
  [Lockdown](docs/api.md#the-lockdown)
- **Bounded requests**: a bearer token compared in constant time, a 16 MiB body cap, a 30-second
  limit on every request (the SQL is interrupted), and a separate connection kept for
  `GET /healthz`. [Limits](docs/api.md#limits-and-lifecycle)
- **A log with no data in it**: one JSON line per request, never the SQL, parameters, body or token.

## Quickstart

From a clean machine with Go (the version in `go.mod` or later) and a C/C++ toolchain (DuckDB is
linked in through cgo):

```sh
git clone https://github.com/oramasearch/lake-duckdb.git
cd lake-duckdb
make run   # serves ./local.duckdb on :8080 with the throwaway token local-dev-token
```

In another terminal:

```sh
curl -s -H 'Authorization: Bearer local-dev-token' -d '{"sql":"SELECT 42 AS answer"}' localhost:8080/v1/query
```

You should see `{"columns":["answer"],"rows":[[42]]}`. `local-dev-token` is an example; give a real
service a long random token in `LAKE_DUCKDB_TOKEN`.

With Docker, the image is `oramasearch/lake-duckdb:<version>` (the versions are on the
[releases page](https://github.com/oramasearch/lake-duckdb/releases)):

```sh
docker run --rm -p 8080:8080 -e LAKE_DUCKDB_TOKEN=local-dev-token -v lake-duckdb:/data oramasearch/lake-duckdb:<version>
```

To try the whole stack at once, use the [Orama Lake quickstart](https://github.com/oramasearch/lake-platform).

### Build from source

Go (the version in `go.mod` or later), a C/C++ toolchain, and `make`. The prebuilt DuckDB libraries
cover linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 and windows/amd64.

```sh
go build ./... && go vet ./... && go test ./...
docker buildx build --platform linux/amd64 -t lake-duckdb:local .   # the image, on an amd64 host
```

## How it fits the stack

lake creates the service and gives its users the address and token; the operator runs it as a
one-pod StatefulSet with a volume and a Secret holding the token, and probes `/healthz`. lake-duckdb
talks to nothing else: it has no outbound network access at all.

| Piece | What it does | Relation to this repo |
|---|---|---|
| [lake](https://github.com/oramasearch/lake) | The AI workspace | creates DuckDB services and is their main client |
| [orama-operator](https://github.com/oramasearch/orama-operator) | Runs the stack on Kubernetes | runs one lake-duckdb pod per service (`internal/controller/lake_service_duckdb.go`) |
| [lake-sqlite](https://github.com/oramasearch/lake-sqlite) | The SQLite service | the same API, over SQLite |
| [zetta](https://github.com/oramasearch/zetta) | The engine | none directly |
| [liquid](https://github.com/oramasearch/liquid) | Version control, builds, packages, deploys | none directly |

## Known limits

- **One copy**: one pod on one volume, no replication and no backup yet. A lost volume loses the
  database, and the service is down while its pod restarts.
- **No bring-your-own files**: with external access off, data arrives through `INSERT`, not
  `read_parquet` or `COPY FROM` a file or URL.
- **One statement per `sql`**: several are refused; send them through `/v1/batch`. `COPY`,
  `EXPORT DATABASE` and `DETACH` are refused too.
- **The statement guard is a list of names** for DuckDB v1.5.5 (the tests classify every PRAGMA and
  table function), until a DuckDB with the upstream fix replaces it. It also refuses `query(...)`
  calls and a refused name inside a string literal.

Every limit, with the reason: [`docs/api.md` → Known limits](docs/api.md#known-limits).

## Documentation

- [`docs/api.md`](docs/api.md): the whole API, configuration, the lockdown, limits, errors, build
  and local run.
- [`docs/releases/`](docs/releases/README.md): the release notes. The latest release is
  [v0.2.0](docs/releases/v0.2.0.md).

## Community

- Questions and bugs: [issues](https://github.com/oramasearch/lake-duckdb/issues).
- Security reports: privately, per [SECURITY.md](SECURITY.md). Please don't open a public issue.

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test and open a
pull request (against `dev`). Every contributor signs the
[Contributor License Agreement](CLA.md) once, before their first pull request is merged; a bot
links it on that pull request.

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

## License

MIT. See [LICENSE](LICENSE). Third-party components keep their own licenses, listed in
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES).

"Orama Lake", "liquid", "lake", "zetta" and their logos are trademarks of OramaSearch, Inc. The
[trademark policy](https://github.com/oramasearch/lake-platform/blob/main/TRADEMARKS.md) says how you
can use them, including in a fork. DuckDB is a trademark of the DuckDB Foundation: this service is
"powered by DuckDB", and is not, and is never called, "DuckDB Cloud" or an official DuckDB product.
