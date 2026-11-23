package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/protocol"
	"github.com/eidonix-ai/autobot/internal/store"
)

// A session is a test run whose container stays open for an agent on a donated machine. The run is
// queued, taken, stopped and reported exactly as any test run is; what is added is a queue of
// commands, kept here in memory (a session cannot outlive this process), which the machine's
// connector takes with a long request and answers one by one.

// idleLimit is how long a session may go without a command before Autobot closes it.
const idleLimit = 10 * time.Minute

// sessionQueues are the command queues of the sessions open now, by run id.
type sessionQueues struct {
	mu    sync.Mutex
	byRun map[int64]*queue
}

func (q *sessionQueues) get(run int64) *queue {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.byRun[run]
}

func (q *sessionQueues) open(run int64) *queue {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.byRun == nil {
		q.byRun = map[int64]*queue{}
	}
	qu := &queue{wake: make(chan struct{}), done: make(chan struct{}), waiting: map[string]chan protocol.CommandResult{}}
	q.byRun[run] = qu
	return qu
}

func (q *sessionQueues) remove(run int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if qu := q.byRun[run]; qu != nil {
		qu.close()
		delete(q.byRun, run)
	}
}

// queue holds one session's commands: queued until the connector takes them, then waiting for results.
type queue struct {
	mu      sync.Mutex
	n       int
	queued  []protocol.Command
	waiting map[string]chan protocol.CommandResult
	wake    chan struct{} // closed, and replaced, whenever a command is queued
	done    chan struct{} // closed when the session is closed
	closed  bool
}

func (q *queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.done)
	}
}

// add queues a command and returns its id and the channel its result arrives on.
func (q *queue) add(c protocol.Command) (string, chan protocol.CommandResult) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.n++
	c.ID = fmt.Sprintf("c%d", q.n)
	ch := make(chan protocol.CommandResult, 1)
	q.waiting[c.ID] = ch
	q.queued = append(q.queued, c)
	close(q.wake)
	q.wake = make(chan struct{})
	return c.ID, ch
}

// forget drops a command nobody waits for any more.
func (q *queue) forget(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.waiting, id)
	for i, c := range q.queued {
		if c.ID == id {
			q.queued = append(q.queued[:i], q.queued[i+1:]...)
			break
		}
	}
}

// take hands over the queued commands, or, when there are none, the channel that closes when there are.
func (q *queue) take() ([]protocol.Command, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	cs := q.queued
	q.queued = nil
	return cs, q.wake
}

// deliver gives a command's result to whoever waits for it, and reports whether anyone does.
func (q *queue) deliver(id string, r protocol.CommandResult) bool {
	q.mu.Lock()
	ch, ok := q.waiting[id]
	delete(q.waiting, id)
	q.mu.Unlock()
	if ok {
		ch <- r
	}
	return ok
}

// sessionRun returns the run in the URL when it is an active session of this machine, and its
// queue; to any other machine, or once the session is closed or over, it does not exist.
func (s *Server) sessionRun(w http.ResponseWriter, r *http.Request, m *store.Machine) (*store.TestRun, *queue, bool) {
	run, ok := s.machineRun(w, r, m)
	if !ok {
		return nil, nil, false
	}
	q := s.sessions.get(run.ID)
	if !run.Session || run.State != "running" || run.Cancel || q == nil {
		http.Error(w, "no such session for this machine", http.StatusNotFound)
		return nil, nil, false
	}
	return run, q, true
}

// sessionCommands holds the connector's request until the agent has queued commands, or PollWait.
func (s *Server) sessionCommands(w http.ResponseWriter, r *http.Request, m *store.Machine) {
	_, q, ok := s.sessionRun(w, r, m)
	if !ok {
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(protocol.PollWait + 10*time.Second))
	timer := time.NewTimer(protocol.PollWait)
	defer timer.Stop()
	for {
		cmds, wake := q.take()
		if len(cmds) > 0 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(protocol.Commands{Commands: cmds})
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"commands":[]}`+"\n")
			return
		case <-q.done:
			http.Error(w, "the session is closed", http.StatusNotFound)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// sessionResult takes a command's result from the connector.
func (s *Server) sessionResult(w http.ResponseWriter, r *http.Request, m *store.Machine) {
	_, q, ok := s.sessionRun(w, r, m)
	if !ok {
		return
	}
	var res protocol.CommandResult
	// Output is capped by the connector; JSON escaping can grow it several times over, not more.
	r.Body = http.MaxBytesReader(w, r.Body, 8*protocol.MaxCommandOutput)
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, "bad result: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !q.deliver(r.PathValue("cid"), res) {
		http.Error(w, "no such command", http.StatusNotFound)
		return
	}
	reply(w, protocol.Reply{})
}

// Session is an agent's shell on a donated machine. It implements agent.Session.
type Session struct {
	s      *Server
	run    int64
	q      *queue
	once   sync.Once
	mu     sync.Mutex
	busy   int         // commands in flight
	idle   *time.Timer // closes the session after idleLimit without a command
	closed bool
}

// OpenSession queues a session on a machine for a workspace: its code is the repository at commit
// (a private repository's comes through Autobot), it gets all the machine's GPUs, and it ends by
// itself after limit. It returns at once; Ready waits until the container takes commands.
func (s *Server) OpenSession(m *store.Machine, ref, commit string, private bool, gpus int, limit time.Duration, by *store.User) (*Session, error) {
	run, err := s.st.QueueSession(m, ref, commit, private, min(gpus, 8), limit, by)
	if err != nil {
		return nil, err
	}
	se := &Session{s: s, run: run.ID, q: s.sessions.open(run.ID)}
	se.mu.Lock()
	se.idle = time.AfterFunc(s.idleLimit, se.Close)
	se.mu.Unlock()
	return se, nil
}

// Run is a command's turn: queued, taken by the connector, run, and answered.
func (se *Session) Run(ctx context.Context, script string, timeout time.Duration) (agent.CommandResult, error) {
	return se.do(ctx, protocol.Command{Script: script, TimeoutSec: int((timeout + time.Second - 1) / time.Second)}, timeout+30*time.Second)
}

// Put writes a file in the session's container.
func (se *Session) Put(ctx context.Context, path string, data []byte) error {
	if len(data) > protocol.MaxFile {
		return fmt.Errorf("a file may be up to %d MB", protocol.MaxFile>>20)
	}
	r, err := se.do(ctx, protocol.Command{Path: path, Data: data}, 2*time.Minute)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("could not write %s: %s", path, strings.TrimSpace(r.Output))
	}
	return nil
}

// Ready waits until the container has its code and takes commands, by sending it one.
func (se *Session) Ready(ctx context.Context) error {
	r, err := se.do(ctx, protocol.Command{Script: "true", TimeoutSec: 60}, 20*time.Minute)
	if err == nil && r.ExitCode != 0 {
		err = fmt.Errorf("its first command exited %d: %s", r.ExitCode, strings.TrimSpace(r.Output))
	}
	return err
}

func (se *Session) do(ctx context.Context, cmd protocol.Command, wait time.Duration) (agent.CommandResult, error) {
	se.mu.Lock()
	if se.closed {
		se.mu.Unlock()
		return agent.CommandResult{}, errors.New("the session is closed")
	}
	se.busy++
	se.idle.Stop()
	se.mu.Unlock()
	defer func() {
		se.mu.Lock()
		if se.busy--; se.busy == 0 && !se.closed {
			se.idle.Reset(se.s.idleLimit)
		}
		se.mu.Unlock()
	}()

	start := time.Now()
	id, ch := se.q.add(cmd)
	defer se.q.forget(id)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case res := <-ch:
			if res.Error != "" {
				return agent.CommandResult{}, errors.New("the machine could not run the command: " + res.Error)
			}
			return agent.CommandResult{ExitCode: res.ExitCode, Output: res.Output, TimedOut: res.TimedOut, Elapsed: time.Since(start)}, nil
		case <-ctx.Done():
			return agent.CommandResult{}, ctx.Err()
		case <-se.q.done:
			return agent.CommandResult{}, errors.New("the session is closed")
		case <-deadline.C:
			return agent.CommandResult{}, errors.New("the machine did not answer in time")
		case <-tick.C:
			// The machine may have lost the session, or hit its time limit.
			state, cancel, summary, err := se.s.st.RunState(se.run)
			if err == nil && (state != "queued" && state != "running" || cancel) {
				return agent.CommandResult{}, fmt.Errorf("the session ended: %s%s", summary, se.s.outputTail(se.run))
			}
		}
	}
}

// outputTail is the end of a run's output, to say why a session did not start or ended.
func (s *Server) outputTail(run int64) string {
	r, err := s.st.Run(run)
	if err != nil {
		return ""
	}
	out := strings.TrimSpace(r.Output)
	if len(out) > 1500 {
		out = "…" + out[len(out)-1500:]
	}
	if out == "" {
		return ""
	}
	return "\nIts output ended:\n" + out
}

// Close ends the session: its run is cancelled, so the connector removes the container.
func (se *Session) Close() {
	se.once.Do(func() {
		se.mu.Lock()
		se.closed = true
		se.idle.Stop()
		se.mu.Unlock()
		se.s.st.RequestCancel(se.run)
		se.s.sessions.remove(se.run)
	})
}

// RunID is the id of the run behind the session.
func (se *Session) RunID() int64 { return se.run }
