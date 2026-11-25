package webui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Streamed model text reaches dashboard clients as typed SSE "delta" events.
func TestServer_SSEStreamsModelDeltas(t *testing.T) {
	srv, _, _ := newTestWebServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	req.Host = "127.0.0.1"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rd := bufio.NewReader(resp.Body)
	// Wait for the init event so the subscription is registered.
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read init: %v", err)
		}
		if strings.HasPrefix(line, "data: init") {
			break
		}
	}
	srv.publishDelta("br_x", "Hello, ")
	srv.publishDelta("br_x", "world")
	var got strings.Builder
	for !strings.Contains(got.String(), "world") {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read delta: %v (so far %q)", err, got.String())
		}
		if strings.HasPrefix(line, "event: ") && strings.TrimSpace(line) != "event: delta" && !strings.Contains(line, "state") {
			t.Fatalf("unexpected event %q", line)
		}
		if strings.HasPrefix(line, "data: {") {
			if !strings.Contains(line, `"branch_id":"br_x"`) {
				t.Fatalf("delta missing branch: %q", line)
			}
			got.WriteString(line)
		}
	}
	if !strings.Contains(got.String(), "Hello, ") {
		t.Fatalf("deltas lost: %q", got.String())
	}
}
