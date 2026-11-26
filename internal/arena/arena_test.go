package arena

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyMetersUsageAndEnforcesBudgets(t *testing.T) {
	var sawAuth string
	var sawSession string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawSession = r.Header.Get("x-opencode-session")
		if r.URL.Query().Get("stream") == "1" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":60}}}\n\ndata: [DONE]\n\n")
			return
		}
		if r.URL.Path != "/v9/chat/completions" {
			http.Error(w, "bad path "+r.URL.Path, 404)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50}}`)
	}))
	defer up.Close()
	p, err := NewProxy(up.URL+"/v9", "real-key", Price{Input: 1, Cached: 0.1, Output: 10}, 0.0016)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	post := func(q string) int {
		req, _ := http.NewRequest(http.MethodPost, p.BaseURL()+"/chat/completions"+q, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer contestant-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	p.Begin("a", 2)
	if c1, c2 := post(""), post("?stream=1"); c1 != 200 || c2 != 200 {
		t.Fatalf("proxied calls failed: %d %d", c1, c2)
	}
	if sawSession != upstreamSessionID("a") || !strings.HasPrefix(sawSession, "ses_") {
		t.Fatalf("x-opencode-session = %q, want stable ses_ id", sawSession)
	}
	if sawAuth != "Bearer real-key" {
		t.Fatalf("upstream auth = %q, contestant key must be replaced", sawAuth)
	}
	if code := post(""); code != http.StatusBadRequest {
		t.Fatalf("call over budget returned %d, want 400", code)
	}
	u := p.End("a")
	if u.Calls != 2 || u.InputTokens != 1100 || u.CachedTokens != 60 || u.OutputTokens != 57 || u.Refused != 1 {
		t.Fatalf("usage = %+v", u)
	}
	// cost: (1000*1 + 50*10 + 40*1 + 60*0.1 + 7*10)/1e6 = 0.001616 >= cap 0.0016
	p.Begin("b", 10)
	if code := post(""); code != http.StatusBadRequest {
		t.Fatalf("call over spend cap returned %d, want 400", code)
	}
}

// Every grader must reject the untouched seed and accept a correct solution, so a contestant
// that does nothing (or games the visible tests) cannot pass.
func TestGradersRejectSeedAndAcceptReference(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	reference := map[string]map[string]string{
		"fix-off-by-one": {"stats.py": strings.Replace(Tasks[0].Files["stats.py"], "range(len(values) - window)", "range(len(values) - window + 1)", 1)},
		"implement-slugify": {"text_utils.py": `import re, unicodedata
def slugify(text):
    text = text.replace("ß", "ss").replace("ẞ", "ss")
    text = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode().lower()
    return re.sub(r"[^a-z0-9]+", "-", text).strip("-")
`},
		"log-root-cause": {"answer.txt": "r041337\n"},
		"rename-across-files": {
			"billing/core.py":    strings.ReplaceAll(Tasks[3].Files["billing/core.py"], "calc_total", "compute_total"),
			"billing/invoice.py": strings.ReplaceAll(Tasks[3].Files["billing/invoice.py"], "calc_total", "compute_total"),
			"reports.py":         strings.ReplaceAll(Tasks[3].Files["reports.py"], "calc_total", "compute_total"),
		},
	}
	for _, task := range Tasks {
		t.Run(task.ID, func(t *testing.T) {
			dir := t.TempDir()
			if err := task.Seed(dir); err != nil {
				t.Fatal(err)
			}
			if ok, out := grade(context.Background(), task, dir); ok {
				t.Fatalf("seed passes the grader: %s", out)
			}
			ref, ok := reference[task.ID]
			if !ok {
				t.Fatalf("no reference solution for %s", task.ID)
			}
			for rel, body := range ref {
				if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if ok, out := grade(context.Background(), task, dir); !ok {
				t.Fatalf("reference solution fails the grader: %s", out)
			}
		})
	}
}

func TestWilsonInterval(t *testing.T) {
	lo, hi := wilson(8, 10)
	if lo < 0.49 || lo > 0.50 || hi < 0.94 || hi > 0.95 {
		t.Fatalf("wilson(8,10) = %.3f..%.3f", lo, hi)
	}
}
