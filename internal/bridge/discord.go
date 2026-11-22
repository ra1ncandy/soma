package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultDiscordAPIBase = "https://discord.com/api/v10"

// DiscordConfig configures the out-of-process Discord Channel Bridge adapter.
type DiscordConfig struct {
	BotToken          string
	PublicKeyHex      string
	APIBaseURL        string
	ChannelID         string
	AllowedUserIDs    []string
	AllowedChannelIDs []string
	ListenAddr        string
	PollInboxInterval time.Duration
}

// DiscordAdapter bridges Discord messages/interactions and Inbox alerts to KernelClient over Unix IPC.
type DiscordAdapter struct {
	Client     *KernelClient
	Config     DiscordConfig
	HTTPClient *http.Client

	mu        sync.Mutex
	seenInbox map[string]bool
}

// NewDiscordAdapter constructs a fail-closed DiscordAdapter talking strictly to client.
func NewDiscordAdapter(client *KernelClient, cfg DiscordConfig) *DiscordAdapter {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultDiscordAPIBase
	}
	cfg.APIBaseURL = strings.TrimRight(cfg.APIBaseURL, "/")
	if cfg.PollInboxInterval <= 0 {
		cfg.PollInboxInterval = 5 * time.Second
	}
	return &DiscordAdapter{
		Client:     client,
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		seenInbox:  make(map[string]bool),
	}
}

// VerifyInteractionSignature verifies Discord's Ed25519 signature header over timestamp + raw body.
func VerifyInteractionSignature(pubKeyHex, timestamp, signatureHex string, body []byte) bool {
	if pubKeyHex == "" || timestamp == "" || signatureHex == "" {
		return false
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubKeyHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signatureHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg := make([]byte, 0, len(timestamp)+len(body))
	msg = append(msg, []byte(timestamp)...)
	msg = append(msg, body...)
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}

func (a *DiscordAdapter) isUserAllowed(userID string) bool {
	if userID == "" || len(a.Config.AllowedUserIDs) == 0 {
		return false
	}
	for _, allowed := range a.Config.AllowedUserIDs {
		if allowed == userID {
			return true
		}
	}
	return false
}

func (a *DiscordAdapter) isChannelAllowed(channelID string) bool {
	if len(a.Config.AllowedChannelIDs) == 0 {
		return true
	}
	for _, allowed := range a.Config.AllowedChannelIDs {
		if allowed == channelID {
			return true
		}
	}
	return false
}

// HandleMessage validates the fail-closed user/channel allowlist and dispatches parsed commands.
// Returns (reply, handled, err).
func (a *DiscordAdapter) HandleMessage(ctx context.Context, userID, channelID, content string) (string, bool, error) {
	if !a.isChannelAllowed(channelID) || !a.isUserAllowed(userID) {
		return "", false, ErrUnauthorized
	}
	cmd, ok := ParseCommand(content)
	if !ok {
		return "", false, nil
	}
	reply, err := dispatchBridgeCommand(ctx, a.Client, "discord", "discord:"+userID, "!soma ", cmd)
	if err != nil {
		return "error: " + err.Error(), true, err
	}
	return reply, true, nil
}

type discordInteraction struct {
	Type      int    `json:"type"`
	ChannelID string `json:"channel_id"`
	Member    *struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"member,omitempty"`
	User *struct {
		ID string `json:"id"`
	} `json:"user,omitempty"`
	Data struct {
		Name    string `json:"name"`
		Options []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"options,omitempty"`
	} `json:"data"`
}

// InteractionHandler handles Discord HTTP Interactions with Ed25519 verification + allowlist enforcement.
func (a *DiscordAdapter) InteractionHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		sig := r.Header.Get("X-Signature-Ed25519")
		ts := r.Header.Get("X-Signature-Timestamp")
		if !VerifyInteractionSignature(a.Config.PublicKeyHex, ts, sig, body) {
			http.Error(w, "invalid request signature", http.StatusUnauthorized)
			return
		}

		var inter discordInteraction
		if err := json.Unmarshal(body, &inter); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if inter.Type == 1 {
			writeJSON(w, http.StatusOK, map[string]int{"type": 1})
			return
		}

		var userID string
		if inter.Member != nil {
			userID = inter.Member.User.ID
		}
		if userID == "" && inter.User != nil {
			userID = inter.User.ID
		}

		cmdLine := "!soma " + inter.Data.Name
		if len(inter.Data.Options) > 0 && inter.Data.Options[0].Value != "" {
			cmdLine += " " + inter.Data.Options[0].Value
		}

		reply, _, hErr := a.HandleMessage(r.Context(), userID, inter.ChannelID, cmdLine)
		if errors.Is(hErr, ErrUnauthorized) {
			writeJSON(w, http.StatusOK, map[string]any{
				"type": 4,
				"data": map[string]any{"content": "unauthorized: sender not in soma allowlist", "flags": 64},
			})
			return
		}
		chunks := ChunkMessage(reply, 1900)
		writeJSON(w, http.StatusOK, map[string]any{
			"type": 4,
			"data": map[string]any{"content": chunks[0]},
		})
	})
}

// SendMessage posts text to Discord channelID, chunking cleanly at 1900 characters.
func (a *DiscordAdapter) SendMessage(ctx context.Context, channelID, content string) error {
	if channelID == "" {
		channelID = a.Config.ChannelID
	}
	if channelID == "" {
		return errors.New("discord: ChannelID is required")
	}
	for _, chunk := range ChunkMessage(content, 1900) {
		raw, _ := json.Marshal(map[string]string{"content": chunk})
		endpoint := fmt.Sprintf("%s/channels/%s/messages", a.Config.APIBaseURL, channelID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+a.Config.BotToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("discord api status %d", resp.StatusCode)
		}
	}
	return nil
}

// PollInboxOnce checks KernelClient for unseen pending inbox items and sends alert messages to ChannelID.
func (a *DiscordAdapter) PollInboxOnce(ctx context.Context) (int, error) {
	items, err := a.Client.ListInbox(ctx, "pending")
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, it := range items {
		a.mu.Lock()
		seen := a.seenInbox[it.ID]
		if !seen {
			a.seenInbox[it.ID] = true
		}
		a.mu.Unlock()
		if seen {
			continue
		}
		msg := fmt.Sprintf("[soma inbox:%s] **%s**\n%s\nReply: `!soma approve %s` | `!soma dismiss %s`",
			it.Urgency, it.Title, it.Summary, it.ID, it.ID)
		if err := a.SendMessage(ctx, a.Config.ChannelID, msg); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// Run starts the HTTP interactions endpoint (if ListenAddr is set) and inbox poller until ctx cancels.
func (a *DiscordAdapter) Run(ctx context.Context) error {
	var srv *http.Server
	errCh := make(chan error, 1)
	if a.Config.ListenAddr != "" {
		srv = &http.Server{
			Addr:              a.Config.ListenAddr,
			Handler:           a.InteractionHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	ticker := time.NewTicker(a.Config.PollInboxInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if srv != nil {
				shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutCtx)
			}
			return nil
		case err := <-errCh:
			return err
		case <-ticker.C:
			if a.Config.ChannelID != "" && a.Config.BotToken != "" {
				_, _ = a.PollInboxOnce(ctx)
			}
		}
	}
}
