package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OutboundMessage represents a message delivered from soma to an external chat channel.
type OutboundMessage struct {
	Channel  string `json:"channel"`
	Target   string `json:"target"`
	ThreadID string `json:"thread_id,omitempty"`
	Text     string `json:"text"`
}

// InboundMessage represents a normalized message received from any external chat channel.
type InboundMessage struct {
	Channel    string `json:"channel"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name,omitempty"`
	Target     string `json:"target"`
	ThreadID   string `json:"thread_id,omitempty"`
	Text       string `json:"text"`
	IsDM       bool   `json:"is_dm,omitempty"`
}

// ChannelAdapter is implemented by every messaging platform connector (Discord, Telegram, Slack, Email, etc.).
type ChannelAdapter interface {
	Name() string
	Send(ctx context.Context, msg OutboundMessage) error
}

// SessionRecord tracks unified cross-channel session state for a user or channel target.
type SessionRecord struct {
	SessionKey  string    `json:"session_key"`
	LastChannel string    `json:"last_channel"`
	LastTarget  string    `json:"last_target"`
	ActiveRunID string    `json:"active_run_id,omitempty"`
	BranchID    string    `json:"branch_id,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Gateway routes inbound messages, enforces pairing/allowlists, maintains cross-channel
// session continuity, and delivers outbound/cron notifications to registered ChannelAdapters.
type Gateway struct {
	client       *KernelClient
	pairing      *PairingStore
	sessionsPath string

	mu              sync.RWMutex
	adapters        map[string]ChannelAdapter
	staticAllowlist map[string]map[string]bool
	sessions        map[string]SessionRecord
}

// NewGateway creates a unified multi-channel Gateway backed by client and somaDir.
func NewGateway(client *KernelClient, somaDir string) *Gateway {
	if strings.TrimSpace(somaDir) == "" {
		somaDir = ".soma"
	}
	gw := &Gateway{
		client:          client,
		pairing:         NewPairingStore(somaDir),
		sessionsPath:    filepath.Join(somaDir, "bridge_sessions.json"),
		adapters:        map[string]ChannelAdapter{},
		staticAllowlist: map[string]map[string]bool{},
		sessions:        map[string]SessionRecord{},
	}
	gw.loadSessions()
	return gw
}

// Pairing returns the Gateway's persistent PairingStore.
func (g *Gateway) Pairing() *PairingStore { return g.pairing }

// RegisterAdapter registers a ChannelAdapter by its normalized lowercase name.
func (g *Gateway) RegisterAdapter(adapter ChannelAdapter) {
	if adapter == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.adapters[strings.ToLower(strings.TrimSpace(adapter.Name()))] = adapter
}

// ListAdapters returns the names of all registered channel adapters.
func (g *Gateway) ListAdapters() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.adapters))
	for name := range g.adapters {
		out = append(out, name)
	}
	return out
}

// SetStaticAllowlist configures static allowed sender IDs for a channel.
func (g *Gateway) SetStaticAllowlist(channel string, senderIDs []string) {
	ch := strings.ToLower(strings.TrimSpace(channel))
	m := make(map[string]bool, len(senderIDs))
	for _, id := range senderIDs {
		if t := strings.TrimSpace(id); t != "" {
			m[t] = true
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.staticAllowlist[ch] = m
}

// IsAllowed reports whether senderID on channel is authorized via static allowlist or DM pairing.
func (g *Gateway) IsAllowed(channel, senderID string) bool {
	ch := strings.ToLower(strings.TrimSpace(channel))
	sid := strings.TrimSpace(senderID)
	if ch == "" || sid == "" {
		return false
	}
	g.mu.RLock()
	if m, ok := g.staticAllowlist[ch]; ok && m[sid] {
		g.mu.RUnlock()
		return true
	}
	g.mu.RUnlock()
	return g.pairing.IsApproved(ch, sid)
}

// HandleInbound authorizes msg, updates cross-channel session state, and dispatches commands or runs.
func (g *Gateway) HandleInbound(ctx context.Context, msg InboundMessage) (string, error) {
	ch := strings.ToLower(strings.TrimSpace(msg.Channel))
	if !g.IsAllowed(ch, msg.SenderID) {
		if msg.IsDM {
			req, err := g.pairing.IssueCode(ch, msg.SenderID, msg.SenderName)
			if err != nil {
				return "", ErrUnauthorized
			}
			return fmt.Sprintf("Pairing required. Ask the host operator to run:\nsoma bridge pair %s", req.Code), nil
		}
		return "", ErrUnauthorized
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return "", nil
	}

	sessKey := "operator:" + ch + ":" + msg.SenderID
	if msg.Target != "" && !msg.IsDM {
		sessKey = ch + ":" + msg.Target
	}

	if parsed, ok := ParseCommand(text); ok {
		if parsed.Name == "session" {
			rec, has := g.GetSession(sessKey)
			if !has || rec.ActiveRunID == "" {
				return "session: no active run linked yet", nil
			}
			return fmt.Sprintf("session %s: run=%s branch=%s last_channel=%s", rec.SessionKey, rec.ActiveRunID, rec.BranchID, rec.LastChannel), nil
		}
		if g.client == nil {
			return "", errors.New("gateway: kernel client is not configured")
		}
		reply, err := dispatchBridgeCommand(ctx, g.client, ch+":"+msg.Target, msg.SenderID, "!", parsed)
		if err != nil {
			return "", err
		}
		if parsed.Name == "run" && strings.HasPrefix(reply, "started run ") {
			fields := strings.Fields(reply)
			if len(fields) >= 3 {
				g.upsertSession(sessKey, ch, msg.Target, fields[2], "")
			}
		}
		return reply, nil
	}

	// Free-form DM / channel message starts a synchronous run when kernel client is attached.
	if g.client == nil {
		return "", errors.New("gateway: kernel client is not configured")
	}
	resp, err := g.client.StartRun(ctx, StartRunRequest{
		Goal:   text,
		Source: ch + ":" + msg.Target,
		Sync:   true,
	})
	if err != nil {
		return "", err
	}
	g.upsertSession(sessKey, ch, msg.Target, resp.RunID, resp.BranchID)
	if strings.TrimSpace(resp.Summary) != "" {
		return resp.Summary, nil
	}
	return fmt.Sprintf("run %s (%s)", resp.RunID, resp.Status), nil
}

// Deliver sends msg through the registered ChannelAdapter for msg.Channel.
func (g *Gateway) Deliver(ctx context.Context, msg OutboundMessage) error {
	ch := strings.ToLower(strings.TrimSpace(msg.Channel))
	g.mu.RLock()
	adapter, ok := g.adapters[ch]
	g.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gateway: no adapter registered for channel %q", ch)
	}
	return adapter.Send(ctx, msg)
}

// Broadcast delivers text to every target in targets, aggregating any per-target errors.
func (g *Gateway) Broadcast(ctx context.Context, targets []OutboundMessage, text string) error {
	var errs []string
	for _, t := range targets {
		t.Text = text
		if err := g.Deliver(ctx, t); err != nil {
			errs = append(errs, fmt.Sprintf("%s:%s: %v", t.Channel, t.Target, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// GetSession returns the stored SessionRecord for sessionKey.
func (g *Gateway) GetSession(sessionKey string) (SessionRecord, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rec, ok := g.sessions[sessionKey]
	return rec, ok
}

func (g *Gateway) upsertSession(sessionKey, channel, target, runID, branchID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[sessionKey] = SessionRecord{
		SessionKey:  sessionKey,
		LastChannel: channel,
		LastTarget:  target,
		ActiveRunID: runID,
		BranchID:    branchID,
		UpdatedAt:   time.Now().UTC(),
	}
	_ = g.saveSessionsLocked()
}

func (g *Gateway) loadSessions() {
	raw, err := os.ReadFile(g.sessionsPath)
	if err != nil {
		return
	}
	var m map[string]SessionRecord
	if json.Unmarshal(raw, &m) == nil && m != nil {
		g.sessions = m
	}
}

func (g *Gateway) saveSessionsLocked() error {
	if err := os.MkdirAll(filepath.Dir(g.sessionsPath), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(g.sessions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(g.sessionsPath, raw, 0o600)
}
