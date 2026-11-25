package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// TestLFAPagesCRUDAndIsolation verifies creating, viewing, renaming, and deleting user-defined LFA
// pages, plus workspace isolation so a page of one workspace cannot be opened in another.
func TestLFAPagesCRUDAndIsolation(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws1, err := e.st.CreateWorkspace("acme/flare", derek, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := e.st.CreateWorkspace("acme/other", derek, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")

	// Empty LFA tab renders the create-first-page view.
	code, body := b.get(ws1.Path() + "/workflows/lfa")
	if code != http.StatusOK || !strings.Contains(body, "Create your first Large Feature workflow page") {
		t.Fatalf("empty LFA tab: %d\n%s", code, body)
	}

	csrf := b.csrf()

	// Create page 1.
	code, loc, body := b.post(ws1.Path()+"/workflows/lfa/pages", url.Values{
		"csrf":        {csrf},
		"title":       {"Flare Support for New Model"},
		"description": {"PDCA workflow for adding new models to Flare"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/workflows/lfa:") {
		t.Fatalf("create page: %d %q\n%s", code, loc, body)
	}
	pages, err := e.st.WorkflowPages(ws1.ID)
	if err != nil || len(pages) != 1 || pages[0].Title != "Flare Support for New Model" {
		t.Fatalf("pages: %+v %v", pages, err)
	}
	p1 := pages[0]

	// Opening /workflows/lfa now redirects straight to page 1.
	code, body = b.get(ws1.Path() + "/workflows/lfa")
	if code != http.StatusSeeOther || !strings.Contains(body, "/workflows/"+workflow.LFAKind(p1.ID)) {
		t.Fatalf("LFA redirect: %d\n%s", code, body)
	}
	code, body = b.get(ws1.Path() + "/workflows/" + workflow.LFAKind(p1.ID))
	if code != http.StatusOK || !strings.Contains(body, "Flare Support for New Model") || !strings.Contains(body, "Kick Off") {
		t.Fatalf("LFA page 1: %d\n%s", code, body)
	}

	// Workspace 2 cannot access workspace 1's LFA page.
	code, _ = b.get(ws2.Path() + "/workflows/" + workflow.LFAKind(p1.ID))
	if code != http.StatusNotFound {
		t.Fatalf("cross-workspace LFA page returned %d, want 404", code)
	}

	// Create page 2 ("De-Slop") and verify both appear in the Pages strip.
	code, _, body = b.post(ws1.Path()+"/workflows/lfa/pages", url.Values{
		"csrf":  {csrf},
		"title": {"De-Slop"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("create page 2: %d\n%s", code, body)
	}
	code, body = b.get(ws1.Path() + "/workflows/" + workflow.LFAKind(p1.ID))
	if code != http.StatusOK || !strings.Contains(body, "De-Slop") || !strings.Contains(body, "Flare Support for New Model") {
		t.Fatalf("pages strip missing titles: %d", code)
	}

	// Rename page 1.
	code, _, _ = b.post(ws1.Path()+"/workflows/lfa/pages/"+p1.ID+"/rename", url.Values{
		"csrf":        {csrf},
		"title":       {"Flare New Model PDCA"},
		"description": {"Updated desc"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("rename: %d", code)
	}
	gotP1, err := e.st.WorkflowPage(ws1.ID, p1.ID)
	if err != nil || gotP1.Title != "Flare New Model PDCA" || gotP1.Description != "Updated desc" {
		t.Fatalf("renamed page: %+v %v", gotP1, err)
	}

	// Delete page 1.
	code, _, _ = b.post(ws1.Path()+"/workflows/lfa/pages/"+p1.ID+"/delete", url.Values{"csrf": {csrf}})
	if code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if _, err := e.st.WorkflowPage(ws1.ID, p1.ID); err != store.ErrNotFound {
		t.Fatalf("deleted page still exists: %v", err)
	}
}

// TestLFAKickOffRunCaseAndPages verifies running an LFA page from its Kick Off block end to end:
// rendering {lfa:title}/{lfa:prompt}/{lfa:base}/{lfa:slug}, writing a real LFA case file
// accessible at /cases/lfa-<runID>, running Decisions + CaseEntry, and showing up on Runs & Cases.
func TestLFAKickOffRunCaseAndPages(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, err := e.st.CreateWorkspace("acme/flare", derek, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SaveCharter(ws.ID, 1, "Keep Flare fast and clean.", derek); err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")
	csrf := b.csrf()

	// Fake decision endpoint returning "met".
	var mu sync.Mutex
	var decPrompts []string
	llm := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req)
		mu.Lock()
		decPrompts = append(decPrompts, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/decisions") {
			fmt.Fprint(w, `{"answers":[{"choice":"met","probabilities":[{"value":"met","probability":0.95},{"value":"needs_work","probability":0.05}]}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	p, err := e.st.CreateProvider(store.ProviderEdit{Name: "Mock", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "sk-test"}, derek)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetProviderModels(p.ID, []string{"dec-1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetProviderRoutes(p.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if err := e.st.LendProvider(p, []int64{ws.ID}, derek); err != nil {
		t.Fatal(err)
	}

	page, err := e.st.CreateWorkflowPage(ws, "Flare Support for New Model", "PDCA model enablement", "", derek)
	if err != nil {
		t.Fatal(err)
	}
	kind := workflow.LFAKind(page.ID)
	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ko", Type: workflow.LFAKickOff, X: 0, Y: 0, Settings: mustJSON(workflow.EventSettings{KickOffGuide: "Model name and quant"})},
			{ID: "ce", Type: workflow.CaseEntry, Name: "Record Brief", X: 250, Y: 0, Settings: mustJSON(workflow.CaseEntrySettings{
				Entry:    "Page: {lfa:page}\nTitle: {lfa:title}\nBase: {lfa:base}\nSlug: {lfa:slug}\nPrompt: {lfa:prompt}\nCharter: {charter}",
				Finalize: true,
				OutputTo: "record_brief",
			})},
			{ID: "dc", Type: workflow.Decision, Name: "Goals Met", X: 500, Y: 0, Settings: mustJSON(workflow.DecisionSettings{
				Model:    workflow.ModelRef{Provider: p.ID, Model: "dec-1"},
				Question: "Are goals met for {lfa:title}?",
				Context:  "{record_brief}",
				Output:   "probabilities",
				Choices: []workflow.Choice{
					{Name: "met", Description: "Done"},
					{Name: "needs_work", Description: "More work"},
				},
				OutputTo: "goals_met",
			})},
		},
		Links: []workflow.Link{
			{From: "ko", Out: "out", To: "ce", In: "in"},
			{From: "ce", Out: "out", To: "dc", In: "in"},
		},
	}
	if err := g.Check(kind); err != nil {
		t.Fatalf("graph check: %v", err)
	}
	if _, err := e.st.SaveWorkflow(ws.ID, kind, 1, string(mustJSON(g)), derek); err != nil {
		t.Fatal(err)
	}

	// Kick Off modal renders the prompt guide.
	code, body := b.get(ws.Path() + "/workflows/" + kind + "/nodes/ko/test")
	if code != http.StatusOK || !strings.Contains(body, "Model name and quant") {
		t.Fatalf("kickoff modal: %d\n%s", code, body)
	}

	// Start the LFA workflow via Kick Off form.
	code, loc, body := b.post(ws.Path()+"/workflows/"+kind+"/nodes/ko/run", url.Values{
		"csrf":   {csrf},
		"title":  {"Add Qwen3.9 MoE FP8"},
		"prompt": {"Support Qwen3.9-MoE with FP8 weights and MLA KV cache."},
		"base":   {"main"},
	})
	if code != http.StatusSeeOther || !strings.Contains(loc, "/workflow-runs/") {
		t.Fatalf("kickoff run: %d %q\n%s", code, loc, body)
	}
	var runID int64
	fmt.Sscanf(loc[strings.LastIndex(loc, "/")+1:], "%d", &runID)
	waitFor(t, "LFA workflow run to finish", func() bool {
		r, err := e.st.WorkflowRun(runID)
		return err == nil && r.Status == "done"
	})

	// Real LFA case file exists and contains rendered built-in tags.
	cs, err := e.st.CaseByLFARun(ws.ID, runID)
	if err != nil {
		t.Fatalf("CaseByLFARun: %v", err)
	}
	for _, want := range []string{
		"Page: Flare Support for New Model",
		"Title: Add Qwen3.9 MoE FP8",
		"Base: main",
		"Slug: add-qwen3-9-moe-fp8",
		"Support Qwen3.9-MoE with FP8 weights",
		"Keep Flare fast and clean.",
	} {
		if !strings.Contains(cs.Contents, want) {
			t.Errorf("LFA case missing %q:\n%s", want, cs.Contents)
		}
	}

	// LFA case page, Cases list, Workflow run page, and Runs list all render 200 cleanly.
	for _, path := range []string{
		fmt.Sprintf("%s/cases/lfa-%d", ws.Path(), runID),
		ws.Path() + "/cases",
		fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), runID),
		ws.Path() + "/workflow-runs",
	} {
		code, pageBody := b.get(path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d", path, code)
		}
		if !strings.Contains(pageBody, "Add Qwen3.9 MoE FP8") {
			t.Errorf("GET %s missing LFA title", path)
		}
	}

	// Dry run LFA opens an isolated dry-run case.
	code, dloc, body := b.post(ws.Path()+"/workflows/"+kind+"/nodes/ko/run", url.Values{
		"csrf":    {csrf},
		"title":   {"Dry test feature"},
		"prompt":  {"Just checking dry run."},
		"dry_run": {"on"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("dry kickoff: %d %s", code, body)
	}
	var dryID int64
	fmt.Sscanf(dloc[strings.LastIndex(dloc, "/")+1:], "%d", &dryID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := e.st.WorkflowRun(dryID); err == nil && r.Status == "done" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := e.st.DryCase(dryID); err != nil {
		t.Fatalf("DryCase(%d): %v", dryID, err)
	}
	if _, err := e.st.CaseByLFARun(ws.ID, dryID); err != store.ErrNotFound {
		t.Fatalf("dry run should not create real LFA case: %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
