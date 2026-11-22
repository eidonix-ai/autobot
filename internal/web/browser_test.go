//go:build browser

// The workflow canvas in a real browser (headless Chrome), driven as a person would: right-click,
// choose a node, place it with the mouse, open its settings and save. Run with:
//
//	go test -p 1 -tags browser -run Browser ./internal/web/
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestBrowserWorkflowCanvas(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")

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

	// Anything the page logs as an error, a blocked script or style included, fails the test.
	var mu sync.Mutex
	var problems []string
	chromedp.ListenTarget(ctx, func(ev any) {
		mu.Lock()
		defer mu.Unlock()
		switch ev := ev.(type) {
		case *log.EventEntryAdded:
			if ev.Entry.Level == log.LevelError {
				problems = append(problems, ev.Entry.Text)
			}
		case *runtime.EventExceptionThrown:
			problems = append(problems, ev.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError {
				problems = append(problems, fmt.Sprint(ev.Args))
			}
		}
	})

	stored := func() workflow.Graph {
		w, err := e.st.Workflow(ws.ID, workflow.PR)
		if err != nil {
			t.Fatal(err)
		}
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		return g
	}
	waitSaved := chromedp.Poll(`document.getElementById('wf-status').textContent === 'Saved'`, nil, chromedp.WithPollingTimeout(10*time.Second))
	rightClick := func(x, y float64) chromedp.Action {
		return chromedp.MouseClickXY(x, y, chromedp.ButtonType(input.Right))
	}
	menuItem := func(label string) chromedp.Action {
		return chromedp.Click(fmt.Sprintf(`//div[@id="wf-menu"]/button[normalize-space()=%q]`, label), chromedp.BySearch)
	}
	place := func(label string, x, y float64) chromedp.Tasks {
		return chromedp.Tasks{
			rightClick(x-60, y-60), menuItem(label),
			chromedp.MouseEvent(input.MouseMoved, x, y),
			chromedp.MouseClickXY(x, y),
			waitSaved,
		}
	}

	page := e.srv.URL + "/w/o/r/workflows/pr"
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password123"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#wf .drawflow`),
		place("PR Ready", 400, 300),
	)
	if err != nil {
		t.Fatalf("placing PR Ready on %s: %v", page, err)
	}
	g := stored()
	if len(g.Nodes) != 1 || g.Nodes[0].Type != "pr_ready" {
		t.Fatalf("stored after placing: %+v", g.Nodes)
	}
	id := g.Nodes[0].ID

	// Open its settings with a click, change them, save.
	var summary string
	err = chromedp.Run(ctx,
		chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery),
		chromedp.WaitVisible(`#wf-modal form.node-form`),
		chromedp.Click(`#wf-modal input[name=mode][value=summoned]`),
		chromedp.SetValue(`#wf-modal input[name=stop_tag]`, "autobot: skip"),
		chromedp.Click(`#wf-modal form.node-form button:not([type])`),
		waitSaved,
		chromedp.Text(`#node-1 .wf-sub`, &summary),
	)
	if err != nil {
		var status, modalText string
		chromedp.Run(ctx, chromedp.Text(`#wf-status`, &status, chromedp.ByQuery), chromedp.Evaluate(`document.getElementById('wf-modal').innerText`, &modalText))
		t.Fatalf("settings: %v\nstatus: %q\nmodal: %s", err, status, modalText)
	}
	s, _ := workflow.EventSettingsOf(stored().Nodes[0])
	if s.Mode != "summoned" || s.StopTag != "autobot: skip" || stored().Nodes[0].ID != id {
		t.Fatalf("stored settings: %+v", s)
	}
	if !strings.Contains(summary, "Only if summoned") || !strings.Contains(summary, "autobot: skip") {
		t.Fatalf("node summary %q", summary)
	}

	// A second node; the first keeps its settings when the layout is saved again.
	if err := chromedp.Run(ctx, place("PR Activity", 700, 450)); err != nil {
		t.Fatalf("placing PR Activity: %v", err)
	}
	g = stored()
	if len(g.Nodes) != 2 {
		t.Fatalf("stored nodes: %+v", g.Nodes)
	}
	for _, n := range g.Nodes {
		if n.Type == "pr_ready" {
			if s, _ := workflow.EventSettingsOf(n); s.Mode != "summoned" {
				t.Fatal("a layout save lost a node's settings")
			}
		}
	}

	// With SHOTS set to a folder, save pictures of the canvas and the settings modal.
	if dir := os.Getenv("SHOTS"); dir != "" {
		var canvas, modal []byte
		chromedp.Run(ctx, chromedp.FullScreenshot(&canvas, 90),
			chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`),
			chromedp.Sleep(300*time.Millisecond), chromedp.FullScreenshot(&modal, 90))
		os.WriteFile(dir+"/canvas.jpg", canvas, 0o644)
		os.WriteFile(dir+"/modal.jpg", modal, 0o644)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("the page logged errors:\n%s", strings.Join(problems, "\n"))
	}
}
