//go:build browser

// A block whose text uses a tag the rules refuse, in a real browser: the card shows the marker and the
// reason, the bar names the block, the tag and the reason (when the canvas loads and after a save), and
// clicking the card opens its dialog. Run with:
//
//	go test -p 1 -tags browser -run BrowserProblems ./internal/web/
package web

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestBrowserProblemsMarkTheCardAndNameTheTagOnTheBar(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	post := workflow.DefaultPost()
	post.Message = "Done: [[value.report]]" // saved before [[ ]] was checked
	b, _ := json.Marshal(post)
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "po", Type: workflow.Post, Name: "GitHub Comment", X: 400, Y: 200, Settings: b},
		{ID: "ag", Type: workflow.Agent, Name: "Look", X: 800, Y: 200}}, Links: []workflow.Link{}}
	gb, _ := json.Marshal(g)
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(gb), admin); err != nil {
		t.Fatal(err)
	}
	ctx, done := chromeContext(t)
	defer done()
	until := func(js string) chromedp.Action {
		return chromedp.Poll(js, nil, chromedp.WithPollingTimeout(10*time.Second))
	}
	const want = "GitHub Comment: [[value.report]]: no block is called Value"
	var bar, card, plain string
	var barBad, marked, plainMarked bool
	var border, plainBorder string
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#node-1`),
		chromedp.Evaluate(`document.getElementById('wf-status').textContent`, &bar),
		chromedp.Evaluate(`document.getElementById('wf-status').classList.contains('bad')`, &barBad),
		chromedp.Evaluate(`document.getElementById('node-1').classList.contains('wf-problem')`, &marked),
		chromedp.Text(`#node-1 .wf-sub`, &card, chromedp.ByQuery),
		chromedp.Evaluate(`getComputedStyle(document.getElementById('node-1')).borderTopWidth + ' ' + getComputedStyle(document.getElementById('node-1')).borderTopColor`, &border),
		chromedp.Evaluate(`getComputedStyle(document.getElementById('node-2')).borderTopWidth + ' ' + getComputedStyle(document.getElementById('node-2')).borderTopColor`, &plainBorder),
		chromedp.Evaluate(`document.getElementById('node-2').classList.contains('wf-problem')`, &plainMarked),
		chromedp.Text(`#node-2 .wf-sub`, &plain, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bar, want) || !barBad {
		t.Errorf("the bar on load says %q (bad %v), want it to say %q", bar, barBad, want)
	}
	if !marked || !strings.HasPrefix(card, "⚠") || !strings.Contains(card, "[[value.report]]: no block is called Value") {
		t.Errorf("the card on load: marked %v, says %q", marked, card)
	}
	if !strings.HasPrefix(border, "2px ") || border == plainBorder {
		t.Errorf("the marked card's border is %q, an unmarked card's %q: it should be thicker and in the error colour", border, plainBorder)
	}
	if plainMarked || strings.Contains(plain, "⚠") {
		t.Errorf("a block with nothing wrong is marked: %v %q", plainMarked, plain)
	}

	// A save of the layout (deleting the other block) says it again, and the card keeps its marker.
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById('wf-status').textContent = ''`, nil),
		chromedp.Click(`#node-2 .wf-title`, chromedp.ByQuery), // opens its dialog, which we leave at once
		chromedp.WaitVisible(`#wf-modal form.node-form`),
		chromedp.Click(`#wf-modal [data-close]`, chromedp.ByQuery),
		chromedp.Evaluate(`(function(){var r=document.querySelector('#node-2 .wf-title').getBoundingClientRect();
			document.querySelector('#node-2 .wf-title').dispatchEvent(new MouseEvent('contextmenu', {bubbles: true, clientX: r.x+5, clientY: r.y+5}));})()`, nil),
		chromedp.WaitVisible(`#wf-menu button`),
		chromedp.Click(`//div[@id="wf-menu"]/button[normalize-space()="Delete"]`, chromedp.BySearch),
		until(`document.getElementById('wf-status').textContent.indexOf('Saved') === 0`),
		chromedp.Evaluate(`document.getElementById('wf-status').textContent`, &bar),
		chromedp.Evaluate(`document.getElementById('wf-status').classList.contains('bad')`, &barBad),
		chromedp.Evaluate(`document.getElementById('node-1').classList.contains('wf-problem')`, &marked),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bar, "Saved. Needs a fix: ") || !strings.Contains(bar, want) || !barBad || !marked {
		t.Errorf("after a save the bar says %q (bad %v), the card is marked: %v", bar, barBad, marked)
	}

	// Clicking the marker opens the block's dialog as usual.
	var title string
	err = chromedp.Run(ctx,
		chromedp.Click(`#node-1 .wf-sub`, chromedp.ByQuery),
		chromedp.WaitVisible(`#wf-modal form.node-form`),
		chromedp.Text(`#wf-modal .modal-head h2`, &title, chromedp.ByQuery),
	)
	if err != nil || title == "" {
		t.Fatalf("the dialog from the marker: %q %v", title, err)
	}
}
