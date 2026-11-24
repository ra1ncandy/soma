package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func postJSON(ctx context.Context, hc *http.Client, targetURL, bearer string, payload any) error {
	if strings.TrimSpace(targetURL) == "" {
		return errors.New("webhook: target URL is required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("webhook POST %s HTTP %d: %s", targetURL, resp.StatusCode, string(b))
	}
	return nil
}

// MattermostConfig configures Mattermost Bot API / Incoming & Outgoing Webhooks.
type MattermostConfig struct {
	ServerURL      string
	BotToken       string
	WebhookURL     string
	OutgoingToken  string
	AllowedUserIDs []string
}

// MattermostAdapter bridges Mattermost channels and outgoing webhooks to soma's Gateway.
type MattermostAdapter struct {
	Gateway    *Gateway
	Config     MattermostConfig
	HTTPClient *http.Client
}

// NewMattermostAdapter constructs a MattermostAdapter wired to gw.
func NewMattermostAdapter(gw *Gateway, cfg MattermostConfig) *MattermostAdapter {
	if gw != nil && len(cfg.AllowedUserIDs) > 0 {
		gw.SetStaticAllowlist("mattermost", cfg.AllowedUserIDs)
	}
	return &MattermostAdapter{Gateway: gw, Config: cfg, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// Name returns "mattermost".
func (a *MattermostAdapter) Name() string { return "mattermost" }

// Send posts a message to Mattermost via /api/v4/posts or WebhookURL.
func (a *MattermostAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	if strings.TrimSpace(a.Config.ServerURL) != "" && strings.TrimSpace(a.Config.BotToken) != "" {
		u := strings.TrimRight(a.Config.ServerURL, "/") + "/api/v4/posts"
		payload := map[string]any{
			"channel_id": msg.Target,
			"message":    msg.Text,
		}
		if msg.ThreadID != "" {
			payload["root_id"] = msg.ThreadID
		}
		return postJSON(ctx, a.HTTPClient, u, a.Config.BotToken, payload)
	}
	return postJSON(ctx, a.HTTPClient, a.Config.WebhookURL, "", map[string]any{"text": msg.Text})
}

// Handler handles Mattermost outgoing webhook POSTs and returns a synchronous {"text": ...} reply.
func (a *MattermostAdapter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var payload struct {
			Token     string `json:"token"`
			UserID    string `json:"user_id"`
			UserName  string `json:"user_name"`
			ChannelID string `json:"channel_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if a.Config.OutgoingToken != "" && payload.Token != a.Config.OutgoingToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if a.Gateway == nil {
			http.Error(w, "gateway not configured", http.StatusInternalServerError)
			return
		}
		reply, err := a.Gateway.HandleInbound(r.Context(), InboundMessage{
			Channel:    "mattermost",
			SenderID:   payload.UserID,
			SenderName: payload.UserName,
			Target:     payload.ChannelID,
			Text:       payload.Text,
		})
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": reply})
	})
}

// TeamsAdapter delivers messages to Microsoft Teams Incoming Webhooks and handles outgoing webhooks.
type TeamsAdapter struct {
	Gateway    *Gateway
	WebhookURL string
	HTTPClient *http.Client
}

// NewTeamsAdapter constructs a Microsoft Teams adapter.
func NewTeamsAdapter(gw *Gateway, webhookURL string, allowedUsers []string) *TeamsAdapter {
	if gw != nil && len(allowedUsers) > 0 {
		gw.SetStaticAllowlist("teams", allowedUsers)
	}
	return &TeamsAdapter{Gateway: gw, WebhookURL: webhookURL, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// Name returns "teams".
func (a *TeamsAdapter) Name() string { return "teams" }

// Send posts a MessageCard payload to the configured Teams WebhookURL (or msg.Target if WebhookURL is empty).
func (a *TeamsAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	target := a.WebhookURL
	if strings.HasPrefix(msg.Target, "http") {
		target = msg.Target
	}
	return postJSON(ctx, a.HTTPClient, target, "", map[string]any{
		"@type": "MessageCard",
		"text":  msg.Text,
	})
}

// GoogleChatAdapter bridges Google Chat incoming webhooks and synchronous bot event callbacks.
type GoogleChatAdapter struct {
	Gateway    *Gateway
	WebhookURL string
	HTTPClient *http.Client
}

// NewGoogleChatAdapter constructs a Google Chat adapter.
func NewGoogleChatAdapter(gw *Gateway, webhookURL string, allowedUsers []string) *GoogleChatAdapter {
	if gw != nil && len(allowedUsers) > 0 {
		gw.SetStaticAllowlist("google-chat", allowedUsers)
	}
	return &GoogleChatAdapter{Gateway: gw, WebhookURL: webhookURL, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// Name returns "google-chat".
func (a *GoogleChatAdapter) Name() string { return "google-chat" }

// Send delivers msg.Text to Google Chat via WebhookURL.
func (a *GoogleChatAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	target := a.WebhookURL
	if strings.HasPrefix(msg.Target, "http") {
		target = msg.Target
	}
	payload := map[string]any{"text": msg.Text}
	if msg.ThreadID != "" {
		payload["thread"] = map[string]string{"name": msg.ThreadID}
	}
	return postJSON(ctx, a.HTTPClient, target, "", payload)
}

// Handler handles Google Chat event webhook requests and returns synchronous {"text": ...}.
func (a *GoogleChatAdapter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Text   string `json:"text"`
				Thread struct {
					Name string `json:"name"`
				} `json:"thread"`
				Sender struct {
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
				} `json:"sender"`
			} `json:"message"`
			Space struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"space"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if ev.Type != "MESSAGE" || a.Gateway == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		reply, err := a.Gateway.HandleInbound(r.Context(), InboundMessage{
			Channel:    "google-chat",
			SenderID:   ev.Message.Sender.Name,
			SenderName: ev.Message.Sender.DisplayName,
			Target:     ev.Space.Name,
			ThreadID:   ev.Message.Thread.Name,
			Text:       ev.Message.Text,
			IsDM:       ev.Space.Type == "DM",
		})
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"text": reply})
	})
}

// WebhookBridgeConfig configures a generic HTTP JSON sidecar bridge (used for WhatsApp, Signal, SMS, etc.).
type WebhookBridgeConfig struct {
	ChannelName    string
	OutboundURL    string
	BearerToken    string
	HMACSecret     string
	AllowedSenders []string
}

// WebhookBridgeAdapter connects external sidecar bridges (WhatsApp, Signal, SMS) to soma's Gateway.
type WebhookBridgeAdapter struct {
	Gateway    *Gateway
	Config     WebhookBridgeConfig
	HTTPClient *http.Client
}

// NewWebhookBridgeAdapter constructs a generic sidecar webhook bridge for WhatsApp, Signal, SMS, etc.
func NewWebhookBridgeAdapter(gw *Gateway, cfg WebhookBridgeConfig) *WebhookBridgeAdapter {
	ch := strings.ToLower(strings.TrimSpace(cfg.ChannelName))
	if ch == "" {
		ch = "webhook"
	}
	cfg.ChannelName = ch
	if gw != nil && len(cfg.AllowedSenders) > 0 {
		gw.SetStaticAllowlist(ch, cfg.AllowedSenders)
	}
	return &WebhookBridgeAdapter{Gateway: gw, Config: cfg, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// Name returns the configured channel name (e.g. "whatsapp", "signal", "sms").
func (a *WebhookBridgeAdapter) Name() string { return a.Config.ChannelName }

// Send forwards OutboundMessage JSON to the sidecar OutboundURL.
func (a *WebhookBridgeAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	if msg.Channel == "" {
		msg.Channel = a.Config.ChannelName
	}
	return postJSON(ctx, a.HTTPClient, a.Config.OutboundURL, a.Config.BearerToken, msg)
}

// Handler accepts InboundMessage JSON from a sidecar bridge, verifies optional HMAC, and returns {"reply": ...}.
func (a *WebhookBridgeAdapter) Handler() http.Handler {
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
		if a.Config.HMACSecret != "" {
			sig := r.Header.Get("X-Soma-Signature")
			mac := hmac.New(sha256.New, []byte(a.Config.HMACSecret))
			mac.Write(body)
			want := hex.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(want), []byte(strings.TrimPrefix(sig, "sha256="))) {
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}
		}
		var in InboundMessage
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if in.Channel == "" {
			in.Channel = a.Config.ChannelName
		}
		if a.Gateway == nil {
			http.Error(w, "gateway not configured", http.StatusInternalServerError)
			return
		}
		reply, err := a.Gateway.HandleInbound(r.Context(), in)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"reply": reply})
	})
}
