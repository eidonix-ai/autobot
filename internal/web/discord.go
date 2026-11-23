package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
)

// addDiscord saves a Discord channel, once Discord confirms the webhook (without posting).
func (s *Server) addDiscord(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	hook := f.Get("webhook")
	fail := func(status int, msg string) {
		f.Del("webhook") // never send a webhook back to the browser
		s.settingsPage(w, r, status, ws, map[string]any{"DiscordError": msg, "Form": f})
	}
	if _, err := store.WebhookID(hook); err != nil {
		msg, _ := inputError(err)
		fail(http.StatusBadRequest, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if _, err := discord.Check(ctx, hook); err != nil {
		fail(http.StatusBadGateway, "The channel was not added: "+err.Error()+".")
		return
	}
	_, err := s.st.AddDiscordChannel(ws, f.Get("name"), hook, me(r))
	if msg, ok := inputError(err); ok {
		fail(http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
}

// discordChannel resolves the channel in the URL, one of the workspace's.
func (s *Server) discordChannel(w http.ResponseWriter, r *http.Request) (*store.Workspace, int64, string, bool) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return nil, 0, "", false
	}
	id, _ := strconv.ParseInt(r.PathValue("channel"), 10, 64)
	hook, err := s.st.DiscordWebhook(ws.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, 0, "", false
	}
	if err != nil {
		s.fail(w, err)
		return nil, 0, "", false
	}
	return ws, id, hook, true
}

func (s *Server) testDiscord(w http.ResponseWriter, r *http.Request) {
	ws, _, hook, ok := s.discordChannel(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := discord.Post(ctx, hook, "Autobot is connected to this channel for "+ws.Name+"."); err != nil {
		s.settingsPage(w, r, http.StatusBadGateway, ws, map[string]any{"DiscordError": "The test message was not sent: " + err.Error() + "."})
		return
	}
	s.settingsPage(w, r, http.StatusOK, ws, map[string]any{"DiscordNotice": "Test message sent."})
}

func (s *Server) removeDiscord(w http.ResponseWriter, r *http.Request) {
	ws, id, _, ok := s.discordChannel(w, r)
	if !ok {
		return
	}
	if err := s.st.RemoveDiscordChannel(ws, id, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
}

// postDiscord sends a message to one of the workspace's channels.
func (s *Server) postDiscord(ctx context.Context, ws *store.Workspace, channel int64, text string) error {
	hook, err := s.st.DiscordWebhook(ws.ID, channel)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("the channel is no longer one of this workspace's; choose another")
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return discord.Post(ctx, hook, text)
}

// channelNames maps a workspace's Discord channels by id, for forms and the canvas.
func (s *Server) channelNames(ws *store.Workspace) map[int64]string {
	out := map[int64]string{}
	list, _ := s.st.DiscordChannels(ws.ID)
	for _, c := range list {
		out[c.ID] = c.Name
	}
	return out
}
