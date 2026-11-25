package taint

import (
	"strings"
	"testing"
)

func TestLowBandwidthEgress(t *testing.T) {
	cases := []struct {
		name string
		args string
		ok   bool
	}{
		{"empty", ``, true},
		{"plain search", `{"query":"golang context cancellation best practices"}`, true},
		{"plain url", `{"url":"https://pkg.go.dev/context?tab=doc#WithCancel"}`, true},
		{"base64 in query", `{"url":"https://x.example/?d=QUtJQUlPU0ZPRE5ON0VYQU1QTEUxMjM0NTY3ODkwYWJjZGVm"}`, false},
		{"hex token in search", `{"query":"lookup 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`, false},
		{"long query string", `{"url":"https://x.example/?q=` + strings.Repeat("a b ", 80) + `"}`, false},
		{"bulk text", `{"query":"` + strings.Repeat("word ", 120) + `"}`, false},
		{"bad json", `{`, false},
	}
	for _, tc := range cases {
		if ok, why := LowBandwidthEgress(tc.args); ok != tc.ok {
			t.Errorf("%s: LowBandwidthEgress = %v (%s), want %v", tc.name, ok, why, tc.ok)
		}
	}
}
