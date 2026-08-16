package session_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/jsonrpc"
	"github.com/albererinofigo-droid/mcp-diet/prune"
	"github.com/albererinofigo-droid/mcp-diet/session"
)

func toolsListResponse(t testing.TB, id string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../testdata/tools.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	compact, err := json.Marshal(json.RawMessage(fixture.Result.Tools))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}`, id, compact))
}

func frame(t testing.TB, raw string) jsonrpc.Frame {
	t.Helper()
	return jsonrpc.Parse([]byte(raw))
}

func TestSessionPrunesOnlyTrackedToolsListResponses(t *testing.T) {
	s := session.New(prune.DefaultConfig())

	// A response we never saw a request for must not be treated as a list.
	if s.ObserveServer(frame(t, `{"jsonrpc":"2.0","id":99,"result":{"tools":[]}}`)) {
		t.Error("untracked response was claimed as a tools/list")
	}

	s.ObserveClient(frame(t, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`))
	if !s.ObserveServer(frame(t, string(toolsListResponse(t, "4")))) {
		t.Fatal("tracked tools/list response was not recognised")
	}
	// The pending entry is consumed, so a duplicate must not match again.
	if s.ObserveServer(frame(t, string(toolsListResponse(t, "4")))) {
		t.Error("tools/list id was matched twice")
	}
}

func TestSessionPruneToolsResultReducesAndStaysValid(t *testing.T) {
	s := session.New(prune.DefaultConfig())
	raw := toolsListResponse(t, "1")

	out, stats, ok := s.PruneToolsResult(raw)
	if !ok {
		t.Fatal("PruneToolsResult refused a well-formed payload")
	}
	if stats.TokenReduction() < 0.5 {
		t.Errorf("token reduction = %.1f%%, want >= 50%%", stats.TokenReduction()*100)
	}
	var probe struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("pruned frame is not valid JSON: %v", err)
	}
	if probe.JSONRPC != "2.0" || probe.ID != 1 {
		t.Errorf("envelope was mangled: %+v", probe)
	}
	if len(probe.Result.Tools) != 28 {
		t.Errorf("got %d tools, want 28", len(probe.Result.Tools))
	}

	agg := s.Stats()
	if agg.Lists != 1 || agg.TokensBefore == 0 {
		t.Errorf("aggregate not updated: %+v", agg)
	}
}

func TestSessionLeavesUnexpectedShapesAlone(t *testing.T) {
	s := session.New(prune.DefaultConfig())
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"notTools":[]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":"not-an-array"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
		`not json`,
	} {
		out, _, ok := s.PruneToolsResult([]byte(raw))
		if ok {
			t.Errorf("claimed to prune %q", raw)
		}
		if string(out) != raw {
			t.Errorf("rewrote a payload it did not understand:\n got %s\nwant %s", out, raw)
		}
	}
}

func TestSessionDetectsExplicitIntentAfterCompression(t *testing.T) {
	cfg := prune.DefaultConfig()
	cfg.TopN = 2
	s := session.New(cfg)

	if _, _, ok := s.PruneToolsResult(toolsListResponse(t, "1")); !ok {
		t.Fatal("initial prune failed")
	}
	// vector_search cannot be in the top 2 of a cold session.
	ev := s.ObserveClient(frame(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"vector_search","arguments":{"collection":"docs","query":"pricing"}}}`))
	if ev.RevealedTool != "vector_search" {
		t.Fatalf("RevealedTool = %q, want vector_search", ev.RevealedTool)
	}
	if s.Stats().Reveals != 1 {
		t.Errorf("reveals = %d, want 1", s.Stats().Reveals)
	}

	// Calling it again is not a new reveal: it is already at full fidelity.
	ev = s.ObserveClient(frame(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"vector_search","arguments":{}}}`))
	if ev.RevealedTool != "" {
		t.Errorf("second call re-reported a reveal")
	}

	out, _, ok := s.PruneToolsResult(toolsListResponse(t, "4"))
	if !ok {
		t.Fatal("second prune failed")
	}
	if !hasFullSchema(t, out, "vector_search") {
		t.Error("vector_search did not come back at full fidelity after being called")
	}
}

func hasFullSchema(t *testing.T, frameBytes []byte, name string) bool {
	t.Helper()
	var probe struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frameBytes, &probe); err != nil {
		t.Fatalf("invalid frame: %v", err)
	}
	for _, tool := range probe.Result.Tools {
		if tool.Name == name {
			_, ok := tool.InputSchema["properties"]
			return ok
		}
	}
	t.Fatalf("tool %q not found", name)
	return false
}

func TestSessionTracksCapabilities(t *testing.T) {
	s := session.New(prune.DefaultConfig())
	s.ObserveClient(frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	s.ObserveServer(frame(t, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{"listChanged":true}}}}`))
	if !s.Capabilities().ToolsListChanged {
		t.Error("listChanged capability was not recorded")
	}

	s2 := session.New(prune.DefaultConfig())
	s2.ObserveClient(frame(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	s2.ObserveServer(frame(t, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}}}}`))
	if s2.Capabilities().ToolsListChanged {
		t.Error("listChanged was assumed without the server declaring it")
	}
}

func TestSessionLearnsWorkflowAndRanksAccordingly(t *testing.T) {
	cfg := prune.DefaultConfig()
	cfg.TopN = 4
	s := session.New(cfg)

	// Replay a git workflow a few times.
	for i := 0; i < 4; i++ {
		for _, name := range []string{"git_status", "git_diff", "git_commit"} {
			s.ObserveClient(frame(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{"repo":"/srv/app"}}}`, i*10+1, name)))
		}
	}
	out, _, ok := s.PruneToolsResult(toolsListResponse(t, "9"))
	if !ok {
		t.Fatal("prune failed")
	}
	for _, name := range []string{"git_status", "git_diff", "git_commit"} {
		if !hasFullSchema(t, out, name) {
			t.Errorf("%s should be full: it is part of the active workflow", name)
		}
	}
	for _, name := range []string{"calendar_create_event", "browser_screenshot"} {
		if hasFullSchema(t, out, name) {
			t.Errorf("%s should be compressed: it is unrelated to the active workflow", name)
		}
	}
}

func TestSessionIgnoresMalformedToolCalls(t *testing.T) {
	s := session.New(prune.DefaultConfig())
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":"nope"}`,
	} {
		if ev := s.ObserveClient(frame(t, raw)); ev.RevealedTool != "" {
			t.Errorf("malformed call %q produced a reveal", raw)
		}
	}
	if s.Stats().ToolCalls != 0 {
		t.Errorf("malformed calls were counted: %d", s.Stats().ToolCalls)
	}
}

func BenchmarkPruneToolsResult(b *testing.B) {
	s := session.New(prune.DefaultConfig())
	raw := toolsListResponse(b, "1")
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.PruneToolsResult(raw)
	}
}
