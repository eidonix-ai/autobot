package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
	"github.com/eidonix-ai/autobot/internal/store"
)

// machineView is a machine with what its connector last reported.
type machineView struct {
	store.Machine
	Path   string
	Online bool
	Live   liveStatus
	Run    *store.TestRun // its queued or running run
}

// State is the machine's condition in a few words.
func (m machineView) State() string {
	switch {
	case !m.Online && m.Live.Seen.IsZero():
		return "Never connected"
	case !m.Online:
		return "Offline"
	case m.Run != nil && m.Run.State == "queued":
		return fmt.Sprintf("Starting run %d", m.Run.ID)
	case m.Run != nil:
		return fmt.Sprintf("Running run %d", m.Run.ID)
	case m.Live.Problem != "":
		return "Not taking runs: " + m.Live.Problem
	default:
		return "Ready"
	}
}

// Ready reports whether a run can start on the machine now.
func (m machineView) Ready() bool { return m.Online && m.Run == nil && m.Live.Problem == "" }

func (s *Server) machineViews(ws *store.Workspace) ([]machineView, error) {
	machines, err := s.st.Machines(ws.ID)
	if err != nil {
		return nil, err
	}
	var out []machineView
	for _, m := range machines {
		v, err := s.machineView(ws, m)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) machineView(ws *store.Workspace, m store.Machine) (machineView, error) {
	v := machineView{Machine: m, Path: fmt.Sprintf("%s/machines/%d", ws.Path(), m.ID)}
	v.Live, v.Online = s.live.get(m.ID)
	run, err := s.activeRun(&m)
	v.Run = run
	return v, err
}

// openMachine resolves the machine in the URL within the workspace.
func (s *Server) openMachine(w http.ResponseWriter, r *http.Request) (*store.Workspace, *store.Machine, bool) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return nil, nil, false
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	m, err := s.st.Machine(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (m.WorkspaceID != ws.ID || m.Revoked)) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, nil, false
	}
	return ws, m, true
}

func (s *Server) newMachinePage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "machine_new.html", map[string]any{"Title": "Add a machine · " + ws.Name, "WS": ws})
}

func (s *Server) addMachine(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	m, token, err := s.st.AddMachine(ws.ID, r.PostForm.Get("name"), me(r))
	if msg, ok := inputError(err); ok {
		s.render(w, r, http.StatusBadRequest, "machine_new.html", map[string]any{"Title": "Add a machine · " + ws.Name, "WS": ws,
			"Error": msg, "Form": r.PostForm})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// Shown once: the page is not cached and the token is not stored anywhere readable.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "machine_added.html", map[string]any{"Title": "Machine added · " + ws.Name, "WS": ws,
		"Machine": m, "Token": token, "URL": publicURL(r)})
}

// publicURL is the address connectors reach Autobot at, as this request came in.
func publicURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) machinePage(w http.ResponseWriter, r *http.Request) {
	ws, m, ok := s.openMachine(w, r)
	if !ok {
		return
	}
	s.machinePageWith(w, r, http.StatusOK, ws, m, map[string]any{})
}

func (s *Server) machinePageWith(w http.ResponseWriter, r *http.Request, status int, ws *store.Workspace, m *store.Machine, data map[string]any) {
	v, err := s.machineView(ws, *m)
	if err != nil {
		s.fail(w, err)
		return
	}
	runs, err := s.st.Runs(m.ID, 20)
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, ok := data["Form"]; !ok {
		data["Form"] = url.Values{"script": {defaultScript}, "minutes": {"30"}, "gpus": {"1"}}
	}
	data["Title"], data["WS"], data["M"], data["Runs"] = m.Name+" · "+ws.Name, ws, v, runs
	s.render(w, r, status, "machine.html", data)
}

const defaultScript = `nvidia-smi
ls /models`

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	ws, m, ok := s.openMachine(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	again := func(msg string) {
		s.machinePageWith(w, r, http.StatusBadRequest, ws, m, map[string]any{"Error": msg, "Form": f})
	}
	script := strings.TrimSpace(f.Get("script"))
	minutes, _ := strconv.Atoi(f.Get("minutes"))
	gpus, err := strconv.Atoi(f.Get("gpus"))
	switch {
	case script == "":
		again("Write the script the run should execute.")
		return
	case len(script) > 64<<10:
		again("The script is limited to 64 KB.")
		return
	case minutes < 1 || minutes > int(protocol.MaxRun/time.Minute):
		again(fmt.Sprintf("The time limit is 1 to %d minutes.", int(protocol.MaxRun/time.Minute)))
		return
	case err != nil || gpus < 0:
		again("Give the number of GPUs the run needs (0 for none).")
		return
	}
	v, err := s.machineView(ws, *m)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !v.Ready() {
		again(m.Name + " cannot take a run now: " + v.State() + ".")
		return
	}
	if gpus > len(v.Live.GPUs) {
		again(fmt.Sprintf("%s has %d GPUs.", m.Name, len(v.Live.GPUs)))
		return
	}
	ref, commit, private := strings.TrimSpace(f.Get("ref")), "", false
	if ref != "" {
		token, err := s.token(ws)
		if err != nil {
			s.fail(w, err)
			return
		}
		if commit, err = s.gh.CommitSHA(r.Context(), token, ws.Name, ref); err != nil {
			again(fmt.Sprintf("Could not find %s on GitHub: %v.", ref, err))
			return
		}
		// Without a token the repository is public: the commit was found anonymously.
		if token != "" {
			if private, err = s.gh.Private(r.Context(), token, ws.Name); err != nil {
				again(fmt.Sprintf("Could not check whether %s is private: %v.", ws.Name, err))
				return
			}
		}
	}
	run, err := s.st.QueueRun(m, script, ref, commit, private, gpus, time.Duration(minutes)*time.Minute, me(r))
	if msg, ok := inputError(err); ok {
		again(msg)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}

func (s *Server) removeMachine(w http.ResponseWriter, r *http.Request) {
	ws, m, ok := s.openMachine(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(r.PostForm.Get("confirm")) != m.Name {
		s.machinePageWith(w, r, http.StatusBadRequest, ws, m, map[string]any{"Error": "The machine was not removed: type its name, " + m.Name + ", to confirm."})
		return
	}
	if err := s.st.RevokeMachine(m, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path(), http.StatusSeeOther)
}

// openRun resolves the run in the URL within the workspace.
func (s *Server) openRun(w http.ResponseWriter, r *http.Request) (*store.Workspace, *store.TestRun, bool) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return nil, nil, false
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.Run(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, nil, false
	}
	return ws, run, true
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	ws, run, ok := s.openRun(w, r)
	if !ok {
		return
	}
	data := map[string]any{"Title": fmt.Sprintf("Run %d · %s", run.ID, ws.Name), "WS": ws, "Run": run,
		"MachinePath": fmt.Sprintf("%s/machines/%d", ws.Path(), run.MachineID)}
	if run.Active() {
		data["Refresh"] = r.URL.Path + "#end" // reload every few seconds, at the end of the output
	}
	s.render(w, r, http.StatusOK, "run.html", data)
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	ws, run, ok := s.openRun(w, r)
	if !ok {
		return
	}
	if err := s.st.RequestCancel(run.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}

// resourceSummary is the Resources tile's one line: how many machines, and the colour of its dot.
type resourceSummary struct {
	Total, Ready, Busy, Offline int
	Dot                         string // on (some ready), busy (online, none ready), off (none online)
}

func summarize(ms []machineView) resourceSummary {
	r := resourceSummary{Total: len(ms), Dot: "off"}
	for _, m := range ms {
		switch {
		case m.Ready():
			r.Ready++
		case m.Online:
			r.Busy++
		default:
			r.Offline++
		}
	}
	if r.Ready > 0 {
		r.Dot = "on"
	} else if r.Busy > 0 {
		r.Dot = "busy"
	}
	return r
}
