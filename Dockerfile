# SPDX-License-Identifier: MIT

# lake-duckdb: one binary over one DuckDB database, powered by DuckDB. The driver,
# github.com/duckdb/duckdb-go/v2 (MIT), links DuckDB (MIT) statically through cgo, so the build
# needs a C/C++ toolchain and the runtime needs glibc: the builder is Debian bookworm and the runtime
# distroless/cc-debian12 (glibc, no shell), running as the numeric nonroot user 65532. libstdc++ and
# libgcc are linked statically, so the runtime depends only on glibc. Base images are digest-pinned.
#
#   docker buildx build --platform linux/amd64 -t oramasearch/lake-duckdb:<tag> .
FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS builder
# BuildKit sets these from --platform (default: the builder's own); cgo builds natively, so build
# each platform on a host of that architecture.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
# Module downloads first, so a source-only change reuses the layer.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags='-s -w -extldflags "-static-libstdc++ -static-libgcc"' \
      -o /out/lake-duckdb ./cmd/lake-duckdb
# An empty /data owned by the runtime user: a new Docker volume mounted there takes its owner, so
# `docker run -v <volume>:/data` works without root. (On Kubernetes the pod's fsGroup does this.)
RUN mkdir -p /out/data

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=builder /out/lake-duckdb /lake-duckdb
COPY --from=builder --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 8080
# The database lives on the mounted volume; the Orama operator's StatefulSet mounts it at /data.
# DuckDB spills to main.duckdb.tmp beside it.
ENV LAKE_DUCKDB_PATH=/data/main.duckdb
ENTRYPOINT ["/lake-duckdb"]
