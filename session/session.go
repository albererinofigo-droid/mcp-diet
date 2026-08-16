// Package session wires the state engine to the live JSON-RPC stream.
//
// A Session watches both directions of an MCP connection and maintains exactly
// the state the pruner needs: which tool ran last, what the traffic has been
// talking about, which tools the model has shown intent to call, and which
// tools are currently advertised in compressed form.
//
// All state is in-memory, bounded, and per-connection: nothing is persisted,
// nothing leaves the process.
package session

import (
	"encoding/json"
	"sync"

	"github.com/albererinofigo-droid/mcp-diet/graph"
	"github.com/albererinofigo-droid/mcp-diet/jsonrpc"
	"github.com/albererinofigo-droid/mcp-diet/mcp"
	"github.com/albererinofigo-droid/mcp-diet/prune"
)

// maxStringsPerFrame bounds how much text a single frame can contribute to the
// lexical window, so a large tool result cannot dominate it or slow the pass.
const maxStringsPerFrame = 64

// Aggregate accumulates savings across every tools/list handled.
type Aggregate struct {
	Lists        int   `json:"lists"`
	ToolCalls    int   `json:"toolCalls"`
	Reveals      int   `json:"reveals"`
	BytesBefore  int64 `json:"bytesBefore"`
	BytesAfter   int64 `json:"bytesAfter"`
	TokensBefore int64 `json:"tokensBefore"`
	TokensAfter  int64 `json:"tokensAfter"`
	MaxNanos     int64 `json:"maxNanos"`
	SumNanos     int64 `json:"sumNanos"`
}

// TokenReduction is the aggregate share of estimated tokens removed.
func (a Aggregate) TokenReduction() float64 {
	if a.TokensBefore == 0 {
		return 0
	}
	return 1 - float64(a.TokensAfter)/float64(a.TokensBefore)
}

// ByteReduction is the aggregate share of payload bytes removed.
func (a Aggregate) ByteReduction() float64 {
	if a.BytesBefore == 0 {
		return 0
	}
	return 1 - float64(a.BytesAfter)/float64(a.BytesBefore)
}

// ClientEvent reports what the proxy should do after a client frame.
type ClientEvent struct {
	// IsToolsList marks a tools/list request whose response must be pruned.
	IsToolsList bool
	// RevealedTool is set when the model called a tool that is currently
	// advertised in compressed form. The proxy uses it to tell the client
	// its tool list changed, so the next tools/list carries the full schema.
	RevealedTool string
}

// Session is safe for concurrent use by the two proxy pumps.
type Session struct {
	mu         sync.Mutex
	cfg        prune.Config
	graph      *graph.Graph
	terms      *prune.TermWindow
	revealed   map[string]uint64
	compressed map[string]struct{}
	pendingLst map[string]struct{}
	pendingCal map[string]string
	pendingIni map[string]struct{}
	caps       mcp.ServerCapabilities
	agg        Aggregate
}

// New builds a session from a config.
func New(cfg prune.Config) *Session {
	cfg.Normalize()
	return &Session{
		cfg:        cfg,
		graph:      graph.New(cfg.MaxNodes, cfg.MaxEdgesPerNode),
		terms:      prune.NewTermWindow(cfg.ContextTerms, cfg.TermDecay),
		revealed:   make(map[string]uint64, 16),
		compressed: make(map[string]struct{}, 32),
		pendingLst: make(map[string]struct{}, 4),
		pendingCal: make(map[string]string, 8),
		pendingIni: make(map[string]struct{}, 1),
	}
}

// Config returns the active configuration.
func (s *Session) Config() prune.Config { return s.cfg }

// Capabilities returns what the upstream server declared at initialize time.
func (s *Session) Capabilities() mcp.ServerCapabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// Stats returns a snapshot of the aggregate savings.
func (s *Session) Stats() Aggregate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agg
}

// ObserveClient inspects a client -> server frame and updates state.
func (s *Session) ObserveClient(f jsonrpc.Frame) ClientEvent {
	var ev ClientEvent
	if f.Kind != jsonrpc.KindRequest && f.Kind != jsonrpc.KindNotification {
		return ev
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	switch f.Method {
	case mcp.MethodInitialize:
		if f.ID != "" {
			s.pendingIni[f.ID] = struct{}{}
		}
	case mcp.MethodToolsList:
		if f.ID != "" {
			s.pendingLst[f.ID] = struct{}{}
			ev.IsToolsList = true
		}
	case mcp.MethodToolsCall:
		p, ok := mcp.ParseToolsCall(f.Params)
		if !ok {
			break
		}
		s.agg.ToolCalls++
		s.graph.Record(p.Name)
		if f.ID != "" {
			s.pendingCal[f.ID] = p.Name
		}
		terms := prune.Tokenize(p.Name, nil)
		for _, str := range mcp.CollectStrings(p.Arguments, nil, maxStringsPerFrame) {
			terms = prune.Tokenize(str, terms)
		}
		s.terms.Add(terms)

		// Explicit intent: the model reached for a tool we had compressed.
		// Mark it so the next tools/list restores the full schema.
		if _, isCompressed := s.compressed[p.Name]; isCompressed {
			delete(s.compressed, p.Name)
			s.revealed[p.Name] = s.graph.Seq()
			s.agg.Reveals++
			ev.RevealedTool = p.Name
		} else {
			s.revealed[p.Name] = s.graph.Seq()
		}
		s.pruneRevealed()
	default:
		// Prompts, resources and completions carry the user's intent too.
		if len(f.Params) > 0 && len(f.Params) < 64<<10 {
			var terms []string
			for _, str := range mcp.CollectStrings(f.Params, nil, maxStringsPerFrame) {
				terms = prune.Tokenize(str, terms)
			}
			if len(terms) > 0 {
				s.terms.Add(terms)
			}
		}
	}
	return ev
}

// ObserveServer inspects a server -> client frame. It reports whether the
// frame is the response to a tools/list request, in which case the proxy hands
// it to PruneToolsResult.
func (s *Session) ObserveServer(f jsonrpc.Frame) (isToolsList bool) {
	if f.Kind != jsonrpc.KindResponse || f.ID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.pendingIni[f.ID]; ok {
		delete(s.pendingIni, f.ID)
		if !f.HasErr {
			s.caps = mcp.ParseInitializeResult(f.Result)
		}
		return false
	}
	if name, ok := s.pendingCal[f.ID]; ok {
		delete(s.pendingCal, f.ID)
		// Tool output feeds the lexical window: what a tool returned is a
		// strong hint about what the agent will reach for next.
		if !f.HasErr && len(f.Result) > 0 {
			terms := prune.Tokenize(name, nil)
			for _, str := range mcp.CollectStrings(f.Result, nil, maxStringsPerFrame) {
				terms = prune.Tokenize(str, terms)
			}
			s.terms.Add(terms)
		}
		return false
	}
	if _, ok := s.pendingLst[f.ID]; ok {
		delete(s.pendingLst, f.ID)
		return true
	}
	return false
}

// PruneToolsResult rewrites a tools/list response frame.
//
// It returns the rewritten frame and the pass statistics. If anything about
// the payload is unexpected the original frame is returned untouched: a proxy
// that cannot understand a message must not rewrite it.
func (s *Session) PruneToolsResult(raw []byte) ([]byte, prune.Stats, bool) {
	obj, err := jsonrpc.Object(raw)
	if err != nil {
		return raw, prune.Stats{}, false
	}
	result, ok := obj["result"]
	if !ok {
		return raw, prune.Stats{}, false
	}
	resObj, err := jsonrpc.Object(result)
	if err != nil {
		return raw, prune.Stats{}, false
	}
	toolsRaw, ok := resObj["tools"]
	if !ok {
		return raw, prune.Stats{}, false
	}
	tools, err := mcp.ParseTools(toolsRaw)
	if err != nil || len(tools) == 0 {
		return raw, prune.Stats{}, false
	}

	s.mu.Lock()
	res := prune.Prune(tools, prune.Context{
		Graph:    s.graph,
		Terms:    s.terms,
		Revealed: s.revealed,
	}, s.cfg)

	s.compressed = make(map[string]struct{}, len(res.Decisions))
	for _, d := range res.Decisions {
		if d.Tier == prune.TierCompressed.String() && d.Name != "" {
			s.compressed[d.Name] = struct{}{}
		}
	}
	s.agg.Lists++
	s.agg.BytesBefore += int64(res.Stats.BytesBefore)
	s.agg.BytesAfter += int64(res.Stats.BytesAfter)
	s.agg.TokensBefore += int64(res.Stats.TokensBefore)
	s.agg.TokensAfter += int64(res.Stats.TokensAfter)
	s.agg.SumNanos += res.Stats.Duration.Nanoseconds()
	if n := res.Stats.Duration.Nanoseconds(); n > s.agg.MaxNanos {
		s.agg.MaxNanos = n
	}
	s.mu.Unlock()

	resObj["tools"] = mcp.JoinArray(res.Tools)
	newResult, err := jsonrpc.Encode(resObj)
	if err != nil {
		return raw, res.Stats, false
	}
	obj["result"] = newResult
	out, err := jsonrpc.Encode(obj)
	if err != nil {
		return raw, res.Stats, false
	}
	return out, res.Stats, true
}

// LastDecisions is exposed for the debug log and the analyze command.
func (s *Session) Snapshot() prune.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	revealed := make(map[string]uint64, len(s.revealed))
	for k, v := range s.revealed {
		revealed[k] = v
	}
	return prune.Context{Graph: s.graph, Terms: s.terms, Revealed: revealed}
}

// pruneRevealed keeps the reveal set bounded; entries older than the reveal
// window carry no weight anyway.
func (s *Session) pruneRevealed() {
	if len(s.revealed) <= s.cfg.MaxNodes {
		return
	}
	cutoff := s.graph.Seq()
	for name, at := range s.revealed {
		if cutoff-at > uint64(s.cfg.RevealSteps) {
			delete(s.revealed, name)
		}
	}
}

// MarshalStats renders the aggregate as JSON for --stats.
func (a Aggregate) MarshalStats() string {
	b, _ := json.Marshal(a)
	return string(b)
}
