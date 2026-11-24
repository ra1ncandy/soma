package mcp

import (
	"net/http"
	"os"
	"path"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HermesToolName formats a server/tool pair using Hermes's collision-free mcp__<server>__<tool> convention.
func HermesToolName(server, tool string) string {
	return "mcp__" + sanitizeNameSegment(server) + "__" + sanitizeNameSegment(tool)
}

func sanitizeNameSegment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// ParseToolName parses either "mcp__<server>__<tool>" or "<server>.<tool>" into (server, tool, true).
func ParseToolName(qualified string) (server, tool string, ok bool) {
	q := strings.TrimSpace(qualified)
	if strings.HasPrefix(q, "mcp__") {
		rest := strings.TrimPrefix(q, "mcp__")
		idx := strings.Index(rest, "__")
		if idx > 0 && idx+2 < len(rest) {
			return rest[:idx], rest[idx+2:], true
		}
		return "", "", false
	}
	if idx := strings.IndexByte(q, '.'); idx > 0 && idx+1 < len(q) {
		return q[:idx], q[idx+1:], true
	}
	return "", "", false
}

// IsToolAllowed checks whether toolName passes include (allowlist when non-empty) and exclude (denylist) patterns.
// Supports exact matches and shell-style glob patterns (*, ?). Exclude always takes precedence over include.
func IsToolAllowed(toolName string, include, exclude []string) bool {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return false
	}
	for _, pat := range exclude {
		if matchToolPattern(pat, name) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, pat := range include {
		if matchToolPattern(pat, name) {
			return true
		}
	}
	return false
}

// FilterTools returns only tools permitted by include/exclude rules.
func FilterTools(tools []sdkmcp.Tool, include, exclude []string) []sdkmcp.Tool {
	if len(include) == 0 && len(exclude) == 0 {
		return append([]sdkmcp.Tool(nil), tools...)
	}
	out := make([]sdkmcp.Tool, 0, len(tools))
	for _, t := range tools {
		if IsToolAllowed(t.Name, include, exclude) {
			out = append(out, t)
		}
	}
	return out
}

func matchToolPattern(pattern, name string) bool {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return false
	}
	if p == name {
		return true
	}
	matched, err := path.Match(p, name)
	return err == nil && matched
}

// expandEnvVars expands ${VAR} and $VAR references using process environment variables.
func expandEnvVars(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.ExpandEnv(s)
}

// ResolveHeaders returns ep.Headers with ${VAR} environment variable references expanded.
func ResolveHeaders(ep ServerEndpoint) map[string]string {
	if len(ep.Headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(ep.Headers))
	for k, v := range ep.Headers {
		out[k] = expandEnvVars(v)
	}
	return out
}

// ResolveEnv returns ep.Env with ${VAR} environment variable references expanded.
func ResolveEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = expandEnvVars(v)
	}
	return out
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
	bearer  string
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range rt.headers {
		if clone.Header.Get(k) == "" {
			clone.Header.Set(k, v)
		}
	}
	if rt.bearer != "" && clone.Header.Get("Authorization") == "" {
		clone.Header.Set("Authorization", "Bearer "+rt.bearer)
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// NewEndpointHTTPClient returns an *http.Client configured with ep's timeout, AuthToken, and expanded Headers.
func NewEndpointHTTPClient(ep ServerEndpoint, base *http.Client) *http.Client {
	var baseRT http.RoundTripper
	timeout := ep.EffectiveTimeout()
	if base != nil {
		baseRT = base.Transport
		if base.Timeout > 0 && ep.TimeoutSeconds == 0 {
			timeout = base.Timeout
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &headerRoundTripper{
			base:    baseRT,
			headers: ResolveHeaders(ep),
			bearer:  expandEnvVars(ep.AuthToken),
		},
	}
}
