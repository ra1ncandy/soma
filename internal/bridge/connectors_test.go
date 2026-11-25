package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

func startTestGateway(t *testing.T) (*Gateway, func()) {
	t.Helper()
	ws := t.TempDir()
	somaDir := filepath.Join(ws, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := NewKernelServer(st, ServerConfig{
		SomaDir:      somaDir,
		WorkspaceDir: ws,
		RunStarter: func(_ context.Context, req StartRunRequest) (StartRunResponse, error) {
			return StartRunResponse{
				RunID:    "run-100",
				BranchID: "br-1",
				Status:   "completed",
				Summary:  "echo:" + req.Goal,
			}, nil
		},
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ks.Start(ctx) }()
	client := NewClient(ks.SocketPath(), ks.Token())
	for i := 0; i < 50; i++ {
		if h, err := client.Health(ctx); err == nil && h.OK {
			break
		}
	}
	gw := NewGateway(client, somaDir)
	return gw, func() {
		cancel()
		_ = st.Close()
	}
}

func TestSlackAdapterWebhookAndSend(t *testing.T) {
	gw, cleanup := startTestGateway(t)
	defer cleanup()

	var posted []map[string]any
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackAPI.Close()

	secret := "slack-signing-secret"
	adp := NewSlackAdapter(gw, SlackConfig{
		BotToken:       "xoxb-test",
		SigningSecret:  secret,
		APIBase:        slackAPI.URL,
		AllowedUserIDs: []string{"U1"},
	})
	h := adp.Handler()

	// 1. url_verification
	chalBody := []byte(`{"type":"url_verification","challenge":"xyz-123"}`)
	req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(chalBody))
	tsStr := fmt.Sprintf("%d", timeNowUnix())
	req.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req.Header.Set("X-Slack-Signature", signSlack(secret, tsStr, chalBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "xyz-123") {
		t.Fatalf("url_verification failed: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 2. event_callback DM
	evBody := []byte(`{"type":"event_callback","event":{"type":"message","user":"U1","text":"ping soma","channel":"D1","channel_type":"im","ts":"1700.1"}}`)
	req2 := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(evBody))
	req2.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req2.Header.Set("X-Slack-Signature", signSlack(secret, tsStr, evBody))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || len(posted) != 1 || posted[0]["text"] != "echo:ping soma" {
		t.Fatalf("event_callback failed: code=%d posted=%+v", rec2.Code, posted)
	}
}

func TestEmailAdapterSend(t *testing.T) {
	var sentAddr, sentFrom string
	var sentTo []string
	var sentBody string
	adp := NewEmailAdapter(EmailConfig{
		SMTPHost: "smtp.example.com",
		SMTPPort: 587,
		From:     "soma@example.com",
		Subject:  "Alert",
	})
	adp.sendMail = func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
		sentAddr = addr
		sentFrom = from
		sentTo = to
		sentBody = string(msg)
		return nil
	}

	err := adp.Send(context.Background(), OutboundMessage{
		Channel: "email",
		Target:  "ops@example.com, dev@example.com",
		Text:    "build green",
	})
	if err != nil {
		t.Fatalf("EmailAdapter.Send: %v", err)
	}
	if sentAddr != "smtp.example.com:587" || sentFrom != "soma@example.com" || len(sentTo) != 2 || !strings.Contains(sentBody, "build green") {
		t.Fatalf("unexpected email send: addr=%s from=%s to=%v body=%s", sentAddr, sentFrom, sentTo, sentBody)
	}
}

func TestMatrixAdapterPollAndSend(t *testing.T) {
	gw, cleanup := startTestGateway(t)
	defer cleanup()

	var sentText string
	mxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/sync") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"next_batch": "batch-2",
				"rooms": map[string]any{
					"join": map[string]any{
						"!room1:matrix.org": map[string]any{
							"timeline": map[string]any{
								"events": []map[string]any{
									{
										"type":   "m.room.message",
										"sender": "@alice:matrix.org",
										"content": map[string]any{
											"msgtype": "m.text",
											"body":    "!soma status",
										},
									},
								},
							},
						},
					},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/send/m.room.message/") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sentText, _ = body["body"].(string)
			_ = json.NewEncoder(w).Encode(map[string]string{"event_id": "$ev1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer mxSrv.Close()

	adp := NewMatrixAdapter(gw, MatrixConfig{
		HomeserverURL:  mxSrv.URL,
		AccessToken:    "mx-tok",
		UserID:         "@soma:matrix.org",
		AllowedSenders: []string{"@alice:matrix.org"},
	})
	next, err := adp.PollOnce(context.Background(), "")
	if err != nil || next != "batch-2" || !strings.Contains(sentText, "soma status:") {
		t.Fatalf("Matrix PollOnce failed: err=%v next=%q sent=%q", err, next, sentText)
	}
}

func TestWebhookAdaptersMattermostGoogleChatAndSidecar(t *testing.T) {
	gw, cleanup := startTestGateway(t)
	defer cleanup()

	// Mattermost
	mm := NewMattermostAdapter(gw, MattermostConfig{
		OutgoingToken:  "mm-tok",
		AllowedUserIDs: []string{"u-mm"},
	})
	mmBody := []byte(`{"token":"mm-tok","user_id":"u-mm","channel_id":"c1","text":"!soma status"}`)
	mmReq := httptest.NewRequest(http.MethodPost, "/mm", bytes.NewReader(mmBody))
	mmRec := httptest.NewRecorder()
	mm.Handler().ServeHTTP(mmRec, mmReq)
	if mmRec.Code != http.StatusOK || !strings.Contains(mmRec.Body.String(), "soma status:") {
		t.Fatalf("Mattermost failed: code=%d body=%s", mmRec.Code, mmRec.Body.String())
	}

	// Google Chat
	gc := NewGoogleChatAdapter(gw, "", []string{"users/1"})
	gcBody := []byte(`{"type":"MESSAGE","space":{"name":"spaces/1","type":"DM"},"message":{"text":"hello gchat","sender":{"name":"users/1"}}}`)
	gc.InboundToken = "gc-secret"
	gcReq := httptest.NewRequest(http.MethodPost, "/gchat?token=gc-secret", bytes.NewReader(gcBody))
	gcRec := httptest.NewRecorder()
	gc.Handler().ServeHTTP(gcRec, gcReq)
	if gcRec.Code != http.StatusOK || !strings.Contains(gcRec.Body.String(), "echo:hello gchat") {
		t.Fatalf("GoogleChat failed: code=%d body=%s", gcRec.Code, gcRec.Body.String())
	}

	// Generic sidecar (WhatsApp / Signal) with HMAC
	wa := NewWebhookBridgeAdapter(gw, WebhookBridgeConfig{
		ChannelName:    "whatsapp",
		HMACSecret:     "wa-secret",
		AllowedSenders: []string{"+15551234567"},
	})
	waBody := []byte(`{"sender_id":"+15551234567","target":"+15551234567","text":"hi whatsapp","is_dm":true}`)
	mac := hmac.New(sha256.New, []byte("wa-secret"))
	mac.Write(waBody)
	waReq := httptest.NewRequest(http.MethodPost, "/wa", bytes.NewReader(waBody))
	waReq.Header.Set("X-Soma-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	waRec := httptest.NewRecorder()
	wa.Handler().ServeHTTP(waRec, waReq)
	if waRec.Code != http.StatusOK || !strings.Contains(waRec.Body.String(), "echo:hi whatsapp") {
		t.Fatalf("WhatsApp sidecar failed: code=%d body=%s", waRec.Code, waRec.Body.String())
	}
}

func signSlack(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func timeNowUnix() int64 {
	return time.Now().Unix()
}
