package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FSRootPolicy authorizes controlled host-side reads/writes outside the branch workspace.
type FSRootPolicy struct {
	RootPath string
	ReadOnly bool
}

// FSBroker mediates explicit external filesystem reads/writes.
type FSBroker struct {
	Roots []FSRootPolicy
}

// NewFSBroker builds an FSBroker canonicalizing allowlisted roots.
func NewFSBroker(roots ...FSRootPolicy) *FSBroker {
	out := make([]FSRootPolicy, 0, len(roots))
	for _, r := range roots {
		abs, err := filepath.Abs(r.RootPath)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		out = append(out, FSRootPolicy{RootPath: filepath.Clean(abs), ReadOnly: r.ReadOnly})
	}
	return &FSBroker{Roots: out}
}

func (f *FSBroker) Name() string { return "fs" }

// Validate enforces external filesystem root containment and read-only constraints before taint approval prompts.
func (f *FSBroker) Validate(req BrokerRequest) error {
	_, _, err := f.validateAndResolve(req)
	return err
}

func (f *FSBroker) validateAndResolve(req BrokerRequest) (string, *FSRootPolicy, error) {
	if req.Target == "" {
		return "", nil, fmt.Errorf("%w: empty fs target", ErrPolicyDenied)
	}
	absTarget, err := filepath.Abs(req.Target)
	if err != nil {
		return "", nil, err
	}
	resolved, err := resolveWithParentSymlinks(absTarget)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrPolicyDenied, err)
	}

	var matched *FSRootPolicy
	for i := range f.Roots {
		rp := &f.Roots[i]
		if resolved == rp.RootPath || strings.HasPrefix(resolved, rp.RootPath+string(filepath.Separator)) {
			matched = rp
			break
		}
	}
	if matched == nil {
		return "", nil, fmt.Errorf("%w: path %q outside allowlisted external roots", ErrPolicyDenied, resolved)
	}
	if strings.EqualFold(req.Op, "write") && matched.ReadOnly {
		return "", nil, fmt.Errorf("%w: fs root %q is read-only", ErrPolicyDenied, matched.RootPath)
	}
	return resolved, matched, nil
}

func (f *FSBroker) Execute(_ context.Context, req BrokerRequest) (BrokerResponse, error) {
	resolved, matched, err := f.validateAndResolve(req)
	if err != nil {
		return BrokerResponse{}, err
	}

	switch strings.ToLower(req.Op) {
	case "read":
		data, err := os.ReadFile(resolved)
		if err != nil {
			return BrokerResponse{}, err
		}
		return BrokerResponse{
			StatusCode:  200,
			Body:        data,
			ContentType: "application/octet-stream",
			TrustTier:   "trusted_repo",
		}, nil
	case "write":
		if matched.ReadOnly {
			return BrokerResponse{}, fmt.Errorf("%w: fs root %q is read-only", ErrPolicyDenied, matched.RootPath)
		}
		var params struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(req.ParamsJSON), &params); err != nil {
			return BrokerResponse{}, err
		}
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			return BrokerResponse{}, err
		}
		if err := os.WriteFile(resolved, []byte(params.Content), 0o644); err != nil {
			return BrokerResponse{}, err
		}
		return BrokerResponse{
			StatusCode:  200,
			Body:        []byte("ok"),
			ContentType: "text/plain",
			TrustTier:   "trusted_repo",
		}, nil
	default:
		return BrokerResponse{}, fmt.Errorf("%w: unsupported fs op %q", ErrPolicyDenied, req.Op)
	}
}

func resolveWithParentSymlinks(absPath string) (string, error) {
	if real, err := filepath.EvalSymlinks(absPath); err == nil {
		return filepath.Clean(real), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(realDir, base)), nil
}
