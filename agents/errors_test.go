package agents

import (
	"atlas/core"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken = "ghp_AGENTAGENTAGENTAGENTAGENTAGENTAGENT"
)

// fakeGH answers api.github.com requests and records them; every other host
// (the httptest Ollama server) passes through.
type fakeGH struct {
	mu      sync.Mutex
	calls   []string
	handler func(r *http.Request) *http.Response
}

func (f *fakeGH) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return http.DefaultTransport.RoundTrip(r)
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Host+r.URL.Path)
	f.mu.Unlock()
	return f.handler(r), nil
}

func (f *fakeGH) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func resp(status int, body string, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func installGH(t *testing.T, h func(r *http.Request) *http.Response) *fakeGH {
	t.Helper()
	f := &fakeGH{handler: h}
	old := core.SharedHTTPClient
	core.SharedHTTPClient = &http.Client{Transport: f, Timeout: 30 * time.Second}
	t.Cleanup(func() { core.SharedHTTPClient = old })
	return f
}

func newTestOrchestrator(t *testing.T) (*OrchestratorAgent, *bytes.Buffer) {
	t.Helper()
	o := NewOrchestratorAgent("test-model")
	var buf bytes.Buffer
	o.errOut = &buf
	return o, &buf
}

func contains(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

func lacks(t *testing.T, s string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(s, b) {
			t.Errorf("must not contain %q in:\n%s", b, s)
		}
	}
}

func contents404(repoStatus int) func(r *http.Request) *http.Response {
	return func(r *http.Request) *http.Response {
		if strings.HasSuffix(r.URL.Path, "/contents/package-lock.json") {
			return resp(404, `{"message":"Not Found"}`, nil)
		}
		return resp(repoStatus, `{}`, nil)
	}
}

// The reported cloudflare/binary-install case: lockfile absent, repo readable.
func TestAnalyzeRepository_MissingLockfile_ReportedFactually_AndStops(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testToken)
	gh := installGH(t, contents404(200))
	o, errBuf := newTestOrchestrator(t)

	result := o.AnalyzeRepository("cloudflare/binary-install", "", "", "")

	contains(t, result, "FAILED: No package-lock.json", "already shown the user", "Do not add explanations or guesses")
	// Everything the user sees comes from Atlas, not from the model.
	contains(t, errBuf.String(),
		`No package-lock.json at "package-lock.json" in cloudflare/binary-install on its default branch`,
		"The repository is accessible with the credentials used")
	lacks(t, errBuf.String()+result, "authoriz", "lacks", "does the repo have", testToken)
	if !o.failed {
		t.Error("failure flag not set; RunTurn would give the model another turn to speculate")
	}
	// Prerequisite failed => nothing downstream ran.
	if o.sess.Graph != nil || o.sess.SrcDir != "" || o.sess.Cleanup != nil {
		t.Errorf("session must stay empty after a failed fetch: %+v", o.sess)
	}
	for _, c := range gh.Calls() {
		if !strings.Contains(c, "/repos/cloudflare/binary-install") {
			t.Errorf("unexpected downstream request %q", c)
		}
	}
	if len(gh.Calls()) != 2 {
		t.Errorf("want contents + repo probe only, got %v", gh.Calls())
	}
}

func TestAnalyzeRepository_PrivateRepoNotVisible(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testToken)
	installGH(t, contents404(404))
	o, errBuf := newTestOrchestrator(t)
	result := o.AnalyzeRepository("acme/secret", "", "", "")
	contains(t, result, "FAILED: GitHub returned 404 for repository acme/secret")
	contains(t, errBuf.String(), "both when a repository does not exist and when it is private", "Contents: Read")
	lacks(t, errBuf.String(), "No package-lock.json", testToken)
}

func TestAnalyzeRepository_TokenRejectedMidSession(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testToken)
	installGH(t, func(r *http.Request) *http.Response { return resp(401, `{"message":"Bad credentials"}`, nil) })
	o, errBuf := newTestOrchestrator(t)
	result := o.AnalyzeRepository("o/r", "", "", "")
	contains(t, result, "FAILED: GitHub rejected GITHUB_TOKEN")
	contains(t, errBuf.String(), "invalid, expired or revoked", "https://github.com/settings/tokens")
	lacks(t, errBuf.String()+result, testToken)
}

func TestAnalyzeRepository_RateLimitAndNetworkFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	installGH(t, func(r *http.Request) *http.Response {
		return resp(403, `{"message":"API rate limit exceeded"}`, map[string]string{"X-RateLimit-Remaining": "0"})
	})
	o, errBuf := newTestOrchestrator(t)
	o.AnalyzeRepository("o/r", "", "", "")
	contains(t, errBuf.String(), "GitHub API rate limit reached (HTTP 403)", "export GITHUB_TOKEN=<token>")

	old := core.SharedHTTPClient
	core.SharedHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})}
	t.Cleanup(func() { core.SharedHTTPClient = old })
	o2, errBuf2 := newTestOrchestrator(t)
	o2.AnalyzeRepository("o/r", "", "", "")
	contains(t, errBuf2.String(), "Could not reach api.github.com", "Atlas was fetching package-lock.json from o/r.")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnalyzeRepository_LocalLockfileProblems(t *testing.T) {
	dir := t.TempDir()

	o, errBuf := newTestOrchestrator(t)
	o.AnalyzeRepository("", filepath.Join(dir, "missing.json"), "", "")
	contains(t, errBuf.String(), "lockfile not found at", "-file")

	bad := filepath.Join(dir, "package-lock.json")
	if err := os.WriteFile(bad, []byte(`{"lockfileVersion": 3,`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, errBuf = newTestOrchestrator(t)
	res := o.AnalyzeRepository("", bad, "", "")
	contains(t, errBuf.String(), bad+" is not valid JSON", "npm install --package-lock-only")
	contains(t, res, "FAILED:")

	empty := filepath.Join(dir, "empty", "package-lock.json")
	_ = os.MkdirAll(filepath.Dir(empty), 0o755)
	_ = os.WriteFile(empty, []byte(`{"name":"x"}`), 0o644)
	o, errBuf = newTestOrchestrator(t)
	o.AnalyzeRepository("", empty, "", "")
	contains(t, errBuf.String(), "does not look like an npm package-lock.json")
}

// A v1 lockfile with a missing package.json explains why package.json matters.
func TestAnalyzeRepository_V1LockfileNeedsPackageJSON(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "package-lock.json")
	_ = os.WriteFile(lock, []byte(`{"lockfileVersion":1,"dependencies":{"a":{"version":"1.0.0"}}}`), 0o644)
	o, errBuf := newTestOrchestrator(t)
	o.AnalyzeRepository("", lock, "", "")
	contains(t, errBuf.String(), "version 1 lockfile, which does not list the project's direct dependencies",
		"package.json not found at")
}

// The model must not get a second turn to paraphrase a failure Atlas has
// already shown (root cause of the speculative "authorization issue" text).
func TestRunTurn_ToolFailureEndsTurnWithoutAnotherModelCall(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testToken)
	installGH(t, contents404(200))

	var mu sync.Mutex
	chatCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		chatCalls++
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"done": true,
			"message": map[string]any{"role": "assistant", "tool_calls": []map[string]any{
				{"function": map[string]any{"name": "analyze_repository", "arguments": map[string]any{"repo": "cloudflare/binary-install"}}},
			}},
		})
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	o, errBuf := newTestOrchestrator(t)
	history := []core.OllamaMessage{{Role: "system", Content: systemPrompt}, {Role: "user", Content: "analyze cloudflare/binary-install"}}
	if err := o.RunTurn("test-model", &history, o.AgentTools()); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if chatCalls != 1 {
		t.Errorf("model called %d times; a failed tool must end the turn after the first call", chatCalls)
	}
	if o.failed {
		t.Error("failure flag must be cleared for the next turn")
	}
	contains(t, errBuf.String(), `No package-lock.json at "package-lock.json" in cloudflare/binary-install`)
	last := history[len(history)-1]
	if last.Role != "tool" || !strings.HasPrefix(last.Content, "FAILED: ") {
		t.Errorf("history must record the terse failure, got %+v", last)
	}
}

// ---- issue creation ----

func TestCreateGithubIssue_FailureClassification(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", testToken)
	cases := []struct {
		status int
		hdr    map[string]string
		body   string
		want   []string
		not    []string
	}{
		{401, nil, `{"message":"Bad credentials"}`, []string{"GitHub rejected GITHUB_TOKEN (HTTP 401"}, []string{"lack", "scope"}},
		{403, nil, `{"message":"Resource not accessible by personal access token"}`,
			[]string{"GitHub denied the request (HTTP 403"}, []string{"rate limit reached"}},
		{403, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"rate limit"}`, []string{"rate limit reached (HTTP 403)"}, nil},
		{404, nil, `{"message":"Not Found"}`, []string{"GitHub returned 404 for repository acme/app"}, nil},
		{422, nil, `{"message":"Validation Failed"}`, []string{"unexpected response (HTTP 422"}, nil},
	}
	for _, c := range cases {
		installGH(t, func(r *http.Request) *http.Response { return resp(c.status, c.body, c.hdr) })
		_, err := CreateGithubIssue("acme/app", IssueDraft{Title: "t", Body: "b"})
		if err == nil {
			t.Fatalf("%d: expected error", c.status)
		}
		short := core.ShortError(err)
		contains(t, short, c.want...)
		lacks(t, short, append(c.not, testToken)...)
	}
}
