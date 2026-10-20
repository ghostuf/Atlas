package core

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// FetchFile retrieves a file from GitHub API. Failures are returned as
// *Error values that say what GitHub actually established (see github.go).
func FetchFile(repo, ref, dir, name string) ([]byte, error) {
	if ref == "" {
		ref = "HEAD" // resolves to the default branch
	}
	userRef := ref
	if ref == "HEAD" {
		userRef = ""
	}
	path := name
	if dir != "" {
		path = strings.Trim(dir, "/") + "/" + path
	}
	owner, rname, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || rname == "" || strings.Contains(rname, "/") {
		return nil, &Error{
			Kind:  ErrConfig,
			Title: fmt.Sprintf("Invalid repository %q", repo),
			Doing: "Atlas expects a GitHub repository written as owner/name.",
			Next:  []string{"Use the form owner/name, e.g. -repo expressjs/express."},
		}
	}
	doing := fmt.Sprintf("Atlas was fetching %s from %s.", path, repo)
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", owner, rname, path, ref)

	r, body, err := githubGet(url)
	if err != nil {
		return nil, errGithubNetwork(doing, err)
	}
	switch {
	case r.Status == http.StatusOK:
	case r.Status == http.StatusNotFound:
		return nil, diagnose404(repo, userRef, dir, name, path)
	default:
		return nil, classifyGithubResponse(doing, r)
	}

	var ghFile struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
	}
	if err := json.Unmarshal(body, &ghFile); err != nil {
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			return nil, &Error{
				Kind:  ErrFileUnreadable,
				Title: fmt.Sprintf("%q in %s is a directory, not a file", path, repo),
				Doing: doing,
				Facts: []string{"GitHub answered with a directory listing."},
				Next:  []string{"Point -dir at the directory that contains package-lock.json."},
			}
		}
		return nil, &Error{Kind: ErrGithubUnexpected, Title: "GitHub returned a response Atlas could not parse", Doing: doing, Err: err}
	}
	if ghFile.Content == "" {
		e := &Error{
			Kind:  ErrFileUnreadable,
			Doing: doing,
			Facts: []string{fmt.Sprintf("GitHub answered HTTP 200 with encoding %q and no inline content.", ghFile.Encoding)},
		}
		switch {
		case ghFile.Size > 1_000_000:
			e.Title = fmt.Sprintf("GitHub did not return the contents of %s (%d bytes)", path, ghFile.Size)
			e.Facts = append(e.Facts, "GitHub's contents API does not inline files larger than 1 MB.")
			e.Next = []string{"Clone the repository and analyze the local copy: go run . -file ./package-lock.json"}
		case ghFile.Size == 0:
			e.Title = fmt.Sprintf("%s in %s is empty", path, repo)
			e.Next = []string{"Regenerate it with `npm install --package-lock-only` and commit it."}
		default:
			e.Title = fmt.Sprintf("GitHub did not return the contents of %s", path)
		}
		return nil, e
	}
	switch ghFile.Encoding {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(ghFile.Content)
		if err != nil {
			return nil, &Error{Kind: ErrGithubUnexpected, Title: "GitHub returned content Atlas could not decode", Doing: doing, Err: err}
		}
		return data, nil
	default:
		return nil, &Error{
			Kind:  ErrGithubUnexpected,
			Title: fmt.Sprintf("GitHub returned %s with unsupported encoding %q", path, ghFile.Encoding),
			Doing: doing,
		}
	}
}

// CloneRepo shallow-clones github.com/<repo> into a temp dir (needs git installed).
// When GITHUB_TOKEN is set it's embedded in the URL so private repos clone too.
func CloneRepo(repo, ref string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "atlas-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	cloneURL := "https://github.com/" + repo + ".git"
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		cloneURL = "https://x-access-token:" + tok + "@github.com/" + repo + ".git"
	}
	args = append(args, cloneURL, tmp)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		cleanup()
		return "", nil, classifyCloneFailure(repo, ref, err, string(out))
	}
	return tmp, cleanup, nil
}

// classifyCloneFailure turns a failed `git clone` into an *Error. Only what
// git itself reported is stated as fact. The clone URL embeds GITHUB_TOKEN, and
// git may echo it, so all output goes through Redact.
func classifyCloneFailure(repo, ref string, err error, out string) *Error {
	out = Redact(out)
	doing := fmt.Sprintf("Atlas was cloning %s to scan its source for dependency usage.", repo)
	low := strings.ToLower(out)
	e := &Error{Kind: ErrCloneFailed, Doing: doing, Err: err, Detail: lastLines(out, 4)}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		e.Title = "git is not installed or not on PATH"
		e.Facts = []string{"The git executable could not be found."}
		e.Next = []string{"Install git and make sure `git --version` works."}
	case strings.Contains(low, "remote branch") && strings.Contains(low, "not found"):
		e.Title = fmt.Sprintf("Branch or tag %q was not found in %s", ref, repo)
		e.Facts = []string{"git reported that the remote branch does not exist.", "`git clone --branch` accepts a branch or tag name, not a commit SHA."}
		e.Next = []string{"Use an existing branch or tag for -ref."}
	case strings.Contains(low, "authentication failed"), strings.Contains(low, "invalid username or password"),
		strings.Contains(low, "http basic: access denied"), strings.Contains(low, "returned error: 403"):
		e.Title = fmt.Sprintf("git clone of %s was refused (authentication failure)", repo)
		e.Facts = []string{"git reported an authentication or access failure.", tokenFact()}
		e.Next = []string{"Check that GITHUB_TOKEN is valid and can read this repository (classic: `repo` scope; fine-grained: this repository with Contents: Read)."}
	case strings.Contains(low, "repository not found"), strings.Contains(low, "returned error: 404"):
		e.Title = fmt.Sprintf("git could not find repository %s", repo)
		e.Facts = []string{"GitHub reports a repository as not found both when it does not exist and when it is private and the credentials cannot see it.", tokenFact()}
		e.Next = []string{"Check the spelling, and for a private repository that GITHUB_TOKEN can read it."}
	case strings.Contains(low, "could not resolve host"), strings.Contains(low, "failed to connect"),
		strings.Contains(low, "timed out"), strings.Contains(low, "unable to access"):
		e.Title = "git could not reach github.com"
		e.Facts = []string{"git reported a network failure."}
		e.Next = []string{"Check your internet connection and proxy settings, then retry."}
	default:
		e.Title = fmt.Sprintf("git clone of %s failed", repo)
	}
	return e
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
