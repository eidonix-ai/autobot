package web

import (
	"context"
	"log"
	"path/filepath"
	"slices"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/repo"
	"github.com/eidonix-ai/autobot/internal/store"
)

// Autobot keeps a local copy of each workspace's repository (package repo): the default branch and
// the open pull requests' heads. It gives agents read-only code tools over it. Without UseRepos
// nothing is cloned and agents read through GitHub.

// UseRepos turns the local copies on, kept in a folder beside the database file (dbFile).
func (s *Server) UseRepos(dbFile string) {
	s.repos = repo.New(filepath.Dir(dbFile))
}

// SyncRepos keeps the copies current until ctx ends: every minute it clones or fetches the
// repository of each workspace whose turn it is (every ten minutes, and after a failure only when
// its wait is over), and removes the copies of workspaces that are gone.
func (s *Server) SyncRepos(ctx context.Context) {
	if s.repos == nil {
		return
	}
	s.repoRound(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.repoRound(ctx)
		}
	}
}

func (s *Server) repoRound(ctx context.Context) {
	list, err := s.st.Workspaces(&store.User{IsAdmin: true})
	if err != nil {
		log.Printf("local copies: %v", err)
		return
	}
	var ids []int64
	for _, ws := range list {
		ids = append(ids, ws.ID)
	}
	s.repos.Prune(ids)
	for _, ws := range list {
		if !s.repos.Due(ws.ID, time.Now()) {
			continue
		}
		token, err := s.token(&ws)
		if err != nil {
			log.Printf("local copy of %s: %v", ws.Name, err)
			continue
		}
		// Each repository on its own, so one large clone does not hold up the others.
		go func() {
			defer survive("local copy of "+ws.Name, nil)
			if err := s.repos.Sync(ctx, ws.ID, ws.Name, token, s.openPulls(ctx, &ws, token)); err != nil && ctx.Err() == nil {
				log.Printf("local copy of %s: %v", ws.Name, err)
			}
		}()
	}
}

// openPulls lists the workspace's open pull requests, the ones whose heads the local copy keeps.
// GitHub is asked with an ETag, so a list that has not changed costs nothing. When it cannot be
// asked, the copy is told it does not know.
func (s *Server) openPulls(ctx context.Context, ws *store.Workspace, token string) repo.Pulls {
	var open []int
	for page := 1; page <= repo.MaxOpenPulls/github.PageSize; page++ {
		list, err := s.gh.ListPulls(ctx, token, ws.Name, "open", page)
		if err != nil {
			log.Printf("local copy of %s: listing open pull requests: %v", ws.Name, err)
			return repo.Pulls{}
		}
		for _, l := range list {
			open = append(open, l.Number)
		}
		if len(list) < github.PageSize {
			break
		}
	}
	return repo.Pulls{Open: open, Known: true}
}

// repoSettings adds what the settings page shows about the workspace's local copy.
func (s *Server) repoSettings(ws *store.Workspace, data map[string]any) {
	if s.repos != nil {
		data["Repo"] = s.repos.Status(ws.ID)
	}
}

// githubCodeTools are the GitHub-backed tools the local copy replaces when it can serve them.
var githubCodeTools = []string{"list_files", "read_file", "pr_diff"}

// withLocalCode gives an agent the code tools over the workspace's local copy, in place of the
// GitHub-backed list_files, read_file and pr_diff. For a pull request the copy is first made to
// hold its head. Where the copy is not ready, or cannot serve this run, the tools are unchanged
// and the agent reads through GitHub as before. A sandbox has its own read_file, which stays.
func (s *Server) withLocalCode(ctx context.Context, ws *store.Workspace, token string, pull *agent.PR, sandboxed bool, tools []agent.Tool) []agent.Tool {
	if s.repos == nil || !s.repos.Ready(ws.ID) {
		return tools
	}
	var local *agent.LocalPR
	if pull != nil {
		ectx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := s.repos.Ensure(ectx, ws.ID, ws.Name, token, pull.Info.HeadSHA, pull.Info.Number); err != nil {
			log.Printf("local copy of %s: reading from GitHub instead: %v", ws.Name, err)
			return tools
		}
		local = &agent.LocalPR{Number: pull.Info.Number, Head: pull.Info.HeadSHA, Base: pull.Info.Base}
		for _, f := range pull.Files {
			local.Files = append(local.Files, f.Path)
		}
	}
	replaced := githubCodeTools
	if sandboxed { // its shell reads the files; only the pull request's diff comes from GitHub
		replaced = []string{"pr_diff"}
	}
	tools = slices.DeleteFunc(tools, func(t agent.Tool) bool { return slices.Contains(replaced, t.Spec.Name) })
	for _, t := range agent.CodeTools(s.repos.Repo(ws.ID, token), local) {
		if !slices.ContainsFunc(tools, func(have agent.Tool) bool { return have.Spec.Name == t.Spec.Name }) {
			tools = append(tools, t)
		}
	}
	return tools
}
