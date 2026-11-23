//go:build browser

// A run's page in a real browser takes in what is new without reloading. Run with:
//
//	go test -p 1 -tags browser -run BrowserLive ./internal/web/
package web

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestBrowserLiveRunPage(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	m, _, err := e.st.AddMachine(ws.ID, "cn1", derek)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := e.st.QueueRun(m, "make test", "", "", false, 0, time.Minute, derek)
	e.st.StartRun(run.ID)
	e.st.AppendOutput(run.ID, "first line\n", 1<<20)

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

	page := fmt.Sprintf("/w/o/r/runs/%d", run.ID)
	var marker, open bool
	var fetches int
	err = chromedp.Run(ctx,
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape(page)),
		chromedp.SendKeys(`input[name=username]`, "derek"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`#run-output`),
		// Something the person did on the page: a reload would undo both.
		chromedp.Evaluate(`window.notReloaded = true; document.querySelector('details').open = true; true`, nil),
		chromedp.ActionFunc(func(context.Context) error {
			_, _, err := e.st.AppendOutput(run.ID, "second line\n", 1<<20)
			return err
		}),
		chromedp.Poll(`document.getElementById('run-output').textContent.includes('second line')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.ActionFunc(func(context.Context) error { return e.st.FinishRun(run.ID, "passed", "Passed.") }),
		chromedp.Poll(`document.getElementById('run-title').textContent.includes('passed') && !document.getElementById('run-title').querySelector('form')`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`window.notReloaded === true`, &marker),
		chromedp.Evaluate(`document.querySelector('details').open`, &open),
		// Once the run is over the page stops asking.
		chromedp.Sleep(7*time.Second),
		chromedp.Evaluate(`performance.getEntriesByType('resource').filter(function(r){return r.initiatorType==='fetch' && r.startTime > performance.now()-6500}).length`, &fetches),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !marker || !open {
		t.Fatalf("the page reloaded (marker %v) or closed what was open (details %v)", marker, open)
	}
	if fetches != 0 {
		t.Fatalf("%d fetches after the run ended", fetches)
	}
}
