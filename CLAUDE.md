# CLAUDE.md — lake-duckdb

Org-wide conventions are in [`oramasearch/orama-bridge`](https://github.com/oramasearch/orama-bridge)
`CLAUDE.md` and apply in full: no AI attribution, merge commits never squash, commit means push,
private by default, documentation in the same change.

- **What it is:** one Go binary (`cmd/lake-duckdb`) serving one DuckDB database over HTTP
  (`internal/server`), "powered by DuckDB" — never "DuckDB Cloud". The API contract is lake
  `docs/control-plane/services.md` (the `duckdb` row: the same API as lake-sqlite); what is built is
  [`docs/api.md`](docs/api.md). Change both together.
- **Language and driver:** Go with `github.com/duckdb/duckdb-go/v2` (MIT; the successor of
  `github.com/marcboeker/go-duckdb/v2`), which links DuckDB (MIT) statically through cgo. CGO is
  **on**; the runtime is `distroless/cc-debian12` (glibc) with libstdc++/libgcc linked statically.
  The DuckDB version is the driver's (`v2.1MMPP.x` bundles DuckDB `1.M.P`).
- **The lockdown is the security boundary.** `internal/server/engine.go` `lockdown()` runs before
  the service answers, and `lock_configuration=true` must stay last. Every escape has a test in
  `internal/server/lockdown_test.go`; a new DuckDB version or a new setting means re-running them,
  and a new way out (a new file-reading function, a new extension mechanism) means a new case.
  Never add an extension, `allowed_directories`, or anything that re-opens file or network access.
- **Never log** SQL, parameters, bodies or the token.
- **Build and verify:** `go build ./... && go vet ./... && go test ./...`.
- **Release:** build `linux/amd64` on the cluster BuildKit (liquid-bridge `.claude/skills/liquid-dev`;
  cgo cross-compiles poorly, so build on amd64), create the Docker Hub repository **private** before
  the first push, push `oramasearch/lake-duckdb:<semver>` (an immutable tag), confirm the Docker Hub
  repository answers 404 anonymously, then bump the operator's default image
  (`chart/values.yaml` `lakeServices.duckdbImage`, `cmd/main.go` `--lake-duckdb-image`).
