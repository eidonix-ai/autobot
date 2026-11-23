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

	"github.com/eidonix-ai/autobot/internal/netguard"
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

// Usage is what one model call cost. Cached is how much of the input the provider answered from
// its cache of earlier requests: it reuses work only when a request starts with exactly the bytes
// of an earlier one.
type Usage struct {
	In     int
	Out    int
	Cached int
}

// apiUsage is a chat completion's usage object. Providers report cached input in one of two
// places: OpenAI-style gateways in prompt_tokens_details.cached_tokens, DeepSeek in
// prompt_cache_hit_tokens.
type apiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	Details          struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

func (u apiUsage) usage() Usage {
	return Usage{In: u.PromptTokens, Out: u.CompletionTokens, Cached: min(max(u.Details.CachedTokens, u.CacheHitTokens), u.PromptTokens)}
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
	// Session, when set, is sent as the request's user field, the same for every request of a
	// conversation, so that a gateway can send the conversation to the server that holds its cache.
	Session string
	HTTP    *http.Client
}

// request is the body of a chat completion request. It is a struct, never a map, so that the same
// conversation always serializes to the same bytes, in this order: the parts that stay the same
// (model, session, tools) come first, then the messages, which only ever grow at their end. A request
// is then the previous request with messages added, which is what a provider's cache can reuse.
type request struct {
	Model    string    `json:"model"`
	User     string    `json:"user,omitempty"`
	Tools    []reqTool `json:"tools,omitempty"`
	Messages []Message `json:"messages"`
}

type reqTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

var chatHTTP = &http.Client{
	// Only public addresses, or hosts an admin trusts: the address comes from a person.
	Transport:     netguard.Transport(),
	Timeout:       10 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Complete asks for the next message. Errors never contain the key.
func (o *OpenAI) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	rq := request{Model: o.Model, User: o.Session, Messages: msgs}
	for _, t := range tools {
		rt := reqTool{Type: "function"}
		rt.Function.Name, rt.Function.Description, rt.Function.Parameters = t.Name, t.Description, t.Parameters
		rq.Tools = append(rq.Tools, rt)
	}
	body, err := json.Marshal(rq)
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
		Usage apiUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("%s answered with something other than a chat completion", req.URL.Host)
	}
	m := r.Choices[0].Message
	m.Role = "assistant"
	return m, r.Usage.usage(), nil
}
