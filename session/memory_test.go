package session_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/prune"
	"github.com/albererinofigo-droid/mcp-diet/session"
)

// TestSessionMemoryFootprint backs the "< 10 MB of overhead" claim: after a
// long, adversarial session the retained state must still be a rounding error.
//
// It measures retained heap, not peak allocation — transient buffers are the
// garbage collector's problem, while the graph, the term window and the reveal
// set are the pruner's.
func TestSessionMemoryFootprint(t *testing.T) {
	const budget = 10 << 20 // 10 MiB

	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)

	s := session.New(prune.DefaultConfig())
	raw := toolsListResponse(t, "1")

	// 20k tool calls across 4k distinct tool names, far beyond the node
	// budget, plus a tools/list every 100 calls.
	for i := 0; i < 20000; i++ {
		name := fmt.Sprintf("tool_%d", i%4000)
		s.ObserveClient(frame(t, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{"path":"/srv/app/pkg_%d/file_%d.go","query":"unique term %d"}}}`,
			i, name, i%97, i%31, i)))
		if i%100 == 0 {
			if _, _, ok := s.PruneToolsResult(raw); !ok {
				t.Fatal("prune failed mid-session")
			}
		}
	}

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("retained heap after 20k calls and 200 tools/list passes: %.2f KiB", float64(retained)/1024)
	if retained > budget {
		t.Errorf("session retains %.2f MiB, budget is 10 MiB", float64(retained)/(1<<20))
	}
}
