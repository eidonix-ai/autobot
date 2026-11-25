package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// backlogPrefix marks workflow runs started by idle backlog catch-up so the Runs page shows "Backlog".
const backlogPrefix = "Backlog catch-up (no case file yet)"

// backlogState caches recently skipped candidates in memory so filtered items (opt-out, size limit,
// bot author, etc.) do not repeatedly query GitHub every tick unless the item is updated.
type backlogState struct {
	mu      sync.Mutex
	skipped map[string]backlogSkip // "ws:kind:node:num" -> skip record
}

type backlogSkip struct {
	updatedAt time.Time
	until     time.Time // re-check after this if UpdatedAt didn't change (e.g. pending CI)
}

func newBacklogState() *backlogState {
	return &backlogState{skipped: map[string]backlogSkip{}}
}

func (b *backlogState) isSkipped(wsID int64, kind, nodeID string, num int, updatedAt time.Time) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sk, ok := b.skipped[fmt.Sprintf("%d:%s:%s:%d", wsID, kind, nodeID, num)]
	if !ok {
		return false
	}
	if !updatedAt.IsZero() && updatedAt.After(sk.updatedAt) {
		return false
	}
	return sk.until.IsZero() || time.Now().Before(sk.until)
}

func (b *backlogState) markSkipped(wsID int64, kind, nodeID string, num int, updatedAt time.Time, ttl time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var until time.Time
	if ttl > 0 {
		until = time.Now().Add(ttl)
	}
	b.skipped[fmt.Sprintf("%d:%s:%s:%d", wsID, kind, nodeID, num)] = backlogSkip{updatedAt: updatedAt, until: until}
}

func (b *backlogState) clearWorkspace(wsID int64, kind string) {
	if b == nil {
		return
	}
	prefix := fmt.Sprintf("%d:%s:", wsID, kind)
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.skipped {
		if strings.HasPrefix(k, prefix) {
			delete(b.skipped, k)
		}
	}
}

// backlogRound inspects workspaces with backlog catch-up enabled and starts at most one catch-up
// run per idle workspace (strictly lower priority than queued runs and approval slot waiters).
func (s *Server) backlogRound(ctx context.Context) {
	entries, err := s.st.EnabledBacklogs()
	if err != nil {
		log.Printf("backlog: %v", err)
		return
	}
	startedWS := map[int64]bool{}
	for _, e := range entries {
		if ctx.Err() != nil || startedWS[e.WorkspaceID] {
			continue
		}
		if s.poll.backedOff(e.WorkspaceID) {
			continue
		}
		if !s.backlogIdleFor(e.WorkspaceID, e.Config.MaxSlots) {
			continue
		}
		ws, err := s.st.WorkspaceByID(e.WorkspaceID)
		if err != nil {
			continue
		}
		if !index.Ready(s.st, ws.ID) {
			continue
		}
		started, err := s.tryBacklogOne(ctx, ws, e.Kind, e.Config)
		if err != nil {
			log.Printf("backlog (%s %s): %v", ws.Name, e.Kind, err)
			continue
		}
		if started {
			startedWS[e.WorkspaceID] = true
		}
	}
}

// backlogIdleFor reports whether workspace wsID has no queued runs, no runs waiting to reclaim a
// slot after an approval, and fewer than min(maxSlots, workspace MaxRuns) active runs.
func (s *Server) backlogIdleFor(wsID int64, maxSlots int) bool {
	if queued, err := s.st.QueuedRuns(wsID); err != nil || len(queued) > 0 {
		return false
	}
	limit := min(maxSlots, s.st.MaxRuns(wsID))
	if limit < 1 {
		return false
	}
	s.slots.mu.Lock()
	defer s.slots.mu.Unlock()
	if s.slots.waiting(wsID) {
		return false
	}
	return s.slots.used(wsID) < limit
}

// tryBacklogOne picks the next eligible open PR or issue with no case file and starts it.
func (s *Server) tryBacklogOne(ctx context.Context, ws *store.Workspace, kind string, cfg store.BacklogConfig) (bool, error) {
	_, g, err := s.loadWorkflow(ws, kind)
	if err != nil {
		return false, err
	}
	var evNode *workflow.Node
	for i, n := range g.Nodes {
		if n.ID == cfg.NodeID {
			evNode = &g.Nodes[i]
			break
		}
	}
	if evNode == nil {
		return false, nil // block removed from canvas
	}
	if t, _ := workflow.TypeOf(evNode.Type); !t.Event {
		return false, nil
	}
	ev, err := workflow.EventSettingsOf(*evNode)
	if err != nil {
		return false, err
	}
	optOut := s.optOut(ws)
	ev = ev.WithOptOut(optOut)
	if evNode.Type == "pr_activity" && ev.CaseExists {
		return false, nil
	}

	cands, err := s.st.BacklogCandidates(ws.ID, kind, cfg.Order, 50)
	if err != nil {
		return false, err
	}
	token, _ := s.token(ws)
	defaultBranch := ""

	for _, c := range cands {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if s.backlog.isSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt) {
			continue
		}
		// Cheap local filters before calling GitHub.
		if !backlogLocalMatch(evNode.Type, ev, c) {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
		// Verify live item state, comments (opt-out), branches, maintainer, and CI on GitHub.
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		it, err := s.loadItem(lctx, ws, kind, c.Number)
		cancel()
		if err != nil {
			return false, fmt.Errorf("#%d: %w", c.Number, err)
		}
		if it.state != "open" {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 30*time.Minute)
			continue
		}
		if evNode.Type == "pr_ready" && (it.pr == nil || it.pr.Draft) {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
		if evNode.Type == "pr_draft" && (it.pr == nil || !it.pr.Draft) {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
		// Check opt-out tag in description and trusted comments.
		if workflow.Stops(it.body, ev.StopTag) {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
		stopCounts := s.stopRule(ctx, ws, token, it)
		optedOut := false
		for _, cm := range it.comments {
			if workflow.Stops(cm.Body, ev.StopTag) && stopCounts(cm.Author) {
				optedOut = true
				break
			}
		}
		if optedOut {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
		if kind == workflow.PR && it.pr != nil {
			lines := 0
			for _, f := range it.files {
				lines += f.Additions + f.Deletions
			}
			if strings.TrimSpace(ev.Branches) == "" && defaultBranch == "" {
				bctx, bcancel := context.WithTimeout(ctx, 15*time.Second)
				defaultBranch, _ = s.gh.DefaultBranch(bctx, token, ws.Name)
				bcancel()
			}
			facts := workflow.PRFacts{
				Author:        it.author,
				Base:          it.pr.Base,
				DefaultBranch: defaultBranch,
				Lines:         lines,
			}
			if ev.SkipMaintain {
				rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
				role, rerr := s.gh.Role(rctx, token, ws.Name, it.author)
				rcancel()
				if rerr == nil {
					facts.MaintainerKnown, facts.AuthorMaintains = true, writes(role)
				}
			}
			ok := true
			for _, chk := range workflow.PRFilters(ev, facts) {
				if !chk.Pass {
					ok = false
					break
				}
			}
			if !ok {
				s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
				continue
			}
			if ev.WaitCI == "finished" || ev.WaitCI == "passed" {
				cctx, ccancel := context.WithTimeout(ctx, 15*time.Second)
				ci, cerr := s.gh.CIState(cctx, token, ws.Name, it.pr.HeadSHA)
				ccancel()
				if cerr != nil {
					return false, cerr
				}
				if ci == "pending" || (ev.WaitCI == "passed" && ci != "passed" && ci != "none") {
					// CI may finish soon: re-check after 10 minutes.
					s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 10*time.Minute)
					continue
				}
			}
		}
		// Final re-check that the workspace is still idle right before admitting.
		if !s.backlogIdleFor(ws.ID, cfg.MaxSlots) {
			return false, nil
		}
		id, err := s.startBacklogWorkflow(ctx, ws, kind, g, *evNode, it)
		if err != nil {
			return false, err
		}
		log.Printf("backlog (%s %s #%d): started run %d via %s", ws.Name, kind, c.Number, id, workflow.DisplayName(*evNode))
		return true, nil
	}
	return false, nil
}

// backlogLocalMatch applies cheap local checks from the item index before touching GitHub.
func backlogLocalMatch(nodeType string, ev workflow.EventSettings, c store.BacklogCandidate) bool {
	if nodeType == "pr_ready" && c.Draft {
		return false
	}
	if nodeType == "pr_draft" && !c.Draft {
		return false
	}
	if workflow.Stops(c.Body, ev.StopTag) {
		return false
	}
	if nodeType == "issue_opened" {
		return true
	}
	facts := workflow.PRFacts{
		Author: c.Author,
		Base:   c.BaseRef,
		Lines:  c.Additions + c.Deletions,
	}
	// Only enforce Branches locally if explicit branches were configured (default branch requires GitHub).
	checkEv := ev
	if strings.TrimSpace(ev.Branches) == "" {
		facts.DefaultBranch = c.BaseRef
	}
	checkEv.SkipMaintain = false
	for _, chk := range workflow.PRFilters(checkEv, facts) {
		if !chk.Pass {
			return false
		}
	}
	return true
}

// startBacklogWorkflow starts a real workflow run for a backlog item using an already-loaded item.
func (s *Server) startBacklogWorkflow(ctx context.Context, ws *store.Workspace, kind string, g workflow.Graph, ev workflow.Node, it *item) (int64, error) {
	rs := &runState{ws: ws, g: g, it: it, builtIn: it.values(ws), tags: map[string]string{}}
	rs.builtIn["repo:opt_out"] = s.optOut(ws)
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		rs.builtIn["charter"] = c.Body
	}
	rs.builtIn["machines"] = s.machinesSummary(ws)
	token, _ := s.token(ws)
	_, lines := verdict(ev, it, false, s.optOut(ws), s.stopRule(ctx, ws, token, it))
	graph, _ := json.Marshal(g)
	var ids []string
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	r := store.WorkflowRun{
		WorkspaceID: ws.ID,
		Kind:        kind,
		EventNode:   ev.ID,
		Item:        it.number,
		Graph:       string(graph),
		Note:        backlogPrefix + "\n" + strings.Join(lines, "\n"),
	}
	id, queued, err := s.admit(r, ids, nil)
	if err != nil {
		return 0, err
	}
	if queued {
		return id, nil
	}
	run, err := s.st.WorkflowRun(id)
	if err != nil {
		s.st.FinishWorkflowRun(id, "failed")
		s.releaseSlot(id)
		return 0, err
	}
	rs.run = run
	s.launch(rs)
	return id, nil
}

// backlogView populates the Backlog catch-up controls on the PR and Issue workflow pages.
func (s *Server) backlogView(data map[string]any, ws *store.Workspace, kind string, g workflow.Graph) {
	if kind != workflow.PR && kind != workflow.Issue {
		return
	}
	cfg := s.st.BacklogConfig(ws.ID, kind)
	type eventOpt struct {
		ID, Name string
		Selected bool
	}
	var opts []eventOpt
	selectedName := ""
	selectedType := ""
	for _, n := range g.Nodes {
		t, _ := workflow.TypeOf(n.Type)
		if !t.Event {
			continue
		}
		if n.Type == "pr_activity" {
			if ev, err := workflow.EventSettingsOf(n); err == nil && ev.CaseExists {
				continue
			}
		}
		sel := n.ID == cfg.NodeID
		if sel {
			selectedName, selectedType = g.NameOf(n), n.Type
		}
		opts = append(opts, eventOpt{ID: n.ID, Name: g.NameOf(n), Selected: sel})
	}
	if cfg.NodeID == "" && len(opts) > 0 {
		opts[0].Selected = true
		selectedType = ""
		for _, n := range g.Nodes {
			if n.ID == opts[0].ID {
				selectedType = n.Type
			}
		}
	}
	draftFilter := -1
	switch selectedType {
	case "pr_ready":
		draftFilter = 0
	case "pr_draft":
		draftFilter = 1
	}
	remaining, _ := s.st.BacklogCount(ws.ID, kind, draftFilter)
	data["Backlog"] = cfg
	data["BacklogEvents"] = opts
	data["BacklogEventName"] = selectedName
	data["BacklogRemaining"] = remaining
	data["BacklogMaxSlots"] = s.st.MaxRuns(ws.ID)
	data["BacklogIndexReady"] = index.Ready(s.st, ws.ID)
	if cfg.Enabled && selectedName == "" {
		data["BacklogProblem"] = "The selected event block is no longer on this canvas; choose another and save."
	}
}

// saveBacklog saves a workspace's backlog catch-up settings for kind (pr or issue).
func (s *Server) saveBacklog(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	kind, ok := itemKind(w, r)
	if !ok {
		return
	}
	back := ws.Path() + "/workflows/" + kind
	f := r.PostForm
	slots, _ := strconv.Atoi(strings.TrimSpace(f.Get("max_slots")))
	if slots == 0 {
		slots = 1
	}
	cfg := store.BacklogConfig{
		Enabled:  f.Get("enabled") == "on",
		NodeID:   strings.TrimSpace(f.Get("node_id")),
		Order:    strings.TrimSpace(f.Get("order")),
		MaxSlots: slots,
	}
	if cfg.Enabled {
		_, g, err := s.loadWorkflow(ws, kind)
		if err != nil {
			s.fail(w, err)
			return
		}
		found := false
		for _, n := range g.Nodes {
			if t, _ := workflow.TypeOf(n.Type); t.Event && n.ID == cfg.NodeID {
				found = true
				break
			}
		}
		if !found {
			s.poll.note(ws.ID, kind, "Backlog catch-up: choose an event block on this canvas.")
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
	}
	if err := s.st.SetBacklogConfig(ws, kind, cfg, me(r)); err != nil {
		var ie *store.InputError
		if errors.As(err, &ie) {
			s.poll.note(ws.ID, kind, "Backlog catch-up: "+ie.Msg)
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		s.fail(w, err)
		return
	}
	s.poll.note(ws.ID, kind, "")
	s.backlog.clearWorkspace(ws.ID, kind)
	http.Redirect(w, r, back, http.StatusSeeOther)
}
