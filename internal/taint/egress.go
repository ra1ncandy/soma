package taint

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Egress limits for external reads in a tainted run. A search query or a URL is a covert
// channel: injected instructions can ask the model to append secrets to it. Short,
// human-readable arguments are allowed; long or encoded payloads need operator approval.
const (
	maxEgressStringBytes = 512 // total bytes across all string arguments
	maxURLDataBytes      = 256 // query + fragment bytes of any URL argument
	maxOpaqueTokenRunes  = 40  // longest run of base64/hex-like characters
)

// LowBandwidthEgress reports whether the arguments of an outbound read carry little enough
// free-form data to be safe in a run that has observed untrusted content.
func LowBandwidthEgress(argsJSON string) (bool, string) {
	if strings.TrimSpace(argsJSON) == "" {
		return true, ""
	}
	var v any
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return false, "unparseable arguments"
	}
	var strs []string
	collectStrings(v, &strs)
	total := 0
	for _, s := range strs {
		total += len(s)
		if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
			if n := len(u.RawQuery) + len(u.Fragment); n > maxURLDataBytes {
				return false, fmt.Sprintf("URL carries %d bytes of query/fragment data", n)
			}
			if run := longestOpaqueRun(u.Path + "?" + u.RawQuery + "#" + u.Fragment); run > maxOpaqueTokenRunes {
				return false, fmt.Sprintf("URL contains a %d-character encoded token", run)
			}
			continue
		}
		if run := longestOpaqueRun(s); run > maxOpaqueTokenRunes {
			return false, fmt.Sprintf("argument contains a %d-character encoded token", run)
		}
	}
	if total > maxEgressStringBytes {
		return false, fmt.Sprintf("arguments carry %d bytes", total)
	}
	return true, ""
}

func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			collectStrings(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectStrings(e, out)
		}
	}
}

// longestOpaqueRun returns the longest run of characters that look like encoded data
// (base64, base64url, hex) and contain both letters and digits.
func longestOpaqueRun(s string) int {
	best, cur := 0, 0
	hasDigit, hasAlpha := false, false
	flush := func() {
		if hasDigit && hasAlpha && cur > best {
			best = cur
		}
		cur, hasDigit, hasAlpha = 0, false, false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			cur++
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			cur++
			hasAlpha = true
		case r == '+' || r == '/' || r == '=' || r == '_' || r == '-':
			cur++
		default:
			flush()
		}
	}
	flush()
	return best
}
