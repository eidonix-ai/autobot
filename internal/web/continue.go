package web

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Threads that continue. A run of an agent block is a new session, starting fresh, in the item's
// thread for the block: (workspace, workflow kind, the block that started it, item). A thread has one
// scratchpad, shared by all its runs, and every run's final report is kept in it:
//   - the same block run again on the item resumes its thread, unless it is set to start fresh (a new
//     thread, with an empty scratchpad and no report);
//   - a block set to "Continue the conversation of" another block joins that block's thread on the
//     item. Either way the session opens with the block's own Instruction and Done when, the thread's
//     last report, its scratchpad's index, and what is new on the item since; it has the block's own
//     model and tools, and none of the earlier messages;
//   - a test from the canvas has a thread of its own, never continued.
// A thread takes one run at a time: a run that would join one in progress waits for it.

// agentPoll is how often a step looks to see whether its agent run has ended.
var agentPoll = 2 * time.Second

// convWait is how often a run that wants a thread with a run in progress looks again.
var convWait = 3 * time.Second

// errNoConversation is what a continuation answers when the item has no earlier report of the block
// it continues, and it is set to skip.
var errNoConversation = errors.New("no earlier report")

// chooseThread says which thread a run joins, or nil for a new one. The errors are errNoConversation
// (skip), store.ErrThreadBusy (the thread has a run in progress: wait) and a plain explanation.
func (s *Server) chooseThread(ws *store.Workspace, kind string, n workflow.Node, a workflow.AgentSettings, item int, test bool) (*store.Thread, error) {
	if test || item == 0 {
		return nil, nil
	}
	var th *store.Thread
	var err error
	switch {
	case a.Continues():
		th, err = s.st.LatestThread(ws.ID, kind, a.ContinueOf, item)
		if err == nil {
			if busy, berr := s.st.ThreadRunning(th.ID); berr != nil || busy {
				return nil, errors.Join(berr, store.ErrThreadBusy) // wait for its report
			}
			// The other block has run, but with nothing to continue from if it ended with no report.
			if _, ok, rerr := s.st.Pad(th.ID).LastReport(); rerr != nil || !ok {
				err = store.ErrNotFound
			}
		}
		if errors.Is(err, store.ErrNotFound) {
			if a.FreshWithoutConversation() {
				return nil, nil
			}
			return nil, errNoConversation
		}
	case a.Rerun == "resume":
		if th, err = s.st.LatestThread(ws.ID, kind, n.ID, item); errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if busy, err := s.st.ThreadRunning(th.ID); err != nil {
		return nil, err
	} else if busy {
		return nil, store.ErrThreadBusy
	}
	return th, nil
}

// continuedBlock is the Agent block a continuation continues, from its canvas; the error says
// plainly why it cannot be found.
func continuedBlock(g workflow.Graph, kind string, a workflow.AgentSettings) (*workflow.Node, workflow.AgentSettings, error) {
	for i, n := range g.Nodes {
		if n.ID != a.ContinueOf {
			continue
		}
		src, err := workflow.AgentSettingsOf(n, kind)
		if n.Type != workflow.Agent || err != nil || src.Continues() {
			break
		}
		return &g.Nodes[i], src, nil
	}
	return nil, workflow.AgentSettings{}, errors.New("the block it continues is no longer on the canvas (or no longer starts a conversation of its own); choose another in its settings")
}

// withSourceModel gives a continuation the model of the block it continues when it has none of
// its own (a reference block starts with a copy of it, which the person may change), and fails
// plainly if that block is gone.
func withSourceModel(g workflow.Graph, kind string, a workflow.AgentSettings) (workflow.AgentSettings, error) {
	if !a.Continues() {
		return a, nil
	}
	_, src, err := continuedBlock(g, kind, a)
	if err != nil {
		return a, err
	}
	if !a.Model.Set() {
		a.Model = src.Model
	}
	return a, nil
}

// sinceTags are the values of {pr:new_commits} and {pr:new_comments} for a session that continues a
// thread: what came after the head and time the thread's latest run saw. head is the pull request's
// head now.
func (s *Server) sinceTags(ctx context.Context, ws *store.Workspace, token string, number int, head string, th *store.Thread) (commits, comments string) {
	since := th.Seen
	if since.IsZero() {
		since = th.CreatedAt
	}
	return s.newCommits(ctx, ws, token, number, th.Head, head), s.newComments(ctx, ws, token, number, since)
}

// newCommits describes the commits pushed since the head the conversation last saw, with their diff.
func (s *Server) newCommits(ctx context.Context, ws *store.Workspace, token string, number int, old, head string) string {
	switch {
	case old == "":
		return "(The conversation did not record which commit it last saw, so the new commits are not known; the whole pull request is in the other tags.)"
	case old == head:
		return "No new commits."
	}
	if s.repos != nil && s.repos.Ready(ws.ID) {
		out, err := func() (string, error) {
			if err := s.repos.Ensure(ctx, ws.ID, ws.Name, token, head, number); err != nil {
				return "", err
			}
			r := s.repos.Repo(ws.ID, token)
			log, err := r.Commits(ctx, old, head)
			if err != nil {
				return "", err
			}
			diff, err := r.Diff(ctx, old, head)
			if err != nil {
				return "", err
			}
			return commitsText(log, diff), nil
		}()
		if err == nil {
			return out
		}
	}
	commits, files, err := s.gh.Compare(ctx, token, ws.Name, old, head)
	if err != nil {
		return fmt.Sprintf("(Could not read the commits pushed since %.8s: %v. The branch may have been rewritten; the whole pull request is in the other tags.)", old, err)
	}
	var log strings.Builder
	for _, c := range commits {
		fmt.Fprintf(&log, "%.8s %s: %s\n", c.SHA, c.Author, c.Message)
	}
	return commitsText(log.String(), github.Diff(files))
}

func commitsText(log, diff string) string {
	if len(diff) > maxDiff {
		diff = strings.ToValidUTF8(diff[:maxDiff], "") + "\n[the diff goes on; read the rest with the pr_diff tool]"
	}
	return "Commits pushed since your last turn:\n" + strings.TrimSpace(log) + "\n\nWhat they changed:\n" + diff
}

// newComment is one comment, review or inline comment, as the new-comments tag lists it.
type newComment struct {
	kind, author, body, state, path string
	line                            int
	at                              time.Time
}

// newComments lists the comments and reviews posted after since, oldest first: from Autobot's own
// copy of the item when it has one in full, else from GitHub.
func (s *Server) newComments(ctx context.Context, ws *store.Workspace, token string, number int, since time.Time) string {
	all, err := s.discussion(ctx, ws, token, number)
	if err != nil {
		return fmt.Sprintf("(Could not read the comments: %v.)", err)
	}
	var fresh []newComment
	for _, c := range all {
		if c.at.After(since) {
			fresh = append(fresh, c)
		}
	}
	if len(fresh) == 0 {
		return "No new comments."
	}
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].at.Before(fresh[j].at) })
	var b strings.Builder
	for i, c := range fresh {
		if i > 0 {
			b.WriteString("\n\n")
		}
		what := "commented"
		switch c.kind {
		case "review":
			what = "reviewed (" + strings.ToLower(strings.ReplaceAll(c.state, "_", " ")) + ")"
		case "review_comment":
			what = fmt.Sprintf("commented on %s:%d", c.path, c.line)
		}
		fmt.Fprintf(&b, "@%s %s at %s:\n%s", c.author, what, c.at.UTC().Format("2006-01-02 15:04 UTC"), strings.TrimSpace(c.body))
	}
	return b.String()
}

// discussion is everything said on an item: from Autobot's copy when it is ready and holds the
// item in full, else read from GitHub.
func (s *Server) discussion(ctx context.Context, ws *store.Workspace, token string, number int) ([]newComment, error) {
	var out []newComment
	if index.Ready(s.st, ws.ID) {
		if d, err := s.st.ItemIndex(ws.ID).Read(number); err == nil && !d.Slim {
			for _, c := range d.Discussion {
				out = append(out, newComment{c.Kind, c.Author, c.Body, c.ReviewState, c.Path, c.Line, c.Created})
			}
			return out, nil
		}
	}
	add := func(rs []github.CommentRecord) {
		for _, c := range rs {
			out = append(out, newComment{c.Kind, c.Author, c.Body, c.State, c.Path, c.Line, c.Created})
		}
	}
	for page := 1; page <= 3; page++ {
		rs, err := s.gh.ItemComments(ctx, token, ws.Name, number, page)
		if err != nil {
			return nil, err
		}
		add(rs)
		if len(rs) < github.PageSize {
			break
		}
	}
	reviews, err := s.gh.ReviewRecords(ctx, token, ws.Name, number)
	if err != nil {
		return nil, err
	}
	add(reviews)
	for page := 1; page <= 3; page++ {
		rs, err := s.gh.PullComments(ctx, token, ws.Name, number, page)
		if err != nil {
			return nil, err
		}
		add(rs)
		if len(rs) < github.PageSize {
			break
		}
	}
	return out, nil
}
