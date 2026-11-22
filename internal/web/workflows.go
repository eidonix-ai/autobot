package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// workflowKind reads the kind in the URL, writing a 404 for an unknown one.
func workflowKind(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.PathValue("kind")
	if k != workflow.PR && k != workflow.Issue {
		http.NotFound(w, r)
		return "", false
	}
	return k, true
}

func loadGraph(stored *store.StoredWorkflow) (workflow.Graph, error) {
	var g workflow.Graph
	err := json.Unmarshal([]byte(stored.Graph), &g)
	return g, err
}

// summaries is each event node's one-line summary, for the canvas.
func summaries(g workflow.Graph) map[string]string {
	out := map[string]string{}
	for _, n := range g.Nodes {
		if s, err := workflow.EventSettingsOf(n); err == nil {
			out[n.ID] = s.Summary()
		}
	}
	return out
}

func (s *Server) workflowsIndex(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, ws.Path()+"/workflows/"+workflow.PR, http.StatusSeeOther)
}

// workflowPage is the full-width canvas for one kind of workflow, with a tab per kind.
func (s *Server) workflowPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	kind, ok := workflowKind(w, r)
	if !ok {
		return
	}
	stored, err := s.st.Workflow(ws.ID, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	g, err := loadGraph(stored)
	if err != nil {
		s.fail(w, err)
		return
	}
	state, _ := json.Marshal(map[string]any{"version": stored.Version, "graph": g, "summaries": summaries(g),
		"types": workflow.TypesFor(kind)})
	var tabs []map[string]string
	for _, k := range workflow.Kinds {
		tabs = append(tabs, map[string]string{"Kind": k, "Label": workflow.KindLabel(k)})
	}
	s.render(w, r, http.StatusOK, "workflows.html", map[string]any{"Title": workflow.KindLabel(kind) + " workflow · " + ws.Name,
		"WS": ws, "Kind": kind, "Tabs": tabs, "State": string(state), "Wide": true,
		"Base": ws.Path() + "/workflows/" + kind})
}

// saveLayout stores the canvas's nodes (type and position) and links. Settings are not sent: each
// node keeps the settings stored for it, and a new node starts with its type's defaults.
func (s *Server) saveLayout(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	kind, ok := workflowKind(w, r)
	if !ok {
		return
	}
	var req struct {
		Version int `json:"version"`
		Nodes   []struct {
			ID   string  `json:"id"`
			Type string  `json:"type"`
			X    float64 `json:"x"`
			Y    float64 `json:"y"`
		} `json:"nodes"`
		Links []workflow.Link `json:"links"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "The canvas sent something unreadable; reload the page.")
		return
	}
	stored, err := s.st.Workflow(ws.ID, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	old, err := loadGraph(stored)
	if err != nil {
		s.fail(w, err)
		return
	}
	kept := map[string]workflow.Node{}
	for _, n := range old.Nodes {
		kept[n.ID] = n
	}
	g := workflow.Graph{Links: req.Links}
	for _, n := range req.Nodes {
		node := workflow.Node{ID: n.ID, Type: n.Type, X: n.X, Y: n.Y}
		if k, ok := kept[n.ID]; ok && k.Type == n.Type {
			node.Settings = k.Settings
		}
		g.Nodes = append(g.Nodes, node)
	}
	if g.Links == nil {
		g.Links = []workflow.Link{}
	}
	s.storeGraph(w, ws, kind, req.Version, g, me(r))
}

// storeGraph checks and saves a workflow and answers with its new version and summaries.
func (s *Server) storeGraph(w http.ResponseWriter, ws *store.Workspace, kind string, base int, g workflow.Graph, actor *store.User) {
	if err := g.Check(kind); err != nil {
		jsonError(w, http.StatusBadRequest, "Not saved: "+err.Error()+".")
		return
	}
	b, err := json.Marshal(g)
	if err != nil {
		s.fail(w, err)
		return
	}
	version, err := s.st.SaveWorkflow(ws.ID, kind, base, string(b), actor)
	if errors.Is(err, store.ErrStaleWorkflow) {
		jsonError(w, http.StatusConflict, "Someone else changed this workflow while you were editing it. Reload to see their changes.")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"version": version, "summaries": summaries(g)})
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// findNode returns the stored workflow and the node in the URL.
func (s *Server) findNode(w http.ResponseWriter, r *http.Request) (*store.Workspace, string, *store.StoredWorkflow, workflow.Graph, int, bool) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return nil, "", nil, workflow.Graph{}, 0, false
	}
	kind, ok := workflowKind(w, r)
	if !ok {
		return nil, "", nil, workflow.Graph{}, 0, false
	}
	stored, err := s.st.Workflow(ws.ID, kind)
	if err != nil {
		s.fail(w, err)
		return nil, "", nil, workflow.Graph{}, 0, false
	}
	g, err := loadGraph(stored)
	if err != nil {
		s.fail(w, err)
		return nil, "", nil, workflow.Graph{}, 0, false
	}
	for i, n := range g.Nodes {
		if n.ID == r.PathValue("node") {
			return ws, kind, stored, g, i, true
		}
	}
	http.NotFound(w, r)
	return nil, "", nil, workflow.Graph{}, 0, false
}

// nodeSettings is a node's settings form, for the canvas's modal.
func (s *Server) nodeSettings(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := g.Nodes[i]
	settings, err := workflow.EventSettingsOf(n)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.nodeForm(w, r, http.StatusOK, ws, kind, n, settings, "")
}

func (s *Server) nodeForm(w http.ResponseWriter, r *http.Request, status int, ws *store.Workspace, kind string, n workflow.Node, settings workflow.EventSettings, problem string) {
	t, _ := workflow.TypeOf(n.Type)
	s.renderFragment(w, r, status, "node_event.html", map[string]any{"WS": ws, "Kind": kind, "Node": n, "Type": t,
		"S": settings, "Activities": workflow.Activities, "Error": problem,
		"Action": ws.Path() + "/workflows/" + kind + "/nodes/" + n.ID})
}

// saveNodeSettings stores a node's settings from its form.
func (s *Server) saveNodeSettings(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := &g.Nodes[i]
	settings := workflow.EventFromForm(n.Type, r.PostForm.Get, func(k string) []string { return r.PostForm[k] })
	if err := settings.Check(n.Type); err != nil {
		s.nodeForm(w, r, http.StatusBadRequest, ws, kind, *n, settings, err.Error()+".")
		return
	}
	b, err := json.Marshal(settings)
	if err != nil {
		s.fail(w, err)
		return
	}
	n.Settings = b
	base, _ := strconv.Atoi(r.PostForm.Get("version"))
	s.storeGraph(w, ws, kind, base, g, me(r))
}

// renderFragment executes a page's "content" without the layout, for HTML loaded into a modal.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		s.fail(w, errors.New("no page "+page))
		return
	}
	if sv, ok := r.Context().Value(ctxKey{}).(*session); ok {
		data["Me"], data["CSRF"] = sv.user, sv.csrf
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "content", data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}
