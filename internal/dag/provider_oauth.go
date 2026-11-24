package dag

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	openRouterAuthBase  = "https://openrouter.ai"
	codexOAuthClientID  = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthAuthorize = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL  = "https://auth.openai.com/oauth/token"
)

func generateProviderPKCE() (verifier, challenge, state string, err error) {
	vBuf := make([]byte, 32)
	if _, err = rand.Read(vBuf); err != nil {
		return "", "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(vBuf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])

	sBuf := make([]byte, 16)
	if _, err = rand.Read(sBuf); err != nil {
		return "", "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(sBuf)
	return verifier, challenge, state, nil
}

// BeginOpenRouterPKCE starts a local loopback listener for OpenRouter's official PKCE OAuth flow
// and returns the authorize URL plus a wait function returning the provisioned OpenRouter API key.
func BeginOpenRouterPKCE(httpClient *http.Client) (authURL string, waitFn func(context.Context) (string, error), cancelFn func(), err error) {
	return beginOpenRouterPKCEWithBase(openRouterAuthBase, httpClient)
}

func beginOpenRouterPKCEWithBase(baseURL string, httpClient *http.Client) (authURL string, waitFn func(context.Context) (string, error), cancelFn func(), err error) {
	verifier, challenge, _, err := generateProviderPKCE()
	if err != nil {
		return "", nil, nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}
	callbackURL := fmt.Sprintf("http://%s/callback", ln.Addr().String())
	u, _ := url.Parse(strings.TrimRight(baseURL, "/") + "/auth")
	q := u.Query()
	q.Set("callback_url", callbackURL)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			select {
			case errCh <- errors.New("openrouter oauth: missing code"):
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><h3>OpenRouter connected to soma.</h3><p>You may close this tab.</p><script>window.close();</script></body></html>"))
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	cleanup := func() {
		_ = srv.Shutdown(context.Background())
		_ = ln.Close()
	}

	wait := func(ctx context.Context) (string, error) {
		defer cleanup()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case cbErr := <-errCh:
			return "", cbErr
		case code := <-codeCh:
			payload, _ := json.Marshal(map[string]string{
				"code":                  code,
				"code_verifier":         verifier,
				"code_challenge_method": "S256",
			})
			exURL := strings.TrimRight(baseURL, "/") + "/api/v1/auth/keys"
			req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, exURL, bytes.NewReader(payload))
			if rErr != nil {
				return "", rErr
			}
			req.Header.Set("Content-Type", "application/json")
			hc := httpClient
			if hc == nil {
				hc = &http.Client{Timeout: 15 * time.Second}
			}
			resp, dErr := hc.Do(req)
			if dErr != nil {
				return "", dErr
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return "", fmt.Errorf("openrouter key exchange HTTP %d: %s", resp.StatusCode, string(raw))
			}
			var res struct {
				Key string `json:"key"`
			}
			if uErr := json.Unmarshal(raw, &res); uErr != nil || res.Key == "" {
				return "", fmt.Errorf("openrouter key exchange: invalid response")
			}
			return res.Key, nil
		}
	}
	return u.String(), wait, cleanup, nil
}

// BeginCodexPKCE starts a local loopback listener for OpenAI Codex OAuth PKCE
// and persists the resulting token to ~/.soma/codex_auth.json.
func BeginCodexPKCE(httpClient *http.Client) (authURL string, waitFn func(context.Context) (string, error), cancelFn func(), err error) {
	verifier, challenge, state, err := generateProviderPKCE()
	if err != nil {
		return "", nil, nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", nil, nil, err
		}
	}
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", ln.Addr().(*net.TCPAddr).Port)
	u, _ := url.Parse(codexOAuthAuthorize)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", codexOAuthClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	u.RawQuery = q.Encode()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			select {
			case errCh <- errors.New("codex oauth: state mismatch"):
			default:
			}
			return
		}
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			http.Error(w, "oauth error: "+errParam, http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("codex oauth: %s", errParam):
			default:
			}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			select {
			case errCh <- errors.New("codex oauth: missing code"):
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><h3>OpenAI Codex connected to soma.</h3><p>You may close this tab.</p><script>window.close();</script></body></html>"))
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	cleanup := func() {
		_ = srv.Shutdown(context.Background())
		_ = ln.Close()
	}

	wait := func(ctx context.Context) (string, error) {
		defer cleanup()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case cbErr := <-errCh:
			return "", cbErr
		case code := <-codeCh:
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {codexOAuthClientID},
				"code":          {code},
				"code_verifier": {verifier},
				"redirect_uri":  {redirectURI},
			}
			req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
			if rErr != nil {
				return "", rErr
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			hc := httpClient
			if hc == nil {
				hc = &http.Client{Timeout: 15 * time.Second}
			}
			resp, dErr := hc.Do(req)
			if dErr != nil {
				return "", dErr
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return "", fmt.Errorf("codex token exchange HTTP %d: %s", resp.StatusCode, string(raw))
			}
			var tok struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int64  `json:"expires_in"`
			}
			if uErr := json.Unmarshal(raw, &tok); uErr != nil || tok.AccessToken == "" {
				return "", errors.New("codex token exchange: empty access_token")
			}
			_ = saveSomaCodexAuth(raw)
			return tok.AccessToken, nil
		}
	}
	return u.String(), wait, cleanup, nil
}

func saveSomaCodexAuth(raw []byte) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".soma")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "codex_auth.json"), raw, 0o600)
}
