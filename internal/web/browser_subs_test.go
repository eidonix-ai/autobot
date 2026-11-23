//go:build browser

// Sub-agents on the run page, and the limit on them in the agent form, in a real browser. Run with:
//
//	go test -p 1 -tags browser -run BrowserSub ./internal/web/
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

func chromeContext(t *testing.T) (context.Context, func()) {
	chrome := os.Getenv("CHROME")
	if chrome == "" {
		chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	actx, acancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome))...)
	ctx, cancel := chromedp.NewContext(actx)
	ctx, tcancel := context.WithTimeout(ctx, 60*time.Second)
	return ctx, func() { tcancel(); cancel(); acancel() }
}

// A sub-agent started while the page is open appears in place, with its notes and transcript; one a
// person has opened stays open as the sub-agent goes on.
func TestBrowserSubAgentAppearsLive(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	run, _, _, err := e.st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "ag", NodeName: "Researcher", Item: 3, Model: "m"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, done := chromeContext(t)
	defer done()
	wait := func(js string) chromedp.Action {
		return chromedp.Poll(js, nil, chromedp.WithPollingTimeout(15*time.Second))
	}
	err = chromedp.Run(ctx,
		chromedp.EmulateViewport(1200, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape(fmt.Sprintf("/w/o/r/agent-runs/%d", run))),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#run-subs`, chromedp.ByID),
		chromedp.Evaluate(`window.__sameDocument = 7`, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('#run-subs > li').length`, &n))
	if n != 0 {
		t.Fatalf("%d sub-agents before there are any", n)
	}

	// The main agent starts a sub-agent: it appears, with its first message.
	sub, err := e.st.StartSub(run, "s1", "callers of take_reused", "find them")
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal(agent.Message{Role: "user", Content: "You are sub-agent s1."})
	e.st.AppendConv(sub.Conversation, store.ConvEvent{T: "header", System: "the sub-agent preamble"}, store.ConvEvent{T: "msg", M: msg})
	e.st.SubPad(sub.ID, "s1").Add(store.PadEntry{Title: "First find", Importance: store.High, Description: "d", Body: "the first body"})
	if err := chromedp.Run(ctx, wait(`document.querySelectorAll('#run-subs > li').length === 1`)); err != nil {
		t.Fatalf("the sub-agent did not appear: %v", err)
	}
	var head, status string
	var same bool
	chromedp.Run(ctx, chromedp.Text(`#run-subs > li h2`, &head), chromedp.Text(`#run-subs > li p.muted`, &status), chromedp.Evaluate(`window.__sameDocument === 7`, &same))
	if head != "Sub-agent s1: callers of take_reused" || !same || status == "" {
		t.Fatalf("heading %q, status %q, same document %v", head, status, same)
	}

	// Open its note; the sub-agent goes on (a second note): the page follows and the opened note stays open.
	err = chromedp.Run(ctx, chromedp.Click(`#run-subs ol.pad li summary`), wait(`document.querySelector('#run-subs ol.pad li details').open`))
	if err != nil {
		t.Fatal(err)
	}
	e.st.SubPad(sub.ID, "s1").Add(store.PadEntry{Title: "Second find", Importance: store.Low, Description: "d", Body: "the second body"})
	if err := chromedp.Run(ctx, wait(`document.querySelectorAll('#run-subs ol.pad li').length === 2`)); err != nil {
		t.Fatalf("the second note did not appear: %v", err)
	}
	var open bool
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('#run-subs ol.pad li details').open`, &open), chromedp.Evaluate(`window.__sameDocument === 7`, &same))
	if !open || !same {
		t.Fatalf("the opened note closed (%v) or the page reloaded (%v)", !open, !same)
	}
	e.st.SaveAgentRun(run, "met", `{"outcome":"met"}`, 0)
}

// In the agent form, the limit on sub-agents per run is a checkbox that shows its number when checked.
func TestBrowserSubAgentLimitCheckbox(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph([]workflow.Node{{ID: "aa", Type: workflow.Agent, X: 300, Y: 250}})
	ctx, done := chromeContext(t)
	defer done()
	state := func() (checked, shown bool) {
		var raw string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify([document.querySelector('#wf-modal input[data-limit-subs]').checked, !document.querySelector('#wf-modal [data-when-limit]').hidden])`, &raw)); err != nil {
			t.Fatal(err)
		}
		var v [2]bool
		json.Unmarshal([]byte(raw), &v)
		return v[0], v[1]
	}
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(c.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#node-1`),
		chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if checked, shown := state(); checked || shown {
		t.Fatalf("a new block: checked %v, number shown %v", checked, shown)
	}
	waitSaved := chromedp.Poll(`document.getElementById('wf-status').textContent === 'Saved'`, nil, chromedp.WithPollingTimeout(10*time.Second))
	err = chromedp.Run(ctx, chromedp.Click(`#wf-modal input[data-limit-subs]`))
	if checked, shown := state(); err != nil || !checked || !shown {
		t.Fatalf("after checking: %v %v %v", checked, shown, err)
	}
	err = chromedp.Run(ctx,
		chromedp.SetValue(`#wf-modal input[name=sub_agents]`, "3"), chromedp.SetValue(`#wf-modal input[name=sub_agents_per_run]`, "6"),
		chromedp.SetValue(`#wf-modal input[name=time_warn_percent]`, "15"),
		chromedp.Click(`#wf-modal form.node-form button:not([type])`), waitSaved,
	)
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	a, _ := workflow.AgentSettingsOf(g.Nodes[0], workflow.PR)
	if err != nil || a.SubAgents != 3 || a.SubAgentsPerRun != 6 || a.EffectiveTimeWarn() != 15 {
		t.Fatalf("stored %+v: %v", a, err)
	}
	// Reopened: still checked, with its number; unchecked and saved: no limit.
	err = chromedp.Run(ctx, chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`))
	if checked, shown := state(); err != nil || !checked || !shown {
		t.Fatalf("reopened: %v %v %v", checked, shown, err)
	}
	err = chromedp.Run(ctx, chromedp.Click(`#wf-modal input[data-limit-subs]`))
	if checked, shown := state(); err != nil || checked || shown {
		t.Fatalf("after unchecking: %v %v %v", checked, shown, err)
	}
	err = chromedp.Run(ctx, chromedp.Click(`#wf-modal form.node-form button:not([type])`), waitSaved)
	w, _ = c.st.Workflow(c.ws.ID, workflow.PR)
	json.Unmarshal([]byte(w.Graph), &g)
	if a, _ = workflow.AgentSettingsOf(g.Nodes[0], workflow.PR); err != nil || a.SubAgentsPerRun != 0 {
		t.Fatalf("stored after unchecking %+v: %v", a, err)
	}
}
