package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// retryBackoffs is the backoff schedule when a workflow run hits a transient GitHub failure
// (5xx, rate limit, or network/timeout error): 1m, 5m, 20m, 60m, then permanently fail.
// Between attempts the run returns to the queue and releases its slot so other runs keep moving.
var retryBackoffs = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	20 * time.Minute,
	60 * time.Minute,
}

func formatRetryWait(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int((d+30*time.Second)/time.Minute))
	}
	return d.Round(time.Second).String()
}

// A workspace runs at most so many workflow runs at once (Derek, 2026-11-24: "max jobs"; the
// workspace's Settings say how many, 3 unless changed). A run counts against the limit while it
// goes on, except while its only unfinished work is steps waiting for a person's answer: a
// 24-hour approval does not block other runs. A run over the limit is recorded as queued and does
// not start; queued runs start oldest first as slots free up, when a run ends, is stopped, or
// begins to wait only for approvals, when the limit is raised, and every queueEvery as a safety net.
//
// The slots are kept in memory, by the Server: a restart empties them. Runs that were queued stay
// queued in the database and start once Autobot is up (RunQueue); a run that was waiting for a
// person resumes without a slot and takes one when its answer lets it go on.

// queueEvery is how often the queue is looked at though nothing told it to start.
var queueEvery = 30 * time.Second

// maxDiscordRuns is how many Discord mention runs of one workspace may run at once, in a separate
// pool from the workspace's PR/Issue "Runs at once" limit so quick Discord replies never wait
// behind long PR or Issue reviews.
var maxDiscordRuns = 2

// slotPool identifies one independent concurrency pool: PR/Issue runs vs Discord mention runs.
type slotPool struct {
	ws      int64
	discord bool
}

// slotWaiter is a run that has had a slot and needs one again: its approval was answered.
type slotWaiter struct {
	run  int64
	pool slotPool
	ready chan struct{}
}

// slots are the runs that count against their pool's limit, by run id.
type slots struct {
	mu      sync.Mutex
	held    map[int64]slotPool
	waiters []*slotWaiter // oldest first
}

// used is how many runs in a pool hold a slot. The caller holds the mutex.
func (k *slots) used(p slotPool) int {
	n := 0
	for _, h := range k.held {
		if h == p {
			n++
		}
	}
	return n
}

// take records a run as holding a slot in p. The caller holds the mutex.
func (k *slots) take(run int64, p slotPool) {
	if k.held == nil {
		k.held = map[int64]slotPool{}
	}
	k.held[run] = p
}

// waiting says whether a run in p waits for a slot. The caller holds the mutex.
func (k *slots) waiting(p slotPool) bool {
	return slices.ContainsFunc(k.waiters, func(w *slotWaiter) bool { return w.pool == p })
}

func (s *Server) poolLimit(p slotPool) int {
	if p.discord {
		return maxDiscordRuns
	}
	return s.st.MaxRuns(p.ws)
}

// admit records a new run: running, with a slot, when its pool has one free and nothing waits
// for it; otherwise queued.
func (s *Server) admit(r store.WorkflowRun, nodes []string, by *store.User) (id int64, queued bool, err error) {
	p := slotPool{ws: r.WorkspaceID, discord: r.Kind == workflow.Discord}
	k := &s.slots
	k.mu.Lock()
	defer k.mu.Unlock()
	ahead, err := s.st.QueuedRunsFor(p.ws, p.discord)
	free := err == nil && len(ahead) == 0 && !k.waiting(p) && k.used(p) < s.poolLimit(p)
	if !free {
		r.Status = "queued"
	}
	if id, err = s.st.StartWorkflowRun(r, nodes, by); err != nil {
		return 0, false, err
	}
	if free {
		k.take(id, p)
	}
	return id, !free, nil
}

// holdsSlot says whether a run holds a slot.
func (s *Server) holdsSlot(run int64) bool {
	s.slots.mu.Lock()
	defer s.slots.mu.Unlock()
	_, ok := s.slots.held[run]
	return ok
}

// releaseSlot gives a run's slot back, if it holds one, and starts what that lets start.
func (s *Server) releaseSlot(run int64) {
	k := &s.slots
	k.mu.Lock()
	p, ok := k.held[run]
	delete(k.held, run)
	k.mu.Unlock()
	if ok {
		s.dispatchPool(p)
	}
}

// acquireSlot waits until a run that gave its slot up (it was waiting for approvals) may hold one
// again. It reports false when the run's context ended first.
func (s *Server) acquireSlot(ctx context.Context, ws int64, kind string, run int64) bool {
	p := slotPool{ws: ws, discord: kind == workflow.Discord}
	k := &s.slots
	k.mu.Lock()
	if _, ok := k.held[run]; ok {
		k.mu.Unlock()
		return true
	}
	if !k.waiting(p) && k.used(p) < s.poolLimit(p) {
		k.take(run, p)
		k.mu.Unlock()
		return true
	}
	w := &slotWaiter{run: run, pool: p, ready: make(chan struct{})}
	k.waiters = append(k.waiters, w)
	k.mu.Unlock()
	select {
	case <-w.ready:
		return true
	case <-ctx.Done():
		k.mu.Lock()
		k.waiters = slices.DeleteFunc(k.waiters, func(o *slotWaiter) bool { return o == w })
		k.mu.Unlock() // a slot granted at the same moment is given back when the run ends
		return false
	}
}

// dispatch starts queued runs in both the workspace's PR/Issue pool and its Discord pool.
func (s *Server) dispatch(ws int64) {
	s.dispatchPool(slotPool{ws: ws, discord: false})
	s.dispatchPool(slotPool{ws: ws, discord: true})
}

// dispatchPool gives a pool's free slots to those waiting for one, then to its queued runs,
// oldest first, and starts those.
func (s *Server) dispatchPool(p slotPool) {
	k := &s.slots
	var start []int64
	k.mu.Lock()
	limit := s.poolLimit(p)
fill:
	for k.used(p) < limit {
		for i, w := range k.waiters {
			if w.pool == p {
				k.waiters = slices.Delete(k.waiters, i, i+1)
				k.take(w.run, p)
				close(w.ready)
				continue fill
			}
		}
		queued, err := s.st.QueuedRunsFor(p.ws, p.discord)
		if err != nil {
			log.Printf("workflow queue: %v", err)
			break
		}
		for _, id := range queued {
			claimed, err := s.st.ClaimQueuedRun(id) // not one a person has just stopped
			if err != nil {
				log.Printf("workflow queue: run %d: %v", id, err)
				break fill
			}
			if claimed {
				k.take(id, p)
				start = append(start, id)
				continue fill
			}
		}
		break
	}
	k.mu.Unlock()
	for _, id := range start {
		go s.startQueued(id)
	}
}

// startQueued starts a run that has been given a slot, from the state it was queued in.
func (s *Server) startQueued(id int64) {
	defer survive(fmt.Sprintf("starting workflow run %d", id), func(string) {
		s.st.FinishWorkflowRun(id, "failed")
		s.releaseSlot(id)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rs, err := s.runState(ctx, id)
	if err != nil {
		log.Printf("workflow run %d: could not start: %v", id, err)
		if run, rerr := s.st.WorkflowRun(id); rerr == nil {
			if !run.DryRun && github.IsTransient(err) && run.RetryCount < len(retryBackoffs) {
				delay := retryBackoffs[run.RetryCount]
				nextCount := run.RetryCount + 1
				msg := fmt.Sprintf("Could not start: %s — queued retry %d/%d in %s", err.Error(), nextCount, len(retryBackoffs), formatRetryWait(delay))
				if qerr := s.st.RequeueStartRetry(id, run.EventNode, nextCount, time.Now().Add(delay), msg); qerr == nil {
					log.Printf("workflow run %d: transient start error; requeued retry %d/%d in %s", id, nextCount, len(retryBackoffs), delay)
					s.releaseSlot(id)
					return
				}
			}
			s.st.FailStart(id, run.EventNode, "Could not start: "+err.Error())
		}
		s.releaseSlot(id)
		return
	}
	if rs.run.StartedByID == 0 && (rs.run.Kind == workflow.PR || rs.run.Kind == workflow.Issue) {
		if by := s.optedOutBy(ctx, rs.ws, rs.g, rs.run.EventNode, rs.it); by != "" {
			log.Printf("workflow run %d: stopped before start (%s)", id, by)
			s.st.StopWorkflowRun(id, by)
			s.releaseSlot(id)
			return
		}
	}
	s.launch(rs)
}

// RunQueue starts the queued runs that fit, then runs low-priority backlog catch-up when a
// workspace is idle, at start and then every queueEvery.
func (s *Server) RunQueue(ctx context.Context) {
	for {
		s.dispatchAll()
		s.backlogRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(queueEvery):
		}
	}
}

// dispatchAll looks at every workspace that has a queued run or a run waiting for a slot.
func (s *Server) dispatchAll() {
	ids, err := s.st.WorkspacesWithQueue()
	if err != nil {
		log.Printf("workflow queue: %v", err)
	}
	s.slots.mu.Lock()
	for _, w := range s.slots.waiters {
		if !slices.Contains(ids, w.pool.ws) {
			ids = append(ids, w.pool.ws)
		}
	}
	s.slots.mu.Unlock()
	for _, id := range ids {
		s.dispatch(id)
	}
}

// ordinal writes 1 as "1st", 2 as "2nd", 11 as "11th".
func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// saveMaxRuns sets how many workflow runs of the workspace may go at once.
func (s *Server) saveMaxRuns(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	typed := strings.TrimSpace(r.PostForm.Get("max_runs"))
	n, err := strconv.Atoi(typed)
	if err != nil {
		err = inputf("Runs at once is a whole number from %d to %d.", store.MinMaxRuns, store.MaxMaxRuns)
	} else {
		err = s.st.SetMaxRuns(ws, n, me(r))
	}
	if msg, ok := inputError(err); ok {
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"MaxRunsError": msg, "MaxRunsTyped": typed})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.dispatch(ws.ID) // a raised limit starts what was queued
	http.Redirect(w, r, ws.Path()+"/settings?saved=1#max-runs", http.StatusSeeOther)
}

// queuePlaces are the places of a workspace's queued runs in their own queue (PR/Issue vs Discord),
// by run id: the first to start is 1.
func (s *Server) queuePlaces(ws int64) map[int64]int {
	places := map[int64]int{}
	for _, discord := range []bool{false, true} {
		ids, err := s.st.QueuedRunsFor(ws, discord)
		if err != nil {
			continue
		}
		for i, id := range ids {
			places[id] = i + 1
		}
	}
	return places
}

// bypassWorkflowRun starts a queued run immediately, bypassing its pool's concurrency limit.
// The run still takes a slot while it executes so remaining queued runs continue waiting until
// active runs drop below the limit.
func (s *Server) bypassWorkflowRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.WorkflowRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if run.Status == "queued" {
		p := slotPool{ws: ws.ID, discord: run.Kind == workflow.Discord}
		s.slots.mu.Lock()
		claimed, cerr := s.st.ClaimQueuedRun(run.ID)
		if cerr == nil && claimed {
			s.slots.take(run.ID, p)
		}
		s.slots.mu.Unlock()
		if cerr != nil {
			s.fail(w, cerr)
			return
		}
		if claimed {
			go s.startQueued(run.ID)
		}
	}
	if r.FormValue("from") == "list" {
		http.Redirect(w, r, ws.Path()+"/workflow-runs", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}

// resumeWorkflowRun puts a failed or stopped non-dry run back into the queue, preserving all
// already-completed steps and resetting only failed/stopped steps and their downstream skipped
// dependents back to pending.
func (s *Server) resumeWorkflowRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.WorkflowRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !run.DryRun && (run.Status == "failed" || run.Status == "stopped") {
		steps, serr := s.st.WorkflowSteps(run.ID)
		if serr != nil {
			s.fail(w, serr)
			return
		}
		var g workflow.Graph
		_ = json.Unmarshal([]byte(run.Graph), &g)
		resetSet := map[string]bool{}
		for nid, st := range steps {
			if st.Status == "failed" || st.Status == "stopped" || st.Status == "running" {
				resetSet[nid] = true
				for rid := range g.ReachableFrom(nid) {
					if rid != nid && steps[rid].Status == "skipped" {
						resetSet[rid] = true
					}
				}
			}
		}
		var resetNodes []string
		for nid := range resetSet {
			resetNodes = append(resetNodes, nid)
		}
		resumed, rerr := s.st.ResumeFailedRun(run.ID, resetNodes)
		if rerr != nil {
			s.fail(w, rerr)
			return
		}
		if resumed {
			s.dispatch(ws.ID)
		}
	}
	if r.FormValue("from") == "list" {
		http.Redirect(w, r, ws.Path()+"/workflow-runs", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}
