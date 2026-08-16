package prune_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/albererinofigo-droid/mcp-diet/graph"
	"github.com/albererinofigo-droid/mcp-diet/mcp"
	"github.com/albererinofigo-droid/mcp-diet/prune"
)

const fixture = "../testdata/tools.json"

func loadFixture(t testing.TB) []mcp.Tool {
	t.Helper()
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var msg struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	tools, err := mcp.ParseTools(msg.Result.Tools)
	if err != nil {
		t.Fatalf("parse tools: %v", err)
	}
	if len(tools) < 20 {
		t.Fatalf("fixture must hold at least 20 tools, got %d", len(tools))
	}
	return tools
}

// warmContext replays a plausible agent workflow so the state engine has
// something to predict from, which is the regime the pruner is designed for.
func warmContext(t testing.TB, cfg prune.Config, calls []string, text string) prune.Context {
	t.Helper()
	ctx := prune.Context{
		Graph:    graph.New(cfg.MaxNodes, cfg.MaxEdgesPerNode),
		Terms:    prune.NewTermWindow(cfg.ContextTerms, cfg.TermDecay),
		Revealed: map[string]uint64{},
	}
	for _, c := range calls {
		ctx.Graph.Record(c)
		ctx.Terms.Add(prune.Tokenize(c, nil))
		ctx.Revealed[c] = ctx.Graph.Seq()
	}
	if text != "" {
		ctx.Terms.Add(prune.Tokenize(text, nil))
	}
	return ctx
}

// skipIfInstrumented drops the wall-clock budget tests when the binary is
// built with -race or coverage instrumentation, both of which add an order of
// magnitude of overhead and make the measurement meaningless.
func skipIfInstrumented(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("latency budgets are not meaningful under the race detector")
	}
	if testing.CoverMode() != "" {
		t.Skip("latency budgets are not meaningful under coverage instrumentation")
	}
}

func TestPruneHalvesTokenCost(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	ctx := warmContext(t, cfg, []string{"fs_read_file", "fs_edit_file", "fs_read_file", "git_diff"}, "")

	res := prune.Prune(tools, ctx, cfg)

	if got := res.Stats.TokenReduction(); got < 0.50 {
		t.Errorf("token reduction = %.1f%%, want >= 50%%", got*100)
	}
	if got := res.Stats.ByteReduction(); got < 0.50 {
		t.Errorf("byte reduction = %.1f%%, want >= 50%%", got*100)
	}
	if res.Stats.Compressed == 0 {
		t.Error("no tool was compressed")
	}
	t.Logf("tools=%d full=%d compressed=%d bytes %d->%d (-%.1f%%) tokens %d->%d (-%.1f%%) in %v",
		res.Stats.Tools, res.Stats.Full, res.Stats.Compressed,
		res.Stats.BytesBefore, res.Stats.BytesAfter, res.Stats.ByteReduction()*100,
		res.Stats.TokensBefore, res.Stats.TokensAfter, res.Stats.TokenReduction()*100,
		res.Stats.Duration)
}

func TestPruneColdStartStillReduces(t *testing.T) {
	// With no session history at all the ranker has no signal; it must still
	// produce a valid, materially smaller list.
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	res := prune.Prune(tools, prune.Context{}, cfg)

	if got := res.Stats.TokenReduction(); got < 0.50 {
		t.Errorf("cold-start token reduction = %.1f%%, want >= 50%%", got*100)
	}
	assertValidToolList(t, tools, res)
}

func TestPrunePreservesEveryToolAndValidJSON(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	ctx := warmContext(t, cfg, []string{"slack_post_message", "slack_list_channels"}, "post a message to the release channel")

	res := prune.Prune(tools, ctx, cfg)
	assertValidToolList(t, tools, res)
}

func assertValidToolList(t *testing.T, in []mcp.Tool, res prune.Result) {
	t.Helper()
	if len(res.Tools) != len(in) {
		t.Fatalf("tool count changed: %d -> %d (default config must never drop a tool)", len(in), len(res.Tools))
	}

	// The whole array must still be valid JSON.
	arr := mcp.JoinArray(res.Tools)
	var probe []map[string]any
	if err := json.Unmarshal(arr, &probe); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	byName := make(map[string]map[string]any, len(probe))
	for i, tool := range probe {
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tool %d lost its name", i)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %q lost a usable inputSchema", name)
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q inputSchema.type = %v, want object", name, schema["type"])
		}
		byName[name] = tool
	}

	tierOf := make(map[string]string, len(res.Decisions))
	for _, d := range res.Decisions {
		tierOf[d.Name] = d.Tier
	}
	for _, orig := range in {
		out, ok := byName[orig.Name]
		if !ok {
			t.Fatalf("tool %q disappeared from the list", orig.Name)
		}
		if tierOf[orig.Name] != "compressed" {
			continue
		}
		if desc, _ := out["description"].(string); desc != "" {
			if n := len([]rune(desc)); n > 100 {
				t.Errorf("compressed tool %q description is %d runes, want <= 100", orig.Name, n)
			}
		}
		if schema, _ := out["inputSchema"].(map[string]any); len(schema) != 1 {
			t.Errorf("compressed tool %q kept a detailed inputSchema: %v", orig.Name, schema)
		}
		if _, has := out["required"]; has {
			t.Errorf("compressed tool %q kept a required array", orig.Name)
		}
	}

	// Tools kept at full fidelity must be byte-identical to the input.
	emitted := make(map[string]json.RawMessage, len(res.Tools))
	for _, raw := range res.Tools {
		var probe struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("emitted tool is not an object: %v", err)
		}
		emitted[probe.Name] = raw
	}
	full := 0
	for _, d := range res.Decisions {
		if d.Tier != "full" {
			continue
		}
		full++
		if string(emitted[d.Name]) != string(findRaw(in, d.Name)) {
			t.Errorf("tool %q is marked full but its bytes changed", d.Name)
		}
	}
	if full == 0 {
		t.Error("expected at least one full-fidelity tool")
	}
}

func findRaw(tools []mcp.Tool, name string) json.RawMessage {
	for _, t := range tools {
		if t.Name == name {
			return t.Raw
		}
	}
	return nil
}

func TestPruneIsDeterministic(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	calls := []string{"github_list_issues", "github_create_issue_comment", "github_list_issues"}
	text := "triage the open issues and reply to the reporter"

	want := string(mcp.JoinArray(prune.Prune(tools, warmContext(t, cfg, calls, text), cfg).Tools))
	for i := 0; i < 50; i++ {
		// A fresh context replaying the same events must land on the same
		// bytes: this is what keeps provider-side prompt caches warm.
		got := string(mcp.JoinArray(prune.Prune(tools, warmContext(t, cfg, calls, text), cfg).Tools))
		if got != want {
			t.Fatalf("iteration %d produced different bytes", i)
		}
	}
}

func TestPrunePreservesUpstreamOrder(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	ctx := warmContext(t, cfg, []string{"calendar_create_event"}, "schedule a meeting")

	res := prune.Prune(tools, ctx, cfg)
	var probe []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(mcp.JoinArray(res.Tools), &probe); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for i := range tools {
		if probe[i].Name != tools[i].Name {
			t.Fatalf("position %d: got %q, want %q — order must be stable for prompt caching",
				i, probe[i].Name, tools[i].Name)
		}
	}
}

func TestPruneKeepsPredictedSuccessorFull(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	cfg.TopN = 3

	// Teach the graph that git_diff is followed by git_commit.
	ctx := warmContext(t, cfg, nil, "")
	for i := 0; i < 5; i++ {
		ctx.Graph.Record("git_diff")
		ctx.Graph.Record("git_commit")
	}
	ctx.Graph.Record("git_diff")

	res := prune.Prune(tools, ctx, cfg)
	tier := map[string]string{}
	for _, d := range res.Decisions {
		tier[d.Name] = d.Tier
	}
	if tier["git_commit"] != "full" {
		t.Errorf("git_commit tier = %q, want full: it is the learned successor of git_diff", tier["git_commit"])
	}
}

func TestPruneHonoursPins(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	cfg.TopN = 1
	cfg.Pinned = []string{"http_request", "browser_*"}
	ctx := warmContext(t, cfg, []string{"fs_read_file"}, "")

	res := prune.Prune(tools, ctx, cfg)
	for _, d := range res.Decisions {
		pinned := d.Name == "http_request" || strings.HasPrefix(d.Name, "browser_")
		if pinned && d.Tier != "full" {
			t.Errorf("pinned tool %q was %s", d.Name, d.Tier)
		}
	}
}

func TestPruneRevealedToolComesBackFull(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	cfg.TopN = 2
	ctx := warmContext(t, cfg, []string{"fs_read_file", "fs_edit_file"}, "")
	// Only vector_search carries explicit intent, so the reveal bonus alone
	// has to lift it into the full-schema budget.
	ctx.Revealed = map[string]uint64{"vector_search": ctx.Graph.Seq()}

	res := prune.Prune(tools, ctx, cfg)
	for _, d := range res.Decisions {
		if d.Name == "vector_search" && d.Tier != "full" {
			t.Errorf("revealed tool vector_search was %s, want full", d.Tier)
		}
	}
}

func TestPruneDisabledIsPassthrough(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	cfg.Enabled = false

	res := prune.Prune(tools, prune.Context{}, cfg)
	if res.Stats.Compressed != 0 || res.Stats.Dropped != 0 {
		t.Fatalf("disabled pruner modified the list: %+v", res.Stats)
	}
	for i, tool := range res.Tools {
		if string(tool) != string(tools[i].Raw) {
			t.Fatalf("tool %d was rewritten while pruning is disabled", i)
		}
	}
}

func TestPruneSmallListUntouched(t *testing.T) {
	tools := loadFixture(t)[:5]
	cfg := prune.DefaultConfig() // MinTools = 6
	res := prune.Prune(tools, prune.Context{}, cfg)
	if res.Stats.Compressed != 0 {
		t.Errorf("small list was pruned: %+v", res.Stats)
	}
}

func TestPruneDropBelowRemovesTools(t *testing.T) {
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	cfg.TopN = 4
	cfg.DropBelow = 0.05
	ctx := warmContext(t, cfg, []string{"fs_read_file"}, "")

	res := prune.Prune(tools, ctx, cfg)
	if res.Stats.Dropped == 0 {
		t.Fatal("dropBelow did not remove any tool")
	}
	if len(res.Tools) != len(tools)-res.Stats.Dropped {
		t.Fatalf("emitted %d tools, expected %d", len(res.Tools), len(tools)-res.Stats.Dropped)
	}
	var probe []map[string]any
	if err := json.Unmarshal(mcp.JoinArray(res.Tools), &probe); err != nil {
		t.Fatalf("invalid JSON after dropping: %v", err)
	}
}

func TestPruneNeverInflates(t *testing.T) {
	// A tool whose compressed form would be larger than the original must be
	// forwarded untouched.
	raw := json.RawMessage(`{"name":"a","inputSchema":{"type":"object"}}`)
	tools := make([]mcp.Tool, 0, 8)
	for i := 0; i < 8; i++ {
		tools = append(tools, mcp.Tool{Raw: raw, Name: "a", SchemaBytes: 19})
	}
	cfg := prune.DefaultConfig()
	cfg.TopN = 0
	cfg.MinTools = 0

	res := prune.Prune(tools, prune.Context{}, cfg)
	if res.Stats.BytesAfter > res.Stats.BytesBefore {
		t.Fatalf("pruning inflated the payload: %d -> %d", res.Stats.BytesBefore, res.Stats.BytesAfter)
	}
}

func TestPruneLatencyUnderBudget(t *testing.T) {
	skipIfInstrumented(t)
	tools := loadFixture(t)
	cfg := prune.DefaultConfig()
	ctx := warmContext(t, cfg, []string{"fs_read_file", "fs_edit_file", "git_diff", "git_commit"}, "review the diff and open a pull request")

	const iterations = 2000
	var worst time.Duration
	var total time.Duration
	for i := 0; i < iterations; i++ {
		res := prune.Prune(tools, ctx, cfg)
		total += res.Stats.Duration
		if res.Stats.Duration > worst {
			worst = res.Stats.Duration
		}
	}
	avg := total / iterations
	t.Logf("prune latency over %d tools: avg %v, worst %v", len(tools), avg, worst)
	if worst > 5*time.Millisecond {
		t.Errorf("worst-case prune took %v, budget is 5ms", worst)
	}
}

func TestTruncationIsRuneSafe(t *testing.T) {
	long := "Esegue una ricerca semantica sull'indice vettoriale — restituisce i passaggi più simili alla query con punteggio, metadati e riferimenti completi."
	tools := []mcp.Tool{{Raw: json.RawMessage(`{"name":"x","description":"` + long + `","inputSchema":{"type":"object","properties":{"a":{"type":"string","description":"padding padding padding padding padding padding"}}}}`), Name: "x", Description: long}}
	cfg := prune.DefaultConfig()
	cfg.MinTools = 0
	cfg.TopN = 0

	res := prune.Prune(tools, prune.Context{}, cfg)
	var out []struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(mcp.JoinArray(res.Tools), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	desc := out[0].Description
	if n := len([]rune(desc)); n > cfg.MaxDescriptionChars {
		t.Errorf("description is %d runes, want <= %d", n, cfg.MaxDescriptionChars)
	}
	if !strings.HasSuffix(desc, "…") {
		t.Errorf("truncated description should end with an ellipsis, got %q", desc)
	}
	if !utf8Valid(desc) {
		t.Error("truncation split a multi-byte rune")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func BenchmarkPrune(b *testing.B) {
	tools := loadFixture(b)
	cfg := prune.DefaultConfig()
	ctx := warmContext(b, cfg, []string{"fs_read_file", "fs_edit_file", "git_diff"}, "open a pull request")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prune.Prune(tools, ctx, cfg)
	}
}
