package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

type mockChannelAdapter struct {
	name     string
	outbound []OutboundMessage
}

func (m *mockChannelAdapter) Name() string { return m.name }
func (m *mockChannelAdapter) Send(_ context.Context, msg OutboundMessage) error {
	m.outbound = append(m.outbound, msg)
	return nil
}

func TestPairingStoreLifecycle(t *testing.T) {
	ps := NewPairingStore(t.TempDir())
	if ps.IsApproved("slack", "U123") {
		t.Fatal("expected unapproved sender initially")
	}
	req1, err := ps.IssueCode("slack", "U123", "alice")
	if err != nil || req1.Code == "" {
		t.Fatalf("IssueCode: err=%v req=%+v", err, req1)
	}
	req2, err := ps.IssueCode("slack", "U123", "alice")
	if err != nil || req2.Code != req1.Code {
		t.Fatalf("expected idempotent pending code, got %q vs %q", req2.Code, req1.Code)
	}

	pending, err := ps.ListPending()
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending: err=%v pending=%+v", err, pending)
	}

	paired, err := ps.ApproveCode(req1.Code)
	if err != nil || paired.SenderID != "U123" || paired.Channel != "slack" {
		t.Fatalf("ApproveCode: err=%v paired=%+v", err, paired)
	}
	if !ps.IsApproved("slack", "U123") {
		t.Fatal("expected sender to be approved after ApproveCode")
	}

	ok, err := ps.Revoke("slack", "U123")
	if err != nil || !ok || ps.IsApproved("slack", "U123") {
		t.Fatalf("Revoke failed: ok=%v err=%v", ok, err)
	}
}

func TestGatewayInboundPairingSessionAndBroadcast(t *testing.T) {
	ws := t.TempDir()
	somaDir := filepath.Join(ws, ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ks, err := NewKernelServer(st, ServerConfig{
		SomaDir:      somaDir,
		WorkspaceDir: ws,
		RunStarter: func(_ context.Context, req StartRunRequest) (StartRunResponse, error) {
			return StartRunResponse{
				RunID:    "run-42",
				BranchID: "br-1",
				Status:   "completed",
				Summary:  "done: " + req.Goal,
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ks.Start(ctx) }()

	// Wait briefly by polling Health.
	client := NewClient(ks.SocketPath(), ks.Token())
	var healthy bool
	for i := 0; i < 50; i++ {
		if h, hErr := client.Health(ctx); hErr == nil && h.OK {
			healthy = true
			break
		}
	}
	if !healthy {
		t.Fatal("kernel server did not become healthy")
	}

	gw := NewGateway(client, somaDir)

	// Non-DM unauthorized -> ErrUnauthorized
	if _, err := gw.HandleInbound(ctx, InboundMessage{
		Channel:  "slack",
		SenderID: "U999",
		Target:   "C1",
		Text:     "!soma status",
		IsDM:     false,
	}); err == nil {
		t.Fatal("expected ErrUnauthorized for non-DM")
	}

	// DM unauthorized -> returns pairing prompt
	prompt, err := gw.HandleInbound(ctx, InboundMessage{
		Channel:    "slack",
		SenderID:   "U999",
		SenderName: "bob",
		Target:     "D999",
		Text:       "hello",
		IsDM:       true,
	})
	if err != nil || !strings.Contains(prompt, "soma bridge pair ") {
		t.Fatalf("expected pairing prompt, got err=%v prompt=%q", err, prompt)
	}
	pending, _ := gw.Pairing().ListPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending pairing, got %d", len(pending))
	}
	if _, err := gw.Pairing().ApproveCode(pending[0].Code); err != nil {
		t.Fatal(err)
	}

	// Now DM runs work and populate unified session.
	reply, err := gw.HandleInbound(ctx, InboundMessage{
		Channel:  "slack",
		SenderID: "U999",
		Target:   "D999",
		Text:     "summarize repo",
		IsDM:     true,
	})
	if err != nil || reply != "done: summarize repo" {
		t.Fatalf("HandleInbound after pairing: err=%v reply=%q", err, reply)
	}

	sessReply, err := gw.HandleInbound(ctx, InboundMessage{
		Channel:  "slack",
		SenderID: "U999",
		Target:   "D999",
		Text:     "!soma session",
		IsDM:     true,
	})
	if err != nil || !strings.Contains(sessReply, "run=run-42") {
		t.Fatalf("expected session run-42, got err=%v reply=%q", err, sessReply)
	}

	// Broadcast to multiple adapters.
	slackAdp := &mockChannelAdapter{name: "slack"}
	emailAdp := &mockChannelAdapter{name: "email"}
	gw.RegisterAdapter(slackAdp)
	gw.RegisterAdapter(emailAdp)

	err = gw.Broadcast(ctx, []OutboundMessage{
		{Channel: "slack", Target: "#alerts"},
		{Channel: "email", Target: "ops@example.com"},
	}, "cron job finished")
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(slackAdp.outbound) != 1 || len(emailAdp.outbound) != 1 {
		t.Fatalf("expected 1 message per adapter, got slack=%d email=%d", len(slackAdp.outbound), len(emailAdp.outbound))
	}
}
