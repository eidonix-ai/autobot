package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/discord"
)

// bot returns the instance's Discord bot, or nil when none is set.
func (s *Server) bot() (*discord.Bot, string) {
	token, key, err := s.st.DiscordBot()
	if err != nil || token == "" {
		return nil, ""
	}
	return &discord.Bot{Token: token}, key
}

// discordPage is the admin's page for the bot: its token and public key, and whether Discord
// accepts them.
func (s *Server) discordPage(w http.ResponseWriter, r *http.Request) {
	s.renderDiscord(w, r, http.StatusOK, map[string]any{})
}

func (s *Server) renderDiscord(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	data["Title"], data["Endpoint"] = "Discord", publicURL(r)+"/discord/interactions"
	if b, key := s.bot(); b != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		data["Set"], data["PublicKey"] = true, key
		if me, err := b.Me(ctx); err != nil {
			data["BotProblem"] = err.Error() + "."
		} else {
			data["BotName"] = me.Username
			if forums, err := b.Forums(ctx); err == nil {
				data["Forums"] = forums
			}
		}
	}
	if r.URL.Query().Has("saved") {
		data["Notice"] = "Saved."
	}
	s.render(w, r, status, "discord.html", data)
}

func (s *Server) saveDiscord(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	if f.Get("remove") == "on" {
		if err := s.st.RemoveDiscordBot(); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, "/admin/discord?saved=1", http.StatusSeeOther)
		return
	}
	token, key := strings.TrimSpace(f.Get("token")), strings.ToLower(strings.TrimSpace(f.Get("public_key")))
	if k, err := hex.DecodeString(key); err != nil || len(k) != 32 {
		s.renderDiscord(w, r, http.StatusBadRequest, map[string]any{"Error": "The public key is 64 hexadecimal characters: the application's General Information page shows it."})
		return
	}
	if token == "" {
		if old, _, err := s.st.DiscordBot(); err == nil {
			token = old
		}
	}
	if token == "" {
		s.renderDiscord(w, r, http.StatusBadRequest, map[string]any{"Error": "Give the bot's token: the application's Bot page makes one."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := (discord.Bot{Token: token}).Me(ctx); err != nil {
		s.renderDiscord(w, r, http.StatusBadRequest, map[string]any{"Error": "Not saved: " + err.Error() + "."})
		return
	}
	if err := s.st.SetDiscordBot(f.Get("token"), key); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/admin/discord?saved=1", http.StatusSeeOther)
}

// setForum sets the forum a workspace's cases are posted in: one the bot can see.
func (s *Server) setForum(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id := r.PostForm.Get("forum")
	if id == "" {
		if err := s.st.SetDiscordForum(ws, "", "", me(r)); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
		return
	}
	b, _ := s.bot()
	if b == nil {
		http.Error(w, "No Discord bot is set up.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	forums, err := b.Forums(ctx)
	if err != nil {
		s.settingsPage(w, r, http.StatusBadGateway, ws, map[string]any{"DiscordError": "Could not ask Discord for its forums: " + err.Error() + "."})
		return
	}
	for _, f := range forums {
		if f.ID == id {
			if err := s.st.SetDiscordForum(ws, f.ID, f.Name, me(r)); err != nil {
				s.fail(w, err)
				return
			}
			http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
			return
		}
	}
	s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"DiscordError": "Choose a forum the bot can see."})
}

// interaction is what Discord sends when someone clicks one of the bot's buttons. Each one is
// checked to be signed by Discord, with the application's public key; anything else is refused.
func (s *Server) interaction(w http.ResponseWriter, r *http.Request) {
	_, key, err := s.st.DiscordBot()
	if err != nil || key == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !discord.Verify(key, r.Header.Get("X-Signature-Ed25519"), r.Header.Get("X-Signature-Timestamp"), body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var in struct {
		Type int `json:"type"`
	}
	json.Unmarshal(body, &in)
	w.Header().Set("Content-Type", "application/json")
	switch in.Type {
	case 1: // Discord checking the endpoint
		io.WriteString(w, `{"type":1}`)
	default:
		s.discordClick(w, body)
	}
}

// discordClick answers a click on one of the bot's buttons.
func (s *Server) discordClick(w http.ResponseWriter, body []byte) { s.approvalClick(w, body) }
