//go:build browser

// A reference block on the canvas, in a real browser: right-click an Agent block, choose New
// reference, place it with the mouse. Run with:
//
//	go test -p 1 -tags browser -run BrowserReference ./internal/web/
package web

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestBrowserReferenceBlock(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("aa", "Reviewer", nil)
	a.X, a.Y = 300, 250
	c.saveGraph([]workflow.Node{a})

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

	stored := func() workflow.Graph {
		w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		return g
	}
	waitSaved := chromedp.Poll(`document.getElementById('wf-status').textContent === 'Saved'`, nil, chromedp.WithPollingTimeout(10*time.Second))
	centre := func(sel string, v *[]float64) chromedp.Action {
		return chromedp.Evaluate(`(function(){var r=document.querySelector('`+sel+`').getBoundingClientRect();return [r.x+r.width/2,r.y+r.height/2]})()`, v)
	}
	rightClickNode := func(sel string) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			var p []float64
			if err := centre(sel, &p).Do(ctx); err != nil {
				return err
			}
			return chromedp.MouseClickXY(p[0], p[1], chromedp.ButtonType(input.Right)).Do(ctx)
		})
	}
	var menu []string
	var dashed, line, shownName string
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(c.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#node-1`),
		// Right-click the block: Delete, and New reference.
		rightClickNode(`#node-1 .wf-title`),
		chromedp.WaitVisible(`#wf-menu button`),
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#wf-menu button')).map(function(b){return b.textContent})`, &menu),
		chromedp.Click(`//div[@id="wf-menu"]/button[normalize-space()="New reference"]`, chromedp.BySearch),
		// Placed with the mouse, like any new block.
		chromedp.MouseEvent(input.MouseMoved, 900, 450),
		chromedp.MouseClickXY(900, 450),
		waitSaved,
		chromedp.Evaluate(`document.getElementById('node-2').classList.contains('wf-reference') ? getComputedStyle(document.getElementById('node-2')).borderStyle : 'not marked'`, &dashed),
		chromedp.Text(`#node-2 .wf-ref`, &line, chromedp.ByQuery),
		chromedp.Text(`#node-2 .wf-name`, &shownName, chromedp.ByQuery),
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(menu, ",") != "Delete,New reference" {
		t.Fatalf("the block's menu: %v", menu)
	}
	if shownName != "Reviewer (continued)" {
		t.Fatalf("the new block is called %q", shownName)
	}
	if dashed != "dashed" || line != "continues Reviewer" {
		t.Fatalf("the new block: border %q, line %q", dashed, line)
	}
	g := stored()
	if len(g.Nodes) != 2 {
		t.Fatalf("stored: %+v", g.Nodes)
	}
	ref, _ := workflow.AgentSettingsOf(g.Nodes[1], workflow.PR)
	if g.Nodes[1].Type != workflow.Agent || ref.ContinueOf != "aa" || ref.Model.Model != "m" {
		t.Fatalf("the reference as stored: %+v", ref)
	}

	// After a reload the server's facts show it again. A plain block has neither the dashes nor the line.
	var plain, again string
	var plainLine string
	err = chromedp.Run(ctx,
		chromedp.Reload(), chromedp.WaitVisible(`#node-2`),
		chromedp.Evaluate(`getComputedStyle(document.getElementById('node-2')).borderStyle`, &again),
		chromedp.Text(`#node-2 .wf-ref`, &line, chromedp.ByQuery),
		chromedp.Text(`#node-2 .wf-name`, &shownName, chromedp.ByQuery),
		chromedp.Evaluate(`getComputedStyle(document.getElementById('node-1')).borderStyle`, &plain),
		chromedp.Evaluate(`getComputedStyle(document.querySelector('#node-1 .wf-ref')).display`, &plainLine),
	)
	if err != nil || shownName != "Reviewer (continued)" || again != "dashed" || line != "continues Reviewer" || plain == "dashed" || plainLine != "none" {
		t.Fatalf("after reload: %q %q; plain block %q %q: %v", again, line, plain, plainLine, err)
	}

	// Its settings: a model of its own that starts as the other block's, and what to do with no earlier
	// report. Choosing the block it continues again puts the other block's model back; another model
	// can be chosen and saved.
	var model, disabled, nothing, back string
	other := modelValue(workflow.ModelRef{Provider: c.prov.ID, Model: "m2"})
	err = chromedp.Run(ctx,
		chromedp.Click(`#node-2 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`),
		chromedp.Evaluate(`document.querySelector('#wf-modal select[data-model]').selectedOptions[0].textContent`, &model),
		chromedp.Evaluate(`String(document.querySelector('#wf-modal select[data-model]').disabled)`, &disabled),
		chromedp.Evaluate(`document.querySelector('#wf-modal select[name=no_conversation]').value`, &nothing),
		chromedp.Evaluate(`(function(){var m=document.querySelector('#wf-modal select[data-model]'); m.value='`+other+`';
			document.querySelector('#wf-modal select[data-continue]').dispatchEvent(new Event('change', {bubbles: true})); return m.selectedOptions[0].textContent})()`, &back),
	)
	if err != nil || model != "m · LLM" || disabled != "false" || nothing != "skip" || back != "m · LLM" {
		t.Fatalf("the reference's form: model %q (disabled %s), no conversation %q, after choosing the block again %q: %v", model, disabled, nothing, back, err)
	}
	err = chromedp.Run(ctx,
		chromedp.SetValue(`#wf-modal select[data-model]`, other),
		chromedp.Click(`#wf-modal form.node-form button:not([type])`), waitSaved,
	)
	if ref, _ = workflow.AgentSettingsOf(stored().Nodes[1], workflow.PR); err != nil || ref.Model.Model != "m2" || ref.ContinueOf != "aa" {
		t.Fatalf("a model of its own: %+v: %v", ref, err)
	}

	// Delete the block it continues: the save is not refused, and the reference says so, also after a reload.
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById('wf-status').textContent = ''`, nil),
		rightClickNode(`#node-1 .wf-title`),
		chromedp.WaitVisible(`#wf-menu button`),
		chromedp.Click(`//div[@id="wf-menu"]/button[normalize-space()="Delete"]`, chromedp.BySearch),
		waitSaved,
		chromedp.Reload(), chromedp.WaitVisible(`#node-1`), // the only block left is numbered 1 again
		chromedp.Text(`#node-1 .wf-ref`, &line, chromedp.ByQuery),
	)
	if err != nil || line != "continues: (deleted)" || len(stored().Nodes) != 1 {
		t.Fatalf("after deleting the block: %q, %d nodes stored, %v", line, len(stored().Nodes), err)
	}
}
