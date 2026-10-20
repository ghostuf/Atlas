package core

import (
	"encoding/base64"
	"errors"
	"net/http"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func asErr(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	return e
}

// github routes by path: contents → contents, /repos/o/r → repo.
func github(contents, repo func() *http.Response) func(r *http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/repos/cloudflare/binary-install/contents/package-lock.json",
			"/repos/o/r/contents/package-lock.json", "/repos/o/r/contents/app/package-lock.json":
			return contents(), nil
		case "/repos/cloudflare/binary-install", "/repos/o/r":
			return repo(), nil
		}
		return reply(500, `{"message":"unexpected path `+r.URL.Path+`"}`, nil), nil
	}
}

func notFound() *http.Response { return reply(404, `{"message":"Not Found"}`, nil) }

// ---- the reported scenario: cloudflare/binary-install ----

func TestFetchFile_MissingLockfile_RepoAccessible(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	gh := installGitHub(t, github(notFound, func() *http.Response { return reply(200, `{"private":false}`, nil) }))

	_, err := FetchFile("cloudflare/binary-install", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrFileNotFound {
		t.Fatalf("kind = %s, want %s", e.Kind, ErrFileNotFound)
	}
	out := e.Format()
	mustContain(t, out,
		`No package-lock.json at "package-lock.json" in cloudflare/binary-install on its default branch`,
		"The repository is accessible with the credentials used",
		"Not verified (possible causes):", "yarn.lock or pnpm-lock.yaml",
		"-dir <path>", "npm install --package-lock-only", "-file ./package-lock.json")
	// SC5: an absent file must not be presented as an authorization problem,
	// and nothing may be asserted about the token or repo visibility.
	mustNotContain(t, out, "authoriz", "lacks", "private repository", "scope", validToken)
	if got := gh.Calls(); len(got) != 2 {
		t.Errorf("expected contents + one repo probe, got %v", got)
	}
}

func TestFetchFile_MissingLockfile_WithRefAndDir(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	installGitHub(t, github(notFound, func() *http.Response { return reply(200, `{}`, nil) }))
	_, err := FetchFile("o/r", "v2", "app", "package-lock.json")
	out := asErr(t, err).Format()
	mustContain(t, out, `No package-lock.json at "app/package-lock.json" in o/r on ref "v2"`,
		"the path or the ref does not exist", `Check -dir "app"`)
}

func TestFetchFile_PrivateRepoNotVisible_WithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, github(notFound, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrRepoNotFound {
		t.Fatalf("kind = %s", e.Kind)
	}
	out := e.Format()
	mustContain(t, out, "GitHub returned 404 for repository o/r",
		"both when a repository does not exist and when it is private and the credentials cannot see it",
		"A GITHUB_TOKEN was sent", "Contents: Read", "SSO-authorized", "https://github.com/o/r")
	mustNotContain(t, out, validToken, "No package-lock.json")
}

func TestFetchFile_RepoNotFound_NoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	installGitHub(t, github(notFound, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	mustContain(t, asErr(t, err).Format(), "No GITHUB_TOKEN was sent", "export GITHUB_TOKEN=<token>")
}

func TestFetchFile_RejectedToken_401_NoFollowUp(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", invalidToken)
	gh := installGitHub(t, github(func() *http.Response { return reply(401, `{"message":"Bad credentials"}`, nil) }, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubCredentials {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "GitHub rejected GITHUB_TOKEN (HTTP 401", "Atlas was fetching package-lock.json from o/r.")
	mustNotContain(t, e.Format(), "does the repo have", "committed", invalidToken)
	if len(gh.Calls()) != 1 {
		t.Errorf("a 401 is conclusive; no extra requests expected, got %v", gh.Calls())
	}
}

func TestFetchFile_Forbidden_NotRateLimit(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, github(func() *http.Response {
		return reply(403, `{"message":"Resource not accessible by personal access token"}`, map[string]string{"X-RateLimit-Remaining": "4000"})
	}, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubForbidden {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), `HTTP 403: "Resource not accessible by personal access token"`,
		"not a rate-limit response", "Not verified (possible causes):", "Contents: Read")
	// the file's existence must not be inferred from a 403
	mustNotContain(t, e.Format(), "No package-lock.json", "does not exist")
}

func TestFetchFile_SSORequired(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, github(func() *http.Response {
		return reply(403, `{"message":"Resource protected by organization SAML enforcement."}`,
			map[string]string{"X-GitHub-SSO": "required; url=https://github.com/orgs/acme/sso?authorization_request=XYZ"})
	}, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubSSO {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "requires SSO authorization", "https://github.com/orgs/acme/sso?authorization_request=XYZ")
}

func TestFetchFile_RateLimit_403WithExhaustedHeader(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	reset := time.Now().Add(20 * time.Minute).Unix()
	installGitHub(t, github(func() *http.Response {
		return reply(403, `{"message":"API rate limit exceeded for 1.2.3.4."}`,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset, 10)})
	}, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubRateLimit {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "GitHub API rate limit reached (HTTP 403)", "The limit resets at", "60 unauthenticated requests per hour",
		"export GITHUB_TOKEN=<token>")
}

func TestFetchFile_RateLimit_429_WithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, github(func() *http.Response {
		return reply(429, `{"message":"slow down"}`, map[string]string{"Retry-After": "60"})
	}, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubRateLimit {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "HTTP 429", "Wait for the limit to reset")
	mustNotContain(t, e.Format(), "Set GITHUB_TOKEN") // a token was already sent
}

func TestFetchFile_NetworkFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	installGitHub(t, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: lookup api.github.com: no such host")
	})
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubNetwork {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "Could not reach api.github.com", "Atlas was fetching package-lock.json from o/r.", "Detail:")
	mustNotContain(t, e.Format(), "committed", "private")
}

func TestFetchFile_GitHub5xx(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	installGitHub(t, github(func() *http.Response { return reply(503, `{}`, nil) }, notFound))
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	e := asErr(t, err)
	if e.Kind != ErrGithubUnavailable {
		t.Fatalf("kind = %s", e.Kind)
	}
	mustContain(t, e.Format(), "server-side failure", "githubstatus.com")
}

func TestFetchFile_404ThenProbeFails_StatesWhatIsUnknown(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	n := 0
	installGitHub(t, func(r *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			return notFound(), nil
		}
		return nil, errors.New("connection reset by peer")
	})
	_, err := FetchFile("o/r", "", "", "package-lock.json")
	out := asErr(t, err).Format()
	mustContain(t, out, "Atlas could not tell which: the follow-up repository check failed.")
	mustNotContain(t, out, "The repository is accessible")
}

func TestFetchFile_InvalidRepoSpec(t *testing.T) {
	for _, spec := range []string{"justname", "/x", "x/", "a/b/c"} {
		_, err := FetchFile(spec, "", "", "package-lock.json")
		e := asErr(t, err)
		if e.Kind != ErrConfig {
			t.Errorf("%q: kind = %s", spec, e.Kind)
		}
		mustContain(t, e.Format(), "owner/name")
	}
}

func TestFetchFile_DirectoryEmptyAndTooLarge(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cases := []struct {
		name, body string
		kind       ErrKind
		want       []string
	}{
		{"directory", `[{"name":"a"}]`, ErrFileUnreadable, []string{"is a directory, not a file", "-dir"}},
		{"empty", `{"content":"","encoding":"base64","size":0}`, ErrFileUnreadable, []string{"is empty"}},
		{"large", `{"content":"","encoding":"none","size":2500000}`, ErrFileUnreadable,
			[]string{"(2500000 bytes)", "does not inline files larger than 1 MB", "-file ./package-lock.json"}},
	}
	for _, c := range cases {
		installGitHub(t, github(func() *http.Response { return reply(200, c.body, nil) }, notFound))
		_, err := FetchFile("o/r", "", "", "package-lock.json")
		e := asErr(t, err)
		if e.Kind != c.kind {
			t.Errorf("%s: kind = %s", c.name, e.Kind)
		}
		mustContain(t, e.Format(), c.want...)
	}
}

// SC8: the success path still returns the file bytes, with one request.
func TestFetchFile_Success_Unchanged(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	want := `{"lockfileVersion":3}`
	gh := installGitHub(t, github(func() *http.Response {
		return reply(200, `{"content":"`+base64.StdEncoding.EncodeToString([]byte(want))+`","encoding":"base64","size":21}`, nil)
	}, notFound))
	got, err := FetchFile("o/r", "", "", "package-lock.json")
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v", got, err)
	}
	if !reflect.DeepEqual(gh.Calls(), []string{"GET /repos/o/r/contents/package-lock.json"}) {
		t.Errorf("calls = %v", gh.Calls())
	}
}

// ---- git clone classification ----

func TestClassifyCloneFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	exitErr := errors.New("exit status 128")
	cases := []struct {
		name string
		err  error
		out  string
		ref  string
		want []string
	}{
		{"ref is a sha", exitErr, "fatal: Remote branch 3d316b3 not found in upstream origin", "3d316b3",
			[]string{`Branch or tag "3d316b3" was not found in o/r`, "not a commit SHA"}},
		{"auth", exitErr, "remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/o/r.git/'", "",
			[]string{"refused (authentication failure)", "git reported an authentication or access failure", "A GITHUB_TOKEN was sent"}},
		{"not found", exitErr, "remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found", "",
			[]string{"git could not find repository o/r", "does not exist and when it is private"}},
		{"network", exitErr, "fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com", "",
			[]string{"git could not reach github.com"}},
		{"no git", exec.ErrNotFound, "", "", []string{"git is not installed or not on PATH"}},
		{"other", exitErr, "fatal: something odd", "", []string{"git clone of o/r failed", "Detail: fatal: something odd"}},
	}
	for _, c := range cases {
		e := classifyCloneFailure("o/r", c.ref, c.err, c.out)
		if e.Kind != ErrCloneFailed {
			t.Errorf("%s: kind = %s", c.name, e.Kind)
		}
		mustContain(t, e.Format(), c.want...)
	}
}

func TestClassifyCloneFailure_RedactsTokenEchoedByGit(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", validToken)
	out := "fatal: unable to access 'https://x-access-token:" + validToken + "@github.com/o/r.git/': The requested URL returned error: 403"
	e := classifyCloneFailure("o/r", "", errors.New("exit status 128"), out)
	mustNotContain(t, e.Format(), validToken)
	mustNotContain(t, e.Detail, validToken)
	mustNotContain(t, e.Short(), validToken)
	mustContain(t, e.Format(), "https://***@github.com/o/r.git") // rest of the line survives
}
