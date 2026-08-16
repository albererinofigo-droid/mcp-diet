package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/albererinofigo-droid/mcp-diet/graph"
	"github.com/albererinofigo-droid/mcp-diet/mcp"
	"github.com/albererinofigo-droid/mcp-diet/prune"
)

const analyzeUsage = `mcp-diet analyze <file|-> [flags]

Reports how much a tools/list payload shrinks under the current policy,
without running a server. The input may be a full JSON-RPC response, a bare
{"tools": [...]} object, or a plain array of tool objects.

Flags:
  --config <path>     JSON config file
  --top-n <n>         tools that keep their full schema
  --max-desc <n>      max description characters for compressed tools
  --drop-below <f>    drop instead of compress below this score (0 = never)
  --calls <a,b,c>     simulate this sequence of tool executions first
  --context <text>    simulate recent conversation/arguments text
  --json              emit machine-readable JSON instead of a table
`

func runAnalyze(argv []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, analyzeUsage) }

	var (
		configPath = fs.String("config", "", "")
		topN       = fs.Int("top-n", -1, "")
		maxDesc    = fs.Int("max-desc", -1, "")
		dropBelow  = fs.Float64("drop-below", -1, "")
		calls      = fs.String("calls", "", "")
		contextStr = fs.String("context", "", "")
		asJSON     = fs.Bool("json", false, "")
	)
	// Go's flag package stops at the first non-flag argument, so parse in a
	// loop: users write both "analyze tools.json --top-n 4" and
	// "analyze --top-n 4 tools.json", and both should work.
	var positional []string
	for rest := argv; ; {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}

	cfg := prune.DefaultConfig()
	if *configPath != "" {
		loaded, err := prune.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp-diet: %v\n", err)
			return 1
		}
		cfg = loaded
	}
	if *topN >= 0 {
		cfg.TopN = *topN
	}
	if *maxDesc >= 0 {
		cfg.MaxDescriptionChars = *maxDesc
	}
	if *dropBelow >= 0 {
		cfg.DropBelow = *dropBelow
	}
	cfg.Normalize()

	raw, err := readInput(positional[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-diet: %v\n", err)
		return 1
	}
	tools, err := ExtractTools(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-diet: %v\n", err)
		return 1
	}

	ctx := prune.Context{
		Graph:    graph.New(cfg.MaxNodes, cfg.MaxEdgesPerNode),
		Terms:    prune.NewTermWindow(cfg.ContextTerms, cfg.TermDecay),
		Revealed: map[string]uint64{},
	}
	for _, name := range splitList(*calls) {
		ctx.Graph.Record(name)
		ctx.Terms.Add(prune.Tokenize(name, nil))
		ctx.Revealed[name] = ctx.Graph.Seq()
	}
	if *contextStr != "" {
		ctx.Terms.Add(prune.Tokenize(*contextStr, nil))
	}

	res := prune.Prune(tools, ctx, cfg)

	if *asJSON {
		out := struct {
			Stats     prune.Stats      `json:"stats"`
			Decisions []prune.Decision `json:"decisions"`
		}{res.Stats, res.Decisions}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tTIER\tSCORE\tSAVED")
	for _, d := range res.Decisions {
		saved := "-"
		if d.BytesSaved > 0 {
			saved = fmt.Sprintf("%d B", d.BytesSaved)
		}
		fmt.Fprintf(w, "%s\t%s\t%.3f\t%s\n", d.Name, d.Tier, d.Score, saved)
	}
	_ = w.Flush()

	s := res.Stats
	fmt.Printf("\ntools       %d (full %d, compressed %d, dropped %d)\n", s.Tools, s.Full, s.Compressed, s.Dropped)
	fmt.Printf("bytes       %d -> %d  (-%.1f%%)\n", s.BytesBefore, s.BytesAfter, s.ByteReduction()*100)
	fmt.Printf("est tokens  %d -> %d  (-%.1f%%)\n", s.TokensBefore, s.TokensAfter, s.TokenReduction()*100)
	fmt.Printf("prune time  %.3f ms\n", float64(s.Duration.Nanoseconds())/1e6)
	return 0
}

func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// ExtractTools accepts the three shapes a user is likely to have on disk: a
// captured JSON-RPC response, a bare result object, or just the array.
func ExtractTools(raw []byte) ([]mcp.Tool, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		if result, ok := probe["result"]; ok {
			var inner map[string]json.RawMessage
			if err := json.Unmarshal(result, &inner); err == nil {
				if tools, ok := inner["tools"]; ok {
					return mcp.ParseTools(tools)
				}
			}
		}
		if tools, ok := probe["tools"]; ok {
			return mcp.ParseTools(tools)
		}
		return nil, fmt.Errorf("no tools array found in input")
	}
	return mcp.ParseTools(raw)
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
