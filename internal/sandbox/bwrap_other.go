//go:build !linux

package sandbox

import "context"

type bwrapBackend struct{}

func newBwrapBackend() Backend { return &bwrapBackend{} }

func (b *bwrapBackend) Name() string                    { return "bwrap" }
func (b *bwrapBackend) Available() bool                 { return false }
func (b *bwrapBackend) SupportsNetworkIsolation() bool  { return true }
func (b *bwrapBackend) Exec(_ context.Context, _ ExecSpec) (ExecResult, error) {
	return ExecResult{}, ErrSandboxUnavailable
}
