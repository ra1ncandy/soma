package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
	"github.com/ra1ncandy/soma/internal/workspace"
)

// chatSession is one conversational REPL: messages continue the same branch (full history),
// /new starts a fresh conversation, and Ctrl+C interrupts only the running turn.
type chatSession struct {
	st       *store.Store
	ex       *dag.Executor
	ws       string
	accID    string
	modelID  string
	maxTurns int
	branchID string
	out      io.Writer
	// streamed is set by the provider's streaming hook when model text was already printed.
	streamed *bool
}

const chatHelp = `Commands:
  /new                 start a new conversation
  /resume <branch_id>  continue an earlier conversation
  /sessions            list recent conversations
  /search <words>      full-text search across past conversations
  /todo                show the agent's task list
  /diff                show changes made in this conversation's workspace
  /apply               apply those changes to the project (3-way merge)
  /status              show the current conversation
  /help                this help
  /exit                quit
End a line with \ to continue typing on the next line. Ctrl+C interrupts the running turn.`

func (c *chatSession) loop(ctx context.Context, in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for {
		fmt.Fprint(c.out, "\nsoma> ")
		var lines []string
		for {
			if !sc.Scan() {
				return sc.Err()
			}
			l := sc.Text()
			if strings.HasSuffix(l, `\`) {
				lines = append(lines, strings.TrimSuffix(l, `\`))
				fmt.Fprint(c.out, "  ... ")
				continue
			}
			lines = append(lines, l)
			break
		}
		msg := strings.TrimSpace(strings.Join(lines, "\n"))
		if msg == "" {
			continue
		}
		if strings.HasPrefix(msg, "/") {
			if quit := c.command(ctx, msg); quit {
				return nil
			}
			continue
		}
		c.send(ctx, msg)
	}
}

func (c *chatSession) command(ctx context.Context, line string) bool {
	fields := strings.Fields(line)
	arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	switch fields[0] {
	case "/exit", "/quit":
		return true
	case "/help", "/?":
		fmt.Fprintln(c.out, chatHelp)
	case "/new", "/reset":
		c.branchID = ""
		fmt.Fprintln(c.out, "Started a new conversation.")
	case "/resume":
		if arg == "" {
			fmt.Fprintln(c.out, "usage: /resume <branch_id>")
			break
		}
		if _, err := c.st.GetBranch(ctx, arg); err != nil {
			fmt.Fprintf(c.out, "no such branch: %v\n", err)
			break
		}
		c.branchID = arg
		fmt.Fprintf(c.out, "Resumed %s.\n", arg)
	case "/sessions":
		runs, err := c.st.ListRuns(ctx)
		if err != nil {
			fmt.Fprintf(c.out, "error: %v\n", err)
			break
		}
		for i, r := range runs {
			if i >= 15 {
				break
			}
			brs, _ := c.st.ListBranches(ctx, r.ID)
			main := ""
			for _, b := range brs {
				if b.ParentBranchID == "" {
					main = b.ID
				}
			}
			fmt.Fprintf(c.out, "%s  %-10s %s\n", main, r.Status, oneLineTrunc(r.GoalSpec, 70))
		}
	case "/search":
		lt := dag.NewLocalToolRunner(vmem.NewManager(c.st))
		out, err := lt.Execute(ctx, dag.ToolCall{Name: "session_search", ArgsJSON: fmt.Sprintf(`{"query":%q}`, arg)})
		if err != nil {
			fmt.Fprintf(c.out, "error: %v\n", err)
			break
		}
		fmt.Fprintln(c.out, string(out.Content))
	case "/todo":
		if c.branchID == "" {
			fmt.Fprintln(c.out, "no conversation yet")
			break
		}
		items, _ := dag.LoadTodos(c.st.SomaDir(), c.branchID)
		if len(items) == 0 {
			fmt.Fprintln(c.out, "no task list")
		}
		for _, it := range items {
			fmt.Fprintf(c.out, "[%s] %s\n", it["status"], it["content"])
		}
	case "/diff", "/apply":
		br, err := c.currentBranch(ctx)
		if err != nil {
			fmt.Fprintln(c.out, err)
			break
		}
		mgr := workspace.NewManager(c.st.SomaDir())
		if fields[0] == "/diff" {
			rep, err := mgr.DiffBranchWorkspace(ctx, c.ws, br.WorkspaceDir)
			if err != nil {
				fmt.Fprintf(c.out, "error: %v\n", err)
				break
			}
			if len(rep.Files) == 0 {
				fmt.Fprintln(c.out, "no changes")
			}
			for _, f := range rep.Files {
				fmt.Fprintf(c.out, "%s %s\n%s\n", f.Status, f.Path, f.UnifiedDiff)
			}
			break
		}
		rep, err := mgr.ApplyBranchWorkspace(ctx, c.ws, br.WorkspaceDir)
		if err != nil {
			fmt.Fprintf(c.out, "apply failed: %v (conflicts: %v)\n", err, rep.Conflicts)
			break
		}
		fmt.Fprintf(c.out, "applied: +%d ~%d -%d (3-way merged %d)\n", len(rep.AddedFiles), len(rep.ModifiedFiles), len(rep.DeletedFiles), len(rep.ThreeWayMerged))
	case "/status":
		if c.branchID == "" {
			fmt.Fprintf(c.out, "model=%s, no conversation yet\n", c.modelID)
			break
		}
		br, _ := c.st.GetBranch(ctx, c.branchID)
		fmt.Fprintf(c.out, "model=%s branch=%s status=%s workspace=%s\n", c.modelID, br.ID, br.Status, br.WorkspaceDir)
	default:
		fmt.Fprintf(c.out, "unknown command %s (try /help)\n", fields[0])
	}
	return false
}

func (c *chatSession) currentBranch(ctx context.Context) (store.Branch, error) {
	if c.branchID == "" {
		return store.Branch{}, fmt.Errorf("no conversation yet")
	}
	return c.st.GetBranch(ctx, c.branchID)
}

// send runs one user message. Ctrl+C cancels the turn and returns to the prompt.
func (c *chatSession) send(parent context.Context, msg string) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()
	if c.branchID == "" {
		_, brID, err := c.ex.StartRun(ctx, dag.RunRequest{
			Title: oneLineTrunc(msg, 64), WorkspacePath: c.ws, GoalSpec: msg, AccountIDs: []string{c.accID},
		})
		if err != nil {
			fmt.Fprintf(c.out, "error: %v\n", err)
			return
		}
		c.branchID = brID
	} else if err := c.ex.AppendFollowUpUserTurn(ctx, c.branchID, msg); err != nil {
		fmt.Fprintf(c.out, "error: %v\n", err)
		return
	}
	if c.streamed != nil {
		*c.streamed = false
	}
	res, err := c.ex.ExecuteBranch(ctx, c.branchID, c.maxTurns)
	if ctx.Err() != nil && parent.Err() == nil {
		fmt.Fprintln(c.out, "\n[interrupted — send a message to continue]")
		return
	}
	if err != nil {
		fmt.Fprintf(c.out, "\nerror: %v\n", err)
		return
	}
	switch {
	case res.Status == dag.StatusAwaitingInput:
		fmt.Fprintf(c.out, "\n? %s\n", res.FinalSummary)
	case res.Status == dag.StatusBudgetExhausted:
		fmt.Fprintf(c.out, "\n[turn budget used up after %d turns — send a message to continue]\n", res.TurnsExecuted)
	case c.streamed != nil && *c.streamed:
		fmt.Fprintln(c.out)
	case res.FinalSummary != "":
		fmt.Fprintf(c.out, "\n%s\n", res.FinalSummary)
	default:
		fmt.Fprintf(c.out, "\n[%s turns=%d tokens=%d cached=%d]\n", res.Status, res.TurnsExecuted, res.TotalTokens, res.CacheReadToks)
	}
}

func oneLineTrunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
