//go:build browser

// The Presets pages in a real browser, driven as a person would: make a preset, copy it, edit the copy,
// remove it in two steps. Run with:
//
//	go test -p 1 -tags browser -run BrowserPresets ./internal/web/
package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestBrowserPresetsCreateCopyEditRemove(t *testing.T) {
	e := newEnv(t)
	e.user("erin", false)
	ctx, done := chromeContext(t)
	defer done()
	// until waits for the page to say so; a page that is still loading is not an error.
	until := func(js string) chromedp.Action {
		return chromedp.ActionFunc(func(c context.Context) error {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				var ok bool
				if err := chromedp.Evaluate(js, &ok).Do(c); err == nil && ok {
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			return fmt.Errorf("the page never said %s", js)
		})
	}
	text := func(sel string) string {
		var s string
		chromedp.Run(ctx, chromedp.Text(sel, &s, chromedp.ByQuery))
		return strings.TrimSpace(s)
	}

	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(1200, 900),
		chromedp.Navigate(e.srv.URL+"/login?next="+url.QueryEscape("/presets")),
		chromedp.SendKeys(`input[name=username]`, "erin"),
		chromedp.SendKeys(`input[name=password]`, "password-1234"),
		chromedp.Submit(`input[name=password]`),
		chromedp.WaitVisible(`a[href="/presets/new"]`),
	)
	if err != nil {
		t.Fatalf("opening the presets page: %v", err)
	}
	if shared := text(`section.panel`); !strings.Contains(shared, "Shared") {
		t.Fatalf("the first panel: %q", shared)
	}
	var sharedRows int
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('section.panel')[0].querySelectorAll('tbody tr').length`, &sharedRows))
	if sharedRows != 5 {
		t.Fatalf("%d shared presets listed, want the 5 built-in ones", sharedRows)
	}

	// Make one.
	err = chromedp.Run(ctx,
		chromedp.Click(`a[href="/presets/new"]`, chromedp.ByQuery),
		chromedp.WaitVisible(`input[name=name]`),
		chromedp.SetValue(`input[name=name]`, "Terse reviewer", chromedp.ByQuery),
		chromedp.SetValue(`input[name=description]`, "Short and to the point", chromedp.ByQuery),
		chromedp.SetValue(`textarea[name=pr_instruction]`, "Review {pr:title} briefly.", chromedp.ByQuery),
		chromedp.SetValue(`textarea[name=pr_goal]`, "Reviewed.", chromedp.ByQuery),
		chromedp.SetValue(`textarea[name=pr_summary]`, "**Verdict:** <one word>", chromedp.ByQuery),
		chromedp.SetValue(`textarea[name=issue_instruction]`, "Look at {issue:title} briefly.", chromedp.ByQuery),
		chromedp.SetValue(`textarea[name=issue_goal]`, "Looked at.", chromedp.ByQuery),
		chromedp.Click(`form.stack button`, chromedp.ByQuery),
		until(`document.querySelector('p.notice') && document.querySelector('p.notice').textContent.trim() === 'Saved.'`),
	)
	if err != nil {
		t.Fatalf("making a preset: %v", err)
	}
	if h := text(`h1`); h != "Terse reviewer" {
		t.Fatalf("the new preset's page: %q", h)
	}
	var summary string
	chromedp.Run(ctx, chromedp.Value(`textarea[name=pr_summary]`, &summary, chromedp.ByQuery))
	if summary != "**Verdict:** <one word>" {
		t.Fatalf("the summary format: %q", summary)
	}

	// Copy it: the copy opens, owned by the person, ready to edit.
	err = chromedp.Run(ctx,
		chromedp.Click(`form[action$="/copy"] button`, chromedp.ByQuery),
		until(`document.querySelector('p.notice') && document.querySelector('p.notice').textContent.trim() === 'This is your copy.'`),
	)
	if err != nil {
		t.Fatalf("copying: %v", err)
	}
	if h := text(`h1`); h != "Copy of Terse reviewer" {
		t.Fatalf("the copy's page: %q", h)
	}

	// Edit the copy and save.
	err = chromedp.Run(ctx,
		chromedp.SetValue(`input[name=name]`, "Terse, but kind", chromedp.ByQuery),
		chromedp.SetValue(`textarea[name=pr_goal]`, "Reviewed kindly.", chromedp.ByQuery),
		chromedp.Click(`form.stack button`, chromedp.ByQuery),
		until(`document.querySelector('p.notice') && document.querySelector('p.notice').textContent.trim() === 'Saved.' && document.querySelector('h1').textContent === 'Terse, but kind'`),
	)
	if err != nil {
		t.Fatalf("editing: %v", err)
	}
	var goal string
	chromedp.Run(ctx, chromedp.Value(`textarea[name=pr_goal]`, &goal, chromedp.ByQuery))
	if goal != "Reviewed kindly." {
		t.Fatalf("the saved goal: %q", goal)
	}

	// The list shows both, under Mine, each with one action.
	err = chromedp.Run(ctx, chromedp.Navigate(e.srv.URL+"/presets"), chromedp.WaitVisible(`a[href="/presets/new"]`))
	if err != nil {
		t.Fatal(err)
	}
	var mine string
	chromedp.Run(ctx, chromedp.Evaluate(`document.querySelectorAll('section.panel')[1].textContent`, &mine))
	if !strings.Contains(mine, "Terse reviewer") || !strings.Contains(mine, "Terse, but kind") || strings.Count(mine, "Open") != 2 {
		t.Fatalf("Mine: %q", mine)
	}

	// Remove it in two steps: the page first, then the button.
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`Array.from(document.querySelectorAll('section.panel')[1].querySelectorAll('tr')).find(function(r){return r.textContent.indexOf('Terse, but kind')>=0}).querySelector('a').click()`, nil),
		until(`document.querySelector('h1') && document.querySelector('h1').textContent === 'Terse, but kind'`),
		chromedp.Click(`a[href$="/remove"]`, chromedp.ByQuery),
		until(`document.querySelector('h1') && document.querySelector('h1').textContent === 'Remove Terse, but kind?'`),
	)
	if err != nil {
		t.Fatalf("the removal page: %v", err)
	}
	if body := text(`main`); !strings.Contains(body, "No block follows it.") {
		t.Fatalf("the removal page: %q", body)
	}
	if _, err := e.st.PresetByKey("copy_of_terse_reviewer"); err != nil {
		t.Fatal("the first step removed the preset")
	}
	err = chromedp.Run(ctx,
		chromedp.Click(`form[action$="/remove"] button`, chromedp.ByQuery),
		until(`document.querySelector('p.notice') && document.querySelector('p.notice').textContent.trim() === 'Preset removed.'`),
	)
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	if _, err := e.st.PresetByKey("copy_of_terse_reviewer"); err == nil {
		t.Fatal("the preset is still there")
	}
	if all, _ := e.st.Presets(); len(all) != 6 {
		t.Fatalf("%d presets left, want 5 built-in and the first one made", len(all))
	}
}
