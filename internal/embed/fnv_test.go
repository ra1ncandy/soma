package embed

import (
	"hash/fnv"
	"testing"
)

// The chained allocation-free hash must equal hash/fnv over the concatenated feature string, or
// vectors computed by earlier versions (already stored in project.db) would stop matching.
func TestChainedFNVMatchesStdlib(t *testing.T) {
	std := func(s string) uint64 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(s))
		return h.Sum64()
	}
	cases := [][]string{
		{"tok:", "timeout"},
		{"stem:", "confi"},
		{"g3:", "ñañ"},
		{"bi:", "retry", "_", "backoff"},
		{"p4:", ""},
	}
	for _, parts := range cases {
		h := uint64(fnvOffset64)
		joined := ""
		for _, p := range parts {
			h = fnv1a(h, p)
			joined += p
		}
		if h != std(joined) {
			t.Fatalf("fnv1a chain over %q = %x, want %x", parts, h, std(joined))
		}
	}
}

func BenchmarkSimHashEmbed(b *testing.B) {
	e := NewLocalSimHashEmbedder(DefaultMatryoshkaDims)
	text := "the quick brown fox jumps over the lazy dog while the scheduler retries the failed connection with exponential backoff"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = e.embedOne(text)
	}
}
