//go:build browser

// The block settings dialog of the workflow canvas in a real browser: the (i) notes stay inside it, and
// a refused save does not leave a message on the bar once the dialog is closed. Run with:
//
//	go test -p 1 -tags browser -run BrowserDialog ./internal/web/
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

// dialogEnv opens the canvas with one Post block named "Say it" and signs in.
func dialogEnv(t *testing.T) (*env, context.Context, func()) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	post := workflow.DefaultPost()
	post.Message, post.Discord = "Hello {pr:title}", true
	b, _ := json.Marshal(post)
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "po", Type: workflow.Post, Name: "Say it", X: 400, Y: 200, Settings: b},
		{ID: "ag", Type: workflow.Agent, Name: "Look", X: 800, Y: 200}}, Links: []workflow.Link{}}
	gb, _ := json.Marshal(g)
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(gb), admin); err != nil {
		t.Fatal(err)
	}
	ctx, done := chromeContext(t)
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#wf .drawflow`),
		chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery),
		chromedp.WaitVisible(`#wf-modal form.node-form`),
	)
	if err != nil {
		done()
		t.Fatalf("opening the dialog: %v", err)
	}
	return e, ctx, done
}

// Each (i) in the dialog, hovered, shows its note wholly inside the dialog: the note was cut at the
// dialog's left edge when it opened leftward from the icon on one line.
func TestBrowserDialogNotesStayInside(t *testing.T) {
	_, ctx, done := dialogEnv(t)
	defer done()
	checkNotes(t, ctx, 3)
	// The Agent dialog is wider and has notes at its right as well as its left.
	if err := chromedp.Run(ctx, chromedp.Click(`#wf-modal button.close`, chromedp.ByQuery), chromedp.Poll(`!document.getElementById('wf-modal').matches(':popover-open')`, nil),
		chromedp.Click(`#node-2 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`)); err != nil {
		t.Fatal(err)
	}
	checkNotes(t, ctx, 6)
}

// checkNotes hovers each (i) of the dialog that is open, and checks its note lies inside the dialog.
func checkNotes(t *testing.T, ctx context.Context, atLeast int) {
	t.Helper()
	var icons int
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('#wf-modal .info').length`, &icons))
	if icons < atLeast {
		t.Fatalf("%d notes in the dialog, want at least %d", icons, atLeast)
	}
	for i := 0; i < icons; i++ {
		var centre [2]float64
		if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(function(){var icon=document.querySelectorAll('#wf-modal .info')[%d];icon.scrollIntoView({block:'center'});var r=icon.getBoundingClientRect();return [r.left+r.width/2, r.top+r.height/2]})()`, i), &centre)); err != nil {
			t.Fatal(err)
		}
		chromedp.Run(ctx, chromedp.MouseEvent(input.MouseMoved, 1, 1), chromedp.MouseEvent(input.MouseMoved, centre[0], centre[1]))
		var box struct {
			Left, Top, Right, Bottom, MLeft, MTop, MRight, MBottom, Opacity float64
			Text                                                            string
		}
		err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(function(){
			var icon=document.querySelectorAll('#wf-modal .info')[%d], st=getComputedStyle(icon,'::after'), r=icon.getBoundingClientRect(), m=document.getElementById('wf-modal').getBoundingClientRect();
			var left=r.left+parseFloat(st.left), top=r.top+parseFloat(st.top);
			return {Left:left, Top:top, Right:left+parseFloat(st.width), Bottom:top+parseFloat(st.height), MLeft:m.left, MTop:m.top, MRight:m.right, MBottom:m.bottom, Opacity:parseFloat(st.opacity), Text:icon.dataset.tip};
		})()`, i), &box))
		if err != nil {
			t.Fatal(err)
		}
		if box.Opacity != 1 {
			t.Errorf("note %d (%.40q) is not shown on hover", i, box.Text)
		}
		if box.Left < box.MLeft || box.Right > box.MRight || box.Top < box.MTop || box.Bottom > box.MBottom {
			t.Errorf("note %d (%.40q) spans %.0f,%.0f to %.0f,%.0f, outside the dialog's %.0f,%.0f to %.0f,%.0f", i, box.Text, box.Left, box.Top, box.Right, box.Bottom, box.MLeft, box.MTop, box.MRight, box.MBottom)
		}
	}
}

// A save the server refuses leaves its message in the dialog and says on the bar which block's settings
// were not saved; closing the dialog without saving (Cancel, the cross, Escape) takes the bar's message
// away; a later save says Saved.
func TestBrowserDialogRefusedSaveMessageGoesWithTheDialog(t *testing.T) {
	_, ctx, done := dialogEnv(t)
	defer done()
	bar := func() (text string, bad bool) {
		chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById('wf-status').textContent`, &text), chromedp.Evaluate(`document.getElementById('wf-status').classList.contains('bad')`, &bad))
		return
	}
	until := func(js string) chromedp.Action {
		return chromedp.Poll(js, nil, chromedp.WithPollingTimeout(10*time.Second))
	}
	const refusal = "Say it: settings not saved (see the message in the dialog)"
	refuse := func() {
		t.Helper()
		err := chromedp.Run(ctx,
			chromedp.SetValue(`#wf-modal textarea[name=message]`, "Hello {nosuch}", chromedp.ByQuery),
			chromedp.Click(`#wf-modal form.node-form button:not([type])`, chromedp.ByQuery),
			until(`document.getElementById('wf-status').textContent.indexOf('not saved') >= 0`),
		)
		if err != nil {
			t.Fatal(err)
		}
		var inDialog string
		chromedp.Run(ctx, chromedp.Text(`#wf-modal .error`, &inDialog, chromedp.ByQuery))
		if text, bad := bar(); text != refusal || !bad || inDialog != "{nosuch} is not a tag here: use a built-in tag or another step's output tag, or write {{nosuch}} for the text itself." {
			t.Fatalf("after the refusal the bar says %q (bad %v) and the dialog %q", text, bad, inDialog)
		}
	}
	reopen := func() {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`)); err != nil {
			t.Fatal(err)
		}
	}
	for name, closeIt := range map[string]chromedp.Action{
		"Cancel":    chromedp.Click(`#wf-modal button.secondary[data-close]`, chromedp.ByQuery),
		"the cross": chromedp.Click(`#wf-modal button.close`, chromedp.ByQuery),
		"Escape":    chromedp.KeyEvent(kb.Escape),
	} {
		refuse()
		if err := chromedp.Run(ctx, closeIt, until(`!document.getElementById('wf-modal').matches(':popover-open')`)); err != nil {
			t.Fatalf("closing with %s: %v", name, err)
		}
		if err := chromedp.Run(ctx, until(`document.getElementById('wf-status').textContent === ''`)); err != nil {
			text, _ := bar()
			t.Errorf("closing with %s left %q on the bar", name, text)
		}
		reopen()
	}
	// Saved for real: the bar says so, and closing afterwards does not take it away.
	err := chromedp.Run(ctx,
		chromedp.SetValue(`#wf-modal textarea[name=message]`, "Hello {pr:title}", chromedp.ByQuery),
		chromedp.Click(`#wf-modal form.node-form button:not([type])`, chromedp.ByQuery),
		until(`document.getElementById('wf-status').textContent === 'Saved'`),
		until(`!document.getElementById('wf-modal').matches(':popover-open')`),
	)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if text, bad := bar(); text != "Saved" || bad {
		t.Errorf("after a save the bar says %q (bad %v)", text, bad)
	}
}
