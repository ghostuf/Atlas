package core

// Minimal client for a local Ollama instance (https://ollama.com), used to
// drive the interactive tool-calling agent and specialist analysis agents.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		return fmt.Errorf("%s: %s: %s", url, http.StatusText(resp.StatusCode), strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
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
		return OllamaMessage{}, err
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
		return "", err
	}
	return resp.Message.Content, nil
}

// ---------- availability check ----------

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// CheckOllamaModel confirms Ollama is reachable and the requested model is pulled.
func CheckOllamaModel(model string) error {
	body, status, err := HttpGet(OllamaBaseURL()+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("could not reach Ollama at %s (is `ollama serve` running?): %w", OllamaBaseURL(), err)
	}
	if status != 200 {
		return fmt.Errorf("ollama /api/tags returned %s", http.StatusText(status))
	}
	var tags ollamaTagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		return fmt.Errorf("parse ollama tags: %w", err)
	}
	for _, m := range tags.Models {
		if m.Name == model || strings.TrimSuffix(m.Name, ":latest") == model {
			return nil
		}
	}
	return fmt.Errorf("model %q not found in Ollama (try: ollama pull %s)", model, model)
}
