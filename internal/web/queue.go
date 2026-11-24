package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

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

// slotWaiter is a run that has had a slot and needs one again: its approval was answered.
type slotWaiter struct {
	run, ws int64
	ready   chan struct{}
}

// slots are the runs that count against their workspace's limit, by run id, with the workspace.
type slots struct {
	mu      sync.Mutex
	held    map[int64]int64
	waiters []*slotWaiter // oldest first
}

// used is how many runs of a workspace hold a slot. The caller holds the mutex.
func (k *slots) used(ws int64) int {
	n := 0
	for _, w := range k.held {
		if w == ws {
			n++
		}
	}
	return n
}

// take records a run as holding a slot. The caller holds the mutex.
func (k *slots) take(run, ws int64) {
	if k.held == nil {
		k.held = map[int64]int64{}
	}
	k.held[run] = ws
}

// waiting says whether a run of the workspace waits for a slot. The caller holds the mutex.
func (k *slots) waiting(ws int64) bool {
	return slices.ContainsFunc(k.waiters, func(w *slotWaiter) bool { return w.ws == ws })
}

// admit records a new run: running, with a slot, when the workspace has one free and nothing waits
// for it; otherwise queued.
func (s *Server) admit(r store.WorkflowRun, nodes []string, by *store.User) (id int64, queued bool, err error) {
	k := &s.slots
	k.mu.Lock()
	defer k.mu.Unlock()
	ahead, err := s.st.QueuedRuns(r.WorkspaceID)
	free := err == nil && len(ahead) == 0 && !k.waiting(r.WorkspaceID) && k.used(r.WorkspaceID) < s.st.MaxRuns(r.WorkspaceID)
	if !free {
		r.Status = "queued"
	}
	if id, err = s.st.StartWorkflowRun(r, nodes, by); err != nil {
		return 0, false, err
	}
	if free {
		k.take(id, r.WorkspaceID)
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
	ws, ok := k.held[run]
	delete(k.held, run)
	k.mu.Unlock()
	if ok {
		s.dispatch(ws)
	}
}

// acquireSlot waits until a run that gave its slot up (it was waiting for approvals) may hold one
// again. It reports false when the run's context ended first.
func (s *Server) acquireSlot(ctx context.Context, ws, run int64) bool {
	k := &s.slots
	k.mu.Lock()
	if _, ok := k.held[run]; ok {
		k.mu.Unlock()
		return true
	}
	if !k.waiting(ws) && k.used(ws) < s.st.MaxRuns(ws) {
		k.take(run, ws)
		k.mu.Unlock()
		return true
	}
	w := &slotWaiter{run: run, ws: ws, ready: make(chan struct{})}
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

// dispatch gives a workspace's free slots to those waiting for one, then to its queued runs,
// oldest first, and starts those.
func (s *Server) dispatch(ws int64) {
	k := &s.slots
	var start []int64
	k.mu.Lock()
	limit := s.st.MaxRuns(ws)
fill:
	for k.used(ws) < limit {
		for i, w := range k.waiters {
			if w.ws == ws {
				k.waiters = slices.Delete(k.waiters, i, i+1)
				k.take(w.run, ws)
				close(w.ready)
				continue fill
			}
		}
		queued, err := s.st.QueuedRuns(ws)
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
				k.take(id, ws)
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
			s.st.FailStart(id, run.EventNode, "Could not start: "+err.Error())
		}
		s.releaseSlot(id)
		return
	}
	s.launch(rs)
}

// RunQueue starts the queued runs that fit, at start and then every queueEvery.
func (s *Server) RunQueue(ctx context.Context) {
	for {
		s.dispatchAll()
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
		if !slices.Contains(ids, w.ws) {
			ids = append(ids, w.ws)
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

// queuePlaces are the places of a workspace's queued runs in line, by run id: the first to start is 1.
func (s *Server) queuePlaces(ws int64) map[int64]int {
	places := map[int64]int{}
	ids, err := s.st.QueuedRuns(ws)
	if err != nil {
		return places
	}
	for i, id := range ids {
		places[id] = i + 1
	}
	return places
}
