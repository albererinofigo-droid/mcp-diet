// Package prune contains the schema-aware pruning algorithm: it ranks the
// tools a server advertises against the live state of the session and rewrites
// the tools/list payload so that only the tools plausibly needed next carry
// their full JSON Schema.
//
// Two invariants hold for every output this package produces:
//
//  1. Callability. By default no tool is ever removed. A secondary tool keeps
//     its name and a truncated description, so the model can still name it;
//     the proxy then reveals the full schema on demand. Tool removal is opt-in
//     via Config.DropBelow.
//
//  2. Determinism. Identical (tools, context, config) triples produce
//     byte-identical output. Rankings use stable sorts with explicit
//     tie-breaks, compressed objects are built from sorted keys, and kept
//     tools are forwarded as the exact bytes the upstream server sent. This is
//     what makes the result safe for Anthropic and OpenAI prompt caching.
package prune

import (
	"encoding/json"
	"math"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/albererinofigo-droid/mcp-diet/graph"
	"github.com/albererinofigo-droid/mcp-diet/mcp"
)

// Tier is the fidelity level assigned to a tool.
type Tier uint8

const (
	// TierFull forwards the upstream schema byte-for-byte.
	TierFull Tier = iota
	// TierCompressed keeps name + truncated description and replaces the
	// input schema with the permissive {"type":"object"}.
	TierCompressed
	// TierDropped removes the tool from the list (opt-in).
	TierDropped
)

// String renders a tier for logs.
func (t Tier) String() string {
	switch t {
	case TierFull:
		return "full"
	case TierCompressed:
		return "compressed"
	case TierDropped:
		return "dropped"
	}
	return "?"
}

// Context is the read-only view of session state the ranker consumes.
type Context struct {
	// Graph is the tool-transition state engine. May be nil.
	Graph *graph.Graph
	// Terms is the lexical window over recent traffic. May be nil.
	Terms *TermWindow
	// Revealed maps a tool name to the graph sequence at which the model
	// showed explicit intent to call it.
	Revealed map[string]uint64
}

// Decision records why a tool ended up in its tier.
type Decision struct {
	Name       string  `json:"name"`
	Score      float64 `json:"score"`
	Tier       string  `json:"tier"`
	Successor  float64 `json:"successor"`
	Recency    float64 `json:"recency"`
	Frequency  float64 `json:"frequency"`
	Lexical    float64 `json:"lexical"`
	Pinned     bool    `json:"pinned"`
	BytesSaved int     `json:"bytesSaved"`
}

// Stats summarises one pruning pass.
type Stats struct {
	Tools        int           `json:"tools"`
	Full         int           `json:"full"`
	Compressed   int           `json:"compressed"`
	Dropped      int           `json:"dropped"`
	BytesBefore  int           `json:"bytesBefore"`
	BytesAfter   int           `json:"bytesAfter"`
	TokensBefore int           `json:"tokensBefore"`
	TokensAfter  int           `json:"tokensAfter"`
	Duration     time.Duration `json:"durationNs"`
}

// ByteReduction is the fraction of payload bytes removed, in [0,1].
func (s Stats) ByteReduction() float64 {
	if s.BytesBefore == 0 {
		return 0
	}
	return 1 - float64(s.BytesAfter)/float64(s.BytesBefore)
}

// TokenReduction is the fraction of estimated tokens removed, in [0,1].
func (s Stats) TokenReduction() float64 {
	if s.TokensBefore == 0 {
		return 0
	}
	return 1 - float64(s.TokensAfter)/float64(s.TokensBefore)
}

// Result is the rewritten tool list plus the reasoning behind it.
type Result struct {
	Tools     []json.RawMessage
	Decisions []Decision
	Stats     Stats
}

// Prune ranks and rewrites tools. The input slice is never mutated.
func Prune(tools []mcp.Tool, ctx Context, cfg Config) Result {
	start := time.Now()
	res := Result{
		Tools:     make([]json.RawMessage, 0, len(tools)),
		Decisions: make([]Decision, 0, len(tools)),
	}
	res.Stats.Tools = len(tools)
	for _, t := range tools {
		res.Stats.BytesBefore += len(t.Raw)
		res.Stats.TokensBefore += EstimateTokens(t.Raw)
	}

	// Small lists and disabled pruning are pure pass-through.
	if !cfg.Enabled || len(tools) <= cfg.MinTools {
		for _, t := range tools {
			res.Tools = append(res.Tools, t.Raw)
			res.Decisions = append(res.Decisions, Decision{Name: t.Name, Tier: TierFull.String()})
		}
		res.Stats.Full = len(tools)
		res.Stats.BytesAfter = res.Stats.BytesBefore
		res.Stats.TokensAfter = res.Stats.TokensBefore
		res.Stats.Duration = time.Since(start)
		return res
	}

	decisions := make([]Decision, len(tools))
	pinned := make([]bool, len(tools))
	termBuf := make([]string, 0, 48)
	for i, t := range tools {
		termBuf = termBuf[:0]
		termBuf = Tokenize(t.Name, termBuf)
		termBuf = Tokenize(t.Description, termBuf)
		termBuf = Tokenize(t.Title, termBuf)
		decisions[i] = score(t, termBuf, ctx, cfg)
		pinned[i] = cfg.isPinned(t.Name)
		decisions[i].Pinned = pinned[i]
	}

	// Rank by score, breaking ties on the original position so the order is
	// reproducible regardless of map iteration or input churn.
	order := make([]int, len(tools))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if pinned[ia] != pinned[ib] {
			return pinned[ia]
		}
		if decisions[ia].Score != decisions[ib].Score {
			return decisions[ia].Score > decisions[ib].Score
		}
		return ia < ib
	})

	tiers := make([]Tier, len(tools))
	budget := cfg.TopN
	for rank, idx := range order {
		switch {
		case pinned[idx]:
			tiers[idx] = TierFull
		case rank < budget:
			tiers[idx] = TierFull
		case cfg.DropBelow > 0 && decisions[idx].Score < cfg.DropBelow:
			tiers[idx] = TierDropped
		default:
			tiers[idx] = TierCompressed
		}
	}

	emit := order
	if cfg.PreserveOrder {
		emit = make([]int, len(tools))
		for i := range emit {
			emit[i] = i
		}
	}

	for _, idx := range emit {
		t := tools[idx]
		switch tiers[idx] {
		case TierDropped:
			res.Stats.Dropped++
			decisions[idx].Tier = TierDropped.String()
			decisions[idx].BytesSaved = len(t.Raw)
		case TierCompressed:
			small, ok := compress(t, cfg)
			if !ok || len(small) >= len(t.Raw) {
				// Compression that does not pay for itself is not applied;
				// the tool keeps its full schema.
				tiers[idx] = TierFull
				res.Stats.Full++
				res.Tools = append(res.Tools, t.Raw)
				res.Stats.BytesAfter += len(t.Raw)
				res.Stats.TokensAfter += EstimateTokens(t.Raw)
				decisions[idx].Tier = TierFull.String()
				break
			}
			res.Stats.Compressed++
			res.Tools = append(res.Tools, small)
			res.Stats.BytesAfter += len(small)
			res.Stats.TokensAfter += EstimateTokens(small)
			decisions[idx].Tier = TierCompressed.String()
			decisions[idx].BytesSaved = len(t.Raw) - len(small)
		default:
			res.Stats.Full++
			res.Tools = append(res.Tools, t.Raw)
			res.Stats.BytesAfter += len(t.Raw)
			res.Stats.TokensAfter += EstimateTokens(t.Raw)
			decisions[idx].Tier = TierFull.String()
		}
	}

	for _, idx := range emit {
		res.Decisions = append(res.Decisions, decisions[idx])
	}
	res.Stats.Duration = time.Since(start)
	return res
}

// score computes the relevance of one tool. Every component is normalised to
// [0,1] before weighting.
func score(t mcp.Tool, terms []string, ctx Context, cfg Config) Decision {
	d := Decision{Name: t.Name}
	if t.Name == "" {
		return d
	}

	if g := ctx.Graph; g != nil {
		s1 := g.TransitionProb(g.Last(), t.Name)
		s2 := g.TransitionProb(g.Prev(), t.Name)
		d.Successor = s1
		d.Frequency = g.UseShare(t.Name)
		if steps := g.StepsSince(t.Name); steps >= 0 {
			d.Recency = math.Exp2(-float64(steps) / cfg.RecencyHalfLife)
		}
		d.Score += cfg.Weights.Successor*s1 + cfg.Weights.Successor2*s2
		d.Score += cfg.Weights.Recency * d.Recency
		d.Score += cfg.Weights.Frequency * d.Frequency
	}

	if ctx.Terms != nil {
		d.Lexical = ctx.Terms.Score(terms)
		d.Score += cfg.Weights.Lexical * d.Lexical
	}

	if ctx.Revealed != nil && ctx.Graph != nil {
		if at, ok := ctx.Revealed[t.Name]; ok {
			age := int64(ctx.Graph.Seq()) - int64(at)
			if age <= int64(cfg.RevealSteps) {
				d.Score += cfg.Weights.Revealed
			}
		}
	} else if ctx.Revealed != nil {
		if _, ok := ctx.Revealed[t.Name]; ok {
			d.Score += cfg.Weights.Revealed
		}
	}
	return d
}

// compress builds the reduced representation of a secondary tool.
//
// The output is a valid MCP tool object: name is preserved exactly, the
// description is truncated on a word boundary, and inputSchema becomes the
// permissive {"type":"object"} rather than being deleted — an absent
// inputSchema is invalid per the MCP schema, and an empty object schema keeps
// every client's validator happy while costing 19 bytes.
// Keys are emitted in sorted order, matching what encoding/json would produce
// for a map, so the bytes are identical no matter how the object is built.
func compress(t mcp.Tool, cfg Config) (json.RawMessage, bool) {
	if t.Name == "" {
		return nil, false
	}
	desc := t.Description
	if desc == "" {
		desc = t.Title
	}
	if cfg.MaxDescriptionChars <= 0 {
		desc = ""
	} else {
		desc = truncate(desc, cfg.MaxDescriptionChars)
	}

	buf := make([]byte, 0, 48+len(desc)+len(t.Name))
	buf = append(buf, '{')
	if desc != "" {
		buf = append(buf, `"description":`...)
		buf = appendJSONString(buf, desc)
		buf = append(buf, ',')
	}
	buf = append(buf, `"inputSchema":{"type":"object"},"name":`...)
	buf = appendJSONString(buf, t.Name)
	buf = append(buf, '}')
	return buf, true
}

// truncate cuts s to at most max runes, preferring the last word boundary in
// the final third of the budget, and marks the cut with a single ellipsis
// rune.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	var (
		count     int
		cut       = -1
		lastSpace = -1
		lower     = max * 2 / 3
	)
	for i, r := range s {
		if count == max-1 {
			cut = i
		}
		if count >= max {
			end := cut
			if lastSpace > 0 {
				end = lastSpace
			}
			if end <= 0 {
				end = i
			}
			return s[:end] + "…"
		}
		if r == ' ' && count > lower {
			lastSpace = i
		}
		count++
	}
	return s
}

const hexDigits = "0123456789abcdef"

// appendJSONString writes a JSON string literal. It escapes exactly what RFC
// 8259 requires plus the C0 range, and replaces invalid UTF-8 with U+FFFD so
// the output is always valid JSON even if the upstream server is not.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				if c < 0x20 {
					dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
				} else {
					dst = append(dst, c)
				}
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, "�"...)
			i++
			continue
		}
		dst = append(dst, s[i:i+size]...)
		i += size
	}
	return append(dst, '"')
}
