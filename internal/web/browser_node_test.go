//go:build browser

// The Node block in a real browser: placed from the palette, with several links into and out of it.
// Run with:
//
//	go test -p 1 -tags browser -run BrowserNode ./internal/web/
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

	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestBrowserNodeBlock(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	ctx, done := chromeContext(t)
	defer done()
	stored := func() workflow.Graph {
		w, err := e.st.Workflow(ws.ID, workflow.PR)
		if err != nil {
			t.Fatal(err)
		}
		var g workflow.Graph
		json.Unmarshal([]byte(w.Graph), &g)
		return g
	}
	waitSaved := chromedp.Poll(`document.getElementById('wf-status').textContent.indexOf('Saved') === 0`, nil, chromedp.WithPollingTimeout(10*time.Second))
	place := func(label string, x, y float64) chromedp.Tasks {
		return chromedp.Tasks{
			chromedp.Evaluate(`document.getElementById('wf-status').textContent = ''`, nil),
			chromedp.MouseClickXY(x-60, y-60, chromedp.ButtonType(input.Right)),
			chromedp.Click(fmt.Sprintf(`//div[@id="wf-menu"]/button[normalize-space()=%q]`, label), chromedp.BySearch),
			chromedp.MouseEvent(input.MouseMoved, x, y),
			chromedp.MouseClickXY(x, y),
			waitSaved,
		}
	}
	center := func(sel string, v *[]float64) chromedp.Action {
		return chromedp.Evaluate(`(function(){var r=document.querySelector('`+sel+`').getBoundingClientRect();return [r.x+r.width/2,r.y+r.height/2]})()`, v)
	}
	link := func(from, to string) chromedp.Tasks {
		var a, b []float64
		return chromedp.Tasks{
			center(from, &a), center(to, &b),
			chromedp.Evaluate(`document.getElementById('wf-status').textContent = ''`, nil),
			chromedp.ActionFunc(func(ctx context.Context) error {
				for _, act := range []chromedp.Action{
					chromedp.MouseEvent(input.MouseMoved, a[0], a[1]),
					chromedp.MouseEvent(input.MousePressed, a[0], a[1], chromedp.ButtonLeft, chromedp.ClickCount(1)),
					chromedp.MouseEvent(input.MouseMoved, (a[0]+b[0])/2, (a[1]+b[1])/2, chromedp.ButtonLeft),
					chromedp.MouseEvent(input.MouseMoved, b[0], b[1], chromedp.ButtonLeft),
					chromedp.MouseEvent(input.MouseReleased, b[0], b[1], chromedp.ButtonLeft, chromedp.ClickCount(1)),
					waitSaved,
				} {
					if err := act.Do(ctx); err != nil {
						return err
					}
				}
				return nil
			}),
		}
	}
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#wf .drawflow`),
		place("PR First Ready", 300, 250),
		place("PR Draft", 300, 600),
		place("Node", 650, 420),
		place("Case Entry", 1000, 250),
		place("Case Entry", 1000, 600),
	)
	if err != nil {
		t.Fatalf("placing blocks: %v", err)
	}
	g := stored()
	byType := map[string][]workflow.Node{}
	for _, n := range g.Nodes {
		byType[n.Type] = append(byType[n.Type], n)
	}
	if len(g.Nodes) != 5 || len(byType[workflow.Junction]) != 1 || byType[workflow.Junction][0].Name != "" {
		t.Fatalf("stored after placing: %+v", g.Nodes)
	}
	// Node 3 is the Node; two events link in, and it links out to both entries.
	err = chromedp.Run(ctx,
		link(`#node-1 .output_1`, `#node-3 .input_1`),
		link(`#node-2 .output_1`, `#node-3 .input_1`),
		link(`#node-3 .output_1`, `#node-4 .input_1`),
		link(`#node-3 .output_1`, `#node-5 .input_1`),
	)
	if err != nil {
		t.Fatalf("linking: %v", err)
	}
	j := byType[workflow.Junction][0].ID
	g = stored()
	in, out := 0, 0
	for _, l := range g.Links {
		if l.To == j && l.In == "in" && l.Out == "out" {
			in++
		}
		if l.From == j && l.Out == "out" {
			out++
		}
	}
	if len(g.Links) != 4 || in != 2 || out != 2 {
		t.Fatalf("links: %+v", g.Links)
	}
	var title, sub, name string
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector("#node-3 .wf-title").textContent`, &title), chromedp.Text(`#node-3 .wf-sub`, &sub), chromedp.Text(`#node-3 .wf-name`, &name))
	if err != nil || title != "Node" || sub != "connects links" || name != "Node" {
		t.Fatalf("the card: %q %q %q, %v", title, sub, name, err)
	}
	if err := workflow.Graph(g).Check(workflow.PR); err != nil {
		t.Fatal(err)
	}
}
