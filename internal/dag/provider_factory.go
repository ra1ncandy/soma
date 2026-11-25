package dag

import (
	"net/url"
	"strings"
)

// Wire protocols a provider endpoint can speak.
const (
	ProtocolOpenAIChat = "openai_chat"
	ProtocolAnthropic  = "anthropic_messages"
)

// ProtocolForEndpoint picks the native protocol for a base URL. Anthropic's own API is spoken
// natively (cache breakpoints, tool_use blocks) unless the URL points at its OpenAI-compatible path.
func ProtocolForEndpoint(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ProtocolOpenAIChat
	}
	host := strings.ToLower(u.Hostname())
	if host == "api.anthropic.com" && !strings.Contains(u.Path, "openai") {
		return ProtocolAnthropic
	}
	return ProtocolOpenAIChat
}

// NewProvider is the single constructor every surface uses for an endpoint.
func NewProvider(baseURL, apiKey string) ModelProvider {
	switch ProtocolForEndpoint(baseURL) {
	case ProtocolAnthropic:
		return NewAnthropicProvider(baseURL, apiKey)
	default:
		return NewOpenAICompatProvider(baseURL, apiKey)
	}
}
