package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// errPrivateRepo is why a private repository's run posts nothing on Discord.
var errPrivateRepo = errors.New("a private repository's cases stay out of Discord")

// toDiscord returns the bot and the case's forum post when the run may post to Discord: the
// workspace has a forum, its repository is public (a private repository's cases never reach
// Discord), and the run is not dry. The case's post is opened, titled after the item, if it has none.
func (s *Server) toDiscord(ctx context.Context, rs *runState) (*discord.Bot, string, string, error) {
	forum, _ := s.st.DiscordForum(rs.ws.ID)
	b, _ := s.bot(rs.ws)
	switch {
	case rs.stopped():
		return nil, "", "", rs.errStopped()
	case rs.run.DryRun:
		return nil, "", "", errors.New("dry run: nothing posted")
	case b == nil || forum == "":
		return nil, "", "", errors.New("the workspace posts to no Discord forum: choose one in its Settings")
	case s.facts(ctx, rs.ws).private:
		return nil, "", "", errPrivateRepo
	}
	cs, err := s.runCase(rs)
	if errors.Is(err, store.ErrNotFound) {
		cs, err = s.caseForRun(rs)
	}
	if err != nil {
		return nil, "", "", err
	}
	thread, summary := s.st.DiscordPost(cs.UUID)
	if thread != "" {
		return b, thread, summary, nil
	}
	what := "PR"
	if rs.it.kind == workflow.Issue {
		what = "Issue"
	}
	title := fmt.Sprintf("%s #%d: %s", what, rs.it.number, rs.it.title)
	p, err := b.CreatePost(ctx, forum, title, discord.Message{Text: fmt.Sprintf("**%s**\n%s\n\nThe summary of findings appears here.", title, rs.it.url)})
	if err != nil {
		return nil, "", "", fmt.Errorf("opening the case's post on Discord: %v", err)
	}
	if err := s.st.SetDiscordPost(cs.UUID, p.Channel, p.ID); err != nil {
		return nil, "", "", err
	}
	return b, p.Channel, p.ID, nil
}

// discordStep posts in the case's forum post, or, with summary, replaces the post's first message.
func (s *Server) discordStep(ctx context.Context, rs *runState, summary bool, text string) stepOutcome {
	b, thread, first, err := s.toDiscord(ctx, rs)
	if err != nil {
		return stepOutcome{err: err}
	}
	if summary {
		what := "PR"
		if rs.it.kind == workflow.Issue {
			what = "Issue"
		}
		head := fmt.Sprintf("**%s #%d: %s**\n%s\n\n", what, rs.it.number, rs.it.title, rs.it.url)
		if err := b.Edit(ctx, thread, first, discord.Message{Text: head + text}); err != nil {
			return stepOutcome{err: fmt.Errorf("updating the summary on Discord: %v", err)}
		}
		return stepOutcome{port: "out", output: text, note: "summary updated on Discord"}
	}
	if _, err := b.Send(ctx, thread, discord.Message{Text: text}); err != nil {
		return stepOutcome{err: fmt.Errorf("posting on Discord: %v", err)}
	}
	return stepOutcome{port: "out", output: text, note: "posted on Discord"}
}

// discordReplyStep replies directly to the Discord message that triggered a Discord workflow run.
// Respects dry runs, trial mode, private repositories, and suppresses embeds and mass pings.
func (s *Server) discordReplyStep(ctx context.Context, rs *runState, trial bool, text string) stepOutcome {
	if rs.stopped() {
		return stepOutcome{err: rs.errStopped()}
	}
	if rs.run.DryRun {
		return stepOutcome{port: "out", output: text, note: "dry run: not posted to Discord"}
	}
	if trial {
		return stepOutcome{port: "out", output: text, note: "trial run: not posted to Discord"}
	}
	if rs.discord == nil || rs.discord.ChannelID == "" {
		return stepOutcome{port: "out", output: text, note: "manual test: no Discord channel to reply in"}
	}
	if s.facts(ctx, rs.ws).private {
		return stepOutcome{port: "out", output: text, note: "skipped: " + errPrivateRepo.Error()}
	}
	b, _ := s.bot(rs.ws)
	if b == nil {
		return stepOutcome{err: errors.New("this workspace has no Discord bot: configure one in workspace Settings")}
	}
	var err error
	if rs.discord.MessageID != "" {
		_, err = b.Reply(ctx, rs.discord.ChannelID, rs.discord.MessageID, discord.Message{Text: text})
	} else {
		_, err = b.Send(ctx, rs.discord.ChannelID, discord.Message{Text: text})
	}
	if err != nil {
		return stepOutcome{err: fmt.Errorf("replying on Discord: %v", err)}
	}
	return stepOutcome{port: "out", output: text, note: "replied on Discord"}
}

// approvalStep asks for a person's answer: on Discord when it may, and always on the run's page,
// then waits for it.
func (s *Server) approvalStep(ctx context.Context, rs *runState, n workflow.Node, a workflow.ApprovalSettings, request, question string) stepOutcome {
	if rs.stopped() {
		return stepOutcome{err: rs.errStopped()}
	}
	ap, err := s.st.OpenApproval(rs.run.ID, n.ID, request, question, time.Now().Add(time.Duration(a.Hours)*time.Hour))
	if err != nil {
		return stepOutcome{err: err}
	}
	if err := s.st.SetWorkflowStep(rs.run.ID, store.WorkflowStep{NodeID: n.ID, Status: "waiting", StartedAt: time.Now()}); err != nil {
		return stepOutcome{err: err}
	}
	note := ""
	if b, thread, _, err := s.toDiscord(ctx, rs); err == nil && ap.Message == "" {
		m, err := b.Send(ctx, thread, approvalMessage(n, ap))
		if err != nil {
			note = "not asked on Discord: " + err.Error()
		} else {
			s.st.SetApprovalMessage(ap.ID, m.Channel, m.ID)
		}
	} else if err != nil {
		note = "asked on the run's page only (" + err.Error() + ")"
	}
	o := s.awaitApproval(ctx, rs, n)
	if o.note == "" {
		o.note = note
	}
	return o
}

// approvalMessage is a request as Discord shows it: pending with buttons, or answered without.
func approvalMessage(n workflow.Node, ap *store.Approval) discord.Message {
	head := "**Approval needed** · " + workflow.DisplayName(n)
	var buttons []discord.Button
	switch ap.Status {
	case "pending":
		buttons = []discord.Button{{ID: fmt.Sprintf("ap:%d:approved", ap.ID), Label: "Approve", Style: 3},
			{ID: fmt.Sprintf("ap:%d:denied", ap.ID), Label: "Deny", Style: 4}}
		by := "_Answer by " + ap.ExpiresAt.In(eastern).Format("Jan 2, 3:04 PM") + " ET._"
		if ap.Question == "" { // asked before the question existed
			by = "\n" + by
		}
		return discord.Message{Text: head + approvalBody(ap) + "\n" + by, Buttons: buttons}
	case "timed_out":
		head = "**No answer in time** · " + workflow.DisplayName(n)
	case "stopped":
		head = fmt.Sprintf("**The run was stopped** by %s · %s ET · %s", ap.DecidedBy, ap.DecidedAt.In(eastern).Format("Jan 2, 3:04 PM"), workflow.DisplayName(n))
	default:
		head = fmt.Sprintf("**%s** by %s on %s · %s", map[string]string{"approved": "Approved", "denied": "Denied"}[ap.Status],
			ap.DecidedBy, ap.DecidedOn, ap.DecidedAt.In(eastern).Format("Jan 2, 3:04 PM")+" ET")
	}
	return discord.Message{Text: head + approvalBody(ap)}
}

// approvalBody is what a card says below its head: the details, then the question in bold. A request
// made before the question existed shows its details only.
func approvalBody(ap *store.Approval) string {
	if ap.Question == "" {
		return "\n\n" + ap.Request
	}
	body := ""
	if ap.Request != "" {
		body = "\n\n" + ap.Request
	}
	return body + "\n\n**" + ap.Question + "**"
}

var eastern = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return l
}()

// awaitApproval waits for a step's request to be answered or to run out of time, then records the
// answer in the case file and on Discord.
func (s *Server) awaitApproval(ctx context.Context, rs *runState, n workflow.Node) stepOutcome {
	for {
		if rs.stopped() { // a stopped run's approval is closed by the stop, and records nothing
			return stepOutcome{err: rs.errStopped()}
		}
		ap, err := s.st.ApprovalFor(rs.run.ID, n.ID)
		if err != nil {
			return stepOutcome{err: err}
		}
		if ap.Status == "pending" && !time.Now().Before(ap.ExpiresAt) {
			s.st.Decide(ap.ID, "timed_out", "", "")
			continue
		}
		if ap.Status != "pending" {
			what := map[string]string{"approved": "Approved", "denied": "Denied", "timed_out": "No answer in time"}[ap.Status]
			out := what
			if ap.DecidedBy != "" {
				out = fmt.Sprintf("%s by %s on %s, %s ET", what, ap.DecidedBy, ap.DecidedOn, ap.DecidedAt.In(eastern).Format("Jan 2, 2006 3:04 PM"))
			}
			entry := "**" + workflow.DisplayName(n) + ":** "
			if ap.Question != "" {
				entry += ap.Question + " → "
			}
			entry += out + "."
			if ap.Request != "" {
				entry += "\n\n> " + strings.ReplaceAll(ap.Request, "\n", "\n> ")
			}
			if err := s.writeEntry(rs, n, entry, true); err != nil {
				log.Printf("workflow run %d: %v", rs.run.ID, err)
			}
			if ap.Message != "" && ap.DecidedOn != "Discord" { // a click on Discord updates its own message
				if b, _ := s.bot(rs.ws); b != nil {
					b.Edit(ctx, ap.Channel, ap.Message, approvalMessage(n, ap))
				}
			}
			return stepOutcome{port: ap.Status, output: out}
		}
		select {
		case <-ctx.Done():
			if rs.stopped() {
				return stepOutcome{err: rs.errStopped()}
			}
			return stepOutcome{err: errors.New("stopped waiting for the answer")}
		case <-time.After(2 * time.Second):
		}
	}
}

// ResumeWorkflows picks up runs that were waiting for a person when Autobot stopped.
func (s *Server) ResumeWorkflows(ctx context.Context) {
	ids, err := s.st.WaitingRuns()
	if err != nil {
		log.Printf("resuming workflow runs: %v", err)
		return
	}
	for _, id := range ids {
		if err := s.resume(ctx, id); err != nil {
			log.Printf("resuming workflow run %d: %v", id, err)
			s.st.FinishWorkflowRun(id, "failed")
		}
	}
}

func (s *Server) resume(ctx context.Context, id int64) error {
	rs, err := s.runState(ctx, id)
	if err != nil {
		return err
	}
	s.launch(rs)
	return nil
}

// runState builds the state to drive a recorded run from: its workflow as it was, and its item and
// the case's tags as they are now. A run resumed after a restart and a queued run that is starting
// are both driven from it.
func (s *Server) runState(ctx context.Context, id int64) (*runState, error) {
	run, err := s.st.WorkflowRun(id)
	if err != nil {
		return nil, err
	}
	ws, err := s.st.WorkspaceByID(run.WorkspaceID)
	if err != nil {
		return nil, err
	}
	var g workflow.Graph
	if err := json.Unmarshal([]byte(run.Graph), &g); err != nil {
		return nil, err
	}
	if workflow.IsLFA(run.Kind) {
		trig := parseLFATrigger(run.TriggerData)
		it := &item{kind: workflow.LFA, number: int(run.ID), author: trig.Author, title: trig.Title, body: trig.Prompt}
		rs := &runState{ws: ws, run: run, g: g, it: it, lfa: &trig, builtIn: trig.values(ws), tags: map[string]string{}}
		if run.StartedByID != 0 {
			rs.by, _ = s.st.User(run.StartedByID)
		}
		if !run.DryRun {
			if c, err := s.st.CaseByLFARun(ws.ID, run.ID); err == nil {
				if vals, err := s.st.CaseValues(c.UUID); err == nil {
					rs.tags = vals
				}
			}
		}
		if c, err := s.st.Charter(ws.ID, 0); err == nil {
			rs.builtIn["charter"] = c.Body
		}
		rs.builtIn["machines"] = s.machinesSummary(ws)
		return rs, nil
	}
	if run.Kind == workflow.Discord {
		trig := parseDiscordTrigger(run.TriggerData)
		it := &item{kind: workflow.Discord, author: trig.Author, title: trig.Channel, body: trig.Content, url: trig.URL}
		rs := &runState{ws: ws, run: run, g: g, it: it, discord: &trig, builtIn: trig.values(ws), tags: map[string]string{}}
		if run.StartedByID != 0 {
			rs.by, _ = s.st.User(run.StartedByID)
		}
		if c, err := s.st.Charter(ws.ID, 0); err == nil {
			rs.builtIn["charter"] = c.Body
		}
		rs.builtIn["machines"] = s.machinesSummary(ws)
		return rs, nil
	}
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	it, err := s.loadItem(lctx, ws, run.Kind, run.Item)
	if err != nil {
		return nil, err
	}
	rs := &runState{ws: ws, run: run, g: g, it: it, builtIn: it.values(ws), tags: map[string]string{}}
	if run.StartedByID != 0 {
		rs.by, _ = s.st.User(run.StartedByID)
	}
	rs.builtIn["repo:opt_out"] = s.optOut(ws)
	if c, err := s.st.CaseByNumber(ws.ID, run.Item); err == nil && !run.DryRun { // a dry run reads nothing of the real case
		if rs.tags, err = s.st.CaseValues(c.UUID); err != nil {
			return nil, err
		}
	}
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		rs.builtIn["charter"] = c.Body
	}
	rs.builtIn["machines"] = s.machinesSummary(ws)
	return rs, nil
}

// approvalClick takes a click on an approval's button. Anyone who can see the forum may answer
// (Derek, 2026-11-23: its members are the people trusted to make these calls); a bot may not.
func (s *Server) approvalClick(w http.ResponseWriter, ws *store.Workspace, body []byte) {
	var in struct {
		Data struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		Member *struct {
			User discordUser `json:"user"`
		} `json:"member"`
		User *discordUser `json:"user"`
	}
	json.Unmarshal(body, &in)
	var u discordUser
	switch {
	case in.Member != nil:
		u = in.Member.User
	case in.User != nil:
		u = *in.User
	}
	parts := strings.Split(in.Data.CustomID, ":")
	if len(parts) != 3 || parts[0] != "ap" || u.ID == "" {
		w.Write(discord.Private("This button is not Autobot's any more."))
		return
	}
	if u.Bot {
		w.Write(discord.Private("A bot cannot answer."))
		return
	}
	id, _ := strconv.ParseInt(parts[1], 10, 64)
	ap, err := s.st.Approval(id)
	if err != nil {
		w.Write(discord.Private("This request is not Autobot's any more."))
		return
	}
	// A workspace's bot answers only that workspace's requests.
	if run, err := s.st.WorkflowRun(ap.RunID); err != nil || run.WorkspaceID != ws.ID {
		w.Write(discord.Private("This request is not Autobot's any more."))
		return
	}
	name := u.GlobalName
	if name == "" {
		name = u.Username
	}
	taken, err := s.st.Decide(id, parts[2], name, "Discord")
	if err != nil {
		w.Write(discord.Private("That is not an answer."))
		return
	}
	if ap, err = s.st.Approval(id); err != nil {
		w.Write(discord.Private("This request is not Autobot's any more."))
		return
	}
	if !taken {
		w.Write(discord.Private("Already answered: " + approvalSummary(ap) + "."))
		return
	}
	run, _ := s.st.WorkflowRun(ap.RunID)
	node := workflow.Node{ID: ap.NodeID, Type: workflow.Approval}
	if run != nil {
		var g workflow.Graph
		json.Unmarshal([]byte(run.Graph), &g)
		for _, n := range g.Nodes {
			if n.ID == ap.NodeID {
				node = n
			}
		}
	}
	w.Write(discord.Update(approvalMessage(node, ap)))
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

func approvalSummary(ap *store.Approval) string {
	switch ap.Status {
	case "approved", "denied":
		return fmt.Sprintf("%s by %s on %s", ap.Status, ap.DecidedBy, ap.DecidedOn)
	case "timed_out":
		return "no answer came in time"
	case "stopped":
		return "the run was stopped by " + ap.DecidedBy
	}
	return "still waiting"
}

// decideOnPage answers a request from the run's page, as a member of the workspace.
func (s *Server) decideOnPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	runID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	apID, _ := strconv.ParseInt(r.PathValue("approval"), 10, 64)
	run, err := s.st.WorkflowRun(runID)
	ap, aerr := s.st.Approval(apID)
	if err != nil || aerr != nil || run.WorkspaceID != ws.ID || ap.RunID != run.ID {
		http.NotFound(w, r)
		return
	}
	answer := r.PostForm.Get("answer")
	if answer != "approved" && answer != "denied" {
		http.Error(w, "Approve or deny.", http.StatusBadRequest)
		return
	}
	if _, err := s.st.Decide(apID, answer, me(r).Name(), "Autobot"); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), run.ID), http.StatusSeeOther)
}
