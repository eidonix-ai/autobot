package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/repo"
)

// localSource makes a git repository "o/r" with one commit, standing in for GitHub, and returns
// the address to clone from and the commit.
func localSource(t *testing.T) (base, commit string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare, work := filepath.Join(root, "o", "r.git"), filepath.Join(root, "work")
	os.MkdirAll(bare, 0o755)
	os.MkdirAll(work, 0o755)
	run(bare, "init", "--bare", "-b", "main")
	run(bare, "config", "uploadpack.allowFilter", "true")
	run(work, "init", "-b", "main")
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644)
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "first")
	run(work, "push", "-q", bare, "main")
	return "file://" + root, run(work, "rev-parse", "HEAD")
}

func toolNames(tools []agent.Tool) string {
	var n []string
	for _, t := range tools {
		n = append(n, t.Spec.Name)
	}
	slices.Sort(n)
	return strings.Join(n, " ")
}

// An agent reads the code from the local copy once the copy is ready and holds the pull request's
// head; otherwise, and when a sandbox has its own file tools, nothing it relied on goes missing.
func TestAgentToolsUseTheLocalCopy(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	other, _ := e.st.CreateWorkspace("o/other", derek, "", "")
	base, head := localSource(t)
	e.ui.UseRepos(filepath.Join(e.dir, "test.db"))
	e.ui.repos.BaseURL = base
	ctx := context.Background()

	pull := &agent.PR{Repo: "o/r", Info: &github.PullRequest{Number: 3, HeadSHA: head, Base: "main"}, Files: []github.ChangedFile{{Path: "main.go"}}}
	githubTools := func() []agent.Tool { return agent.PRTools(pull) }
	was := toolNames(githubTools())

	if got := toolNames(e.ui.withLocalCode(ctx, ws, "tok-good", pull, false, githubTools())); got != was {
		t.Fatalf("before the copy exists the tools should be unchanged: %s", got)
	}
	if err := e.ui.repos.Sync(ctx, ws.ID, "o/r", "tok-good", repo.Pulls{}); err != nil {
		t.Fatal(err)
	}
	tools := e.ui.withLocalCode(ctx, ws, "tok-good", pull, false, githubTools())
	if got := toolNames(tools); got != "list_files pr_comments pr_diff pr_files read_file repo_overview search_code search_history" {
		t.Fatalf("tools with the copy: %s", got)
	}
	for _, tl := range tools { // the files come from the copy: the GitHub reader here is nil and would crash
		if tl.Spec.Name == "read_file" {
			out, err := tl.Run(ctx, json.RawMessage(`{"path":"main.go"}`))
			if err != nil || !strings.Contains(out, "package main") {
				t.Fatalf("read_file: %q, %v", out, err)
			}
		}
	}
	// A pull request head the copy lacks and cannot fetch: the agent reads through GitHub.
	pull.Info = &github.PullRequest{Number: 4, HeadSHA: strings.Repeat("a", 40), Base: "main"}
	if got := toolNames(e.ui.withLocalCode(ctx, ws, "tok-good", pull, false, githubTools())); got != was {
		t.Errorf("an unknown head: %s", got)
	}
	// Another workspace has no copy yet.
	if got := toolNames(e.ui.withLocalCode(ctx, other, "", pull, false, githubTools())); got != was {
		t.Errorf("a workspace without a copy: %s", got)
	}
	if got := toolNames(e.ui.withLocalCode(ctx, other, "", nil, false, nil)); got != "" {
		t.Errorf("no copy and no pull request: %s", got)
	}
	// With no pull request the tools are the code tools only, on the default branch.
	if got := toolNames(e.ui.withLocalCode(ctx, ws, "tok-good", nil, false, nil)); got != "list_files read_file repo_overview search_code search_history" {
		t.Errorf("no pull request: %s", got)
	}
	// A sandbox's own read_file stays, and is not listed twice.
	pull.Info = &github.PullRequest{Number: 3, HeadSHA: head, Base: "main"}
	sandbox := agent.Tool{Spec: agent.ToolSpec{Name: "read_file", Description: "in the sandbox"}}
	have := slices.DeleteFunc(githubTools(), func(t agent.Tool) bool { return t.Spec.Name == "read_file" || t.Spec.Name == "list_files" }) // as startAgent does
	tools = e.ui.withLocalCode(ctx, ws, "tok-good", pull, true, append(have, sandbox))
	if n := strings.Count(" "+toolNames(tools)+" ", " read_file "); n != 1 {
		t.Errorf("read_file listed %d times: %s", n, toolNames(tools))
	}
	for _, tl := range tools {
		if tl.Spec.Name == "read_file" && tl.Spec.Description != "in the sandbox" {
			t.Error("the sandbox's read_file was replaced")
		}
	}
	// Without the local copies switched on, nothing changes.
	e.ui.repos = nil
	if got := toolNames(e.ui.withLocalCode(ctx, ws, "tok-good", pull, false, githubTools())); got != was {
		t.Errorf("repos off: %s", got)
	}
}

// The background round clones each workspace's repository with its token, removes the copies of
// workspaces that are gone, and the settings page shows the copy.
func TestRepoRoundAndSettings(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	base, _ := localSource(t)
	e.ui.UseRepos(filepath.Join(e.dir, "test.db"))
	e.ui.repos.BaseURL = base
	gone := filepath.Join(e.dir, "repos", "999.git")
	os.MkdirAll(gone, 0o700)

	d := e.signIn("derek")
	if _, body := d.get("/w/o/r/settings"); !strings.Contains(body, "Not cloned yet") {
		t.Errorf("before the clone:\n%s", body)
	}
	e.ui.repoRound(context.Background())
	for i := 0; i < 100 && !e.ui.repos.Ready(ws.ID); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !e.ui.repos.Ready(ws.ID) {
		t.Fatalf("not cloned: %+v", e.ui.repos.Status(ws.ID))
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("the copy of a workspace that is gone was kept")
	}
	if _, body := d.get("/w/o/r/settings"); !strings.Contains(body, "Local copy") || !strings.Contains(body, "Fetched") {
		t.Errorf("after the clone:\n%s", body)
	}
	if e.ui.repos.Due(ws.ID, time.Now()) {
		t.Error("due again at once")
	}
}
