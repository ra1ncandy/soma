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
	"strconv"
	"strings"
	"time"
)

const defaultSlackAPIBase = "https://slack.com/api"

// SlackConfig configures the Slack Events API + Web API bridge adapter.
type SlackConfig struct {
	BotToken          string
	SigningSecret     string
	APIBase           string
	AllowedUserIDs    []string
	AllowedChannelIDs []string
}

// SlackAdapter bridges Slack Events API webhooks and chat.postMessage to soma's Gateway.
type SlackAdapter struct {
	Gateway    *Gateway
	Config     SlackConfig
	HTTPClient *http.Client
}

// NewSlackAdapter constructs a SlackAdapter wired to gw (with static allowlists registered).
func NewSlackAdapter(gw *Gateway, cfg SlackConfig) *SlackAdapter {
	if strings.TrimSpace(cfg.APIBase) == "" {
		cfg.APIBase = defaultSlackAPIBase
	}
	if gw != nil && len(cfg.AllowedUserIDs) > 0 {
		gw.SetStaticAllowlist("slack", cfg.AllowedUserIDs)
	}
	return &SlackAdapter{
		Gateway:    gw,
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Name returns "slack".
func (a *SlackAdapter) Name() string { return "slack" }

// Send delivers msg to a Slack channel or thread via chat.postMessage.
func (a *SlackAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	if strings.TrimSpace(a.Config.BotToken) == "" {
		return errors.New("slack: BotToken is required")
	}
	for _, chunk := range ChunkMessage(msg.Text, 3500) {
		body := map[string]any{
			"channel": msg.Target,
			"text":    chunk,
		}
		if msg.ThreadID != "" {
			body["thread_ts"] = msg.ThreadID
		}
		raw, _ := json.Marshal(body)
		url := strings.TrimRight(a.Config.APIBase, "/") + "/chat.postMessage"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+a.Config.BotToken)
		hc := a.HTTPClient
		if hc == nil {
			hc = http.DefaultClient
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("slack chat.postMessage HTTP %d: %s", resp.StatusCode, string(respBytes))
		}
		var apiRes struct {
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
		}
		if json.Unmarshal(respBytes, &apiRes) == nil && !apiRes.OK && apiRes.Error != "" {
			return fmt.Errorf("slack api error: %s", apiRes.Error)
		}
	}
	return nil
}

// Handler returns an http.Handler for Slack Events API webhooks.
func (a *SlackAdapter) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		// Fail closed: without a signing secret anyone who can reach this URL could impersonate
		// an allowlisted Slack user.
		if a.Config.SigningSecret == "" {
			http.Error(w, "slack signing secret is not configured", http.StatusUnauthorized)
			return
		}
		if !verifySlackSignature(a.Config.SigningSecret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body) {
			http.Error(w, "invalid slack signature", http.StatusUnauthorized)
			return
		}

		var env struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
			Event     struct {
				Type        string `json:"type"`
				User        string `json:"user"`
				BotID       string `json:"bot_id"`
				Text        string `json:"text"`
				Channel     string `json:"channel"`
				ChannelType string `json:"channel_type"`
				ThreadTS    string `json:"thread_ts"`
				TS          string `json:"ts"`
			} `json:"event"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if env.Type == "url_verification" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": env.Challenge})
			return
		}
		if env.Type != "event_callback" || env.Event.BotID != "" || env.Event.User == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if len(a.Config.AllowedChannelIDs) > 0 && !containsString(a.Config.AllowedChannelIDs, env.Event.Channel) {
			w.WriteHeader(http.StatusOK)
			return
		}
		if a.Gateway == nil {
			http.Error(w, "gateway not configured", http.StatusInternalServerError)
			return
		}

		threadTS := env.Event.ThreadTS
		if threadTS == "" {
			threadTS = env.Event.TS
		}
		reply, err := a.Gateway.HandleInbound(r.Context(), InboundMessage{
			Channel:  "slack",
			SenderID: env.Event.User,
			Target:   env.Event.Channel,
			ThreadID: threadTS,
			Text:     env.Event.Text,
			IsDM:     env.Event.ChannelType == "im",
		})
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if reply != "" && a.Config.BotToken != "" {
			_ = a.Send(r.Context(), OutboundMessage{
				Channel:  "slack",
				Target:   env.Event.Channel,
				ThreadID: threadTS,
				Text:     reply,
			})
		}
		w.WriteHeader(http.StatusOK)
	})
}

func verifySlackSignature(secret, tsHeader, sigHeader string, body []byte) bool {
	if secret == "" || tsHeader == "" || sigHeader == "" {
		return false
	}
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return false
	}
	diff := time.Now().Unix() - ts
	if diff < -300 || diff > 300 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + tsHeader + ":"))
	mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if strings.TrimSpace(s) == target {
			return true
		}
	}
	return false
}
