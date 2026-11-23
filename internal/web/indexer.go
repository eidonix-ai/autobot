package web

import (
	"context"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
)

// Index keeps each workspace's local copy of its repository's pull requests and issues up to date
// until ctx ends (see package index). It does not depend on any workflow being live. A workspace
// without a GitHub identity is left alone: anonymous reads are too few to copy a repository with.
//
// The poller (poller.go) already asks GitHub every minute for changed items and comments of the
// workspaces that have a live workflow. The two could share one read later: the index's sync is a
// superset of that scan (its changed items, comments and review comments, with their times), so the
// poller could take its changes from the index's tables instead of asking GitHub again.
func (s *Server) Index(ctx context.Context) {
	index.New(s.st, s.gh).Loop(ctx, func() ([]index.Target, error) {
		all, err := s.st.AllWorkspaces()
		if err != nil {
			return nil, err
		}
		var out []index.Target
		for _, ws := range all {
			if token, err := s.token(&ws); err == nil && token != "" {
				out = append(out, index.Target{WS: ws, Token: token})
			}
		}
		return out, nil
	})
}

// itemTools are the tools that search and read a workspace's pull requests and issues: none until
// its copy has been read in full, and then only that workspace's own.
func (s *Server) itemTools(ws *store.Workspace) []agent.Tool {
	if !index.Ready(s.st, ws.ID) {
		return nil
	}
	return agent.ItemTools(itemSource{s.st.ItemIndex(ws.ID), func(ctx context.Context, number int) (*store.ItemDetail, error) {
		token, err := s.token(ws)
		if err != nil {
			return nil, err
		}
		sy := index.New(s.st, s.gh)
		sy.Pace = 0 // an agent is waiting
		return sy.Fetch(ctx, *ws, token, number)
	}})
}

// itemSource is a workspace's local copy, which can also ask GitHub for one item it does not keep.
type itemSource struct {
	*store.ItemIndex
	fetch func(ctx context.Context, number int) (*store.ItemDetail, error)
}

func (x itemSource) Fetch(ctx context.Context, number int) (*store.ItemDetail, error) {
	return x.fetch(ctx, number)
}
