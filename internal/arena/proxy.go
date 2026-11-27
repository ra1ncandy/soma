// Package arena runs soma and other agent harnesses on identical tasks with the same model and
// reports only measured results. Both contestants talk to the model through a metering reverse
// proxy, so call counts, tokens and cost are counted by the harness, not self-reported, and the
// same call budget and spend cap apply to both.
package arena

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Usage is what the proxy measured for one contestant run.
type Usage struct {
	Calls int `json:"model_calls"`
	// InputTokens is every prompt token, including the cached ones.
	InputTokens  int `json:"input_tokens"`
	CachedTokens int `json:"cached_tokens"`
	// CacheWriteTokens are prompt tokens written to the cache (billed above the input rate on
	// APIs with explicit caching, such as Anthropic's).
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Refused          int     `json:"refused_calls"`
}

// Price is USD per million tokens, used to enforce the spend cap. CacheWrite defaults to 1.25x
// Input when zero (the usual premium; erring high keeps the spend cap safe).
type Price struct {
	Input, Cached, CacheWrite, Output float64
}

func (p Price) cacheWrite() float64 {
	if p.CacheWrite > 0 {
		return p.CacheWrite
	}
	return 1.25 * p.Input
}

// Proxy is a metering reverse proxy in front of an OpenAI-compatible endpoint.
type Proxy struct {
	upstream *url.URL
	apiKey   string
	price    Price
	capUSD   float64

	mu       sync.Mutex
	session  string
	maxCalls int
	usage    map[string]*Usage
	began    time.Time
	warming  bool
	// forceAnthropic selects Anthropic mode for an upstream that is not api.anthropic.com (tests).
	forceAnthropic bool
	calls          map[string][]CallRecord
	totalUSD       float64

	ln  net.Listener
	srv *http.Server
}

// NewProxy starts a proxy on 127.0.0.1 that forwards to upstreamBase (e.g. https://api.x/v1) with
// apiKey, refusing calls once a session's call budget or the global spend cap is reached.
func NewProxy(upstreamBase, apiKey string, price Price, spendCapUSD float64) (*Proxy, error) {
	u, err := url.Parse(strings.TrimRight(upstreamBase, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("arena: invalid upstream base URL %q", upstreamBase)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{upstream: u, apiKey: apiKey, price: price, capUSD: spendCapUSD, ln: ln, usage: map[string]*Usage{}, calls: map[string][]CallRecord{}}
	p.srv = &http.Server{Handler: p}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// BaseURL is the OpenAI-compatible base URL contestants should use.
func (p *Proxy) BaseURL() string { return "http://" + p.ln.Addr().String() + "/v1" }

// Close stops the proxy.
func (p *Proxy) Close() error { return p.srv.Close() }

// Begin starts metering a new session with its own call budget.
func (p *Proxy) Begin(session string, maxCalls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = session
	p.maxCalls = maxCalls
	p.warming = false
	p.began = time.Now()
	p.usage[session] = &Usage{}
}

// BeginWarmup puts the proxy in warm-up mode until the next Begin: model calls are answered
// locally with a fixed one-word reply, never forwarded, and nothing is metered. It lets a
// contestant finish its first-run setup before an attempt is timed.
func (p *Proxy) BeginWarmup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = ""
	p.warming = true
}

// End returns the measured usage of a session.
func (p *Proxy) End(session string) Usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u := p.usage[session]; u != nil {
		return *u
	}
	return Usage{}
}

// Calls returns the per-call records of a session, in completion order.
func (p *Proxy) Calls(session string) []CallRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]CallRecord(nil), p.calls[session]...)
}

// SpentUSD is the total measured spend so far.
func (p *Proxy) SpentUSD() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.totalUSD
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if p.warming {
		p.mu.Unlock()
		serveWarmup(w, r)
		return
	}
	u := p.usage[p.session]
	refuse := ""
	switch {
	case u == nil:
		refuse = "no active arena session"
	case p.capUSD > 0 && p.totalUSD >= p.capUSD:
		refuse = fmt.Sprintf("arena spend cap of $%.2f reached", p.capUSD)
	case p.maxCalls > 0 && u.Calls >= p.maxCalls && isModelCall(r):
		refuse = fmt.Sprintf("arena call budget of %d model calls exhausted", p.maxCalls)
	}
	if refuse != "" && u != nil {
		u.Refused++
	}
	if refuse == "" && isModelCall(r) {
		u.Calls++
	}
	session := p.session
	began := p.began
	sessionHeader := upstreamSessionID(session)
	p.mu.Unlock()
	if refuse != "" {
		// 400, not 429: agents must stop, not wait and retry.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": refuse, "type": "arena_budget"}})
		return
	}

	var rec *CallRecord
	if isModelCall(r) && r.Body != nil {
		// Buffer the request so it can be summarised for diagnosis; it is forwarded unchanged.
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "arena: cannot read request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		rec = summarizeRequest(body)
	}
	started := time.Now()

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.upstream)
			// Contestants call {proxy}/v1/...; the upstream base already ends in its own version path.
			// Hermes only treats a custom base URL as Anthropic-native when its path ends in
			// /anthropic, so that prefix is accepted too.
			pr.Out.URL.Path = strings.TrimRight(p.upstream.Path, "/") + strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/anthropic"), "/v1")
			pr.Out.Host = p.upstream.Host
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			switch {
			case p.apiKey == "":
			case p.anthropicUpstream():
				// The Anthropic API takes its key in x-api-key and requires a version header.
				pr.Out.Header.Set("X-Api-Key", p.apiKey)
				if pr.Out.Header.Get("Anthropic-Version") == "" {
					pr.Out.Header.Set("Anthropic-Version", "2023-06-01")
				}
			default:
				pr.Out.Header.Set("Authorization", "Bearer "+p.apiKey)
			}
			pr.Out.Header.Del("Accept-Encoding") // keep bodies readable for metering
			// Some gateways (OpenCode Go) reject requests without a session id. Give every trial a
			// stable one unless the contestant sent its own.
			if pr.Out.Header.Get("x-opencode-session") == "" {
				pr.Out.Header.Set("x-opencode-session", sessionHeader)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// Only model calls are metered; metadata probes (model lists, capability lookups)
			// pass through uncounted.
			if isModelCall(r) {
				status := resp.StatusCode
				resp.Body = &meteringBody{rc: resp.Body, sse: strings.Contains(resp.Header.Get("Content-Type"), "event-stream"),
					onDone: func(body []byte, sse bool) {
						in, cached, write, out := extractUsage(body, sse)
						p.record(in, cached, write, out)
						if rec == nil {
							return
						}
						rec.Status = status
						rec.StartMS = started.Sub(began).Milliseconds()
						rec.LatencyMS = time.Since(started).Milliseconds()
						rec.InputTokens, rec.CachedTokens, rec.OutputTokens = in, cached, out
						summarizeResponse(rec, body, sse)
						p.mu.Lock()
						rec.Seq = len(p.calls[session]) + 1
						p.calls[session] = append(p.calls[session], *rec)
						p.mu.Unlock()
					}}
			}
			return nil
		},
		FlushInterval: -1,
	}
	rp.ServeHTTP(w, r)
}

// serveWarmup answers a warm-up request without contacting the upstream: a canned completion for
// model calls, an empty list for anything else.
func serveWarmup(w http.ResponseWriter, r *http.Request) {
	if !isModelCall(r) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	_ = json.Unmarshal(body, &req)
	if strings.HasSuffix(r.URL.Path, "/messages") {
		serveAnthropicWarmup(w, req.Model, req.Stream)
		return
	}
	usage := map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "arena-warmup", "object": "chat.completion", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(choices []map[string]any, withUsage bool) {
		c := map[string]any{"id": "arena-warmup", "object": "chat.completion.chunk", "model": req.Model, "choices": choices}
		if withUsage {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}
	chunk([]map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": nil}}, false)
	chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, false)
	chunk([]map[string]any{}, true)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// serveAnthropicWarmup is the warm-up reply in Anthropic Messages shape.
func serveAnthropicWarmup(w http.ResponseWriter, model string, stream bool) {
	usage := map[string]int{"input_tokens": 1, "output_tokens": 1}
	msg := map[string]any{
		"id": "msg_arena_warmup", "type": "message", "role": "assistant", "model": model,
		"content": []map[string]any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage,
	}
	if !stream {
		msg["content"] = []map[string]any{{"type": "text", "text": "ok"}}
		msg["stop_reason"] = "end_turn"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msg)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, payload map[string]any) {
		payload["type"] = name
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	event("message_start", map[string]any{"message": msg})
	event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}})
	event("content_block_stop", map[string]any{"index": 0})
	event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	event("message_stop", map[string]any{})
}

// anthropicUpstream reports whether the proxy forwards to Anthropic's own API.
func (p *Proxy) anthropicUpstream() bool {
	return p.forceAnthropic || strings.EqualFold(p.upstream.Hostname(), "api.anthropic.com")
}

// AnthropicUpstream reports whether contestants must speak the Anthropic Messages protocol.
func (p *Proxy) AnthropicUpstream() bool { return p.anthropicUpstream() }

// upstreamSessionID is a stable, header-safe session id for one arena trial.
func upstreamSessionID(session string) string {
	sum := sha256.Sum256([]byte(session))
	return "ses_" + hex.EncodeToString(sum[:12])
}

func isModelCall(r *http.Request) bool {
	return r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/chat/completions") || strings.HasSuffix(r.URL.Path, "/responses") || strings.HasSuffix(r.URL.Path, "/messages"))
}

func (p *Proxy) record(in, cached, write, out int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.usage[p.session]
	if u == nil {
		return
	}
	u.InputTokens += in
	u.CachedTokens += cached
	u.CacheWriteTokens += write
	u.OutputTokens += out
	cost := (float64(in-cached-write)*p.price.Input + float64(cached)*p.price.Cached +
		float64(write)*p.price.cacheWrite() + float64(out)*p.price.Output) / 1e6
	u.CostUSD += cost
	p.totalUSD += cost
}

// meteringBody passes the response through unchanged and extracts token usage from it (a JSON
// body, or the usage chunk of an SSE stream) when it is closed.
type meteringBody struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	sse    bool
	onDone func(body []byte, sse bool)
	done   bool
}

func (m *meteringBody) Read(b []byte) (int, error) {
	n, err := m.rc.Read(b)
	if n > 0 && m.buf.Len() < 32<<20 {
		m.buf.Write(b[:n])
	}
	return n, err
}

func (m *meteringBody) Close() error {
	if !m.done {
		m.done = true
		m.onDone(m.buf.Bytes(), m.sse)
	}
	return m.rc.Close()
}

// usageJSON covers the usage objects of the OpenAI chat, OpenAI Responses and Anthropic Messages
// APIs. OpenAI counts cached tokens inside prompt/input tokens; Anthropic reports uncached input,
// cache reads and cache writes as three separate numbers.
type usageJSON struct {
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	PromptTokensDetails      struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u usageJSON) split() (in, cached, write, out int) {
	in = u.PromptTokens + u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	cached = u.PromptTokensDetails.CachedTokens + u.InputTokensDetails.CachedTokens + u.CacheReadInputTokens
	write = u.CacheCreationInputTokens
	out = u.CompletionTokens + u.OutputTokens
	return
}

func extractUsage(body []byte, sse bool) (in, cached, write, out int) {
	if !sse {
		var v struct {
			Usage usageJSON `json:"usage"`
		}
		if json.Unmarshal(body, &v) == nil {
			return v.Usage.split()
		}
		return 0, 0, 0, 0
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var v struct {
			Type     string     `json:"type"`
			Usage    *usageJSON `json:"usage"`
			Response *struct {
				Usage *usageJSON `json:"usage"`
			} `json:"response"`
			Message *struct {
				Usage *usageJSON `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(data), &v) != nil {
			continue
		}
		switch {
		case v.Message != nil && v.Message.Usage != nil:
			// Anthropic message_start: the prompt side of the bill.
			in, cached, write, out = v.Message.Usage.split()
		case v.Type == "message_delta" && v.Usage != nil:
			// Anthropic message_delta: cumulative output tokens (and, when present, the same
			// prompt numbers again); never let it erase what message_start reported.
			i, c, w, o := v.Usage.split()
			if i > in {
				in, cached, write = i, c, w
			}
			if o > out {
				out = o
			}
		case v.Usage != nil:
			in, cached, write, out = v.Usage.split()
		}
		if v.Response != nil && v.Response.Usage != nil {
			in, cached, write, out = v.Response.Usage.split()
		}
	}
	return
}
