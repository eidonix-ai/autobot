package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// itemRow is one pull request or issue on the Pull requests page.
type itemRow struct {
	store.ItemRow
	URL       string // on GitHub
	Status    string // draft, ready, open, merged or closed
	Ago       string // last activity, relative
	Run       *itemRun
	CaseURL   string // "" without a real case file
	LabelText string
}

// itemRun is the latest workflow run on an item.
type itemRun struct {
	ID     int64
	Status string
	Dot    string // busy, on or off
	Dry    bool
	At     time.Time
}

// since says how long before now t was, roughly: "just now", "5 minutes ago", "3 days ago".
func since(now, t time.Time) string {
	d := now.Sub(t)
	n := func(v int, unit string) string {
		if v == 1 {
			return "1 " + unit + " ago"
		}
		return strconv.Itoa(v) + " " + unit + "s ago"
	}
	switch {
	case t.IsZero():
		return ""
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return n(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return n(int(d/time.Hour), "hour")
	default:
		return n(int(d/(24*time.Hour)), "day")
	}
}

// itemsPage lists a workspace's pull requests or issues from the local copy, with a form to run the
// workflow on each. While a run on any listed item is going the page updates itself.
func (s *Server) itemsPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	kind, ok := workflowKind(w, r)
	if !ok {
		return
	}
	s.renderItems(w, r, http.StatusOK, ws, kind, "")
}

// itemFilters reads the page's filters from the address; a state it does not know is Open.
func itemFilters(q url.Values) (words, state string, withCase bool) {
	words, state, withCase = strings.TrimSpace(q.Get("q")), q.Get("state"), q.Get("case") == "on"
	if state != "closed" && state != "all" {
		state = "open"
	}
	return
}

func (s *Server) renderItems(w http.ResponseWriter, r *http.Request, status int, ws *store.Workspace, kind, problem string) {
	words, state, withCase := itemFilters(r.URL.Query())
	data := map[string]any{"WS": ws, "Kind": kind, "Q": words, "State": state, "HasCase": withCase, "Wide": true,
		"Title": workflow.KindLabel(kind) + " · " + ws.Name}
	var tabs []map[string]string
	for _, k := range workflow.Kinds {
		tabs = append(tabs, map[string]string{"Kind": k, "Label": workflow.KindLabel(k)})
	}
	data["Tabs"] = tabs
	if problem != "" {
		data["Error"] = problem
	}

	if st, err := index.StatusOf(s.st, ws.ID); err == nil && !st.Ready {
		data["Copying"] = st.Progress
	}
	// The event blocks on the canvas that the Run form offers.
	_, g, err := s.loadWorkflow(ws, kind)
	if err != nil {
		s.fail(w, err)
		return
	}
	var blocks []map[string]string
	for _, n := range g.Nodes {
		if t, _ := workflow.TypeOf(n.Type); t.Event {
			blocks = append(blocks, map[string]string{"ID": n.ID, "Name": g.NameOf(n)})
		}
	}
	data["Blocks"] = blocks

	rows, more, err := s.st.ItemIndex(ws.ID).Page(store.ItemPage{Kind: kind, Query: words, State: state, HasCase: withCase})
	if err != nil {
		if msg, ok := inputError(err); ok { // a search with no words in it
			data["Error"] = msg
			rows = nil
		} else {
			s.fail(w, err)
			return
		}
	}
	numbers := make([]int, len(rows))
	for i, row := range rows {
		numbers[i] = row.Number
	}
	last, running, err := s.st.LastRuns(ws.ID, kind, numbers)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now()
	var views []itemRow
	for _, row := range rows {
		v := itemRow{ItemRow: row, URL: itemURL(ws, kind, row.Number), Status: row.State, Ago: since(now, row.Updated), LabelText: strings.Join(row.Labels, ", ")}
		if kind == workflow.PR && row.State == "open" {
			v.Status = "ready"
			if row.Draft {
				v.Status = "draft"
			}
		}
		if row.HasCase {
			v.CaseURL = fmt.Sprintf("%s/cases/%d", ws.Path(), row.Number)
		}
		if b, ok := last[row.Number]; ok {
			v.Run = &itemRun{ID: b.ID, Status: b.Status, Dry: b.DryRun, At: b.Started,
				Dot: map[string]string{"running": "busy", "done": "on"}[b.Status]}
			if v.Run.Dot == "" {
				v.Run.Dot = "off"
			}
		}
		views = append(views, v)
	}
	data["Rows"], data["More"], data["Running"] = views, more, running && status == http.StatusOK
	s.render(w, r, status, "items.html", data)
}

// runItem starts the workflow on a listed item from the event block chosen, as shift-clicking that
// block on the canvas does, and goes to the new run's page.
func (s *Server) runItem(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	kind, ok := workflowKind(w, r)
	if !ok {
		return
	}
	num, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("item")), "#"))
	if num <= 0 {
		s.renderItems(w, r, http.StatusBadRequest, ws, kind, "Give the number of the pull request or issue.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	id, err := s.startWorkflow(ctx, ws, kind, r.PostForm.Get("block"), num, r.PostForm.Get("dry_run") == "on", me(r))
	if err != nil {
		msg := "Not started: " + err.Error() + "."
		if errors.Is(err, store.ErrNotFound) {
			msg = "Not started: that is not an item GitHub has."
		}
		s.renderItems(w, r, http.StatusBadRequest, ws, kind, msg)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), id), http.StatusSeeOther)
}
