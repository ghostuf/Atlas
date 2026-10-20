package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// ---- redaction ----

func TestRedact(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "custom-token-value-123")
	in := strings.Join([]string{
		"env: custom-token-value-123",
		"classic: ghp_" + strings.Repeat("a", 36),
		"oauth: gho_" + strings.Repeat("b", 36),
		"fine: github_pat_" + strings.Repeat("C", 30),
		"url: https://x-access-token:sekrit@github.com/o/r.git",
		"hdr: Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123",
	}, "\n")
	out := Redact(in)
	mustNotContain(t, out, "custom-token-value-123", "ghp_", "gho_", "github_pat_", "sekrit", "abcdefghijklmnop")
	mustContain(t, out, "https://***@github.com/o/r.git", "Bearer ***")
}

func TestRedact_ShortTokenValueDoesNotMangleOutput(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x") // would otherwise replace every 'x'
	if got := Redact("express exists"); got != "express exists" {
		t.Fatalf("got %q", got)
	}
}

// Every user-facing rendering of every error path must be secret-free, even
// when the secret is carried in the underlying cause.
func TestRedaction_AcrossAllErrorPaths(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	leaky := errors.New(`Get "https://api.github.com/x": proxy auth failed for Bearer ` + validToken + ` (` + validToken + `)`)

	all := []error{
		errGithubNetwork("doing", leaky),
		errOllamaUnreachable("http://localhost:11434", "doing", leaky),
		classifyOllamaChatErr("m", leaky),
		classifyCloneFailure("o/r", "", leaky, "fatal: https://x-access-token:"+validToken+"@github.com/o/r.git"),
		LockfileParseError(leaky, "lockfile "+validToken),
		Annotate(leaky, "doing"),
		leaky,
	}
	for i, err := range all {
		for name, s := range map[string]string{
			"FormatError": FormatError(err), "ShortError": ShortError(err), "Error()": Redact(err.Error()),
		} {
			if strings.Contains(s, validToken) {
				t.Errorf("error %d leaked the token via %s: %s", i, name, s)
			}
		}
		var ue *Error
		if errors.As(err, &ue) && strings.Contains(ue.Error(), validToken) {
			t.Errorf("error %d leaked the token via (*Error).Error()", i)
		}
	}
}

func TestRedaction_PreflightOutputWithRejectedToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	installGitHub(t, func(r *http.Request) (*http.Response, error) {
		// A hostile/odd server echoing the credential back in its message.
		return reply(401, `{"message":"Bad credentials for `+invalidToken+`"}`, nil), nil
	})
	res := Preflight(PreflightOptions{CheckGithub: true})
	mustNotContain(t, FormatErrors(res.Blocking, PreflightHeader), invalidToken)
}

// ---- formatting contract ----

func TestError_FormatKeepsFactsAndHypothesesSeparate(t *testing.T) {
	e := &Error{Title: "T", Doing: "D", Facts: []string{"f1"}, Maybe: []string{"m1"}, Next: []string{"n1"}, Detail: "x\n  y"}
	out := e.Format()
	want := "error: T\n  D\n\n  Known:\n    - f1\n\n  Not verified (possible causes):\n    - m1\n\n  Next:\n    - n1\n\n  Detail: x y\n"
	if out != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
	if e.Short() != "T — n1" {
		t.Errorf("Short() = %q", e.Short())
	}
	// Sections with no content are omitted entirely.
	if out := (&Error{Title: "only title"}).Format(); out != "error: only title\n" {
		t.Errorf("got %q", out)
	}
}

func TestFormatErrors_SingleErrorHasNoHeader(t *testing.T) {
	out := FormatErrors([]*Error{{Title: "one"}}, PreflightHeader)
	if strings.Contains(out, "[1/") || strings.Contains(out, "Atlas can't start") {
		t.Errorf("single error should be printed alone, got:\n%s", out)
	}
}

func TestAnnotate_PrependsContextAndKeepsKind(t *testing.T) {
	orig := &Error{Kind: ErrGithubNetwork, Title: "t", Doing: "Atlas was fetching package.json from o/r."}
	got := Annotate(orig, "This is a v1 lockfile.").(*Error)
	if got.Kind != ErrGithubNetwork || got.Doing != "This is a v1 lockfile. Atlas was fetching package.json from o/r." {
		t.Errorf("got %+v", got)
	}
	if orig.Doing != "Atlas was fetching package.json from o/r." {
		t.Error("Annotate must not mutate its input")
	}
	if plain := Annotate(errors.New("boom"), "ctx").(*Error); plain.Title != "boom" || plain.Doing != "ctx" {
		t.Errorf("plain wrap: %+v", plain)
	}
}

// ---- local files / lockfile shape ----

func TestReadLocalFile_Missing(t *testing.T) {
	_, err := ReadLocalFile(filepath.Join(t.TempDir(), "nope.json"), "lockfile")
	e := asErr(t, err)
	if e.Kind != ErrLocalFile {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "lockfile not found at", "No file exists at that path", "-file")
}

func TestLockfileParseError_InvalidJSON(t *testing.T) {
	var lf Lockfile
	err := json.Unmarshal([]byte(`{"lockfileVersion": 3,`), &lf)
	e := LockfileParseError(err, "./package-lock.json")
	if e.Kind != ErrLockfileInvalid {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "./package-lock.json is not valid JSON (syntax error at byte", "Not verified (possible causes):",
		"merge-conflict markers", "npm install --package-lock-only")
}

func TestLockfileParseError_WrongShape(t *testing.T) {
	var lf Lockfile
	err := json.Unmarshal([]byte(`{"packages": []}`), &lf)
	e := LockfileParseError(err, "lock")
	mustContain(t, e.Format(), "has an unexpected structure", `"packages"`, "valid JSON but does not match")
}

func TestBuildGraph_EmptyLockfileMessages(t *testing.T) {
	cases := []struct {
		version int
		want    string
	}{
		{0, "does not look like an npm package-lock.json"},
		{9, "lockfileVersion 9 is not supported"},
		{3, "lists no packages (lockfileVersion 3)"},
	}
	for _, c := range cases {
		_, err := BuildGraph(&Lockfile{LockfileVersion: c.version})
		e := asErr(t, err)
		if e.Kind != ErrLockfileInvalid {
			t.Errorf("v%d: kind = %s", c.version, e.Kind)
		}
		mustContain(t, e.Format(), c.want, "yarn.lock or pnpm-lock.yaml")
	}
}

func TestLoadPackageJSON_LocalMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadPackageJSON("", "", "", filepath.Join(dir, "package-lock.json"))
	e := asErr(t, err)
	mustContain(t, e.Format(), "package.json not found at")
}

// ---- Ollama failures after startup ----

func chatServer(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
}

func TestChatOllama_ModelRemovedMidSession(t *testing.T) {
	chatServer(t, 404, `{"error":"model \"gemma4:e4b\" not found, try pulling it first"}`)
	_, err := ChatOllama("gemma4:e4b", nil, nil)
	e := asErr(t, err)
	if e.Kind != ErrOllamaModelMissing {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), `Ollama reports that model "gemma4:e4b" is not installed`, "ollama pull gemma4:e4b", "HTTP 404")
}

func TestChatOllama_Generic404IsNotAMissingModel(t *testing.T) {
	chatServer(t, 404, "404 page not found")
	_, err := ChatOllamaJSON("m", nil, map[string]any{})
	e := asErr(t, err)
	if e.Kind != ErrOllamaUnexpected {
		t.Fatalf("kind = %s (a bare 404 must not be reported as a missing model)", e.Kind)
	}
	mustContain(t, e.Format(), "Ollama returned HTTP 404", "404 page not found")
}

func TestChatOllama_ServerError(t *testing.T) {
	chatServer(t, 500, `{"error":"llama runner process has terminated"}`)
	_, err := ChatOllama("m", nil, nil)
	e := asErr(t, err)
	mustContain(t, e.Format(), "Ollama returned HTTP 500", "Ollama said: llama runner process has terminated", "server log")
}

func TestChatOllama_ServerStoppedMidSession(t *testing.T) {
	url := ollamaDown(t)
	_, err := ChatOllama("m", nil, nil)
	e := asErr(t, err)
	if e.Kind != ErrOllamaUnreachable {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "Cannot connect to Ollama at "+url, "connection was refused")
	mustNotContain(t, e.Format(), fmt.Sprintf("%q is not installed", "m"))
}
