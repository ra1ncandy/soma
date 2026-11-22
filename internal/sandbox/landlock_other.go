//go:build !linux

package sandbox

import "context"

// ProbeLandlockABI returns 0 on non-Linux hosts.
func ProbeLandlockABI() int { return 0 }

type landlockBackend struct{}

func newLandlockBackend() Backend                  { return &landlockBackend{} }
func (l *landlockBackend) Name() string            { return "landlock" }
func (l *landlockBackend) Available() bool         { return false }
func (l *landlockBackend) SupportsNetworkIsolation() bool { return false }
func (l *landlockBackend) Exec(_ context.Context, _ ExecSpec) (ExecResult, error) {
	return ExecResult{}, ErrSandboxUnavailable
}
