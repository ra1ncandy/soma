package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProviderPreset describes a built-in LLM provider matching Hermes Agent's provider catalog.
type ProviderPreset struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Category             string   `json:"category"` // "aggregator" | "cloud" | "inference" | "local" | "custom"
	BaseURL              string   `json:"base_url"`
	EnvVar               string   `json:"env_var,omitempty"`
	AltEnvVar            string   `json:"alt_env_var,omitempty"`
	DefaultModel         string   `json:"default_model,omitempty"`
	SuggestedBillingMode string   `json:"suggested_billing_mode,omitempty"` // "local" | "subscription" | "metered"
	SuggestedModels      []string `json:"suggested_models,omitempty"`
	RequiresKey          bool     `json:"requires_key"`
}

var hermesProviderCatalog = []ProviderPreset{
	{
		ID:           "openrouter",
		Name:         "OpenRouter",
		Category:     "aggregator",
		BaseURL:      "https://openrouter.ai/api/v1",
		EnvVar:       "OPENROUTER_API_KEY",
		DefaultModel: "nousresearch/hermes-3-llama-3.1-70b",
		RequiresKey:  true,
	},
	{
		ID:           "nous",
		Name:         "Nous Portal",
		Category:     "cloud",
		BaseURL:      "https://inference-api.nousresearch.com/v1",
		EnvVar:       "NOUS_API_KEY",
		DefaultModel: "Hermes-3-Llama-3.1-70B",
		RequiresKey:  true,
	},
	{
		ID:           "anthropic",
		Name:         "Anthropic",
		Category:     "cloud",
		BaseURL:      "https://api.anthropic.com/v1",
		EnvVar:       "ANTHROPIC_API_KEY",
		AltEnvVar:    "ANTHROPIC_TOKEN",
		DefaultModel: "claude-sonnet-4-20250514",
		RequiresKey:  true,
	},
	{
		ID:           "openai",
		Name:         "OpenAI",
		Category:     "cloud",
		BaseURL:      "https://api.openai.com/v1",
		EnvVar:       "OPENAI_API_KEY",
		DefaultModel: "gpt-4o",
		RequiresKey:  true,
	},
	{
		ID:           "openai-codex",
		Name:         "OpenAI Codex (OAuth / ~/.codex)",
		Category:     "cloud",
		BaseURL:      "https://chatgpt.com/backend-api/codex",
		EnvVar:       "OPENAI_CODEX_TOKEN",
		AltEnvVar:    "OPENAI_API_KEY",
		DefaultModel: "gpt-5.3-codex",
		RequiresKey:  false,
	},
	{
		ID:           "google",
		Name:         "Google Gemini",
		Category:     "cloud",
		BaseURL:      "https://generativelanguage.googleapis.com/v1beta/openai",
		EnvVar:       "GEMINI_API_KEY",
		AltEnvVar:    "GOOGLE_API_KEY",
		DefaultModel: "gemini-2.5-flash",
		RequiresKey:  true,
	},
	{
		ID:           "xai",
		Name:         "xAI (Grok)",
		Category:     "cloud",
		BaseURL:      "https://api.x.ai/v1",
		EnvVar:       "XAI_API_KEY",
		DefaultModel: "grok-3",
		RequiresKey:  true,
	},
	{
		ID:           "groq",
		Name:         "Groq",
		Category:     "inference",
		BaseURL:      "https://api.groq.com/openai/v1",
		EnvVar:       "GROQ_API_KEY",
		DefaultModel: "llama-3.3-70b-versatile",
		RequiresKey:  true,
	},
	{
		ID:           "together",
		Name:         "Together AI",
		Category:     "inference",
		BaseURL:      "https://api.together.xyz/v1",
		EnvVar:       "TOGETHER_API_KEY",
		DefaultModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo",
		RequiresKey:  true,
	},
	{
		ID:           "fireworks",
		Name:         "Fireworks AI",
		Category:     "inference",
		BaseURL:      "https://api.fireworks.ai/inference/v1",
		EnvVar:       "FIREWORKS_API_KEY",
		DefaultModel: "accounts/fireworks/models/llama-v3p3-70b-instruct",
		RequiresKey:  true,
	},
	{
		ID:           "deepseek",
		Name:         "DeepSeek",
		Category:     "inference",
		BaseURL:      "https://api.deepseek.com/v1",
		EnvVar:       "DEEPSEEK_API_KEY",
		DefaultModel: "deepseek-chat",
		RequiresKey:  true,
	},
	{
		ID:           "mistral",
		Name:         "Mistral AI",
		Category:     "inference",
		BaseURL:      "https://api.mistral.ai/v1",
		EnvVar:       "MISTRAL_API_KEY",
		DefaultModel: "mistral-large-latest",
		RequiresKey:  true,
	},
	{
		ID:           "opencode",
		Name:         "OpenCode Zen",
		Category:     "aggregator",
		BaseURL:      "https://opencode.ai/zen/go/v1",
		EnvVar:       "OPENCODE_API_KEY",
		DefaultModel: "qwen3-coder",
		RequiresKey:  true,
	},
	{
		ID:           "ollama",
		Name:         "Ollama (Local / Cloud)",
		Category:     "local",
		BaseURL:      "http://127.0.0.1:11434/v1",
		EnvVar:       "OLLAMA_API_KEY",
		DefaultModel: "hermes3:8b",
		RequiresKey:  false,
	},
	{
		ID:           "lmstudio",
		Name:         "LM Studio (Local)",
		Category:     "local",
		BaseURL:      "http://127.0.0.1:1234/v1",
		DefaultModel: "local-model",
		RequiresKey:  false,
	},
	{
		ID:           "vllm",
		Name:         "vLLM / SGLang (Local)",
		Category:     "local",
		BaseURL:      "http://127.0.0.1:8000/v1",
		EnvVar:       "VLLM_API_KEY",
		DefaultModel: "Hermes-3-Llama-3.1-8B",
		RequiresKey:  false,
	},
}

// ListProviderPresets returns all built-in Hermes-compatible AI provider presets.
func ListProviderPresets() []ProviderPreset {
	out := make([]ProviderPreset, len(hermesProviderCatalog))
	for i, p := range hermesProviderCatalog {
		if p.SuggestedBillingMode == "" {
			switch p.ID {
			case "ollama", "lmstudio", "vllm":
				p.SuggestedBillingMode = "local"
			case "openai-codex":
				p.SuggestedBillingMode = "subscription"
			default:
				p.SuggestedBillingMode = "metered"
			}
		}
		if len(p.SuggestedModels) == 0 {
			p.SuggestedModels = append([]string(nil), curatedModelsByProvider[p.ID]...)
		}
		out[i] = p
	}
	return out
}

// GetProviderPreset looks up a provider preset by ID (supporting common aliases like fw, gemini, grok).
func GetProviderPreset(id string) (ProviderPreset, bool) {
	norm := strings.ToLower(strings.TrimSpace(id))
	switch norm {
	case "fw", "fireworks-ai":
		norm = "fireworks"
	case "gemini":
		norm = "google"
	case "grok":
		norm = "xai"
	case "claude":
		norm = "anthropic"
	case "nousresearch":
		norm = "nous"
	case "codex":
		norm = "openai-codex"
	}
	for _, p := range hermesProviderCatalog {
		if p.ID == norm {
			return p, true
		}
	}
	return ProviderPreset{}, false
}

// ResolveProviderEndpoint resolves the effective baseURL, apiKey, and modelID for a provider preset.
func ResolveProviderEndpoint(providerID, explicitBase, explicitKey, explicitModel string) (baseURL, apiKey, modelID string, ok bool) {
	preset, found := GetProviderPreset(providerID)
	if !found {
		return strings.TrimSpace(explicitBase), strings.TrimSpace(explicitKey), strings.TrimSpace(explicitModel), false
	}
	baseURL = strings.TrimSpace(explicitBase)
	if baseURL == "" {
		if preset.ID == "ollama" {
			if envBase := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL")); envBase != "" {
				baseURL = strings.TrimRight(envBase, "/")
				if !strings.HasSuffix(baseURL, "/v1") {
					baseURL += "/v1"
				}
			}
		}
		if baseURL == "" {
			baseURL = preset.BaseURL
		}
	}
	apiKey = strings.TrimSpace(explicitKey)
	if apiKey == "" && preset.EnvVar != "" {
		apiKey = strings.TrimSpace(os.Getenv(preset.EnvVar))
	}
	if apiKey == "" && preset.AltEnvVar != "" {
		apiKey = strings.TrimSpace(os.Getenv(preset.AltEnvVar))
	}
	if apiKey == "" && preset.ID == "openai-codex" {
		apiKey = readCodexAuthToken()
	}
	modelID = strings.TrimSpace(explicitModel)
	if modelID == "" {
		modelID = preset.DefaultModel
	}
	return baseURL, apiKey, modelID, true
}

// FallbackProvider wraps an ordered list of ModelProviders, falling back on RateLimitError or TransientError.
type FallbackProvider struct {
	Providers []ModelProvider
}

// Generate tries each provider in order, falling back when a provider hits rate limits or transient 5xx errors.
func (f *FallbackProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	if len(f.Providers) == 0 {
		return nil, errors.New("fallback provider: no providers configured")
	}
	var lastErr error
	for _, p := range f.Providers {
		if p == nil {
			continue
		}
		resp, err := p.Generate(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var rl *RateLimitError
		var tr *TransientError
		if errors.As(err, &rl) || errors.As(err, &tr) {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func readCodexAuthToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, p := range []string{
		filepath.Join(home, ".soma", "codex_auth.json"),
		filepath.Join(home, ".codex", "auth.json"),
	} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc struct {
			OpenAIAPIKey string `json:"OPENAI_API_KEY"`
			AccessToken  string `json:"access_token"`
			Tokens       struct {
				AccessToken string `json:"access_token"`
			} `json:"tokens"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		if strings.TrimSpace(doc.Tokens.AccessToken) != "" {
			return strings.TrimSpace(doc.Tokens.AccessToken)
		}
		if strings.TrimSpace(doc.AccessToken) != "" {
			return strings.TrimSpace(doc.AccessToken)
		}
		if strings.TrimSpace(doc.OpenAIAPIKey) != "" {
			return strings.TrimSpace(doc.OpenAIAPIKey)
		}
	}
	return ""
}

// ModelListResult captures models returned by FetchProviderModels.
type ModelListResult struct {
	Models  []string `json:"models"`
	Source  string   `json:"source"` // "live" | "curated"
	Warning string   `json:"warning,omitempty"`
}

var curatedModelsByProvider = map[string][]string{
	"openrouter": {
		"nousresearch/hermes-3-llama-3.1-70b",
		"anthropic/claude-sonnet-4",
		"openai/gpt-4o",
		"google/gemini-2.5-pro",
		"deepseek/deepseek-r1",
		"qwen/qwen3-235b-a22b",
	},
	"anthropic": {
		"claude-sonnet-4-20250514",
		"claude-opus-4-20250514",
		"claude-3-7-sonnet-20250219",
		"claude-3-5-haiku-20241022",
	},
	"openai": {
		"gpt-4o",
		"gpt-4.1",
		"o3",
		"o4-mini",
		"gpt-4o-mini",
	},
	"openai-codex": {
		"gpt-5.3-codex",
		"gpt-5.2-codex",
		"codex-mini-latest",
		"gpt-5-codex",
	},
	"google": {
		"gemini-2.5-flash",
		"gemini-2.5-pro",
		"gemini-2.0-flash",
	},
	"xai": {
		"grok-3",
		"grok-3-mini",
		"grok-2-vision-1212",
	},
	"groq": {
		"llama-3.3-70b-versatile",
		"llama-3.1-8b-instant",
		"qwen-qwq-32b",
		"deepseek-r1-distill-llama-70b",
	},
	"deepseek": {
		"deepseek-chat",
		"deepseek-reasoner",
	},
	"mistral": {
		"mistral-large-latest",
		"codestral-latest",
		"mistral-small-latest",
	},
	"together": {
		"meta-llama/Llama-3.3-70B-Instruct-Turbo",
		"Qwen/Qwen2.5-Coder-32B-Instruct",
		"deepseek-ai/DeepSeek-R1",
	},
	"fireworks": {
		"accounts/fireworks/models/llama-v3p3-70b-instruct",
		"accounts/fireworks/models/deepseek-r1",
		"accounts/fireworks/models/qwen2p5-coder-32b-instruct",
	},
	"nous": {
		"Hermes-3-Llama-3.1-70B",
		"Hermes-3-Llama-3.1-405B",
		"DeepHermes-3-Llama-3-8B-Preview",
	},
	"ollama": {
		"hermes3:8b",
		"qwen3:32b",
		"qwen2.5-coder:32b",
		"llama3.3:70b",
	},
	"mock": {
		"mock-worker-v1",
		"mock-frontier-v1",
	},
}

// FetchProviderModels queries the provider's live /models (or Ollama /api/tags) endpoint,
// gracefully falling back to curated defaults if the endpoint is unreachable or unauthenticated.
func FetchProviderModels(ctx context.Context, hc *http.Client, providerID, explicitBase, explicitKey string) ModelListResult {
	norm := strings.ToLower(strings.TrimSpace(providerID))
	if preset, ok := GetProviderPreset(norm); ok {
		norm = preset.ID
	}
	if norm == "mock" {
		return ModelListResult{
			Models: append([]string(nil), curatedModelsByProvider["mock"]...),
			Source: "curated",
		}
	}

	baseURL, apiKey, defModel, _ := ResolveProviderEndpoint(norm, explicitBase, explicitKey, "")
	if hc == nil {
		hc = &http.Client{Timeout: 8 * time.Second}
	}

	var liveModels []string
	var fetchErr error

	if norm == "ollama" && baseURL != "" {
		tagsURL := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1") + "/api/tags"
		liveModels, fetchErr = queryOllamaTags(ctx, hc, tagsURL)
	}
	if len(liveModels) == 0 && baseURL != "" {
		liveModels, fetchErr = queryOpenAIModels(ctx, hc, norm, baseURL, apiKey)
	}

	if len(liveModels) > 0 {
		sort.Strings(liveModels)
		return ModelListResult{
			Models: dedupStrings(liveModels),
			Source: "live",
		}
	}

	fallback := append([]string(nil), curatedModelsByProvider[norm]...)
	if defModel != "" && !containsStr(fallback, defModel) {
		fallback = append([]string{defModel}, fallback...)
	}
	warn := ""
	if fetchErr != nil {
		warn = fetchErr.Error()
	}
	return ModelListResult{
		Models:  dedupStrings(fallback),
		Source:  "curated",
		Warning: warn,
	}
}

func queryOllamaTags(ctx context.Context, hc *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama /api/tags HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Models))
	for _, m := range doc.Models {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out, nil
}

func queryOpenAIModels(ctx context.Context, hc *http.Client, norm, baseURL, apiKey string) ([]string, error) {
	modelsURL := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	if norm == "anthropic" {
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s HTTP %d", modelsURL, resp.StatusCode)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Data))
	for _, d := range doc.Data {
		if d.ID != "" {
			out = append(out, d.ID)
		}
	}
	return out, nil
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
