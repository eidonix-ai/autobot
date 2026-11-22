package web

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func (e *env) apiGet(t *testing.T, b *browser, path, bearer string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := http.DefaultClient
	if b != nil {
		client = b.c
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Who may read a case file over the API: anyone for a public repository; for a private one, a
// signed-in user of the workspace or a request with the repository's secret. Without the secret, a
// real private case explains how to set it up, without naming the repository.
func TestCaseAPI(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	e.user("outsider", false)
	pub, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	priv, _ := e.st.CreateWorkspace("o/secret", admin, "flare-autobot", "tok-good")
	pc, _, _ := e.st.OpenCase(pub, 1, "pr", "Public", "Opened for o/r PR #1")
	sc, _, _ := e.st.OpenCase(priv, 2, "issue", "Private", "Opened for o/secret issue #2")

	if code, body := e.apiGet(t, nil, "/api/v1/cases/"+pc.UUID, ""); code != http.StatusOK || body != "Opened for o/r PR #1\n" {
		t.Fatalf("public case: %d %q", code, body)
	}
	if code, body := e.apiGet(t, nil, "/api/v1/cases/00000000-0000-4000-8000-000000000000", ""); code != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Fatalf("unknown uuid: %d %q", code, body)
	}

	path := "/api/v1/cases/" + sc.UUID
	fakeSecret = ""
	defer func() { fakeSecret = "" }()
	code, body := e.apiGet(t, nil, path, "")
	if code != http.StatusUnauthorized || !strings.Contains(body, "has no secret yet") || !strings.Contains(body, ".github/autobot-secret.txt") ||
		strings.Contains(body, "o/secret") || strings.Contains(body, "Opened for") {
		t.Fatalf("private case, no secret file: %d\n%s", code, body)
	}

	// A secret is set: a missing or wrong one is refused with directions; the right one reads.
	secret := "autobot-0123456789abcdef0123456789abcdef"
	fakeSecret = secret
	e.ui.access.byWS = map[int64]repoFacts{} // as if five minutes had passed
	for _, bad := range []string{"", "wrong-wrong-wrong-wrong-wrong-wrong-wrong"} {
		if code, body := e.apiGet(t, nil, path, bad); code != http.StatusUnauthorized || !strings.Contains(body, "did not send it, or sent a value that does not match") ||
			strings.Contains(body, "Opened for") {
			t.Errorf("bearer %q: %d %q", bad, code, body)
		}
	}
	if code, body := e.apiGet(t, nil, path, secret); code != http.StatusOK || body != "Opened for o/secret issue #2\n" {
		t.Fatalf("right secret: %d %q", code, body)
	}

	// A secret too short to be one is not accepted, even when sent exactly.
	fakeSecret = "short"
	e.ui.access.byWS = map[int64]repoFacts{}
	if code, body := e.apiGet(t, nil, path, "short"); code != http.StatusUnauthorized || !strings.Contains(body, "too short") {
		t.Fatalf("short secret: %d %q", code, body)
	}

	// Signed in: the workspace's users read it; other users do not.
	if code, _ := e.apiGet(t, e.signIn("derek"), path, ""); code != http.StatusOK {
		t.Fatalf("workspace admin: %d", code)
	}
	if code, _ := e.apiGet(t, e.signIn("outsider"), path, ""); code != http.StatusUnauthorized {
		t.Fatalf("a user outside the workspace: %d", code)
	}
}
