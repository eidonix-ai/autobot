package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Conversations that continue. A run of an agent block is part of a conversation, which belongs to
// (workspace, workflow kind, the block that started it, item) and may take several runs:
//   - the same block run again on the item resumes its own conversation, unless it is set to start
//     fresh (or its model has changed since);
//   - a block set to "Continue the conversation of" another block continues that block's conversation
//     on the item: it sends its own Instruction and Done when as the next message, and the model,
//     system prompt and tools stay the ones the conversation started with;
//   - a test from the canvas is a conversation of its own, never continued.
// A conversation takes one run at a time: a run that would continue one in progress waits for it.

// agentPoll is how often a step looks to see whether its agent run has ended.
var agentPoll = 2 * time.Second

// convWait is how often a run that wants a conversation with a run in progress looks again.
var convWait = 3 * time.Second

// errNoConversation is what a continuation answers when the item has no conversation of the block
// it continues, and it is set to skip.
var errNoConversation = errors.New("no earlier conversation")

// chooseConversation says which conversation a run continues: its row and the conversation as the
// file holds it, or nil for a new one. The errors are errNoConversation (skip), store.ErrConversationBusy
// (the conversation has a run in progress: wait) and a plain explanation.
func (s *Server) chooseConversation(ws *store.Workspace, kind string, n workflow.Node, a workflow.AgentSettings, item int, test bool) (*store.Conversation, *agent.Conversation, error) {
	if test || item == 0 {
		return nil, nil, nil
	}
	var c *store.Conversation
	var err error
	switch {
	case a.Continues():
		if c, err = s.st.LatestConversation(ws.ID, kind, a.ContinueOf, item); errors.Is(err, store.ErrNotFound) {
			if a.FreshWithoutConversation() {
				return nil, nil, nil
			}
			return nil, nil, errNoConversation
		}
	case a.Rerun == "resume":
		if c, err = s.st.LatestConversation(ws.ID, kind, n.ID, item); errors.Is(err, store.ErrNotFound) {
			return nil, nil, nil
		}
	default:
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if busy, err := s.st.ConversationRunning(c.ID); err != nil {
		return nil, nil, err
	} else if busy {
		return nil, nil, store.ErrConversationBusy
	}
	evs, err := s.st.ReadConv(c.ID)
	var conv *agent.Conversation
	if err == nil {
		conv, err = agent.LoadConversation(evs)
	}
	if err != nil {
		// Its file is missing (Autobot stopped before the first turn was written) or cannot be read:
		// it is set aside, so that it never blocks the block, and the item is as if it had none.
		log.Printf("conversation %d could not be read and is set aside: %v", c.ID, err)
		if err := s.st.ForgetConversation(c.ID); err != nil {
			return nil, nil, err
		}
		return s.chooseConversation(ws, kind, n, a, item, test)
	}
	// A new model cannot take over a conversation it did not have; a block that is not a
	// continuation starts a new one with its new model.
	if !a.Continues() && (conv.Model != a.Model.Model || conv.Provider != a.Model.Provider) {
		return nil, nil, nil
	}
	return c, conv, nil
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

// withSourceModel gives a continuation the model of the block it continues, which is the model of
// a conversation it has to start for itself, and fails plainly if that block is gone.
func withSourceModel(g workflow.Graph, kind string, a workflow.AgentSettings) (workflow.AgentSettings, error) {
	if !a.Continues() {
		return a, nil
	}
	_, src, err := continuedBlock(g, kind, a)
	if err != nil {
		return a, err
	}
	a.Model = src.Model
	return a, nil
}

// sinceTags are the values of {pr:new_commits} and {pr:new_comments} for a conversation being
// continued: what came after the latest run of it last saw the item. head is the pull request's head
// now.
func (s *Server) sinceTags(ctx context.Context, ws *store.Workspace, token string, number int, head string, row *store.Conversation, conv *agent.Conversation) (commits, comments string) {
	since := conv.Seen
	if since.IsZero() {
		since = row.CreatedAt
	}
	return s.newCommits(ctx, ws, token, number, conv.Head, head), s.newComments(ctx, ws, token, number, since)
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
