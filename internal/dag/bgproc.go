package dag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/vmem"
)

type bgJob struct {
	id           string
	runID        string
	label        string
	command      string
	workspaceDir string
	startedAt    time.Time
	cmd          *exec.Cmd
	sandboxed    bool

	mu       sync.Mutex
	buf      bytes.Buffer
	done     bool
	exitCode int
	exitErr  string
	endedAt  time.Time
	lastPage string
}

func (j *bgJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Cap in-memory buffer at 4 MiB per background job.
	const maxBuf = 4 << 20
	if j.buf.Len()+len(p) > maxBuf {
		keep := maxBuf - j.buf.Len()
		if keep <= 0 {
			return len(p), nil
		}
		p = p[:keep]
	}
	return j.buf.Write(p)
}

// BgProcessManager manages non-blocking background shell processes per workspace and spills
// their live stdout/stderr streams into @page virtual memory handles on poll/kill.
type BgProcessManager struct {
	mu   sync.Mutex
	seq  atomic.Uint64
	jobs map[string]*bgJob
}

// NewBgProcessManager creates a background process manager.
func NewBgProcessManager() *BgProcessManager {
	return &BgProcessManager{
		jobs: make(map[string]*bgJob),
	}
}

// BgStartSpec describes one background job launch.
type BgStartSpec struct {
	RunID        string
	WorkspaceDir string
	Command      string
	Label        string
	// Sandbox wraps the job in the same fail-closed OS isolation as synchronous exec.
	Sandbox *sandbox.Manager
	// AllowUnsandboxed permits a bare host shell when Sandbox is nil. Never set this for
	// model-driven runs: it hands the model the operator's network and filesystem.
	AllowUnsandboxed bool
}

// ErrUnsandboxedExec is returned when a shell tool is invoked with no sandbox and no explicit opt-out.
var ErrUnsandboxedExec = errors.New("dag: refusing to run shell command without an OS sandbox (fail-closed)")

// Start launches spec.Command asynchronously inside the OS sandbox (network-isolated, workspace-
// confined, secret-scrubbed). It fails closed when no sandbox backend is available.
func (m *BgProcessManager) Start(spec BgStartSpec) (string, error) {
	runID, workspaceDir, command, label := spec.RunID, spec.WorkspaceDir, spec.Command, spec.Label
	if strings.TrimSpace(command) == "" {
		return "", errors.New("bg_exec requires non-empty command")
	}

	var cmd *exec.Cmd
	sandboxed := false
	switch {
	case spec.Sandbox != nil:
		var err error
		cmd, _, err = spec.Sandbox.Command(context.Background(), sandbox.ExecSpec{
			WorkspaceDir: workspaceDir,
			Command:      "/bin/sh",
			Args:         []string{"-c", command},
		})
		if err != nil {
			return "", fmt.Errorf("bg_exec sandbox: %w", err)
		}
		sandboxed = true
	case spec.AllowUnsandboxed:
		cmd = exec.Command("/bin/sh", "-c", command)
		cmd.Dir = workspaceDir
		cmd.Env = sandbox.ScrubEnv(nil)
		setProcGroup(cmd)
	default:
		return "", ErrUnsandboxedExec
	}
	// Do not let a grandchild holding the output pipe block Wait forever after exit/kill.
	cmd.WaitDelay = 2 * time.Second

	jobID := fmt.Sprintf("bg_%04d", m.seq.Add(1))
	if label == "" {
		label = jobID
	}

	job := &bgJob{
		id:           jobID,
		runID:        runID,
		label:        label,
		command:      command,
		workspaceDir: workspaceDir,
		startedAt:    time.Now(),
		cmd:          cmd,
		sandboxed:    sandboxed,
	}
	cmd.Stdout = job
	cmd.Stderr = job

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("bg_exec start: %w", err)
	}

	m.mu.Lock()
	m.jobs[jobID] = job
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		defer job.mu.Unlock()
		job.done = true
		job.endedAt = time.Now()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				job.exitCode = ee.ExitCode()
			} else {
				job.exitCode = -1
				job.exitErr = err.Error()
			}
		} else {
			job.exitCode = 0
		}
	}()

	return jobID, nil
}

// Poll returns the current status and output of jobID, spilling outputs > inlineThreshold to @page.
func (m *BgProcessManager) Poll(ctx context.Context, vm *vmem.Manager, jobID string, inlineThreshold int) (string, error) {
	m.mu.Lock()
	job, ok := m.jobs[strings.TrimSpace(jobID)]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("bg_poll: unknown job_id %q", jobID)
	}

	job.mu.Lock()
	done := job.done
	exitCode := job.exitCode
	exitErr := job.exitErr
	rawOut := append([]byte(nil), job.buf.Bytes()...)
	dur := time.Since(job.startedAt)
	if done && !job.endedAt.IsZero() {
		dur = job.endedAt.Sub(job.startedAt)
	}
	runID := job.runID
	label := job.label
	job.mu.Unlock()

	stateStr := "running"
	if done {
		stateStr = fmt.Sprintf("exited(code=%d)", exitCode)
		if exitErr != "" {
			stateStr += " err=" + exitErr
		}
	}

	header := fmt.Sprintf("job_id=%s label=%q status=%s elapsed=%s bytes=%d",
		job.id, label, stateStr, dur.Round(time.Millisecond), len(rawOut))

	if len(rawOut) == 0 {
		return header + "\n(no output yet)", nil
	}

	if vm != nil {
		if inlineThreshold <= 0 {
			inlineThreshold = 3 * 1024
		}
		obs, err := vm.Observe(ctx, vmem.ObserveRequest{
			RunID:                runID,
			NameHint:             job.id + ".log",
			Content:              rawOut,
			TrustTier:            "trusted_repo",
			InlineThresholdBytes: inlineThreshold,
		})
		if err == nil {
			if obs.Paged {
				job.mu.Lock()
				job.lastPage = obs.PageID
				job.mu.Unlock()
				return header + "\n" + obs.Manifest, nil
			}
			return header + "\n" + obs.InlineText, nil
		}
	}
	return header + "\n" + string(rawOut), nil
}

// Kill terminates jobID's process group and returns its final Poll snapshot.
func (m *BgProcessManager) Kill(ctx context.Context, vm *vmem.Manager, jobID string, inlineThreshold int) (string, error) {
	m.mu.Lock()
	job, ok := m.jobs[strings.TrimSpace(jobID)]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("bg_kill: unknown job_id %q", jobID)
	}

	job.mu.Lock()
	alreadyDone := job.done
	job.mu.Unlock()

	if !alreadyDone && job.cmd != nil && job.cmd.Process != nil {
		if job.sandboxed {
			// Killing the sandbox launcher tears down its whole PID namespace.
			_ = job.cmd.Process.Kill()
		} else {
			killProcTree(job.cmd)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			job.mu.Lock()
			d := job.done
			job.mu.Unlock()
			if d {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return m.Poll(ctx, vm, jobID, inlineThreshold)
}
