package vmem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ContentType identifies the structural previewer used when spilling an observation to @page.
type ContentType string

const (
	TypeLog     ContentType = "log"
	TypeCode    ContentType = "code"
	TypeJSON    ContentType = "json"
	TypeTabular ContentType = "tabular"
	TypeDiff    ContentType = "diff"
	TypeHTTP    ContentType = "http"
	TypeText    ContentType = "text"
)

var (
	codeDeclRe = regexp.MustCompile(`^(func\s|type\s|def\s|class\s|export\s|async\s+function\s|pub\s+fn\s|struct\s)`)
	logErrRe   = regexp.MustCompile(`(?i)(FAIL|ERROR|PANIC|DATA RACE|Traceback|Exception|exit status [1-9]|exit code [1-9])`)
	logPassRe  = regexp.MustCompile(`(?i)\b(PASS|PASSED|OK)\b`)
	exitCodeRe = regexp.MustCompile(`(?i)exit\s+(?:status|code)[:\s]+(\d+)`)
)

// DetectContentType infers ContentType from a filename/tool hint and raw content bytes.
func DetectContentType(nameHint string, content []byte) ContentType {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(nameHint)))
	switch ext {
	case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs", ".c", ".cpp", ".h", ".java", ".rb":
		return TypeCode
	case ".json", ".jsonl":
		return TypeJSON
	case ".csv", ".tsv", ".parquet":
		return TypeTabular
	case ".diff", ".patch":
		return TypeDiff
	case ".log":
		return TypeLog
	}

	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		return TypeJSON
	}
	sample := string(trimmed)
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	if strings.HasPrefix(sample, "HTTP/1.") || strings.HasPrefix(sample, "HTTP/2") {
		return TypeHTTP
	}
	if strings.Contains(sample, "diff --git ") || (strings.Contains(sample, "--- ") && strings.Contains(sample, "+++ ")) {
		return TypeDiff
	}
	if logErrRe.MatchString(sample) || strings.Contains(nameHint, "test") || strings.Contains(nameHint, "build") || strings.Contains(nameHint, "exec") {
		return TypeLog
	}
	lines := strings.Split(sample, "\n")
	if len(lines) >= 2 && strings.Count(lines[0], ",") >= 2 && strings.Count(lines[1], ",") == strings.Count(lines[0], ",") {
		return TypeTabular
	}
	return TypeText
}

// GeneratePreview builds a bounded (<= maxLines) structural summary tailored to ct.
func GeneratePreview(ct ContentType, content []byte, maxLines int) string {
	if maxLines <= 0 {
		maxLines = 15
	}
	raw := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var out []string
	switch ct {
	case TypeLog:
		out = previewLog(lines, len(content), maxLines)
	case TypeCode:
		out = previewCode(lines, len(content), maxLines)
	case TypeJSON:
		out = previewJSON(content, len(lines), len(content), maxLines)
	case TypeTabular:
		out = previewTabular(lines, len(content), maxLines)
	case TypeDiff:
		out = previewDiff(lines, len(content), maxLines)
	case TypeHTTP:
		out = previewHTTP(lines, len(content), maxLines)
	default:
		out = previewText(lines, len(content), maxLines)
	}

	if len(out) > maxLines {
		out = out[:maxLines]
	}
	return strings.Join(out, "\n")
}

func previewLog(lines []string, byteLen, maxLines int) []string {
	var passCount, errCount int
	var errLines []string
	exitCode := "unknown"

	for idx, line := range lines {
		if m := exitCodeRe.FindStringSubmatch(line); len(m) == 2 {
			exitCode = m[1]
		}
		if logPassRe.MatchString(line) {
			passCount++
		}
		if logErrRe.MatchString(line) {
			errCount++
			errLines = append(errLines, fmt.Sprintf("L%d: %s", idx+1, truncateLine(line, 140)))
		}
	}

	out := []string{
		fmt.Sprintf("log summary: %d lines, %d bytes, exit=%s, pass_hits=%d, err_hits=%d",
			len(lines), byteLen, exitCode, passCount, errCount),
	}

	if len(errLines) == 0 {
		return append(out, headTailSample(lines, maxLines-1)...)
	}

	budget := maxLines - 1
	if len(errLines) <= budget {
		return append(out, errLines...)
	}
	half := budget / 2
	out = append(out, errLines[:half]...)
	out = append(out, fmt.Sprintf("... (%d omitted error lines) ...", len(errLines)-budget+1))
	out = append(out, errLines[len(errLines)-(budget-half-1):]...)
	return out
}

func previewCode(lines []string, byteLen, maxLines int) []string {
	lang := detectCodeLang(lines)
	var decls []string
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if codeDeclRe.MatchString(trimmed) {
			decls = append(decls, fmt.Sprintf("L%d: %s", idx+1, truncateLine(trimmed, 120)))
		}
	}
	out := []string{
		fmt.Sprintf("code summary (%s): %d lines, %d bytes, %d top-level symbols",
			lang, len(lines), byteLen, len(decls)),
	}
	if len(decls) == 0 {
		return append(out, headTailSample(lines, maxLines-1)...)
	}
	if len(decls) > maxLines-1 {
		decls = decls[:maxLines-1]
	}
	return append(out, decls...)
}

func detectCodeLang(lines []string) string {
	sample := strings.Join(lines[:minInt(len(lines), 40)], "\n")
	switch {
	case strings.Contains(sample, "package ") && strings.Contains(sample, "func "):
		return "go"
	case strings.Contains(sample, "def ") && strings.Contains(sample, ":"):
		return "python"
	case strings.Contains(sample, "export ") || strings.Contains(sample, "function "):
		return "js/ts"
	case strings.Contains(sample, "fn ") && strings.Contains(sample, "pub "):
		return "rust"
	default:
		return "source"
	}
}

func previewJSON(content []byte, lineCount, byteLen, maxLines int) []string {
	var v any
	if err := json.Unmarshal(bytes.TrimSpace(content), &v); err != nil {
		return previewText(strings.Split(string(content), "\n"), byteLen, maxLines)
	}

	out := []string{fmt.Sprintf("json summary: %d lines, %d bytes", lineCount, byteLen)}
	switch typed := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, fmt.Sprintf("object keys (%d): %s", len(keys), truncateLine(strings.Join(keys, ", "), 140)))
		for _, k := range keys {
			if len(out) >= maxLines {
				break
			}
			out = append(out, fmt.Sprintf("  %s: %s", k, describeJSONValue(typed[k])))
		}
	case []any:
		out = append(out, fmt.Sprintf("array length: %d", len(typed)))
		if len(typed) > 0 {
			sample, _ := json.Marshal(typed[0])
			out = append(out, "first_item: "+truncateLine(string(sample), 140))
		}
	default:
		out = append(out, "value: "+truncateLine(fmt.Sprint(typed), 140))
	}
	return out
}

func describeJSONValue(v any) string {
	switch tv := v.(type) {
	case []any:
		return fmt.Sprintf("array[%d]", len(tv))
	case map[string]any:
		return fmt.Sprintf("object{%d keys}", len(tv))
	case string:
		return fmt.Sprintf("string(%q)", truncateLine(tv, 60))
	default:
		return fmt.Sprintf("%T(%v)", v, v)
	}
}

func previewTabular(lines []string, byteLen, maxLines int) []string {
	if len(lines) == 0 {
		return []string{"tabular summary: empty"}
	}
	cols := strings.Split(lines[0], ",")
	out := []string{
		fmt.Sprintf("tabular summary: %d rows (%d data), %d bytes, %d cols",
			len(lines), maxInt(0, len(lines)-1), byteLen, len(cols)),
		"header: " + truncateLine(lines[0], 140),
	}
	limit := minInt(len(lines), minInt(5, maxLines))
	for i := 1; i < limit; i++ {
		out = append(out, fmt.Sprintf("row %d: %s", i, truncateLine(lines[i], 140)))
	}
	return out
}

func previewDiff(lines []string, byteLen, maxLines int) []string {
	var files, hunks []string
	var added, removed int
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, strings.TrimPrefix(line, "diff --git "))
		case strings.HasPrefix(line, "@@"):
			if len(hunks) < maxLines {
				hunks = append(hunks, truncateLine(line, 120))
			}
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			added++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			removed++
		}
	}
	out := []string{
		fmt.Sprintf("diff summary: %d files, +%d -%d lines (%d bytes)", len(files), added, removed, byteLen),
	}
	for _, f := range files {
		if len(out) >= maxLines-len(hunks) && len(hunks) > 0 {
			break
		}
		if len(out) >= maxLines {
			break
		}
		out = append(out, "file: "+truncateLine(f, 120))
	}
	for _, h := range hunks {
		if len(out) >= maxLines {
			break
		}
		out = append(out, h)
	}
	return out
}

func previewHTTP(lines []string, byteLen, maxLines int) []string {
	out := []string{fmt.Sprintf("http summary: %d lines, %d bytes", len(lines), byteLen)}
	return append(out, headTailSample(lines, maxLines-1)...)
}

func previewText(lines []string, byteLen, maxLines int) []string {
	out := []string{fmt.Sprintf("text summary: %d lines, %d bytes", len(lines), byteLen)}
	return append(out, headTailSample(lines, maxLines-1)...)
}

func headTailSample(lines []string, budget int) []string {
	if budget <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) <= budget {
		res := make([]string, len(lines))
		for i, l := range lines {
			res[i] = fmt.Sprintf("L%d: %s", i+1, truncateLine(l, 140))
		}
		return res
	}
	head := budget / 2
	tail := budget - head - 1
	if tail < 1 {
		tail = 1
		head = budget - 2
	}
	var out []string
	for i := 0; i < head; i++ {
		out = append(out, fmt.Sprintf("L%d: %s", i+1, truncateLine(lines[i], 140)))
	}
	out = append(out, fmt.Sprintf("... (%d lines omitted) ...", len(lines)-head-tail))
	for i := len(lines) - tail; i < len(lines); i++ {
		out = append(out, fmt.Sprintf("L%d: %s", i+1, truncateLine(lines[i], 140)))
	}
	return out
}

func truncateLine(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	if maxChars <= 3 {
		return s[:maxChars]
	}
	return s[:maxChars-3] + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
