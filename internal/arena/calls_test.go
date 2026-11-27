package arena

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

// Contained (the default), Hermes may write to the task workspace and its own state directory,
// and nowhere in the operator's home; with homeIsWorkspace the workspace is mounted at the home
// path. The test runs a shell in place of Hermes.
func TestHermesContainmentLive(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	// A stand-in "hermes" that runs its -z argument as a shell script.
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "hermes")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sh -c \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := HermesContestant{Bin: fake}
	marker := filepath.Join(realHome, ".arena-containment-test-marker")
	_ = os.Remove(marker)
	defer func() { _ = os.Remove(marker) }()

	for _, homeIsWorkspace := range []bool{false, true} {
		ws, state := t.TempDir(), t.TempDir()
		script := `echo in-cwd > made-in-cwd; echo in-home > "$HOME/.arena-containment-test-marker" 2>/dev/null; ` +
			`echo state > "$HERMES_HOME/s"; ls "$HOME" | wc -l; touch /usr/arena-x 2>/dev/null && echo USR-WRITABLE; true`
		// The supervisor runs the warm-up prompt, waits for a line, then runs the task prompt.
		cmd, err := c.command(state, ws, homeIsWorkspace, "true", script)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdin = strings.NewReader("go\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("homeIsWorkspace=%v: %v\n%s", homeIsWorkspace, err, out)
		}
		if !strings.Contains(string(out), hermesReady) {
			t.Fatalf("homeIsWorkspace=%v: supervisor never announced readiness:\n%s", homeIsWorkspace, out)
		}
		if strings.Contains(string(out), "USR-WRITABLE") {
			t.Errorf("homeIsWorkspace=%v: host files must be read-only", homeIsWorkspace)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("homeIsWorkspace=%v: the contestant wrote into the real home directory", homeIsWorkspace)
		}
		if _, err := os.Stat(filepath.Join(ws, "made-in-cwd")); err != nil {
			t.Errorf("homeIsWorkspace=%v: workspace writes must persist: %v", homeIsWorkspace, err)
		}
		if _, err := os.Stat(filepath.Join(state, "s")); err != nil {
			t.Errorf("homeIsWorkspace=%v: state directory writes must persist: %v", homeIsWorkspace, err)
		}
		_, inWorkspace := os.Stat(filepath.Join(ws, ".arena-containment-test-marker"))
		if homeIsWorkspace && inWorkspace != nil {
			t.Errorf("with homeIsWorkspace, a write to $HOME must land in the workspace")
		}
		if !homeIsWorkspace && inWorkspace == nil {
			t.Errorf("without homeIsWorkspace, $HOME must not be the workspace")
		}
	}
}

// A capped contestant that touches more memory than its limit is killed; one that stays under
// it runs normally.
func TestCapMemoryLive(t *testing.T) {
	if !MemoryCapAvailable() {
		t.Skip("no systemd user session for transient scopes")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	small := exec.Command(py, "-c", "print('fine')")
	capMemory(small, "256M")
	if out, err := small.CombinedOutput(); err != nil || !strings.Contains(string(out), "fine") {
		t.Fatalf("a small job must run under the cap: %v %s", err, out)
	}
	big := exec.Command(py, "-c", "b = b'x' * (700 * 1024 * 1024); print('SURVIVED')")
	capMemory(big, "256M")
	if out, err := big.CombinedOutput(); err == nil || strings.Contains(string(out), "SURVIVED") {
		t.Fatalf("a job over the cap must be killed, got err=%v out=%s", err, out)
	}
	uncapped := exec.Command(py, "-c", "print('x')")
	capMemory(uncapped, "")
	if filepath.Base(uncapped.Path) != filepath.Base(py) {
		t.Fatalf("an empty cap must leave the command alone, got %s", uncapped.Path)
	}
}

// With Anthropic upstream the proxy sends the real key as x-api-key (never as a bearer token and
// never the contestant's placeholder), accepts Hermes' /anthropic path prefix, and meters cache
// reads and writes at their own prices.
func TestProxyAnthropicUpstream(t *testing.T) {
	var gotPath, gotKey, gotAuth, gotVersion string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotAuth, gotVersion = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("Authorization"), r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":1000,\"cache_creation_input_tokens\":200,\"output_tokens\":1}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"fs_read\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"p\\\":1}\"}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":50}}\n\n")
	}))
	defer up.Close()
	p, err := NewProxy(up.URL+"/v1", "real-key", Price{Input: 1, Cached: 0.1, CacheWrite: 1.25, Output: 5}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	// The test server is not api.anthropic.com; force the mode the hostname would select.
	p.upstream.Host = "api.anthropic.com"
	if !p.AnthropicUpstream() {
		t.Fatal("api.anthropic.com must select Anthropic mode")
	}
	p.upstream.Host = strings.TrimPrefix(up.URL, "http://")
	p.forceAnthropic = true

	p.Begin("s", 5)
	root := strings.TrimSuffix(p.BaseURL(), "/v1")
	req, _ := http.NewRequest(http.MethodPost, root+"/anthropic/v1/messages", strings.NewReader(`{"model":"m","stream":true,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[]}`))
	req.Header.Set("x-api-key", "arena")
	req.Header.Set("Authorization", "Bearer arena")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "real-key" || gotAuth != "" || gotVersion == "" {
		t.Errorf("upstream auth: x-api-key=%q authorization=%q version=%q", gotKey, gotAuth, gotVersion)
	}
	var u Usage
	for i := 0; i < 200; i++ {
		if u = p.End("s"); u.OutputTokens == 50 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if u.InputTokens != 1300 || u.CachedTokens != 1000 || u.CacheWriteTokens != 200 || u.OutputTokens != 50 {
		t.Fatalf("usage = %+v", u)
	}
	want := (100*1.0 + 1000*0.1 + 200*1.25 + 50*5.0) / 1e6
	if d := u.CostUSD - want; d > 1e-12 || d < -1e-12 {
		t.Fatalf("cost = %v, want %v", u.CostUSD, want)
	}
	var calls []CallRecord
	for i := 0; i < 200 && len(calls) == 0; i++ { // appended just after the usage is recorded
		calls = p.Calls("s")
		time.Sleep(5 * time.Millisecond)
	}
	if len(calls) != 1 || calls[0].CacheBreakpoints != 1 || calls[0].FinishReason != "tool_use" || len(calls[0].ToolNames) != 1 || calls[0].ToolArgChars != 7 {
		t.Fatalf("call record = %+v", calls)
	}

	// The warm-up reply for a Messages request is Anthropic-shaped and never reaches the upstream.
	gotPath = ""
	p.BeginWarmup()
	wresp, err := http.Post(root+"/anthropic/v1/messages", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	wbody, _ := io.ReadAll(wresp.Body)
	_ = wresp.Body.Close()
	if gotPath != "" || !strings.Contains(string(wbody), `"type":"message_start"`) || !strings.Contains(string(wbody), `"text":"ok"`) {
		t.Fatalf("warm-up must be local and Anthropic-shaped: upstream path %q, body %s", gotPath, wbody)
	}
}

func TestPriceCacheWriteDefault(t *testing.T) {
	if got := (Price{Input: 2}).cacheWrite(); got != 2.5 {
		t.Fatalf("default cache-write price = %v, want 1.25x input", got)
	}
	if got := (Price{Input: 2, CacheWrite: 3}).cacheWrite(); got != 3 {
		t.Fatalf("explicit cache-write price = %v", got)
	}
}
