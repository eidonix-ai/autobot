//go:build browser

// The agent form's Closing step in a real browser: a preset's own, read-only; Custom's to choose, with
// the report's turns shown only for the report. Run with:
//
//	go test -p 1 -tags browser -run BrowserClosing ./internal/web/
package web

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestBrowserClosingStep(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph([]workflow.Node{{ID: "aa", Type: workflow.Agent, X: 300, Y: 250}}) // a new block: Gather Information

	chrome := os.Getenv("CHROME")
	if chrome == "" {
		chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	actx, acancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome))...)
	defer acancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	waitSaved := chromedp.Poll(`document.getElementById('wf-status').textContent === 'Saved'`, nil, chromedp.WithPollingTimeout(10*time.Second))
	// state is what the form shows of the closing step: its value, whether it is locked, whether the
	// report's turns are visible.
	state := func() (v struct {
		Closing  string
		Disabled bool
		Turns    bool
	}) {
		var raw string
		err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var s=document.querySelector('#wf-modal select[data-closing]'), l=document.querySelector('#wf-modal [data-when-report]');
			return JSON.stringify({Closing: s.value, Disabled: s.disabled, Turns: !l.hidden})})()`, &raw))
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(raw), &v)
		return
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
	if st := state(); st.Closing != "report" || !st.Disabled || !st.Turns {
		t.Fatalf("Gather Information: %+v", st)
	}
	// A preset without a report: none, locked, no turns for it.
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal select[data-preset]`, "code_quality")); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.Closing != "" || !st.Disabled || st.Turns {
		t.Fatalf("Code Quality: %+v", st)
	}
	// A preset's starting Additional instructions fill an empty box, are replaced by the next preset's
	// while untouched, and are kept once the person has written their own.
	extra := func() string {
		var v string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('#wf-modal [name=system_prompt]').value`, &v)); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal select[data-preset]`, "editor_prose")); err != nil {
		t.Fatal(err)
	}
	if got := extra(); got != "{input.summary}" {
		t.Fatalf("the Editor's starting text: %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal select[data-preset]`, "code_quality")); err != nil {
		t.Fatal(err)
	}
	if got := extra(); got != "" {
		t.Fatalf("an untouched starting text stayed after another preset: %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal [name=system_prompt]`, "my own"), chromedp.SetValue(`#wf-modal select[data-preset]`, "editor_prose")); err != nil {
		t.Fatal(err)
	}
	if got := extra(); got != "my own" {
		t.Fatalf("the person's own text was replaced: %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal [name=system_prompt]`, ""), chromedp.SetValue(`#wf-modal select[data-preset]`, "code_quality")); err != nil {
		t.Fatal(err)
	}
	// A preset's starting run settings fill the inputs when it is chosen, 0 per run unticks the limit, and
	// Custom changes nothing.
	runs := func() string {
		var v string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`['max_turns', 'max_minutes', 'time_warn_percent', 'sub_agents'].map(function (n) { return document.querySelector('#wf-modal [name=' + n + ']').value; }).join(',') + ',' + document.querySelector('#wf-modal [name=limit_sub_agents]').checked`, &v)); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#wf-modal input[data-limit-subs]`), chromedp.SetValue(`#wf-modal select[data-preset]`, "review_lens_team")); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != "60,60,20,6,false" {
		t.Fatalf("the Lens Team's starting settings: %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal input[name=max_minutes]`, "45"), chromedp.SetValue(`#wf-modal select[data-preset]`, "custom")); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != "60,45,20,6,false" {
		t.Fatalf("Custom changed the settings: %q", got)
	}
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal select[data-preset]`, "code_quality")); err != nil {
		t.Fatal(err)
	}
	// Custom unlocks it; the report brings its turns.
	if err := chromedp.Run(ctx, chromedp.SetValue(`#wf-modal select[data-preset]`, "custom")); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.Closing != "" || st.Disabled || st.Turns {
		t.Fatalf("Custom: %+v", st)
	}
	err = chromedp.Run(ctx,
		chromedp.SetValue(`#wf-modal select[data-closing]`, "report"),
		chromedp.SetValue(`#wf-modal input[name=report_turns]`, "7"),
		chromedp.Click(`#wf-modal form.node-form button:not([type])`), waitSaved,
	)
	if st := state(); err == nil && (st.Closing != "report" || st.Disabled || !st.Turns) {
		t.Fatalf("Custom with the report: %+v", st)
	}
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	a, _ := workflow.AgentSettingsOf(g.Nodes[0], workflow.PR)
	if err != nil || a.Persona != "custom" || a.Closing != workflow.ClosingReport || a.ReportTurns != 7 {
		t.Fatalf("stored %+v: %v", a, err)
	}
}
