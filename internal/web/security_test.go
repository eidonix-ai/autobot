package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/netguard"
)

// Ten failed sign-ins lock the account, from wherever they come, even against the right password;
// other accounts, and names that do not exist, are counted the same way.
func TestSignInLockout(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	e.user("alice", false)
	b := e.browser()
	try := func(name, pw string) int {
		code, _, _ := b.post("/login", url.Values{"username": {name}, "password": {pw}})
		return code
	}
	for i := 0; i < maxFails; i++ {
		if code := try("derek", "wrong-password-x"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, code)
		}
	}
	if code := try("Derek", "password-1234"); code != http.StatusTooManyRequests {
		t.Fatalf("locked account signed in: %d", code)
	}
	if code := try("alice", "password-1234"); code != http.StatusSeeOther {
		t.Fatalf("another account was locked too: %d", code)
	}
	for i := 0; i < maxFails; i++ {
		try("nobody", "x")
	}
	if code := try("nobody", "x"); code != http.StatusTooManyRequests {
		t.Fatalf("an unknown name is not counted like a real one: %d", code)
	}
}

// After sign-in Autobot goes only to a path on itself.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/w/o/r": "/w/o/r", "/w/o/r?x=1#y": "/w/o/r?x=1#y",
		"//evil.example": "/", "/\\evil.example": "/", "/\t/evil.example": "/", "/%09/evil.example": "/%09/evil.example",
		"/\n/evil.example": "/", "https://evil.example": "/", "evil": "/", " /x": "/", "/ /evil.example": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// The setup page is closed for good once someone exists: a post does nothing.
func TestSetupClosed(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	code, loc, _ := e.browser().post("/setup", url.Values{"username": {"mallory"}, "password": {"long-enough-pass"}, "confirm": {"long-enough-pass"}})
	if code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("%d %q", code, loc)
	}
	if u, _ := e.st.UserByUsername("mallory"); u != nil {
		t.Fatal("setup made an account after setup")
	}
}

// A signed-in page is never cached, and no page may load a remote image.
func TestPageHeaders(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	resp, err := e.signIn("derek").c.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "img-src 'self' data:;") {
		t.Fatalf("%v", resp.Header)
	}
}

// A provider's address must be public unless an admin trusts its host: no user can point Autobot
// at its own network. Only an admin can change the list.
func TestProviderAddressesGuarded(t *testing.T) {
	defer netguard.SetTrusted([]string{"127.0.0.1"})
	netguard.SetTrusted(nil)
	e := newEnv(t)
	e.user("derek", true)
	e.user("alice", false)
	models := modelServer(t)
	alice := e.signIn("alice")
	code, loc, _ := alice.post("/providers", url.Values{"csrf": {alice.csrf()}, "preset": {"custom"}, "name": {"Inside"}, "endpoint": {models.URL}})
	if code != http.StatusSeeOther {
		t.Fatalf("create: %d", code)
	}
	p := strings.SplitN(loc, "?", 2)[0]
	if code, _, body := alice.post(p+"/discover", url.Values{"csrf": {alice.csrf()}}); code == http.StatusOK || !strings.Contains(body, "not a public address") {
		t.Fatalf("reached the local network: %d", code)
	}
	if code, _, _ := alice.post("/providers/trusted", url.Values{"csrf": {alice.csrf()}, "hosts": {"127.0.0.1"}}); code != http.StatusNotFound {
		t.Fatalf("a user changed the trusted hosts: %d", code)
	}
	admin := e.signIn("derek")
	if code, _, _ := admin.post("/providers/trusted", url.Values{"csrf": {admin.csrf()}, "hosts": {"http://127.0.0.1/x"}}); code != http.StatusBadRequest {
		t.Fatalf("a URL was taken as a host: %d", code)
	}
	if code, _, _ := admin.post("/providers/trusted", url.Values{"csrf": {admin.csrf()}, "hosts": {"127.0.0.1"}}); code != http.StatusSeeOther {
		t.Fatalf("trusting: %d", code)
	}
	if code, _, body := alice.post(p+"/discover", url.Values{"csrf": {alice.csrf()}}); code != http.StatusOK || !strings.Contains(body, "Vendor: Small") {
		t.Fatalf("a trusted host was refused: %d", code)
	}
}
