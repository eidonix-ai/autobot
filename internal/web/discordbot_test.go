package web

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
)

// fakeDiscord answers as Discord does for a bot in one server with one forum.
// discordSent records what the bot sent to the fake Discord.
var discordSent []string

func readBody(r *http.Request) string { b, _ := io.ReadAll(r.Body); return string(b) }

func fakeDiscord(t *testing.T) {
	discordSent = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"401: Unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/users/@me":
			io.WriteString(w, `{"id":"1","username":"flare-autobot"}`)
		case "/users/@me/guilds":
			io.WriteString(w, `[{"id":"g1","name":"Local Inference Lab"}]`)
		case "/guilds/g1/channels":
			io.WriteString(w, `[{"id":"f1","name":"flare-autobot","type":15},{"id":"t1","name":"general","type":0}]`)
		case "/channels/f1/threads":
			discordSent = append(discordSent, "post: "+readBody(r))
			io.WriteString(w, `{"id":"th1","message":{"id":"m1"}}`)
		case "/channels/th1/messages":
			discordSent = append(discordSent, "send: "+readBody(r))
			io.WriteString(w, `{"id":"m2","channel_id":"th1"}`)
		default:
			if r.Method == http.MethodPatch {
				discordSent = append(discordSent, "edit "+r.URL.Path+": "+readBody(r))
				io.WriteString(w, `{}`)
				return
			}
			http.NotFound(w, r)
		}
	}))
	old := discord.API
	discord.API = srv.URL
	t.Cleanup(func() { discord.API = old; srv.Close() })
}

// Any member sets up the workspace's bot (no one else); its token is sealed and never shown; the
// forum must be one the bot sees; and Discord's clicks are taken only when signed with this
// workspace's key, at this workspace's address.
func TestDiscordBotSetup(t *testing.T) {
	fakeDiscord(t)
	e := newEnv(t)
	derek := e.user("derek", true)
	alice := e.user("alice", false)
	e.user("mallory", false)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	other, _ := e.st.CreateWorkspace("o/r2", derek, "", "")
	e.st.SetMembers(ws.ID, []int64{alice.ID}, derek)
	pub, priv, _ := ed25519.GenerateKey(nil)
	key := hex.EncodeToString(pub)

	m := e.signIn("mallory")
	if code, _, _ := m.post("/w/o/r/settings/discord/bot", url.Values{"csrf": {m.csrf()}, "token": {"good-token"}, "public_key": {key}}); code != http.StatusNotFound {
		t.Fatalf("a non-member set up the bot: %d", code)
	}
	a := e.signIn("alice")
	if code, _, _ := a.post("/w/o/r/settings/discord/bot", url.Values{"csrf": {a.csrf()}, "token": {"bad-token"}, "public_key": {key}}); code != http.StatusBadRequest {
		t.Fatalf("a token Discord refuses was saved: %d", code)
	}
	if code, _, _ := a.post("/w/o/r/settings/discord/bot", url.Values{"csrf": {a.csrf()}, "token": {"good-token"}, "public_key": {key}}); code != http.StatusSeeOther {
		t.Fatalf("a member setting up the bot: %d", code)
	}
	if _, body := a.get("/w/o/r/settings"); !strings.Contains(body, "flare-autobot") || strings.Contains(body, "good-token") || !strings.Contains(body, "/discord/interactions/o/r") {
		i := strings.Index(body, `id="discord"`)
		t.Fatalf("the settings page does not show the bot and its address, or shows its token:\n%s", body[i:min(len(body), i+1500)])
	}
	if v, _ := e.st.Setting("discord/" + itoa(ws.ID) + "/token"); v == "" || strings.Contains(v, "good-token") {
		t.Fatalf("the token is missing or stored in the clear: %q", v)
	}
	if code, _, _ := a.post("/w/o/r/settings/discord", url.Values{"csrf": {a.csrf()}, "forum": {"t1"}}); code != http.StatusBadRequest {
		t.Fatalf("a text channel was taken as the forum: %d", code)
	}
	if code, _, _ := a.post("/w/o/r/settings/discord", url.Values{"csrf": {a.csrf()}, "forum": {"f1"}}); code != http.StatusSeeOther {
		t.Fatalf("choosing the forum: %d", code)
	}
	if id, name := e.st.DiscordForum(ws.ID); id != "f1" || name != "flare-autobot" {
		t.Fatalf("forum %q %q", id, name)
	}
	if id, _ := e.st.DiscordForum(other.ID); id != "" {
		t.Fatal("another workspace got the forum")
	}

	post := func(path, body, ts, sig string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("X-Signature-Ed25519", sig)
		req.Header.Set("X-Signature-Timestamp", ts)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	ping := `{"type":1}`
	good := hex.EncodeToString(ed25519.Sign(priv, []byte("100"+ping)))
	if code := post("/discord/interactions/o/r", ping, "100", good); code != http.StatusOK {
		t.Fatalf("Discord's signed check was refused: %d", code)
	}
	if code := post("/discord/interactions/o/r", ping, "101", good); code != http.StatusUnauthorized {
		t.Fatalf("a forged interaction was taken: %d", code)
	}
	if code := post("/discord/interactions/o/r2", ping, "100", good); code != http.StatusNotFound {
		t.Fatalf("a workspace without a bot took a click: %d", code)
	}
}

func mustUserByName(t *testing.T, e *env, name string) *store.User {
	t.Helper()
	u, err := e.st.UserByUsername(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
