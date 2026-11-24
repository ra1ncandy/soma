package dag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type failingProvider struct {
	err error
}

func (f failingProvider) Generate(_ context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	return nil, f.err
}

func TestProviderCatalogAndFallback(t *testing.T) {
	presets := ListProviderPresets()
	if len(presets) < 15 {
		t.Fatalf("expected >=15 Hermes provider presets, got %d", len(presets))
	}

	// Alias resolution (fw -> fireworks, gemini -> google)
	if p, ok := GetProviderPreset("fw"); !ok || p.ID != "fireworks" {
		t.Fatalf("expected alias fw->fireworks, got %+v ok=%v", p, ok)
	}
	if p, ok := GetProviderPreset("gemini"); !ok || p.ID != "google" {
		t.Fatalf("expected alias gemini->google, got %+v ok=%v", p, ok)
	}
	if p, ok := GetProviderPreset("codex"); !ok || p.ID != "openai-codex" {
		t.Fatalf("expected alias codex->openai-codex, got %+v ok=%v", p, ok)
	}

	t.Setenv("OPENROUTER_API_KEY", "or-env-key")
	base, key, model, ok := ResolveProviderEndpoint("openrouter", "", "", "")
	if !ok || base != "https://openrouter.ai/api/v1" || key != "or-env-key" || model == "" {
		t.Fatalf("ResolveProviderEndpoint(openrouter) = (%q, %q, %q, %v)", base, key, model, ok)
	}

	// FallbackProvider falls back on RateLimitError to MockProvider.
	fb := &FallbackProvider{
		Providers: []ModelProvider{
			failingProvider{err: &RateLimitError{AccountID: "a1", Err: errors.New("429")}},
			&MockProvider{ModelName: "fallback-mock"},
		},
	}
	resp, err := fb.Generate(context.Background(), GenerateRequest{TurnIndex: 0})
	if err != nil || resp == nil {
		t.Fatalf("FallbackProvider failed: err=%v resp=%+v", err, resp)
	}
}

func TestBeginOpenRouterPKCEWithBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/keys" && r.Method == http.MethodPost {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] == "or-code-99" && body["code_verifier"] != "" {
				_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-or-v1-provisioned"})
				return
			}
		}
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	authURL, waitFn, cancelFn, err := beginOpenRouterPKCEWithBase(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("beginOpenRouterPKCEWithBase: %v", err)
	}
	defer cancelFn()

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	cb := u.Query().Get("callback_url")
	if cb == "" {
		t.Fatalf("expected callback_url in %s", authURL)
	}

	go func() {
		resp, _ := http.Get(cb + "?code=or-code-99")
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()

	key, err := waitFn(context.Background())
	if err != nil || key != "sk-or-v1-provisioned" {
		t.Fatalf("waitFn failed: err=%v key=%q", err, key)
	}
}

func TestFetchProviderModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer sk-test" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "gpt-4o"}, {"id": "o3"}},
			})
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]string{{"name": "hermes3:8b"}, {"name": "qwen3:32b"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	live := FetchProviderModels(context.Background(), srv.Client(), "openai", srv.URL+"/v1", "sk-test")
	if live.Source != "live" || len(live.Models) != 2 {
		t.Fatalf("expected live openai models, got %+v", live)
	}

	ollamaLive := FetchProviderModels(context.Background(), srv.Client(), "ollama", srv.URL+"/v1", "")
	if ollamaLive.Source != "live" || len(ollamaLive.Models) != 2 {
		t.Fatalf("expected live ollama models, got %+v", ollamaLive)
	}

	curated := FetchProviderModels(context.Background(), srv.Client(), "anthropic", srv.URL+"/bad", "")
	if curated.Source != "curated" || len(curated.Models) == 0 {
		t.Fatalf("expected curated fallback, got %+v", curated)
	}
}
