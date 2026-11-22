package taint

import (
	"context"
	"fmt"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// EffectClass defines the risk tier of a tool or broker invocation.
type EffectClass string

const (
	EffectReadOnly       EffectClass = "read_only"
	EffectWorkspaceWrite EffectClass = "workspace_write"
	EffectExternalWrite  EffectClass = "external_write"
	EffectIrreversible   EffectClass = "irreversible_write"
)

// PolicyDecision is the deterministic policy outcome before dispatching an action.
type PolicyDecision string

const (
	DecisionAllow PolicyDecision = "allow"
	DecisionAsk   PolicyDecision = "ask"
	DecisionDeny  PolicyDecision = "deny"
)

// ActionPolicyInput describes the exact data provenance and scope of a proposed action.
type ActionPolicyInput struct {
	ToolName          string
	CapabilityScope   string
	EffectClass       EffectClass
	ReferencedPageIDs []string
	ExplicitDeny      bool
}

// EvaluateAction applies data/action-level taint provenance rather than branch-wide lockdowns:
// - Explicit deny always wins.
// - Irreversible actions always require operator approval (DecisionAsk).
// - Isolated workspace read/write stays autonomous even after reading untrusted_external pages.
// - External writes block with DecisionAsk if any ReferencedPageID is untrusted_external and
//   lacks a matching scoped declassification.
func EvaluateAction(ctx context.Context, st *store.Store, in ActionPolicyInput) (PolicyDecision, string, error) {
	if in.ExplicitDeny {
		return DecisionDeny, fmt.Sprintf("tool %s denied by explicit policy", in.ToolName), nil
	}
	if in.EffectClass == EffectIrreversible {
		return DecisionAsk, fmt.Sprintf("irreversible action %s requires operator approval", in.ToolName), nil
	}
	if in.EffectClass == EffectReadOnly || in.EffectClass == EffectWorkspaceWrite {
		return DecisionAllow, "isolated workspace local action permitted", nil
	}

	scope := in.CapabilityScope
	if scope == "" {
		scope = in.ToolName
	}

	for _, pid := range in.ReferencedPageIDs {
		if pid == "" {
			continue
		}
		page, err := st.GetPage(ctx, pid)
		if err != nil {
			return DecisionAsk, fmt.Sprintf("unable to verify provenance of %s (%v)", pid, err), nil
		}
		if page.TrustTier != "untrusted_external" {
			continue
		}
		ok, err := st.HasDeclassification(ctx, pid, scope)
		if err != nil {
			return "", "", fmt.Errorf("check declassification %s: %w", pid, err)
		}
		if !ok {
			return DecisionAsk, fmt.Sprintf("external action %s references untrusted @%s without declassification for %s",
				in.ToolName, pid, scope), nil
		}
	}

	return DecisionAllow, "all referenced inputs trusted or declassified for scope", nil
}

// DeclassifyStructured runs a deterministic non-LLM parser on an untrusted @page blob and,
// if valid, records a scoped schema_parser declassification for capabilityScope.
func DeclassifyStructured[T any](
	ctx context.Context,
	st *store.Store,
	pageID string,
	capabilityScope string,
	schemaName string,
	parser func([]byte) (T, error),
) (T, error) {
	var zero T
	page, err := st.GetPage(ctx, pageID)
	if err != nil {
		return zero, fmt.Errorf("load page %s: %w", pageID, err)
	}
	raw, err := st.Blobs().Get(page.ContentHash)
	if err != nil {
		return zero, fmt.Errorf("load blob for %s: %w", pageID, err)
	}
	parsed, err := parser(raw)
	if err != nil {
		return zero, fmt.Errorf("schema %s validation failed on %s: %w", schemaName, pageID, err)
	}

	dec := store.Declassification{
		ID:              fmt.Sprintf("dec_%d_%s", time.Now().UnixNano(), pageID),
		PageID:          pageID,
		CapabilityScope: capabilityScope,
		Method:          "schema_parser",
		ApprovedBy:      "parser:" + schemaName,
		SchemaName:      schemaName,
	}
	if err := st.AddDeclassification(ctx, dec); err != nil {
		return zero, fmt.Errorf("record declassification: %w", err)
	}
	return parsed, nil
}

// ApproveDeclassification records an explicit operator approval scoped strictly to (pageID, capabilityScope).
func ApproveDeclassification(ctx context.Context, st *store.Store, pageID, capabilityScope, approvedBy string) error {
	if approvedBy == "" {
		approvedBy = "operator"
	}
	return st.AddDeclassification(ctx, store.Declassification{
		ID:              fmt.Sprintf("dec_%d_%s", time.Now().UnixNano(), pageID),
		PageID:          pageID,
		CapabilityScope: capabilityScope,
		Method:          "operator_approval",
		ApprovedBy:      approvedBy,
	})
}
