package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// FetchFile retrieves a file from GitHub API.
func FetchFile(repo, ref, dir, name string) ([]byte, error) {
	if ref == "" {
		ref = "HEAD" // raw.githubusercontent.com resolves HEAD to the default branch
	}
	path := name
	if dir != "" {
		path = strings.Trim(dir, "/") + "/" + path
	}
	owner, rname, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || rname == "" {
		return nil, fmt.Errorf("invalid repo spec %q (want owner/name)", repo)
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", owner, rname, path, ref)

	req, _ := http.NewRequest("GET", url, nil)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s (does the repo have a committed %q at that path?)%s", url, resp.Status, name, GithubAccessHint(resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ghFile struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(body, &ghFile); err != nil {
		return nil, fmt.Errorf("parse github contents response: %w", err)
	}
	if ghFile.Content == "" {
		return nil, fmt.Errorf("GET %s: %s (no content returned)", url, resp.Status)
	}
	switch ghFile.Encoding {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(ghFile.Content)
		if err != nil {
			return nil, fmt.Errorf("decode base64 content: %w", err)
		}
		return data, nil
	default:
		return nil, fmt.Errorf("unsupported github content encoding %q", ghFile.Encoding)
	}
}

// GithubAccessHint explains the most common reason a GitHub API call comes
// back 404/401/403: the repo is private and GITHUB_TOKEN either isn't set or
// doesn't have the "repo" scope needed to read it.
func GithubAccessHint(status int) string {
	if status != 404 && status != 401 && status != 403 {
		return ""
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return " -- if this is a private repo, set GITHUB_TOKEN (a token with repo scope) and retry"
	}
	return " -- if this is a private repo, your GITHUB_TOKEN likely lacks access: update its scope to include \"repo\" in GitHub settings (Settings > Developer settings > Personal access tokens) and retry"
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
		msg := string(out)
		// git can echo the clone URL (and therefore the embedded token) in
		// its error output; never let the token reach the terminal or logs.
		if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			msg = strings.ReplaceAll(msg, tok, "***")
		}
		hint := ""
		if strings.Contains(msg, "not found") || strings.Contains(msg, "Authentication failed") || strings.Contains(msg, "403") {
			if os.Getenv("GITHUB_TOKEN") == "" {
				hint = " -- if this is a private repo, set GITHUB_TOKEN (a token with repo scope) and retry"
			} else {
				hint = " -- if this is a private repo, your GITHUB_TOKEN likely lacks access: update its scope to include \"repo\" in GitHub settings (Settings > Developer settings > Personal access tokens) and retry"
			}
		}
		return "", nil, fmt.Errorf("git clone failed: %v\n%s%s", err, msg, hint)
	}
	return tmp, cleanup, nil
}
