package dag

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	in := strings.Join([]string{
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz0123",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"aws AKIAIOSFODNN7EXAMPLE",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz.0123456789",
		`{"password": "hunter2hunter2"}`,
		"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3NzaC1yc2E\n-----END OPENSSH PRIVATE KEY-----",
		"ordinary text: tests passed in 0.42s, commit 9f86d081884c7d659a2feaa0",
	}, "\n")
	out := string(RedactSecrets([]byte(in)))
	for _, leak := range []string{"sk-proj-abc", "ghp_abc", "AKIAIOSFODNN7EXAMPLE", "abcdefghijklmnopqrstuvwxyz.0123456789", "hunter2hunter2", "AAAAB3NzaC1yc2E"} {
		if strings.Contains(out, leak) {
			t.Errorf("secret %q not redacted:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "tests passed in 0.42s, commit 9f86d081884c7d659a2feaa0") {
		t.Errorf("ordinary text was changed:\n%s", out)
	}
	if !strings.Contains(out, "OPENAI_API_KEY=[REDACTED]") {
		t.Errorf("assignment form not masked as expected:\n%s", out)
	}
}
