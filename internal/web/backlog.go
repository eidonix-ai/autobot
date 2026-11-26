package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

// backlogRound inspects live PR and Issue workflows that have a linked PR Backlog or Issue Backlog
// event block on their canvas and starts at most one catch-up run per idle workspace (strictly lower
// priority than queued runs and approval slot waiters).
func (s *Server) backlogRound(ctx context.Context) {
	live, err := s.st.LiveWorkflows()
	if err != nil {
		log.Printf("backlog: %v", err)
		return
	}
	startedWS := map[int64]bool{}
	for _, w := range live {
		if ctx.Err() != nil || startedWS[w.WorkspaceID] {
			continue
		}
		if w.Kind != workflow.PR && w.Kind != workflow.Issue {
			continue
		}
		if s.poll.backedOff(w.WorkspaceID) {
			continue
		}
		var g workflow.Graph
		if err := json.Unmarshal([]byte(w.Graph), &g); err != nil {
			continue
		}
		linked := map[string]bool{}
		for _, l := range g.Links {
			linked[l.From] = true
		}
		for _, n := range g.Nodes {
			if !workflow.IsBacklogEvent(n.Type) || !linked[n.ID] {
				continue
			}
			ev, err := workflow.EventSettingsOf(n)
			if err != nil {
				continue
			}
			maxSlots := ev.IdleSlots
			if maxSlots < 1 {
				maxSlots = 1
			}
			if !s.backlogIdleFor(w.WorkspaceID, maxSlots) {
				continue
			}
			ws, err := s.st.WorkspaceByID(w.WorkspaceID)
			if err != nil {
				break
			}
			if !index.Ready(s.st, ws.ID) {
				break
			}
			started, err := s.tryBacklogNode(ctx, ws, w.Kind, g, n, ev, maxSlots)
			if err != nil {
				log.Printf("backlog (%s %s): %v", ws.Name, w.Kind, err)
				continue
			}
			if started {
				startedWS[w.WorkspaceID] = true
				break
			}
		}
	}
}

// backlogIdleFor reports whether workspace wsID has no queued PR/Issue runs (including runs
// cooling down on transient retry backoff), no runs waiting to reclaim a slot after an approval,
// and fewer than min(maxSlots, workspace MaxRuns) active PR/Issue runs.
func (s *Server) backlogIdleFor(wsID int64, maxSlots int) bool {
	if hasQueued, err := s.st.HasQueuedRunsFor(wsID, false); err != nil || hasQueued {
		return false
	}
	limit := min(maxSlots, s.st.MaxRuns(wsID))
	if limit < 1 {
		return false
	}
	p := slotPool{ws: wsID, discord: false}
	s.slots.mu.Lock()
	defer s.slots.mu.Unlock()
	if s.slots.waiting(p) {
		return false
	}
	return s.slots.used(p) < limit
}

// tryBacklogNode picks the next eligible open PR or issue with no case file and starts it via evNode.
func (s *Server) tryBacklogNode(ctx context.Context, ws *store.Workspace, kind string, g workflow.Graph, evNode workflow.Node, ev workflow.EventSettings, maxSlots int) (bool, error) {
	ev = ev.WithOptOut(s.optOut(ws))
	order := ev.BacklogOrder
	if order != "oldest" {
		order = "newest"
	}
	cands, err := s.st.BacklogCandidates(ws.ID, kind, order, 50)
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
		if !backlogLocalMatch(evNode.Type, ev, c) {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
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
		if evNode.Type == workflow.PRBacklog && !ev.IncludeDrafts && (it.pr == nil || it.pr.Draft) {
			s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 0)
			continue
		}
		if s.optedOutBy(ctx, ws, g, evNode.ID, it) != "" {
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
					s.backlog.markSkipped(ws.ID, kind, evNode.ID, c.Number, c.UpdatedAt, 10*time.Minute)
					continue
				}
			}
		}
		if !s.backlogIdleFor(ws.ID, maxSlots) {
			return false, nil
		}
		id, err := s.startBacklogWorkflow(ctx, ws, kind, g, evNode, it)
		if err != nil {
			return false, err
		}
		log.Printf("backlog (%s %s #%d): started run %d via %s", ws.Name, kind, c.Number, id, workflow.DisplayName(evNode))
		return true, nil
	}
	return false, nil
}

// backlogLocalMatch applies cheap local checks from the item index before touching GitHub.
func backlogLocalMatch(nodeType string, ev workflow.EventSettings, c store.BacklogCandidate) bool {
	if nodeType == workflow.PRBacklog && !ev.IncludeDrafts && c.Draft {
		return false
	}
	if workflow.Stops(c.Body, ev.StopTag) {
		return false
	}
	if nodeType == workflow.IssueBacklog {
		return true
	}
	facts := workflow.PRFacts{
		Author: c.Author,
		Base:   c.BaseRef,
		Lines:  c.Additions + c.Deletions,
	}
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
