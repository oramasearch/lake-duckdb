# Contributing to lake-duckdb

lake-duckdb, powered by DuckDB, is one Go binary (`cmd/lake-duckdb`) serving one DuckDB database
over HTTP (`internal/server`). Call it "powered by DuckDB", never "DuckDB Cloud". Its API contract
is the `duckdb` row of lake's `docs/control-plane/services.md` (the same API as lake-sqlite); what
is built is [`docs/api.md`](docs/api.md). Change both together.

## Build and test

```sh
go build ./... && go vet ./... && go test ./...
```

cgo is on, so a C/C++ toolchain is needed.

## Rules for changes

- **Language and driver:** Go with `github.com/duckdb/duckdb-go/v2` (MIT; the successor of
  `github.com/marcboeker/go-duckdb/v2`), which links DuckDB (MIT) statically through cgo. The
  runtime is `distroless/cc-debian12` (glibc) with libstdc++/libgcc linked statically. The DuckDB
  version is the driver's (`v2.1MMPP.x` bundles DuckDB `1.M.P`).
- **The lockdown is the security boundary.** `internal/server/engine.go` `lockdown()` runs before
  the service answers, and `lock_configuration=true` must stay last. Every escape has a test in
  `internal/server/lockdown_test.go`; a new DuckDB version or a new setting means re-running them,
  and a new way out (a new file-reading function, a new extension mechanism) means a new case.
  Never add an extension, `allowed_directories`, or anything that re-opens file or network access.
- **Never log** SQL, parameters, bodies or the token.
- Documentation changes in the same pull request as the code.
- Branch from `dev` and open the pull request against `dev`; `main` holds the last release.
- Images are built for `linux/amd64` on an amd64 builder (cgo cross-compiles poorly) and pushed with
  an immutable `<semver>` tag; the operator's default image (`lakeServices.duckdbImage` in its chart)
  moves to the new tag after the push.

## Security

Report a vulnerability privately, as [`SECURITY.md`](SECURITY.md) describes, never in a public issue.
