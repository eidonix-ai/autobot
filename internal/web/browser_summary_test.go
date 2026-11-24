//go:build browser

// The run page's Summary panel in a real browser: it is not there while a run has no summary, appears
// above the Report panel without the page reloading when the run saves one, and follows it. Run with:
//
//	go test -p 1 -tags browser -run BrowserSummary ./internal/web/
package web

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/eidonix-ai/autobot/internal/store"
)

func TestBrowserSummaryPanelAppearsLive(t *testing.T) {
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
		chromedp.WaitVisible(`#run-head`),
		chromedp.Evaluate(`window.__sameDocument = 42`, nil),
	)
	if err != nil {
		t.Fatalf("opening the run page: %v", err)
	}
	var panels int
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('#run-head h2').length`, &panels))
	if panels != 0 {
		t.Fatalf("a run with nothing yet has %d panels", panels)
	}

	// The run saves its summary and report: both panels appear, the Summary first, in the same document.
	e.st.SaveAgentRun(run, "running", `{"turns":2,"summary":"THE SUMMARY: three findings.","report":"THE REPORT in full."}`, 0)
	if err := chromedp.Run(ctx, wait(`document.querySelectorAll('#run-head h2').length === 2`)); err != nil {
		t.Fatalf("the panels did not appear: %v", err)
	}
	var heads, summary string
	var same bool
	chromedp.Run(ctx,
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#run-head h2')).map(function(h){return h.textContent}).join(',')`, &heads),
		chromedp.Text(`#run-summary .md`, &summary),
		chromedp.Evaluate(`window.__sameDocument === 42`, &same))
	if heads != "Summary,Report" || summary != "THE SUMMARY: three findings." || !same {
		t.Fatalf("panels %q, summary %q, same document %v", heads, summary, same)
	}

	// A later save changes the text in place.
	e.st.SaveAgentRun(run, "running", `{"turns":3,"summary":"A NEWER SUMMARY","report":"THE REPORT in full."}`, 0)
	if err := chromedp.Run(ctx, wait(`document.querySelector('#run-summary .md').textContent.indexOf('A NEWER SUMMARY') >= 0`)); err != nil {
		t.Fatalf("the summary did not change: %v", err)
	}
}
