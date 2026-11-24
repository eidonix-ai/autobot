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
	"math"
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

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestBrowserWorkflowCanvas(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	e.st.SetOptOutTag(ws, "autobot: skip", admin) // the workspace's; the event block shows it
	prov, err := e.st.CreateProvider(store.ProviderEdit{Name: "Eidonix", Kind: store.KindOpenAI, Endpoint: "https://llm.example/v1"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	e.st.SetProviderModels(prov.ID, []string{"GLM-5.3-Flash"})
	e.st.SetProviderRoutes(prov.ID, true, false)
	e.st.LendProvider(prov, []int64{ws.ID}, admin)

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
			// The one refusal the test provokes (an unknown tag) is answered 400, as it should be.
			if ev.Entry.Level == log.LevelError && !strings.Contains(ev.Entry.Text, "status of 400") {
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
	waitSaved := chromedp.Poll(`document.getElementById('wf-status').textContent.indexOf('Saved') === 0`, nil, chromedp.WithPollingTimeout(10*time.Second))
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
	err = chromedp.Run(ctx,
		chromedp.EmulateViewport(1400, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape("/w/o/r/workflows/pr")),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#wf .drawflow`),
		place("PR First Ready", 400, 300),
	)
	if err != nil {
		t.Fatalf("placing PR First Ready on %s: %v", page, err)
	}
	g := stored()
	if len(g.Nodes) != 1 || g.Nodes[0].Type != "pr_ready" {
		t.Fatalf("stored after placing: %+v", g.Nodes)
	}
	id := g.Nodes[0].ID

	// Open its settings with a click, change them, save.
	var summary, name string
	err = chromedp.Run(ctx,
		chromedp.Click(`#node-1 .wf-title`, chromedp.ByQuery),
		chromedp.WaitVisible(`#wf-modal form.node-form`),
		chromedp.Click(`#wf-modal input[name=mode][value=summoned]`),
		chromedp.SetValue(`#wf-modal input[name=summon_phrase]`, "autobot: go"),
		chromedp.SetValue(`#wf-modal input[name=name]`, "New ready PRs"),
		chromedp.Click(`#wf-modal form.node-form button:not([type])`),
		waitSaved,
		chromedp.Text(`#node-1 .wf-sub`, &summary),
		chromedp.Text(`#node-1 .wf-name`, &name),
	)
	if err != nil {
		var status, modalText string
		chromedp.Run(ctx, chromedp.Text(`#wf-status`, &status, chromedp.ByQuery), chromedp.Evaluate(`document.getElementById('wf-modal').innerText`, &modalText))
		t.Fatalf("settings: %v\nstatus: %q\nmodal: %s", err, status, modalText)
	}
	s, _ := workflow.EventSettingsOf(stored().Nodes[0])
	if s.Mode != "summoned" || s.SummonPhrase != "autobot: go" || stored().Nodes[0].ID != id {
		t.Fatalf("stored settings: %+v", s)
	}
	if name != "New ready PRs" || stored().Nodes[0].Name != "New ready PRs" {
		t.Fatalf("node name shown %q, stored %q", name, stored().Nodes[0].Name)
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

	// Steps: a decision has one labelled output per choice, and links follow its choices.
	nodeOf := func(typ string) workflow.Node {
		for _, n := range stored().Nodes {
			if n.Type == typ {
				return n
			}
		}
		t.Fatalf("no %s node stored", typ)
		return workflow.Node{}
	}
	saveForm := chromedp.Tasks{chromedp.Click(`#wf-modal form.node-form button:not([type])`), waitSaved}
	var labels []string
	err = chromedp.Run(ctx,
		place("Decision", 400, 620),
		chromedp.Click(`#node-3 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal textarea[name=choices]`),
		chromedp.SetValue(`#wf-modal textarea[name=choices]`, "approve: safe\nrequest-changes: needs work\nhold: wait for CI"),
		saveForm,
		chromedp.Evaluate(`Array.from(document.querySelectorAll('#node-3 .outputs .output')).map(function(p){return p.dataset.label})`, &labels),
		place("Case Entry", 900, 650),
	)
	if err != nil || strings.Join(labels, ",") != "approve,request-changes,hold" {
		t.Fatalf("decision ports %v: %v", labels, err)
	}
	d := nodeOf(workflow.Decision)
	if ds, _ := workflow.DecisionSettingsOf(d, workflow.PR); !ds.Model.Set() || len(ds.Choices) != 3 {
		t.Fatalf("decision settings %+v", ds)
	}
	center := func(sel string, v *[]float64) chromedp.Action {
		return chromedp.Evaluate(`(function(){var r=document.querySelector('`+sel+`').getBoundingClientRect();return [r.x+r.width/2,r.y+r.height/2]})()`, v)
	}
	var from, to []float64
	err = chromedp.Run(ctx, center(`#node-3 .output_3`, &from), center(`#node-4 .input_1`, &to))
	if err == nil {
		err = chromedp.Run(ctx,
			chromedp.Evaluate(`document.getElementById('wf-status').textContent = ''`, nil),
			chromedp.MouseEvent(input.MouseMoved, from[0], from[1]),
			chromedp.MouseEvent(input.MousePressed, from[0], from[1], chromedp.ButtonLeft, chromedp.ClickCount(1)),
			chromedp.MouseEvent(input.MouseMoved, (from[0]+to[0])/2, (from[1]+to[1])/2, chromedp.ButtonLeft),
			chromedp.MouseEvent(input.MouseMoved, to[0], to[1], chromedp.ButtonLeft),
			chromedp.MouseEvent(input.MouseReleased, to[0], to[1], chromedp.ButtonLeft, chromedp.ClickCount(1)),
			waitSaved)
	}
	entry := nodeOf(workflow.CaseEntry)
	if g := stored(); err != nil || len(g.Links) != 1 || g.Links[0] != (workflow.Link{From: d.ID, Out: "hold", To: entry.ID, In: "in"}) {
		t.Fatalf("link from a choice: %+v, %v", g.Links, err)
	}
	var ports int
	err = chromedp.Run(ctx,
		chromedp.Click(`#node-3 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal textarea[name=choices]`),
		chromedp.SetValue(`#wf-modal textarea[name=choices]`, "approve: safe\nrequest-changes: needs work"),
		saveForm,
		chromedp.Evaluate(`document.querySelectorAll('#node-3 .outputs .output').length`, &ports))
	if g := stored(); err != nil || ports != 2 || len(g.Links) != 0 {
		t.Fatalf("after dropping a choice: %d ports, links %+v, %v", ports, g.Links, err)
	}

	// An agent is named, which gives its output the tag {value_summary}; a step after it may use the tag
	// (one that is not after it may not); an unknown tag is refused.
	var problem string
	err = chromedp.Run(ctx,
		place("Agent", 700, 250),
		chromedp.Click(`#node-5 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal input[name=name]`),
		chromedp.SetValue(`#wf-modal input[name=name]`, "Value summary"),
		saveForm,
		chromedp.Poll(`!document.getElementById('wf-modal').matches(':popover-open')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		center(`#node-5 .output_1`, &from), center(`#node-4 .input_1`, &to),
	)
	var renamed string
	chromedp.Run(ctx, chromedp.Text(`#wf-status`, &renamed, chromedp.ByQuery))
	if renamed != "Saved. Renamed {agent} to {value_summary} in 0 blocks" {
		t.Fatalf("the bar after naming the agent: %q", renamed)
	}
	if err == nil { // the points are known now
		err = chromedp.Run(ctx,
			chromedp.Evaluate(`document.getElementById('wf-status').textContent = ''`, nil),
			chromedp.MouseEvent(input.MouseMoved, from[0], from[1]),
			chromedp.MouseEvent(input.MousePressed, from[0], from[1], chromedp.ButtonLeft, chromedp.ClickCount(1)),
			chromedp.MouseEvent(input.MouseMoved, (from[0]+to[0])/2, (from[1]+to[1])/2, chromedp.ButtonLeft),
			chromedp.MouseEvent(input.MouseMoved, to[0], to[1], chromedp.ButtonLeft),
			chromedp.MouseEvent(input.MouseReleased, to[0], to[1], chromedp.ButtonLeft, chromedp.ClickCount(1)),
			waitSaved)
	}
	if err == nil {
		err = chromedp.Run(ctx,
			chromedp.Click(`#node-4 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal textarea[name=entry]`),
			chromedp.SetValue(`#wf-modal textarea[name=entry]`, "{nope}"),
			chromedp.Click(`#wf-modal form.node-form button:not([type])`),
			chromedp.WaitVisible(`#wf-modal p.error`), chromedp.Text(`#wf-modal p.error`, &problem),
			chromedp.SetValue(`#wf-modal textarea[name=entry]`, "Summary: {value_summary}"),
			saveForm)
	}
	if err != nil || !strings.Contains(problem, "{nope} is not a tag") {
		t.Fatalf("tags: %q, %v", problem, err)
	}
	// A preset's Instruction is shown read-only; Custom unlocks it with the same text; another preset swaps it.
	var locked, unlocked bool
	var keptText, swapped string
	err = chromedp.Run(ctx,
		chromedp.Click(`#node-5 .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal select[name=persona]`),
		chromedp.Evaluate(`document.querySelector('#wf-modal textarea[name=instruction]').readOnly`, &locked),
		chromedp.SetValue(`#wf-modal select[name=persona]`, "custom"),
		chromedp.Evaluate(`(function(){var s=document.querySelector('#wf-modal select[name=persona]');s.dispatchEvent(new Event('change',{bubbles:true}));return true})()`, nil),
		chromedp.Evaluate(`document.querySelector('#wf-modal textarea[name=instruction]').readOnly`, &unlocked),
		chromedp.Evaluate(`document.querySelector('#wf-modal textarea[name=instruction]').value`, &keptText),
		chromedp.SetValue(`#wf-modal select[name=persona]`, "code_quality"),
		chromedp.Evaluate(`(function(){var s=document.querySelector('#wf-modal select[name=persona]');s.dispatchEvent(new Event('change',{bubbles:true}));return true})()`, nil),
		chromedp.Evaluate(`document.querySelector('#wf-modal textarea[name=instruction]').value`, &swapped),
		chromedp.Click(`#wf-modal .close`, chromedp.ByQuery),
	)
	if err != nil || !locked || unlocked || !strings.Contains(keptText, "preparing the case file") || !strings.Contains(swapped, "correctness first") {
		t.Fatalf("presets in the form: locked %v, unlocked %v, kept %q, swapped %q, %v", locked, !unlocked, keptText[:min(40, len(keptText))], swapped[:min(40, len(swapped))], err)
	}
	if a, _ := workflow.AgentSettingsOf(nodeOf(workflow.Agent), workflow.PR); workflow.StoredOutputTo(nodeOf(workflow.Agent)) != "value_summary" || !a.Model.Set() {
		t.Fatalf("agent settings %+v", a)
	}
	if c, _ := workflow.CaseEntrySettingsOf(nodeOf(workflow.CaseEntry)); c.Entry != "Summary: {value_summary}" || !c.Finalize {
		t.Fatalf("case entry settings %+v", c)
	}

	// Shift-click tests a block: its tags are asked for, and a case entry shows what it would write.
	var preview string
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`(function(){var r=document.querySelector('#node-4 .wf-title').getBoundingClientRect();window.__p=[r.x+r.width/2,r.y+r.height/2]})()`, nil),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var pt []float64
			if err := chromedp.Evaluate(`window.__p`, &pt).Do(ctx); err != nil {
				return err
			}
			for _, typ := range []input.MouseType{input.MousePressed, input.MouseReleased} {
				if err := input.DispatchMouseEvent(typ, pt[0], pt[1]).WithButton(input.Left).WithClickCount(1).WithModifiers(input.ModifierShift).Do(ctx); err != nil {
					return err
				}
			}
			return nil
		}),
		chromedp.WaitVisible(`#wf-modal textarea[name=tag_value_summary]`),
		chromedp.SetValue(`#wf-modal textarea[name=tag_value_summary]`, "Ships safely."),
		chromedp.Click(`#wf-modal form.test-form button:not([type])`),
		chromedp.WaitVisible(`#wf-modal pre.entry`), chromedp.Text(`#wf-modal pre.entry`, &preview),
		chromedp.Click(`#wf-modal .close`, chromedp.ByQuery),
	)
	if err != nil || preview != "Summary: Ships safely." {
		t.Fatalf("shift-click test: %q, %v", preview, err)
	}

	// A drag on the background pans the canvas, and the dot grid moves with it.
	var grid, transform string
	err = chromedp.Run(ctx,
		chromedp.MouseEvent(input.MouseMoved, 1200, 800),
		chromedp.MouseEvent(input.MousePressed, 1200, 800, chromedp.ButtonLeft, chromedp.ClickCount(1)),
		chromedp.MouseEvent(input.MouseMoved, 1250, 830, chromedp.ButtonLeft),
		chromedp.MouseEvent(input.MouseReleased, 1250, 830, chromedp.ButtonLeft, chromedp.ClickCount(1)),
		chromedp.Evaluate(`document.getElementById('wf').style.backgroundPosition`, &grid),
		chromedp.Evaluate(`document.querySelector('#wf .drawflow').style.transform`, &transform),
	)
	if err != nil || grid != "50px 30px" || !strings.HasPrefix(transform, "translate(50px, 30px)") {
		t.Fatalf("panning: grid %q, canvas %q, %v", grid, transform, err)
	}

	// The wheel zooms about the mouse: the point under it stays put while the node grows.
	rect := func(v *[]float64) chromedp.Action {
		return chromedp.Evaluate(`(function(){var r=document.getElementById('node-1').getBoundingClientRect();return [r.x,r.y,r.width]})()`, v)
	}
	var before, after []float64
	err = chromedp.Run(ctx, rect(&before),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchMouseEvent(input.MouseWheel, before[0]+10, before[1]+10).WithDeltaX(0).WithDeltaY(-200).Do(ctx)
		}),
		chromedp.Sleep(100*time.Millisecond), rect(&after))
	if err != nil || len(after) != 3 || after[2] < before[2]*1.3 || math.Abs(after[0]-(before[0]+10-10*after[2]/before[2])) > 1.5 {
		t.Fatalf("zoom: before %v, after %v, %v", before, after, err)
	}

	// Panned out of sight, Fit brings the whole workflow back into view; a reload keeps the view.
	const inView = `(function(){var c=document.getElementById('wf').getBoundingClientRect(),ok=true;
		document.querySelectorAll('#wf .drawflow-node').forEach(function(n){var q=n.getBoundingClientRect();
		ok=ok&&q.left>=c.left&&q.top>=c.top&&q.right<=c.right&&q.bottom<=c.bottom});return ok})()`
	var lost, found, kept bool
	var viewBefore, viewAfter string
	const transformOf = `document.querySelector('#wf .drawflow').style.transform`
	err = chromedp.Run(ctx,
		chromedp.MouseEvent(input.MouseMoved, 1250, 780),
		chromedp.MouseEvent(input.MousePressed, 1250, 780, chromedp.ButtonLeft, chromedp.ClickCount(1)),
		chromedp.MouseEvent(input.MouseMoved, 700, 500, chromedp.ButtonLeft),
		chromedp.MouseEvent(input.MouseMoved, 100, 100, chromedp.ButtonLeft),
		chromedp.MouseEvent(input.MouseReleased, 100, 100, chromedp.ButtonLeft, chromedp.ClickCount(1)),
		chromedp.Evaluate(inView, &lost),
		chromedp.Click(`#wf-fit`, chromedp.ByQuery),
		chromedp.Evaluate(inView, &found),
		chromedp.Evaluate(transformOf, &viewBefore),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Reload(), chromedp.WaitVisible(`#wf .drawflow-node`),
		chromedp.Evaluate(transformOf, &viewAfter),
		chromedp.Evaluate(inView, &kept),
	)
	if err != nil || lost || !found || !kept || viewBefore != viewAfter {
		t.Fatalf("Fit and reload: out of view before Fit %v, in view after %v, after reload %v; view %q then %q; %v", !lost, found, kept, viewBefore, viewAfter, err)
	}

	// With SHOTS set to a folder, save pictures of the canvas and the settings modals.
	if dir := os.Getenv("SHOTS"); dir != "" {
		shot := func(name, node string) {
			var b []byte
			tasks := chromedp.Tasks{chromedp.Click(`#wf-fit`, chromedp.ByQuery), chromedp.Sleep(200 * time.Millisecond)}
			if node != "" {
				tasks = append(tasks, chromedp.Click(node+` .wf-title`, chromedp.ByQuery), chromedp.WaitVisible(`#wf-modal form.node-form`), chromedp.Sleep(300*time.Millisecond))
			}
			tasks = append(tasks, chromedp.FullScreenshot(&b, 90))
			if node != "" {
				tasks = append(tasks, chromedp.Click(`#wf-modal .close`, chromedp.ByQuery))
			}
			chromedp.Run(ctx, tasks)
			os.WriteFile(dir+"/"+name+".jpg", b, 0o644)
		}
		shot("canvas", "")
		shot("event", "#node-1")
		shot("decision", "#node-3")
		shot("agent", "#node-5")
		shot("case", "#node-4")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(problems) > 0 {
		t.Fatalf("the page logged errors:\n%s", strings.Join(problems, "\n"))
	}
}
