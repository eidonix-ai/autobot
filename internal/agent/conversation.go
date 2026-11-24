package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
)

// A provider reuses work it did for an earlier request only when a request starts with exactly the
// bytes of that one. So a conversation is kept append-only: what was sent is never rewritten, except
// when the agent itself prunes tool results (prune_result) or compacts (compact), or Autobot compacts
// a conversation that has outgrown its model's window. Nothing is ever trimmed automatically. The
// system prompt and tool definitions are fixed when the conversation starts and stored with it.

// Conversation is what a model has been told and has answered in one session. Run adds to it; its
// events (messages, prunes, compactions, usage) are taken out with Take and written to the
// conversation's file, which LoadConversation replays.
type Conversation struct {
	Model    string
	Provider int64
	Session  string // the key sent with every request, so a gateway can route the conversation to one server
	System   string
	Tools    []ToolSpec // the definitions sent, in order: finish and update_plan first
	Messages []Message  // after the system prompt; added to, pruned and compacted
	Turns    int        // model calls so far
	Shown    int        // messages ever added, compacted ones included: what the run page lists

	// The size of the last request as the provider counted it, and what the conversation held when
	// it was sent, to estimate the next request's size without counting tokens.
	lastIn, lastChars int

	pending []store.ConvEvent
}

// Specs are the tool definitions a research conversation starts with: the agent's own tools (finish,
// the plan, pruning, compacting and the scratchpad), then the tools.
func Specs(tools []Tool) []ToolSpec { return withBuiltins(researchSpecs, tools) }

func withBuiltins(builtins []ToolSpec, tools []Tool) []ToolSpec {
	specs := slices.Clone(builtins)
	for _, t := range tools {
		specs = append(specs, t.Spec)
	}
	return specs
}

// NewConversation starts a conversation with a model, its system prompt and the tools it is given.
func NewConversation(model string, provider int64, system string, tools []Tool) *Conversation {
	return startConversation(model, provider, system, Specs(tools))
}

// NewResearchConversation is NewConversation for a block with a closing step: finish asks for a short
// summary of where the research got to, since the full report is written next, from the notes.
func NewResearchConversation(model string, provider int64, system string, tools []Tool, closing bool) *Conversation {
	if !closing {
		return NewConversation(model, provider, system, tools)
	}
	specs := Specs(tools)
	specs[0] = finishSummarySpec
	return startConversation(model, provider, system, specs)
}

// NewPlainConversation starts a conversation with only the finish tool, for text-only tasks
// (such as editing or rewriting a draft) that do not research code or keep a scratchpad.
func NewPlainConversation(model string, provider int64, system string) *Conversation {
	return startConversation(model, provider, system, []ToolSpec{finishSpec})
}

// NewWriterConversation starts the conversation in which a report is written from the scratchpad:
// it can read the scratchpad, but not write to it or compact.
func NewWriterConversation(model string, provider int64, system string, tools []Tool) *Conversation {
	return startConversation(model, provider, system, withBuiltins(writerSpecs, tools))
}

func startConversation(model string, provider int64, system string, specs []ToolSpec) *Conversation {
	b := make([]byte, 12)
	rand.Read(b)
	c := &Conversation{Model: model, Provider: provider, Session: "autobot-" + hex.EncodeToString(b), System: system, Tools: specs}
	hdr, _ := json.Marshal(c.Tools)
	c.pending = append(c.pending, store.ConvEvent{T: "header", Model: model, Provider: provider, Session: c.Session, System: system, Tools: hdr})
	return c
}

// add appends a message.
func (c *Conversation) add(m Message) {
	c.Messages = append(c.Messages, m)
	c.Shown++
	b, _ := json.Marshal(m)
	c.pending = append(c.pending, store.ConvEvent{T: "msg", M: b})
}

// addResult adds a tool's result: its content starts with its number, counted over the tool
// results the conversation holds (so the numbers are the same after a replay).
func (c *Conversation) addResult(callID, content string) {
	c.add(Message{Role: "tool", ToolCallID: callID, Content: fmt.Sprintf("[result %d] %s", countResults(c.Messages)+1, content)})
}

// countResults is how many tool results msgs hold.
func countResults(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" {
			n++
		}
	}
	return n
}

// repair answers the calls of an assistant message that has no result, which a run cut off in the
// middle of a turn (Autobot restarted) leaves behind: a provider refuses a conversation that has them.
func (c *Conversation) repair() {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role != "assistant" {
			continue
		}
		answered := map[string]bool{}
		for _, r := range c.Messages[i+1:] {
			answered[r.ToolCallID] = r.Role == "tool"
		}
		for _, tc := range c.Messages[i].ToolCalls {
			if !answered[tc.ID] {
				c.addResult(tc.ID, "error: the run was interrupted before this tool ran")
			}
		}
		return
	}
}

// Take returns the events not yet written, and forgets them.
func (c *Conversation) Take() []store.ConvEvent {
	out := c.pending
	c.pending = nil
	return out
}

// Write writes the events not yet written with write, and forgets them only once it succeeds: after
// a failed write they are tried again with the next.
func (c *Conversation) Write(write func(...store.ConvEvent) error) error {
	if err := write(c.pending...); err != nil {
		return err
	}
	c.pending = nil
	return nil
}

// request is the conversation as sent: the system prompt, then the messages with any consecutive
// user messages merged so providers that require alternating roles accept the request.
func (c *Conversation) request() []Message {
	out := make([]Message, 0, len(c.Messages)+1)
	out = append(out, Message{Role: "system", Content: c.System})
	for _, m := range c.Messages {
		if m.Role == "user" && len(out) > 1 && out[len(out)-1].Role == "user" {
			out[len(out)-1].Content += "\n\n" + m.Content
			continue
		}
		out = append(out, m)
	}
	return out
}

// LoadConversation rebuilds a conversation from its file's events, replaying every change to it:
// shortenings (old files), prunes and compactions.
func LoadConversation(events []store.ConvEvent) (*Conversation, error) {
	if len(events) == 0 || events[0].T != "header" {
		return nil, errors.New("the conversation's file has no header")
	}
	h := events[0]
	c := &Conversation{Model: h.Model, Provider: h.Provider, Session: h.Session, System: h.System}
	if len(h.Tools) > 0 {
		if err := json.Unmarshal(h.Tools, &c.Tools); err != nil {
			return nil, fmt.Errorf("the conversation's tools: %v", err)
		}
	}
	for _, e := range events[1:] {
		switch e.T {
		case "msg":
			var m Message
			if err := json.Unmarshal(e.M, &m); err != nil {
				return nil, fmt.Errorf("a message of the conversation: %v", err)
			}
			c.Messages = append(c.Messages, m)
			c.Shown++
		case "trim":
			shorten(c.Messages, e.Upto)
		case "prune":
			pruneResults(c.Messages, e.Nums)
		case "compact":
			msgs := make([]Message, 0, len(e.Msgs))
			for _, raw := range e.Msgs {
				var m Message
				if err := json.Unmarshal(raw, &m); err != nil {
					return nil, fmt.Errorf("a compacted message of the conversation: %v", err)
				}
				msgs = append(msgs, m)
			}
			c.Messages = msgs
			c.Shown += len(msgs)
		case "turn":
			c.Turns++
		}
	}
	return c, nil
}

// maxStub is how long a tool result may be before shortening cuts it.
const maxStub = 400

// stub is what a tool result was shortened to by the automatic batches of old conversations: its
// first line and how much was left out.
func stub(content string) string {
	first, _, _ := strings.Cut(content, "\n")
	if len(first) > 200 {
		first = first[:200]
	}
	return strings.ToValidUTF8(first, "") + fmt.Sprintf("\n[%d characters left out to save room; call the tool again if you need them]", len(content))
}

// shorten cuts the tool results of msgs[:upto] to their stubs. Nothing shortens a conversation
// automatically any more; this only replays the "trim" events of files written before.
func shorten(msgs []Message, upto int) {
	for i := range msgs[:min(upto, len(msgs))] {
		if m := msgs[i]; m.Role == "tool" && len(m.Content) > maxStub {
			msgs[i].Content = stub(m.Content)
		}
	}
}

// maxPrunedFirst is how much of a result's first line its pruned stub keeps.
const maxPrunedFirst = 200

// prunedStub is what a pruned result becomes: its number, its first line and how long it was.
func prunedStub(n int, content string) string {
	first, _, _ := strings.Cut(content, "\n")
	if r := []rune(first); len(r) > maxPrunedFirst {
		first = string(r[:maxPrunedFirst])
	}
	return fmt.Sprintf("[result %d pruned: %s; %d characters]", n, strings.ToValidUTF8(first, ""), len([]rune(content)))
}

// pruneResults replaces the tool results of msgs with the given numbers by their stubs, and returns
// the numbers it pruned: those that exist and were not pruned already.
func pruneResults(msgs []Message, nums []int) []int {
	var done []int
	k := 0
	for i := range msgs {
		if msgs[i].Role != "tool" {
			continue
		}
		k++
		if !slices.Contains(nums, k) || strings.HasPrefix(msgs[i].Content, fmt.Sprintf("[result %d pruned:", k)) {
			continue
		}
		body := strings.TrimPrefix(msgs[i].Content, fmt.Sprintf("[result %d] ", k))
		msgs[i].Content = prunedStub(k, body)
		done = append(done, k)
	}
	return done
}

// prune prunes results and records which, so that a replay does the same.
func (c *Conversation) prune(nums []int) []int {
	done := pruneResults(c.Messages, nums)
	if len(done) > 0 {
		c.pending = append(c.pending, store.ConvEvent{T: "prune", Nums: done})
	}
	return done
}

// compactTo replaces every message by msgs and records that: the conversation then has exactly them.
func (c *Conversation) compactTo(msgs ...Message) {
	c.Messages = nil
	var raws []json.RawMessage
	for _, m := range msgs {
		c.Messages = append(c.Messages, m)
		b, _ := json.Marshal(m)
		raws = append(raws, b)
	}
	c.Shown += len(msgs)
	c.lastIn, c.lastChars = 0, 0
	c.pending = append(c.pending, store.ConvEvent{T: "compact", Msgs: raws})
}

// charsPerToken is the rough size of a token, for estimating a request before it is sent.
const charsPerToken = 3.5

// chars is the size of the next request in characters: the system prompt, the tool definitions and
// the messages.
func (c *Conversation) chars() int {
	n := len(c.System)
	for _, t := range c.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	for _, m := range c.Messages {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n
}

// sent notes the size of the request just answered, as the provider counted it.
func (c *Conversation) sent(in int) {
	c.lastIn, c.lastChars = in, c.chars()
	if in == 0 { // the provider did not say: estimate everything
		c.lastChars = 0
	}
}

// estimate is the size of the next request in tokens: the last request as the provider counted it,
// plus what was added since at 3.5 characters a token.
func (c *Conversation) estimate() int {
	return max(0, c.lastIn+int(float64(c.chars()-c.lastChars)/charsPerToken))
}

// recordTurn notes what a model call cost.
func (c *Conversation) recordTurn(run int64, u Usage) {
	c.Turns++
	c.pending = append(c.pending, store.ConvEvent{T: "turn", Run: run, In: u.In, Out: u.Out, Cached: u.Cached})
}
