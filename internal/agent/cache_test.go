package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/eidonix-ai/autobot/internal/netguard"
	"github.com/eidonix-ai/autobot/internal/store"
)

func init() { netguard.SetTrusted([]string{"127.0.0.1"}) }

// chatServer answers chat completions with the replies given, in order, and keeps the raw request
// bodies. usage, when set, is the usage object it answers with.
type chatServer struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []string
	replies []Message
	usage   string
}

func newChatServer(t *testing.T, replies ...Message) *chatServer {
	cs := &chatServer{replies: replies, usage: `{"prompt_tokens":100,"completion_tokens":5}`}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		defer cs.mu.Unlock()
		cs.bodies = append(cs.bodies, string(b))
		if len(cs.bodies) > len(cs.replies) {
			http.Error(w, "the script ran out", http.StatusInternalServerError)
			return
		}
		reply, _ := json.Marshal(cs.replies[len(cs.bodies)-1])
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":%s}`, reply, cs.usage)
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *chatServer) model(session string) *OpenAI {
	return &OpenAI{Endpoint: cs.URL, Key: "k", Model: "m", Session: session}
}

// continues reports whether a request is the previous one with messages added at the end.
func continues(prev, next string) bool {
	return strings.HasPrefix(next, strings.TrimSuffix(prev, "]}"))
}

func bigTool(name string, size int) Tool {
	return Tool{Spec: ToolSpec{Name: name, Description: "reads " + name, Parameters: schema(`{"type":"object","properties":{}}`)},
		Run: func(context.Context, json.RawMessage) (string, error) {
			return "first line\n" + strings.Repeat("y", size), nil
		}}
}

// Over ten turns a request is always the previous request with messages added, except at a batch,
// where the old tool results are shortened for good: the two together cost one cache miss, not one a
// turn.
func TestRequestsOnlyGrow(t *testing.T) {
	var replies []Message
	for i := 0; i < 9; i++ {
		replies = append(replies, call(fmt.Sprint("c", i), "long", `{}`))
	}
	replies = append(replies, call("f", "finish", `{"status":"met","report":"done"}`))
	cs := newChatServer(t, replies...)
	conv := NewConversation("m", 1, "system prompt", []Tool{bigTool("long", 10_000)})
	var events []store.ConvEvent
	var batchAt []int // the request each batch came before
	cfg := Config{Model: cs.model(conv.Session), Tools: []Tool{bigTool("long", 10_000)}, MaxTurns: 20, BatchTokens: 16_000, Task: "go",
		Save: func(*Transcript) {
			for _, e := range conv.Take() {
				events = append(events, e)
				if e.T == "trim" {
					batchAt = append(batchAt, len(cs.bodies)-1) // saved after the turn whose request it changed
				}
			}
		}}
	tr := Run(context.Background(), cfg, conv)
	if tr.Outcome != Met || len(cs.bodies) != 10 {
		t.Fatalf("%s after %d requests: %s", tr.Outcome, len(cs.bodies), tr.Error)
	}
	var broke []int
	for i := 1; i < len(cs.bodies); i++ {
		if !continues(cs.bodies[i-1], cs.bodies[i]) {
			broke = append(broke, i)
		}
	}
	if len(broke) == 0 || len(broke) > 2 {
		t.Fatalf("%d batches in 10 turns, want one or two: %v", len(broke), broke)
	}
	if !reflect.DeepEqual(broke, batchAt) {
		t.Fatalf("requests changed their start at %v, batches were recorded at %v", broke, batchAt)
	}
	// Shortened for good: the last request has the first tool result as a stub, and every request
	// after a batch keeps what the batch cut.
	last := cs.bodies[len(cs.bodies)-1]
	if !strings.Contains(last, "left out to save room") || strings.Count(last, strings.Repeat("y", 10_000)) != keepTurns {
		t.Fatalf("the last request has %d full results", strings.Count(last, strings.Repeat("y", 10_000)))
	}
	// A conversation read back from its events is the one that was sent.
	events = append([]store.ConvEvent{events[0]}, events[1:]...)
	back, err := LoadConversation(events)
	if err != nil || !reflect.DeepEqual(back.Messages, conv.Messages) || back.Turns != 10 || back.Session != conv.Session {
		t.Fatalf("replay: %v, %d turns", err, back.Turns)
	}
}

// With no way to cut anything (every result is in the last six turns) nothing is rewritten and no
// batch is recorded.
func TestNothingToShortenNothingChanges(t *testing.T) {
	replies := []Message{call("1", "long", `{}`), call("2", "long", `{}`), call("f", "finish", `{"status":"met","report":"done"}`)}
	cs := newChatServer(t, replies...)
	conv := NewConversation("m", 1, "s", []Tool{bigTool("long", 10_000)})
	Run(context.Background(), Config{Model: cs.model(""), Tools: []Tool{bigTool("long", 10_000)}, MaxTurns: 5, BatchTokens: 100, Task: "go"}, conv)
	for _, e := range conv.Take() {
		if e.T == "trim" {
			t.Fatal("a batch was recorded with nothing to cut")
		}
	}
	for i := 1; i < len(cs.bodies); i++ {
		if !continues(cs.bodies[i-1], cs.bodies[i]) {
			t.Fatalf("request %d does not start with the one before", i)
		}
	}
}

// A resumed conversation sends the system prompt and tool definitions it was started with, byte for
// byte, after the messages it had; a tool that cannot run now still has its definition and answers
// that it is not available.
func TestResumeSendsTheStoredStart(t *testing.T) {
	first := newChatServer(t, call("a", "alpha", `{}`), call("b", "beta", `{}`), call("f", "finish", `{"status":"met","report":"one"}`))
	alpha, beta := bigTool("alpha", 50), bigTool("beta", 50)
	conv := NewConversation("m", 1, "the first system prompt", []Tool{alpha, beta})
	var events []store.ConvEvent
	cfg := Config{Model: first.model(conv.Session), Tools: []Tool{alpha, beta}, MaxTurns: 9, Task: "first task",
		Save: func(*Transcript) { events = append(events, conv.Take()...) }}
	Run(context.Background(), cfg, conv)
	events = append(events, conv.Take()...)

	// A later run: read back from the file, with another system prompt and without beta.
	loaded, err := LoadConversation(events)
	if err != nil {
		t.Fatal(err)
	}
	second := newChatServer(t, call("b2", "beta", `{}`), call("f2", "finish", `{"status":"met","report":"two"}`))
	var events2 []store.ConvEvent
	cfg2 := Config{Model: second.model(loaded.Session), System: "a different system prompt", Tools: []Tool{alpha}, MaxTurns: 9, Task: "second task",
		Save: func(*Transcript) { events2 = append(events2, loaded.Take()...) }}
	Run(context.Background(), cfg2, loaded)

	lastFirst, firstSecond, secondTwo := first.bodies[len(first.bodies)-1], second.bodies[0], second.bodies[1]
	if !continues(lastFirst, firstSecond) {
		t.Fatalf("the resumed conversation does not start with what was sent before:\n%.300s\n%.300s", lastFirst, firstSecond)
	}
	head := func(body string) string { return body[:strings.Index(body, `"messages"`)] }
	if head(lastFirst) != head(firstSecond) || !strings.Contains(head(firstSecond), `"name":"beta"`) {
		t.Fatalf("the tool definitions changed:\n%s\n%s", head(lastFirst), head(firstSecond))
	}
	if !strings.Contains(firstSecond, "the first system prompt") || strings.Contains(firstSecond, "different system prompt") || !strings.Contains(firstSecond, "second task") {
		t.Fatal("the stored system prompt was not kept")
	}
	if !strings.Contains(secondTwo, notAvailable) {
		t.Fatalf("beta did not answer that it is not available:\n%.500s", secondTwo)
	}
	// Its first request is the old one, the new instruction, and nothing else.
	var req struct{ Messages []Message }
	json.Unmarshal([]byte(firstSecond), &req)
	if n := len(loaded.Messages); req.Messages[len(req.Messages)-1].Content != "second task" || n == 0 {
		t.Fatalf("%d messages", n)
	}
}

// Every request of a conversation carries the same session key in the user field; none is sent
// without one.
func TestSessionKey(t *testing.T) {
	cs := newChatServer(t, call("1", "long", `{}`), call("f", "finish", `{"status":"met","report":"done"}`))
	conv := NewConversation("m", 1, "s", []Tool{bigTool("long", 10)})
	if !strings.HasPrefix(conv.Session, "autobot-") || len(conv.Session) < 20 || conv.Session == NewConversation("m", 1, "s", nil).Session {
		t.Fatalf("session key %q", conv.Session)
	}
	Run(context.Background(), Config{Model: cs.model(conv.Session), Tools: []Tool{bigTool("long", 10)}, MaxTurns: 4, Task: "go"}, conv)
	for i, b := range cs.bodies {
		var req struct{ User string }
		json.Unmarshal([]byte(b), &req)
		if req.User != conv.Session {
			t.Fatalf("request %d has user %q", i, req.User)
		}
	}
	none := newChatServer(t, call("f", "finish", `{"status":"met","report":"done"}`))
	Run(context.Background(), Config{Model: none.model(""), MaxTurns: 4, Task: "go"}, nil)
	var keys map[string]json.RawMessage
	json.Unmarshal([]byte(none.bodies[0]), &keys)
	if _, ok := keys["user"]; ok || keys["model"] == nil {
		t.Fatalf("a user field was sent with no session: %.200s", none.bodies[0])
	}
}

// Cached input is read from either place providers report it.
func TestUsageCachedTokens(t *testing.T) {
	for name, tc := range map[string]struct {
		usage  string
		cached int
	}{
		"OpenAI style":             {`{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":800}}`, 800},
		"DeepSeek style":           {`{"prompt_tokens":1000,"completion_tokens":20,"prompt_cache_hit_tokens":640,"prompt_cache_miss_tokens":360}`, 640},
		"neither":                  {`{"prompt_tokens":1000,"completion_tokens":20}`, 0},
		"never more than was sent": {`{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":1200}}`, 1000},
	} {
		cs := newChatServer(t, Message{Role: "assistant", Content: "hi"})
		cs.usage = tc.usage
		_, u, err := cs.model("").Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
		if err != nil || u.In != 1000 || u.Out != 20 || u.Cached != tc.cached {
			t.Errorf("%s: %+v %v", name, u, err)
		}
	}
	// The run adds it up, and the conversation's file records each turn.
	cs := newChatServer(t, call("1", "long", `{}`), call("f", "finish", `{"status":"met","report":"done"}`))
	cs.usage = `{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":60}`
	conv := NewConversation("m", 1, "s", []Tool{bigTool("long", 10)})
	tr := Run(context.Background(), Config{Model: cs.model(""), Tools: []Tool{bigTool("long", 10)}, MaxTurns: 4, Task: "go", RunID: 7}, conv)
	turns := 0
	for _, e := range conv.Take() {
		if e.T == "turn" && e.Cached == 60 && e.In == 100 && e.Run == 7 {
			turns++
		}
	}
	if tr.Cached != 120 || tr.In != 200 || turns != 2 {
		t.Fatalf("cached %d of %d, %d turns recorded", tr.Cached, tr.In, turns)
	}
}
