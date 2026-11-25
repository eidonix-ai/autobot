package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// errMachineBusy signals that at least one configured machine is online and enabled but currently
// running another job; workflow steps wait for it instead of failing.
var errMachineBusy = errors.New("all configured machines are busy")

// machinePoll is how often a workflow step waiting for a busy machine re-checks.
var machinePoll = 5 * time.Second

// sandboxSetup is the time a session gets, beyond the agent's own, to download the code and its
// dependencies before the agent starts.
const sandboxSetup = 15 * time.Minute

// openSandbox opens a session for an agent on the first of the machines its settings list that is
// enabled, online and idle, for the repository at commit (or, with none, the default branch's head).
// It returns the session and a note for the agent's system prompt saying where it is. With no
// machines listed it returns nil. When none is free it says so, and why.
func (s *Server) openSandbox(ctx context.Context, ws *store.Workspace, a workflow.AgentSettings, token, commit string, by *store.User, waitBusy bool) (*Session, string, error) {
	if len(a.Machines) == 0 {
		return nil, "", nil
	}
	var chosen *machineView
	var why []string
	anyBusy := false
	for _, id := range a.Machines {
		m, err := s.st.Machine(id)
		if err != nil || m.WorkspaceID != ws.ID || m.Revoked {
			continue
		}
		v, err := s.machineView(ws, *m)
		if err != nil {
			return nil, "", err
		}
		if v.Ready() {
			chosen = &v
			break
		}
		if v.Enabled && v.Online && v.Live.Problem == "" && v.Run != nil {
			anyBusy = true
		}
		why = append(why, m.Name+" ("+strings.ToLower(v.State()[:1])+v.State()[1:]+")")
	}
	if chosen == nil {
		if len(why) == 0 {
			return nil, "", fmt.Errorf("the machines this agent may use no longer exist in %s; choose them again in its settings", ws.Name)
		}
		if waitBusy && anyBusy {
			return nil, "", errMachineBusy
		}
		return nil, "", fmt.Errorf("no machine is free for this agent: %s. Try again when one is ready.", strings.Join(why, ", "))
	}
	if commit == "" {
		var err error
		if commit, err = s.gh.CommitSHA(ctx, token, ws.Name, "HEAD"); err != nil {
			return nil, "", fmt.Errorf("finding the repository's latest commit: %v", err)
		}
	}
	private := false
	if token != "" {
		var err error
		if private, err = s.gh.Private(ctx, token, ws.Name); err != nil {
			return nil, "", fmt.Errorf("checking whether %s is private: %v", ws.Name, err)
		}
	}
	limit := min(time.Duration(a.MaxMinutes)*time.Minute+sandboxSetup, protocol.MaxRun)
	se, err := s.OpenSession(&chosen.Machine, commit, commit, private, len(chosen.Live.GPUs), limit, by)
	if err != nil {
		var ie *store.InputError
		if waitBusy && errors.As(err, &ie) && strings.Contains(ie.Msg, "already running a test") {
			return nil, "", errMachineBusy
		}
		return nil, "", err
	}
	return se, sandboxNote(ws.Name, commit, *chosen), nil
}

// sandboxNote tells the agent where its shell is.
func sandboxNote(repo, commit string, v machineView) string {
	st := v.Live.Status
	var b strings.Builder
	fmt.Fprintf(&b, "You have a shell on the machine %s, a %s machine", v.Name, map[string]string{"amd64": "x86_64", "arm64": "arm64"}[st.Arch])
	if len(st.GPUs) > 0 {
		names := map[string]int{}
		var order []string
		for _, g := range st.GPUs {
			if names[g.Name] == 0 {
				order = append(order, g.Name)
			}
			names[g.Name]++
		}
		var parts []string
		for _, n := range order {
			parts = append(parts, fmt.Sprintf("%d × %s", names[n], n))
		}
		fmt.Fprintf(&b, " with %s (all of them are yours)", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, ". Your commands run in a sandbox container with %d CPUs and %d GiB of memory, of which %d GiB is the scratch folder /work. ",
		st.JobCPUs, st.JobMemoryGiB, st.JobScratchGiB)
	fmt.Fprintf(&b, "%s at commit %s is in /work/source, the folder your commands start in. ", repo, commit[:min(12, len(commit))])
	b.WriteString("The sandbox is an ephemeral container removed when the run ends. " +
		"The dependencies named by Cargo.lock and uv.lock were prefetched before it started, and the container has outbound internet and a writable root so you can install any missing packages or system headers (e.g. apt-get, uv pip install, pip install, cargo) needed to build and test. ")
	if len(st.Models) > 0 {
		var names []string
		for _, m := range st.Models[:min(len(st.Models), 12)] {
			names = append(names, m.Path)
		}
		fmt.Fprintf(&b, "Models are under /models (read-only), such as %s. ", strings.Join(names, ", "))
	}
	b.WriteString("Use bash (background for long builds and tests, then bash_output), read_file, write_file, edit_file, grep and glob; " +
		"read what a command printed before you draw conclusions from it, and cite its exit code in your report.")
	return b.String()
}
