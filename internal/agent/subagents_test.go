package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// fnModel is a model made of a function, safe to use from several sessions at once.
type fnModel struct {
	mu sync.Mutex
	f  func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error)
}

func (m *fnModel) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	r, err := m.f(ctx, msgs, tools)
	return r, Usage{In: 10, Out: 2}, err
}

// subEnv is a store with one run in a thread, for sub-agents to belong to.
type subEnv struct {
	st     *store.Store
	run    int64
	thread int64
}

func newSubEnv(t *testing.T) *subEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u, err := st.CreateUser("derek", "", "password-1234", true)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := st.CreateWorkspace("o/r", u, "", "")
	run, _, thread, err := st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "ag", NodeName: "ag", Item: 5, Model: "m"}, u)
	if err != nil {
		t.Fatal(err)
	}
	return &subEnv{st, run, thread}
}

// subs makes the sub-agents of the run, with a model for their sessions and a read-only tool.
func (e *subEnv) subs(f func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error), atOnce, perRun int) *SubAgents {
	return &SubAgents{St: e.st, Run: e.run, Model: func(string) Model { return &fnModel{f: f} }, ModelName: "m", Provider: 1, System: "SUB PREAMBLE",
		Tools: []Tool{readTool}, MaxTurns: 4, Window: 100_000, AtOnce: atOnce, PerRun: perRun}
}

// results are the tool answers of the first request after the n-th assistant message of a model's
// conversation, by call id.
func answersTo(msgs []Message) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		if m.Role == "tool" {
			_, text, _ := strings.Cut(m.Content, "] ")
			out[m.ToolCallID] = text
		}
	}
	return out
}

func spawnCall(id, title, task string) Message {
	b, _ := json.Marshal(map[string]string{"title": title, "task": task})
	return call(id, "spawn_agent", string(b))
}

// quick is a sub-agent model that finishes at once with a report, after writing one note.
func quick(report string) func(context.Context, []Message, []ToolSpec) (Message, error) {
	return func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		if !slices.ContainsFunc(msgs, func(m Message) bool { return m.Role == "assistant" }) {
			return call("n", "scratchpad_write", `{"title":"Note","importance":"high","description":"what it says","body":"BODY OF THE NOTE"}`), nil
		}
		return finishCall(report), nil
	}
}

// The main agent has its sub-agents work at most AtOnce at a time: a call made while that many are
// working is refused, and one made after they have finished is accepted. Every answer ends with the
// run's totals.
func TestSubAgentsAtOnceLimit(t *testing.T) {
	e := newSubEnv(t)
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		if !slices.ContainsFunc(msgs, func(m Message) bool { return m.Role == "assistant" }) {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return finishCall("sub report"), nil
	}, 2, 0)
	go func() { // once both have started, let them go
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				return
			}
		}
		time.Sleep(100 * time.Millisecond) // the third call has long been answered
		close(release)
	}()
	main := &script{replies: []Message{
		calls(spawnCall("a", "one", "task one"), spawnCall("b", "two", "task two"), spawnCall("c", "three", "task three")),
		spawnCall("d", "four", "task four"),
		finishCall("done"),
	}}
	tr := Run(context.Background(), Config{Model: main, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go"}, nil)
	if tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	first := answersTo(main.seen[1])
	refused := 0
	for id, text := range first {
		if strings.HasPrefix(text, "error: 2 sub-agents are already working") {
			refused++
			if !strings.HasSuffix(text, "Sub-agents so far: 2 started, 0 turns used.") {
				t.Errorf("the refusal of %s: %q", id, text)
			}
		} else if !strings.Contains(text, "finished (met)") {
			t.Errorf("%s: %q", id, text)
		}
	}
	if refused != 1 || len(first) != 3 {
		t.Fatalf("%d of %d calls refused: %v", refused, len(first), first)
	}
	// After they finished a call is accepted, and the totals count the turns used.
	fourth := answersTo(main.seen[2])["d"]
	if !strings.HasPrefix(fourth, `Sub-agent s3 "four": finished (met), 1 turns.`) || !strings.HasSuffix(fourth, "Sub-agents so far: 3 started, 3 turns used.") {
		t.Fatalf("the fourth call: %q", fourth)
	}
}

// A total for the run, when set, refuses a call past it; none is set by default. The description and
// the totals line say so.
func TestSubAgentsPerRunTotal(t *testing.T) {
	for _, perRun := range []int{0, 2} {
		e := newSubEnv(t)
		subs := e.subs(quick("r"), 3, perRun)
		main := &script{replies: []Message{spawnCall("a", "one", "t"), spawnCall("b", "two", "t"), spawnCall("c", "three", "t"), finishCall("done")}}
		Run(context.Background(), Config{Model: main, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go"}, nil)
		last := answersTo(main.seen[3])["c"]
		desc := subs.Tool().Spec.Description
		if perRun == 0 {
			if !strings.Contains(last, `Sub-agent s3 "three": finished (met)`) || !strings.HasSuffix(last, "Sub-agents so far: 3 started, 6 turns used.") || strings.Contains(desc, "in this run") {
				t.Fatalf("with no total: %q\n%s", last, desc)
			}
			continue
		}
		if last != "error: this run's total of 2 sub-agents is reached\n\nSub-agents so far: 2 of 2 started, 4 turns used." || !strings.Contains(desc, "up to 3 sub-agents working at once and 2 in this run") ||
			!strings.Contains(answersTo(main.seen[1])["a"], "Sub-agents so far: 1 of 2 started, 2 turns used.") {
			t.Fatalf("with a total of 2: %q\n%s", last, desc)
		}
	}
	// The description gives the limit at a time and the sub-agents' turns.
	e := newSubEnv(t)
	if d := e.subs(quick("r"), 2, 0).Tool().Spec.Description; !strings.Contains(d, "up to 2 sub-agents working at once. Each has up to 4 turns of its own, so start one only for a substantial piece of work that can be done separately.") {
		t.Fatal(d)
	}
}

// Two spawn_agent calls of one turn run at the same time: each sub-agent waits for the other to have asked.
func TestSubAgentsRunConcurrently(t *testing.T) {
	e := newSubEnv(t)
	var wg sync.WaitGroup
	wg.Add(2)
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		if !slices.ContainsFunc(msgs, func(m Message) bool { return m.Role == "assistant" }) {
			wg.Done()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				return Message{}, fmt.Errorf("the other sub-agent never started: they ran one after the other")
			}
		}
		return finishCall("together"), nil
	}, 2, 0)
	main := &script{replies: []Message{calls(spawnCall("a", "one", "t"), spawnCall("b", "two", "t")), finishCall("done")}}
	tr := Run(context.Background(), Config{Model: main, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go"}, nil)
	got := answersTo(main.seen[1])
	if tr.Outcome != Met || !strings.Contains(got["a"], "finished (met)") || !strings.Contains(got["b"], "finished (met)") {
		t.Fatalf("%s: %v", tr.Outcome, got)
	}
}

// What a sub-agent has: the block's turn budget, the harness's tools with its own scratchpad, the
// read-only tools and no others, no spawn_agent; it cannot see the main scratchpad, and its notes
// do not reach it.
func TestSubAgentHasItsOwnScratchpadAndNoShell(t *testing.T) {
	e := newSubEnv(t)
	main := &memPad{}
	main.Add(store.PadEntry{Title: "main note", Importance: store.High, Description: "d", Body: "MAIN BODY"})
	var seenTools []string
	var system string
	var asked []string // what its own scratchpad tools answered
	var requests int
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		requests++
		if requests == 1 {
			seenTools, system = toolNames(tools), msgs[0].Content
			return calls(call("w", "scratchpad_write", `{"title":"private","importance":"medium","description":"mine","body":"PRIVATE BODY"}`),
				call("i", "scratchpad_index", `{}`), call("x", "scratchpad_index", `{"agent":"s1"}`), call("b", "bash", `{"command":"ls"}`), call("s", "spawn_agent", `{"title":"t","task":"t"}`)), nil
		}
		if requests <= 4 { // never finishes: its turn budget ends it
			for _, a := range answersTo(msgs) {
				asked = append(asked, a)
			}
			return call(fmt.Sprint("r", requests), "scratchpad_index", `{}`), nil
		}
		return Message{}, fmt.Errorf("it asked a fifth time")
	}, 1, 0)
	mainModel := &script{replies: []Message{spawnCall("a", "one", "t"), finishCall("done")}}
	tr := Run(context.Background(), Config{Model: mainModel, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go", Pad: main, Subs: subs}, nil)
	if tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	want := []string{"compact", "finish", "prune_result", "read", "scratchpad_index", "scratchpad_read", "scratchpad_search", "scratchpad_write", "update_plan"}
	if !slices.Equal(seenTools, want) || system != "SUB PREAMBLE" {
		t.Fatalf("tools %v\nsystem %q", seenTools, system)
	}
	if requests != 4 { // the block's budget: MaxTurns 4
		t.Fatalf("the sub-agent had %d turns", requests)
	}
	answers := answersTo(mainModel.seen[1])["a"]
	if !strings.Contains(answers, "stopped without finishing") || strings.Contains(answers, "MAIN BODY") {
		t.Fatalf("%s", answers)
	}
	// It saw its own note and not the main agent's; a shell and spawn_agent are not tools it has.
	joined := strings.Join(asked, "|")
	if !strings.Contains(joined, "#1 [medium] private — mine") || strings.Contains(joined, "main note") {
		t.Fatalf("its scratchpad: %s", joined)
	}
	rows, _ := e.st.Subs(e.run)
	notes, _ := e.st.SubNotes(e.run, "s1")
	if len(rows) != 1 || len(notes) != 1 || notes[0].Body != "PRIVATE BODY" || notes[0].Author != "s1" {
		t.Fatalf("%+v %+v", rows, notes)
	}
	if list, _ := main.List(); len(list) != 1 {
		t.Fatalf("the main scratchpad was written to: %+v", list)
	}
}

// The answer holds the report, the outcome and the notes as an index, never their bodies, and the main
// agent reads them with scratchpad_read, agent "s1"; it decides what to copy, and nothing is copied.
func TestSubAgentResultShapeAndReadingItsNotes(t *testing.T) {
	e := newSubEnv(t)
	subs := e.subs(quick("The callers are in the loader."), 2, 0)
	main := &memPad{}
	mainModel := &script{replies: []Message{
		spawnCall("a", "callers of take_reused", "find them"),
		calls(call("r", "scratchpad_read", `{"entries":[1],"agent":"s1"}`), call("x", "scratchpad_read", `{"entries":[1],"agent":"s9"}`), call("i", "scratchpad_index", `{"agent":"s1"}`),
			call("q", "scratchpad_search", `{"pattern":"BODY","agent":"s1"}`), call("m", "scratchpad_read", `{"entries":[1]}`)),
		finishCall("done"),
	}}
	tr := Run(context.Background(), Config{Model: mainModel, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go", Pad: main, Subs: subs}, nil)
	if tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	got := answersTo(mainModel.seen[1])["a"]
	want := "Sub-agent s1 \"callers of take_reused\": finished (met), 2 turns.\n\nReport:\nThe callers are in the loader.\n\n" +
		"Its notes (read any with scratchpad_read, agent \"s1\"):\n#1 [high] Note — what it says (by s1)\n\nSub-agents so far: 1 started, 2 turns used."
	if got != want {
		t.Fatalf("the answer:\n%s\nwant\n%s", got, want)
	}
	second := answersTo(mainModel.seen[2])
	if !strings.Contains(second["r"], "BODY OF THE NOTE") || !strings.HasPrefix(second["x"], "error: the scratchpad could not be read: no such sub-agent") ||
		second["i"] != "#1 [high] Note — what it says (by s1)" || !strings.Contains(second["q"], "line 1: BODY OF THE NOTE") {
		t.Fatalf("%v", second)
	}
	if list, _ := main.List(); len(list) != 0 || !strings.Contains(second["m"], "No such entries: 1") {
		t.Fatalf("something was copied: %+v / %q", list, second["m"])
	}
}

// A sub-agent that ends without finishing still returns what it had.
func TestSubAgentWithoutFinishReturnsWordsOrNotes(t *testing.T) {
	e := newSubEnv(t)
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		if !slices.ContainsFunc(msgs, func(m Message) bool { return m.Role == "assistant" }) {
			return call("n", "scratchpad_write", `{"title":"Finding","importance":"high","description":"d","body":"found this"}`), nil
		}
		return Message{Role: "assistant"}, nil // nothing to say, ever
	}, 1, 0)
	subs.MaxTurns = 10
	mainModel := &script{replies: []Message{spawnCall("a", "one", "t"), finishCall("done")}}
	Run(context.Background(), Config{Model: mainModel, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go"}, nil)
	got := answersTo(mainModel.seen[1])["a"]
	if !strings.Contains(got, "ended without finishing") || !strings.Contains(got, "Report:\n## Finding\n\nfound this") {
		t.Fatalf("%s", got)
	}
}

// The main agent's compaction message lists each sub-agent with its outcome and the first line of its
// report; their notes stay readable.
func TestCompactionListsSubAgents(t *testing.T) {
	e := newSubEnv(t)
	subs := e.subs(quick("First line of the report.\nSecond line."), 2, 0)
	mainModel := &script{replies: []Message{
		spawnCall("a", "callers", "t"),
		call("k", "compact", `{"carry_forward":"carry on"}`),
		finishCall("done"),
	}}
	conv := NewConversation("m", 1, "s", []Tool{subs.Tool()})
	Run(context.Background(), Config{Model: mainModel, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go", Pad: &memPad{}, Subs: subs}, conv)
	after := last(mainModel.seen[2])
	if !strings.Contains(after, "Your sub-agents (their notes stay readable") || !strings.Contains(after, `s1 "callers": met — First line of the report.`) || strings.Contains(after, "Second line") {
		t.Fatalf("the compacted message:\n%s", after)
	}
}

// With a closing step the writing session's first message lists the sub-agents' reports, and it can
// read their notes.
func TestWriterSeesSubAgents(t *testing.T) {
	e := newSubEnv(t)
	subs := e.subs(quick("Sub report line.\nmore"), 2, 0)
	pad := e.st.Pad(e.thread)
	var writerFirst string
	var writerAnswer string
	model := &fnModel{f: func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		answers := answersTo(msgs)
		switch {
		case strings.Contains(msgs[1].Content, "Your research is done"):
			writerFirst = msgs[1].Content
			if a, ok := answers["w1"]; ok {
				writerAnswer = a
				return finishCall("final report"), nil
			}
			return call("w1", "scratchpad_read", `{"entries":[1],"agent":"s1"}`), nil
		case strings.HasPrefix(msgs[len(msgs)-1].Content, "Before your report is written"):
			return Message{Role: "assistant"}, nil
		case len(answers) == 0:
			return spawnCall("a", "callers", "t"), nil
		}
		return finishCall("research summary"), nil
	}}
	conv := NewConversation("m", 1, "s", []Tool{subs.Tool()})
	wtools := []Tool{readTool}
	w := Writer{Config: Config{Model: model, System: "s", Tools: wtools, MaxTurns: 5}, Conv: NewWriterConversation("m", 1, "s", wtools), Intro: "Do it.", Time: time.Minute}
	tr := RunClosing(context.Background(), Config{Model: model, Task: "go", Tools: []Tool{subs.Tool()}, MaxTurns: 9, Pad: pad, Subs: subs}, conv, w)
	if tr.Report != "final report" {
		t.Fatalf("%s %q %s", tr.Outcome, tr.Report, tr.Error)
	}
	if !strings.Contains(writerFirst, "Your sub-agents' reports") || !strings.Contains(writerFirst, `s1 "callers": met — Sub report line.`) {
		t.Fatalf("the writer's first message:\n%s", writerFirst)
	}
	if !strings.Contains(writerAnswer, "BODY OF THE NOTE") {
		t.Fatalf("the writer could not read the notes: %q", writerAnswer)
	}
}

// Stopping the main agent (its context) stops its sub-agents.
func TestStoppingTheParentStopsSubAgents(t *testing.T) {
	e := newSubEnv(t)
	started := make(chan struct{})
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		close(started)
		<-ctx.Done() // it works until it is stopped
		return Message{}, ctx.Err()
	}, 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Transcript)
	go func() {
		done <- Run(ctx, Config{Model: &script{replies: []Message{spawnCall("a", "one", "t")}}, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go"}, nil)
	}()
	<-started
	cancel()
	select {
	case tr := <-done:
		if tr.Outcome != Budget && tr.Outcome != Failed {
			t.Fatalf("%s", tr.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run kept going after it was stopped")
	}
	rows, _ := e.st.Subs(e.run)
	if len(rows) != 1 || rows[0].Status == "running" {
		t.Fatalf("the sub-agent was left running: %+v", rows)
	}
}

// What only a sub-agent read counts as read for the main agent's report: it is not marked "(not opened)".
func TestSubAgentReadsCountForTheReport(t *testing.T) {
	e := newSubEnv(t)
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		switch {
		case !slices.ContainsFunc(msgs, func(m Message) bool { return m.Role == "assistant" }):
			return call("r", "read", `{"path":"sub_read.go"}`), nil
		}
		return finishCall("read it"), nil
	}, 1, 0)
	mainModel := &script{replies: []Message{spawnCall("a", "one", "t"), finishCall("see sub_read.go:3 and other.go:4")}}
	tr := Run(context.Background(), Config{Model: mainModel, Tools: []Tool{subs.Tool(), readTool}, MaxTurns: 9, Task: "go"}, nil)
	if tr.Report != "see sub_read.go:3 and other.go:4 (not opened)" {
		t.Fatalf("%q", tr.Report)
	}
}

// spawn_agent calls are tool calls like any other: past the sixteen of a turn they are refused, to be
// made in the next, and the sixteen that run do so together.
func TestSpawnCallsKeepTheCallsPerTurnCap(t *testing.T) {
	e := newSubEnv(t)
	subs := e.subs(quick("r"), 20, 0)
	var many []Message
	for i := 0; i < maxCalls+1; i++ {
		many = append(many, spawnCall(fmt.Sprint("c", i), "t", "t"))
	}
	main := &script{replies: []Message{calls(many...), finishCall("done")}}
	Run(context.Background(), Config{Model: main, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go"}, nil)
	got := answersTo(main.seen[1])
	if len(got) != maxCalls+1 || !strings.HasPrefix(got[fmt.Sprint("c", maxCalls)], "error: at most 16 tool calls run in one turn") || !strings.Contains(got["c0"], "finished (met)") {
		t.Fatalf("%d answers; the last: %q", len(got), got[fmt.Sprint("c", maxCalls)])
	}
	if rows, _ := e.st.Subs(e.run); len(rows) != maxCalls {
		t.Fatalf("%d sub-agents started", len(rows))
	}
}

// Passing include_context: true prepends the shared Context to the sub-agent's task so the lead does
// not have to copy it verbatim into each spawn_agent call.
func TestSubAgentIncludeContext(t *testing.T) {
	e := newSubEnv(t)
	var mu sync.Mutex
	var seen []string
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		mu.Lock()
		seen = append(seen, msgs[1].Content)
		mu.Unlock()
		return finishCall("ok"), nil
	}, 2, 0)
	subs.Context = "Pull request: https://github.com/o/r/pull/5\n## Orientation packet\nArea: pkg/a"
	main := &script{replies: []Message{
		calls(
			call("a", "spawn_agent", `{"title":"with","task":"review correctness","include_context":true}`),
			call("b", "spawn_agent", `{"title":"without","task":"review prose"}`),
		),
		finishCall("done"),
	}}
	if tr := Run(context.Background(), Config{Model: main, Tools: []Tool{subs.Tool()}, MaxTurns: 5, Task: "go"}, nil); tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	var withFound, withoutFound bool
	for _, s := range seen {
		if strings.Contains(s, "review correctness") {
			withFound = strings.Contains(s, "Orientation packet")
		}
		if strings.Contains(s, "review prose") {
			withoutFound = !strings.Contains(s, "Orientation packet")
		}
	}
	if !withFound || !withoutFound {
		t.Fatalf("include_context mismatch: %v", seen)
	}
}
