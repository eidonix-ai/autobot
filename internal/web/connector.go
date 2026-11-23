package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
	"github.com/eidonix-ai/autobot/internal/store"
)

// live is what each machine's connector last reported, kept in memory: a connector reports every
// two seconds, so after a restart it is back within moments.
type live struct {
	mu      sync.Mutex
	started time.Time
	byID    map[int64]liveStatus
}

type liveStatus struct {
	protocol.Status
	Seen time.Time
}

func (l *live) set(id int64, st protocol.Status) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID[id] = liveStatus{st, time.Now()}
}

// get returns a machine's last report and whether it is online (reported in the last 10 seconds).
func (l *live) get(id int64) (liveStatus, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.byID[id]
	return st, ok && time.Since(st.Seen) < 5*protocol.Heartbeat
}

// lastSeen is when a machine last reported, or when Autobot started if it has not since.
func (l *live) lastSeen(id int64) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, ok := l.byID[id]; ok {
		return st.Seen
	}
	return l.started
}

// connectorAPI admits requests carrying a machine's token.
func (s *Server) connectorAPI(next func(http.ResponseWriter, *http.Request, *store.Machine)) http.Handler {
	return s.connectorAPIMax(protocol.OutputChunk+4096, next)
}

// connectorAPIMax is connectorAPI for a request that may carry up to max bytes.
func (s *Server) connectorAPIMax(max int64, next func(http.ResponseWriter, *http.Request, *store.Machine)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var m *store.Machine
		var err error
		if ok {
			m, err = s.st.MachineByToken(token)
		}
		if !ok || errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown or removed machine token", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next(w, r, m)
	})
}

func reply(w http.ResponseWriter, v protocol.Reply) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// activeRun is the machine's queued or running run, or nil.
func (s *Server) activeRun(m *store.Machine) (*store.TestRun, error) {
	run, err := s.st.ActiveRun(m.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return run, err
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, m *store.Machine) {
	var st protocol.Status
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		http.Error(w, "bad status: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.live.set(m.ID, st)
	run, err := s.activeRun(m)
	if err != nil {
		s.fail(w, err)
		return
	}
	var out protocol.Reply
	// Autobot handed this machine a run that its connector no longer has: it restarted, or the
	// reply that carried the run was lost.
	if run != nil && run.State == "running" && st.Run != run.ID {
		s.st.FinishRun(run.ID, "error", "The machine lost this run: its connector restarted or never started it.")
		run = nil
	}
	if st.Run != 0 && (run == nil || run.ID != st.Run || run.Cancel) {
		out.Cancel = true
	}
	if st.Run == 0 && st.Problem == "" && m.Enabled && run != nil && run.State == "queued" {
		job, err := s.job(run)
		if err != nil {
			s.fail(w, err)
			return
		}
		if ok, err := s.st.StartRun(run.ID); err != nil {
			s.fail(w, err)
			return
		} else if ok {
			out.Job = job
		}
	}
	reply(w, out)
}

func (s *Server) job(run *store.TestRun) (*protocol.Job, error) {
	j := &protocol.Job{Run: run.ID, Commit: run.Commit, Private: run.Private, Script: run.Script, Session: run.Session, GPUs: run.GPUs, TimeoutSec: int(run.Timeout / time.Second)}
	if run.Commit != "" {
		ws, err := s.st.WorkspaceByID(run.WorkspaceID)
		if err != nil {
			return nil, err
		}
		j.Repo = ws.Name
	}
	return j, nil
}

// machineRun returns the run in the URL if it belongs to the machine; to any other machine it does
// not exist.
func (s *Server) machineRun(w http.ResponseWriter, r *http.Request, m *store.Machine) (*store.TestRun, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.Run(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.MachineID != m.ID) {
		http.Error(w, "no such run for this machine", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return run, true
}

// runSource streams a private run's code to its machine. Autobot fetches it from GitHub with the
// workspace's token, so the token never leaves Autobot. A public run's connector downloads straight
// from GitHub, so its code is refused here: it would only cost Autobot's bandwidth.
func (s *Server) runSource(w http.ResponseWriter, r *http.Request, m *store.Machine) {
	run, ok := s.machineRun(w, r, m)
	if !ok {
		return
	}
	if run.State != "running" || run.Commit == "" || !run.Private {
		http.Error(w, "this run has no code to download from Autobot: a public repository comes straight from GitHub", http.StatusConflict)
		return
	}
	ws, err := s.st.WorkspaceByID(run.WorkspaceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	token, err := s.token(ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	body, err := s.gh.Tarball(r.Context(), token, ws.Name, run.Commit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()
	// A large repository can take longer than the server's usual write limit.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, body)
}

func (s *Server) runOutput(w http.ResponseWriter, r *http.Request, m *store.Machine) {
	run, ok := s.machineRun(w, r, m)
	if !ok {
		return
	}
	text, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "output chunk too large", http.StatusRequestEntityTooLarge)
		return
	}
	running, full, err := s.st.AppendOutput(run.ID, string(text), protocol.MaxOutput)
	if err != nil {
		s.fail(w, err)
		return
	}
	if running && full {
		s.st.RequestCancel(run.ID)
	}
	reply(w, protocol.Reply{Cancel: !running || full || run.Cancel})
}

func (s *Server) runResult(w http.ResponseWriter, r *http.Request, m *store.Machine) {
	run, ok := s.machineRun(w, r, m)
	if !ok {
		return
	}
	var res protocol.Result
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, "bad result: "+err.Error(), http.StatusBadRequest)
		return
	}
	state, summary := judge(res, run.OutputSize >= protocol.MaxOutput, run.Timeout)
	if run.Session && res.Error == "" && (res.Cancelled || res.TimedOut) {
		// A session has no script to pass or fail: closing it, or reaching its time limit, is how it ends.
		state, summary = "passed", "Session closed."
		if res.TimedOut {
			summary = fmt.Sprintf("Session ended at its time limit of %s.", run.Timeout)
		}
	}
	if err := s.st.FinishRun(run.ID, state, summary); err != nil {
		s.fail(w, err)
		return
	}
	reply(w, protocol.Reply{})
}

// judge turns a connector's result into the run's state and a one-line summary.
func judge(res protocol.Result, outputFull bool, timeout time.Duration) (string, string) {
	switch {
	case res.Error != "":
		return "error", "Did not complete: " + res.Error
	case res.Cancelled && outputFull:
		return "failed", fmt.Sprintf("Stopped: its output passed %d MB.", protocol.MaxOutput>>20)
	case res.Cancelled:
		return "error", "Stopped on request."
	case res.TimedOut:
		return "failed", fmt.Sprintf("Timed out after %s.", timeout)
	case res.OOMKilled:
		return "failed", fmt.Sprintf("Ran out of memory (exit code %d).", res.ExitCode)
	case res.ExitCode == 0:
		return "passed", "Passed."
	default:
		return "failed", fmt.Sprintf("Failed with exit code %d.", res.ExitCode)
	}
}

// Watch ends runs whose machine has gone quiet, every 15 seconds until ctx ends.
func (s *Server) Watch(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.sweep(); err != nil {
				log.Printf("checking runs: %v", err)
			}
		}
	}
}

func (s *Server) sweep() error {
	runs, err := s.st.ActiveRuns()
	if err != nil {
		return err
	}
	for _, r := range runs {
		switch {
		case r.State == "queued" && time.Since(r.CreatedAt) > 2*time.Minute:
			s.st.FinishRun(r.ID, "error", "The machine did not pick it up within 2 minutes.")
		case r.State == "running" && time.Since(s.live.lastSeen(r.MachineID)) > time.Minute:
			s.st.FinishRun(r.ID, "error", "The machine stopped checking in during the run.")
		case r.State == "running" && time.Since(r.StartedAt) > r.Timeout+5*time.Minute:
			s.st.FinishRun(r.ID, "error", "The machine did not report back after the time limit.")
		}
	}
	return nil
}
