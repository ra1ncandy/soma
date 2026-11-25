package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A page that DNS-rebinds its own hostname to 127.0.0.1 still sends its own Host header; the
// token-less dashboard must refuse it.
func TestServer_RejectsNonLoopbackHostWithoutToken(t *testing.T) {
	srv, _, _ := newTestWebServer(t, nil)
	h := srv.Handler()

	for host, want := range map[string]int{
		"evil.example:7420": http.StatusForbidden,
		"127.0.0.1:7420":    http.StatusOK,
		"localhost:7420":    http.StatusOK,
		"[::1]:7420":        http.StatusOK,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, rec.Code, want)
		}
	}
}

func TestServer_SessionCookieDoesNotContainToken(t *testing.T) {
	const token = "correct-horse-battery-staple-token"
	if got := sessionValueFor(token); got == token || strings.Contains(got, token) || len(got) != 64 {
		t.Fatalf("session value must be a digest, got %q", got)
	}
	srv, _, _ := newTestWebServer(t, func(c *ServerConfig) { c.AuthToken = token })
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.AddCookie(&http.Cookie{Name: "soma_session", Value: sessionValueFor(token)})
	if !srv.validAuth(req) {
		t.Fatal("derived session cookie should authenticate")
	}
	raw := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	raw.AddCookie(&http.Cookie{Name: "soma_session", Value: token})
	if srv.validAuth(raw) {
		t.Fatal("raw token as cookie value must no longer authenticate")
	}
}

func TestLoginThrottleLocksAfterRepeatedFailures(t *testing.T) {
	var lt loginThrottle
	now := time.Now()
	for i := 0; i < 5; i++ {
		if ok, _ := lt.allowed(now); !ok {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		lt.record(false, now)
	}
	if ok, wait := lt.allowed(now); ok || wait <= 0 {
		t.Fatal("expected lockout after 5 consecutive failures")
	}
	if ok, _ := lt.allowed(now.Add(10 * time.Minute)); !ok {
		t.Fatal("lockout should expire")
	}
	lt.record(true, now)
	if ok, _ := lt.allowed(now); !ok {
		t.Fatal("successful login should clear the lockout")
	}
}
