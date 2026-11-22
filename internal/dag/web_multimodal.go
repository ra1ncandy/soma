package dag

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)[^>]*>.*?</(script|style|noscript|svg|head)>`)
	blockBreakRe  = regexp.MustCompile(`(?i)</?(p|div|li|tr|h[1-6]|br|section|article|header|footer|blockquote|pre)[^>]*>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	ddgLinkRe     = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe  = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
)

// CleanHTMLToText converts raw HTML bytes into readable plain text while stripping scripts and tags.
func CleanHTMLToText(raw []byte) string {
	s := string(raw)
	s = scriptStyleRe.ReplaceAllString(s, " ")
	s = blockBreakRe.ReplaceAllString(s, "\n")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)

	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) == 0 {
			continue
		}
		out = append(out, strings.Join(fields, " "))
	}
	return strings.Join(out, "\n")
}

// ExecuteWebExtract fetches targetURL, extracts readable text, and marks TrustTier="untrusted_external"
// so any spilled @page is tracked by soma's provenance taint engine.
func ExecuteWebExtract(ctx context.Context, client *http.Client, targetURL string) (ToolOutput, error) {
	targetURL = strings.TrimSpace(targetURL)
	parsed, err := url.Parse(targetURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ToolOutput{}, fmt.Errorf("web_extract requires valid http/https URL, got %q", targetURL)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return ToolOutput{}, err
	}
	req.Header.Set("User-Agent", "soma-web-extract/0.4")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return ToolOutput{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return ToolOutput{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolOutput{}, fmt.Errorf("web_extract HTTP %d from %s", resp.StatusCode, targetURL)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var text string
	if strings.Contains(ct, "html") || bytes.Contains(bytes.ToLower(raw[:minInt(len(raw), 512)]), []byte("<html")) {
		text = CleanHTMLToText(raw)
	} else {
		text = string(raw)
	}
	body := fmt.Sprintf("url: %s\nstatus: %d\n\n%s", targetURL, resp.StatusCode, strings.TrimSpace(text))
	return ToolOutput{
		NameHint:  "web_extract.txt",
		Content:   []byte(body),
		TrustTier: "untrusted_external",
	}, nil
}

// ExecuteWebSearch queries a search endpoint (JSON search API or DuckDuckGo HTML fallback) and
// returns results tagged as TrustTier="untrusted_external".
func ExecuteWebSearch(ctx context.Context, client *http.Client, searchEndpoint, query string, maxResults int) (ToolOutput, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return ToolOutput{}, errors.New("web_search requires non-empty query")
	}
	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 15 {
		maxResults = 15
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if searchEndpoint == "" {
		searchEndpoint = strings.TrimSpace(os.Getenv("SOMA_SEARCH_URL"))
	}
	if searchEndpoint == "" {
		searchEndpoint = "https://html.duckduckgo.com/html/"
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	u, err := url.Parse(searchEndpoint)
	if err != nil {
		return ToolOutput{}, err
	}
	q := u.Query()
	q.Set("q", query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ToolOutput{}, err
	}
	req.Header.Set("User-Agent", "soma-web-search/0.4")

	resp, err := client.Do(req)
	if err != nil {
		return ToolOutput{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return ToolOutput{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolOutput{}, fmt.Errorf("web_search HTTP %d", resp.StatusCode)
	}

	// 1. Check JSON response format: {"results":[{"title":"...","url":"...","snippet":"..."}]}
	var jsonParsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &jsonParsed) == nil && len(jsonParsed.Results) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "web_search results for %q:\n", query)
		limit := minInt(len(jsonParsed.Results), maxResults)
		for i := 0; i < limit; i++ {
			r := jsonParsed.Results[i]
			snip := r.Snippet
			if snip == "" {
				snip = r.Content
			}
			fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL), strings.TrimSpace(snip))
		}
		return ToolOutput{
			NameHint:  "web_search.txt",
			Content:   []byte(strings.TrimRight(sb.String(), "\n")),
			TrustTier: "untrusted_external",
		}, nil
	}

	// 2. Parse HTML search results (DuckDuckGo HTML lite or generic HTML fallback).
	links := ddgLinkRe.FindAllSubmatch(raw, maxResults)
	snips := ddgSnippetRe.FindAllSubmatch(raw, maxResults)
	if len(links) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "web_search results for %q:\n", query)
		for i, m := range links {
			href := html.UnescapeString(string(m[1]))
			title := CleanHTMLToText(m[2])
			var snip string
			if i < len(snips) {
				snip = CleanHTMLToText(snips[i][1])
			}
			fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n", i+1, title, href, snip)
		}
		return ToolOutput{
			NameHint:  "web_search.txt",
			Content:   []byte(strings.TrimRight(sb.String(), "\n")),
			TrustTier: "untrusted_external",
		}, nil
	}

	// 3. Fallback cleaned HTML text.
	cleaned := CleanHTMLToText(raw)
	if len(cleaned) > 4000 {
		cleaned = cleaned[:4000]
	}
	return ToolOutput{
		NameHint:  "web_search.txt",
		Content:   []byte(fmt.Sprintf("web_search results for %q:\n%s", query, cleaned)),
		TrustTier: "untrusted_external",
	}, nil
}

// ExecuteVisionAnalyze inspects an image inside workspaceDir, decoding its format and pixel dimensions
// and optionally querying an OpenAI-compatible multimodal vision endpoint when configured.
func ExecuteVisionAnalyze(
	ctx context.Context,
	client *http.Client,
	visionBaseURL, apiKey, modelID, workspaceDir, relImagePath, prompt string,
) (ToolOutput, error) {
	target, err := resolveInsideWorkspace(workspaceDir, relImagePath)
	if err != nil {
		return ToolOutput{}, err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return ToolOutput{}, err
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Describe the contents, text, layout, and any anomalies in this image."
	}

	cfg, format, cfgErr := image.DecodeConfig(bytes.NewReader(raw))
	if cfgErr != nil {
		format = strings.TrimPrefix(strings.ToLower(filepath.Ext(relImagePath)), ".")
		if format == "" {
			format = "binary"
		}
	}
	mimeType := "image/png"
	switch format {
	case "jpeg", "jpg":
		mimeType = "image/jpeg"
	case "gif":
		mimeType = "image/gif"
	case "webp":
		mimeType = "image/webp"
	}

	metaHeader := fmt.Sprintf("image: %s | format=%s | dimensions=%dx%d | bytes=%d | sha256=%s",
		relImagePath, format, cfg.Width, cfg.Height, len(raw), sha256Hex(raw)[:12])

	if visionBaseURL == "" {
		visionBaseURL = strings.TrimSpace(os.Getenv("SOMA_VISION_URL"))
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("SOMA_VISION_KEY"))
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("SOMA_API_KEY"))
		}
	}
	if modelID == "" {
		modelID = strings.TrimSpace(os.Getenv("SOMA_VISION_MODEL"))
		if modelID == "" {
			modelID = "gpt-4o-mini"
		}
	}

	if visionBaseURL == "" {
		return ToolOutput{
			NameHint:  "vision_analyze.txt",
			Content:   []byte(fmt.Sprintf("%s\nprompt: %s\nstatus: local structural image inspection complete (set SOMA_VISION_URL for remote VLM captioning)", metaHeader, prompt)),
			TrustTier: "trusted_repo",
		}, nil
	}

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(raw))
	payload := map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
					{"type": "image_url", "image_url": map[string]string{"url": dataURI}},
				},
			},
		},
		"max_tokens": 512,
	}
	reqBytes, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(visionBaseURL, "/")+"/chat/completions", bytes.NewReader(reqBytes))
	if err != nil {
		return ToolOutput{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ToolOutput{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return ToolOutput{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolOutput{}, fmt.Errorf("vision_analyze HTTP %d: %s", resp.StatusCode, string(respBytes))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil || len(parsed.Choices) == 0 {
		return ToolOutput{}, fmt.Errorf("vision_analyze decode response: %w", err)
	}
	analysis := strings.TrimSpace(parsed.Choices[0].Message.Content)
	return ToolOutput{
		NameHint:  "vision_analyze.txt",
		Content:   []byte(fmt.Sprintf("%s\nanalysis:\n%s", metaHeader, analysis)),
		TrustTier: "trusted_repo",
	}, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
