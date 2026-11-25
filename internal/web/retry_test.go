package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

func TestDefaultRetryBackoffs(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 20 * time.Minute, 60 * time.Minute}
	if len(retryBackoffs) != len(want) {
		t.Fatalf("retryBackoffs = %v, want %v", retryBackoffs, want)
	}
	for i := range want {
		if retryBackoffs[i] != want[i] {
			t.Fatalf("retryBackoffs[%d] = %v, want %v", i, retryBackoffs[i], want[i])
		}
	}
}

func TestTransientGitHubRequeueAndRecover(t *testing.T) {
	orig := retryBackoffs
	retryBackoffs = []time.Duration{1 * time.Second, 0, 0, 0}
	t.Cleanup(func() { retryBackoffs = orig })

	var mu sync.Mutex
	postAttempts := 0
	srvGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1":
			w.Write([]byte(`{"number":1,"html_url":"https://github.com/o/r/pull/1","title":"PR 1","body":"b","state":"open","user":{"login":"alice"},"head":{"sha":"aaa"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1/files":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/1/comments":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/1/comments":
			mu.Lock()
			postAttempts++
			n := postAttempts
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":99,"html_url":"https://github.com/o/r/pull/1#issuecomment-99"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srvGH.Close)

	e := newEnv(t)
	admin := e.user("admin", true)
	ws, err := e.st.CreateWorkspace("o/r", admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetGitHub(ws.ID, "bot", "tok", admin); err != nil {
		t.Fatal(err)
	}
	e.ui.gh = &github.Client{BaseURL: srvGH.URL}

	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev", Type: "pr_ready"},
			{ID: "ce", Type: workflow.CaseEntry, Name: "Note", Settings: mustJSON(workflow.CaseEntrySettings{Entry: "Recorded once", Finalize: true})},
			{ID: "po", Type: workflow.Post, Name: "Post GH", Settings: mustJSON(workflow.PostSettings{GitHub: true, Message: "Hello PR 1"})},
			{ID: "ce2", Type: workflow.CaseEntry, Name: "After Post", Settings: mustJSON(workflow.CaseEntrySettings{Entry: "Posted ok", Finalize: true})},
		},
		Links: []workflow.Link{
			{From: "ev", Out: "out", To: "ce", In: "in"},
			{From: "ce", Out: "out", To: "po", In: "in"},
			{From: "po", Out: "out", To: "ce2", In: "in"},
		},
	}
	raw, _ := json.Marshal(g)
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw), admin); err != nil {
		t.Fatal(err)
	}

	runID, err := e.ui.startWorkflow(context.Background(), ws, workflow.PR, "ev", 1, false, admin)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the first attempt to hit 500 and return to the queue with retry_count=1.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := e.st.WorkflowRun(runID)
		if r != nil && r.Status == "queued" && r.RetryCount == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r, err := e.st.WorkflowRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "queued" || r.RetryCount != 1 || r.RetryAfter.IsZero() {
		t.Fatalf("after 500: status=%q retry_count=%d retry_after=%v", r.Status, r.RetryCount, r.RetryAfter)
	}
	// While cooling down, it must not be eligible in QueuedRunsFor yet.
	if ready, _ := e.st.QueuedRunsFor(ws.ID, false); len(ready) != 0 {
		t.Fatalf("cooling-down run should not be ready yet: %v", ready)
	}
	// Upstream step 'ce' stays done; failed 'po' and downstream 'ce2' were reset to pending.
	steps, _ := e.st.WorkflowSteps(runID)
	if steps["ce"].Status != "done" || steps["po"].Status != "pending" || steps["ce2"].Status != "pending" {
		t.Fatalf("unexpected step states while queued for retry: ce=%s po=%s ce2=%s", steps["ce"].Status, steps["po"].Status, steps["ce2"].Status)
	}

	// Wait for the 1s backoff to elapse, then dispatch (as happens when another run finishes or RunQueue ticks).
	time.Sleep(1200 * time.Millisecond)
	e.ui.dispatch(ws.ID)

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ = e.st.WorkflowRun(runID)
		if r != nil && r.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.Status != "done" {
		steps, _ = e.st.WorkflowSteps(runID)
		t.Fatalf("run did not recover to done: status=%q steps=%+v", r.Status, steps)
	}
	steps, _ = e.st.WorkflowSteps(runID)
	if steps["po"].Status != "done" || steps["ce2"].Status != "done" {
		t.Fatalf("steps after retry: po=%+v ce2=%+v", steps["po"], steps["ce2"])
	}
}

func TestTransientGitHubDeduplicatesCommentAndExhaustsBackoffs(t *testing.T) {
	orig := retryBackoffs
	retryBackoffs = []time.Duration{0, 0, 0, 0}
	t.Cleanup(func() { retryBackoffs = orig })

	var mu sync.Mutex
	pr1Posts := 0
	pr2Posts := 0
	srvGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1":
			w.Write([]byte(`{"number":1,"html_url":"https://github.com/o/r/pull/1","title":"PR 1","body":"b","state":"open","user":{"login":"alice"},"head":{"sha":"aaa"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/2":
			w.Write([]byte(`{"number":2,"html_url":"https://github.com/o/r/pull/2","title":"PR 2","body":"b","state":"open","user":{"login":"alice"},"head":{"sha":"bbb"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/o/r/pulls/1/files" || r.URL.Path == "/repos/o/r/pulls/2/files"):
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/1/comments":
			mu.Lock()
			n := pr1Posts
			mu.Unlock()
			if n >= 1 {
				// Simulate GitHub having actually saved the comment before returning 500!
				w.Write([]byte(`[{"body":"Unique review body","created_at":"` + time.Now().UTC().Format(time.RFC3339) + `","user":{"login":"bot"}}]`))
				return
			}
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/1/comments":
			mu.Lock()
			pr1Posts++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/2/comments":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/2/comments":
			mu.Lock()
			pr2Posts++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srvGH.Close)

	e := newEnv(t)
	admin := e.user("admin", true)
	ws, err := e.st.CreateWorkspace("o/r", admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetGitHub(ws.ID, "bot", "tok", admin); err != nil {
		t.Fatal(err)
	}
	e.ui.gh = &github.Client{BaseURL: srvGH.URL}

	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev", Type: "pr_ready"},
			{ID: "po", Type: workflow.Post, Name: "Post GH", Settings: mustJSON(workflow.PostSettings{GitHub: true, Message: "Unique review body"})},
		},
		Links: []workflow.Link{{From: "ev", Out: "out", To: "po", In: "in"}},
	}
	raw, _ := json.Marshal(g)
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw), admin); err != nil {
		t.Fatal(err)
	}

	// Case 1: GitHub saved the comment before returning 500 -> retry sees it and succeeds with 0 duplicates.
	run1, err := e.ui.startWorkflow(context.Background(), ws, workflow.PR, "ev", 1, false, admin)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitUntil) {
		r, _ := e.st.WorkflowRun(run1)
		if r.Status == "done" {
			break
		}
		if r.Status == "queued" {
			e.ui.dispatch(ws.ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r, _ := e.st.WorkflowRun(run1); r.Status != "done" {
		t.Fatalf("PR 1 expected done via dedup, got %q", r.Status)
	}
	mu.Lock()
	got1 := pr1Posts
	mu.Unlock()
	if got1 != 1 {
		t.Fatalf("PR 1 posted %d times, want 1 (no duplicate comment)", got1)
	}

	// Case 2: Persistent 500s exhaust all 4 retries (5 total POST attempts) and then fail.
	run2, err := e.ui.startWorkflow(context.Background(), ws, workflow.PR, "ev", 2, false, admin)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil = time.Now().Add(5 * time.Second)
	for time.Now().Before(waitUntil) {
		r, _ := e.st.WorkflowRun(run2)
		if r.Status == "failed" {
			break
		}
		if r.Status == "queued" {
			e.ui.dispatch(ws.ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r2, _ := e.st.WorkflowRun(run2)
	if r2.Status != "failed" || r2.RetryCount != 4 {
		t.Fatalf("PR 2 expected failed with retry_count=4, got status=%q retry_count=%d", r2.Status, r2.RetryCount)
	}
	mu.Lock()
	got2 := pr2Posts
	mu.Unlock()
	if got2 != 5 {
		t.Fatalf("PR 2 total POST attempts = %d, want 5 (1 initial + 4 retries)", got2)
	}
}

func TestManualResumeFailedRun(t *testing.T) {
	var mu sync.Mutex
	allowPost := false
	postCalls := 0
	srvGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/3":
			w.Write([]byte(`{"number":3,"html_url":"https://github.com/o/r/pull/3","title":"PR 3","body":"b","state":"open","user":{"login":"alice"},"head":{"sha":"ccc"},"base":{"ref":"main"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/3/files":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/3/comments":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/3/comments":
			mu.Lock()
			ok := allowPost
			postCalls++
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusForbidden) // permanent error -> fails immediately without auto-retry
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":301,"html_url":"https://github.com/o/r/pull/3#issuecomment-301"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srvGH.Close)

	e := newEnv(t)
	admin := e.user("admin", true)
	ws, err := e.st.CreateWorkspace("o/r", admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetGitHub(ws.ID, "bot", "tok", admin); err != nil {
		t.Fatal(err)
	}
	e.ui.gh = &github.Client{BaseURL: srvGH.URL}

	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev", Type: "pr_ready"},
			{ID: "ce", Type: workflow.CaseEntry, Name: "First Note", Settings: mustJSON(workflow.CaseEntrySettings{Entry: "Step one done", Finalize: true})},
			{ID: "po", Type: workflow.Post, Name: "Post GH", Settings: mustJSON(workflow.PostSettings{GitHub: true, Message: "Resume test comment"})},
			{ID: "ce2", Type: workflow.CaseEntry, Name: "Second Note", Settings: mustJSON(workflow.CaseEntrySettings{Entry: "Step two done", Finalize: true})},
		},
		Links: []workflow.Link{
			{From: "ev", Out: "out", To: "ce", In: "in"},
			{From: "ce", Out: "out", To: "po", In: "in"},
			{From: "po", Out: "out", To: "ce2", In: "in"},
		},
	}
	raw, _ := json.Marshal(g)
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw), admin); err != nil {
		t.Fatal(err)
	}

	runID, err := e.ui.startWorkflow(context.Background(), ws, workflow.PR, "ev", 3, false, admin)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := e.st.WorkflowRun(runID)
		if r != nil && r.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r, _ := e.st.WorkflowRun(runID)
	if r.Status != "failed" || r.RetryCount != 0 {
		t.Fatalf("permanent 403 should fail immediately: status=%q retry_count=%d", r.Status, r.RetryCount)
	}
	stepsBefore, _ := e.st.WorkflowSteps(runID)
	ceFinishedAt := stepsBefore["ce"].FinishedAt
	if stepsBefore["ce"].Status != "done" || stepsBefore["po"].Status != "failed" || stepsBefore["ce2"].Status != "skipped" {
		t.Fatalf("unexpected pre-resume steps: %+v", stepsBefore)
	}

	// Verify the run page renders the Resume button, then click it after fixing the GitHub error.
	b := e.signIn("admin")
	code, body := b.get(fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), runID))
	if code != http.StatusOK || !strings.Contains(body, "Resume from failed step") {
		t.Fatalf("run page missing Resume button: code=%d", code)
	}
	mu.Lock()
	allowPost = true
	mu.Unlock()
	code, _, _ = b.post(fmt.Sprintf("%s/workflow-runs/%d/resume", ws.Path(), runID), url.Values{"csrf": {b.csrf()}})
	if code != http.StatusSeeOther {
		t.Fatalf("POST resume = %d, want 303", code)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ = e.st.WorkflowRun(runID)
		if r != nil && r.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.Status != "done" {
		stepsAfter, _ := e.st.WorkflowSteps(runID)
		t.Fatalf("resumed run did not finish done: status=%q steps=%+v", r.Status, stepsAfter)
	}
	stepsAfter, _ := e.st.WorkflowSteps(runID)
	if !stepsAfter["ce"].FinishedAt.Equal(ceFinishedAt) {
		t.Fatalf("completed step 'ce' was re-run! before=%v after=%v", ceFinishedAt, stepsAfter["ce"].FinishedAt)
	}
	if stepsAfter["po"].Status != "done" || stepsAfter["ce2"].Status != "done" {
		t.Fatalf("resumed downstream steps not done: po=%+v ce2=%+v", stepsAfter["po"], stepsAfter["ce2"])
	}
}
