package dag

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/vmem"
)

// execute_code: programmatic tool calling. The model writes a Python script that runs in the
// same fail-closed OS sandbox as exec and calls soma tools through a unix-socket RPC. Unlike
// Hermes, the script never runs on the host, and every inner call is policy-checked: only
// local and outbound-read tools are callable (anything that writes outside the workspace must be
// a direct, journaled, approvable tool call), and outbound reads are egress-bounded once the run
// is tainted. Untrusted results taint the run exactly as direct calls do.

var codeToolAllow = map[string]string{
	// python name -> soma tool
	"read_file":      "fs_read",
	"list_files":     "fs_list",
	"grep":           "fs_grep",
	"write_file":     "fs_write",
	"patch_file":     "fs_patch",
	"page_slice":     "page_slice",
	"page_grep":      "page_grep",
	"page_search":    "page_search",
	"memory_search":  "memory_search",
	"session_search": "session_search",
	"web_search":     "web_search",
	"web_extract":    "web_extract",
}

const (
	maxCodeToolCalls   = 50
	maxCodeOutputBytes = 50_000
	defaultCodeTimeout = 60 * time.Second
	maxCodeTimeout     = 300 * time.Second
)

func init() {
	names := make([]string, 0, len(codeToolAllow))
	for n := range codeToolAllow {
		names = append(names, n)
	}
	sort.Strings(names)
	registerTool(ToolDef{
		Name:      "execute_code",
		Signature: "execute_code(code, timeout_sec?)",
		Description: "Run a Python 3 script in the sandbox that can call tools directly, to do many steps (loops, filtering, aggregation) in one turn without filling your context. " +
			"Inside the script, `import soma_tools as t` and call: " + strings.Join(names, ", ") + " (keyword arguments as in the tools of the same purpose, e.g. t.read_file(path=\"x\"), t.grep(pattern=\"TODO\")). Each returns the tool output as a string. " +
			"Only print() output comes back to you (truncated at 50 KB). At most 50 tool calls per script; tools that act outside the workspace are not available here.",
		Properties: map[string]any{
			"code":        strProp("Python 3 source"),
			"timeout_sec": map[string]any{"type": "integer", "description": "Wall-clock limit (default 60, max 300)"},
		},
		Required: []string{"code"},
		Effect:   taint.EffectWorkspaceWrite,
		Handler:  toolExecuteCode,
	})
}

const pythonHelper = `import json, os, socket

_conn = None
_reader = None

def call(tool, /, **args):
    global _conn, _reader
    if _conn is None:
        _conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        _conn.connect(os.environ["SOMA_RPC_SOCKET"])
        _reader = _conn.makefile("r", encoding="utf-8")
    args = {k: v for k, v in args.items() if v is not None}
    _conn.sendall((json.dumps({"tool": tool, "args": args}) + "\n").encode("utf-8"))
    line = _reader.readline()
    if not line:
        raise RuntimeError("soma tool RPC closed")
    resp = json.loads(line)
    if not resp.get("ok"):
        raise RuntimeError(resp.get("error", "tool failed"))
    return resp.get("output", "")
`

func pythonHelperModule() string {
	var sb strings.Builder
	sb.WriteString(pythonHelper)
	names := make([]string, 0, len(codeToolAllow))
	for n := range codeToolAllow {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&sb, "\ndef %s(**kwargs):\n    return call(%q, **kwargs)\n", n, n)
	}
	return sb.String()
}

const pythonBootstrap = `import os, sys
d = os.path.dirname(os.environ["SOMA_RPC_SOCKET"])
sys.path.insert(0, d)
sys.argv = ["main.py"]
exec(compile(open(os.path.join(d, "main.py"), encoding="utf-8").read(), "main.py", "exec"), {"__name__": "__main__"})
`

func toolExecuteCode(ctx context.Context, r *LocalToolRunner, call ToolCall, args map[string]any) (ToolOutput, error) {
	code, _ := args["code"].(string)
	if strings.TrimSpace(code) == "" {
		return ToolOutput{}, errors.New("execute_code requires code")
	}
	if r.Sandbox == nil {
		return ToolOutput{}, ErrUnsandboxedExec
	}
	timeout := defaultCodeTimeout
	if f, ok := args["timeout_sec"].(float64); ok && f > 0 {
		timeout = time.Duration(f) * time.Second
	}
	if timeout > maxCodeTimeout {
		timeout = maxCodeTimeout
	}

	dir, err := os.MkdirTemp("", "soma-x")
	if err != nil {
		return ToolOutput{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(code), 0o644); err != nil {
		return ToolOutput{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "soma_tools.py"), []byte(pythonHelperModule()), 0o644); err != nil {
		return ToolOutput{}, err
	}
	sock := filepath.Join(dir, "rpc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("execute_code: rpc socket: %w", err)
	}
	_ = os.Chmod(sock, 0o666) // reachable by the sandbox user mapping
	_ = os.Chmod(dir, 0o755)

	srv := &codeRPC{r: r, call: call}
	runCtx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.serve(runCtx, ln) }()

	res, execErr := r.Sandbox.Exec(runCtx, sandbox.ExecSpec{
		WorkspaceDir: call.WorkspaceDir,
		Command:      "python3",
		Args:         []string{"-c", pythonBootstrap},
		AllowNetwork: r.AllowSandboxNetwork,
		Timeout:      timeout,
		RPCSocket:    sock,
	})
	_ = ln.Close()
	cancel()
	wg.Wait()

	var sb strings.Builder
	switch {
	case errors.Is(execErr, sandbox.ErrCommandTimeout):
		fmt.Fprintf(&sb, "exit_code=-1 (TIMEOUT after %s)\n", timeout)
	case execErr != nil:
		return ToolOutput{}, execErr
	default:
		fmt.Fprintf(&sb, "exit_code=%d tool_calls=%d\n", res.ExitCode, srv.calls.Load())
	}
	sb.WriteString(truncateMiddle(string(res.Stdout), maxCodeOutputBytes))
	if len(res.Stderr) > 0 {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(truncateMiddle(string(res.Stderr), 10_000))
	}
	tier := shellOutputTier(r.AllowSandboxNetwork)
	if srv.untrusted.Load() {
		tier = "untrusted_external"
	}
	return ToolOutput{NameHint: "execute_code.log", Content: []byte(sb.String()), TrustTier: tier}, nil
}

func truncateMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit * 4 / 5
	tail := limit - head
	return s[:head] + fmt.Sprintf("\n… [%d bytes truncated] …\n", len(s)-head-tail) + s[len(s)-tail:]
}

type codeRPC struct {
	r         *LocalToolRunner
	call      ToolCall
	calls     atomic.Int32
	untrusted atomic.Bool
}

type codeRPCRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type codeRPCResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *codeRPC) serve(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			go func() { <-ctx.Done(); _ = c.Close() }()
			sc := bufio.NewScanner(c)
			sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
			enc := json.NewEncoder(c)
			for sc.Scan() {
				var req codeRPCRequest
				var resp codeRPCResponse
				if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
					resp.Error = "bad request: " + err.Error()
				} else {
					resp = s.handle(ctx, req)
				}
				if enc.Encode(resp) != nil {
					return
				}
			}
		}(conn)
	}
}

func (s *codeRPC) handle(ctx context.Context, req codeRPCRequest) codeRPCResponse {
	tool, ok := codeToolAllow[req.Tool]
	if !ok {
		return codeRPCResponse{Error: fmt.Sprintf("%s is not callable from execute_code", req.Tool)}
	}
	if s.calls.Add(1) > maxCodeToolCalls {
		return codeRPCResponse{Error: fmt.Sprintf("tool call limit (%d) reached", maxCodeToolCalls)}
	}
	argsJSON, _ := json.Marshal(req.Args)
	cls := inferToolEffectClass(tool)
	vm := s.r.vm
	if cls == taint.EffectExternalRead && vm != nil && vm.Store() != nil {
		implied, err := vm.Store().ListUntrustedPageIDs(ctx, s.call.RunID)
		if err != nil {
			return codeRPCResponse{Error: "load run provenance: " + err.Error()}
		}
		decision, reason, err := taint.EvaluateAction(ctx, vm.Store(), taint.ActionPolicyInput{
			ToolName: tool, EffectClass: cls, ReferencedPageIDs: implied, ArgsJSON: string(argsJSON),
		})
		if err != nil {
			return codeRPCResponse{Error: err.Error()}
		}
		if decision != taint.DecisionAllow {
			return codeRPCResponse{Error: "blocked by policy: " + reason + " (call the tool directly to request approval)"}
		}
	}
	inner := s.call
	inner.Name = tool
	inner.ArgsJSON = string(argsJSON)
	inner.EffectClass = cls
	out, err := s.r.Execute(ctx, inner)
	if err != nil {
		return codeRPCResponse{Error: err.Error()}
	}
	if out.TrustTier == "untrusted_external" {
		s.untrusted.Store(true)
		// Record the observation so the run is tainted exactly as for a direct call.
		if vm != nil {
			_, _ = vm.Observe(ctx, vmem.ObserveRequest{
				RunID: s.call.RunID, NameHint: "execute_code." + tool, Content: out.Content,
				TrustTier: "untrusted_external", InlineThresholdBytes: 1,
			})
		}
	}
	return codeRPCResponse{OK: true, Output: string(out.Content)}
}
