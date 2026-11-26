package dag

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// configuredTemperature reads SOMA_TEMPERATURE; unset means "use the provider's default".
func configuredTemperature() (float64, bool) {
	s := strings.TrimSpace(os.Getenv("SOMA_TEMPERATURE"))
	if s == "" {
		return 0, false
	}
	t, err := strconv.ParseFloat(s, 64)
	if err != nil || t < 0 || t > 2 {
		return 0, false
	}
	return t, true
}

// DefaultReasoningEffort is what soma asks of reasoning models unless configured otherwise. An
// agent loop makes many small decisions and can check them against tool results, so long hidden
// deliberation on every turn mostly buys output tokens and latency: in the arena's hard tier,
// "low" cut output tokens and wall time by more than half with no loss in pass rate.
const DefaultReasoningEffort = "low"

// effectiveReasoningEffort resolves the effort to request: the explicit setting, then
// SOMA_REASONING_EFFORT, then DefaultReasoningEffort. "provider" (or "default") means "send
// nothing and let the provider decide".
func effectiveReasoningEffort(explicit string) string {
	e := strings.ToLower(strings.TrimSpace(explicit))
	if e == "" {
		e = strings.ToLower(strings.TrimSpace(os.Getenv("SOMA_REASONING_EFFORT")))
	}
	switch e {
	case "":
		return DefaultReasoningEffort
	case "provider", "default":
		return ""
	}
	return e
}

// rejectsReasoningEffort reports whether a 4xx body says the endpoint or model does not accept a
// reasoning-effort setting, so the caller can retry without it.
func rejectsReasoningEffort(status int, body []byte) bool {
	if status != 400 && status != 404 && status != 422 {
		return false
	}
	b := strings.ToLower(string(body))
	return strings.Contains(b, "reasoning_effort") || strings.Contains(b, "reasoning.effort") ||
		(strings.Contains(b, "reasoning") && (strings.Contains(b, "unsupported") || strings.Contains(b, "not supported") ||
			strings.Contains(b, "unrecognized") || strings.Contains(b, "unknown") || strings.Contains(b, "extra inputs")))
}

// OutputTruncatedError means the model spent its whole output budget (often on hidden reasoning)
// without emitting any text or tool call. The executor treats it as a wasted turn and tells the
// model, instead of failing the run.
type OutputTruncatedError struct {
	MaxTokens int
	Reason    string
}

func (e *OutputTruncatedError) Error() string {
	return fmt.Sprintf("model output truncated at max_tokens=%d before emitting content or tool calls %s", e.MaxTokens, e.Reason)
}

// wireToolCall is a tool call as decoded from any provider protocol.
type wireToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// finishGenerateResponse fills the protocol-independent part of a GenerateResponse: assistant
// text, reasoning digest, tool call specs (effect class, cited pages, stable call IDs) and the
// state hash used by the stall breaker. Every provider adapter ends with it.
func finishGenerateResponse(gen *GenerateResponse, req GenerateRequest, content, reasoningContent string, calls []wireToolCall) *GenerateResponse {
	content = strings.TrimSpace(content)
	gen.AssistantContent = content
	gen.ReasoningContent = strings.TrimSpace(reasoningContent)
	reasoning := content
	if reasoning == "" {
		reasoning = gen.ReasoningContent
	}
	if r := []rune(reasoning); len(r) > 280 {
		reasoning = string(r[:277]) + "..."
	}

	if len(calls) == 0 {
		gen.Reasoning = reasoning
		gen.Done = true
		gen.FinalSummary = content
		gen.StateHash = shortHashHex([]byte("done|" + content))
		return gen
	}

	var specs []ToolCallSpec
	var hashParts, names []string
	for idx, c := range calls {
		callID := strings.TrimSpace(c.ID)
		if callID == "" {
			callID = fmt.Sprintf("call_e%d_t%d_%d", req.Frame.Epoch, req.TurnIndex, idx)
		}
		argsJSON := strings.TrimSpace(c.Arguments)
		if argsJSON == "" {
			argsJSON = "{}"
		}
		var refs []string
		var argMap map[string]any
		if json.Unmarshal([]byte(argsJSON), &argMap) == nil {
			if pid, ok := argMap["page_id"].(string); ok && strings.TrimSpace(pid) != "" {
				refs = append(refs, strings.TrimPrefix(strings.TrimSpace(pid), "@"))
			}
		}
		specs = append(specs, ToolCallSpec{
			CallID:            callID,
			Name:              c.Name,
			ArgsJSON:          argsJSON,
			EffectClass:       inferToolEffectClass(c.Name),
			ReferencedPageIDs: refs,
		})
		hashParts = append(hashParts, c.Name+"|"+argsJSON)
		names = append(names, c.Name)
	}
	if reasoning == "" {
		reasoning = fmt.Sprintf("invoke %s", strings.Join(names, ", "))
	}
	gen.Reasoning = reasoning
	gen.Done = false
	gen.ToolCalls = specs
	gen.ToolCall = &gen.ToolCalls[0]
	gen.StateHash = shortHashHex([]byte(strings.Join(hashParts, ";")))
	return gen
}
