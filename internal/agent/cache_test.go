package agent

import (
	"context"
	"encoding/json"
	"errors"
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

// Nothing is trimmed automatically: over a long run, with results far larger than any batch used to
// allow, every request is the previous request with messages added at its end, so the provider's cache
// is never lost; and what a conversation's file replays to is the conversation that was sent.
func TestRequestsOnlyGrow(t *testing.T) {
	var replies []Message
	for i := 0; i < 29; i++ {
		replies = append(replies, call(fmt.Sprint("c", i), "long", `{}`))
	}
	replies = append(replies, call("f", "finish", `{"status":"met","report":"done"}`))
	cs := newChatServer(t, replies...)
	conv := NewConversation("m", 1, "system prompt", []Tool{bigTool("long", 10_000)})
	var events []store.ConvEvent
	cfg := Config{Model: cs.model(conv.Session), Tools: []Tool{bigTool("long", 10_000)}, MaxTurns: 40, Window: 10_000_000, Task: "go",
		Save: func(*Transcript) { events = append(events, conv.Take()...) }}
	tr := Run(context.Background(), cfg, conv)
	if tr.Outcome != Met || len(cs.bodies) != 30 {
		t.Fatalf("%s after %d requests: %s", tr.Outcome, len(cs.bodies), tr.Error)
	}
	for i := 1; i < len(cs.bodies); i++ {
		if !continues(cs.bodies[i-1], cs.bodies[i]) {
			t.Fatalf("request %d does not start with the one before", i)
		}
	}
	// Every one of the 29 results is still there, in full, in the last request.
	if n := strings.Count(cs.bodies[len(cs.bodies)-1], strings.Repeat("y", 10_000)); n != 29 {
		t.Fatalf("the last request has %d full results", n)
	}
	for _, e := range events {
		if e.T == "trim" {
			t.Fatal("a trim was recorded")
		}
	}
	back, err := LoadConversation(events)
	if err != nil || !reflect.DeepEqual(back.Messages, conv.Messages) || back.Turns != 30 || back.Session != conv.Session {
		t.Fatalf("replay: %v, %d turns", err, back.Turns)
	}
}

// Files written before: a "trim" event still replays to what it did.
func TestOldTrimEventsStillReplay(t *testing.T) {
	long := "first line\n" + strings.Repeat("z", 2000)
	mk := func(m Message) store.ConvEvent { b, _ := json.Marshal(m); return store.ConvEvent{T: "msg", M: b} }
	events := []store.ConvEvent{{T: "header", System: "s"}, mk(Message{Role: "user", Content: "go"}), mk(call("a", "long", `{}`)),
		mk(Message{Role: "tool", ToolCallID: "a", Content: long}), mk(Message{Role: "user", Content: "more"}), {T: "trim", Upto: 3}}
	c, err := LoadConversation(events)
	if err != nil || c.Messages[2].Content != stub(long) || !strings.Contains(c.Messages[2].Content, "characters left out") {
		t.Fatalf("%v %q", err, c.Messages[2].Content)
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

// Events stay pending until a write succeeds: a failed write loses nothing.
func TestFailedWriteKeepsEvents(t *testing.T) {
	conv := NewConversation("m", 1, "sys", nil)
	conv.add(Message{Role: "user", Content: "one"})
	if err := conv.Write(func(...store.ConvEvent) error { return errors.New("disk full") }); err == nil {
		t.Fatal("no error")
	}
	conv.add(Message{Role: "user", Content: "two"})
	var got []store.ConvEvent
	conv.Write(func(evs ...store.ConvEvent) error { got = append(got, evs...); return nil })
	if len(got) != 3 || got[0].T != "header" || !strings.Contains(string(got[2].M), "two") {
		t.Fatalf("%+v", got)
	}
	if conv.Write(func(evs ...store.ConvEvent) error { got = evs; return nil }); len(got) != 0 {
		t.Fatalf("written twice: %+v", got)
	}
}
