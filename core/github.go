package core

// GitHub response classification.
//
// What GitHub's status codes do and do not establish (see the REST docs):
//
//	401  credentials were sent and rejected (invalid, expired or revoked).
//	403  authenticated but refused: a rate limit (X-RateLimit-Remaining: 0,
//	     Retry-After, or a rate-limit message), SAML SSO (X-GitHub-SSO), or a
//	     missing permission. Only the first two are provable from the response.
//	404  the resource does not exist OR is private and the credentials can't
//	     see it. GitHub deliberately doesn't say which, so Atlas probes the
//	     repository itself before claiming either.
//	429  rate limited.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func hasToken() bool { return strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != "" }

func tokenFact() string {
	if hasToken() {
		return "A GITHUB_TOKEN was sent with the request."
	}
	return "No GITHUB_TOKEN was sent (unauthenticated request)."
}

// ghResponse is the part of a GitHub response needed to classify a failure.
type ghResponse struct {
	Status  int
	Header  http.Header
	Message string // GitHub's "message" field, if any
}

func newGhResponse(status int, h http.Header, body []byte) ghResponse {
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &m)
	return ghResponse{Status: status, Header: h, Message: m.Message}
}

func (r ghResponse) statusLine() string {
	s := fmt.Sprintf("HTTP %d", r.Status)
	if r.Message != "" {
		s += fmt.Sprintf(": %q", r.Message)
	}
	return s
}

func (r ghResponse) rateLimited() bool {
	if r.Status == http.StatusTooManyRequests {
		return true
	}
	if r.Status != http.StatusForbidden {
		return false
	}
	return r.Header.Get("X-RateLimit-Remaining") == "0" ||
		r.Header.Get("Retry-After") != "" ||
		strings.Contains(strings.ToLower(r.Message), "rate limit")
}

func (r ghResponse) resetAt() (time.Time, bool) {
	n, err := strconv.ParseInt(r.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// ssoURL returns the authorization URL from "X-GitHub-SSO: required; url=...".
func (r ghResponse) ssoURL() string {
	v := r.Header.Get("X-GitHub-SSO")
	if !strings.HasPrefix(v, "required") {
		return ""
	}
	if _, u, ok := strings.Cut(v, "url="); ok {
		return strings.TrimSpace(u)
	}
	return "(see the organization's SSO page)"
}

// githubGet performs an authenticated GET and returns the classified response
// head plus the body. A non-nil error means no usable response was received.
func githubGet(u string) (ghResponse, []byte, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return ghResponse{}, nil, err
	}
	for k, v := range GithubHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return ghResponse{}, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ghResponse{}, nil, err
	}
	return newGhResponse(resp.StatusCode, resp.Header, body), body, nil
}

// ---------- constructors ----------

func errGithubCredentials(doing string, r ghResponse) *Error {
	return &Error{
		Kind:  ErrGithubCredentials,
		Title: "GitHub rejected GITHUB_TOKEN (" + r.statusLine() + ")",
		Doing: doing,
		Facts: []string{
			"GitHub answers 401 when the credentials sent are invalid, expired or revoked; it does not say which.",
			"A rejected token fails on every request, including requests for public repositories.",
		},
		Next: []string{
			"Create a new token at https://github.com/settings/tokens and set it: export GITHUB_TOKEN=<token> (or put it in .env).",
			"For public repositories only, unset GITHUB_TOKEN to run without one.",
		},
	}
}

func errGithubRateLimit(doing string, r ghResponse) *Error {
	e := &Error{
		Kind:  ErrGithubRateLimit,
		Title: fmt.Sprintf("GitHub API rate limit reached (HTTP %d)", r.Status),
		Doing: doing,
		Facts: []string{tokenFact()},
	}
	if t, ok := r.resetAt(); ok {
		in := "in under a minute"
		if m := int(time.Until(t).Round(time.Minute).Minutes()); m >= 1 {
			in = fmt.Sprintf("in about %d min", m)
		}
		e.Facts = append(e.Facts, fmt.Sprintf("The limit resets at %s (%s).", t.Local().Format("15:04:05 MST"), in))
	}
	if hasToken() {
		e.Next = []string{"Wait for the limit to reset, then retry."}
	} else {
		e.Facts = append(e.Facts, "GitHub allows 60 unauthenticated requests per hour per IP, versus 5,000 per hour with a token.")
		e.Next = []string{"Set GITHUB_TOKEN to raise the limit: export GITHUB_TOKEN=<token> (or put it in .env).", "Or wait for the limit to reset, then retry."}
	}
	return e
}

func errGithubForbidden(doing string, r ghResponse) *Error {
	if sso := r.ssoURL(); sso != "" {
		return &Error{
			Kind:  ErrGithubSSO,
			Title: "GitHub requires SSO authorization for this token (" + r.statusLine() + ")",
			Doing: doing,
			Facts: []string{"The response carries an X-GitHub-SSO header: the organization enforces SAML SSO and this token is not authorized for it."},
			Next:  []string{"Authorize the token for the organization: " + sso},
		}
	}
	e := &Error{
		Kind:  ErrGithubForbidden,
		Title: "GitHub denied the request (" + r.statusLine() + ")",
		Doing: doing,
		Facts: []string{
			"The response is not a rate-limit response (no exhausted X-RateLimit-Remaining, no Retry-After, no rate-limit message).",
			tokenFact(),
		},
	}
	if hasToken() {
		e.Maybe = []string{
			"The token lacks a permission this request needs (classic token: `repo` scope; fine-grained token: access to this repository with Contents: Read, or Issues: Read and write for issues).",
			"An organization policy (SSO, IP allow list, or token restrictions) blocks this token.",
		}
		e.Next = []string{"Review the token's permissions at https://github.com/settings/tokens."}
	} else {
		e.Maybe = []string{"The resource requires authentication."}
		e.Next = []string{"Set GITHUB_TOKEN and retry: export GITHUB_TOKEN=<token> (or put it in .env)."}
	}
	return e
}

func errGithubNetwork(doing string, err error) *Error {
	e := &Error{
		Kind:  ErrGithubNetwork,
		Title: "Could not reach api.github.com",
		Doing: doing,
		Err:   err,
		Next:  []string{"Check your internet connection and any proxy settings (HTTPS_PROXY), then retry."},
	}
	switch classifyNet(err) {
	case netRefused:
		e.Facts = []string{"The connection was refused."}
	case netTimeout:
		e.Facts = []string{fmt.Sprintf("No response arrived within %s.", SharedHTTPClient.Timeout)}
	case netDNS:
		e.Facts = []string{"The host name api.github.com could not be resolved."}
	}
	return e
}

func errGithubUnavailable(doing string, r ghResponse) *Error {
	return &Error{
		Kind:  ErrGithubUnavailable,
		Title: "GitHub API returned " + r.statusLine(),
		Doing: doing,
		Facts: []string{"A 5xx status is a server-side failure; it is not caused by your token or repository."},
		Next:  []string{"Check https://www.githubstatus.com and retry later."},
	}
}

func errGithubUnexpected(doing string, r ghResponse) *Error {
	return &Error{
		Kind:  ErrGithubUnexpected,
		Title: "GitHub API returned an unexpected response (" + r.statusLine() + ")",
		Doing: doing,
	}
}

// classifyGithubResponse maps a non-success response to an *Error. It never
// handles 404: what a 404 means depends on the request (see diagnose404).
func classifyGithubResponse(doing string, r ghResponse) *Error {
	switch {
	case r.Status == http.StatusUnauthorized:
		return errGithubCredentials(doing, r)
	case r.rateLimited():
		return errGithubRateLimit(doing, r)
	case r.Status == http.StatusForbidden:
		return errGithubForbidden(doing, r)
	case r.Status >= 500:
		return errGithubUnavailable(doing, r)
	}
	return errGithubUnexpected(doing, r)
}

// GithubRequestError classifies a failed GitHub API write/search made outside
// this package (issue search and creation). subject names the repository for
// 404s, which here mean "not found or not visible to these credentials".
func GithubRequestError(doing, subject string, status int, h http.Header, body []byte) *Error {
	r := newGhResponse(status, h, body)
	if status == http.StatusNotFound {
		return errRepoNotFound(doing, subject)
	}
	return classifyGithubResponse(doing, r)
}

func errRepoNotFound(doing, repo string) *Error {
	e := &Error{
		Kind:  ErrRepoNotFound,
		Title: fmt.Sprintf("GitHub returned 404 for repository %s", repo),
		Doing: doing,
		Facts: []string{
			"GitHub returns 404 both when a repository does not exist and when it is private and the credentials cannot see it; it does not say which.",
			tokenFact(),
		},
		Next: []string{fmt.Sprintf("Check the spelling: https://github.com/%s should open in a browser.", repo)},
	}
	if hasToken() {
		e.Next = append(e.Next, "If it is private, make sure the token can read it (classic token: `repo` scope; fine-grained token: select this repository and grant Contents: Read) and is SSO-authorized for the organization.")
	} else {
		e.Next = append(e.Next, "If it is private, set GITHUB_TOKEN to a token that can read it: export GITHUB_TOKEN=<token> (or put it in .env).")
	}
	return e
}

// diagnose404 explains a 404 from the contents API. GitHub gives the same 404
// for "no such file/ref" and "repo not visible", so it asks the repository
// endpoint (one extra request, only on failure) before saying which it is.
func diagnose404(repo, ref, dir, name, path string) *Error {
	doing := fmt.Sprintf("Atlas was fetching %s from %s.", path, repo)
	r, _, err := githubGet("https://api.github.com/repos/" + repo)
	if err != nil {
		return &Error{
			Kind:  ErrFileNotFound,
			Title: fmt.Sprintf("GitHub returned 404 for %s in %s", path, repo),
			Doing: doing,
			Facts: []string{
				"404 means the file or ref does not exist, or the repository is private/nonexistent for the credentials used.",
				"Atlas could not tell which: the follow-up repository check failed.",
			},
			Err:  err,
			Next: []string{"Retry; if it persists, open https://github.com/" + repo + " in a browser."},
		}
	}
	switch {
	case r.Status == http.StatusOK:
		at := "on its default branch"
		fact := "The repository is accessible with the credentials used, and GitHub has no such file on its default branch."
		if ref != "" {
			at = fmt.Sprintf("on ref %q", ref)
			fact = "The repository is accessible with the credentials used, but GitHub has no file at that path on that ref: the path or the ref does not exist."
		}
		e := &Error{
			Kind:  ErrFileNotFound,
			Title: fmt.Sprintf("No %s at %q in %s %s", name, path, repo, at),
			Doing: doing + " Without it Atlas cannot build the dependency graph.",
			Facts: []string{fact},
			Maybe: []string{"The project uses yarn.lock or pnpm-lock.yaml, which Atlas does not read.", "The project does not commit a lockfile."},
			Next: []string{
				"If the lockfile lives in a subdirectory (monorepo), pass its directory: -dir <path>.",
				"If you passed -ref, check that the branch, tag or SHA exists in the repository.",
				"To analyze a project without a committed package-lock.json, run `npm install --package-lock-only` in a local clone and pass it with -file ./package-lock.json.",
			},
		}
		if dir != "" {
			e.Next[0] = fmt.Sprintf("Check -dir %q: the lockfile must be directly inside that directory.", dir)
		}
		return e
	case r.Status == http.StatusNotFound:
		return errRepoNotFound(doing, repo)
	}
	return classifyGithubResponse(doing, r)
}

// ---------- token validation ----------

// CheckGithubToken verifies that GitHub accepts tok. It calls GET /rate_limit:
// that endpoint needs no particular scope, works for classic, fine-grained and
// app tokens, returns 401 for rejected credentials, and does not count against
// the REST rate limit.
//
// Validity is all this establishes. It says nothing about access to any
// particular repository; that is determined when the repository is fetched.
//
// blocking is set for definite failures (rejected token, GitHub unreachable).
// warning is set when the token could not be verified but nothing proves it
// bad (rate limited, 5xx); analysis may proceed.
func CheckGithubToken(tok string) (blocking, warning *Error) {
	doing := "Atlas checks GITHUB_TOKEN with GitHub before starting; with a rejected token every GitHub request fails, so it can't continue."
	req, err := http.NewRequest("GET", "https://api.github.com/rate_limit", nil)
	if err != nil {
		return &Error{Kind: ErrConfig, Title: "Could not build the GitHub token check request", Err: err}, nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return errGithubNetwork("Atlas checks GITHUB_TOKEN with GitHub before starting, but could not reach GitHub.", err), nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	r := newGhResponse(resp.StatusCode, resp.Header, body)

	switch {
	case r.Status == http.StatusOK:
		return nil, nil
	case r.Status == http.StatusUnauthorized:
		return errGithubCredentials(doing, r), nil
	}
	w := classifyGithubResponse("Atlas could not verify GITHUB_TOKEN with GitHub; it will continue, and any real problem will be reported when it is used.", r)
	w.Title = "Could not verify GITHUB_TOKEN: " + strings.TrimPrefix(w.Title, "GitHub ")
	return nil, w
}
