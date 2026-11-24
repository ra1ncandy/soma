package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// MatrixConfig configures the Matrix Client-Server API v3 adapter.
type MatrixConfig struct {
	HomeserverURL  string
	AccessToken    string
	UserID         string
	AllowedSenders []string
	AllowedRooms   []string
}

// MatrixAdapter bridges Matrix rooms to soma's Gateway via /_matrix/client/v3/sync & send.
type MatrixAdapter struct {
	Gateway    *Gateway
	Config     MatrixConfig
	HTTPClient *http.Client
	txnSeq     atomic.Uint64
}

// NewMatrixAdapter creates a MatrixAdapter wired to gw.
func NewMatrixAdapter(gw *Gateway, cfg MatrixConfig) *MatrixAdapter {
	if gw != nil && len(cfg.AllowedSenders) > 0 {
		gw.SetStaticAllowlist("matrix", cfg.AllowedSenders)
	}
	return &MatrixAdapter{
		Gateway:    gw,
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 40 * time.Second},
	}
}

// Name returns "matrix".
func (a *MatrixAdapter) Name() string { return "matrix" }

// Send delivers msg.Text to Matrix room msg.Target.
func (a *MatrixAdapter) Send(ctx context.Context, msg OutboundMessage) error {
	if strings.TrimSpace(a.Config.HomeserverURL) == "" || strings.TrimSpace(a.Config.AccessToken) == "" {
		return errors.New("matrix: HomeserverURL and AccessToken are required")
	}
	txnID := fmt.Sprintf("soma-%d-%d", time.Now().UnixNano(), a.txnSeq.Add(1))
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		strings.TrimRight(a.Config.HomeserverURL, "/"),
		url.PathEscape(msg.Target),
		url.PathEscape(txnID),
	)
	raw, _ := json.Marshal(map[string]any{
		"msgtype": "m.text",
		"body":    msg.Text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Config.AccessToken)
	hc := a.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("matrix send HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// PollOnce performs a single /_matrix/client/v3/sync request and processes room text messages.
func (a *MatrixAdapter) PollOnce(ctx context.Context, since string) (string, error) {
	u := strings.TrimRight(a.Config.HomeserverURL, "/") + "/_matrix/client/v3/sync?timeout=25000"
	if since != "" {
		u += "&since=" + url.QueryEscape(since)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return since, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Config.AccessToken)
	hc := a.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return since, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return since, fmt.Errorf("matrix sync HTTP %d: %s", resp.StatusCode, string(b))
	}
	var syncRes struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events []struct {
						Type    string `json:"type"`
						Sender  string `json:"sender"`
						Content struct {
							MsgType string `json:"msgtype"`
							Body    string `json:"body"`
						} `json:"content"`
					} `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&syncRes); err != nil {
		return since, err
	}
	for roomID, room := range syncRes.Rooms.Join {
		if len(a.Config.AllowedRooms) > 0 && !containsString(a.Config.AllowedRooms, roomID) {
			continue
		}
		for _, ev := range room.Timeline.Events {
			if ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" || ev.Sender == "" {
				continue
			}
			if a.Config.UserID != "" && ev.Sender == a.Config.UserID {
				continue
			}
			if a.Gateway == nil {
				continue
			}
			reply, err := a.Gateway.HandleInbound(ctx, InboundMessage{
				Channel:  "matrix",
				SenderID: ev.Sender,
				Target:   roomID,
				Text:     ev.Content.Body,
			})
			if err == nil && reply != "" {
				_ = a.Send(ctx, OutboundMessage{
					Channel: "matrix",
					Target:  roomID,
					Text:    reply,
				})
			}
		}
	}
	if syncRes.NextBatch != "" {
		return syncRes.NextBatch, nil
	}
	return since, nil
}

// Run continuously polls Matrix /sync until ctx is cancelled.
func (a *MatrixAdapter) Run(ctx context.Context) error {
	var since string
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		next, err := a.PollOnce(ctx, since)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		since = next
	}
}
