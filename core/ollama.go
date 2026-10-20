package core

// Minimal client for a local Ollama instance (https://ollama.com), used to
// drive the interactive tool-calling agent and specialist analysis agents.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// OllamaBaseURL returns the configured or default Ollama host.
func OllamaBaseURL() string {
	h := os.Getenv("OLLAMA_HOST")
	if h == "" {
		return "http://localhost:11434"
	}
	if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
		h = "http://" + h
	}
	return strings.TrimSuffix(h, "/")
}

func httpPostJSON(url string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 180 * time.Second} // local inference can be slow
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return &HTTPStatusError{URL: url, Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return json.Unmarshal(body, out)
}

// HTTPStatusError is a non-200 answer from Ollama, kept structured so the
// caller can classify it instead of pattern-matching on message text.
type HTTPStatusError struct {
	URL    string
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.URL, http.StatusText(e.Status), e.Body)
}

// ---------- chat + tool calling ----------

type OllamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type OllamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []OllamaToolCall `json:"tool_calls,omitempty"`
}

type OllamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// NewTool creates an OllamaTool definition.
func NewTool(name, description string, parameters map[string]any) OllamaTool {
	var t OllamaTool
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = description
	t.Function.Parameters = parameters
	return t
}

type OllamaOptions struct {
	NumCtx     int `json:"num_ctx,omitempty"`
	NumPredict int `json:"num_predict,omitempty"`
}

type OllamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Tools    []OllamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Format   any             `json:"format,omitempty"` // json schema or "json"
	Options  OllamaOptions   `json:"options"`
}

type OllamaChatResponse struct {
	Message OllamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

const (
	OllamaNumCtx     = 16384
	OllamaNumPredict = 4096
)

// ChatOllama sends a chat request with optional tools to Ollama.
func ChatOllama(model string, messages []OllamaMessage, tools []OllamaTool) (OllamaMessage, error) {
	req := OllamaChatRequest{
		Model: model, Messages: messages, Tools: tools, Stream: false,
		Options: OllamaOptions{NumCtx: OllamaNumCtx, NumPredict: OllamaNumPredict},
	}
	var resp OllamaChatResponse
	if err := httpPostJSON(OllamaBaseURL()+"/api/chat", req, &resp); err != nil {
		return OllamaMessage{}, classifyOllamaChatErr(model, err)
	}
	return resp.Message, nil
}

// ChatOllamaJSON sends a prompt and requests structured JSON output constrained by formatSchema.
func ChatOllamaJSON(model string, messages []OllamaMessage, formatSchema any) (string, error) {
	req := OllamaChatRequest{
		Model: model, Messages: messages, Stream: false,
		Format:  formatSchema,
		Options: OllamaOptions{NumCtx: OllamaNumCtx, NumPredict: OllamaNumPredict},
	}
	var resp OllamaChatResponse
	if err := httpPostJSON(OllamaBaseURL()+"/api/chat", req, &resp); err != nil {
		return "", classifyOllamaChatErr(model, err)
	}
	return resp.Message.Content, nil
}

// ---------- availability check ----------

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// CheckOllamaModel confirms Ollama is reachable and the requested model is
// pulled. A missing model is only reported once the server has answered with
// its model list, so it is never claimed for an unreachable server.
func CheckOllamaModel(model string) *Error {
	base := OllamaBaseURL()
	doing := fmt.Sprintf("Atlas runs its analysis agents on the Ollama model %q at %s.", model, base)
	body, status, err := HttpGet(base+"/api/tags", nil)
	if err != nil {
		return errOllamaUnreachable(base, doing, err)
	}
	if status != 200 {
		return &Error{
			Kind:  ErrOllamaUnexpected,
			Title: fmt.Sprintf("Ollama at %s answered GET /api/tags with HTTP %d", base, status),
			Doing: doing,
			Facts: []string{"Something answered at that address, so the connection works; the reply was not a successful Ollama model listing."},
			Maybe: []string{"The address is not an Ollama server (wrong port or OLLAMA_HOST), or a proxy sits in between."},
			Next:  []string{fmt.Sprintf("Check it by hand: curl %s/api/tags", base)},
		}
	}
	var tags ollamaTagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		return &Error{
			Kind:  ErrOllamaUnexpected,
			Title: fmt.Sprintf("Ollama at %s returned a model list Atlas could not parse", base),
			Doing: doing,
			Maybe: []string{"The address is not an Ollama server, or it runs an incompatible version."},
			Next:  []string{fmt.Sprintf("Check it by hand: curl %s/api/tags", base)},
			Err:   err,
		}
	}
	var installed []string
	for _, m := range tags.Models {
		if m.Name == model || strings.TrimSuffix(m.Name, ":latest") == model {
			return nil
		}
		installed = append(installed, m.Name)
	}
	e := &Error{
		Kind:  ErrOllamaModelMissing,
		Title: fmt.Sprintf("Model %q is not installed in Ollama", model),
		Doing: doing,
		Facts: []string{fmt.Sprintf("Ollama is reachable at %s and does not list %q.", base, model)},
	}
	if len(installed) == 0 {
		e.Facts = append(e.Facts, "No models are installed.")
	} else {
		if len(installed) > 10 {
			installed = append(installed[:10], "…")
		}
		e.Facts = append(e.Facts, "Installed models: "+strings.Join(installed, ", ")+".")
	}
	pull := "ollama pull " + model
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		pull = fmt.Sprintf("OLLAMA_HOST=%s ollama pull %s", h, model)
	}
	e.Next = []string{"Install it: " + pull, "Or use a model you already have: go run . -model <name>"}
	return e
}

func errOllamaUnreachable(base, doing string, err error) *Error {
	e := &Error{
		Kind:  ErrOllamaUnreachable,
		Title: "Cannot connect to Ollama at " + base,
		Doing: doing,
		Err:   err,
	}
	switch classifyNet(err) {
	case netRefused:
		e.Facts = []string{"The connection was refused: nothing is accepting connections on that address."}
	case netTimeout:
		e.Facts = []string{"The connection timed out: nothing answered on that address."}
	case netDNS:
		e.Facts = []string{"The host name in that address could not be resolved."}
	}
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		e.Next = []string{
			fmt.Sprintf("OLLAMA_HOST is set to %q. Check that Ollama is running there, or unset it to use http://localhost:11434.", h),
		}
		if u, perr := url.Parse(base); perr == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") {
			e.Next = append(e.Next, "If Ollama is installed on this machine, start it: ollama serve (or open the Ollama app).")
		}
	} else {
		e.Next = []string{
			"Start Ollama: ollama serve (or open the Ollama app).",
			"If Ollama runs on another host or port, set OLLAMA_HOST, e.g. export OLLAMA_HOST=host:11434.",
		}
	}
	return e
}

// classifyOllamaChatErr turns a failed chat request into an *Error. It is
// used mid-session, where the startup check can no longer protect the user
// (Ollama stopped, or the model was removed).
func classifyOllamaChatErr(model string, err error) error {
	base := OllamaBaseURL()
	doing := fmt.Sprintf("Atlas was sending a request to the Ollama model %q at %s.", model, base)
	var se *HTTPStatusError
	if errors.As(err, &se) {
		msg := se.Body
		var j struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(se.Body), &j) == nil && j.Error != "" {
			msg = j.Error
		}
		low := strings.ToLower(msg)
		if se.Status == http.StatusNotFound && strings.Contains(low, "model") && strings.Contains(low, "not found") {
			return &Error{
				Kind:  ErrOllamaModelMissing,
				Title: fmt.Sprintf("Ollama reports that model %q is not installed", model),
				Doing: doing,
				Facts: []string{fmt.Sprintf("Ollama answered HTTP 404: %s", msg)},
				Next:  []string{"Install it: ollama pull " + model},
			}
		}
		return &Error{
			Kind:  ErrOllamaUnexpected,
			Title: fmt.Sprintf("Ollama returned HTTP %d for the request", se.Status),
			Doing: doing,
			Facts: []string{"Ollama said: " + msg},
			Next:  []string{"Check the Ollama server log for details, then retry."},
		}
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{
			Kind:  ErrOllamaUnreachable,
			Title: fmt.Sprintf("Ollama at %s did not answer within 180s", base),
			Doing: doing,
			Facts: []string{"The request timed out; no response was received."},
			Maybe: []string{"The model is still loading, or the machine lacks the memory/compute to run it quickly."},
			Next:  []string{"Check `ollama ps` and the server log, or try a smaller model with -model <name>."},
			Err:   err,
		}
	}
	if classifyNet(err) != netOther || strings.Contains(err.Error(), "dial tcp") {
		return errOllamaUnreachable(base, doing, err)
	}
	return &Error{Kind: ErrOllamaUnexpected, Title: "Request to Ollama failed", Doing: doing, Err: err}
}
