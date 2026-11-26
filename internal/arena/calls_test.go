package arena

import (
	"reflect"
	"testing"
)

func TestSummarizeRequest(t *testing.T) {
	rec := summarizeRequest([]byte(`{
		"model":"m","reasoning_effort":"medium","max_tokens":100,
		"tools":[{"type":"function"},{"type":"function"}],
		"messages":[
			{"role":"system","content":"abcd"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"","reasoning_content":"thinking...","tool_calls":[]},
			{"role":"assistant","content":"x","reasoning_content":""}
		]}`))
	if rec.Messages != 4 || rec.Tools != 2 {
		t.Fatalf("messages=%d tools=%d", rec.Messages, rec.Tools)
	}
	if rec.Params["reasoning_effort"] != "medium" || rec.Params["model"] != "m" {
		t.Fatalf("params=%v", rec.Params)
	}
	if _, ok := rec.Params["messages"]; ok {
		t.Fatal("conversation must not be copied into the record")
	}
	if rec.ReplayedReasoningChars != len(`"thinking..."`) {
		t.Fatalf("replayed=%d", rec.ReplayedReasoningChars)
	}
	if rec.SystemChars != len(`"abcd"`) {
		t.Fatalf("system=%d", rec.SystemChars)
	}
}

func TestSummarizeResponse(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		rec := &CallRecord{}
		summarizeResponse(rec, []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"content":"ok","reasoning_content":"12345",
			"tool_calls":[{"function":{"name":"a","arguments":"{}"}},{"function":{"name":"b","arguments":"{\"x\":1}"}}]}}],
			"usage":{"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":4}}}`), false)
		want := &CallRecord{ReasoningTokens: 4, ReasoningChars: 5, ContentChars: 2, ToolArgChars: 9, ToolNames: []string{"a", "b"}, FinishReason: "tool_calls"}
		if !reflect.DeepEqual(rec, want) {
			t.Fatalf("got %+v\nwant %+v", rec, want)
		}
	})
	t.Run("sse", func(t *testing.T) {
		rec := &CallRecord{}
		body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"abc\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"w\",\"arguments\":\"{\\\"p\\\":\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"
		summarizeResponse(rec, []byte(body), true)
		want := &CallRecord{ReasoningChars: 3, ContentChars: 2, ToolArgChars: 7, ToolNames: []string{"w"}, FinishReason: "tool_calls"}
		if !reflect.DeepEqual(rec, want) {
			t.Fatalf("got %+v\nwant %+v", rec, want)
		}
	})
}
