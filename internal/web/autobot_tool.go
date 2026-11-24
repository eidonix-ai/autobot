package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// autobotStatusTool lets an agent inspect Autobot's own workspace status: active, waiting, queued
// and recent workflow runs (overall or for a specific PR/issue), GitHub issues/PRs list links and
// counts, public case links, and donated test machine status.
func (s *Server) autobotStatusTool(ws *store.Workspace) agent.Tool {
	return agent.Tool{
		Spec: agent.ToolSpec{
			Name:        "autobot_status",
			Description: "Inspect Autobot's own status for " + ws.Name + ": active, queued, waiting-for-approval, and recent workflow runs (optionally for one PR or issue number), open PR/issue counts, GitHub list links, public case link, and donated test machines.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"item": {"type": "integer", "description": "Optional PR or issue number to check workflow runs and case status for."},
					"limit": {"type": "integer", "description": "How many recent runs to return (default 15, max 30)."}
				}
			}`),
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in struct {
				Item  int `json:"item"`
				Limit int `json:"limit"`
			}
			if len(raw) > 0 {
				json.Unmarshal(raw, &in)
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 15
			}
			if limit > 30 {
				limit = 30
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Repository: %s\n", ws.Name)
			fmt.Fprintf(&b, "Issues list: https://github.com/%s/issues\n", ws.Name)
			fmt.Fprintf(&b, "Pull requests list: https://github.com/%s/pulls\n", ws.Name)
			if st, err := index.StatusOf(s.st, ws.ID); err == nil && st.Ready {
				fmt.Fprintf(&b, "Indexed items: %d open (%d total cached)\n", st.Open, st.Items)
			}

			var runs []store.WorkflowRun
			var err error
			if in.Item > 0 {
				runs, err = s.st.WorkflowRunsOfItem(ws.ID, in.Item, limit)
			} else {
				runs, err = s.st.WorkflowRuns(ws.ID, limit)
			}
			if err != nil {
				return "", err
			}
			rows := s.runRows(ws, runs)
			if in.Item > 0 {
				fmt.Fprintf(&b, "\nWorkflow runs for #%d (%d shown):\n", in.Item, len(rows))
			} else {
				fmt.Fprintf(&b, "\nWorkflow runs (%d shown, in-progress first):\n", len(rows))
			}
			if len(rows) == 0 {
				b.WriteString("- None.\n")
			}
			for _, r := range rows {
				dry := ""
				if r.Dry {
					dry = " [dry run]"
				}
				label := fmt.Sprintf("#%d %s", r.Item, r.Title)
				if r.Kind == workflow.Discord {
					label = fmt.Sprintf("Discord (%s): %s", r.Title, r.Author)
				}
				fmt.Fprintf(&b, "- Run %d · %s · %s · %s%s · started %s by %s",
					r.ID, label, r.Block, r.Status, dry, r.Started.In(eastern).Format("Jan 2 3:04 PM ET"), r.By)
				if r.Took != "" {
					fmt.Fprintf(&b, " (%s)", r.Took)
				}
				b.WriteByte('\n')
				// When inspecting a specific item, include per-step summary for the top 3 runs.
				if in.Item > 0 {
					if steps, serr := s.st.WorkflowSteps(r.ID); serr == nil {
						var parts []string
						var g workflow.Graph
						for _, run := range runs {
							if run.ID == r.ID {
								json.Unmarshal([]byte(run.Graph), &g)
								break
							}
						}
						for _, n := range g.Nodes {
							st := steps[n.ID]
							if st.Status == "" || st.Status == "skipped" || st.Status == "pending" {
								continue
							}
							sline := fmt.Sprintf("%s=%s", g.NameOf(n), st.Status)
							if st.Port != "" && st.Port != "out" {
								sline += "→" + st.Port
							}
							if st.Error != "" && st.Status == "failed" {
								sline += "(" + oneLineOf(st.Error, 80) + ")"
							}
							parts = append(parts, sline)
						}
						if len(parts) > 0 {
							fmt.Fprintf(&b, "  Steps: %s\n", strings.Join(parts, ", "))
						}
					}
				}
			}

			// Include public case link when inspecting a specific item on a public repo.
			if in.Item > 0 && !s.facts(ctx, ws).private {
				if c, err := s.st.CaseByNumber(ws.ID, in.Item); err == nil {
					if link := s.caseLink(c); link != "" {
						fmt.Fprintf(&b, "\nCase file for #%d: %s\n", in.Item, link)
					}
				}
			}

			if m := strings.TrimSpace(s.machinesSummary(ws)); m != "" {
				fmt.Fprintf(&b, "\nTest machines:\n%s\n", m)
			}
			return b.String(), nil
		},
	}
}
