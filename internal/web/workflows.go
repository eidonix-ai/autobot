package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

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

// canvasFacts is what the canvas shows of each node besides its type: its name and outputs.
func canvasFacts(g workflow.Graph) (map[string]string, map[string][]string) {
	names, outputs := map[string]string{}, map[string][]string{}
	for _, n := range g.Nodes {
		names[n.ID], outputs[n.ID] = workflow.DisplayName(n), workflow.Outputs(n)
	}
	return names, outputs
}

// summaries is each node's one-line summary, for the canvas.
func summaries(g workflow.Graph, kind string) map[string]string {
	out := map[string]string{}
	for _, n := range g.Nodes {
		out[n.ID] = workflow.Summary(n, kind, func(m workflow.ModelRef) string { return m.Model })
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
	names, outputs := canvasFacts(g)
	state, _ := json.Marshal(map[string]any{"version": stored.Version, "graph": g, "summaries": summaries(g, kind),
		"names": names, "outputs": outputs, "types": workflow.TypesFor(kind)})
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
			node.Settings, node.Name = k.Settings, k.Name
		}
		g.Nodes = append(g.Nodes, node)
	}
	if g.Links == nil {
		g.Links = []workflow.Link{}
	}
	s.storeGraph(w, ws, kind, req.Version, g, me(r))
}

// modelOption is one entry of a step's model dropdown.
type modelOption struct {
	Value, Label string
	Selected     bool
}

func modelValue(m workflow.ModelRef) string { return strconv.FormatInt(m.Provider, 10) + ":" + m.Model }

func parseModel(v string) workflow.ModelRef {
	p, m, _ := strings.Cut(v, ":")
	id, _ := strconv.ParseInt(p, 10, 64)
	return workflow.ModelRef{Provider: id, Model: m}
}

// usable reports whether the workspace may use a model as a text model, or as a decision model.
func usable(a store.AvailableModel, decisions bool) bool {
	if decisions {
		return a.DecisionAPI != ""
	}
	return a.Text
}

// modelOptions lists the models a step may choose: the workspace's text or decision models, with
// the current choice first when it is no longer assigned.
func (s *Server) modelOptions(ws *store.Workspace, decisions bool, cur workflow.ModelRef) ([]modelOption, error) {
	all, err := s.st.AvailableModels(ws.ID)
	if err != nil {
		return nil, err
	}
	var out []modelOption
	found := false
	for _, a := range all {
		if !usable(a, decisions) {
			continue
		}
		ref := workflow.ModelRef{Provider: a.ProviderID, Model: a.Model}
		sel := ref == cur
		found = found || sel
		out = append(out, modelOption{modelValue(ref), a.Model + " · " + a.ProviderName, sel})
	}
	if cur.Set() && !found {
		out = append([]modelOption{{modelValue(cur), cur.Model + " (no longer assigned)", true}}, out...)
	}
	return out, nil
}

// checkModel confirms the workspace may use the model for the step.
func (s *Server) checkModel(ws *store.Workspace, m workflow.ModelRef, decisions bool) error {
	all, err := s.st.AvailableModels(ws.ID)
	if err != nil {
		return err
	}
	for _, a := range all {
		if a.ProviderID == m.Provider && a.Model == m.Model && usable(a, decisions) {
			return nil
		}
	}
	if decisions {
		return errors.New("choose one of this workspace's decision models")
	}
	return errors.New("choose one of this workspace's text models")
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
	names, outputs := canvasFacts(g)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"version": version, "summaries": summaries(g, kind), "names": names, "outputs": outputs, "links": g.Links})
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
	var settings any
	var err error
	switch n.Type {
	case workflow.Agent:
		settings, err = workflow.AgentSettingsOf(n, kind)
	case workflow.Decision:
		settings, err = workflow.DecisionSettingsOf(n, kind)
	case workflow.CaseEntry:
		settings, err = workflow.CaseEntrySettingsOf(n)
	default:
		settings, err = workflow.EventSettingsOf(n)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.nodeForm(w, r, http.StatusOK, ws, kind, g, n, settings, "")
}

// nodeForm renders a node's settings form: the event, agent or decision form, by its type.
func (s *Server) nodeForm(w http.ResponseWriter, r *http.Request, status int, ws *store.Workspace, kind string, g workflow.Graph, n workflow.Node, settings any, problem string) {
	t, _ := workflow.TypeOf(n.Type)
	data := map[string]any{"WS": ws, "Kind": kind, "Node": n, "Type": t, "Name": workflow.DisplayName(n),
		"S": settings, "Error": problem, "Action": ws.Path() + "/workflows/" + kind + "/nodes/" + n.ID}
	page := "node_event.html"
	var model workflow.ModelRef
	switch st := settings.(type) {
	case workflow.AgentSettings:
		page, model = "node_agent.html", st.Model
		data["Tools"] = workflow.AgentTools
	case workflow.DecisionSettings:
		page, model = "node_decision.html", st.Model
	case workflow.CaseEntrySettings:
		page = "node_case.html"
	default:
		data["Activities"] = workflow.Activities
	}
	if page != "node_event.html" {
		if n.Type != workflow.CaseEntry {
			models, err := s.modelOptions(ws, n.Type == workflow.Decision, model)
			if err != nil {
				s.fail(w, err)
				return
			}
			data["Models"] = models
		}
		var outs []string
		for tag := range g.OutputTags(kind, n.ID) {
			outs = append(outs, tag)
		}
		sort.Strings(outs)
		data["Tags"], data["OutputTags"] = workflow.Tags(kind), outs
	}
	s.renderFragment(w, r, status, page, data)
}

// saveNodeSettings stores a node's settings from its form. A decision whose choices changed loses
// the links from choices it no longer has.
func (s *Server) saveNodeSettings(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := &g.Nodes[i]
	f := r.PostForm
	n.Name = strings.TrimSpace(f.Get("name"))
	if t, _ := workflow.TypeOf(n.Type); n.Name == t.Label {
		n.Name = "" // the default needs no storing
	}
	outputs := g.OutputTags(kind, n.ID)
	var settings any
	var err error
	switch n.Type {
	case workflow.Agent:
		a := workflow.AgentFromForm(f.Get, func(k string) []string { return f[k] })
		a.Model = parseModel(f.Get("model"))
		if err = a.Check(kind); err == nil {
			if err = workflow.CheckTemplates(kind, outputs, a.SystemPrompt, a.Instruction, a.FollowUp); err == nil {
				err = s.checkModel(ws, a.Model, false)
			}
		}
		settings = a
	case workflow.Decision:
		d := workflow.DecisionFromForm(f.Get)
		d.Model = parseModel(f.Get("model"))
		if err = d.Check(kind); err == nil {
			if err = workflow.CheckTemplates(kind, outputs, d.Context, d.Question); err == nil {
				err = s.checkModel(ws, d.Model, true)
			}
		}
		settings = d
	case workflow.CaseEntry:
		c := workflow.CaseEntryFromForm(f.Get)
		if err = c.Check(); err == nil {
			err = workflow.CheckTemplates(kind, outputs, c.Entry)
		}
		settings = c
	default:
		e := workflow.EventFromForm(n.Type, f.Get, func(k string) []string { return f[k] })
		err = e.Check(n.Type)
		settings = e
	}
	if err != nil {
		s.nodeForm(w, r, http.StatusBadRequest, ws, kind, g, *n, settings, err.Error()+".")
		return
	}
	b, err := json.Marshal(settings)
	if err != nil {
		s.fail(w, err)
		return
	}
	n.Settings = b
	ports := workflow.Outputs(*n)
	links := g.Links[:0]
	for _, l := range g.Links {
		if l.From != n.ID || slices.Contains(ports, l.Out) {
			links = append(links, l)
		}
	}
	g.Links = links
	base, _ := strconv.Atoi(f.Get("version"))
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
