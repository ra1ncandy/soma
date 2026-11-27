package dag

import (
	"net/url"
	"strings"
)

// Wire protocols a provider endpoint can speak.
const (
	ProtocolOpenAIChat      = "openai_chat"
	ProtocolAnthropic       = "anthropic_messages"
	ProtocolOpenAIResponses = "openai_responses"
)

// ProtocolForEndpoint picks the native protocol for a base URL. Anthropic's own API is spoken
// natively (cache breakpoints, tool_use blocks) unless the URL points at its OpenAI-compatible path.
func ProtocolForEndpoint(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ProtocolOpenAIChat
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "api.anthropic.com" && !strings.Contains(u.Path, "openai"):
		return ProtocolAnthropic
	case host == "chatgpt.com" && strings.Contains(u.Path, "/codex"):
		// The ChatGPT/Codex subscription backend only speaks the Responses API.
		return ProtocolOpenAIResponses
	case strings.Contains(u.Fragment, "responses"):
		// Opt in for any endpoint by appending #responses to its base URL.
		return ProtocolOpenAIResponses
	case strings.Contains(u.Fragment, "anthropic"):
		// Likewise #anthropic, for a proxy or gateway in front of the Anthropic Messages API.
		return ProtocolAnthropic
	}
	return ProtocolOpenAIChat
}

// NewProvider is the single constructor every surface uses for an endpoint.
func NewProvider(baseURL, apiKey string) ModelProvider {
	protocol := ProtocolForEndpoint(baseURL)
	base := baseURL
	if i := strings.Index(base, "#"); i >= 0 {
		base = base[:i] // the fragment only selects the protocol
	}
	switch protocol {
	case ProtocolAnthropic:
		return NewAnthropicProvider(base, apiKey)
	case ProtocolOpenAIResponses:
		return NewResponsesProvider(base, apiKey)
	default:
		return NewOpenAICompatProvider(baseURL, apiKey)
	}
}
