package dag

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// chatToolCall is one function call in a chat completion message.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatCompletion is the subset of an OpenAI-compatible chat completion soma reads. Streaming and
// non-streaming responses are both normalised into it.
type chatCompletion struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

type chatUsage struct {
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	Cost                float64 `json:"cost"`
	PromptTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OpenCode                 struct {
		Tokens struct {
			CacheRead int `json:"cache_read"`
		} `json:"tokens"`
	} `json:"opencode"`
}

// streamIdleTimeout is how long a streaming response may go without delivering any bytes before
// the request is abandoned. Unlike a whole-request timeout it never cuts off a long generation
// that is still making progress.
const streamIdleTimeout = 180 * time.Second

// errStreamStalled reports a streaming response that stopped delivering data.
var errStreamStalled = errors.New("model stream stalled")

// readChatStream consumes a server-sent-events chat completion stream and assembles the final
// message. cancel aborts the underlying request; it is invoked when no data arrives for idle.
func readChatStream(body io.Reader, cancel context.CancelFunc, idle time.Duration, onDelta func(string)) (*chatCompletion, error) {
	if idle <= 0 {
		idle = streamIdleTimeout
	}
	stalled := false
	watchdog := time.AfterFunc(idle, func() {
		stalled = true
		cancel()
	})
	defer watchdog.Stop()

	type toolAcc struct {
		id, typ, name string
		args          strings.Builder
	}
	var (
		content, reasoning strings.Builder
		finish             string
		usage              chatUsage
		tools              = map[int]*toolAcc{}
		sawData            bool
	)

	reader := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		watchdog.Reset(idle)
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(data, []byte("[DONE]")) {
				break
			}
			if len(data) > 0 {
				sawData = true
				var chunk struct {
					Choices []struct {
						FinishReason string `json:"finish_reason"`
						Delta        struct {
							Content          string `json:"content"`
							ReasoningContent string `json:"reasoning_content"`
							Reasoning        string `json:"reasoning"`
							ToolCalls        []struct {
								Index    int    `json:"index"`
								ID       string `json:"id"`
								Type     string `json:"type"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
					Usage *chatUsage `json:"usage"`
					Error *struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if jErr := json.Unmarshal(data, &chunk); jErr == nil {
					if chunk.Error != nil && chunk.Error.Message != "" {
						return nil, fmt.Errorf("upstream stream error: %s", chunk.Error.Message)
					}
					if chunk.Usage != nil {
						usage = *chunk.Usage
					}
					for _, ch := range chunk.Choices {
						if ch.FinishReason != "" {
							finish = ch.FinishReason
						}
						if ch.Delta.Content != "" {
							content.WriteString(ch.Delta.Content)
							if onDelta != nil {
								onDelta(ch.Delta.Content)
							}
						}
						reasoning.WriteString(ch.Delta.ReasoningContent)
						if ch.Delta.ReasoningContent == "" {
							reasoning.WriteString(ch.Delta.Reasoning)
						}
						for _, tc := range ch.Delta.ToolCalls {
							acc := tools[tc.Index]
							if acc == nil {
								acc = &toolAcc{}
								tools[tc.Index] = acc
							}
							if tc.ID != "" {
								acc.id = tc.ID
							}
							if tc.Type != "" {
								acc.typ = tc.Type
							}
							if tc.Function.Name != "" {
								acc.name = tc.Function.Name
							}
							acc.args.WriteString(tc.Function.Arguments)
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if stalled {
				return nil, fmt.Errorf("%w: no data for %s", errStreamStalled, idle)
			}
			return nil, err
		}
	}
	if !sawData {
		return nil, errors.New("model stream ended without any data")
	}

	out := &chatCompletion{Usage: usage}
	out.Choices = make([]struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	}, 1)
	out.Choices[0].FinishReason = finish
	out.Choices[0].Message.Content = content.String()
	out.Choices[0].Message.ReasoningContent = reasoning.String()
	idxs := make([]int, 0, len(tools))
	for i := range tools {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		acc := tools[i]
		var tc chatToolCall
		tc.ID = acc.id
		tc.Type = acc.typ
		if tc.Type == "" {
			tc.Type = "function"
		}
		tc.Function.Name = acc.name
		tc.Function.Arguments = acc.args.String()
		out.Choices[0].Message.ToolCalls = append(out.Choices[0].Message.ToolCalls, tc)
	}
	return out, nil
}

// wantsExplicitCacheControl reports models that only cache prompt prefixes marked with
// cache_control (Anthropic-family models, including through OpenAI-compatible gateways). Other
// providers cache prefixes automatically and ignore or reject the marker.
func wantsExplicitCacheControl(modelID string) bool {
	m := strings.ToLower(modelID)
	return strings.Contains(m, "claude") || strings.Contains(m, "anthropic")
}

// markCacheBreakpoints tags the frozen system prompt (Layer 0+1) and the opening user turn as
// cacheable prefixes for providers that need explicit markers.
func markCacheBreakpoints(msgs []map[string]any) {
	marked := 0
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		if role != "system" && role != "user" {
			continue
		}
		text, ok := msg["content"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		msg["content"] = []map[string]any{{
			"type":          "text",
			"text":          text,
			"cache_control": map[string]string{"type": "ephemeral"},
		}}
		marked++
		if marked == 2 {
			return
		}
	}
}
