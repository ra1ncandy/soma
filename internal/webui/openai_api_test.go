package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/dag"
)

type rememberingProvider struct{}

func (rememberingProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	all := req.Frame.Layer0 + req.Frame.Layer2
	answer := "I don't know your name."
	if strings.Contains(all, "I am Grace") {
		answer = "Your name is Grace."
	}
	return &dag.GenerateResponse{Done: true, FinalSummary: answer, AssistantContent: answer, StateHash: "x"}, nil
}

func TestOpenAICompatibleAPI(t *testing.T) {
	srv, _, _ := newTestWebServer(t, func(c *ServerConfig) {
		c.Provider = rememberingProvider{}
		c.UseMock = false
	})
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	call := func(key, session, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Host = "127.0.0.1"
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if session != "" {
			req.Header.Set("X-Soma-Session", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp, m
	}

	// Disabled without a configured key (fail closed).
	if resp, _ := call("x", "", `{"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("API without a configured key returned %d, want 403", resp.StatusCode)
	}
	t.Setenv("SOMA_API_KEY_SERVER", "k-test")
	if resp, _ := call("wrong", "", `{"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key returned %d", resp.StatusCode)
	}
	resp, body := call("k-test", "", `{"model":"soma","messages":[{"role":"user","content":"I am Grace"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	session := resp.Header.Get("X-Soma-Session")
	if session == "" {
		t.Fatal("no X-Soma-Session header")
	}
	// Follow-up continues the same branch: the earlier message is in context.
	_, body = call("k-test", session, `{"messages":[{"role":"user","content":"What is my name?"}]}`)
	msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if msg != "Your name is Grace." {
		t.Fatalf("follow-up lost the conversation: %v", body)
	}
}
