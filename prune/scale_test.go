package prune_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/albererinofigo-droid/mcp-diet/mcp"
	"github.com/albererinofigo-droid/mcp-diet/prune"
)

// synthTools builds a large tool list shaped like the fixtures: verbose
// description, five documented properties, a required array.
func synthTools(t testing.TB, n int) []mcp.Tool {
	t.Helper()
	tools := make([]mcp.Tool, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("service%02d_operation_%03d", i%20, i)
		desc := fmt.Sprintf("Perform operation %d against the %s subsystem, validating the request payload, "+
			"resolving credentials from the ambient session and returning a structured result envelope.", i, name)
		raw := fmt.Sprintf(`{"name":%q,"description":%q,"inputSchema":{"type":"object","properties":{`+
			`"identifier":{"type":"string","description":"Primary identifier of the target resource."},`+
			`"payload":{"type":"object","description":"Request payload forwarded verbatim to the upstream API."},`+
			`"timeoutMs":{"type":"integer","minimum":100,"maximum":60000,"default":5000,"description":"Timeout in milliseconds."},`+
			`"dryRun":{"type":"boolean","default":false,"description":"Validate the request without applying it."},`+
			`"tags":{"type":"array","items":{"type":"string"},"description":"Free-form labels attached to the operation."}},`+
			`"required":["identifier","payload"],"additionalProperties":false}}`, name, desc)
		var probe map[string]any
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			t.Fatalf("synthetic tool %d is invalid JSON: %v", i, err)
		}
		tools = append(tools, mcp.Tool{Raw: json.RawMessage(raw), Name: name, Description: desc})
	}
	return tools
}

// TestPruneLatencyAtScale backs the "< 5 ms of added latency" claim for tool
// lists far larger than any single MCP server publishes today.
func TestPruneLatencyAtScale(t *testing.T) {
	skipIfInstrumented(t)
	const toolCount = 500
	tools := synthTools(t, toolCount)
	cfg := prune.DefaultConfig()
	cfg.TopN = 16
	ctx := warmContext(t, cfg, []string{"service01_operation_021", "service02_operation_042"}, "resolve credentials and retry the failed operation")

	const iterations = 200
	var worst time.Duration
	for i := 0; i < iterations; i++ {
		res := prune.Prune(tools, ctx, cfg)
		if res.Stats.Duration > worst {
			worst = res.Stats.Duration
		}
	}
	res := prune.Prune(tools, ctx, cfg)
	t.Logf("%d tools: worst %v, bytes %d->%d (-%.1f%%), tokens %d->%d (-%.1f%%)",
		toolCount, worst,
		res.Stats.BytesBefore, res.Stats.BytesAfter, res.Stats.ByteReduction()*100,
		res.Stats.TokensBefore, res.Stats.TokensAfter, res.Stats.TokenReduction()*100)

	if worst > 5*time.Millisecond {
		t.Errorf("worst-case prune of %d tools took %v, budget is 5ms", toolCount, worst)
	}
	if res.Stats.TokenReduction() < 0.5 {
		t.Errorf("token reduction at scale = %.1f%%, want >= 50%%", res.Stats.TokenReduction()*100)
	}
}

func BenchmarkPrune500(b *testing.B) {
	tools := synthTools(b, 500)
	cfg := prune.DefaultConfig()
	cfg.TopN = 16
	ctx := warmContext(b, cfg, []string{"service01_operation_021"}, "retry the failed operation")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prune.Prune(tools, ctx, cfg)
	}
}
