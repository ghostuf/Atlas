package core

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test doubles: no real credentials, no live GitHub, no running Ollama.

const (
	validToken   = "ghp_VALIDVALIDVALIDVALIDVALIDVALIDVALID"
	invalidToken = "ghp_BADBADBADBADBADBADBADBADBADBADBADBAD"
)

// fakeGitHub routes api.github.com requests to handler and records every call.
// Other hosts (e.g. httptest Ollama servers on 127.0.0.1) pass through.
type fakeGitHub struct {
	mu      sync.Mutex
	calls   []string
	handler func(r *http.Request) (*http.Response, error)
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return http.DefaultTransport.RoundTrip(r)
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	return f.handler(r)
}

func (f *fakeGitHub) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func reply(status int, body string, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Status: http.StatusText(status)}
}

// installGitHub swaps SharedHTTPClient for the test and restores it after.
func installGitHub(t *testing.T, h func(r *http.Request) (*http.Response, error)) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{handler: h}
	old := SharedHTTPClient
	SharedHTTPClient = &http.Client{Transport: f, Timeout: 30 * time.Second}
	t.Cleanup(func() { SharedHTTPClient = old })
	return f
}

// githubAcceptingOnly answers /rate_limit with 200 for validToken and 401
// otherwise; other paths return 404 unless extra handles them.
func githubAcceptingOnly(extra func(r *http.Request) (*http.Response, error)) func(r *http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/rate_limit" {
			if r.Header.Get("Authorization") == "Bearer "+validToken {
				return reply(200, `{"resources":{}}`, nil), nil
			}
			return reply(401, `{"message":"Bad credentials"}`, nil), nil
		}
		if extra != nil {
			return extra(r)
		}
		return reply(404, `{"message":"Not Found"}`, nil), nil
	}
}

// ollamaUp starts a fake Ollama listing models and points OLLAMA_HOST at it.
func ollamaUp(t *testing.T, models ...string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		var items []string
		for _, m := range models {
			items = append(items, `{"name":"`+m+`"}`)
		}
		_, _ = io.WriteString(w, `{"models":[`+strings.Join(items, ",")+`]}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
}

// ollamaDown points OLLAMA_HOST at an address that refuses connections.
func ollamaDown(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	t.Setenv("OLLAMA_HOST", url)
	return url
}

func kinds(errs []*Error) []ErrKind {
	var out []ErrKind
	for _, e := range errs {
		out = append(out, e.Kind)
	}
	return out
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\n--- output ---\n%s", w, got)
		}
	}
}

func mustNotContain(t *testing.T, got string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(got, b) {
			t.Errorf("output must not contain %q\n--- output ---\n%s", b, got)
		}
	}
}
