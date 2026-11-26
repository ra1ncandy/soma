package dag_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
)

// soma asks reasoning models for low effort unless told otherwise, and stops asking when the
// endpoint does not accept the setting.
func TestReasoningEffortDefaultOverrideAndFallback(t *testing.T) {
	type seen struct {
		effort any
		has    bool
	}
	serve := func(t *testing.T, reject bool) (*httptest.Server, *[]seen) {
		var got []seen
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			e, has := body["reasoning_effort"]
			got = append(got, seen{e, has})
			if has && reject {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "done"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
			})
		}))
		t.Cleanup(ts.Close)
		return ts, &got
	}
	gen := func(t *testing.T, p *dag.OpenAICompatProvider) {
		t.Helper()
		p.DisableStream = true
		if _, err := p.Generate(context.Background(), dag.GenerateRequest{ModelID: "m", Frame: compiler.CompiledFrame{Layer0: "s", Layer2: "u"}}); err != nil {
			t.Fatalf("generate: %v", err)
		}
	}

	t.Run("default is low", func(t *testing.T) {
		t.Setenv("SOMA_REASONING_EFFORT", "")
		ts, got := serve(t, false)
		gen(t, dag.NewOpenAICompatProvider(ts.URL, ""))
		if len(*got) != 1 || (*got)[0].effort != dag.DefaultReasoningEffort || dag.DefaultReasoningEffort != "low" {
			t.Fatalf("got %+v", *got)
		}
	})
	t.Run("environment overrides the default", func(t *testing.T) {
		t.Setenv("SOMA_REASONING_EFFORT", "High")
		ts, got := serve(t, false)
		gen(t, dag.NewOpenAICompatProvider(ts.URL, ""))
		if (*got)[0].effort != "high" {
			t.Fatalf("got %+v", *got)
		}
	})
	t.Run("explicit setting beats the environment", func(t *testing.T) {
		t.Setenv("SOMA_REASONING_EFFORT", "high")
		ts, got := serve(t, false)
		p := dag.NewOpenAICompatProvider(ts.URL, "")
		p.ReasoningEffort = "medium"
		gen(t, p)
		if (*got)[0].effort != "medium" {
			t.Fatalf("got %+v", *got)
		}
	})
	t.Run("provider leaves the choice to the endpoint", func(t *testing.T) {
		t.Setenv("SOMA_REASONING_EFFORT", "provider")
		ts, got := serve(t, false)
		gen(t, dag.NewOpenAICompatProvider(ts.URL, ""))
		if (*got)[0].has {
			t.Fatalf("reasoning_effort must be omitted, got %+v", *got)
		}
	})
	t.Run("an endpoint that rejects it is retried without and remembered", func(t *testing.T) {
		t.Setenv("SOMA_REASONING_EFFORT", "")
		ts, got := serve(t, true)
		p := dag.NewOpenAICompatProvider(ts.URL, "")
		gen(t, p)
		gen(t, p)
		if len(*got) != 3 || !(*got)[0].has || (*got)[1].has || (*got)[2].has {
			t.Fatalf("want [with, without, without], got %+v", *got)
		}
	})
}
