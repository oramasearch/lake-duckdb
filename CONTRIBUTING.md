# Contributing to lake-duckdb

Thank you for helping. This document says how to build and test lake-duckdb, how a change gets in,
and the one thing every contributor does first: sign the Contributor License
Agreement.

## Before you start

- For a bug, open an issue with what you did, what you expected and what happened, and the
  version or commit.
- For a larger change (a new feature, a new dependency, a change to an interface), open an issue
  first so the approach can be agreed before you write it.
- **Security issues are never reported in a public issue.** See [SECURITY.md](SECURITY.md): write
  to **security@orama.com**.

## Build and test

You need Go (the version in `go.mod`) and a C/C++ toolchain, because the DuckDB driver uses cgo
and links DuckDB's prebuilt static library.

```sh
make test              # go test ./...
go build ./...
docker build -t lake-duckdb:local .
```

## Making a change

1. Fork the repository and branch from `dev`. `main` is the last release; it moves only when a
   release is cut.
2. Keep one change per pull request, with the tests that prove it. A change in behavior updates
   the documentation it touches in the same pull request.
3. Write commit messages in the [Conventional Commits](https://www.conventionalcommits.org/) form,
   for example `fix(api): reject an empty name`.
4. Sign the Contributor License Agreement before your first pull request is merged (next section).
5. Open the pull request against `dev`. Start its description with what changes, why, and how.

A maintainer reviews it. Pull requests are merged with a merge commit, so every commit you make
stays in the history.

## Rules for changes

lake-duckdb, powered by DuckDB, is one Go binary (`cmd/lake-duckdb`) serving one DuckDB database
over HTTP (`internal/server`). Call it "powered by DuckDB", never "DuckDB Cloud". Its API contract
is the `duckdb` row of lake's `docs/control-plane/services.md` (the same API as lake-sqlite); what
is built is [`docs/api.md`](docs/api.md). Change both together.

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
- Images are built for `linux/amd64` on an amd64 builder (cgo cross-compiles poorly) and pushed with
  an immutable `<semver>` tag; the operator's default image (`lakeServices.duckdbImage` in its chart)
  moves to the new tag after the push.

## Contributor License Agreement

lake-duckdb is owned by OramaSearch, Inc. Every contributor signs the
[Contributor License Agreement](CLA.md) (CLA) once, before their first pull request is merged. Under
it you assign the copyright in your contributions to OramaSearch, Inc., you keep a licence to use
your own contributions for any purpose, and you grant a patent licence for them. The project is
published under the MIT License (see [LICENSE](LICENSE)).

When you open your first pull request, a bot comments on it with a link to the agreement. Sign
there with your GitHub account; it covers all your later contributions to projects of OramaSearch,
Inc. If you contribute as part of your job, your employer signs too: write to
**opensource@orama.com** for the entity agreement.

## License headers

Every source file starts with the line `SPDX-License-Identifier: MIT`, in that file's comment
syntax, for example `// SPDX-License-Identifier: MIT`. Add it to a new file yourself, or run:

```sh
python3 tools/spdx-headers.py           # adds the line where it is missing
python3 tools/spdx-headers.py --check   # lists files without it
```

Code copied from another project keeps its own license header and is listed in
[THIRD_PARTY_LICENSES](THIRD_PARTY_LICENSES). Say so in the pull request, with where it came from
and its license; it must be compatible with the MIT License.

## Code of conduct

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md). Report unacceptable
behavior to **conduct@orama.com**.
