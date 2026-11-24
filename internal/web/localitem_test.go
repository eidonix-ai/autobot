package web

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/repo"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A test is filled from Autobot's own copies of the pull request and the code; GitHub is asked only
// for what they do not have.
func TestTestIsFilledFromTheLocalCopy(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	base, _ := localSource(t)
	root := strings.TrimPrefix(base, "file://")
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	work := filepath.Join(root, "work")
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\nfunc added() {}\n"), 0o644)
	git(work, "commit", "-qam", "add a function")
	head := git(work, "rev-parse", "HEAD")
	git(work, "push", "-q", filepath.Join(root, "o", "r.git"), "HEAD:refs/pull/3/head")

	e.ui.UseRepos(filepath.Join(e.dir, "test.db"))
	e.ui.repos.BaseURL = base
	ctx := context.Background()
	if err := e.ui.repos.Sync(ctx, ws.ID, "o/r", "tok-good", repo.Pulls{}); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	e.st.SaveItem(ws.ID, store.Item{Number: 3, Kind: "pr", Title: "From the copy", Body: "Adds a function.", State: "open", Author: "carol",
		Created: at, Updated: at}, nil)
	e.st.SavePull(ws.ID, 3, store.PullInfo{State: "open", Base: "main", HeadSHA: head, Updated: at})
	e.st.SaveFiles(ws.ID, 3, []store.ItemFile{{Path: "main.go", Status: "modified", Additions: 2}})

	fill := func(link string) map[string]string {
		v := map[string]string{"pr": link}
		if err := e.ui.fillFromItem(ctx, ws, v); err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		return v
	}
	// Until the copy of pull requests is complete, GitHub is asked (this fake knows only #7).
	if err := e.ui.fillFromItem(ctx, ws, map[string]string{"pr": "https://github.com/o/r/pull/3"}); err == nil {
		t.Fatal("read from a copy that is not complete yet")
	}
	e.st.IndexSet(ws.ID, "backfill_phase", "done")
	v := fill("https://github.com/o/r/pull/3")
	if v["pr:title"] != "From the copy" || v["pr:author"] != "carol" || v["pr"] != "https://github.com/o/r/pull/3" ||
		!strings.Contains(v["pr:diff"], "+func added() {}") || !strings.Contains(v["pr:files"], "main.go (modified, +2 -0)") {
		t.Fatalf("%q", v)
	}
	// An agent starts on it from the copy too: GitHub (this fake) does not know #3.
	llm, seen := llmServer(t)
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.LendProvider(p, []int64{ws.ID}, derek)
	a, _ := e.ui.resolved(workflow.DefaultAgent(workflow.PR))
	a.Model, a.Internet = workflow.ModelRef{Provider: p.ID, Model: "m"}, false
	id, err := e.ui.startAgent(ws, workflow.PR, workflow.Node{ID: "ag", Type: workflow.Agent, Name: "Gather"}, a, v, derek, true, 3)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for i := 0; i < 100; i++ {
		if r, _ := e.st.AgentRun(id); r.Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if all := strings.Join(seen(), "\n"); !strings.Contains(all, "Title: From the copy") || !strings.Contains(all, "func added") {
		t.Fatalf("the agent was not given the pull request from the copy:\n%.2000s", all)
	}

	// One the copy does not have comes from GitHub.
	if v := fill("https://github.com/o/r/pull/7"); v["pr:title"] != "Retry uploads" || !strings.Contains(v["pr:diff"], "retry()") {
		t.Fatalf("%q", v)
	}
}
