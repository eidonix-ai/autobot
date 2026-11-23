package web

import (
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// What the thread has seen of the item (its head commit and when) moves only when a run came to a
// report: a run that failed with nothing leaves "what is new" counted from the last good run.
func TestNewSinceMovesOnlyOnSuccess(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" })), [2]string{"ev1", "A"})
	first := c.agentRun(c.run("ev1"), "A")
	th, err := c.st.LatestThread(c.ws.ID, workflow.PR, "A", 1)
	if err != nil || th.ID != first.Thread || th.Head != "sha1-1" || th.Seen.IsZero() {
		t.Fatalf("after the first run: %+v %v", th, err)
	}
	// A commit is pushed, and the next run fails (the provider refuses it) without a report.
	c.editPR(1, func(l *github.Listed) { l.HeadSHA = "sha1-2" })
	c.llm.status = func(int) int { return 400 }
	failed := c.agentRun(c.run("ev1"), "A")
	after, _ := c.st.LatestThread(c.ws.ID, workflow.PR, "A", 1)
	if failed.Status != "failed" || failed.Thread != first.Thread || after.Head != "sha1-1" || !after.Seen.Equal(th.Seen) {
		t.Fatalf("a failed run moved what the thread has seen: status %s, head %q, seen %v (was %v)", failed.Status, after.Head, after.Seen, th.Seen)
	}
	// The next good run is told what is new since the last good one, and then it has moved.
	c.llm.status = nil
	c.gh.compare = func(base, head string) ([]github.Commit, []github.ChangedFile, error) {
		if base != "sha1-1" || head != "sha1-2" {
			t.Errorf("compared %s with %s", base, head)
		}
		return []github.Commit{{SHA: "abcdef0123456789", Author: "alice", Message: "fix the typo"}}, nil, nil
	}
	c.run("ev1")
	reqs := c.llm.requests()
	if got := lastMessage(t, reqs[len(reqs)-1]); !strings.Contains(got, "abcdef01 alice: fix the typo") {
		t.Fatalf("the run after the failure was not told of the commit:\n%s", got)
	}
	if now, _ := c.st.LatestThread(c.ws.ID, workflow.PR, "A", 1); now.Head != "sha1-2" {
		t.Fatalf("head %q", now.Head)
	}
}
