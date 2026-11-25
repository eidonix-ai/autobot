package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Right-click Copy -> Paste duplicates a configured block on the same canvas or copies it across
// Pull requests, Issues, and Discord tabs, keeping its settings, assigning a unique name/tag,
// adapting {pr...} <-> {issue...} tags, and refusing blocks disallowed on the target tab.
func TestPasteBlockAcrossWorkflows(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	llm, _ := llmServer(t)
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.SetProviderRoutes(p.ID, true, false)
	e.st.LendProvider(p, []int64{ws.ID}, derek)

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	dec := workflow.DecisionSettings{
		Model:    workflow.ModelRef{Provider: p.ID, Model: "m"},
		Context:  "Title: {pr:title}\n\n{pr:body}",
		Question: "Is this malicious?",
		Choices:  []workflow.Choice{{Name: "safe", Description: "Looks fine"}, {Name: "malicious", Description: "Stop and flag"}},
		Output:   "probabilities",
	}
	ap := workflow.DefaultApproval(workflow.PR)
	ap.Question = "Approve {pr:title}?"
	cond := workflow.ConditionSettings{Match: "all", Files: "*.cu", MoreLines: 100, Contains: "kernel"}
	prGraph := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "dec1", Type: workflow.Decision, Name: "Decide Malicious", Settings: raw(dec)},
			{ID: "ap1", Type: workflow.Approval, Name: "Gate", Settings: raw(ap)},
			{ID: "co1", Type: workflow.Condition, Name: "Check", Settings: raw(cond)},
		},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(prGraph)), derek); err != nil {
		t.Fatal(err)
	}

	b := e.signIn("derek")
	csrf := b.csrf()
	paste := func(targetKind string, version int, srcKind, srcNode string) (int, map[string]any) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"version": version, "source_kind": srcKind, "source_node": srcNode, "x": 200, "y": 300,
		})
		req, _ := http.NewRequest("POST", e.srv.URL+"/w/o/r/workflows/"+targetKind+"/paste", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Autobot-CSRF", csrf)
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// 1. Copy Decision from PR -> Issue: keeps choices/model/name and adapts {pr:*} -> {issue:*}.
	code, resp := paste(workflow.Issue, 0, workflow.PR, "dec1")
	if code != http.StatusOK {
		t.Fatalf("PR->Issue paste decision: %d %v", code, resp)
	}
	iw, _ := e.st.Workflow(ws.ID, workflow.Issue)
	ig, _ := loadGraph(iw)
	if len(ig.Nodes) != 1 || ig.Nodes[0].Name != "Decide Malicious" {
		t.Fatalf("issue graph after paste: %+v", ig.Nodes)
	}
	gotDec, _ := workflow.DecisionSettingsOf(ig.Nodes[0], workflow.Issue)
	if !strings.Contains(gotDec.Context, "{issue:title}") || !strings.Contains(gotDec.Context, "{issue:body}") ||
		len(gotDec.Choices) != 2 || gotDec.Choices[1].Name != "malicious" || gotDec.Model.Model != "m" {
		t.Fatalf("adapted decision settings: %+v", gotDec)
	}

	// 2. Duplicate Decision on the same Issue canvas -> gets unique name "Decide Malicious 2".
	code, resp = paste(workflow.Issue, iw.Version, workflow.Issue, ig.Nodes[0].ID)
	if code != http.StatusOK {
		t.Fatalf("same-canvas paste: %d %v", code, resp)
	}
	iw, _ = e.st.Workflow(ws.ID, workflow.Issue)
	ig, _ = loadGraph(iw)
	if len(ig.Nodes) != 2 || ig.Nodes[1].Name != "Decide Malicious 2" || ig.Nodes[1].ID == ig.Nodes[0].ID {
		t.Fatalf("duplicated node: %+v", ig.Nodes)
	}

	// 3. Copy Condition from PR -> Issue clears PR-only Files/MoreLines while keeping Contains.
	code, resp = paste(workflow.Issue, iw.Version, workflow.PR, "co1")
	if code != http.StatusOK {
		t.Fatalf("PR->Issue paste condition: %d %v", code, resp)
	}
	iw, _ = e.st.Workflow(ws.ID, workflow.Issue)
	ig, _ = loadGraph(iw)
	gotCond, _ := workflow.SettingsOf(ig.Nodes[2], workflow.DefaultCondition())
	if gotCond.Files != "" || gotCond.MoreLines != 0 || gotCond.Contains != "kernel" {
		t.Fatalf("adapted condition settings: %+v", gotCond)
	}

	// 4. Copy Decision from PR -> Discord succeeds (NoCaseEntry forced true).
	code, resp = paste(workflow.Discord, 0, workflow.PR, "dec1")
	if code != http.StatusOK {
		t.Fatalf("PR->Discord paste decision: %d %v", code, resp)
	}
	dw, _ := e.st.Workflow(ws.ID, workflow.Discord)
	dg, _ := loadGraph(dw)
	dDec, _ := workflow.DecisionSettingsOf(dg.Nodes[0], workflow.Discord)
	if !dDec.NoCaseEntry || len(dDec.Choices) != 2 {
		t.Fatalf("discord decision settings: %+v", dDec)
	}

	// 5. Copy Human Approval from PR -> Discord is refused (item-only block).
	code, _ = paste(workflow.Discord, dw.Version, workflow.PR, "ap1")
	if code != http.StatusBadRequest {
		t.Fatalf("Approval should not paste into Discord: got %d", code)
	}
}
