package prune_test

import (
	"strings"
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/prune"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"fs_read_file", []string{"read", "file"}},
		{"readFileContents", []string{"read", "file", "contents"}},
		{"GitHub_createPullRequest", []string{"git", "hub", "create", "pull", "request"}},
		{"the file and the index", []string{"file", "index"}},
		{"a b c", nil},
		{"", nil},
		{"pg://db/table_name", []string{"table", "name"}},
	}
	for _, tc := range cases {
		got := prune.Tokenize(tc.in, nil)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("Tokenize(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTokenizeAppendsToDestination(t *testing.T) {
	dst := []string{"seed"}
	dst = prune.Tokenize("read_file", dst)
	if len(dst) != 3 || dst[0] != "seed" {
		t.Fatalf("got %v", dst)
	}
}

func TestTokenizeBoundsInput(t *testing.T) {
	huge := strings.Repeat("alpha bravo charlie ", 2000)
	got := prune.Tokenize(huge, nil)
	if len(got) > 1200 {
		t.Errorf("tokenizer scanned an unbounded amount of text: %d terms", len(got))
	}
}

func TestTermWindowScoring(t *testing.T) {
	w := prune.NewTermWindow(32, 0.5)
	w.Add([]string{"commit", "diff", "branch"})

	strong := w.Score([]string{"commit", "diff"})
	weak := w.Score([]string{"commit"})
	none := w.Score([]string{"calendar"})

	if !(strong > weak && weak > none) {
		t.Errorf("scores not ordered: strong=%v weak=%v none=%v", strong, weak, none)
	}
	if none != 0 {
		t.Errorf("unrelated terms scored %v, want 0", none)
	}
	if strong >= 1 {
		t.Errorf("score %v should stay below 1", strong)
	}
}

func TestTermWindowDuplicatesDoNotStack(t *testing.T) {
	w := prune.NewTermWindow(32, 0.5)
	w.Add([]string{"commit"})
	once := w.Score([]string{"commit"})
	many := w.Score([]string{"commit", "commit", "commit"})
	if once != many {
		t.Errorf("repeating a term changed the score: %v != %v", once, many)
	}
}

func TestTermWindowDecays(t *testing.T) {
	w := prune.NewTermWindow(32, 0.5)
	w.Add([]string{"old"})
	first := w.Score([]string{"old"})
	for i := 0; i < 3; i++ {
		w.Add([]string{"new"})
	}
	if after := w.Score([]string{"old"}); after >= first {
		t.Errorf("old term did not decay: %v -> %v", first, after)
	}
	if w.Score([]string{"new"}) <= w.Score([]string{"old"}) {
		t.Error("recent terms should outweigh old ones")
	}
}

func TestTermWindowIsBounded(t *testing.T) {
	w := prune.NewTermWindow(16, 0.9)
	for i := 0; i < 500; i++ {
		w.Add([]string{string(rune('a'+i%26)) + "term" + string(rune('a'+i%7))})
	}
	if w.Len() > 16 {
		t.Errorf("window holds %d terms, budget is 16", w.Len())
	}
}

func TestTermWindowEmpty(t *testing.T) {
	w := prune.NewTermWindow(8, 0.5)
	if !w.Empty() {
		t.Error("new window should be empty")
	}
	if got := w.Score([]string{"anything"}); got != 0 {
		t.Errorf("empty window scored %v", got)
	}
	w.Add(nil)
	if !w.Empty() {
		t.Error("adding nothing should not populate the window")
	}
}

func BenchmarkTokenize(b *testing.B) {
	const s = "Make selective edits to a text file using exact string replacement, returning a git-style diff."
	buf := make([]string, 0, 32)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = prune.Tokenize(s, buf[:0])
	}
	_ = buf
}
