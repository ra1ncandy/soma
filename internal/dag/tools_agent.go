package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ra1ncandy/soma/internal/taint"
)

func init() {
	registerTool(ToolDef{
		Name:        "todo_write",
		Signature:   "todo_write(items)",
		Description: "Replace this branch's task list. Use it to plan multi-step work and keep it current: exactly one item in_progress, mark items completed as soon as they are done. Returns the rendered list.",
		Properties: map[string]any{
			"items": map[string]any{
				"type":        "array",
				"description": "Full task list in order",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string"},
						"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}},
					},
					"required": []string{"content", "status"},
				},
			},
		},
		Required: []string{"items"},
		Effect:   taint.EffectWorkspaceWrite,
		Handler:  toolTodoWrite,
	})
	registerTool(ToolDef{
		Name:        "clarify",
		Signature:   "clarify(question, choices?)",
		Description: "Ask the user a question and stop until they answer. Use only when you cannot proceed safely without their decision; otherwise make a reasonable assumption and continue.",
		Properties: map[string]any{
			"question": strProp("The question for the user"),
			"choices":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional suggested answers"},
		},
		Required:        []string{"question"},
		Effect:          taint.EffectReadOnly,
		Handler:         toolClarify,
		SubagentBlocked: true,
	})
}

type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// todoPath stores the list next to the project state (not in the branch workspace, which is a
// throwaway copy), keyed by branch so forks and subagents keep separate lists.
func (r *LocalToolRunner) todoPath(call ToolCall) string {
	dir := r.ProjectSomaDir
	if dir == "" {
		dir = filepath.Join(call.WorkspaceDir, ".soma")
	}
	return filepath.Join(dir, "todos", sanitizeID(call.BranchID)+".json")
}

func sanitizeID(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
	if s == "" {
		return "default"
	}
	return s
}

func toolTodoWrite(_ context.Context, r *LocalToolRunner, call ToolCall, args map[string]any) (ToolOutput, error) {
	raw, err := json.Marshal(args["items"])
	if err != nil {
		return ToolOutput{}, err
	}
	var items []todoItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return ToolOutput{}, fmt.Errorf("todo_write: items must be [{content, status}]: %w", err)
	}
	inProgress := 0
	for i := range items {
		items[i].Content = strings.TrimSpace(items[i].Content)
		switch items[i].Status {
		case "pending", "completed", "cancelled":
		case "in_progress":
			inProgress++
		default:
			return ToolOutput{}, fmt.Errorf("todo_write: item %d has invalid status %q", i+1, items[i].Status)
		}
		if items[i].Content == "" {
			return ToolOutput{}, fmt.Errorf("todo_write: item %d is empty", i+1)
		}
	}
	if inProgress > 1 {
		return ToolOutput{}, errors.New("todo_write: at most one item may be in_progress")
	}
	p := r.todoPath(call)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return ToolOutput{}, err
	}
	out, _ := json.MarshalIndent(items, "", "  ")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{NameHint: "todo.txt", Content: []byte(renderTodos(items)), TrustTier: "trusted_repo"}, nil
}

func renderTodos(items []todoItem) string {
	if len(items) == 0 {
		return "todo list cleared"
	}
	marks := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]", "cancelled": "[-]"}
	var sb strings.Builder
	done := 0
	for _, it := range items {
		if it.Status == "completed" {
			done++
		}
		fmt.Fprintf(&sb, "%s %s\n", marks[it.Status], it.Content)
	}
	fmt.Fprintf(&sb, "(%d/%d completed)", done, len(items))
	return sb.String()
}

// LoadTodos returns the stored task list of a branch, for UIs.
func LoadTodos(somaDir, branchID string) ([]map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(somaDir, "todos", sanitizeID(branchID)+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var items []map[string]string
	return items, json.Unmarshal(raw, &items)
}

func toolClarify(_ context.Context, _ *LocalToolRunner, _ ToolCall, args map[string]any) (ToolOutput, error) {
	q, _ := args["question"].(string)
	if strings.TrimSpace(q) == "" {
		return ToolOutput{}, errors.New("clarify requires a question")
	}
	return ToolOutput{NameHint: "clarify.txt", Content: []byte("question delivered to the user; waiting for their reply"), TrustTier: "trusted_repo"}, nil
}

// clarifyQuestion returns the question text if this turn called clarify.
func clarifyQuestion(calls []ToolCallSpec) (string, bool) {
	for _, c := range calls {
		if c.Name != "clarify" {
			continue
		}
		var a struct {
			Question string   `json:"question"`
			Choices  []string `json:"choices"`
		}
		_ = json.Unmarshal([]byte(c.ArgsJSON), &a)
		q := strings.TrimSpace(a.Question)
		if q == "" {
			continue
		}
		if len(a.Choices) > 0 {
			q += "\nOptions: " + strings.Join(a.Choices, " | ")
		}
		return q, true
	}
	return "", false
}
