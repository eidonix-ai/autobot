package web

import (
	"context"

	"github.com/eidonix-ai/autobot/internal/github"
)

// The reads behind the local copy of pull requests and issues: this fake has none. Tests of the
// copy itself (package index) bring their own.
func (fakeGitHub) ListItems(context.Context, string, string, github.ItemQuery) ([]github.ItemRecord, error) {
	return nil, nil
}

func (fakeGitHub) GetItem(context.Context, string, string, int) (*github.ItemRecord, error) {
	return nil, github.ErrNoRepo
}

func (fakeGitHub) PullComments(context.Context, string, string, int, int) ([]github.CommentRecord, error) {
	return nil, nil
}

func (fakeGitHub) PullDetail(context.Context, string, string, int) (*github.PullRecord, error) {
	return &github.PullRecord{State: "open"}, nil
}

func (fakeGitHub) ReviewRecords(context.Context, string, string, int) ([]github.CommentRecord, error) {
	return nil, nil
}

func (fakeGitHub) ListComments(context.Context, string, string, github.CommentQuery) ([]github.CommentRecord, error) {
	return nil, nil
}

func (fakeGitHub) ItemComments(context.Context, string, string, int, int) ([]github.CommentRecord, error) {
	return nil, nil
}

func (fakeGitHub) OpenCount(context.Context, string, string) (int, error) { return 0, nil }
