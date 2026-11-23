package web

import (
	"context"
	"net/http"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// setLive switches a workflow's poller on or off. Anyone who can open the workspace may. Switching
// on first marks everything open as already seen, so only what happens afterwards starts runs.
func (s *Server) setLive(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	kind, ok := workflowKind(w, r)
	if !ok {
		return
	}
	back := ws.Path() + "/workflows/" + kind
	stored, err := s.st.Workflow(ws.ID, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	on := r.PostForm.Get("live") == "on"
	if on == stored.Live {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	s.poll.note(ws.ID, kind, "")
	if on {
		if msg := s.goLive(r.Context(), ws, kind, stored); msg != "" {
			s.poll.note(ws.ID, kind, msg)
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
	}
	if err := s.st.SetWorkflowLive(ws.ID, kind, on, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// goLive checks a workflow can go live and baselines it; it returns what is wrong, or "".
func (s *Server) goLive(ctx context.Context, ws *store.Workspace, kind string, stored *store.StoredWorkflow) string {
	g, err := loadGraph(stored)
	if err != nil || stored.Version == 0 {
		return "Save the workflow before switching it live."
	}
	var events []workflow.Node
	for _, n := range g.Nodes {
		if t, _ := workflow.TypeOf(n.Type); t.Event {
			events = append(events, n)
		}
	}
	if len(events) == 0 {
		return "Add an event block, such as PR Ready, before switching it live."
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.pollBaseline(ctx, ws, kind, events); err != nil {
		return "Not switched live: GitHub could not be read (" + err.Error() + ")."
	}
	return ""
}

// pollView adds to a workflow page what the poller has to show: whether it is live, what it
// decided lately, and what is wrong.
func (s *Server) pollView(data map[string]any, ws *store.Workspace, kind string, stored *store.StoredWorkflow, g workflow.Graph) {
	names, _ := canvasFacts(g)
	type line struct {
		At         time.Time
		Number     int
		Node, Text string
		Run        int64
	}
	var recent []line
	log, err := s.st.PollLog(ws.ID, kind, 20)
	if err != nil {
		data["LiveProblem"] = "The poller's log could not be read."
	}
	for _, e := range log {
		name := names[e.Node]
		if name == "" {
			name = "(removed block)"
		}
		recent = append(recent, line{e.At, e.Number, name, e.Text, e.Run})
	}
	data["Live"], data["Recent"] = stored.Live, recent
	if p := s.poll.problem(ws.ID, kind); p != "" {
		data["LiveProblem"] = p
	}
}
