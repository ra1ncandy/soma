package mcp

import (
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
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// OAuthConfig configures OAuth 2.1 PKCE authentication for a remote MCP server.
type OAuthConfig struct {
	Enabled           bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ClientID          string   `yaml:"client_id,omitempty" json:"client_id,omitempty"`
	ClientSecret      string   `yaml:"client_secret,omitempty" json:"client_secret,omitempty"`
	Scopes            []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	RedirectURI       string   `yaml:"redirect_uri,omitempty" json:"redirect_uri,omitempty"`
	AuthServerURL     string   `yaml:"auth_server_url,omitempty" json:"auth_server_url,omitempty"`
	AuthorizeEndpoint string   `yaml:"authorize_endpoint,omitempty" json:"authorize_endpoint,omitempty"`
	TokenEndpoint     string   `yaml:"token_endpoint,omitempty" json:"token_endpoint,omitempty"`
	TokenDir          string   `yaml:"token_dir,omitempty" json:"token_dir,omitempty"`
}

// OAuthToken represents a persisted OAuth 2.1 token pair stored outside prompts/configs.
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// IsExpired reports whether the access token is expired (with a 60s clock-skew margin).
func (t OAuthToken) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(60 * time.Second).After(t.ExpiresAt)
}

// AuthMetadata holds discovered OAuth 2.1 authorization & token endpoints.
type AuthMetadata struct {
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
}

// TokenStore persists OAuth tokens per MCP server with 0600 file permissions.
type TokenStore struct {
	Dir string
	mu  sync.Mutex
}

// DefaultTokenStore returns a TokenStore rooted at ~/.soma/mcp_tokens.
func DefaultTokenStore() *TokenStore {
	home, err := os.UserHomeDir()
	if err != nil {
		return &TokenStore{Dir: filepath.Join(".soma", "mcp_tokens")}
	}
	return &TokenStore{Dir: filepath.Join(home, ".soma", "mcp_tokens")}
}

func (s *TokenStore) tokenPath(serverName string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, strings.TrimSpace(serverName))
	if safe == "" {
		safe = "default"
	}
	return filepath.Join(s.Dir, safe+".json")
}

// Load reads the persisted OAuthToken for serverName.
func (s *TokenStore) Load(serverName string) (OAuthToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.tokenPath(serverName))
	if err != nil {
		return OAuthToken{}, err
	}
	var tok OAuthToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return OAuthToken{}, err
	}
	return tok, nil
}

// Save writes tok for serverName with 0600 permissions.
func (s *TokenStore) Save(serverName string, tok OAuthToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.tokenPath(serverName), raw, 0o600)
}

// Delete removes any stored token for serverName.
func (s *TokenStore) Delete(serverName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.tokenPath(serverName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// GeneratePKCE produces a cryptographically random RFC 7636 code_verifier and S256 code_challenge.
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// DiscoverAuthMetadata queries RFC 9728 / RFC 8414 well-known metadata for an MCP server URL.
func DiscoverAuthMetadata(ctx context.Context, serverURL string, httpClient *http.Client) (AuthMetadata, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return AuthMetadata{}, fmt.Errorf("oauth discover: invalid server url %q", serverURL)
	}
	origin := u.Scheme + "://" + u.Host

	// 1. Try RFC 9728 protected resource metadata to find authorization_servers.
	prmURL := origin + "/.well-known/oauth-protected-resource"
	if meta, ok := fetchProtectedResourceAuthServer(ctx, prmURL, httpClient); ok {
		if asMeta, err := fetchAuthServerMeta(ctx, meta, httpClient); err == nil {
			return asMeta, nil
		}
	}

	// 2. Fall back to RFC 8414 on the origin itself.
	return fetchAuthServerMeta(ctx, origin, httpClient)
}

func fetchProtectedResourceAuthServer(ctx context.Context, prmURL string, hc *http.Client) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prmURL, nil)
	if err != nil {
		return "", false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&prm) != nil || len(prm.AuthorizationServers) == 0 {
		return "", false
	}
	return strings.TrimRight(prm.AuthorizationServers[0], "/"), true
}

func fetchAuthServerMeta(ctx context.Context, issuer string, hc *http.Client) (AuthMetadata, error) {
	issuer = strings.TrimRight(issuer, "/")
	if meta, err := auth.GetAuthServerMetadata(ctx, issuer, hc); err == nil && meta != nil && meta.AuthorizationEndpoint != "" {
		return AuthMetadata{
			Issuer:                meta.Issuer,
			AuthorizationEndpoint: meta.AuthorizationEndpoint,
			TokenEndpoint:         meta.TokenEndpoint,
			RegistrationEndpoint:  meta.RegistrationEndpoint,
		}, nil
	}
	wellKnown := issuer + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return AuthMetadata{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return AuthMetadata{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AuthMetadata{}, fmt.Errorf("oauth metadata %s: HTTP %d", wellKnown, resp.StatusCode)
	}
	var out AuthMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return AuthMetadata{}, err
	}
	if out.AuthorizationEndpoint == "" || out.TokenEndpoint == "" {
		return AuthMetadata{}, fmt.Errorf("oauth metadata %s: missing authorization_endpoint or token_endpoint", wellKnown)
	}
	return out, nil
}

// BuildAuthorizeURL constructs an OAuth 2.1 PKCE authorization URL.
func BuildAuthorizeURL(authEndpoint string, cfg OAuthConfig, redirectURI, state, codeChallenge string) (string, error) {
	u, err := url.Parse(authEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	if cfg.ClientID != "" {
		q.Set("client_id", expandEnvVars(cfg.ClientID))
	}
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode exchanges an authorization code + PKCE code_verifier for an OAuthToken.
func ExchangeCode(ctx context.Context, tokenEndpoint string, cfg OAuthConfig, code, redirectURI, codeVerifier string, hc *http.Client) (OAuthToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {codeVerifier},
	}
	if cfg.ClientID != "" {
		form.Set("client_id", expandEnvVars(cfg.ClientID))
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", expandEnvVars(cfg.ClientSecret))
	}
	return postTokenForm(ctx, tokenEndpoint, form, hc)
}

// RefreshAccessToken exchanges a refresh_token for a new OAuthToken.
func RefreshAccessToken(ctx context.Context, tokenEndpoint string, cfg OAuthConfig, refreshToken string, hc *http.Client) (OAuthToken, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if cfg.ClientID != "" {
		form.Set("client_id", expandEnvVars(cfg.ClientID))
	}
	if cfg.ClientSecret != "" {
		form.Set("client_secret", expandEnvVars(cfg.ClientSecret))
	}
	tok, err := postTokenForm(ctx, tokenEndpoint, form, hc)
	if err != nil {
		return OAuthToken{}, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func postTokenForm(ctx context.Context, tokenEndpoint string, form url.Values, hc *http.Client) (OAuthToken, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return OAuthToken{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return OAuthToken{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return OAuthToken{}, fmt.Errorf("oauth token endpoint HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return OAuthToken{}, err
	}
	if payload.AccessToken == "" {
		return OAuthToken{}, errors.New("oauth token endpoint: empty access_token")
	}
	var exp time.Time
	if payload.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	tt := payload.TokenType
	if tt == "" {
		tt = "Bearer"
	}
	return OAuthToken{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    tt,
		Scope:        payload.Scope,
		ExpiresAt:    exp,
	}, nil
}

// BeginPKCEFlow starts a local loopback OAuth 2.1 PKCE listener and returns the authorize URL
// plus a wait function that completes the code exchange and persists the token.
func BeginPKCEFlow(ctx context.Context, ep ServerEndpoint, store *TokenStore) (authURL string, waitFn func(context.Context) (OAuthToken, error), cancelFn func(), err error) {
	if ep.OAuth == nil {
		return "", nil, nil, errors.New("oauth: endpoint has no oauth config")
	}
	if store == nil {
		store = DefaultTokenStore()
		if ep.OAuth.TokenDir != "" {
			store = &TokenStore{Dir: ep.OAuth.TokenDir}
		}
	}
	cfg := *ep.OAuth

	authEndpoint := cfg.AuthorizeEndpoint
	tokenEndpoint := cfg.TokenEndpoint
	var regEndpoint string
	if authEndpoint == "" || tokenEndpoint == "" {
		target := cfg.AuthServerURL
		if target == "" {
			target = ep.URL
		}
		meta, dErr := DiscoverAuthMetadata(ctx, target, nil)
		if dErr != nil {
			return "", nil, nil, dErr
		}
		if authEndpoint == "" {
			authEndpoint = meta.AuthorizationEndpoint
		}
		if tokenEndpoint == "" {
			tokenEndpoint = meta.TokenEndpoint
		}
		regEndpoint = meta.RegistrationEndpoint
	}

	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return "", nil, nil, err
	}
	stateBuf := make([]byte, 16)
	if _, err := rand.Read(stateBuf); err != nil {
		return "", nil, nil, err
	}
	state := base64.RawURLEncoding.EncodeToString(stateBuf)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, err
	}

	redirectURI := cfg.RedirectURI
	if redirectURI == "" {
		redirectURI = fmt.Sprintf("http://%s/callback", ln.Addr().String())
	}

	if cfg.ClientID == "" && regEndpoint != "" {
		if cid, rErr := registerDynamicClient(ctx, regEndpoint, redirectURI); rErr == nil {
			cfg.ClientID = cid
		}
	}

	authURL, err = BuildAuthorizeURL(authEndpoint, cfg, redirectURI, state, challenge)
	if err != nil {
		_ = ln.Close()
		return "", nil, nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "invalid oauth state", http.StatusBadRequest)
			select {
			case errCh <- errors.New("oauth callback: state mismatch"):
			default:
			}
			return
		}
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			http.Error(w, "oauth error: "+errParam, http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("oauth callback error: %s", errParam):
			default:
			}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			select {
			case errCh <- errors.New("oauth callback: missing code"):
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><h3>soma MCP authentication complete.</h3><p>You may close this tab and return to soma.</p><script>window.close();</script></body></html>"))
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

	wait := func(wctx context.Context) (OAuthToken, error) {
		defer cleanup()
		select {
		case <-wctx.Done():
			return OAuthToken{}, wctx.Err()
		case cbErr := <-errCh:
			return OAuthToken{}, cbErr
		case code := <-codeCh:
			tok, exErr := ExchangeCode(wctx, tokenEndpoint, cfg, code, redirectURI, verifier, nil)
			if exErr != nil {
				return OAuthToken{}, exErr
			}
			if sErr := store.Save(ep.Name, tok); sErr != nil {
				return OAuthToken{}, sErr
			}
			return tok, nil
		}
	}
	return authURL, wait, cleanup, nil
}

func registerDynamicClient(ctx context.Context, regEndpoint, redirectURI string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"client_name":                "soma",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, regEndpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth dcr HTTP %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", errors.New("oauth dcr: empty client_id")
	}
	return out.ClientID, nil
}

// StartPKCEFlow runs a local loopback OAuth 2.1 PKCE login flow for ep and persists the token to store.
func StartPKCEFlow(ctx context.Context, ep ServerEndpoint, openURL func(string) error, store *TokenStore) (OAuthToken, error) {
	authURL, waitFn, cancelFn, err := BeginPKCEFlow(ctx, ep, store)
	if err != nil {
		return OAuthToken{}, err
	}
	if openURL != nil {
		if err := openURL(authURL); err != nil {
			cancelFn()
			return OAuthToken{}, fmt.Errorf("open oauth url: %w", err)
		}
	}
	return waitFn(ctx)
}

// StoredOAuthHandler implements github.com/modelcontextprotocol/go-sdk/auth.OAuthHandler
// using soma's TokenStore and automatic refresh token rotation.
type StoredOAuthHandler struct {
	Endpoint   ServerEndpoint
	Store      *TokenStore
	HTTPClient *http.Client
	OnAuthURL  func(ctx context.Context, ep ServerEndpoint) error
}

// TokenSource returns an oauth2.TokenSource backed by the persisted TokenStore.
func (h *StoredOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	store := h.Store
	if store == nil {
		store = DefaultTokenStore()
		if h.Endpoint.OAuth != nil && h.Endpoint.OAuth.TokenDir != "" {
			store = &TokenStore{Dir: h.Endpoint.OAuth.TokenDir}
		}
	}
	tok, err := store.Load(h.Endpoint.Name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if tok.IsExpired() && tok.RefreshToken != "" && h.Endpoint.OAuth != nil {
		tokenEndpoint := h.Endpoint.OAuth.TokenEndpoint
		if tokenEndpoint == "" {
			target := h.Endpoint.OAuth.AuthServerURL
			if target == "" {
				target = h.Endpoint.URL
			}
			if meta, dErr := DiscoverAuthMetadata(ctx, target, h.HTTPClient); dErr == nil {
				tokenEndpoint = meta.TokenEndpoint
			}
		}
		if tokenEndpoint != "" {
			if refreshed, rErr := RefreshAccessToken(ctx, tokenEndpoint, *h.Endpoint.OAuth, tok.RefreshToken, h.HTTPClient); rErr == nil {
				tok = refreshed
				_ = store.Save(h.Endpoint.Name, tok)
			}
		}
	}
	return oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.ExpiresAt,
	}), nil
}

// Authorize is invoked by StreamableClientTransport on 401/403 responses.
func (h *StoredOAuthHandler) Authorize(ctx context.Context, _ *http.Request, resp *http.Response) error {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if h.OnAuthURL != nil {
		return h.OnAuthURL(ctx, h.Endpoint)
	}
	return fmt.Errorf("mcp server %q requires OAuth authorization (run `soma mcp auth %s`)", h.Endpoint.Name, h.Endpoint.Name)
}
