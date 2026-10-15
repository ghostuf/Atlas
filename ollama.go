package main

// Minimal client for a local Ollama instance (https://ollama.com), used to
// drive the interactive tool-calling agent. Stdlib only, no SDK.

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

func ollamaBaseURL() string {
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

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func newTool(name, description string, parameters map[string]any) ollamaTool {
	var t ollamaTool
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = description
	t.Function.Parameters = parameters
	return t
}

type ollamaOptions struct {
	NumCtx     int `json:"num_ctx,omitempty"`
	NumPredict int `json:"num_predict,omitempty"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

// Without an explicit num_ctx, Ollama falls back to a small default context
// window regardless of what the model actually supports — with a system
// prompt plus large tool-result JSON (full advisory lists, graph tables),
// that's easily enough to silently overflow and cut the model's own answer
// short mid-sentence. 16384 comfortably fits a typical Atlas session on
// every model this agent supports (qwen3:8b natively supports 40960,
// gemma4:e4b 128k) without being wastefully large.
const (
	ollamaNumCtx     = 16384
	ollamaNumPredict = 4096
)

func chatOllama(model string, messages []ollamaMessage, tools []ollamaTool) (ollamaMessage, error) {
	req := ollamaChatRequest{
		Model: model, Messages: messages, Tools: tools, Stream: false,
		Options: ollamaOptions{NumCtx: ollamaNumCtx, NumPredict: ollamaNumPredict},
	}
	var resp ollamaChatResponse
	if err := httpPostJSON(ollamaBaseURL()+"/api/chat", req, &resp); err != nil {
		return ollamaMessage{}, err
	}
	return resp.Message, nil
}

// ---------- availability check ----------

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// checkOllamaModel confirms Ollama is reachable and the requested model is pulled.
func checkOllamaModel(model string) error {
	body, status, err := httpGet(ollamaBaseURL()+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("could not reach Ollama at %s (is `ollama serve` running?): %w", ollamaBaseURL(), err)
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
