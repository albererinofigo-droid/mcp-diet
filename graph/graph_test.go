package graph_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/graph"
)

func TestTransitionProbability(t *testing.T) {
	g := graph.New(0, 0)
	// read -> edit three times, read -> search once.
	for i := 0; i < 3; i++ {
		g.Record("read")
		g.Record("edit")
	}
	g.Record("read")
	g.Record("search")

	if got := g.TransitionProb("read", "edit"); math.Abs(got-0.75) > 1e-9 {
		t.Errorf("P(edit|read) = %v, want 0.75", got)
	}
	if got := g.TransitionProb("read", "search"); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("P(search|read) = %v, want 0.25", got)
	}
	if got := g.TransitionProb("read", "commit"); got != 0 {
		t.Errorf("P(commit|read) = %v, want 0", got)
	}
	if got := g.TransitionProb("", "edit"); got != 0 {
		t.Errorf("P(edit|<none>) = %v, want 0", got)
	}
}

func TestSelfTransitionIsRecorded(t *testing.T) {
	g := graph.New(0, 0)
	g.Record("list")
	g.Record("list")
	g.Record("list")
	if got := g.TransitionProb("list", "list"); got == 0 {
		t.Error("repeated calls to the same tool should build a self-edge")
	}
}

func TestRecencyAndFrequency(t *testing.T) {
	g := graph.New(0, 0)
	g.Record("a")
	g.Record("b")
	g.Record("c")

	if got := g.StepsSince("c"); got != 0 {
		t.Errorf("StepsSince(c) = %d, want 0", got)
	}
	if got := g.StepsSince("a"); got != 2 {
		t.Errorf("StepsSince(a) = %d, want 2", got)
	}
	if got := g.StepsSince("never"); got != -1 {
		t.Errorf("StepsSince(unknown) = %d, want -1", got)
	}
	if got := g.UseShare("a"); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Errorf("UseShare(a) = %v, want 1/3", got)
	}
	if g.Last() != "c" || g.Prev() != "b" {
		t.Errorf("Last/Prev = %q/%q, want c/b", g.Last(), g.Prev())
	}
}

func TestNodeBudgetIsEnforced(t *testing.T) {
	const max = 32
	g := graph.New(max, 8)
	for i := 0; i < 5000; i++ {
		g.Record(fmt.Sprintf("tool_%d", i))
	}
	if n := g.Nodes(); n > max+2 {
		// +2 leeway: the two most recent nodes are never evicted, because
		// they are what the next prediction is conditioned on.
		t.Errorf("graph holds %d nodes, budget is %d", n, max)
	}
}

func TestEdgeBudgetIsEnforced(t *testing.T) {
	g := graph.New(0, 4)
	for i := 0; i < 200; i++ {
		g.Record("hub")
		g.Record(fmt.Sprintf("spoke_%d", i))
	}
	// Every surviving edge must still carry a valid probability.
	var sum float64
	for i := 0; i < 200; i++ {
		sum += g.TransitionProb("hub", fmt.Sprintf("spoke_%d", i))
	}
	if sum > 1.0000001 {
		t.Errorf("transition probabilities out of hub sum to %v, want <= 1", sum)
	}
}

func TestEvictionIsDeterministic(t *testing.T) {
	build := func() string {
		g := graph.New(16, 4)
		for i := 0; i < 500; i++ {
			g.Record(fmt.Sprintf("t%d", i%64))
		}
		return fmt.Sprintf("%d|%s|%s|%v", g.Nodes(), g.Last(), g.Prev(), g.TransitionProb(g.Prev(), g.Last()))
	}
	want := build()
	for i := 0; i < 20; i++ {
		if got := build(); got != want {
			t.Fatalf("run %d diverged: %q != %q", i, got, want)
		}
	}
}

func TestEmptyNameIsIgnored(t *testing.T) {
	g := graph.New(0, 0)
	g.Record("")
	if g.Seq() != 0 || g.Nodes() != 0 {
		t.Errorf("empty tool name mutated the graph: seq=%d nodes=%d", g.Seq(), g.Nodes())
	}
}

func BenchmarkRecord(b *testing.B) {
	g := graph.New(0, 0)
	names := make([]string, 64)
	for i := range names {
		names[i] = fmt.Sprintf("tool_%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.Record(names[i%len(names)])
	}
}
