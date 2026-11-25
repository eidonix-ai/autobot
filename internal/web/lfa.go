package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// LFATrigger is the user-supplied Kick Off prompt stored in workflow_runs.trigger_data for a Large
// Feature Addition run so queued and restarted runs keep their full context.
type LFATrigger struct {
	PageID    string `json:"page_id"`
	PageTitle string `json:"page_title"`
	Title     string `json:"title"`
	Prompt    string `json:"prompt"`
	Base      string `json:"base,omitempty"`
	Author    string `json:"author,omitempty"`
}

func parseLFATrigger(raw string) LFATrigger {
	var t LFATrigger
	json.Unmarshal([]byte(raw), &t)
	return t
}

func lfaSlug(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if (unicode.IsSpace(r) || r == '-' || r == '_' || r == '/' || r == '.') && b.Len() > 0 && !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "feature"
	}
	return s
}

func (t LFATrigger) values(ws *store.Workspace) map[string]string {
	return map[string]string{
		"repo":       ws.Name,
		"lfa:title":  t.Title,
		"lfa:prompt": t.Prompt,
		"lfa:page":   t.PageTitle,
		"lfa:base":   t.Base,
		"lfa:slug":   lfaSlug(t.Title),
	}
}

// startLFAWorkflow starts a Large Feature Addition workflow run from a Kick Off event node.
func (s *Server) startLFAWorkflow(ctx context.Context, ws *store.Workspace, kind, eventNode string, trig LFATrigger, dry bool, by *store.User) (int64, error) {
	_, g, err := s.loadWorkflow(ws, kind)
	if err != nil {
		return 0, err
	}
	var ev *workflow.Node
	for i, n := range g.Nodes {
		if n.ID == eventNode {
			ev = &g.Nodes[i]
		}
	}
	if ev == nil {
		return 0, errors.New("that block is not on the canvas any more")
	}
	if t, _ := workflow.TypeOf(ev.Type); !t.Event {
		return 0, errors.New("a workflow runs from a Kick Off event block")
	}
	if trig.PageTitle == "" {
		if pid := workflow.LFAPageID(kind); pid != "" {
			if p, err := s.st.WorkflowPage(ws.ID, pid); err == nil {
				trig.PageID, trig.PageTitle = p.ID, p.Title
			}
		}
	}
	if trig.Author == "" && by != nil {
		trig.Author = by.Name()
	}
	if strings.TrimSpace(trig.Base) == "" {
		if token, err := s.token(ws); err == nil {
			if def, err := s.gh.DefaultBranch(ctx, token, ws.Name); err == nil && def != "" {
				trig.Base = def
			}
		}
	}

	graph, _ := json.Marshal(g)
	tdata, _ := json.Marshal(trig)
	var ids []string
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	note := fmt.Sprintf("%s · %s", trig.PageTitle, trig.Title)
	if trig.Base != "" {
		note += fmt.Sprintf(" (base: %s)", trig.Base)
	}
	if trig.Prompt != "" {
		note += "\n\n" + trig.Prompt
	}
	r := store.WorkflowRun{
		WorkspaceID: ws.ID,
		Kind:        kind,
		EventNode:   eventNode,
		Item:        0,
		TriggerData: string(tdata),
		DryRun:      dry,
		Graph:       string(graph),
		Note:        note,
	}
	id, queued, err := s.admit(r, ids, by)
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
	it := &item{kind: workflow.LFA, number: int(run.ID), author: trig.Author, title: trig.Title, body: trig.Prompt}
	rs := &runState{ws: ws, run: run, g: g, it: it, lfa: &trig, builtIn: trig.values(ws), tags: map[string]string{}, by: by}
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		rs.builtIn["charter"] = c.Body
	}
	rs.builtIn["machines"] = s.machinesSummary(ws)
	s.launch(rs)
	return id, nil
}

// defaultLFAGraph is the starter graph placed on a newly created user-defined LFA page: one Kick Off block.
func defaultLFAGraph() string {
	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "kickoff00001", Type: workflow.LFAKickOff, X: 80, Y: 140},
		},
		Links: []workflow.Link{},
	}
	b, _ := json.Marshal(g)
	return string(b)
}

func (s *Server) createLFAPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	title := strings.TrimSpace(r.PostForm.Get("title"))
	desc := strings.TrimSpace(r.PostForm.Get("description"))
	p, err := s.st.CreateWorkflowPage(ws, title, desc, defaultLFAGraph(), me(r))
	if err != nil {
		if msg, ok := inputError(err); ok {
			http.Redirect(w, r, ws.Path()+"/workflows/lfa?error="+urlQueryEscape(msg), http.StatusSeeOther)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/workflows/"+workflow.LFAKind(p.ID), http.StatusSeeOther)
}

func (s *Server) renameLFAPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page")
	title := strings.TrimSpace(r.PostForm.Get("title"))
	desc := strings.TrimSpace(r.PostForm.Get("description"))
	if err := s.st.UpdateWorkflowPage(ws.ID, pageID, title, desc); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if msg, ok := inputError(err); ok {
			http.Redirect(w, r, ws.Path()+"/workflows/"+workflow.LFAKind(pageID)+"?error="+urlQueryEscape(msg), http.StatusSeeOther)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/workflows/"+workflow.LFAKind(pageID), http.StatusSeeOther)
}

func (s *Server) deleteLFAPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	pageID := r.PathValue("page")
	if err := s.st.DeleteWorkflowPage(ws.ID, pageID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/workflows/lfa", http.StatusSeeOther)
}

func urlQueryEscape(s string) string {
	r := strings.NewReplacer("%", "%25", " ", "+", "&", "%26", "=", "%3D", "?", "%3F", "#", "%23")
	return r.Replace(s)
}
