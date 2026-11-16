# CLAUDE.md — lake-duckdb

- **What it is:** one Go binary (`cmd/lake-duckdb`) serving one DuckDB database over HTTP
  (`internal/server`), "powered by DuckDB" — never "DuckDB Cloud". The API contract is lake's
  `docs/control-plane/services.md` (the `duckdb` row: the same API as lake-sqlite); what is built is
  [`docs/api.md`](docs/api.md). Change both together.
- **Language and driver:** Go with `github.com/duckdb/duckdb-go/v2` (MIT; the successor of
  `github.com/marcboeker/go-duckdb/v2`), which links DuckDB (MIT) statically through cgo. CGO is
  **on**. The image is `FROM scratch` with one static binary: the `Dockerfile` builds DuckDB from
  source against musl and LLVM libc++ (no GPL or LGPL code; never add a base image or a GCC runtime)
  and links it with the `duckdb_use_static_lib` tag; a local `go build` uses the prebuilt libraries.
  The DuckDB version is the driver's (`v2.1MMPP.x` bundles DuckDB `1.M.P`); the Dockerfile's
  `DUCKDB_VERSION` moves with it.
- **The lockdown is the security boundary.** `internal/server/engine.go` `lockdown()` runs before
  the service answers, and `lock_configuration=true` must stay last. Every escape has a test in
  `internal/server/lockdown_test.go`; a new DuckDB version or a new setting means re-running them,
  and a new way out (a new file-reading function, a new extension mechanism) means a new case.
  Never add an extension, `allowed_directories`, or anything that re-opens file or network access.
- **Never log** SQL, parameters, bodies or the token.
- **Build and verify:** `go build ./... && go vet ./... && go test ./...`.
- **Releases:** the version is `internal/version.Version`; each has its notes in
  [`docs/releases/`](docs/releases/README.md) (`TestReleaseVersionIsDocumented`).
- **Images** are built for `linux/amd64` on an amd64 host (cgo cross-compiles poorly) and tagged
  with an immutable semantic version. A new image version is adopted by bumping the operator's
  default (`chart/values.yaml` `lakeServices.duckdbImage`, `cmd/main.go` `--lake-duckdb-image`).

## Rules

- **Branches:** branch from `dev`, and open every pull request against `dev`. `main` holds the last
  release. Pull requests are merged with merge commits, never squashed.
- **Documentation in the same change:** `README.md` and the pages under `docs/` change in the same
  commit as the code they describe.
- **No AI attribution** in commits, pull requests, tags or release notes: no `Co-Authored-By`
  trailers for an assistant, no "Generated with" lines.
- **Never write the word "seam"**; name the mechanism (interface, extension point, injected
  dependency, boundary).
- **No time or size estimates**; report what was done.
- Report vulnerabilities privately, as `SECURITY.md` describes, never in a public issue.
