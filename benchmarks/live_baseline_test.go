package benchmarks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The A/B comparison is only meaningful if the linear baseline is a fair one. This offline test
// pins two properties of its compaction step against a fake chat endpoint: the summarizer is shown
// the most recent tool result, and the summary it returns is what the next turn actually sees.
func TestLinearBaseline_CompactionKeepsItsSummary(t *testing.T) {
	ws := t.TempDir()
	big := strings.Repeat("INFO heartbeat ok\n", 400) + "ERROR needle=RAW_NEEDLE_7741\n"
	if err := os.WriteFile(filepath.Join(ws, "big.log"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var summarizerSawNeedle, nextTurnSawSummary bool
	toolTurns := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
			Tools    []any            `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		raw, _ := json.Marshal(req.Messages)
		mu.Lock()
		defer mu.Unlock()

		reply := func(msg map[string]any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": msg}},
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
			})
		}
		if len(req.Tools) == 0 { // summarization call
			summarizerSawNeedle = strings.Contains(string(raw), "RAW_NEEDLE_7741")
			reply(map[string]any{"role": "assistant", "content": "SUMMARY_MARKER: needle is RAW_NEEDLE_7741 in big.log"})
			return
		}
		toolTurns++
		if toolTurns == 1 {
			reply(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "fs_read", "arguments": `{"path":"big.log"}`},
			}}})
			return
		}
		nextTurnSawSummary = strings.Contains(string(raw), "SUMMARY_MARKER: needle is RAW_NEEDLE_7741")
		reply(map[string]any{"role": "assistant", "content": "done"})
	}))
	defer srv.Close()

	res := runLinearBaseline(context.Background(), liveConfig{BaseURL: srv.URL, APIKey: "test", ModelID: "fake"},
		ws, "system", "find the needle", 4, 2000)

	if res.ErrorText != "" {
		t.Fatalf("baseline errored: %s", res.ErrorText)
	}
	if res.Compactions != 1 || res.SummaryFailures != 0 {
		t.Fatalf("want exactly 1 successful compaction, got compactions=%d summary_failures=%d", res.Compactions, res.SummaryFailures)
	}
	if !summarizerSawNeedle {
		t.Error("summarizer was not shown the most recent tool result")
	}
	if !nextTurnSawSummary {
		t.Error("the turn after compaction did not receive the summarizer's own text")
	}
}
