# SPDX-License-Identifier: MIT

IMG ?= oramasearch/lake-duckdb:dev

.PHONY: build test vet docker-build run

build: ## Build the binary for this machine (cgo: DuckDB is linked in).
	CGO_ENABLED=1 go build -trimpath -o lake-duckdb ./cmd/lake-duckdb

test: ## Unit, HTTP and lockdown tests (no network, temp directories only).
	go test ./...

vet:
	go vet ./...

docker-build: ## linux/amd64 image, on an amd64 builder (for example a BuildKit on amd64).
	docker buildx build --platform linux/amd64 -t $(IMG) .

run: build ## Serve ./local.duckdb on :8080 with a throwaway token.
	LAKE_DUCKDB_TOKEN=local-dev-token LAKE_DUCKDB_PATH=./local.duckdb ./lake-duckdb
