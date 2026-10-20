package core

// User-facing errors for the Atlas CLI.
//
// Every failure a user can hit routinely (bad token, Ollama down, repo not
// reachable, lockfile missing...) is reported as an *Error so that it reads
// the same way everywhere:
//
//	error: <what failed>
//	  <what Atlas was doing / why it can't continue>
//
//	  Known:                          facts Atlas actually established
//	  Not verified (possible causes): hypotheses, always labelled as such
//	  Next:                           concrete things the user can do
//
//	  Detail: <underlying error, redacted>
//
// Facts and Maybe are kept apart on purpose: only put something in Facts if a
// response, status code or syscall error proved it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// ErrKind classifies an Error so callers and tests can branch on it without
// parsing message text.
type ErrKind string

const (
	ErrConfig             ErrKind = "config"
	ErrGithubCredentials  ErrKind = "github_credentials"   // 401: token invalid/expired/revoked
	ErrGithubForbidden    ErrKind = "github_forbidden"     // 403 that is not a rate limit
	ErrGithubSSO          ErrKind = "github_sso"           // 403 with X-GitHub-SSO
	ErrGithubRateLimit    ErrKind = "github_rate_limit"    // 429, or 403 with an exhausted limit
	ErrGithubNetwork      ErrKind = "github_network"       // could not reach GitHub at all
	ErrGithubUnavailable  ErrKind = "github_unavailable"   // 5xx
	ErrGithubUnexpected   ErrKind = "github_unexpected"    // any other status / unparsable response
	ErrRepoNotFound       ErrKind = "repo_not_found"       // repo 404: missing OR not visible to these credentials
	ErrFileNotFound       ErrKind = "file_not_found"       // repo reachable, no file at path/ref
	ErrFileUnreadable     ErrKind = "file_unreadable"      // directory, empty, too large for the contents API
	ErrLocalFile          ErrKind = "local_file"           // local path missing / unreadable
	ErrLockfileInvalid    ErrKind = "lockfile_invalid"     // not JSON / wrong shape / unsupported version
	ErrCloneFailed        ErrKind = "clone_failed"         // git clone failed (see Title for specifics)
	ErrOllamaUnreachable  ErrKind = "ollama_unreachable"   // could not connect
	ErrOllamaModelMissing ErrKind = "ollama_model_missing" // reachable, model not installed
	ErrOllamaUnexpected   ErrKind = "ollama_unexpected"    // reachable but misbehaving
)

// Error is a user-facing, classified error.
type Error struct {
	Kind   ErrKind
	Title  string   // what failed; one line, no trailing period
	Doing  string   // what Atlas was attempting and why it can't continue
	Facts  []string // established by evidence
	Maybe  []string // unverified possible causes
	Next   []string // concrete next steps
	Detail string   // underlying technical detail (redacted on output)
	Err    error    // wrapped cause
}

func (e *Error) Error() string { return Redact(e.Title) }
func (e *Error) Unwrap() error { return e.Err }

// Short is a one-line form (title plus the first next step) for places that
// show one line per failure, such as per-issue results.
func (e *Error) Short() string {
	s := e.Title
	if len(e.Next) > 0 {
		s += " — " + e.Next[0]
	}
	return Redact(s)
}

// ShortError renders any error on one line (redacted), keeping the first
// next step when the error is a typed *Error.
func ShortError(err error) string {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Short()
	}
	return Redact(err.Error())
}

// Format renders the full multi-line message. Output is always redacted.
func (e *Error) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: %s\n", e.Title)
	if e.Doing != "" {
		fmt.Fprintf(&b, "  %s\n", e.Doing)
	}
	section := func(head string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n  %s\n", head)
		for _, it := range items {
			fmt.Fprintf(&b, "    - %s\n", it)
		}
	}
	section("Known:", e.Facts)
	section("Not verified (possible causes):", e.Maybe)
	section("Next:", e.Next)
	detail := e.Detail
	if detail == "" && e.Err != nil {
		detail = e.Err.Error()
	}
	if detail != "" {
		fmt.Fprintf(&b, "\n  Detail: %s\n", truncate(oneLine(detail), 300))
	}
	return Redact(b.String())
}

// FormatWarning renders e like Format but as a warning.
func FormatWarning(e *Error) string {
	return "warning:" + strings.TrimPrefix(strings.TrimRight(e.Format(), "\n"), "error:")
}

// FormatError renders any error for the terminal. *Error values get the full
// structured form; anything else is a redacted single line.
func FormatError(err error) string {
	if err == nil {
		return ""
	}
	var ue *Error
	if errors.As(err, &ue) {
		return strings.TrimRight(ue.Format(), "\n")
	}
	return "error: " + Redact(err.Error())
}

// FormatErrors renders several independent failures together.
func FormatErrors(errs []*Error, header string) string {
	if len(errs) == 1 {
		return strings.TrimRight(errs[0].Format(), "\n")
	}
	var b strings.Builder
	b.WriteString(header + "\n")
	for i, e := range errs {
		fmt.Fprintf(&b, "\n[%d/%d] %s", i+1, len(errs), e.Format())
	}
	return strings.TrimRight(b.String(), "\n")
}

// Annotate sets the "what Atlas was doing" line on a typed error (or wraps a
// plain error in one), so a low-level failure can explain why it matters to
// the caller's task.
func Annotate(err error, doing string) error {
	if err == nil {
		return nil
	}
	var ue *Error
	if errors.As(err, &ue) {
		cp := *ue
		if ue.Doing != "" {
			doing += " " + ue.Doing
		}
		cp.Doing = doing
		return &cp
	}
	return &Error{Title: Redact(err.Error()), Doing: doing, Err: err}
}

// ---------- redaction ----------

var (
	reFinePAT   = regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)
	reClassic   = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)
	reURLCreds  = regexp.MustCompile(`(https?://)[^/\s:@]+:[^@\s/]+@`)
	reBearerHdr = regexp.MustCompile(`(?i)\b(bearer|token)\s+[A-Za-z0-9_\-\.]{16,}`)
)

// Redact removes credentials from s: the current GITHUB_TOKEN verbatim, common
// GitHub token formats, URL userinfo (x-access-token:<tok>@host) and
// Authorization header values. Every user-facing error passes through it.
func Redact(s string) string {
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); len(tok) >= 6 {
		s = strings.ReplaceAll(s, tok, "***")
	}
	s = reFinePAT.ReplaceAllString(s, "***")
	s = reClassic.ReplaceAllString(s, "***")
	s = reURLCreds.ReplaceAllString(s, "${1}***@")
	s = reBearerHdr.ReplaceAllString(s, "${1} ***")
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------- network failure classification ----------

type netKind int

const (
	netOther netKind = iota
	netRefused
	netTimeout
	netDNS
)

// classifyNet decides, from the error itself, why a request never got a
// response. Only the first three are facts Atlas can state.
func classifyNet(err error) netKind {
	var dns *net.DNSError
	var ne net.Error
	low := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &dns):
		return netDNS
	case errors.Is(err, syscall.ECONNREFUSED),
		strings.Contains(low, "connection refused"),
		strings.Contains(low, "actively refused"):
		return netRefused
	case errors.As(err, &ne) && ne.Timeout():
		return netTimeout
	}
	return netOther
}

// ---------- local files and lockfile shape ----------

// ReadLocalFile reads a local file, translating the common failures.
func ReadLocalFile(path, what string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil
	}
	e := &Error{Kind: ErrLocalFile, Err: err, Detail: err.Error()}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e.Title = fmt.Sprintf("%s not found at %q", what, path)
		e.Facts = []string{"No file exists at that path (relative paths are resolved from the current directory)."}
		e.Next = []string{"Check the path passed to -file, or run Atlas from the project directory."}
	case errors.Is(err, fs.ErrPermission):
		e.Title = fmt.Sprintf("Permission denied reading %s at %q", what, path)
		e.Next = []string{"Check the file's permissions."}
	default:
		e.Title = fmt.Sprintf("Could not read %s at %q", what, path)
	}
	return nil, e
}

// LockfileParseError explains why bytes that should be a package-lock.json
// could not be decoded. source names where the bytes came from.
func LockfileParseError(err error, source string) *Error {
	e := &Error{
		Kind:   ErrLockfileInvalid,
		Doing:  "Atlas reads npm's package-lock.json (lockfileVersion 1, 2 or 3) to build the dependency graph.",
		Err:    err,
		Detail: err.Error(),
		Next:   []string{"Regenerate it with `npm install --package-lock-only` and try again."},
	}
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		e.Title = fmt.Sprintf("%s is not valid JSON (syntax error at byte %d)", source, se.Offset)
		e.Maybe = []string{"The file is truncated or contains merge-conflict markers (<<<<<<<)."}
	case errors.As(err, &te):
		e.Title = fmt.Sprintf("%s has an unexpected structure: %q should be %s but is %s", source, te.Field, te.Type, te.Value)
		e.Facts = []string{"The file is valid JSON but does not match the package-lock.json schema."}
	default:
		e.Title = fmt.Sprintf("%s could not be parsed", source)
	}
	return e
}

// lockfileEmptyError is returned when a lockfile decodes but lists nothing
// Atlas can build a graph from.
func lockfileEmptyError(lf *Lockfile) *Error {
	e := &Error{
		Kind:  ErrLockfileInvalid,
		Doing: "Atlas reads npm's package-lock.json (lockfileVersion 1, 2 or 3) to build the dependency graph.",
		Next:  []string{"Check that this is an npm package-lock.json; Atlas does not read yarn.lock or pnpm-lock.yaml."},
	}
	switch {
	case lf.LockfileVersion == 0:
		e.Title = "The file has no lockfileVersion and no packages, so it does not look like an npm package-lock.json"
	case lf.LockfileVersion > 3:
		e.Title = fmt.Sprintf("lockfileVersion %d is not supported and no packages could be read", lf.LockfileVersion)
		e.Facts = []string{"Atlas supports lockfileVersion 1, 2 and 3."}
	default:
		e.Title = fmt.Sprintf("The lockfile lists no packages (lockfileVersion %d)", lf.LockfileVersion)
		e.Facts = []string{"The project may simply have no dependencies."}
	}
	return e
}
