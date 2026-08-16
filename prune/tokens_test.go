package prune_test

import (
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/prune"
)

func TestEstimateTokensIsMonotonic(t *testing.T) {
	small := prune.EstimateTokens([]byte(`{"name":"a"}`))
	large := prune.EstimateTokens([]byte(`{"name":"a","description":"a much longer description with many words"}`))
	if small >= large {
		t.Errorf("larger payload estimated at %d tokens vs %d for the smaller one", large, small)
	}
	if prune.EstimateTokens(nil) != 0 {
		t.Error("empty payload should cost 0 tokens")
	}
}

// TestEstimateTokensTracksKnownCounts checks the heuristic against counts
// produced by cl100k_base for these exact strings. The estimator only has to
// stay in the right ballpark: it is a reduction metric, not a billing meter.
func TestEstimateTokensTracksKnownCounts(t *testing.T) {
	cases := []struct {
		in    string
		exact int
	}{
		{`{"name":"fs_read_file"}`, 9},
		{`{"type":"object","properties":{"path":{"type":"string"}}}`, 17},
		{"Read the complete contents of a file from the file system.", 12},
	}
	for _, tc := range cases {
		got := prune.EstimateTokens([]byte(tc.in))
		ratio := float64(got) / float64(tc.exact)
		if ratio < 0.6 || ratio > 1.8 {
			t.Errorf("EstimateTokens(%q) = %d, reference %d (ratio %.2f is outside 0.6-1.8)", tc.in, got, tc.exact, ratio)
		}
	}
}

func TestEstimateTokensHandlesUnicode(t *testing.T) {
	if got := prune.EstimateTokens([]byte("città più caffè — naïve")); got == 0 {
		t.Error("unicode text estimated at 0 tokens")
	}
	// Invalid UTF-8 must not panic or loop forever.
	if got := prune.EstimateTokens([]byte{0xff, 0xfe, 'a', 'b', 'c'}); got == 0 {
		t.Error("invalid UTF-8 estimated at 0 tokens")
	}
}

func BenchmarkEstimateTokens(b *testing.B) {
	tools := loadFixture(b)
	payload := tools[0].Raw
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prune.EstimateTokens(payload)
	}
}
