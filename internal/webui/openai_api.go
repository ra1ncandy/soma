package webui

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ra1ncandy/soma/internal/dag"
)

// OpenAI-compatible API: any OpenAI client (SDKs, Open WebUI, editors) can use soma as a "model".
// Each request runs a soma task — sandboxed tools, journal, checkpoints, oracles — and returns
// the final answer. Conversations continue on the same branch when the client sends the
// X-Soma-Session header returned by the previous response (or "user" field), so follow-ups keep
// the full history and workspace instead of being re-sent and re-run.
//
// Auth: Authorization: Bearer $SOMA_API_KEY (or the dashboard token). With neither configured
// the API is disabled (fail closed).

const apiModelID = "soma"

func (s *Server) apiKey() string {
	if k := strings.TrimSpace(os.Getenv("SOMA_API_KEY_SERVER")); k != "" {
		return k
	}
	return s.activeAuthToken()
}

func (s *Server) serveOpenAIAPI(w http.ResponseWriter, r *http.Request, path string) {
	key := s.apiKey()
	if key == "" {
		writeOpenAIError(w, http.StatusForbidden, "the OpenAI-compatible API is disabled: set SOMA_API_KEY_SERVER (or a dashboard token) and send it as a bearer token")
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/v1/models":
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{{"id": apiModelID, "object": "model", "owned_by": "soma"}}})
	case r.Method == http.MethodPost && path == "/v1/chat/completions":
		s.handleOpenAIChat(w, r)
	default:
		writeOpenAIError(w, http.StatusNotFound, "unknown endpoint "+path)
	}
}

func writeOpenAIError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"message": msg, "type": "invalid_request_error"}})
}

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	User     string `json:"user"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func (s *Server) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var last, transcript strings.Builder
	for i, m := range req.Messages {
		text := strings.TrimSpace(messageText(m.Content))
		if text == "" {
			continue
		}
		if i == len(req.Messages)-1 && m.Role == "user" {
			last.WriteString(text)
			continue
		}
		fmt.Fprintf(&transcript, "%s: %s\n", m.Role, text)
	}
	if last.Len() == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "the last message must be a non-empty user message")
		return
	}
	ctx := r.Context()
	exec, accs, err := s.executorForRequest(ctx, "", "")
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer closeExecutor(exec)

	session := strings.TrimSpace(r.Header.Get("X-Soma-Session"))
	if session == "" {
		session = strings.TrimSpace(req.User)
	}
	branchID := ""
	if session != "" {
		if br, err := s.store.GetBranch(ctx, session); err == nil {
			branchID = br.ID
		}
	}
	if branchID != "" {
		if !s.stopActiveLoop(branchID, 30*time.Second) {
			writeOpenAIError(w, http.StatusConflict, "the session is busy")
			return
		}
		if err := exec.AppendFollowUpUserTurn(ctx, branchID, last.String()); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		goal := last.String()
		if transcript.Len() > 0 {
			goal = "Conversation so far:\n" + transcript.String() + "\nCurrent request: " + goal
		}
		_, brID, err := exec.StartRun(ctx, dag.RunRequest{Title: firstLineTrunc(last.String(), 64), WorkspacePath: s.cfg.WorkspaceDir, GoalSpec: goal, AccountIDs: accs})
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		branchID = brID
	}
	s.notify()

	created := time.Now().Unix()
	id := "chatcmpl-" + branchID
	w.Header().Set("X-Soma-Session", branchID)

	if req.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeOpenAIError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		send := func(delta map[string]any, finish any) {
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": apiModelID,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
			raw, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
		send(map[string]any{"role": "assistant"}, nil)
		streamed := false
		var mu sync.Mutex
		exec.OnDelta = func(bid, text string) {
			if bid != branchID {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			streamed = true
			send(map[string]any{"content": text}, nil)
		}
		res, runErr := exec.ExecuteBranch(ctx, branchID, s.defaultMaxTurns())
		mu.Lock()
		defer mu.Unlock()
		if runErr != nil {
			send(map[string]any{"content": "\n[soma error: " + runErr.Error() + "]"}, nil)
		} else if !streamed && res != nil && res.FinalSummary != "" {
			send(map[string]any{"content": res.FinalSummary}, nil)
		}
		send(map[string]any{}, "stop")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		s.notify()
		return
	}

	res, runErr := exec.ExecuteBranch(ctx, branchID, s.defaultMaxTurns())
	s.notify()
	if runErr != nil && (res == nil || res.FinalSummary == "") {
		writeOpenAIError(w, http.StatusBadGateway, runErr.Error())
		return
	}
	content := ""
	status := ""
	tokens := 0
	if res != nil {
		content, status, tokens = res.FinalSummary, string(res.Status), res.TotalTokens
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": apiModelID,
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"total_tokens": tokens},
		"soma":    map[string]any{"session": branchID, "status": status},
	})
}

func firstLineTrunc(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
