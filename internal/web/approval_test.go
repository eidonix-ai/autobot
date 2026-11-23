package web

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

func approvalWorkflow(t *testing.T, e *env, ws *store.Workspace, by *store.User) {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	entry := func(text string) json.RawMessage { return raw(workflow.CaseEntrySettings{Entry: text, Finalize: true}) }
	g := workflow.Graph{Nodes: []workflow.Node{
		{ID: "ev", Type: "pr_ready"},
		{ID: "ap", Type: workflow.Approval, Name: "Hardware", Settings: raw(workflow.ApprovalSettings{Request: "Run {pr_title} on cn7?", Hours: 1})},
		{ID: "ya", Type: workflow.CaseEntry, Settings: entry("Running it.")},
		{ID: "no", Type: workflow.CaseEntry, Settings: entry("Not running it.")},
	}, Links: []workflow.Link{{From: "ev", Out: "out", To: "ap", In: "in"}, {From: "ap", Out: "approved", To: "ya", In: "in"}, {From: "ap", Out: "denied", To: "no", In: "in"}}}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), by); err != nil {
		t.Fatal(err)
	}
}


// A request goes to the case's Discord post; Discord's signed click from a person answers it, a
// bot's or a forged one does not, and the first answer wins. The run then takes that branch.
func TestApprovalOnDiscord(t *testing.T) {
	fakeDiscord(t)
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	pub, priv, _ := ed25519.GenerateKey(nil)
	e.st.SetDiscordBot("good-token", hex.EncodeToString(pub))
	e.st.SetDiscordForum(ws, "f1", "flare-autobot", derek)
	approvalWorkflow(t, e, ws, derek)
	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.PR, "ev", 7, false, derek)
	if err != nil {
		t.Fatal(err)
	}
	var ap *store.Approval
	waitFor(t, "the request on Discord", func() bool {
		ap, _ = e.st.ApprovalFor(id, "ap")
		return ap != nil && ap.Message != ""
	})
	if len(discordSent) < 2 || !strings.Contains(discordSent[0], "PR #7: Retry uploads") || !strings.Contains(discordSent[1], "Run Retry uploads on cn7?") || !strings.Contains(discordSent[1], `"custom_id":"ap:`) {
		t.Fatalf("sent %q", discordSent)
	}
	click := func(answer string, bot bool, signWith ed25519.PrivateKey) (int, string) {
		body, _ := json.Marshal(map[string]any{"type": 3, "data": map[string]any{"custom_id": "ap:" + itoa(ap.ID) + ":" + answer},
			"member": map[string]any{"user": map[string]any{"id": "u1", "username": "festr", "global_name": "Festr", "bot": bot}}})
		req, _ := http.NewRequest("POST", e.srv.URL+"/discord/interactions", bytes.NewReader(body))
		req.Header.Set("X-Signature-Timestamp", "1")
		req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(signWith, append([]byte("1"), body...))))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	_, other, _ := ed25519.GenerateKey(nil)
	if code, _ := click("approved", false, other); code != http.StatusUnauthorized {
		t.Fatalf("a forged click was taken: %d", code)
	}
	if _, body := click("approved", true, priv); !strings.Contains(body, "A bot cannot answer") {
		t.Fatalf("a bot answered: %s", body)
	}
	if _, body := click("approved", false, priv); !strings.Contains(body, `"type":7`) || !strings.Contains(body, "Approved") || !strings.Contains(body, "Festr") {
		t.Fatalf("the approval: %s", body)
	}
	if _, body := click("denied", false, priv); !strings.Contains(body, "Already answered") {
		t.Fatalf("a second answer: %s", body)
	}
	waitFor(t, "the run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	steps, _ := e.st.WorkflowSteps(id)
	c, _ := e.st.CaseByNumber(ws.ID, 7)
	if steps["ap"].Port != "approved" || steps["ya"].Status != "done" || steps["no"].Status != "skipped" || !strings.Contains(c.Contents, "Approved by Festr on Discord") {
		t.Fatalf("%+v\n%s", steps, c.Contents)
	}
}

// On the run's page only the workspace's members may answer; a run waiting when Autobot stopped
// resumes and goes on once answered.
func TestApprovalOnPageAndResume(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	alice := e.user("alice", false)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	e.st.CreateWorkspace("o/r2", derek, "", "")
	e.st.SetMembers(ws.ID, []int64{alice.ID}, derek)
	e.user("mallory", false)
	approvalWorkflow(t, e, ws, derek)
	// The state Autobot leaves when it stops while a request waits.
	w, _ := e.st.Workflow(ws.ID, workflow.PR)
	id, _ := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: 7, Graph: w.Graph}, []string{"ev", "ap", "ya", "no"}, derek)
	e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: "ev", Status: "done", Port: "out", Output: "PR Ready #7"})
	e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: "ap", Status: "waiting", StartedAt: time.Now()})
	ap, _ := e.st.OpenApproval(id, "ap", "Run Retry uploads on cn7?", time.Now().Add(time.Hour))
	e.st.StopOrphanWorkflows()
	e.ui.ResumeWorkflows(t.Context())

	path := "/w/o/r/workflow-runs/" + itoa(id) + "/approvals/" + itoa(ap.ID)
	m := e.signIn("mallory")
	if code, _, _ := m.post(path, url.Values{"csrf": {m.csrf()}, "answer": {"denied"}}); code != http.StatusNotFound {
		t.Fatalf("a non-member answered: %d", code)
	}
	d := e.signIn("derek")
	if code, _, _ := d.post(strings.Replace(path, "/w/o/r/", "/w/o/r2/", 1), url.Values{"csrf": {d.csrf()}, "answer": {"denied"}}); code != http.StatusNotFound {
		t.Fatalf("answered through another workspace: %d", code)
	}
	a := e.signIn("alice")
	if _, body := a.get("/w/o/r/workflow-runs/" + itoa(id)); !strings.Contains(body, `value="approved"`) {
		t.Fatal("the run page does not offer the answer")
	}
	if code, _, _ := a.post(path, url.Values{"csrf": {a.csrf()}, "answer": {"denied"}}); code != http.StatusSeeOther {
		t.Fatalf("a member's answer: %d", code)
	}
	waitFor(t, "the resumed run to end", func() bool { r, _ := e.st.WorkflowRun(id); return r.Status != "running" })
	steps, _ := e.st.WorkflowSteps(id)
	if steps["ap"].Port != "denied" || steps["no"].Status != "done" || steps["ya"].Status != "skipped" {
		t.Fatalf("%+v", steps)
	}
}
