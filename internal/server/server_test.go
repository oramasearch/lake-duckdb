// SPDX-License-Identifier: MIT

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "test-token-0123456789abcdef"

type harness struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = filepath.Join(t.TempDir(), "main.duckdb")
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	logs := &bytes.Buffer{}
	cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	srv, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, server: srv, http: httptest.NewServer(srv.Handler()), logs: logs}
	t.Cleanup(func() {
		h.http.Close()
		_ = srv.Close()
	})
	return h
}

// call sends body (a string, or a value marshalled to JSON) and decodes the answer.
func (h *harness) call(method, path, token string, body interface{}) (int, map[string]interface{}) {
	h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.http.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	var decoded map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		h.t.Fatalf("%s %s: status %d, body %q is not a JSON object: %v", method, path, resp.StatusCode, raw, err)
	}
	return resp.StatusCode, decoded
}

func (h *harness) post(path string, body interface{}) (int, map[string]interface{}) {
	h.t.Helper()
	return h.call(http.MethodPost, path, testToken, body)
}

func (h *harness) mustExec(sql string, params ...interface{}) map[string]interface{} {
	h.t.Helper()
	if params == nil {
		params = []interface{}{}
	}
	status, body := h.post("/v1/exec", map[string]interface{}{"sql": sql, "params": params})
	if status != http.StatusOK {
		h.t.Fatalf("exec %q: %d %v", sql, status, body)
	}
	return body
}

func errorCode(body map[string]interface{}) string {
	detail, _ := body["error"].(map[string]interface{})
	code, _ := detail["code"].(string)
	return code
}

// canonical renders v as compact JSON, for comparing decoded answers.
func canonical(t *testing.T, v interface{}) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
