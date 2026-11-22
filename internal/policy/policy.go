package policy

import (
	"fmt"
	"strings"

	"github.com/ra1ncandy/soma/internal/taint"
)

// Rule defines a deterministic capability policy rule.
type Rule struct {
	ID            string
	Effect        taint.PolicyDecision // deny | ask | allow
	Capability    string               // e.g. "http", "ssh", "fs", "*"
	OpPattern     string               // e.g. "POST", "GET", "exec", "*"
	TargetPattern string               // e.g. "api.github.com/repos/*", "*"
}

// Request describes a broker or capability operation subject to policy evaluation.
type Request struct {
	Capability string
	Op         string
	Target     string
}

// Engine evaluates policy rules with strict precedence: deny > ask > allow (default ask).
type Engine struct {
	Rules []Rule
}

// NewEngine constructs an Engine from rules.
func NewEngine(rules ...Rule) *Engine {
	return &Engine{Rules: rules}
}

// Evaluate checks all matching rules and resolves deny > ask > allow, defaulting to ask.
func (e *Engine) Evaluate(req Request) (taint.PolicyDecision, string) {
	var matchedAsk *Rule
	var matchedAllow *Rule

	for i := range e.Rules {
		r := &e.Rules[i]
		if !matchGlob(r.Capability, req.Capability) {
			continue
		}
		if !matchGlob(r.OpPattern, req.Op) {
			continue
		}
		if !matchGlob(r.TargetPattern, req.Target) {
			continue
		}
		switch r.Effect {
		case taint.DecisionDeny:
			return taint.DecisionDeny, fmt.Sprintf("denied by rule %s", r.ID)
		case taint.DecisionAsk:
			if matchedAsk == nil {
				matchedAsk = r
			}
		case taint.DecisionAllow:
			if matchedAllow == nil {
				matchedAllow = r
			}
		}
	}

	if matchedAsk != nil {
		return taint.DecisionAsk, fmt.Sprintf("approval required by rule %s", matchedAsk.ID)
	}
	if matchedAllow != nil {
		return taint.DecisionAllow, fmt.Sprintf("allowed by rule %s", matchedAllow.ID)
	}
	return taint.DecisionAsk, "no matching allow rule (default ask)"
}

func matchGlob(pattern, value string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	return wildcardMatch(strings.ToLower(pattern), strings.ToLower(value))
}

func wildcardMatch(pattern, s string) bool {
	px, sx := 0, 0
	starIdx, matchIdx := -1, 0
	for sx < len(s) {
		if px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]) {
			px++
			sx++
		} else if px < len(pattern) && pattern[px] == '*' {
			starIdx = px
			matchIdx = sx
			px++
		} else if starIdx != -1 {
			px = starIdx + 1
			matchIdx++
			sx = matchIdx
		} else {
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}
