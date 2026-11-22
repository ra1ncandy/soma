package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ra1ncandy/soma/internal/store"
)

// ErrUnauthorized indicates a fail-closed sender or channel allowlist rejection.
var ErrUnauthorized = errors.New("bridge: sender not in configured allowlist")

// HealthResponse reports Unix socket bridge server readiness.
type HealthResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// StatusResponse mirrors operator kernel status over IPC.
type StatusResponse struct {
	Runs              []store.Run     `json:"runs"`
	Branches          []store.Branch  `json:"branches"`
	Accounts          []store.Account `json:"accounts"`
	PendingInboxCount int             `json:"pending_inbox_count"`
}

// InboxListResponse returns pending or filtered operator inbox items.
type InboxListResponse struct {
	Items []store.InboxItem `json:"items"`
}

// InboxActionRequest carries operator metadata for inbox approve/dismiss calls.
type InboxActionRequest struct {
	Actor string `json:"actor,omitempty"`
}

// InboxActionResponse reports the result of approving or dismissing an inbox item.
type InboxActionResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// StartRunRequest asks the kernel to start a run from an external bridge adapter.
type StartRunRequest struct {
	Goal     string `json:"goal"`
	Profile  string `json:"profile,omitempty"`
	Title    string `json:"title,omitempty"`
	Source   string `json:"source,omitempty"`
	MaxTurns int    `json:"max_turns,omitempty"`
	Sync     bool   `json:"sync,omitempty"`
}

// StartRunResponse reports the created run and branch handles.
type StartRunResponse struct {
	RunID    string `json:"run_id"`
	BranchID string `json:"branch_id"`
	Status   string `json:"status"`
	Summary  string `json:"summary,omitempty"`
}

// ReconcileRequest resolves an ambiguous in-flight action over IPC.
type ReconcileRequest struct {
	ActionID string `json:"action_id"`
	Outcome  string `json:"outcome"` // succeeded | failed
	Summary  string `json:"summary,omitempty"`
}

// ReconcileResponse reports reconciliation outcome.
type ReconcileResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// ErrorResponse carries structured JSON errors over the bridge socket.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ParsedCommand represents a structured chat command from Discord or Telegram.
type ParsedCommand struct {
	Name string
	Arg  string
}

// ParseCommand parses `!soma <cmd> [arg]` or `/<cmd>[@bot] [arg]` text messages.
func ParseCommand(text string) (ParsedCommand, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ParsedCommand{}, false
	}

	var rest string
	low := strings.ToLower(trimmed)
	switch {
	case low == "!soma":
		return ParsedCommand{Name: "help"}, true
	case strings.HasPrefix(low, "!soma "):
		rest = strings.TrimSpace(trimmed[len("!soma "):])
	case strings.HasPrefix(trimmed, "/"):
		rest = strings.TrimSpace(trimmed[1:])
	default:
		return ParsedCommand{}, false
	}

	if rest == "" {
		return ParsedCommand{Name: "help"}, true
	}

	parts := strings.SplitN(rest, " ", 2)
	cmd := strings.ToLower(strings.TrimSpace(parts[0]))
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}
	var arg string
	if len(parts) == 2 {
		arg = strings.TrimSpace(parts[1])
	}
	return ParsedCommand{Name: cmd, Arg: arg}, true
}

// ChunkMessage splits long replies without exceeding channel size limits.
func ChunkMessage(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 1900
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	rem := text
	for len(rem) > maxLen {
		cut := strings.LastIndexByte(rem[:maxLen], '\n')
		if cut <= maxLen/4 {
			cut = maxLen
		}
		chunks = append(chunks, strings.TrimRight(rem[:cut], "\n"))
		rem = strings.TrimLeft(rem[cut:], "\n")
	}
	if rem != "" {
		chunks = append(chunks, rem)
	}
	return chunks
}

func formatStatusSummary(st StatusResponse) string {
	return fmt.Sprintf("soma status: %d run(s), %d branch(es), %d account(s), %d pending inbox item(s)",
		len(st.Runs), len(st.Branches), len(st.Accounts), st.PendingInboxCount)
}

func formatInboxList(items []store.InboxItem) string {
	if len(items) == 0 {
		return "inbox: no pending items"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "inbox (%d pending):\n", len(items))
	for _, it := range items {
		fmt.Fprintf(&b, "- [%s] (%s/%s) %s\n", it.ID, it.ItemKind, it.Urgency, it.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

func dispatchBridgeCommand(ctx context.Context, client *KernelClient, source, actor, prefix string, cmd ParsedCommand) (string, error) {
	switch cmd.Name {
	case "status":
		st, err := client.Status(ctx)
		if err != nil {
			return "", err
		}
		return formatStatusSummary(st), nil
	case "inbox":
		items, err := client.ListInbox(ctx, "pending")
		if err != nil {
			return "", err
		}
		return formatInboxList(items), nil
	case "approve":
		if cmd.Arg == "" {
			return fmt.Sprintf("usage: %sapprove <inbox_item_id>", prefix), nil
		}
		resp, err := client.ApproveInbox(ctx, cmd.Arg, actor)
		if err != nil {
			return "", err
		}
		return resp.Message, nil
	case "dismiss":
		if cmd.Arg == "" {
			return fmt.Sprintf("usage: %sdismiss <inbox_item_id>", prefix), nil
		}
		resp, err := client.DismissInbox(ctx, cmd.Arg, actor)
		if err != nil {
			return "", err
		}
		return resp.Message, nil
	case "run":
		if cmd.Arg == "" {
			return fmt.Sprintf("usage: %srun <goal>", prefix), nil
		}
		resp, err := client.StartRun(ctx, StartRunRequest{
			Goal:   cmd.Arg,
			Source: source,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("started run %s (branch %s, status=%s)", resp.RunID, resp.BranchID, resp.Status), nil
	default:
		return fmt.Sprintf("soma bridge commands: %sstatus | %sinbox | %sapprove <id> | %sdismiss <id> | %srun <goal>",
			prefix, prefix, prefix, prefix, prefix), nil
	}
}
