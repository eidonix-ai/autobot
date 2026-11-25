package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

const maxCaseToolChars = 24_000

// caseTools returns read-only tools for listing, searching, and reading Autobot's case files
// (findings, reviews, decisions, approvals, and saved step outputs) for a workspace.
func (s *Server) caseTools(ws *store.Workspace) []agent.Tool {
	return []agent.Tool{
		{
			Spec: agent.ToolSpec{
				Name:        "list_cases",
				Description: "List or search Autobot's case files for " + ws.Name + " (PR and issue investigations, reviews, decisions, and approvals). Optionally filter by keyword across title or contents.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "Optional keyword or PR/issue number to search in case titles and contents."},
						"limit": {"type": "integer", "description": "Maximum cases to return (default 20, max 50)."}
					}
				}`),
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				if len(raw) > 0 {
					json.Unmarshal(raw, &in)
				}
				limit := in.Limit
				if limit <= 0 {
					limit = 20
				}
				if limit > 50 {
					limit = 50
				}
				cases, err := s.st.SearchCases(ws.ID, in.Query, limit)
				if err != nil {
					return "", err
				}
				if len(cases) == 0 {
					if strings.TrimSpace(in.Query) != "" {
						return fmt.Sprintf("No case files in %s matched %q.", ws.Name, in.Query), nil
					}
					return fmt.Sprintf("There are no case files in %s yet.", ws.Name), nil
				}
				public := !s.facts(ctx, ws).private
				var b strings.Builder
				fmt.Fprintf(&b, "Case files in %s (%d shown, most recently updated first):\n", ws.Name, len(cases))
				for _, c := range cases {
					fmt.Fprintf(&b, "- %s: %s · updated %s · %s",
						c.Label(), c.Title, c.UpdatedAt.In(eastern).Format("Jan 2 3:04 PM ET"), itemURL(ws, c.Kind, c.Number))
					if public {
						if link := s.caseLink(&c); link != "" {
							fmt.Fprintf(&b, " · case: %s", link)
						}
					}
					b.WriteByte('\n')
				}
				return b.String(), nil
			},
		},
		{
			Spec: agent.ToolSpec{
				Name:        "read_case",
				Description: "Read Autobot's case file for a specific PR or issue number in " + ws.Name + " (including all finalized research summaries, code review findings, decisions, approvals, and links). Optionally read a line range or a specific saved step output tag.",
				Parameters: json.RawMessage(`{
					"type": "object",
					"properties": {
						"item": {"type": "integer", "description": "The PR or issue number."},
						"start_line": {"type": "integer", "description": "Optional 1-based start line when paging through a long case file."},
						"end_line": {"type": "integer", "description": "Optional 1-based end line (inclusive)."},
						"part": {"type": "string", "description": "Optional saved output tag name on the case (e.g. gather_information.report or lens_team.report) to read in full instead of the case log."}
					},
					"required": ["item"]
				}`),
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					Item      int    `json:"item"`
					StartLine int    `json:"start_line"`
					EndLine   int    `json:"end_line"`
					Part      string `json:"part"`
				}
				if err := json.Unmarshal(raw, &in); err != nil || in.Item <= 0 {
					return "Provide a positive PR or issue number in item.", nil
				}
				c, err := s.st.CaseByNumber(ws.ID, in.Item)
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Sprintf("There is no case file for #%d in %s yet.", in.Item, ws.Name), nil
				}
				if err != nil {
					return "", err
				}
				vals, _ := s.st.CaseValues(c.UUID)
				var b strings.Builder
				fmt.Fprintf(&b, "%s: %s\n", c.Label(), c.Title)
				fmt.Fprintf(&b, "GitHub: %s\n", itemURL(ws, c.Kind, c.Number))
				if !s.facts(ctx, ws).private {
					if link := s.caseLink(c); link != "" {
						fmt.Fprintf(&b, "Case page: %s\n", link)
					}
				}
				fmt.Fprintf(&b, "Updated: %s\n", c.UpdatedAt.In(eastern).Format("Jan 2, 2006 3:04 PM ET"))

				if part := strings.TrimSpace(in.Part); part != "" {
					v, ok := vals[part]
					if !ok {
						return fmt.Sprintf("%s\nNo saved output part %q on this case. Available parts: %s", b.String(), part, sortedCaseKeys(vals)), nil
					}
					fmt.Fprintf(&b, "\n--- %s ---\n", part)
					writePagedLines(&b, v, in.StartLine, in.EndLine, maxCaseToolChars)
					return b.String(), nil
				}

				if len(vals) > 0 {
					fmt.Fprintf(&b, "Saved step outputs (readable via read_case with part=...): %s\n", sortedCaseKeys(vals))
				}
				b.WriteString("\n--- Case file ---\n")
				writePagedLines(&b, c.Contents, in.StartLine, in.EndLine, maxCaseToolChars)
				return b.String(), nil
			},
		},
	}
}

func sortedCaseKeys(vals map[string]string) string {
	keys := make([]string, 0, len(vals))
	for k, v := range vals {
		if strings.TrimSpace(v) != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}

func writePagedLines(b *strings.Builder, text string, startLine, endLine, maxChars int) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	total := len(lines)
	start := max(startLine, 1)
	if start > total {
		fmt.Fprintf(b, "(end of text; %d lines total)\n", total)
		return
	}
	end := total
	if endLine >= start && endLine < end {
		end = endLine
	}
	written := 0
	lastLine := start - 1
	for i := start; i <= end; i++ {
		line := fmt.Sprintf("%d: %s\n", i, lines[i-1])
		if written > 0 && written+len(line) > maxChars {
			break
		}
		b.WriteString(line)
		written += len(line)
		lastLine = i
	}
	if lastLine < total {
		fmt.Fprintf(b, "\n[showing lines %d-%d of %d; call read_case with start_line=%d to read more]\n", start, lastLine, total, lastLine+1)
	}
}
