package workflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDiscordWorkflowKindAndBlocks(t *testing.T) {
	types := TypesFor(Discord)
	var names []string
	for _, nt := range types {
		names = append(names, nt.Type)
	}
	for _, want := range []string{DiscordMention, Agent, Decision, Post, Condition, Junction} {
		if !slices.Contains(names, want) {
			t.Errorf("TypesFor(Discord) missing %q: %v", want, names)
		}
	}
	for _, forbid := range []string{CaseEntry, TestRun, Approval, "pr_ready", "issue_opened"} {
		if slices.Contains(names, forbid) {
			t.Errorf("TypesFor(Discord) should not offer %q: %v", forbid, names)
		}
	}

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	g := Graph{
		Nodes: []Node{
			{ID: "ev", Type: DiscordMention, Settings: raw(DefaultEvent(DiscordMention))},
			{ID: "de", Type: Decision, Name: "Route", Settings: raw(DefaultDecision(Discord))},
			{ID: "ag", Type: Agent, Name: "Answer", Settings: raw(DefaultAgent(Discord))},
			{ID: "po", Type: Post, Name: "Reply", Settings: raw(DefaultPostFor(Discord))},
		},
		Links: []Link{
			{From: "ev", Out: "out", To: "de", In: "in"},
			{From: "de", Out: "answer", To: "ag", In: "in"},
			{From: "ag", Out: "out", To: "po", In: "in"},
		},
	}
	if err := g.Check(Discord); err != nil {
		t.Fatalf("valid Discord graph failed Check: %v", err)
	}
	bad := Graph{Nodes: []Node{{ID: "ce", Type: CaseEntry}}}
	if err := bad.Check(Discord); err == nil {
		t.Fatal("CaseEntry should be rejected on a Discord workflow")
	}

	// Templates: discord:* tags allowed, PR/case tags refused.
	if err := CheckTemplates(Discord, nil, "{discord:channel} {discord:author}: {discord:message}\n{discord:context}\n{discord:link} {repo} {charter} {machines}"); err != nil {
		t.Fatalf("discord tags rejected: %v", err)
	}
	if err := CheckTemplates(Discord, nil, "{pr:diff}"); err == nil {
		t.Fatal("pr:diff should be rejected in Discord workflow")
	}
	if err := CheckTemplates(Discord, nil, "{case:link}"); err == nil {
		t.Fatal("case:link should be rejected in Discord workflow")
	}

	// Condition rules on Discord.
	if err := (ConditionSettings{Match: "all", Contains: "status"}).Check(Discord); err != nil {
		t.Fatalf("Contains rule rejected on Discord: %v", err)
	}
	if err := (ConditionSettings{Match: "all", Files: "*.go"}).Check(Discord); err == nil {
		t.Fatal("Files rule should be rejected on Discord")
	}
	if err := (ConditionSettings{Match: "all", Label: "bug"}).Check(Discord); err == nil {
		t.Fatal("Label rule should be rejected on Discord")
	}
}

func TestMatchesDiscordChannel(t *testing.T) {
	ev := DefaultEvent(DiscordMention)
	if !MatchesDiscordChannel(ev, "general", "123") {
		t.Fatal("empty Channels should match any channel")
	}
	ev.Channels = "#general, 456"
	if !MatchesDiscordChannel(ev, "General", "123") || !MatchesDiscordChannel(ev, "other", "456") {
		t.Fatal("should match by name (case-insensitive, # optional) or ID")
	}
	if MatchesDiscordChannel(ev, "random", "789") {
		t.Fatal("should not match unlisted channel")
	}
	ev.Channels = ""
	ev.IgnoreChannels = "announcements, 999"
	if MatchesDiscordChannel(ev, "#announcements", "111") || MatchesDiscordChannel(ev, "dev", "999") {
		t.Fatal("should ignore excluded channel")
	}
	if !MatchesDiscordChannel(ev, "dev", "222") {
		t.Fatal("non-ignored channel should match")
	}
	if sum := ev.SummaryFor(DiscordMention); !strings.Contains(sum, "except announcements") {
		t.Fatalf("SummaryFor = %q", sum)
	}
}
