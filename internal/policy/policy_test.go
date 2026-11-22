package policy

import (
	"testing"

	"github.com/ra1ncandy/soma/internal/taint"
)

func TestPolicyPrecedenceDenyOverAskOverAllow(t *testing.T) {
	eng := NewEngine(
		Rule{ID: "allow-github", Effect: taint.DecisionAllow, Capability: "http", OpPattern: "POST", TargetPattern: "api.github.com/repos/*"},
		Rule{ID: "ask-all-post", Effect: taint.DecisionAsk, Capability: "http", OpPattern: "POST", TargetPattern: "*"},
		Rule{ID: "deny-prod-ssh", Effect: taint.DecisionDeny, Capability: "ssh", OpPattern: "*", TargetPattern: "prod*"},
		Rule{ID: "allow-ssh-all", Effect: taint.DecisionAllow, Capability: "ssh", OpPattern: "exec", TargetPattern: "*"},
	)

	// Allow + Ask matching -> Ask wins.
	dec, _ := eng.Evaluate(Request{Capability: "http", Op: "POST", Target: "api.github.com/repos/ra1ncandy/soma/pulls"})
	if dec != taint.DecisionAsk {
		t.Fatalf("expected Ask to override Allow, got %s", dec)
	}

	// Deny + Allow matching -> Deny wins.
	dec2, _ := eng.Evaluate(Request{Capability: "ssh", Op: "exec", Target: "prod-db-1"})
	if dec2 != taint.DecisionDeny {
		t.Fatalf("expected Deny to override Allow, got %s", dec2)
	}

	// Only Allow matching -> Allow.
	dec3, _ := eng.Evaluate(Request{Capability: "ssh", Op: "exec", Target: "gpu-node-01"})
	if dec3 != taint.DecisionAllow {
		t.Fatalf("expected Allow for gpu-node-01, got %s", dec3)
	}

	// No match -> default Ask.
	dec4, _ := eng.Evaluate(Request{Capability: "fs", Op: "read", Target: "/etc/passwd"})
	if dec4 != taint.DecisionAsk {
		t.Fatalf("expected default Ask on unmatched request, got %s", dec4)
	}
}
