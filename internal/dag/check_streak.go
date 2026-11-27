package dag

import "strings"

// observationFailed reports whether a tool result shows the call did not succeed: a tool or
// policy error, or a command that exited non-zero or timed out.
func observationFailed(obs string) bool {
	obs = strings.TrimSpace(obs)
	if strings.HasPrefix(obs, "TOOL ERROR:") || strings.HasPrefix(obs, "POLICY DENY:") {
		return true
	}
	if rest, ok := strings.CutPrefix(obs, "exit_code="); ok {
		return rest != "0" && !strings.HasPrefix(rest, "0\n") && !strings.HasPrefix(rest, "0 ")
	}
	return false
}
