package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
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
	Author    string // the item's author
	Kind      string // "pull request" or "issue"
	Block     string // the event block it started from
	State     string // the run's status: queued, running, done, failed or stopped
	Waiting   bool   // running, with nothing at work but a step waiting for a person's answer
	Status    string // State as the page says it: a queued run says its place in line
	Dot       string // busy, on or off
	Dry       bool
	Started   time.Time
	By        string // who started it: a person's name, or "Live" for the poller
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

// runRows makes the rows for runs: their items' titles and authors come from the local copy of pull
// requests and issues, the title failing that from the item's case file and the author from the
// poller's record of the item.
func (s *Server) runRows(ws *store.Workspace, runs []store.WorkflowRun) []runRow {
	titles, authors := map[int]string{}, map[int]string{}
	var numbers []int
	for _, r := range runs {
		if r.Item <= 0 {
			continue
		}
		if _, ok := titles[r.Item]; !ok {
			titles[r.Item] = ""
			numbers = append(numbers, r.Item)
		}
	}
	if briefs, err := s.st.ItemIndex(ws.ID).Briefs(numbers); err == nil {
		for n, b := range briefs {
			titles[n], authors[n] = b.Title, b.Author
		}
	}
	for _, n := range numbers {
		if titles[n] == "" {
			if c, err := s.st.CaseByNumber(ws.ID, n); err == nil {
				titles[n] = c.Title
			}
		}
		if authors[n] == "" {
			if it, err := s.st.PollItem(ws.ID, n); err == nil {
				authors[n] = it.Author
			}
		}
	}
	places := s.queuePlaces(ws.ID)
	var rows []runRow
	for _, r := range runs {
		row := runRow{ID: r.ID, Item: r.Item, ItemURL: itemURL(ws, r.Kind, r.Item), Title: titles[r.Item], Author: authors[r.Item], Kind: "pull request",
			State: r.Status, Status: r.Status, Dry: r.DryRun, Started: r.StartedAt, By: r.StartedBy, StoppedBy: r.StoppedBy}
		if r.StartedByID == 0 {
			if strings.HasPrefix(r.Note, backlogPrefix) {
				row.By = "Backlog"
			} else {
				row.By = "Live" // the poller or Discord listener started it
			}
		}
		if place, ok := places[r.ID]; ok {
			row.Status = fmt.Sprintf("queued (%s in line)", ordinal(place))
		}
		if r.Kind == workflow.Issue {
			row.Kind = "issue"
		} else if r.Kind == workflow.Discord {
			trig := parseDiscordTrigger(r.TriggerData)
			ch := trig.Channel
			if ch != "" && !strings.HasPrefix(ch, "#") {
				ch = "#" + ch
			}
			row.Kind = "discord"
			row.ItemURL = trig.URL
			row.Title = oneLineOf(trig.Content, 100)
			if ch != "" {
				row.Title = ch + " · " + row.Title
			}
			row.Author = trig.Author
		}
		var g workflow.Graph
		json.Unmarshal([]byte(r.Graph), &g)
		for _, n := range g.Nodes {
			if n.ID == r.EventNode {
				row.Block = g.NameOf(n)
			}
		}
		if r.Status != "queued" { // a queued run has not started to take any time
			row.Took = took(r.StartedAt, r.FinishedAt)
		}
		row.Dot = map[string]string{"running": "busy", "done": "on"}[r.Status]
		if row.Dot == "" {
			row.Dot = "off"
		}
		if r.Status == "running" {
			// A run whose only unfinished work is a person's answer gives its place to others (see
			// queue.go): it says so, and is not counted as one of those running.
			if name := waitingOn(s.st, r.ID, g); name != "" {
				row.Status, row.Dot, row.Waiting = "waiting: "+name, "wait", true
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// waitingOn is the name of the step a running run waits on for a person's answer, when no step of it
// is working; "" otherwise.
func waitingOn(st *store.Store, run int64, g workflow.Graph) string {
	steps, err := st.WorkflowSteps(run)
	if err != nil {
		return ""
	}
	name := ""
	for id, step := range steps {
		switch step.Status {
		case "running":
			return ""
		case "waiting":
			for _, n := range g.Nodes {
				if n.ID == id && (name == "" || g.NameOf(n) < name) {
					name = g.NameOf(n)
				}
			}
		}
	}
	return name
}

// runGroup is what the Status filter and sort go by: running (at work), waiting (for a person),
// queued, or how the run ended.
func runGroup(r runRow) string {
	switch {
	case r.Waiting:
		return "waiting"
	}
	return r.State
}

// groupRank orders the groups: at work, waiting, queued, then the ended ones.
var groupRank = map[string]int{"running": 0, "waiting": 1, "queued": 2, "done": 3, "failed": 4, "stopped": 5}

// filterRuns keeps the runs that match the search (a number, or words in the title, author, block,
// status or who started it) and the status ("" or "all" for any, "going" for any not ended).
func filterRuns(rows []runRow, q, show string) []runRow {
	words := strings.Fields(strings.ToLower(strings.TrimPrefix(q, "#")))
	return slices.DeleteFunc(rows, func(r runRow) bool {
		g := runGroup(r)
		switch show {
		case "", "all":
		case "going":
			if groupRank[g] > 2 {
				return true
			}
		default:
			if g != show {
				return true
			}
		}
		text := strings.ToLower(fmt.Sprintf("%d %s %s %s %s %s", r.Item, r.Title, r.Author, r.Block, r.Status, r.By))
		for _, w := range words {
			if !strings.Contains(text, w) {
				return true
			}
		}
		return false
	})
}

// sortRuns orders the runs: by default those at work, then waiting, then queued (each as listed),
// then the ended ones newest first; or by number (highest first), title, author, status or start.
func sortRuns(rows []runRow, order string) {
	byGroup := func(a, b runRow) int { return groupRank[runGroup(a)] - groupRank[runGroup(b)] }
	switch order {
	case "number":
		slices.SortStableFunc(rows, func(a, b runRow) int { return cmp.Or(b.Item-a.Item, int(b.ID-a.ID)) })
	case "title":
		slices.SortStableFunc(rows, func(a, b runRow) int { return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)) })
	case "author":
		slices.SortStableFunc(rows, func(a, b runRow) int { return strings.Compare(strings.ToLower(a.Author), strings.ToLower(b.Author)) })
	case "status":
		slices.SortStableFunc(rows, func(a, b runRow) int { return cmp.Or(byGroup(a, b), strings.Compare(a.Status, b.Status)) })
	case "started":
		slices.SortStableFunc(rows, func(a, b runRow) int { return b.Started.Compare(a.Started) })
	default:
		going := func(r runRow) int { return min(groupRank[runGroup(r)], 3) } // the ended ones keep their order
		slices.SortStableFunc(rows, func(a, b runRow) int { return going(a) - going(b) })
	}
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
		running = running || row.State == "running" || row.State == "queued"
	}
	q, show, order := strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("status"), r.URL.Query().Get("sort")
	rows = filterRuns(rows, q, show)
	sortRuns(rows, order)
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
		"Running": running, "Recent": recentRuns, "Wide": true,
		"Q": q, "Show": show, "Sort": order, "Filtered": q != "" || (show != "" && show != "all") || order != ""})
}
