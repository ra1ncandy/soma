package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/ra1ncandy/soma/internal/policy"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
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
	Headers     map[string]string
}

// Broker executes one typed host capability family.
type Broker interface {
	Name() string
	Execute(ctx context.Context, req BrokerRequest) (BrokerResponse, error)
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

	taintDec, taintReason, err := taint.EvaluateAction(ctx, r.Store, taint.ActionPolicyInput{
		ToolName:          req.Broker + ":" + req.Op,
		CapabilityScope:   scope,
		EffectClass:       eff,
		ReferencedPageIDs: req.ReferencedPageIDs,
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

	b, ok := r.Brokers[req.Broker]
	if !ok {
		return BrokerResponse{}, fmt.Errorf("%w: %s", ErrBrokerNotFound, req.Broker)
	}
	return b.Execute(ctx, req)
}
