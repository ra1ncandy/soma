package dag

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

// Within an epoch, each request's conversation must extend the previous one without rewriting
// it: Anthropic signs the model's thinking blocks against everything before them and discards
// them if an earlier turn changed. The run below crosses into the low-budget phase and trips
// the repeating-checks notice, the two places soma used to inject text that vanished again on
// the next request.
func TestAnthropicConversationIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	setupAccount(t, st, "acc_ao")
	tools := &funcToolRunner{fn: func(ToolCall) (ToolOutput, error) {
		return ToolOutput{NameHint: "out.txt", Content: []byte("exit_code=0\nok"), TrustTier: "trusted_repo"}, nil
	}}

	var requests [][]string // per request: each message as canonical JSON, cache markers removed
	script := []string{"fs_write", "exec", "exec", "exec", "exec", "exec"}
	calls := 0
	ex := NewExecutor(st, &scriptedProvider{fn: func(_ int, req GenerateRequest) (*GenerateResponse, error) {
		_, msgs, _ := toAnthropicMessages(buildMessages(req.Frame, false, true))
		var flat []string
		for _, m := range msgs {
			raw, _ := json.Marshal(m)
			flat = append(flat, strings.ReplaceAll(string(raw), `"cache_control":{"type":"ephemeral"},`, ""))
		}
		requests = append(requests, flat)
		if calls >= len(script) {
			return &GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "done"}, nil
		}
		name := script[calls]
		calls++
		id := "toolu_" + string(rune('a'+calls))
		spec := ToolCallSpec{CallID: id, Name: name, ArgsJSON: `{"n":` + string(rune('0'+calls)) + `}`, EffectClass: inferToolEffectClass(name)}
		return &GenerateResponse{
			ToolCalls: []ToolCallSpec{spec}, ToolCall: &spec, StateHash: name + id,
			ProviderContent: `[{"signature":"SIG` + id + `","thinking":"","type":"thinking"},{"id":"` + id + `","input":{"n":1},"name":"` + name + `","type":"tool_use"}]`,
		}, nil
	}}, tools, compiler.WorkerProfile())
	_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: t.TempDir(), GoalSpec: "x", AccountIDs: []string{"acc_ao"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteBranch(ctx, br, 7); err != nil { // 7 turns: the last 3 are "budget low"
		t.Fatal(err)
	}

	if len(requests) != 7 {
		t.Fatalf("requests = %d", len(requests))
	}
	sawNotice, sawThinking := false, false
	for k := 1; k < len(requests); k++ {
		prev, cur := requests[k-1], requests[k]
		if len(cur) <= len(prev) {
			t.Fatalf("request %d did not grow: %d -> %d messages", k+1, len(prev), len(cur))
		}
		for i := range prev {
			if prev[i] != cur[i] {
				t.Fatalf("request %d rewrote message %d of the previous request:\n was %s\n now %s", k+1, i, prev[i], cur[i])
			}
		}
		joined := strings.Join(cur, "\n")
		sawNotice = sawNotice || (strings.Contains(joined, "TURN BUDGET LOW") && strings.Contains(joined, "CHECKS REPEATING"))
		sawThinking = sawThinking || strings.Contains(joined, `"type":"thinking"`)
	}
	if !sawNotice || !sawThinking {
		t.Fatalf("the run must exercise both notices and thinking replay (notice=%v thinking=%v)", sawNotice, sawThinking)
	}
}
