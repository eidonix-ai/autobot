package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PushPatch applies a unified diff onto baseCommit of owner/repo, commits it on branch, and pushes
// the branch to the remote repository using token via git's environment (never on CLI args).
// Everything runs in a fresh temporary worktree that is removed when PushPatch returns.
func (m *Mirrors) PushPatch(ctx context.Context, id int64, name, token, baseCommit, branch, patch, commitMsg string) error {
	if strings.TrimSpace(patch) == "" {
		return errors.New("the patch is empty")
	}
	if strings.TrimSpace(branch) == "" {
		return errors.New("branch name is empty")
	}
	if baseCommit != "" && !shaRE.MatchString(baseCommit) {
		return fmt.Errorf("invalid base commit %q", baseCommit)
	}
	tmp, err := os.MkdirTemp("", "autobot-pr-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	work := filepath.Join(tmp, "work")
	remoteURL := m.url(name)
	// Clone shallowly into tmp worktree: from local mirror if ready (fast), otherwise directly from remote.
	if m.Ready(id) {
		if _, _, err := m.git(ctx, fetchTimeout, 1<<16, token, "clone", "--no-hardlinks", "--quiet", m.path(id), work); err != nil {
			return fmt.Errorf("cloning local mirror: %w", err)
		}
		if _, _, err := m.git(ctx, readTimeout, 1<<14, token, "-C", work, "remote", "set-url", "origin", remoteURL); err != nil {
			return err
		}
	} else {
		if _, _, err := m.git(ctx, fetchTimeout, 1<<16, token, "clone", "--depth=1", "--single-branch", "--no-tags", "--quiet", remoteURL, work); err != nil {
			return fmt.Errorf("cloning %s: %w", name, err)
		}
	}

	startRef := "HEAD"
	if baseCommit != "" {
		if _, _, err := m.git(ctx, readTimeout, 1<<10, token, "-C", work, "cat-file", "-e", baseCommit+"^{commit}"); err != nil {
			if _, _, ferr := m.git(ctx, fetchTimeout, 1<<16, token, "-C", work, "fetch", "--depth=1", "--quiet", "origin", baseCommit); ferr != nil {
				return fmt.Errorf("fetching base commit %s: %w", baseCommit[:12], ferr)
			}
		}
		startRef = baseCommit
	}
	if _, _, err := m.git(ctx, readTimeout, 1<<14, token, "-C", work, "checkout", "--quiet", "-B", branch, startRef); err != nil {
		return fmt.Errorf("checking out %s: %w", branch, err)
	}

	patchPath := filepath.Join(tmp, "change.patch")
	if err := os.WriteFile(patchPath, []byte(patch), 0o600); err != nil {
		return err
	}
	if _, _, err := m.git(ctx, readTimeout, 1<<16, token, "-C", work, "apply", "--whitespace=nowarn", patchPath); err != nil {
		return fmt.Errorf("applying patch: %w", err)
	}
	if _, _, err := m.git(ctx, readTimeout, 1<<14, token, "-C", work, "add", "-A"); err != nil {
		return fmt.Errorf("staging changes: %w", err)
	}
	// Ensure something is actually staged.
	if _, _, err := m.git(ctx, readTimeout, 1<<10, token, "-C", work, "diff", "--cached", "--quiet"); err == nil {
		return errors.New("applying the patch produced no file changes")
	}

	env := []string{
		"GIT_AUTHOR_NAME=Autobot",
		"GIT_AUTHOR_EMAIL=autobot@users.noreply.github.com",
		"GIT_COMMITTER_NAME=Autobot",
		"GIT_COMMITTER_EMAIL=autobot@users.noreply.github.com",
	}
	if strings.TrimSpace(commitMsg) == "" {
		commitMsg = "Autobot patch"
	}
	if _, _, err := m.gitEnv(ctx, readTimeout, 1<<14, token, env, "-C", work, "commit", "--quiet", "-m", commitMsg); err != nil {
		return fmt.Errorf("committing changes: %w", err)
	}
	if _, _, err := m.git(ctx, 2*time.Minute, 1<<16, token, "-C", work, "push", "--force", "--quiet", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("pushing branch %s: %w", branch, err)
	}
	return nil
}
