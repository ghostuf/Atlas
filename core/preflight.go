package core

// Startup validation.
//
// Atlas validates what the requested operation needs before it does any work.
// Checks that don't depend on each other are run independently (GitHub and
// Ollama concurrently), so one failure can never hide another, and every
// failure that was established is reported together.
//
// Reporting order is fixed, whatever order the checks finish in:
//
//  1. configuration  (missing GITHUB_TOKEN when required, empty model name,
//     malformed OLLAMA_HOST)
//  2. GitHub         (is GITHUB_TOKEN accepted by GitHub?)
//  3. Ollama         (reachable? then: is the model installed?)
//
// Within Ollama the model is only looked for after the server answered, so a
// "model missing" error is never reported for an unreachable server.
//
// Preflight establishes token validity only. Whether the token can read a
// particular repository is decided when that repository is fetched.

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
)

// PreflightOptions says which prerequisites the requested operation needs.
type PreflightOptions struct {
	// RequireGithubToken: a token is mandatory (interactive mode). When
	// false, a missing token is fine (public repositories work without one).
	RequireGithubToken bool
	// CheckGithub: GitHub will be called, so validate a token if one is set.
	CheckGithub bool
	// CheckOllama: the operation uses a local model.
	CheckOllama bool
	Model       string
}

// PreflightResult separates failures that must stop the run from warnings.
type PreflightResult struct {
	Blocking []*Error // in the documented order
	Warnings []*Error
}

// PreflightHeader introduces a multi-error report.
const PreflightHeader = "Atlas can't start: several prerequisites failed. Each was checked independently, in this order: configuration, GitHub, Ollama."

// Preflight validates the prerequisites in opts.
func Preflight(opts PreflightOptions) PreflightResult {
	var res PreflightResult
	var cfg []*Error

	tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	runGithub := false
	switch {
	case tok == "" && opts.RequireGithubToken:
		cfg = append(cfg, errMissingToken())
	case tok != "" && (opts.CheckGithub || opts.RequireGithubToken):
		runGithub = true
	}

	runOllama := false
	if opts.CheckOllama {
		if e := validateOllamaConfig(opts.Model); e != nil {
			cfg = append(cfg, e)
		} else {
			runOllama = true
		}
	}

	var ghBlock, ghWarn, ollama *Error
	var wg sync.WaitGroup
	if runGithub {
		wg.Add(1)
		go func() { defer wg.Done(); ghBlock, ghWarn = CheckGithubToken(tok) }()
	}
	if runOllama {
		wg.Add(1)
		go func() { defer wg.Done(); ollama = CheckOllamaModel(opts.Model) }()
	}
	wg.Wait()

	res.Blocking = append(res.Blocking, cfg...)
	if ghBlock != nil {
		res.Blocking = append(res.Blocking, ghBlock)
	}
	if ollama != nil {
		res.Blocking = append(res.Blocking, ollama)
	}
	if ghWarn != nil {
		res.Warnings = append(res.Warnings, ghWarn)
	}
	return res
}

func errMissingToken() *Error {
	return &Error{
		Kind:  ErrConfig,
		Title: "GITHUB_TOKEN is not set",
		Doing: "Interactive mode needs a token to read private repositories, to create issues, and to avoid GitHub's 60-requests-per-hour limit for unauthenticated requests.",
		Next: []string{
			"Set it: export GITHUB_TOKEN=<token> (or add GITHUB_TOKEN=<token> to .env), then run Atlas again.",
			"For a public repository, one-shot mode works without a token: go run . -repo owner/name (health checks are then subject to the lower limit).",
		},
	}
}

func validateOllamaConfig(model string) *Error {
	if strings.TrimSpace(model) == "" {
		return &Error{
			Kind:  ErrConfig,
			Title: "No Ollama model is configured",
			Doing: "Atlas runs its analysis agents on an Ollama model.",
			Next:  []string{"Pass one with -model <name> (default: gemma4:e4b)."},
		}
	}
	u, err := url.Parse(OllamaBaseURL())
	if err != nil || u.Host == "" {
		return &Error{
			Kind:  ErrConfig,
			Title: fmt.Sprintf("OLLAMA_HOST is not a valid address (%q)", os.Getenv("OLLAMA_HOST")),
			Doing: "Atlas connects to Ollama at the address in OLLAMA_HOST (default http://localhost:11434).",
			Next:  []string{"Set it to host:port or a full URL, e.g. export OLLAMA_HOST=localhost:11434, or unset it."},
		}
	}
	return nil
}
