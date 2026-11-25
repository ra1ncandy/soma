package dag

import (
	"context"
	"regexp"
)

// Secret redaction for tool output. Credentials that appear in command output, files or web
// pages (a printed .env, a CI log, an error echoing an Authorization header) would otherwise be
// stored in @pages, indexed for session search, shown in every UI and exported in trajectories.
// They are masked before the observation is recorded anywhere.

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY(?: BLOCK)?-----[\s\S]*?-----END (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY(?: BLOCK)?-----`),
	regexp.MustCompile(`\bsk-(?:ant-|proj-|or-v1-)?[A-Za-z0-9_\-]{20,}`),                      // OpenAI / Anthropic / OpenRouter
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}\b`),                        // GitHub tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}\b`),                                    // GitHub fine-grained
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),                                       // AWS access key id
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),                                    // Slack
	regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),                                          // Google API key
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`),                                       // GitLab
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`),                                             // Hugging Face
	regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_\-]{35}\b`),                                     // Telegram bot token
	regexp.MustCompile(`(?i)\b(?:authorization:\s*)?bearer\s+[A-Za-z0-9._~+/=\-]{20,}`),       // bearer tokens
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWTs
}

// assignment of a secret-looking name to a value: API_KEY=..., "password": "..."
var secretAssignment = regexp.MustCompile(`(?i)((?:api[_-]?key|secret|password|passwd|access[_-]?token|auth[_-]?token|private[_-]?key|client[_-]?secret)["']?\s*[:=]\s*["']?)([^\s"',;]{8,})`)

// RedactSecrets masks credentials in b. It never changes text that does not look like a secret.
func RedactSecrets(b []byte) []byte {
	out := b
	for _, re := range secretPatterns {
		out = re.ReplaceAll(out, []byte("[REDACTED]"))
	}
	return secretAssignment.ReplaceAll(out, []byte("${1}[REDACTED]"))
}

// redactedTools produce output that is not a faithful copy of a workspace file. File reads are
// deliberately excluded: masking them would let the model write "[REDACTED]" back over a real
// credential when it rewrites a config file.
var redactedTools = map[string]bool{
	"exec": true, "bg_exec": true, "bg_poll": true, "bg_kill": true, "execute_code": true,
	"web_search": true, "web_extract": true, "mcp_call": true, "broker_call": true,
	"browser_task": true, "desktop_task": true, "delegate_task": true,
}

// execTool runs a tool and redacts secrets from its output before anything records it.
func (e *Executor) execTool(ctx context.Context, call ToolCall) (ToolOutput, error) {
	out, err := e.Tools.Execute(ctx, call)
	if len(out.Content) > 0 && redactedTools[call.Name] {
		out.Content = RedactSecrets(out.Content)
	}
	return out, err
}
