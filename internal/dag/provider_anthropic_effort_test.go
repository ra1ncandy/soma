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

func anthropicSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		_, _ = io.WriteString(w, "event: x\ndata: "+e+"\n\n")
	}
}

func TestAnthropicEffortMapping(t *testing.T) {
	t.Setenv("SOMA_REASONING_EFFORT", "")
	for in, want := range map[string]string{
		"":         "low", // soma's default is minimal, which the Messages API does not have
		"minimal":  "low",
		"none":     "low", // thinking cannot be turned off on current models
		"low":      "low",
		"Medium":   "medium",
		"xhigh":    "xhigh",
		"max":      "max",
		"provider": "",
		"bogus":    "",
	} {
		if got := anthropicEffort(in); got != want {
			t.Errorf("anthropicEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// The adapter asks for low effort by default, skips thinking blocks in the reply, and stops
// sending the setting to a model that rejects it.
func TestAnthropicProviderEffortThinkingAndFallback(t *testing.T) {
	t.Setenv("SOMA_REASONING_EFFORT", "")
	reject := false
	var efforts []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		oc, _ := body["output_config"].(map[string]any)
		efforts = append(efforts, oc["effort"])
		if _, hasThinking := body["thinking"]; hasThinking {
			t.Errorf("the adapter must leave thinking to the model's adaptive default")
		}
		if reject && oc != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"output_config.effort: this model does not support the effort parameter"}}`)
			return
		}
		anthropicSSE(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(srv.URL, "k")
	resp, err := p.Generate(context.Background(), GenerateRequest{ModelID: "claude-haiku-5-5", Frame: anthropicTestFrame()})
	if err != nil || !resp.Done || resp.FinalSummary != "done" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(efforts) != 1 || efforts[0] != "low" {
		t.Fatalf("default effort sent = %v, want [low]", efforts)
	}

	reject, efforts = true, nil
	p2 := NewAnthropicProvider(srv.URL, "k")
	for i := 0; i < 2; i++ {
		if _, err := p2.Generate(context.Background(), GenerateRequest{ModelID: "claude-haiku-4-5", Frame: anthropicTestFrame()}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if len(efforts) != 3 || efforts[0] != "low" || efforts[1] != nil || efforts[2] != nil {
		t.Fatalf("want [low, none, none] (retried without, then remembered), got %v", efforts)
	}
}

// A classifier refusal arrives as HTTP 200 with stop_reason "refusal"; it must not look like a
// finished run.
func TestAnthropicProviderRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		anthropicSSE(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}`,
		)
	}))
	defer srv.Close()
	_, err := NewAnthropicProvider(srv.URL, "k").Generate(context.Background(), GenerateRequest{ModelID: "m", Frame: anthropicTestFrame()})
	if err == nil || !strings.Contains(err.Error(), "refusal") {
		t.Fatalf("want a refusal error, got %v", err)
	}
}

func TestAnthropicProtocolOptInByFragment(t *testing.T) {
	if got := ProtocolForEndpoint("http://127.0.0.1:9000/v1#anthropic"); got != ProtocolAnthropic {
		t.Fatalf("protocol = %s", got)
	}
	p, ok := NewProvider("http://127.0.0.1:9000/v1#anthropic", "k").(*AnthropicProvider)
	if !ok || p.BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("provider = %#v", p)
	}
	if _, ok := NewProvider("http://127.0.0.1:9000/v1", "k").(*OpenAICompatProvider); !ok {
		t.Fatal("without the fragment a proxy URL stays OpenAI-compatible")
	}
}
