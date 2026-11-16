// SPDX-License-Identifier: MIT

// lake-duckdb serves one DuckDB database over HTTP for lake's duckdb service kind, powered by
// DuckDB. The Orama operator runs it as a one-pod StatefulSet per service (orama-operator
// docs/lake/lake-duckdb.md).
//
// Environment:
//
//	LAKE_DUCKDB_TOKEN         the bearer token (required; mounted from the service's Secret)
//	LAKE_DUCKDB_PATH          the database file (default /data/main.duckdb)
//	LAKE_DUCKDB_ADDR          the listen address (default :8080)
//	LAKE_DUCKDB_MEMORY_LIMIT  the container memory limit in bytes (optional); DuckDB's
//	                          memory_limit is half of it and the Go heap a quarter, leaving a quarter
//	                          for what neither tracks
//	LAKE_DUCKDB_THREADS       DuckDB's worker threads (optional; default: the CPU count DuckDB sees)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/oramasearch/lake-duckdb/internal/server"
)

const (
	defaultPath = "/data/main.duckdb"
	defaultAddr = ":8080"
	// shutdownGrace is how long in-flight requests get after SIGTERM: the request timeout, so a
	// request that started just before the signal still finishes. The pod's
	// terminationGracePeriodSeconds is longer.
	shutdownGrace = server.DefaultRequestTimeout
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("lake-duckdb stopped", "err", err.Error())
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func run(log *slog.Logger) error {
	token := os.Getenv("LAKE_DUCKDB_TOKEN")
	if token == "" {
		return errors.New("LAKE_DUCKDB_TOKEN is not set")
	}
	path := env("LAKE_DUCKDB_PATH", defaultPath)
	addr := env("LAKE_DUCKDB_ADDR", defaultAddr)
	memoryLimit, err := parseCount("LAKE_DUCKDB_MEMORY_LIMIT", os.Getenv("LAKE_DUCKDB_MEMORY_LIMIT"))
	if err != nil {
		return err
	}
	threads, err := parseCount("LAKE_DUCKDB_THREADS", os.Getenv("LAKE_DUCKDB_THREADS"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg := server.Config{Path: path, Token: token, Threads: int(threads), Logger: log}
	if memoryLimit > 0 {
		// DuckDB's buffer manager, the Go heap (which holds a response while it is encoded) and
		// what DuckDB allocates outside its buffer manager share the container limit.
		cfg.MemoryLimit = memoryLimit / 2
		debug.SetMemoryLimit(memoryLimit / 4)
	}
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	srv, err := server.Open(openCtx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			log.Error("closing the database", "err", err.Error())
		}
	}()

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       server.DefaultRequestTimeout,
		// The handler's own deadline is the request timeout; the write deadline leaves room to
		// send the answer after it.
		WriteTimeout: server.DefaultRequestTimeout + 15*time.Second,
		IdleTimeout:  2 * time.Minute,
	}
	errs := make(chan error, 1)
	go func() {
		log.Info("lake-duckdb listening", "addr", addr, "path", path)
		errs <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	log.Info("lake-duckdb shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func parseCount(name, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, value)
	}
	return n, nil
}
