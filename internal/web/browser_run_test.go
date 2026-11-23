//go:build browser

// The run page in a real browser: while a run goes on, new scratchpad entries and the writing
// session's messages appear in place, without the page reloading, and an entry a person has opened
// stays open. Run with:
//
//	go test -p 1 -tags browser -run Browser ./internal/web/
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
)

func TestBrowserRunPageShowsTheScratchpadLive(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	run, _, thread, err := e.st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: "pr", NodeID: "ag", NodeName: "Researcher", Item: 3, Model: "m"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	wconv, err := e.st.StartWriterSession(run)
	if err != nil {
		t.Fatal(err)
	}
	pad := e.st.Pad(thread)

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

	wait := func(js string) chromedp.Action { return chromedp.Poll(js, nil, chromedp.WithPollingTimeout(15*time.Second)) }
	err = chromedp.Run(ctx,
		chromedp.EmulateViewport(1200, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape(fmt.Sprintf("/w/o/r/agent-runs/%d", run))),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#run-pad`),
		chromedp.Evaluate(`window.__sameDocument = 42`, nil),
	)
	if err != nil {
		t.Fatalf("opening the run page: %v", err)
	}
	var text string
	chromedp.Run(ctx, chromedp.Text(`#run-pad`, &text))
	if text != "No notes yet." {
		t.Fatalf("an empty scratchpad shows %q", text)
	}

	// A note is written while the page is open: it appears, and the page is the same document.
	if _, err := pad.Add(store.PadEntry{Title: "The leak", Importance: store.High, Description: "Foo leaks a file", Body: "a.go:12 opens it and never closes it"}); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ctx, wait(`document.querySelectorAll('#run-pad li details').length === 1`)); err != nil {
		t.Fatalf("the new note did not appear: %v", err)
	}
	var summary string
	var same bool
	chromedp.Run(ctx, chromedp.Text(`#run-pad li summary`, &summary), chromedp.Evaluate(`window.__sameDocument === 42`, &same))
	if !same || summary != "#1 · high · main The leak — Foo leaks a file" {
		t.Fatalf("the note: %q, page reloaded: %v", summary, !same)
	}

	// Open it; a second note arrives; the first stays open and the second is added after it.
	err = chromedp.Run(ctx, chromedp.Click(`#run-pad li summary`), wait(`document.querySelector('#run-pad li details').open`))
	if err != nil {
		t.Fatal(err)
	}
	pad.Add(store.PadEntry{Title: "Style", Importance: store.Low, Description: "naming", Body: "minor"})
	if err := chromedp.Run(ctx, wait(`document.querySelectorAll('#run-pad li').length === 2`)); err != nil {
		t.Fatalf("the second note did not appear: %v", err)
	}
	var open bool
	var body string
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelector('#run-pad li details').open`, &open), chromedp.Text(`#run-pad li pre`, &body))
	if !open || body != "a.go:12 opens it and never closes it" {
		t.Fatalf("the opened note closed: open %v, body %q", open, body)
	}

	// The writing session's first message appears in its own list.
	msg, _ := json.Marshal(agent.Message{Role: "user", Content: "Write the final report now."})
	e.st.AppendConv(wconv, store.ConvEvent{T: "header", System: "the preamble"}, store.ConvEvent{T: "msg", M: msg})
	if err := chromedp.Run(ctx, wait(`document.querySelectorAll('#run-writing li').length >= 2`)); err != nil {
		t.Fatalf("the writing session did not appear: %v", err)
	}
	chromedp.Run(ctx, chromedp.Evaluate(`window.__sameDocument === 42`, &same))
	if !same {
		t.Fatal("the page reloaded")
	}

	// When the run ends the page stops asking.
	e.st.SaveAgentRun(run, "met", `{"outcome":"met","report":"done"}`, 0)
	if err := chromedp.Run(ctx, wait(`document.querySelector('#run-head .status').textContent.indexOf('Goal met') >= 0`)); err != nil {
		t.Fatalf("the end of the run did not show: %v", err)
	}
}
