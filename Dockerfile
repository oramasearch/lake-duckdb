# SPDX-License-Identifier: MIT

# lake-duckdb: one static binary over one DuckDB database, powered by DuckDB, on `scratch`.
#
# The image holds no GPL or LGPL code: no libc package, no libstdc++ or libgcc. DuckDB is built from
# source with clang against musl (MIT) and LLVM's libc++, libc++abi, libunwind and compiler-rt
# (Apache-2.0 WITH LLVM-exception), and the Go binary links it statically
# (github.com/duckdb/duckdb-go/v2 with the duckdb_use_static_lib tag). Only these extensions are
# built in, and all of them are loaded: core_functions, parquet, json, icu, autocomplete. tpch and
# tpcds are not built: their data generators are under the TPC's licence, not an open-source one.
#
# Base images are digest-pinned and the DuckDB source is checked against its SHA-256. cgo builds
# natively, so build each platform on a host of that architecture:
#
#   docker buildx build --platform linux/amd64 -t oramasearch/lake-duckdb:<tag> .
#
# The full test suite runs in the build, against this static DuckDB.

FROM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS toolchain
RUN apk add --no-cache clang22 lld22 llvm22 compiler-rt libc++-dev libc++-static llvm-libunwind-static \
      llvm-libunwind-dev musl-dev linux-headers cmake samurai make python3
ENV CC=clang CXX=clang++ PATH=/usr/lib/llvm22/bin:$PATH AR=llvm-ar RANLIB=llvm-ranlib \
    LLVM_RT="-fuse-ld=lld -rtlib=compiler-rt -unwindlib=libunwind"

# DuckDB, the version duckdb-go-bindings v0.10505.0 (go.mod) is written for.
FROM toolchain AS duckdb
ARG DUCKDB_VERSION=1.5.5
ARG DUCKDB_SHA256=f33155ff962e6e1e08fd1e9caffa487d4325aa60999e2eabc76feff534d6558b
WORKDIR /src
RUN wget -q -O duckdb.tar.gz https://github.com/duckdb/duckdb/archive/refs/tags/v${DUCKDB_VERSION}.tar.gz \
 && echo "${DUCKDB_SHA256}  duckdb.tar.gz" | sha256sum -c - \
 && tar xzf duckdb.tar.gz --strip-components=1 && rm duckdb.tar.gz
RUN cmake -S . -B build -G Ninja \
      -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_C_COMPILER=clang -DCMAKE_CXX_COMPILER=clang++ \
      -DCMAKE_AR=/usr/lib/llvm22/bin/llvm-ar -DCMAKE_RANLIB=/usr/lib/llvm22/bin/llvm-ranlib \
      -DCMAKE_CXX_FLAGS="-stdlib=libc++" \
      -DCMAKE_EXE_LINKER_FLAGS="${LLVM_RT} -stdlib=libc++ -lc++abi" \
      -DCMAKE_SHARED_LINKER_FLAGS="${LLVM_RT} -stdlib=libc++" \
      -DOVERRIDE_GIT_DESCRIBE=v${DUCKDB_VERSION}-0-g0000000000 \
      -DBUILD_EXTENSIONS="core_functions;parquet;json;icu;autocomplete" \
      -DBUILD_SHELL=FALSE -DBUILD_UNITTESTS=FALSE -DBUILD_BENCHMARKS=FALSE \
      -DENABLE_EXTENSION_AUTOLOADING=FALSE -DENABLE_EXTENSION_AUTOINSTALL=FALSE \
 && cmake --build build --target duckdb_static duckdb_generated_extension_loader \
      core_functions_extension parquet_extension json_extension icu_extension autocomplete_extension
RUN mkdir /out && cd build \
 && cp src/libduckdb_static.a third_party/*/libduckdb_*.a extension/libduckdb_generated_extension_loader.a \
       extension/*/lib*_extension.a /out/

# STATIC_LINK sets the default thread stack to 8 MiB (-z stack-size), which musl reads from the
# binary. Its own default is 128 KiB, and DuckDB's parser, binder and planner recurse once per level
# of nesting: a query of about 100 nested parentheses overflowed that and killed the process. Go's
# cgo calls run on threads made with that default. TestDeepQueriesDoNotOverflowTheStack covers it.
FROM toolchain AS build
WORKDIR /workspace
COPY --from=duckdb /out/ /duckdb/
# Module downloads first, so a source-only change reuses the layer.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY docs/releases/ docs/releases/
ENV CGO_ENABLED=1 \
    GOFLAGS=-tags=duckdb_use_static_lib \
    CGO_LDFLAGS="-L/duckdb -Wl,--start-group -lduckdb_static -lduckdb_generated_extension_loader -lautocomplete_extension -lcore_functions_extension -licu_extension -ljson_extension -lparquet_extension -lduckdb_fastpforlib -lduckdb_fmt -lduckdb_fsst -lduckdb_hyperloglog -lduckdb_jemalloc -lduckdb_mbedtls -lduckdb_miniz -lduckdb_pg_query -lduckdb_re2 -lduckdb_skiplistlib -lduckdb_utf8proc -lduckdb_yyjson -lduckdb_zstd -Wl,--end-group -lc++ -lc++abi -lunwind -lm" \
    STATIC_LINK='-linkmode external -extldflags "-static -fuse-ld=lld -rtlib=compiler-rt -unwindlib=libunwind -Wl,-z,stack-size=8388608"'
RUN go test -count=1 -ldflags="${STATIC_LINK}" ./...
RUN go build -trimpath -ldflags="-s -w ${STATIC_LINK}" -o /out/lake-duckdb ./cmd/lake-duckdb \
 && /out/lake-duckdb --version \
 && mkdir -p /out/data /out/etc \
 && echo 'nonroot:x:65532:65532:nonroot:/data:/sbin/nologin' > /out/etc/passwd \
 && echo 'nonroot:x:65532:' > /out/etc/group

# The binary, the runtime user, and an empty /data owned by it: a new Docker volume mounted there
# takes its owner, so `docker run -v <volume>:/data` works without root (on Kubernetes the pod's
# fsGroup does this). The operator's StatefulSet mounts the database volume at /data; DuckDB spills
# to main.duckdb.tmp beside the file.
FROM scratch
COPY --from=build /out/lake-duckdb /lake-duckdb
COPY --from=build /out/etc/ /etc/
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 8080
ENV LAKE_DUCKDB_PATH=/data/main.duckdb HOME=/data
ENTRYPOINT ["/lake-duckdb"]
