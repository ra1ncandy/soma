package bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "soma-br-*")
	if err != nil {
		t.Fatalf("MkdirTemp /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startTestKernelServer(t *testing.T) (*store.Store, *KernelServer, *KernelClient, context.CancelFunc) {
	t.Helper()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	if err := st.UpsertAccount(ctx, store.Account{
		ID: "default", Provider: "local", ModelID: "mock-worker-v1", BillingMode: "local", MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	sockDir := shortSocketDir(t)
	srv, err := NewKernelServer(st, ServerConfig{
		SomaDir:      st.SomaDir(),
		WorkspaceDir: ws,
		SocketPath:   filepath.Join(sockDir, "kernel.sock"),
		TokenPath:    filepath.Join(sockDir, "bridge.token"),
		Provider:     &dag.MockProvider{ModelName: "mock-worker-v1"},
	})
	if err != nil {
		t.Fatalf("NewKernelServer: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.Start(runCtx)
	}()

	client := NewClient(srv.SocketPath(), srv.Token())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Health(ctx); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-doneCh
	})
	return st, srv, client, cancel
}

func TestKernelServerPermissionsAuthAndRPC(t *testing.T) {
	ctx := context.Background()
	st, srv, client, _ := startTestKernelServer(t)

	tokInfo, err := os.Stat(srv.TokenPath())
	if err != nil || tokInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected bridge.token 0600, got mode=%v err=%v", tokInfo.Mode().Perm(), err)
	}
	sockInfo, err := os.Stat(srv.SocketPath())
	if err != nil || sockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected kernel.sock 0600, got mode=%v err=%v", sockInfo.Mode().Perm(), err)
	}

	// Unauthenticated / wrong-token request must get 401 Unauthorized.
	badClient := NewClient(srv.SocketPath(), "wrong-token")
	if _, err := badClient.Health(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 Unauthorized for wrong token, got %v", err)
	}

	// Valid Health + StartRun + Status + Inbox Approve round-trip.
	h, err := client.Health(ctx)
	if err != nil || !h.OK {
		t.Fatalf("Health failed: %+v err=%v", h, err)
	}
	sr, err := client.StartRun(ctx, StartRunRequest{
		Goal: "verify bridge rpc", Source: "test", Sync: true, MaxTurns: 5,
	})
	if err != nil || sr.RunID == "" || sr.Status != "completed" {
		t.Fatalf("StartRun sync failed: %+v err=%v", sr, err)
	}
	stResp, err := client.Status(ctx)
	if err != nil || len(stResp.Runs) != 1 || len(stResp.Branches) != 1 {
		t.Fatalf("Status mismatch: %+v err=%v", stResp, err)
	}

	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-br-1", Scope: "global", Kind: "rule", Content: "bridge proposed rule",
		Status: "proposed", TrustTier: "untrusted_external", Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_br_1", ItemKind: "memory_proposal", Title: "Review bridge rule", Summary: "rule", ArtifactRef: "M-br-1",
	}); err != nil {
		t.Fatalf("InsertInboxItem: %v", err)
	}
	items, err := client.ListInbox(ctx, "pending")
	if err != nil || len(items) != 1 || items[0].ID != "inb_br_1" {
		t.Fatalf("ListInbox: %+v err=%v", items, err)
	}
	actResp, err := client.ApproveInbox(ctx, "inb_br_1", "op")
	if err != nil || !actResp.OK {
		t.Fatalf("ApproveInbox: %+v err=%v", actResp, err)
	}
	node, err := st.GetMemoryNode(ctx, "M-br-1")
	if err != nil || node.Status != "active" {
		t.Fatalf("expected M-br-1 active after bridge approve, got %+v err=%v", node, err)
	}
}

func TestDiscordAdapter_Ed25519AllowlistCommandsAndAlerts(t *testing.T) {
	ctx := context.Background()
	st, _, client, _ := startTestKernelServer(t)

	var sentMu sync.Mutex
	var discordPosts []string
	mockDiscord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentMu.Lock()
		discordPosts = append(discordPosts, body["content"])
		sentMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer mockDiscord.Close()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubHex := hex.EncodeToString(pub)

	adapter := NewDiscordAdapter(client, DiscordConfig{
		BotToken:          "bot-tok",
		PublicKeyHex:      pubHex,
		APIBaseURL:        mockDiscord.URL,
		ChannelID:         "chan_1",
		AllowedUserIDs:    []string{"user_allowed"},
		AllowedChannelIDs: []string{"chan_1"},
	})

	// Unauthorized user rejected fail-closed.
	if _, _, err := adapter.HandleMessage(ctx, "user_evil", "chan_1", "!soma status"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for non-allowlisted user, got %v", err)
	}

	// Verify Ed25519 interaction PING and command interaction.
	ih := httptest.NewServer(adapter.InteractionHandler())
	defer ih.Close()

	postSigned := func(payload string, tamper bool) *http.Response {
		ts := "1700000000"
		sig := ed25519.Sign(priv, append([]byte(ts), []byte(payload)...))
		if tamper {
			sig[0] ^= 0xff
		}
		req, _ := http.NewRequest(http.MethodPost, ih.URL, strings.NewReader(payload))
		req.Header.Set("X-Signature-Timestamp", ts)
		req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(sig))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST interaction: %v", err)
		}
		return resp
	}

	respTamper := postSigned(`{"type":1}`, true)
	_ = respTamper.Body.Close()
	if respTamper.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on tampered signature, got %d", respTamper.StatusCode)
	}

	respPing := postSigned(`{"type":1}`, false)
	var pingBody map[string]int
	_ = json.NewDecoder(respPing.Body).Decode(&pingBody)
	_ = respPing.Body.Close()
	if respPing.StatusCode != http.StatusOK || pingBody["type"] != 1 {
		t.Fatalf("expected PING type 1 reply, got code=%d body=%+v", respPing.StatusCode, pingBody)
	}

	// Authorized !soma run + inbox alert + !soma approve.
	runReply, handled, err := adapter.HandleMessage(ctx, "user_allowed", "chan_1", "!soma run inspect repo")
	if err != nil || !handled || !strings.Contains(runReply, "started run") {
		t.Fatalf("HandleMessage run failed: reply=%q handled=%v err=%v", runReply, handled, err)
	}

	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_disc_1", ItemKind: "skill_patch", Title: "Review patch", Summary: "patch diff", Urgency: "urgent",
	}); err != nil {
		t.Fatalf("InsertInboxItem: %v", err)
	}
	alerts, err := adapter.PollInboxOnce(ctx)
	if err != nil || alerts != 1 {
		t.Fatalf("PollInboxOnce expected 1 alert, got %d err=%v", alerts, err)
	}
	sentMu.Lock()
	if len(discordPosts) != 1 || !strings.Contains(discordPosts[0], "!soma approve inb_disc_1") {
		t.Fatalf("unexpected discord alert posts: %+v", discordPosts)
	}
	sentMu.Unlock()

	appReply, handled, err := adapter.HandleMessage(ctx, "user_allowed", "chan_1", "!soma approve inb_disc_1")
	if err != nil || !handled || !strings.Contains(appReply, "approved") {
		t.Fatalf("HandleMessage approve failed: reply=%q handled=%v err=%v", appReply, handled, err)
	}
}

func TestTelegramAdapter_OffsetAllowlistCommandsAndAlerts(t *testing.T) {
	ctx := context.Background()
	st, _, client, _ := startTestKernelServer(t)

	var mu sync.Mutex
	var sentTexts []string
	var lastOffsetSeen string
	updatesQueue := []tgUpdate{
		{
			UpdateID: 10,
			Message: &tgMessage{
				MessageID: 1,
				From:      &struct { ID int64 "json:\"id\""; Username string "json:\"username\"" }{ID: 999, Username: "stranger"},
				Chat:      struct { ID int64 "json:\"id\"" }{ID: 55},
				Text:      "/status",
			},
		},
		{
			UpdateID: 11,
			Message: &tgMessage{
				MessageID: 2,
				From:      &struct { ID int64 "json:\"id\""; Username string "json:\"username\"" }{ID: 42, Username: "alice"},
				Chat:      struct { ID int64 "json:\"id\"" }{ID: 55},
				Text:      "/status@soma_bot",
			},
		},
		{
			UpdateID: 12,
			Message: &tgMessage{
				MessageID: 3,
				From:      &struct { ID int64 "json:\"id\""; Username string "json:\"username\"" }{ID: 42, Username: "alice"},
				Chat:      struct { ID int64 "json:\"id\"" }{ID: 55},
				Text:      "/run verify tests",
			},
		},
	}

	mockTG := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/getUpdates"):
			mu.Lock()
			lastOffsetSeen = r.URL.Query().Get("offset")
			var res []tgUpdate
			if lastOffsetSeen == "0" {
				res = updatesQueue
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(tgUpdatesResp{OK: true, Result: res})
		case strings.Contains(r.URL.Path, "/sendMessage"):
			var body struct {
				ChatID int64  `json:"chat_id"`
				Text   string `json:"text"`
			}
			_ = json.NewDecoder(bytes.NewReader(mustReadBody(r))).Decode(&body)
			mu.Lock()
			sentTexts = append(sentTexts, body.Text)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mockTG.Close()

	tg := NewTelegramAdapter(client, TelegramConfig{
		BotToken:       "tg-tok",
		APIBaseURL:     mockTG.URL,
		AllowedUserIDs: []int64{42},
		AllowedChatIDs: []int64{55},
		AlertChatID:    55,
	})

	processed, err := tg.PollUpdatesOnce(ctx)
	if err != nil || processed != 2 {
		t.Fatalf("expected 2 authorized updates processed (stranger skipped), got %d err=%v", processed, err)
	}
	if _, err := tg.PollUpdatesOnce(ctx); err != nil {
		t.Fatalf("second PollUpdatesOnce: %v", err)
	}
	mu.Lock()
	if lastOffsetSeen != "13" {
		t.Fatalf("expected monotonic offset=13, got %q", lastOffsetSeen)
	}
	if len(sentTexts) != 2 || !strings.Contains(sentTexts[0], "soma status:") || !strings.Contains(sentTexts[1], "started run") {
		t.Fatalf("unexpected telegram replies: %+v", sentTexts)
	}
	mu.Unlock()

	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_tg_1", ItemKind: "draft_pr", Title: "PR ready", Summary: "diff ready", Urgency: "normal",
	}); err != nil {
		t.Fatalf("InsertInboxItem: %v", err)
	}
	alerts, err := tg.PollInboxOnce(ctx)
	if err != nil || alerts != 1 {
		t.Fatalf("PollInboxOnce expected 1, got %d err=%v", alerts, err)
	}
}

func mustReadBody(r *http.Request) []byte {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	return buf.Bytes()
}
