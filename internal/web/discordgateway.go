package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// DiscordTrigger is the stored trigger payload for a Discord workflow run (saved in workflow_runs.trigger_data).
type DiscordTrigger struct {
	GuildID   string `json:"guild_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Channel   string `json:"channel,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	AuthorID  string `json:"author_id,omitempty"`
	Author    string `json:"author,omitempty"`
	Content   string `json:"content,omitempty"`
	Context   string `json:"context,omitempty"`
	URL       string `json:"url,omitempty"`
}

func parseDiscordTrigger(raw string) DiscordTrigger {
	var t DiscordTrigger
	if raw != "" {
		json.Unmarshal([]byte(raw), &t)
	}
	return t
}

func (t DiscordTrigger) values(ws *store.Workspace) map[string]string {
	ch := t.Channel
	if ch != "" && !strings.HasPrefix(ch, "#") {
		ch = "#" + ch
	}
	if ch == "" && t.ChannelID != "" {
		ch = t.ChannelID
	}
	return map[string]string{
		"repo":               ws.Name,
		"discord:message":    t.Content,
		"discord:author":     t.Author,
		"discord:channel":    ch,
		"discord:channel_id": t.ChannelID,
		"discord:link":       t.URL,
		"discord:context":    t.Context,
	}
}

// discordGatewayStatus tracks a workspace's live Discord Gateway connection for the UI.
type discordGatewayStatus struct {
	Connected bool
	BotName   string
	Problem   string
}

type discordWorker struct {
	token  string
	cancel context.CancelFunc
}

type cachedChannel struct {
	name string
	at   time.Time
}

// discordHub manages one Gateway goroutine per workspace that has both a Discord bot configured
// and its Discord workflow switched Live.
type discordHub struct {
	mu       sync.Mutex
	workers  map[int64]*discordWorker
	status   map[int64]discordGatewayStatus
	seen     map[string]time.Time // messageID -> seen time (dedup across reconnects)
	cooldown map[string]time.Time // "ws:node:channel" -> last trigger time
	channels map[string]cachedChannel
	poke     chan struct{}
}

func newDiscordHub() *discordHub {
	return &discordHub{
		workers:  map[int64]*discordWorker{},
		status:   map[int64]discordGatewayStatus{},
		seen:     map[string]time.Time{},
		cooldown: map[string]time.Time{},
		channels: map[string]cachedChannel{},
		poke:     make(chan struct{}, 1),
	}
}

// Poke wakes the reconciliation loop immediately (after Live toggle or bot token save/remove).
func (h *discordHub) Poke() {
	if h == nil {
		return
	}
	select {
	case h.poke <- struct{}{}:
	default:
	}
}

func (h *discordHub) getStatus(wsID int64) discordGatewayStatus {
	if h == nil {
		return discordGatewayStatus{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status[wsID]
}

func (h *discordHub) setStatus(wsID int64, st discordGatewayStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status[wsID] = st
}

// markSeen returns false if msgID was already seen recently.
func (h *discordHub) markSeen(msgID string) bool {
	if msgID == "" {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if _, ok := h.seen[msgID]; ok {
		return false
	}
	if len(h.seen) > 500 {
		for k, at := range h.seen {
			if now.Sub(at) > time.Hour {
				delete(h.seen, k)
			}
		}
	}
	h.seen[msgID] = now
	return true
}

// allowCooldown checks and updates per-(workspace, node, channel) cooldown.
func (h *discordHub) allowCooldown(wsID int64, nodeID, channelID string, sec int) bool {
	if sec <= 0 {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := fmt.Sprintf("%d:%s:%s", wsID, nodeID, channelID)
	now := time.Now()
	if last, ok := h.cooldown[key]; ok && now.Sub(last) < time.Duration(sec)*time.Second {
		return false
	}
	h.cooldown[key] = now
	return true
}

func (h *discordHub) channelName(ctx context.Context, b *discord.Bot, channelID string) string {
	if channelID == "" || b == nil {
		return ""
	}
	h.mu.Lock()
	if c, ok := h.channels[channelID]; ok && time.Since(c.at) < 10*time.Minute {
		h.mu.Unlock()
		return c.name
	}
	h.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := b.ChannelInfo(cctx, channelID)
	if err != nil || info == nil || info.Name == "" {
		return ""
	}
	h.mu.Lock()
	h.channels[channelID] = cachedChannel{name: info.Name, at: time.Now()}
	h.mu.Unlock()
	return info.Name
}

// RunDiscord maintains Gateway listeners for every workspace with a live Discord workflow.
func (s *Server) RunDiscord(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	s.reconcileDiscord(ctx)
	for {
		select {
		case <-ctx.Done():
			s.stopAllDiscord()
			return
		case <-s.discordHub.poke:
			s.reconcileDiscord(ctx)
		case <-t.C:
			s.reconcileDiscord(ctx)
		}
	}
}

func (s *Server) stopAllDiscord() {
	h := s.discordHub
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, w := range h.workers {
		w.cancel()
		delete(h.workers, id)
	}
}

func (s *Server) reconcileDiscord(ctx context.Context) {
	live, err := s.st.LiveWorkflows()
	if err != nil {
		log.Printf("discord gateway: %v", err)
		return
	}
	want := map[int64]string{}
	for _, w := range live {
		if w.Kind != workflow.Discord {
			continue
		}
		tok, _, err := s.st.DiscordBot(w.WorkspaceID)
		if err != nil || tok == "" {
			s.discordHub.setStatus(w.WorkspaceID, discordGatewayStatus{Problem: "No Discord bot is configured in workspace Settings."})
			continue
		}
		want[w.WorkspaceID] = tok
	}

	h := s.discordHub
	h.mu.Lock()
	defer h.mu.Unlock()
	for wsID, w := range h.workers {
		if tok, ok := want[wsID]; !ok || tok != w.token {
			w.cancel()
			delete(h.workers, wsID)
			if !ok {
				delete(h.status, wsID)
			}
		}
	}
	for wsID, tok := range want {
		if _, running := h.workers[wsID]; running {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		h.workers[wsID] = &discordWorker{token: tok, cancel: cancel}
		go s.runWorkspaceGateway(wctx, wsID, tok)
	}
}

func (s *Server) runWorkspaceGateway(ctx context.Context, wsID int64, token string) {
	defer survive(fmt.Sprintf("discord gateway for workspace %d", wsID), nil)
	err := discord.RunGateway(ctx, discord.GatewayConfig{
		Token: token,
		OnStatus: func(connected bool, botName, problem string) {
			s.discordHub.setStatus(wsID, discordGatewayStatus{Connected: connected, BotName: botName, Problem: problem})
		},
		OnMention: func(ev discord.MentionEvent) {
			go s.handleDiscordMention(ctx, wsID, ev)
		},
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("discord gateway (workspace %d): %v", wsID, err)
	}
}

const maxDiscordContextChars = 6000

// handleDiscordMention evaluates a live workspace's Discord workflow for an incoming @mention and
// starts matching Discord Mention event blocks.
func (s *Server) handleDiscordMention(ctx context.Context, wsID int64, ev discord.MentionEvent) {
	defer survive(fmt.Sprintf("discord mention ws %d msg %s", wsID, ev.MessageID), nil)
	if !s.discordHub.markSeen(ev.MessageID) {
		return
	}
	ws, err := s.st.WorkspaceByID(wsID)
	if err != nil {
		return
	}
	stored, g, err := s.loadWorkflow(ws, workflow.Discord)
	if err != nil || !stored.Live {
		return
	}
	b, _ := s.bot(ws)
	channelName := s.discordHub.channelName(ctx, b, ev.ChannelID)

	// Find matching Discord Mention event nodes before fetching recent channel messages.
	var matched []workflow.Node
	for _, n := range g.Nodes {
		if n.Type != workflow.DiscordMention {
			continue
		}
		cfg, err := workflow.EventSettingsOf(n)
		if err != nil {
			continue
		}
		if !workflow.MatchesDiscordChannel(cfg, channelName, ev.ChannelID) {
			continue
		}
		if !s.discordHub.allowCooldown(ws.ID, n.ID, ev.ChannelID, cfg.CooldownSeconds) {
			continue
		}
		matched = append(matched, n)
	}
	if len(matched) == 0 {
		return
	}

	contextText := buildDiscordContext(ctx, b, ev)
	author := ev.Author
	if ev.AuthorHandle != "" && !strings.EqualFold(ev.AuthorHandle, ev.Author) {
		author = fmt.Sprintf("%s (@%s)", ev.Author, ev.AuthorHandle)
	}
	trig := DiscordTrigger{
		GuildID:   ev.GuildID,
		ChannelID: ev.ChannelID,
		Channel:   channelName,
		MessageID: ev.MessageID,
		AuthorID:  ev.AuthorID,
		Author:    author,
		Content:   ev.CleanContent,
		Context:   contextText,
		URL:       ev.URL,
	}
	for _, n := range matched {
		if _, err := s.startDiscordWorkflow(ctx, ws, n.ID, trig, false, nil); err != nil {
			log.Printf("discord mention (%s): starting %s: %v", ws.Name, n.ID, err)
		}
	}
}

func buildDiscordContext(ctx context.Context, b *discord.Bot, ev discord.MentionEvent) string {
	var lines []string
	if ev.ReplyToText != "" {
		lines = append(lines, fmt.Sprintf("[Replying to %s]: %s", ev.ReplyToAuthor, ev.ReplyToText))
	}
	if b != nil && ev.ChannelID != "" {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		msgs, err := b.RecentMessages(rctx, ev.ChannelID, ev.MessageID, 10)
		cancel()
		if err == nil {
			for _, m := range msgs {
				if m.Content == "" {
					continue
				}
				tag := m.Author
				if m.Bot {
					tag += " [bot]"
				}
				lines = append(lines, fmt.Sprintf("%s: %s", tag, m.Content))
			}
		}
	}
	text := strings.Join(lines, "\n")
	if len(text) > maxDiscordContextChars {
		text = strings.ToValidUTF8(text[len(text)-maxDiscordContextChars:], "")
	}
	return text
}
