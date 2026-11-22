package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustUser(t *testing.T, st *Store, name string, admin bool) *User {
	t.Helper()
	u, err := st.CreateUser(name, "", "password123", admin)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func isInput(err error) bool {
	var ie *InputError
	return errors.As(err, &ie)
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("derek", "Derek", "password123", true); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UserByUsername("DEREK"); err != nil {
		t.Fatalf("user lost or case-sensitive after reopen: %v", err)
	}
}

func TestWorkspaceNames(t *testing.T) {
	good := map[string]string{
		"NVIDIA/hyperflare-inference":                      "NVIDIA/hyperflare-inference",
		" https://github.com/NVIDIA/hyperflare-inference/": "NVIDIA/hyperflare-inference",
		"github.com/a/b.git":                               "a/b",
		"eidonix-ai/autobot":                               "eidonix-ai/autobot",
		"x/.github":                                        "x/.github",
	}
	for in, want := range good {
		got, err := NormalizeWorkspaceName(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "nvidia", "-bad/repo", "bad-/repo", "a/b/c", "a/..", "a/", "a b/c", "a/<script>"} {
		if _, err := NormalizeWorkspaceName(in); !isInput(err) {
			t.Errorf("%q: accepted, want an input error", in)
		}
	}
}

func TestWorkspaceCreateAndAccess(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	alice := mustUser(t, st, "alice", false)

	ws, err := st.CreateWorkspace("NVIDIA/hyperflare-inference", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWorkspace("nvidia/HYPERFLARE-inference", admin); !isInput(err) {
		t.Fatalf("duplicate differing only in case: %v, want input error", err)
	}
	if got, err := st.Workspace("nvidia/hyperflare-inference"); err != nil || got.ID != ws.ID {
		t.Fatalf("case-insensitive lookup: %v %v", got, err)
	}
	c, err := st.Charter(ws.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"## Vision", "## Philosophy", "## Goals & Non-Goals", "## Applicable Standards", "## Competitors", "## Roadmap"} {
		if !strings.Contains(c.Body, h) {
			t.Errorf("charter template lacks %q", h)
		}
	}

	if ok, _ := st.CanOpen(alice, ws.ID); ok {
		t.Fatal("unassigned regular user can open the workspace")
	}
	if list, _ := st.Workspaces(alice); len(list) != 0 {
		t.Fatalf("unassigned user lists %v", list)
	}
	if err := st.UpdateUser(alice.ID, UserUpdate{Workspaces: []int64{ws.ID}}, admin); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.CanOpen(alice, ws.ID); !ok {
		t.Fatal("assigned user cannot open the workspace")
	}
	if err := st.UpdateUser(alice.ID, UserUpdate{}, admin); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.CanOpen(alice, ws.ID); ok {
		t.Fatal("unassigned user can still open the workspace")
	}
	events, _ := st.Events(ws.ID, 10)
	var got []string
	for _, e := range events {
		got = append(got, e.Actor+" "+e.Summary)
	}
	want := []string{"admin unassigned alice", "admin assigned alice", "admin created the workspace"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("history %q, want %q", got, want)
	}
}

func TestCharterRevisionsAndConflict(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin)

	rev, err := st.SaveCharter(ws.ID, 1, "# One\r\nline\r\n", admin)
	if err != nil || rev != 2 {
		t.Fatalf("save: rev %d, %v", rev, err)
	}
	if c, _ := st.Charter(ws.ID, 0); c.Body != "# One\nline\n" {
		t.Fatalf("line endings not normalized: %q", c.Body)
	}
	// Saving the same text again stores nothing.
	if rev, err := st.SaveCharter(ws.ID, 2, "# One\nline\n", admin); err != nil || rev != 2 {
		t.Fatalf("unchanged save: rev %d, %v", rev, err)
	}
	// A save from an editor that opened revision 1 must not overwrite revision 2.
	_, err = st.SaveCharter(ws.ID, 1, "stale", admin)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Current.Rev != 2 {
		t.Fatalf("stale save: %v, want conflict at revision 2", err)
	}
	if c, _ := st.Charter(ws.ID, 0); c.Rev != 2 {
		t.Fatalf("stale save stored revision %d", c.Rev)
	}
	if old, err := st.Charter(ws.ID, 1); err != nil || !strings.Contains(old.Body, "## Vision") {
		t.Fatalf("revision 1 not kept: %v", err)
	}
	if h, _ := st.CharterHistory(ws.ID); len(h) != 2 || h[0].Rev != 2 || h[0].Author != "admin" {
		t.Fatalf("history %+v", h)
	}
}

func TestLastAdminGuard(t *testing.T) {
	st := open(t)
	a := mustUser(t, st, "a", true)
	if err := st.UpdateUser(a.ID, UserUpdate{IsAdmin: false}, a); !isInput(err) {
		t.Fatalf("demoting the only admin: %v, want input error", err)
	}
	if err := st.UpdateUser(a.ID, UserUpdate{IsAdmin: true, Disabled: true}, a); !isInput(err) {
		t.Fatalf("disabling the only admin: %v, want input error", err)
	}
	b := mustUser(t, st, "b", true)
	if err := st.UpdateUser(a.ID, UserUpdate{IsAdmin: false}, b); err != nil {
		t.Fatalf("demoting one of two admins: %v", err)
	}
}

func TestSessions(t *testing.T) {
	st := open(t)
	u := mustUser(t, st, "u", false)
	if _, err := st.CheckPassword("u", "wrong-password"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := st.CheckPassword("nobody", "password123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	tok, _ := st.CreateSession(u.ID)
	keep, _ := st.CreateSession(u.ID)
	if got, csrf, err := st.Session(tok); err != nil || got.ID != u.ID || csrf == "" {
		t.Fatalf("session: %v %q %v", got, csrf, err)
	}

	// Changing your own password keeps the session you changed it from and ends the others.
	if err := st.SetPassword(u.ID, "another-password", keep); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Session(tok); !errors.Is(err, ErrNotFound) {
		t.Fatal("other session survived a password change")
	}
	if _, _, err := st.Session(keep); err != nil {
		t.Fatal("current session ended by its own password change")
	}

	// Disabling a user ends their sessions and stops sign-in.
	admin := mustUser(t, st, "admin", true)
	if err := st.UpdateUser(u.ID, UserUpdate{Disabled: true}, admin); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Session(keep); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled user's session still works")
	}
	if _, err := st.CheckPassword("u", "another-password"); !errors.Is(err, ErrNotFound) {
		t.Fatal("disabled user can sign in")
	}

	// Expired sessions are refused.
	tok2, _ := st.CreateSession(admin.ID)
	real := now
	t.Cleanup(func() { now = real })
	now = func() int64 { return real() + int64(SessionTTL.Seconds()) + 1 }
	if _, _, err := st.Session(tok2); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session accepted")
	}
}

func TestUsernames(t *testing.T) {
	st := open(t)
	if u, err := st.CreateUser("  Derek ", "Derek Yates", "password123", true); err != nil || u.Username != "derek" {
		t.Fatalf("got %v, %v", u, err)
	}
	if _, err := st.CreateUser("DEREK", "", "password123", false); !isInput(err) {
		t.Fatalf("duplicate username: %v", err)
	}
	for _, bad := range []string{"", "a b", "-x", strings.Repeat("a", 33)} {
		if _, err := st.CreateUser(bad, "", "password123", false); !isInput(err) {
			t.Errorf("username %q accepted", bad)
		}
	}
	if _, err := st.CreateUser("short", "", "1234567", false); !isInput(err) {
		t.Error("7-character password accepted")
	}
}
