package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// memPad is a scratchpad kept in memory.
type memPad struct {
	mu      sync.Mutex
	entries []store.PadEntry
}

func (p *memPad) Add(e store.PadEntry) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.N = len(p.entries) + 1
	e.At = time.Now()
	if e.Importance != store.Low && e.Importance != store.High {
		e.Importance = store.Medium
	}
	p.entries = append(p.entries, e)
	return e.N, nil
}

func (p *memPad) List() ([]store.PadEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.entries), nil
}

// finishCall ends a run.
func finishCall(report string) Message {
	return call("fin", "finish", fmt.Sprintf(`{"status":"met","report":%q}`, report))
}

func toolNames(specs []ToolSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	slices.Sort(out)
	return out
}

// valid reports a problem with a message list that an OpenAI-compatible API would refuse: a tool
// message that does not answer a call of the assistant message before it, or a call left unanswered.
func valid(msgs []Message) string {
	var open map[string]bool
	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			if len(open) > 0 {
				return fmt.Sprintf("message %d: calls of the one before are unanswered", i)
			}
			open = map[string]bool{}
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case "tool":
			if !open[m.ToolCallID] {
				return fmt.Sprintf("message %d: a tool message with no call", i)
			}
			delete(open, m.ToolCallID)
		default:
			if len(open) > 0 {
				return fmt.Sprintf("message %d: calls are unanswered", i)
			}
		}
	}
	if len(open) > 0 {
		return "calls unanswered at the end"
	}
	return ""
}

func bodyOf(msgs []Message, role string, from int) string {
	for _, m := range msgs[from:] {
		if m.Role == role {
			return m.Content
		}
	}
	return ""
}

// Every tool result starts with its number; the context line is added to the last result of a turn
// only once the previous request passed half the window, and only appended.
func TestContextLineOnlyPastHalf(t *testing.T) {
	tool := bigTool("long", 20)
	m := &script{
		replies: []Message{calls(call("a", "long", `{}`), call("b", "long", `{}`)), calls(call("c", "long", `{}`), call("d", "long", `{}`)), finishCall("done")},
		ins:     []int{500_000, 500_001, 10},
	}
	Run(context.Background(), Config{Model: m, Tools: []Tool{tool}, MaxTurns: 9, Window: 1_000_000, Task: "go"}, nil)
	first := m.seen[1] // after the turn whose request was exactly half the window
	var results []string
	for _, msg := range first {
		if msg.Role == "tool" {
			results = append(results, msg.Content)
		}
	}
	if len(results) != 2 || !strings.HasPrefix(results[0], "[result 1] ") || !strings.HasPrefix(results[1], "[result 2] ") || strings.Contains(strings.Join(results, ""), "[context:") {
		t.Fatalf("at exactly half: %q", results)
	}
	second := m.seen[2]
	var later []string
	for _, msg := range second {
		if msg.Role == "tool" {
			later = append(later, msg.Content)
		}
	}
	if len(later) != 4 || strings.Contains(later[2], "[context:") || !strings.HasSuffix(later[3], "\n[context: 500k of 1,000k tokens]") || !strings.HasPrefix(later[3], "[result 4] ") {
		t.Fatalf("past half: %q", later)
	}
	if !reflect.DeepEqual(second[:len(first)], first) { // what was sent is never rewritten
		t.Fatal("the context line rewrote earlier messages")
	}
}

func TestKilo(t *testing.T) {
	for n, want := range map[int]string{412_000: "412k", 1_000_000: "1,000k", 128_000: "128k", 70_400: "70k", 2_345_000: "2,345k"} {
		if got := kilo(n); got != want {
			t.Errorf("kilo(%d) = %s, want %s", n, got, want)
		}
	}
}

// Result numbers are counted over the conversation, so a conversation read back from its file
// numbers the next result after the last.
func TestResultNumbersStableAcrossResume(t *testing.T) {
	tool := bigTool("long", 20)
	var events []store.ConvEvent
	conv := NewConversation("m", 1, "s", []Tool{tool})
	m := &script{replies: []Message{call("a", "long", `{}`), call("b", "long", `{}`), finishCall("one")}}
	Run(context.Background(), Config{Model: m, Tools: []Tool{tool}, MaxTurns: 9, Task: "first",
		Save: func(*Transcript) { events = append(events, conv.Take()...) }}, conv)
	events = append(events, conv.Take()...)
	loaded, err := LoadConversation(events)
	if err != nil {
		t.Fatal(err)
	}
	m2 := &script{replies: []Message{call("c", "long", `{}`), finishCall("two")}}
	Run(context.Background(), Config{Model: m2, Tools: []Tool{tool}, MaxTurns: 9, Task: "second"}, loaded)
	var got []string
	for _, msg := range m2.seen[1] {
		if msg.Role == "tool" {
			got = append(got, strings.SplitN(msg.Content, "]", 2)[0])
		}
	}
	if !reflect.DeepEqual(got, []string{"[result 1", "[result 2", "[result 3", "[result 4"}) { // the finish call's answer is a result too
		t.Fatalf("%q", got)
	}
}

func runWithPad(m Model, pad Pad, tools []Tool, events *[]store.ConvEvent, conv *Conversation, window int) *Transcript {
	cfg := Config{Model: m, Tools: tools, MaxTurns: 30, Pad: pad, Window: window, Task: "the task"}
	if events != nil {
		cfg.Save = func(*Transcript) { *events = append(*events, conv.Take()...) }
	}
	tr := Run(context.Background(), cfg, conv)
	if events != nil {
		*events = append(*events, conv.Take()...)
	}
	return tr
}

// Pruned results become one-line stubs, the request after a prune starts with the same bytes as the
// one before up to the first pruned result, and the file replays to the same messages.
func TestPrune(t *testing.T) {
	tool := Tool{Spec: ToolSpec{Name: "long"}, Run: func(context.Context, json.RawMessage) (string, error) {
		return "the first line\nsecond line " + strings.Repeat("y", 3000), nil
	}}
	replies := []Message{call("a", "long", `{}`), call("b", "long", `{}`), call("c", "long", `{}`),
		call("p", "prune_result", `{"results":[1,2,99]}`), call("q", "prune_result", `{"results":[2]}`), finishCall("done")}
	cs := newChatServer(t, replies...)
	conv := NewConversation("m", 1, "sys", []Tool{tool})
	var events []store.ConvEvent
	tr := runWithPad(cs.model(conv.Session), &memPad{}, []Tool{tool}, &events, conv, 0)
	if tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	after := cs.bodies[4] // the request after the prune
	if !strings.Contains(after, "[result 1 pruned: the first line; 3027 characters]") || !strings.Contains(after, "[result 2 pruned: the first line; 3027 characters]") {
		t.Fatalf("stubs missing:\n%.800s", after)
	}
	if strings.Count(after, strings.Repeat("y", 3000)) != 1 { // result 3 is whole
		t.Fatal("a result that was not pruned was changed")
	}
	if !strings.Contains(after, "Pruned 2 results. Not pruned (no such result, or already pruned): 99.") {
		t.Fatalf("the answer:\n%.2000s", after)
	}
	if !strings.Contains(cs.bodies[5], "Pruned 0 results. Not pruned (no such result, or already pruned): 2.") {
		t.Fatalf("pruning twice:\n%.2000s", cs.bodies[5])
	}
	// The request after the prune starts like the one before, up to the first pruned result.
	before := cs.bodies[3]
	cut := strings.Index(before, "[result 1]")
	if cut < 0 || !strings.HasPrefix(after, before[:cut]) {
		t.Fatal("the request after a prune changed bytes before the first pruned result")
	}
	// The file replays to the messages the conversation has now.
	back, err := LoadConversation(events)
	if err != nil || !reflect.DeepEqual(back.Messages, conv.Messages) || back.Shown != conv.Shown {
		t.Fatalf("replay: %v", err)
	}
	prunes := 0
	for _, e := range events {
		if e.T == "prune" {
			prunes++
			if !reflect.DeepEqual(e.Nums, []int{1, 2}) {
				t.Fatalf("recorded %v", e.Nums)
			}
		}
	}
	if prunes != 1 { // the second prune pruned nothing, so recorded nothing
		t.Fatalf("%d prune events", prunes)
	}
}

// Compacting leaves the task and one message with what was carried forward, the scratchpad index and
// the plan; no message is left that answers a call that is gone; the file replays to the same.
func TestCompact(t *testing.T) {
	tool := bigTool("long", 50)
	replies := []Message{
		call("a", "long", `{}`),
		call("p", "update_plan", `{"steps":[{"step":"read the diff","status":"done"},{"step":"check callers","status":"in_progress","note":"two left"}]}`),
		calls(call("w", "scratchpad_write", `{"title":"Finding one","importance":"high","description":"it leaks","body":"a.go:12 leaks"}`),
			call("k", "compact", `{"carry_forward":"I am at step two; callers of Foo are next."}`)),
		finishCall("done"),
	}
	cs := newChatServer(t, replies...)
	conv := NewConversation("m", 1, "sys", []Tool{tool})
	var events []store.ConvEvent
	tr := runWithPad(cs.model(conv.Session), &memPad{}, []Tool{tool}, &events, conv, 0)
	if tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	var req struct{ Messages []Message }
	if err := json.Unmarshal([]byte(cs.bodies[3]), &req); err != nil {
		t.Fatal(err)
	}
	msgs := req.Messages
	if len(msgs) != 3 || msgs[0].Role != "system" || msgs[1].Role != "user" || msgs[2].Role != "user" || msgs[1].Content != "the task" {
		t.Fatalf("%d messages: %+v", len(msgs), msgs)
	}
	for _, want := range []string{"Your earlier work was compacted. What you carried forward:", "callers of Foo are next", "#1 [high] Finding one — it leaks",
		"[done] read the diff", "[in_progress] check callers (two left)"} {
		if !strings.Contains(msgs[2].Content, want) {
			t.Errorf("the compacted message lacks %q:\n%s", want, msgs[2].Content)
		}
	}
	if p := valid(msgs[1:]); p != "" {
		t.Fatal(p)
	}
	back, err := LoadConversation(events)
	if err != nil || !reflect.DeepEqual(back.Messages, conv.Messages) || back.Shown != conv.Shown {
		t.Fatalf("replay: %v", err)
	}
	// The messages before the compaction are still in the file, for the run page.
	if len(Show(events).Messages) <= len(conv.Messages) {
		t.Fatal("the run page lost what was compacted")
	}
}

const tooLongError = "llm.example.com answered 400 Bad Request: This model's maximum context length is 8192 tokens"

// Past 85% of the window the agent is told to save and compact, with only three tools; one that
// ignores it is compacted by Autobot; a provider refusing a request as too long gets a compaction
// and one more try.
func TestForcedCompact(t *testing.T) {
	tool := bigTool("long", 50)
	short := func(t *testing.T, m *script, window int) *Transcript {
		return runWithPad(m, &memPad{}, []Tool{tool}, nil, NewConversation("m", 1, "sys", []Tool{tool}), window)
	}
	t.Run("obeys", func(t *testing.T) {
		m := &script{ins: []int{9000},
			replies: []Message{call("a", "long", `{}`), calls(call("w", "scratchpad_write", `{"title":"t","importance":"low","description":"d","body":"b"}`), call("k", "compact", `{"carry_forward":"on it"}`)), finishCall("ok")}}
		tr := short(t, m, 10_000)
		if tr.Outcome != Met || len(m.seen) != 3 {
			t.Fatalf("%s after %d requests: %s", tr.Outcome, len(m.seen), tr.Error)
		}
		if got := toolNames(m.tools[1]); !reflect.DeepEqual(got, []string{"compact", "scratchpad_write", "update_plan"}) {
			t.Fatalf("offered %v", got)
		}
		if got := last(m.seen[1]); !strings.HasPrefix(got, "Your context is at 9") || !strings.HasSuffix(got, "% of its window. Save to your scratchpad what you still need, then call compact.") {
			t.Fatalf("told %q", got)
		}
		if len(m.seen[2]) != 3 || !strings.Contains(last(m.seen[2]), "on it") || len(m.tools[2]) < 8 {
			t.Fatalf("after compacting: %d messages, %d tools", len(m.seen[2]), len(m.tools[2]))
		}
	})
	t.Run("not before", func(t *testing.T) {
		m := &script{ins: []int{8000}, replies: []Message{call("a", "long", `{}`), finishCall("ok")}} // 80%
		short(t, m, 10_000)
		if len(m.tools[1]) < 8 || strings.Contains(last(m.seen[1]), "Your context is at") {
			t.Fatal("told to compact at 85%")
		}
	})
	t.Run("ignored", func(t *testing.T) {
		for name, reply := range map[string]Message{"another tool": call("b", "long", `{}`), "words": {Role: "assistant", Content: "I will keep going"}} {
			m := &script{ins: []int{9000}, replies: []Message{call("a", "long", `{}`), reply, finishCall("ok")}}
			tr := short(t, m, 10_000)
			if tr.Outcome != Met || len(m.seen) != 3 || len(m.seen[2]) != 3 || !strings.Contains(last(m.seen[2]), carriedByAutobot) || valid(m.seen[2][1:]) != "" {
				t.Fatalf("%s: %s after %d requests (%d messages): %s", name, tr.Outcome, len(m.seen), len(m.seen[len(m.seen)-1]), tr.Error)
			}
		}
	})
	t.Run("too long", func(t *testing.T) {
		m := &script{fail: map[int]error{2: errors.New(tooLongError)},
			replies: []Message{call("a", "long", `{}`), call("b", "long", `{}`), finishCall("ok")}}
		tr := short(t, m, 0)
		// request 2 was refused; its retry has been compacted
		if tr.Outcome != Met || len(m.seen) != 4 || len(m.seen[2]) < 5 || len(m.seen[3]) != 3 || !strings.Contains(last(m.seen[3]), carriedByAutobot) {
			t.Fatalf("%s after %d requests: %s", tr.Outcome, len(m.seen), tr.Error)
		}
		// refused again: the run fails with the provider's error, after one try only
		m = &script{fail: map[int]error{1: errors.New(tooLongError), 2: errors.New(tooLongError)}, replies: []Message{call("a", "long", `{}`), finishCall("ok")}}
		tr = short(t, m, 0)
		if tr.Outcome != Failed || !strings.Contains(tr.Error, "maximum context length") || len(m.seen) != 3 {
			t.Fatalf("%s after %d requests: %s", tr.Outcome, len(m.seen), tr.Error)
		}
		// any other refusal is not compacted
		m = &script{fail: map[int]error{1: errors.New("llm.example.com answered 400 Bad Request: bad tool schema")}, replies: []Message{call("a", "long", `{}`)}}
		if tr = short(t, m, 0); tr.Outcome != Failed || len(m.seen) != 2 {
			t.Fatalf("%s after %d requests", tr.Outcome, len(m.seen))
		}
	})
}

func TestTooLongErrors(t *testing.T) {
	for msg, want := range map[string]bool{
		"h answered 400 Bad Request: maximum context length is 1000":         true,
		"h answered 400 Bad Request: {\"code\":\"context_length_exceeded\"}": true,
		"h answered 400 Bad Request: the prompt is too long: 9 tokens":       true,
		"h answered 400 Bad Request: input exceeds the context length":       true,
		"h answered 400 Bad Request: invalid tool":                           false,
		"h answered 500 Internal Server Error: maximum context length":       false,
	} {
		if got := tooLong(errors.New(msg)); got != want {
			t.Errorf("%q: %v", msg, got)
		}
	}
}

// The scratchpad: write, index, read and search; "med" and a wrong word are kept.
func TestScratchpadTools(t *testing.T) {
	pad := &memPad{}
	replies := []Message{
		calls(call("1", "scratchpad_write", `{"title":"Leak in Foo","importance":"high","description":"Foo leaks a file","body":"a.go:12 opens and never closes\nb.go:3 same"}`),
			call("2", "scratchpad_write", `{"title":"Style","importance":"med","description":"naming","body":"minor naming"}`),
			call("3", "scratchpad_write", `{"title":"Odd","importance":"critical!","description":"what","body":"weird thing"}`)),
		calls(call("4", "scratchpad_index", `{}`), call("5", "scratchpad_index", `{"importance":"high"}`),
			call("6", "scratchpad_read", `{"entries":[1,7]}`), call("7", "scratchpad_search", `{"pattern":"never clos|naming"}`),
			call("8", "scratchpad_search", `{"pattern":"("}`)),
		finishCall("ok"),
	}
	m := &script{replies: replies}
	runWithPad(m, pad, nil, nil, NewConversation("m", 1, "s", nil), 0)
	byID := map[string]string{}
	for _, msg := range m.seen[2] {
		if msg.Role == "tool" {
			byID[msg.ToolCallID] = strings.SplitN(msg.Content, "] ", 2)[1]
		}
	}
	if byID["1"] != "Saved as entry 1." || byID["2"] != "Saved as entry 2." || !strings.HasPrefix(byID["3"], "Saved as entry 3. Importance \"critical!\" is not low, medium or high, so it was saved as medium.") {
		t.Fatalf("answers: %q %q %q", byID["1"], byID["2"], byID["3"])
	}
	if imps := []string{pad.entries[0].Importance, pad.entries[1].Importance, pad.entries[2].Importance}; !reflect.DeepEqual(imps, []string{"high", "medium", "medium"}) {
		t.Fatalf("importance %v", imps)
	}
	wantIndex := "#1 [high] Leak in Foo — Foo leaks a file\n#2 [medium] Style — naming\n#3 [medium] Odd — what"
	if byID["4"] != wantIndex || byID["5"] != "#1 [high] Leak in Foo — Foo leaks a file" {
		t.Fatalf("index: %q / %q", byID["4"], byID["5"])
	}
	if !strings.Contains(byID["6"], "a.go:12 opens and never closes\nb.go:3 same") || !strings.Contains(byID["6"], "No such entries: 7.") {
		t.Fatalf("read: %q", byID["6"])
	}
	if want := "#1 [high] Leak in Foo — Foo leaks a file\n  line 1: a.go:12 opens and never closes\n#2 [medium] Style — naming\n  line 1: minor naming"; byID["7"] != want {
		t.Fatalf("search: %q", byID["7"])
	}
	if !strings.HasPrefix(byID["8"], "error: not a regular expression") {
		t.Fatalf("a bad pattern: %q", byID["8"])
	}
	// A note is never refused for its label; one with no title and no body is.
	if got := (&session{cfg: Config{Pad: pad}}).padWrite(`{"title":"","importance":"x","description":"","body":""}`); !strings.HasPrefix(got, "error:") || len(pad.entries) != 3 {
		t.Fatalf("%q", got)
	}
	if got := (&session{cfg: Config{Pad: pad}}).padWrite(`{"importance":"high","body":"only a body\nsecond"}`); got != "Saved as entry 4." || pad.entries[3].Title != "only a body" {
		t.Fatalf("%q %+v", got, pad.entries[3])
	}
}

// Importance words, and the entry's author shown when it is not the main agent.
func TestImportanceWords(t *testing.T) {
	for in, want := range map[string]string{"low": "low", "Medium": "medium", "med": "medium", " HIGH ": "high"} {
		if got, ok := Importance(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	if got, ok := Importance("urgent"); ok || got != "medium" {
		t.Errorf("urgent: %q %v", got, ok)
	}
	if got := indexLine(store.PadEntry{N: 7, Author: "helper", Title: "T", Importance: "high", Description: "d"}); got != "#7 [high] T — d (by helper)" {
		t.Errorf("%q", got)
	}
}

// ---- the closing step ----

type closingRun struct {
	m      *script
	pad    *memPad
	tr     *Transcript
	wconv  *Conversation
	events []store.ConvEvent
}

// closing runs research, then the writing of its report, on one scripted model.
func closing(t *testing.T, replies []Message, writerTurns int, readFile string) *closingRun {
	t.Helper()
	m := &script{replies: replies}
	pad := &memPad{}
	read := Tool{Spec: ToolSpec{Name: "read_file"}, Run: func(context.Context, json.RawMessage) (string, error) { return "package a", nil },
		Reads: func(raw json.RawMessage) []string {
			var a struct{ Path string }
			json.Unmarshal(raw, &a)
			return []string{a.Path}
		}}
	conv := NewConversation("m", 1, "preamble", []Tool{read, bigTool("bash", 5)})
	wconv := NewWriterConversation("m", 1, "preamble", []Tool{read})
	cr := &closingRun{m: m, pad: pad, wconv: wconv}
	research := Config{Model: m, Task: "research task", Tools: []Tool{read, bigTool("bash", 5)}, MaxTurns: 10, Pad: pad, Window: 100_000,
		Save: func(*Transcript) { cr.events = append(cr.events, conv.Take()...) }}
	w := Writer{Config: Config{Model: m, System: "preamble", Tools: []Tool{read}, MaxTurns: writerTurns, Window: 100_000}, Conv: wconv,
		Intro: "Instruction: look at it.\n\nDone when: it is all read", Time: time.Minute}
	cr.tr = RunClosing(context.Background(), research, conv, w)
	return cr
}

var researchOver = []Message{
	call("r", "read_file", `{"path":"a.go"}`),
	call("fin", "finish", `{"status":"met","report":"Found it in a.go:1."}`),
	// the save turn
	call("s", "scratchpad_write", `{"title":"Late find","importance":"medium","description":"found late","body":"see a.go:9"}`),
}

// With the closing step, research ends with a summary note, then one turn only for writing notes,
// then a new session with read-only tools writes the report; the run's report is that one.
func TestClosingReport(t *testing.T) {
	replies := append(slices.Clone(researchOver),
		call("x", "scratchpad_index", `{}`), call("y", "read_file", `{"path":"b.go"}`),
		call("fin2", "finish", `{"status":"met","report":"Final: a.go:1 and b.go:2 and c.go:3."}`))
	cr := closing(t, replies, 15, "a.go")
	m, tr := cr.m, cr.tr
	if tr.Outcome != Met || tr.Report != "Final: a.go:1 and b.go:2 and c.go:3 (not opened)." {
		t.Fatalf("%s: %q", tr.Outcome, tr.Report)
	}
	// The research's report was saved as a high-importance note, then the late note was written.
	if len(cr.pad.entries) != 2 || cr.pad.entries[0].Title != "Research summary" || cr.pad.entries[0].Importance != "high" || cr.pad.entries[0].Body != "Found it in a.go:1." {
		t.Fatalf("%+v", cr.pad.entries)
	}
	// Request 2 is the save turn: told so, and only scratchpad_write offered.
	if got := toolNames(m.tools[2]); !reflect.DeepEqual(got, []string{"scratchpad_write"}) || !strings.Contains(last(m.seen[2]), "Before your report is written: save to your scratchpad anything the report needs that is not there yet.") {
		t.Fatalf("save turn: %v %q", got, last(m.seen[2]))
	}
	// Request 3 is the writer's first: a new conversation with the instruction, Done when and the index.
	first := m.seen[3]
	if len(first) != 2 || first[0].Content != "preamble" {
		t.Fatalf("the writing session continued the research: %d messages", len(first))
	}
	for _, want := range []string{"Instruction: look at it.\n\nDone when: it is all read", "Your research is done and its notes are in your scratchpad. Write the final report now, from your notes:",
		"Call finish with the report.", "#1 [high] Research summary — Found it in a.go:1.", "#2 [medium] Late find — found late"} {
		if !strings.Contains(first[1].Content, want) {
			t.Errorf("the writer's first message lacks %q:\n%s", want, first[1].Content)
		}
	}
	// Read-only tools: the scratchpad's reading tools and the research's read tool, no bash, no write.
	if got := toolNames(m.tools[3]); !reflect.DeepEqual(got, []string{"finish", "prune_result", "read_file", "scratchpad_index", "scratchpad_read", "scratchpad_search", "update_plan"}) {
		t.Fatalf("the writer's tools: %v", got)
	}
	// The writing session has its own transcript.
	if w := tr.Writing; w == nil || w.Turns != 3 || w.Outcome != Met || tr.Turns != 3 {
		t.Fatalf("writing %+v, research turns %d", tr.Writing, tr.Turns)
	}
	// Its first message is the conversation's too.
	if cr.wconv.Messages[0].Role != "user" || !strings.Contains(cr.wconv.Messages[0].Content, "Done when: it is all read") {
		t.Fatal("the writer's conversation has not got its task")
	}
}

// The writing session has its own turns and time: out of turns, the report is what it answered with, or
// the notes.
func TestClosingWriterBudgetAndFallback(t *testing.T) {
	// It never finishes: three turns, the last finish-only; its answer in words is the report.
	replies := append(slices.Clone(researchOver),
		call("x", "scratchpad_index", `{}`), call("y", "scratchpad_index", `{}`), Message{Role: "assistant", Content: "report in words"})
	cr := closing(t, replies, 3, "a.go")
	if cr.tr.Writing.Turns != 3 || cr.tr.Writing.Outcome != Budget || cr.tr.Report != "report in words" || len(cr.m.seen) != 6 {
		t.Fatalf("%+v report %q after %d requests", cr.tr.Writing, cr.tr.Report, len(cr.m.seen))
	}
	if got := toolNames(cr.m.tools[5]); !reflect.DeepEqual(got, []string{"finish"}) {
		t.Fatalf("its last turn offered %v", got)
	}
	// The writer fails (the model cannot be reached): the report is the high and medium notes, in order.
	cr = closing(t, slices.Clone(researchOver), 15, "a.go")
	pad := cr.pad
	pad.Add(store.PadEntry{Title: "Minor", Importance: "low", Body: "skip me"})
	if cr.tr.Writing == nil || cr.tr.Writing.Outcome != Failed {
		t.Fatalf("writing %+v", cr.tr.Writing)
	}
	want := "## Research summary\n\nFound it in a.go:1.\n\n## Late find\n\nsee a.go:9"
	if cr.tr.Report != want || cr.tr.Outcome != Met {
		t.Fatalf("%s: %q", cr.tr.Outcome, cr.tr.Report)
	}
}

// Research that failed with an error gets neither the save turn nor a writing session, and a research
// that took no notes and has no report ends with none.
func TestClosingSkippedWhenResearchFails(t *testing.T) {
	cr := closing(t, nil, 15, "a.go") // the model cannot be reached
	if cr.tr.Outcome != Failed || len(cr.m.seen) != 1 || cr.tr.Writing != nil || cr.tr.Report != "" || len(cr.pad.entries) != 0 {
		t.Fatalf("%s after %d requests, report %q", cr.tr.Outcome, len(cr.m.seen), cr.tr.Report)
	}
}

// A research that ends without finish (here: out of turns) saves what it had.
func TestClosingSavesWhatResearchHad(t *testing.T) {
	m := &script{replies: []Message{call("a", "scratchpad_index", `{}`), Message{Role: "assistant", Content: "my partial findings"}, call("s", "scratchpad_index", `{}`)}}
	pad := &memPad{}
	conv := NewConversation("m", 1, "p", nil)
	w := Writer{Config: Config{Model: m, MaxTurns: 3}, Conv: NewWriterConversation("m", 1, "p", nil), Intro: "i", Time: time.Minute}
	tr := RunClosing(context.Background(), Config{Model: m, Task: "t", MaxTurns: 2, Pad: pad}, conv, w)
	if tr.Outcome != Budget || len(pad.entries) < 1 || pad.entries[0].Title != "Research summary" || pad.entries[0].Body != "my partial findings" {
		t.Fatalf("%s %+v", tr.Outcome, pad.entries)
	}
}
