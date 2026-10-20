package core

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"
)

const defaultModel = "gemma4:e4b"

func agentOpts() PreflightOptions {
	return PreflightOptions{RequireGithubToken: true, CheckGithub: true, CheckOllama: true, Model: defaultModel}
}

// ---- the six token x Ollama combinations from the bug report ----

func TestPreflight_InvalidToken_HealthyOllama(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	gh := installGitHub(t, githubAcceptingOnly(nil))
	ollamaUp(t, defaultModel)

	res := Preflight(agentOpts())
	if got, want := kinds(res.Blocking), []ErrKind{ErrGithubCredentials}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	out := FormatErrors(res.Blocking, PreflightHeader)
	mustContain(t, out, "GitHub rejected GITHUB_TOKEN", "HTTP 401", "Bad credentials",
		"invalid, expired or revoked", "https://github.com/settings/tokens", "unset GITHUB_TOKEN")
	mustNotContain(t, out, invalidToken, "Ollama")
	if got := gh.Calls(); !reflect.DeepEqual(got, []string{"GET /rate_limit"}) {
		t.Errorf("token check should be exactly one /rate_limit call, got %v", got)
	}
}

func TestPreflight_InvalidToken_UnreachableOllama_BothReported(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	installGitHub(t, githubAcceptingOnly(nil))
	url := ollamaDown(t)

	res := Preflight(agentOpts())
	want := []ErrKind{ErrGithubCredentials, ErrOllamaUnreachable}
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v (GitHub failure must not be masked by Ollama)", got, want)
	}
	out := FormatErrors(res.Blocking, PreflightHeader)
	mustContain(t, out, PreflightHeader, "[1/2] error: GitHub rejected GITHUB_TOKEN",
		"[2/2] error: Cannot connect to Ollama at "+url, "connection was refused", "ollama serve", "OLLAMA_HOST")
	mustNotContain(t, out, invalidToken, "is not installed")
}

func TestPreflight_InvalidToken_ReachableOllama_MissingModel(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaUp(t, "qwen3:8b")

	res := Preflight(agentOpts())
	want := []ErrKind{ErrGithubCredentials, ErrOllamaModelMissing}
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	mustContain(t, FormatErrors(res.Blocking, PreflightHeader),
		`Model "gemma4:e4b" is not installed in Ollama`, "Installed models: qwen3:8b", "ollama pull gemma4:e4b", "-model <name>")
}

func TestPreflight_ValidToken_UnreachableOllama(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaDown(t)

	res := Preflight(agentOpts())
	if got, want := kinds(res.Blocking), []ErrKind{ErrOllamaUnreachable}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestPreflight_ValidToken_MissingModel(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaUp(t) // reachable, no models

	res := Preflight(agentOpts())
	if got, want := kinds(res.Blocking), []ErrKind{ErrOllamaModelMissing}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	mustContain(t, res.Blocking[0].Format(), "No models are installed.", "ollama pull gemma4:e4b")
}

func TestPreflight_ValidToken_HealthyOllama_NoErrors(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaUp(t, "gemma4:e4b")
	res := Preflight(agentOpts())
	if len(res.Blocking) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("expected a clean preflight, got %v / %v", kinds(res.Blocking), kinds(res.Warnings))
	}
}

// ---- Ollama specifics ----

func TestPreflight_MissingModelNeverReportedForUnreachableServer(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaDown(t)
	for _, e := range Preflight(agentOpts()).Blocking {
		if e.Kind == ErrOllamaModelMissing {
			t.Fatalf("model reported missing although Ollama was unreachable: %s", e.Format())
		}
	}
}

func TestOllama_NonSuccessStatusIsNotReportedAsMissingModel(t *testing.T) {
	ollamaUp(t, defaultModel)
	// ollamaUp's server 404s everything except /api/tags; ask for a path it
	// doesn't serve by asking CheckOllamaModel through a broken base path.
	t.Setenv("OLLAMA_HOST", OllamaBaseURL()+"/nope")
	e := CheckOllamaModel(defaultModel)
	if e == nil || e.Kind != ErrOllamaUnexpected {
		t.Fatalf("want ErrOllamaUnexpected, got %+v", e)
	}
	mustContain(t, e.Format(), "answered GET /api/tags with HTTP 404", "curl ")
	mustNotContain(t, e.Format(), "is not installed")
}

// ---- configuration ----

func TestPreflight_MissingToken_AgentMode_ReportedWithOllama(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	gh := installGitHub(t, githubAcceptingOnly(nil))
	ollamaDown(t)

	res := Preflight(agentOpts())
	want := []ErrKind{ErrConfig, ErrOllamaUnreachable}
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	mustContain(t, res.Blocking[0].Format(), "GITHUB_TOKEN is not set", "export GITHUB_TOKEN", ".env", "go run . -repo owner/name")
	if len(gh.Calls()) != 0 {
		t.Errorf("no token => nothing to validate => no GitHub calls, got %v", gh.Calls())
	}
}

func TestPreflight_PublicRepoWithoutToken_OneShot(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	gh := installGitHub(t, githubAcceptingOnly(nil))
	res := Preflight(PreflightOptions{CheckGithub: true}) // one-shot -repo: token optional, no Ollama
	if len(res.Blocking) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("public repo without a token must be allowed, got %v", kinds(res.Blocking))
	}
	if len(gh.Calls()) != 0 {
		t.Errorf("no requests expected, got %v", gh.Calls())
	}
}

func TestPreflight_OneShot_DoesNotRequireOllama(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaDown(t)
	if res := Preflight(PreflightOptions{CheckGithub: true}); len(res.Blocking) != 0 {
		t.Fatalf("one-shot mode must ignore Ollama, got %v", kinds(res.Blocking))
	}
}

func TestPreflight_OneShotInvalidTokenStillBlocksPublicRepos(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	installGitHub(t, githubAcceptingOnly(nil))
	res := Preflight(PreflightOptions{CheckGithub: true})
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, []ErrKind{ErrGithubCredentials}) {
		t.Fatalf("kinds = %v", got)
	}
}

func TestPreflight_Config_EmptyModelAndBadOllamaHost(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	gh := installGitHub(t, githubAcceptingOnly(nil))

	res := Preflight(PreflightOptions{CheckGithub: true, CheckOllama: true, Model: ""})
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, []ErrKind{ErrConfig}) {
		t.Fatalf("empty model: kinds = %v", got)
	}
	mustContain(t, res.Blocking[0].Format(), "No Ollama model is configured", "-model <name>")

	t.Setenv("OLLAMA_HOST", "http://exa mple:1")
	res = Preflight(PreflightOptions{CheckGithub: true, CheckOllama: true, Model: defaultModel})
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, []ErrKind{ErrConfig}) {
		t.Fatalf("bad OLLAMA_HOST: kinds = %v", got)
	}
	mustContain(t, res.Blocking[0].Format(), "OLLAMA_HOST is not a valid address")
	_ = gh
}

// ---- GitHub-side outcomes of the token check ----

func TestPreflight_TokenCheckRateLimited_IsWarningNotBlocking(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, func(r *http.Request) (*http.Response, error) {
		return reply(429, `{"message":"secondary rate limit"}`, nil), nil
	})
	res := Preflight(PreflightOptions{CheckGithub: true})
	if len(res.Blocking) != 0 || len(res.Warnings) != 1 || res.Warnings[0].Kind != ErrGithubRateLimit {
		t.Fatalf("want one rate-limit warning, got blocking=%v warnings=%v", kinds(res.Blocking), kinds(res.Warnings))
	}
	mustContain(t, FormatWarning(res.Warnings[0]), "warning: Could not verify GITHUB_TOKEN", "it will continue")
}

func TestPreflight_TokenCheckGitHubDown_IsWarning(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, func(r *http.Request) (*http.Response, error) { return reply(503, `{}`, nil), nil })
	res := Preflight(PreflightOptions{CheckGithub: true})
	if len(res.Blocking) != 0 || len(res.Warnings) != 1 || res.Warnings[0].Kind != ErrGithubUnavailable {
		t.Fatalf("got blocking=%v warnings=%v", kinds(res.Blocking), kinds(res.Warnings))
	}
}

func TestPreflight_GitHubUnreachable_IsBlocking(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup api.github.com: no such host (token " + validToken + ")")
	})
	res := Preflight(PreflightOptions{CheckGithub: true})
	if got := kinds(res.Blocking); !reflect.DeepEqual(got, []ErrKind{ErrGithubNetwork}) {
		t.Fatalf("kinds = %v", got)
	}
	out := res.Blocking[0].Format()
	mustContain(t, out, "Could not reach api.github.com", "HTTPS_PROXY")
	mustNotContain(t, out, validToken) // transport error text carried the token
}

// ---- ordering ----

func TestPreflight_ReportingOrderIsDeterministic(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	// GitHub is slow, Ollama fails instantly: completion order is the reverse
	// of reporting order.
	installGitHub(t, func(r *http.Request) (*http.Response, error) {
		time.Sleep(15 * time.Millisecond)
		return reply(401, `{"message":"Bad credentials"}`, nil), nil
	})
	ollamaDown(t)
	want := []ErrKind{ErrGithubCredentials, ErrOllamaUnreachable}
	for i := 0; i < 20; i++ {
		if got := kinds(Preflight(agentOpts()).Blocking); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: kinds = %v, want %v", i, got, want)
		}
	}
}

func TestPreflight_ConfigIsReportedBeforeGitHubAndOllama(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	installGitHub(t, githubAcceptingOnly(nil))
	ollamaUp(t) // model missing
	res := Preflight(agentOpts())
	if got, want := kinds(res.Blocking), []ErrKind{ErrConfig, ErrOllamaModelMissing}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
}

func TestOllama_LatestTagMatchesUntaggedModelName(t *testing.T) {
	ollamaUp(t, "llama3:latest")
	if e := CheckOllamaModel("llama3"); e != nil {
		t.Fatalf("llama3 should match llama3:latest, got %s", e.Format())
	}
}
