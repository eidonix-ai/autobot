package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Message is one message of a conversation, in the OpenAI chat format our providers speak.
type Message struct {
	Role       string     `json:"role"` // system, user, assistant or tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the model asking for a tool.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON
	} `json:"function"`
}

// Usage is what one model call cost.
type Usage struct {
	In  int `json:"prompt_tokens"`
	Out int `json:"completion_tokens"`
}

// Model answers a conversation, perhaps asking for tools.
type Model interface {
	Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error)
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // a JSON Schema object
}

// OpenAI is a model behind an OpenAI-compatible chat completions API.
type OpenAI struct {
	Endpoint, Key, Model string
	HTTP                 *http.Client
}

var chatHTTP = &http.Client{
	Timeout:       10 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Complete asks for the next message. Errors never contain the key.
func (o *OpenAI) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	type fn struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type tool struct {
		Type     string `json:"type"`
		Function fn     `json:"function"`
	}
	var ts []tool
	for _, t := range tools {
		ts = append(ts, tool{"function", fn{t.Name, t.Description, t.Parameters}})
	}
	body, err := json.Marshal(map[string]any{"model": o.Model, "messages": msgs, "tools": ts})
	if err != nil {
		return Message{}, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.Endpoint, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, errors.New("the provider's endpoint is not a usable web address")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "autobot")
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	hc := o.HTTP
	if hc == nil {
		hc = chatHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Message{}, Usage{}, fmt.Errorf("could not reach %s: %v", req.URL.Host, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Message{}, Usage{}, fmt.Errorf("reading the answer from %s: %v", req.URL.Host, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(b))
		if o.Key != "" {
			msg = strings.ReplaceAll(msg, o.Key, "…")
		}
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return Message{}, Usage{}, fmt.Errorf("%s answered %s: %s", req.URL.Host, resp.Status, msg)
	}
	var r struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("%s answered with something other than a chat completion", req.URL.Host)
	}
	m := r.Choices[0].Message
	m.Role = "assistant"
	return m, r.Usage, nil
}
