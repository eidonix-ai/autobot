package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// Stopping a run (Derek, 2026-11-24: "I have no way to abort a run"). Every workflow run has a
// context that can be cancelled, and every step's, and every agent run's, derives from it; a stop
// cancels it with a StopError naming the person, so that everything under it ends at once and
// nothing that ends after it writes anything. The contexts are kept here, in memory, by run id,
// for as long as the runs go on; a run that is not here (Autobot restarted) is marked stopped in
// the database and nothing more.

// stopWait is how long a stop waits for what it cancelled to end before it marks the run stopped
// itself.
var stopWait = 15 * time.Second

// liveKey names a run in memory: a workflow run or an agent run.
type liveKey struct {
	agent bool
	id    int64
}

// liveRun is a run in memory: how to stop it, and a channel closed once it has ended.
type liveRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// liveRuns are the runs in memory.
type liveRuns struct {
	mu sync.Mutex
	m  map[liveKey]*liveRun
}

// add keeps a run until the returned function is called, when its entry goes and its done channel closes.
func (l *liveRuns) add(k liveKey, cancel context.CancelCauseFunc) (release func()) {
	lr := &liveRun{cancel: cancel, done: make(chan struct{})}
	l.mu.Lock()
	if l.m == nil {
		l.m = map[liveKey]*liveRun{}
	}
	l.m[k] = lr
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		if l.m[k] == lr {
			delete(l.m, k)
		}
		l.mu.Unlock()
		close(lr.done)
		cancel(nil) // frees the context's timers
	}
}

func (l *liveRuns) get(k liveKey) *liveRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.m[k]
}

// stopRun cancels a run in memory as stopped by a person and waits, for a while, for it to end. It
// reports whether the run was in memory.
func (l *liveRuns) stopRun(k liveKey, by string) bool {
	lr := l.get(k)
	if lr == nil {
		return false
	}
	lr.cancel(&agent.StopError{By: by})
	select {
	case <-lr.done:
	case <-time.After(stopWait):
	}
	return true
}

// wait waits, for a while, for a run in memory to end.
func (l *liveRuns) wait(k liveKey) {
	if lr := l.get(k); lr != nil {
		select {
		case <-lr.done:
		case <-time.After(stopWait):
		}
	}
}

// launch starts driving a run in the background, with a context of its own that a stop cancels.
func (s *Server) launch(rs *runState) {
	ctx, cancel := context.WithCancelCause(context.Background())
	rs.ctx = ctx
	id := rs.run.ID
	release := s.running.add(liveKey{id: id}, cancel)
	go func() {
		defer release()
		defer s.releaseSlot(id) // after the run is over, whichever way: the next queued run starts
		defer survive(fmt.Sprintf("workflow run %d", id), func(string) { s.st.FinishWorkflowRun(id, "failed") })
		// A person may have stopped it while it was being started (it was queued a moment ago).
		if cur, err := s.st.WorkflowRun(id); err == nil && cur.Status != "running" {
			return
		}
		s.drive(rs)
	}()
}

// stopWorkflow stops a workflow run for a person: what runs is cancelled and given a moment to end,
// then the run, its steps and its open approvals are marked stopped (see Store.StopWorkflowRun).
// Whichever of the run's own driver and this gets there first marks them; the other finds it done.
func (s *Server) stopWorkflow(id int64, by string) {
	s.running.stopRun(liveKey{id: id}, by)
	s.markStopped(id, by)
}

// markStopped records a run as stopped, and says so on Discord wherever it was waiting for an
// answer: the message loses its buttons, and shows who stopped the run.
func (s *Server) markStopped(id int64, by string) {
	stopped, closed, err := s.st.StopWorkflowRun(id, by)
	if err != nil {
		log.Printf("workflow run %d: stopping: %v", id, err)
		return
	}
	if !stopped || len(closed) == 0 {
		return
	}
	run, err := s.st.WorkflowRun(id)
	if err != nil {
		return
	}
	ws, err := s.st.WorkspaceByID(run.WorkspaceID)
	if err != nil {
		return
	}
	b, _ := s.bot(ws)
	if b == nil {
		return
	}
	var g workflow.Graph
	json.Unmarshal([]byte(run.Graph), &g)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, ap := range closed {
		if ap.Message == "" {
			continue
		}
		node := workflow.Node{ID: ap.NodeID, Type: workflow.Approval}
		for _, n := range g.Nodes {
			if n.ID == ap.NodeID {
				node = n
			}
		}
		if err := b.Edit(ctx, ap.Channel, ap.Message, approvalMessage(node, &ap)); err != nil {
			log.Printf("workflow run %d: telling Discord the run was stopped: %v", id, err)
		}
	}
}

// stopAgent stops an agent run that is not a step of a workflow run (a test from the canvas).
func (s *Server) stopAgent(id int64, by string) {
	s.running.stopRun(liveKey{agent: true, id: id}, by)
	if _, err := s.st.StopAgentRun(id, by); err != nil {
		log.Printf("agent run %d: stopping: %v", id, err)
	}
}

// stopWorkflowRun is the run page's Stop button.
func (s *Server) stopWorkflowRun(w http.ResponseWriter, r *http.Request) {
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
	s.stopWorkflow(run.ID, me(r).Name())
	http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}

// stopAgentRun is the agent run page's Stop button: for a test from the canvas it stops that run;
// for a step of a workflow run it stops the whole workflow run.
func (s *Server) stopAgentRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.AgentRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	by := me(r).Name()
	if wid, ok := s.st.WorkflowOfAgentRun(run.ID); ok && !run.Test {
		s.stopWorkflow(wid, by)
	}
	s.stopAgent(run.ID, by)
	http.Redirect(w, r, fmt.Sprintf("%s/agent-runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}
