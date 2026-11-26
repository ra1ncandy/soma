package arena

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
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

// In warm-up mode the proxy answers model calls itself: nothing reaches the upstream and nothing
// is metered. The next Begin returns it to normal metering.
func TestProxyWarmupIsLocalAndUnmetered(t *testing.T) {
	upstreamHits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"real"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer up.Close()
	p, err := NewProxy(up.URL+"/v1", "k", Price{Input: 1, Output: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	post := func(body string) string {
		resp, err := http.Post(p.BaseURL()+"/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	p.BeginWarmup()
	if got := post(`{"model":"m","stream":false}`); !strings.Contains(got, `"content":"ok"`) {
		t.Fatalf("buffered warm-up reply: %s", got)
	}
	if got := post(`{"model":"m","stream":true}`); !strings.Contains(got, `"content":"ok"`) || !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("streamed warm-up reply: %s", got)
	}
	if upstreamHits != 0 || p.SpentUSD() != 0 {
		t.Fatalf("warm-up must not reach the upstream or cost anything: hits=%d spent=%v", upstreamHits, p.SpentUSD())
	}

	p.Begin("s", 5)
	if got := post(`{"model":"m"}`); !strings.Contains(got, "real") {
		t.Fatalf("after Begin the proxy must forward again: %s", got)
	}
	// Usage is recorded when the proxy finishes relaying the body, just after the client has it.
	var u Usage
	for i := 0; i < 200; i++ {
		if u = p.End("s"); u.OutputTokens == 5 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if upstreamHits != 1 || u.Calls != 1 || u.OutputTokens != 5 {
		t.Fatalf("metering after warm-up: hits=%d usage=%+v", upstreamHits, u)
	}
}
