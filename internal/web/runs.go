package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// recentRuns is how many ended runs the Runs page lists, after every run in progress.
const recentRuns = 100

// canvasTests is how many canvas tests it lists.
const canvasTests = 50

// runRow is one workflow run on the Runs page, or on its item's case page.
type runRow struct {
	ID        int64
	Item      int
	ItemURL   string // the pull request or issue on GitHub
	Title     string
	Kind      string // "pull request" or "issue"
	Block     string // the event block it started from
	Status    string
	Dot       string // busy, on or off
	Dry       bool
	Started   time.Time
	By        string
	Took      string
	StoppedBy string
}

// testRow is one canvas test on the Runs page.
type testRow struct {
	ID      int64
	Block   string
	Item    int
	Model   string
	Status  string
	Dot     string
	Started time.Time
	By      string
	Took    string
}

// itemURL is where a pull request or issue is on GitHub.
func itemURL(ws *store.Workspace, kind string, number int) string {
	path := "pull"
	if kind == workflow.Issue {
		path = "issues"
	}
	return fmt.Sprintf("https://github.com/%s/%s/%d", ws.Name, path, number)
}

// took is how long a run went on, or has gone on so far.
func took(start, end time.Time) string {
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(start).Round(time.Second).String()
}

// runRows makes the rows for runs: their items' titles come from the local copy of pull requests and
// issues, or failing that from the item's case file.
func (s *Server) runRows(ws *store.Workspace, runs []store.WorkflowRun) []runRow {
	titles := map[int]string{}
	var numbers []int
	for _, r := range runs {
		if _, ok := titles[r.Item]; !ok {
			titles[r.Item] = ""
			numbers = append(numbers, r.Item)
		}
	}
	if briefs, err := s.st.ItemIndex(ws.ID).Briefs(numbers); err == nil {
		for n, b := range briefs {
			titles[n] = b.Title
		}
	}
	for _, n := range numbers {
		if titles[n] == "" {
			if c, err := s.st.CaseByNumber(ws.ID, n); err == nil {
				titles[n] = c.Title
			}
		}
	}
	var rows []runRow
	for _, r := range runs {
		row := runRow{ID: r.ID, Item: r.Item, ItemURL: itemURL(ws, r.Kind, r.Item), Title: titles[r.Item], Kind: "pull request",
			Status: r.Status, Dry: r.DryRun, Started: r.StartedAt, By: r.StartedBy, StoppedBy: r.StoppedBy}
		if r.Kind == workflow.Issue {
			row.Kind = "issue"
		}
		var g workflow.Graph
		json.Unmarshal([]byte(r.Graph), &g)
		for _, n := range g.Nodes {
			if n.ID == r.EventNode {
				row.Block = g.NameOf(n)
			}
		}
		end := r.FinishedAt
		row.Took = took(r.StartedAt, end)
		row.Dot = map[string]string{"running": "busy", "done": "on"}[r.Status]
		if row.Dot == "" {
			row.Dot = "off"
		}
		rows = append(rows, row)
	}
	return rows
}

// runsPage lists a workspace's workflow runs, those in progress first, then the most recent, and its
// recent canvas tests; while any is going the page updates itself.
func (s *Server) runsPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	runs, err := s.st.WorkflowRuns(ws.ID, recentRuns)
	if err != nil {
		s.fail(w, err)
		return
	}
	tests, err := s.st.CanvasTests(ws.ID, canvasTests)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows := s.runRows(ws, runs)
	running := false
	for _, row := range rows {
		running = running || row.Status == "running"
	}
	var trows []testRow
	for _, t := range tests {
		row := testRow{ID: t.ID, Block: t.NodeName, Item: t.Item, Model: t.Model, Started: t.StartedAt, By: t.StartedBy, Took: took(t.StartedAt, t.FinishedAt)}
		row.Status = map[string]string{"running": "running", agent.Met: "goal met", agent.Blocked: "blocked", agent.Budget: "out of budget",
			agent.Unfinished: "stopped without finishing", agent.Failed: "failed", agent.Stopped: "stopped"}[t.Status]
		row.Dot = map[string]string{"running": "busy", agent.Met: "on"}[t.Status]
		if row.Dot == "" {
			row.Dot = "off"
		}
		running = running || t.Status == "running"
		trows = append(trows, row)
	}
	s.render(w, r, http.StatusOK, "runs.html", map[string]any{"Title": "Runs · " + ws.Name, "WS": ws, "Rows": rows, "Tests": trows,
		"Running": running, "Recent": recentRuns})
}
