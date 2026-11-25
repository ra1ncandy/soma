package daemon

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With no webhook_secret configured the webhook must not be open: a per-project secret is
// generated and required.
func TestWebhookRequiresSecretEvenWhenNoneConfigured(t *testing.T) {
	st := openTestDaemonStore(t)
	cfg := DefaultConfig()
	cfg.Triggers = []TriggerSpec{{ID: "hook", Type: "webhook", Debounce: "10ms"}}
	d := New(st, cfg, t.TempDir())
	fired := make(chan struct{}, 2)
	d.Handler = func(_ context.Context, _ TriggerSpec, _ CoalescedEvent, _ string) (string, float64, error) {
		fired <- struct{}{}
		return "", 0, nil
	}
	ts := httptest.NewServer(d.WebhookHandler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/webhook/hook", "application/json", bytes.NewReader([]byte(`{"a":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: status %d, want 401", resp.StatusCode)
	}

	raw, err := os.ReadFile(filepath.Join(st.SomaDir(), "webhook_secret"))
	if err != nil {
		t.Fatalf("generated secret file: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhook/hook", bytes.NewReader([]byte(`{"a":1}`)))
	req.Header.Set("X-Soma-Webhook-Secret", strings.TrimSpace(string(raw)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook with generated secret: status %d, want 202", resp.StatusCode)
	}
}
