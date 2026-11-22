package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/ra1ncandy/soma/internal/policy"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/vmem"
)

var (
	// ErrPolicyDenied indicates a hard policy deny rule blocked the broker call.
	ErrPolicyDenied = errors.New("broker: denied by policy")
	// ErrApprovalRequired indicates the policy engine or taint provenance gate requires operator approval.
	ErrApprovalRequired = errors.New("broker: operator approval required")
	// ErrBrokerNotFound indicates no broker is registered for the requested capability.
	ErrBrokerNotFound = errors.New("broker: unknown capability broker")
)

// BrokerRequest describes a host-mediated side effect requested by a sandboxed agent.
type BrokerRequest struct {
	RunID             string
	BranchID          string
	ActionID          string
	IdempotencyKey    string
	Broker            string // "http" | "ssh" | "fs"
	Op                string // "GET" | "POST" | "exec" | "read" | "write"
	Target            string
	ParamsJSON        string
	EffectClass       taint.EffectClass
	CapabilityScope   string
	ReferencedPageIDs []string
}

// BrokerResponse captures host-side capability output and its enforced provenance trust tier.
type BrokerResponse struct {
	StatusCode  int
	Body        []byte
	ContentType string
	TrustTier   string
	PageID      string
	Headers     map[string]string
}

// Broker executes one typed host capability family.
type Broker interface {
	Name() string
	Execute(ctx context.Context, req BrokerRequest) (BrokerResponse, error)
}

// RequestValidator is optionally implemented by a Broker to enforce hard policy deny rules
// (command prefix/metacharacter checks, domain/path allowlists, or filesystem root containment)
// BEFORE taint approval checks so deny > ask > allow precedence holds everywhere.
type RequestValidator interface {
	Validate(req BrokerRequest) error
}

// Router enforces Policy (deny > ask > allow) and Taint Provenance before dispatching to any Broker.
type Router struct {
	Store   *store.Store
	Policy  *policy.Engine
	Brokers map[string]Broker
}

// NewRouter constructs a Router with the provided store, policy engine, and brokers.
func NewRouter(st *store.Store, pol *policy.Engine, brokers ...Broker) *Router {
	if pol == nil {
		pol = policy.NewEngine()
	}
	bm := make(map[string]Broker, len(brokers))
	for _, b := range brokers {
		if b != nil {
			bm[b.Name()] = b
		}
	}
	return &Router{
		Store:   st,
		Policy:  pol,
		Brokers: bm,
	}
}

// Call validates Policy and Data/Action Provenance before dispatching req to the typed broker.
func (r *Router) Call(ctx context.Context, req BrokerRequest) (BrokerResponse, error) {
	polDec, polReason := r.Policy.Evaluate(policy.Request{
		Capability: req.Broker,
		Op:         req.Op,
		Target:     req.Target,
	})
	if polDec == taint.DecisionDeny {
		return BrokerResponse{}, fmt.Errorf("%w: %s", ErrPolicyDenied, polReason)
	}

	b, ok := r.Brokers[req.Broker]
	if !ok {
		return BrokerResponse{}, fmt.Errorf("%w: %s", ErrBrokerNotFound, req.Broker)
	}
	// Hard broker-level policy denies (shell metacharacters, unallowlisted domains/commands, path traversal)
	// take precedence over taint/policy approval prompts (deny > ask > allow).
	if v, ok := b.(RequestValidator); ok {
		if err := v.Validate(req); err != nil {
			return BrokerResponse{}, err
		}
	}

	if polDec == taint.DecisionAsk {
		return BrokerResponse{}, fmt.Errorf("%w: %s", ErrApprovalRequired, polReason)
	}

	eff := req.EffectClass
	if eff == "" {
		eff = taint.EffectExternalWrite
	}
	scope := req.CapabilityScope
	if scope == "" {
		scope = req.Broker + ":" + req.Target
	}

	// Enforce the run-wide taint rule inside capability brokers: any non-local broker action
	// automatically inherits all untrusted_external pages observed by the run, even if the
	// caller did not pass them in ReferencedPageIDs.
	refs := req.ReferencedPageIDs
	if eff != taint.EffectReadOnly && eff != taint.EffectWorkspaceWrite && r.Store != nil && req.RunID != "" {
		implied, err := r.Store.ListUntrustedPageIDs(ctx, req.RunID)
		if err != nil {
			return BrokerResponse{}, fmt.Errorf("broker load run taint provenance: %w", err)
		}
		refs = mergeBrokerPageRefs(refs, implied)
		req.ReferencedPageIDs = refs
	}

	taintDec, taintReason, err := taint.EvaluateAction(ctx, r.Store, taint.ActionPolicyInput{
		ToolName:          req.Broker + ":" + req.Op,
		CapabilityScope:   scope,
		EffectClass:       eff,
		ReferencedPageIDs: refs,
	})
	if err != nil {
		return BrokerResponse{}, err
	}
	if taintDec == taint.DecisionDeny {
		return BrokerResponse{}, fmt.Errorf("%w: %s", ErrPolicyDenied, taintReason)
	}
	if taintDec == taint.DecisionAsk {
		return BrokerResponse{}, fmt.Errorf("%w: %s", ErrApprovalRequired, taintReason)
	}

	resp, err := b.Execute(ctx, req)
	if err != nil {
		return resp, err
	}
	if eff == taint.EffectReadOnly && resp.TrustTier == "untrusted_external" && len(resp.Body) > 0 && r.Store != nil && req.RunID != "" {
		pid, pErr := vmem.NewManager(r.Store).RecordProvenance(ctx, vmem.ObserveRequest{
			RunID:     req.RunID,
			NameHint:  req.Broker + "_" + req.Op + ".out",
			Content:   resp.Body,
			TrustTier: resp.TrustTier,
		})
		if pErr != nil {
			return BrokerResponse{}, fmt.Errorf("broker record untrusted provenance: %w", pErr)
		}
		resp.PageID = pid
	}
	return resp, nil
}

func mergeBrokerPageRefs(declared, implied []string) []string {
	if len(implied) == 0 {
		return declared
	}
	seen := make(map[string]bool, len(declared)+len(implied))
	out := make([]string, 0, len(declared)+len(implied))
	for _, id := range append(append([]string(nil), declared...), implied...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
