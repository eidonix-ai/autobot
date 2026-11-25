package web

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Discord workflows run from a Discord Mention event block through Decision -> Agent (with
// autobot_status) -> Post (reply), enforce private-repo, dry-run, trial, dedup and cooldown rules,
// and never touch GitHub case files.
func TestDiscordMentionWorkflow(t *testing.T) {
	var mu sync.Mutex
	var replies []map[string]any
	dsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/users/@me":
			io.WriteString(w, `{"id":"b1","username":"flare-autobot"}`)
		case r.Method == "GET" && r.URL.Path == "/channels/ch1":
			io.WriteString(w, `{"id":"ch1","name":"general","type":0}`)
		case r.Method == "GET" && r.URL.Path == "/channels/ch1/messages":
			io.WriteString(w, `[{"id":"m0","content":"earlier question","author":{"username":"bob"}}]`)
		case r.Method == "POST" && r.URL.Path == "/channels/ch1/messages":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			replies = append(replies, body)
			mu.Unlock()
			io.WriteString(w, `{"id":"m_reply","channel_id":"ch1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	oldAPI := discord.API
	discord.API = dsrv.URL
	t.Cleanup(func() { discord.API = oldAPI; dsrv.Close() })

	// Fake LLM supporting both /v1/decisions ("answer" or "ignore") and /v1/chat/completions
	// (calls autobot_status once, then finish).
	decisionChoice := "answer"
	var sawStatusTool bool
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/decisions") {
			mu.Lock()
			ch := decisionChoice
			mu.Unlock()
			other := "ignore"
			if ch == "ignore" {
				other = "answer"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"answers": []map[string]any{{
					"choice": ch,
					"probabilities": []map[string]any{
						{"value": ch, "probability": 0.98},
						{"value": other, "probability": 0.02},
					},
				}},
			})
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		hasToolResult := false
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "https://github.com/o/r/issues") {
				hasToolResult = true
			}
		}
		for _, tl := range req.Tools {
			if tl.Function.Name == "autobot_status" {
				mu.Lock()
				sawStatusTool = true
				mu.Unlock()
			}
		}
		if !hasToolResult {
			json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "c1", "type": "function",
						"function": map[string]any{"name": "autobot_status", "arguments": `{}`},
					}},
				},
			}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id": "c2", "type": "function",
					"function": map[string]any{
						"name":      "finish",
						"arguments": `{"status":"met","report":"Open issues: https://github.com/o/r/issues"}`,
					},
				}},
			},
		}}})
	}))
	t.Cleanup(llm.Close)

	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.SetProviderRoutes(p.ID, true, false)
	e.st.LendProvider(p, []int64{ws.ID}, derek)

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	ev := workflow.DefaultEvent(workflow.DiscordMention)
	ev.CooldownSeconds = 60
	dec := workflow.DefaultDecision(workflow.Discord)
	dec.Model = workflow.ModelRef{Provider: p.ID, Model: "m"}
	ag := workflow.DefaultAgent(workflow.Discord)
	ag.Model = workflow.ModelRef{Provider: p.ID, Model: "m"}
	po := workflow.DefaultPostFor(workflow.Discord)

	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev", Type: workflow.DiscordMention, Name: "Mention", Settings: raw(ev)},
			{ID: "de", Type: workflow.Decision, Name: "Gate", Settings: raw(dec)},
			{ID: "ag", Type: workflow.Agent, Name: "Helper", Settings: raw(ag)},
			{ID: "po", Type: workflow.Post, Name: "Reply", Settings: raw(po)},
		},
		Links: []workflow.Link{
			{From: "ev", Out: "out", To: "de", In: "in"},
			{From: "de", Out: "answer", To: "ag", In: "in"},
			{From: "ag", Out: "out", To: "po", In: "in"},
		},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.Discord, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}

	b := e.signIn("derek")
	// Discord tab renders without the GitHub Items link.
	code, page := b.get("/w/o/r/workflows/discord")
	if code != 200 || !strings.Contains(page, "Discord") || strings.Contains(page, "/items/discord") {
		t.Fatalf("discord workflow page: %d", code)
	}
	// Switching Live without a Discord bot fails cleanly.
	if code, _, _ := b.post("/w/o/r/workflows/discord/live", url.Values{"csrf": {b.csrf()}, "live": {"on"}}); code != http.StatusSeeOther {
		t.Fatalf("live without bot: %d", code)
	}
	if w, _ := e.st.Workflow(ws.ID, workflow.Discord); w.Live {
		t.Fatal("discord workflow went live without a bot configured")
	}

	// Configure Discord bot and switch Live.
	pub, _, _ := ed25519.GenerateKey(nil)
	if err := e.st.SetDiscordBot(ws, "tok", hex.EncodeToString(pub), derek); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := b.post("/w/o/r/workflows/discord/live", url.Values{"csrf": {b.csrf()}, "live": {"on"}}); code != http.StatusSeeOther {
		t.Fatalf("live with bot: %d", code)
	}
	if w, _ := e.st.Workflow(ws.ID, workflow.Discord); !w.Live {
		t.Fatal("discord workflow did not go live")
	}

	waitRuns := func(want int) []store.WorkflowRun {
		t.Helper()
		for i := 0; i < 100; i++ {
			runs, _ := e.st.WorkflowRuns(ws.ID, 20)
			if len(runs) >= want {
				allDone := true
				for _, r := range runs {
					if r.Status == "running" || r.Status == "queued" {
						allDone = false
					}
				}
				if allDone {
					return runs
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		runs, _ := e.st.WorkflowRuns(ws.ID, 20)
		t.Fatalf("timed out waiting for %d runs (have %d: %+v)", want, len(runs), runs)
		return nil
	}

	// 1. Live mention -> Decision chooses "answer" -> Agent uses autobot_status -> Post replies.
	e.ui.handleDiscordMention(t.Context(), ws.ID, discord.MentionEvent{
		GuildID: "g1", ChannelID: "ch1", MessageID: "m100",
		AuthorID: "u1", Author: "Alice", AuthorHandle: "alice",
		CleanContent: "where are open issues?", URL: "https://discord.com/channels/g1/ch1/m100",
	})
	runs := waitRuns(1)
	if runs[0].Status != "done" || runs[0].Kind != workflow.Discord {
		steps, _ := e.st.WorkflowSteps(runs[0].ID)
		t.Fatalf("run 1: %+v\nsteps: %+v", runs[0], steps)
	}
	mu.Lock()
	gotReplies := len(replies)
	firstReply := replies[0]
	hadTool := sawStatusTool
	mu.Unlock()
	if gotReplies != 1 || !hadTool {
		t.Fatalf("replies=%d sawStatusTool=%v", gotReplies, hadTool)
	}
	if firstReply["content"] != "Open issues: https://github.com/o/r/issues" {
		t.Fatalf("reply content: %v", firstReply["content"])
	}
	if ref := firstReply["message_reference"].(map[string]any); ref["message_id"] != "m100" {
		t.Fatalf("message_reference: %v", ref)
	}
	if am := firstReply["allowed_mentions"].(map[string]any); len(am["parse"].([]any)) != 0 {
		t.Fatalf("allowed_mentions.parse not empty: %v", am)
	}
	// No GitHub case file created.
	if cases, _ := e.st.Cases(ws.ID, 10); len(cases) != 0 {
		t.Fatalf("discord run created a case file: %+v", cases)
	}
	// Runs page and run detail page render Discord run cleanly (no #0).
	if code, body := b.get("/w/o/r/workflow-runs"); code != 200 || !strings.Contains(body, "where are open issues?") || strings.Contains(body, ">#0<") {
		t.Fatalf("runs page: %d", code)
	}
	if code, body := b.get("/w/o/r/workflow-runs/" + itoa(runs[0].ID)); code != 200 || !strings.Contains(body, "Discord mention in #general") {
		t.Fatalf("workflow run page: %d", code)
	}

	// 2. Duplicate MessageID and cooldown prevent extra runs.
	e.ui.handleDiscordMention(t.Context(), ws.ID, discord.MentionEvent{
		GuildID: "g1", ChannelID: "ch1", MessageID: "m100", Author: "Alice", CleanContent: "dup",
	})
	e.ui.handleDiscordMention(t.Context(), ws.ID, discord.MentionEvent{
		GuildID: "g1", ChannelID: "ch1", MessageID: "m101", Author: "Alice", CleanContent: "too fast",
	})
	if r, _ := e.st.WorkflowRuns(ws.ID, 20); len(r) != 1 {
		t.Fatalf("dedup/cooldown failed: %d runs", len(r))
	}

	// 3. Decision "ignore" skips Agent and Post and sends nothing to Discord.
	mu.Lock()
	decisionChoice = "ignore"
	mu.Unlock()
	id2, err := e.ui.startDiscordWorkflow(t.Context(), ws, "ev", DiscordTrigger{
		ChannelID: "ch1", Channel: "general", MessageID: "m102", Author: "Bob", Content: "lol nice meme",
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRuns(2)
	steps2, _ := e.st.WorkflowSteps(id2)
	if steps2["de"].Port != "ignore" || steps2["ag"].Status != "skipped" || steps2["po"].Status != "skipped" {
		t.Fatalf("ignore routing: %+v", steps2)
	}
	mu.Lock()
	if len(replies) != 1 {
		t.Fatalf("ignore branch posted to Discord: %d", len(replies))
	}
	decisionChoice = "answer"
	mu.Unlock()

	// 4. Private repo never posts to Discord.
	e.ui.access.mu.Lock()
	e.ui.access.byWS[ws.ID] = repoFacts{private: true, at: time.Now()}
	e.ui.access.mu.Unlock()
	id3, err := e.ui.startDiscordWorkflow(t.Context(), ws, "ev", DiscordTrigger{
		ChannelID: "ch1", Channel: "general", MessageID: "m103", Author: "Bob", Content: "status?",
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitRuns(3)
	steps3, _ := e.st.WorkflowSteps(id3)
	if !strings.Contains(steps3["po"].Error, errPrivateRepo.Error()) {
		t.Fatalf("private repo note: %+v", steps3["po"])
	}
	mu.Lock()
	if len(replies) != 1 {
		t.Fatalf("private repo posted to Discord: %d", len(replies))
	}
	mu.Unlock()

	// 5. Manual canvas test run (dry run) works and posts nothing.
	code, loc, body := b.post("/w/o/r/workflows/discord/nodes/ev/run", url.Values{
		"csrf": {b.csrf()}, "message": {"test question"}, "channel": {"dev"}, "dry_run": {"on"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/workflow-runs/") {
		t.Fatalf("manual discord run: %d %q\n%s", code, loc, body)
	}
	waitRuns(4)
	mu.Lock()
	if len(replies) != 1 {
		t.Fatalf("dry run posted to Discord: %d", len(replies))
	}
	mu.Unlock()
}

// Discord case tools list, search and read real case files for the workspace, expose saved step
// output parts, never leak dry-run or other-workspace cases, and omit case links on private repos.
func TestDiscordCaseTools(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	other, _ := e.st.CreateWorkspace("o/other", derek, "", "")
	e.st.SetSetting(publicAddressKey, "https://autobot.example")

	c, _, err := e.st.OpenCase(ws, 42, "pr", "Fix KV cache eviction", "Opened for PR #42.")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := e.st.StartContribution(c.UUID, "Gather Information")
	e.st.SetDraft(id, "Found a race in prefix-cache eviction.")
	e.st.FinalizeContribution(id)
	e.st.SetCaseValue(c.UUID, "lens_team.report", "Blocking: cache.rs:88 race condition.", "n1")
	// Dry-run case and another workspace's case must never appear.
	e.st.OpenDryCase(ws, 99, 42, "pr", "Dry run", "Dry headline")
	e.st.OpenCase(other, 77, "pr", "Secret other repo", "Opened.")

	tools := e.ui.caseTools(ws)
	runTool := func(name, args string) string {
		t.Helper()
		for _, tl := range tools {
			if tl.Spec.Name == name {
				out, err := tl.Run(t.Context(), json.RawMessage(args))
				if err != nil {
					t.Fatalf("%s(%s): %v", name, args, err)
				}
				return out
			}
		}
		t.Fatalf("no tool %s", name)
		return ""
	}

	listed := runTool("list_cases", `{"query":"prefix-cache"}`)
	if !strings.Contains(listed, "PR #42: Fix KV cache eviction") || !strings.Contains(listed, "https://autobot.example/api/v1/cases/"+c.UUID+".html") {
		t.Fatalf("list_cases: %s", listed)
	}
	if strings.Contains(listed, "Dry run") || strings.Contains(listed, "Secret other repo") {
		t.Fatalf("list_cases leaked dry-run or other workspace case: %s", listed)
	}

	read := runTool("read_case", `{"item":42}`)
	if !strings.Contains(read, "Found a race in prefix-cache eviction.") || !strings.Contains(read, "lens_team.report") {
		t.Fatalf("read_case: %s", read)
	}
	part := runTool("read_case", `{"item":42,"part":"lens_team.report"}`)
	if !strings.Contains(part, "Blocking: cache.rs:88 race condition.") {
		t.Fatalf("read_case part: %s", part)
	}
	missing := runTool("read_case", `{"item":999}`)
	if !strings.Contains(missing, "no case file for #999") {
		t.Fatalf("read_case missing: %s", missing)
	}

	// Private repo omits the public case page URL.
	e.ui.access.mu.Lock()
	e.ui.access.byWS[ws.ID] = repoFacts{private: true, at: time.Now()}
	e.ui.access.mu.Unlock()
	priv := runTool("read_case", `{"item":42}`)
	if strings.Contains(priv, "autobot.example/api/v1/cases/") {
		t.Fatalf("private repo leaked case URL: %s", priv)
	}
}
