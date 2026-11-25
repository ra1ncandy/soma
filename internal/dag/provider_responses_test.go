package dag

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesProviderRoundTrip(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"output":[
			{"type":"reasoning","summary":[{"text":"need to search"}]},
			{"type":"message","content":[{"type":"output_text","text":"Searching."}]},
			{"type":"function_call","call_id":"fc_1","name":"fs_grep","arguments":"{\"pattern\":\"x\"}"}],
			"usage":{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":800}}}`)
	}))
	defer srv.Close()

	prov := NewProvider(srv.URL+"/v1#responses", "k")
	rp, ok := prov.(*ResponsesProvider)
	if !ok {
		t.Fatalf("#responses opt-in not honoured: %T", prov)
	}
	resp, err := rp.Generate(context.Background(), GenerateRequest{ModelID: "gpt-x", TurnIndex: 2, Frame: anthropicTestFrame()})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if path != "/v1/responses" || got["store"] != false || got["instructions"] == "" {
		t.Fatalf("request wrong: path=%s body=%v", path, got)
	}
	raw, _ := json.Marshal(got["input"])
	for _, want := range []string{`"type":"function_call"`, `"call_id":"toolu_1"`, `"type":"function_call_output"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("input missing %s: %s", want, raw)
		}
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].CallID != "fc_1" || resp.ToolCalls[0].Name != "fs_grep" {
		t.Fatalf("tool calls: %+v", resp.ToolCalls)
	}
	if resp.InputTokens != 200 || resp.CacheReadTokens != 800 || resp.ReasoningContent != "need to search" {
		t.Fatalf("usage/reasoning: %+v", resp)
	}
	if ProtocolForEndpoint("https://chatgpt.com/backend-api/codex") != ProtocolOpenAIResponses || ProtocolForEndpoint("https://api.anthropic.com/v1") != ProtocolAnthropic || ProtocolForEndpoint("https://api.openai.com/v1") != ProtocolOpenAIChat {
		t.Fatal("protocol detection wrong")
	}
}
