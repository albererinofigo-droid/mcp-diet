// Package graph implements the pruner's state engine: a bounded, in-memory
// directed weighted graph over tool executions.
//
// Each node is a tool. Each edge A->B counts how often B was executed
// immediately after A in this session. From those counts the pruner derives a
// first-order transition probability P(next = B | last = A), which is enough to
// predict the next few plausible steps of an agent's workflow without a vector
// database, an embedding model, or an extra LLM round-trip.
//
// The graph is deliberately *not* acyclic: real agent workflows loop
// (read -> edit -> read). Cycles are expected and carry signal.
//
// Memory is bounded by MaxNodes and MaxEdgesPerNode; eviction is
// least-recently-used with a name tie-break so that two sessions fed the same
// event stream always end up in the same state.
package graph

import "sort"

// Defaults chosen so that a saturated graph stays far below one megabyte:
// 512 nodes x 32 edges x (name + counter) is on the order of 100 KB.
const (
	DefaultMaxNodes       = 512
	DefaultMaxEdgesPerNod = 32
)

type node struct {
	name     string
	uses     uint32
	lastSeq  uint64
	out      map[string]uint32
	outTotal uint32
}

// Graph is not safe for concurrent use; callers serialise access.
type Graph struct {
	nodes           map[string]*node
	seq             uint64
	totalUses       uint64
	maxNodes        int
	maxEdgesPerNode int
	last            string
	prev            string
}

// New returns an empty graph. Non-positive limits fall back to the defaults.
func New(maxNodes, maxEdgesPerNode int) *Graph {
	if maxNodes <= 0 {
		maxNodes = DefaultMaxNodes
	}
	if maxEdgesPerNode <= 0 {
		maxEdgesPerNode = DefaultMaxEdgesPerNod
	}
	return &Graph{
		nodes:           make(map[string]*node, 32),
		maxNodes:        maxNodes,
		maxEdgesPerNode: maxEdgesPerNode,
	}
}

// Record registers the execution of tool name and links it to the previously
// executed tool.
func (g *Graph) Record(name string) {
	if name == "" {
		return
	}
	g.seq++
	g.totalUses++
	n := g.touch(name)
	n.uses++
	n.lastSeq = g.seq

	if g.last != "" && g.last != name {
		if from, ok := g.nodes[g.last]; ok {
			from.out[name]++
			from.outTotal++
			g.trimEdges(from)
		}
	} else if g.last == name {
		// Self-transition: repeated calls to the same tool are common and
		// predictive (paginated reads, retries).
		if from, ok := g.nodes[g.last]; ok {
			from.out[name]++
			from.outTotal++
			g.trimEdges(from)
		}
	}
	g.prev, g.last = g.last, name
	g.evict()
}

// Last returns the most recently executed tool, or "".
func (g *Graph) Last() string { return g.last }

// Prev returns the tool executed before Last, or "".
func (g *Graph) Prev() string { return g.prev }

// Seq is the number of executions recorded so far.
func (g *Graph) Seq() uint64 { return g.seq }

// Nodes is the current node count; used by tests to check the memory bound.
func (g *Graph) Nodes() int { return len(g.nodes) }

// TransitionProb returns P(next = to | last = from) in [0,1]. Unknown nodes
// score 0.
func (g *Graph) TransitionProb(from, to string) float64 {
	if from == "" || to == "" {
		return 0
	}
	n, ok := g.nodes[from]
	if !ok || n.outTotal == 0 {
		return 0
	}
	c := n.out[to]
	if c == 0 {
		return 0
	}
	return float64(c) / float64(n.outTotal)
}

// UseShare returns the fraction of all recorded executions that hit tool name.
func (g *Graph) UseShare(name string) float64 {
	if g.totalUses == 0 {
		return 0
	}
	n, ok := g.nodes[name]
	if !ok {
		return 0
	}
	return float64(n.uses) / float64(g.totalUses)
}

// StepsSince returns how many executions happened after the last use of name.
// Tools never seen return -1.
func (g *Graph) StepsSince(name string) int64 {
	n, ok := g.nodes[name]
	if !ok || n.lastSeq == 0 {
		return -1
	}
	return int64(g.seq - n.lastSeq)
}

func (g *Graph) touch(name string) *node {
	if n, ok := g.nodes[name]; ok {
		return n
	}
	n := &node{name: name, out: make(map[string]uint32, 4)}
	g.nodes[name] = n
	return n
}

// trimEdges keeps only the strongest edges of a node. Ties break on name so
// eviction is reproducible.
func (g *Graph) trimEdges(n *node) {
	if len(n.out) <= g.maxEdgesPerNode {
		return
	}
	type edge struct {
		to string
		c  uint32
	}
	edges := make([]edge, 0, len(n.out))
	for to, c := range n.out {
		edges = append(edges, edge{to, c})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].c != edges[j].c {
			return edges[i].c > edges[j].c
		}
		return edges[i].to < edges[j].to
	})
	var total uint32
	kept := make(map[string]uint32, g.maxEdgesPerNode)
	for _, e := range edges[:g.maxEdgesPerNode] {
		kept[e.to] = e.c
		total += e.c
	}
	n.out = kept
	n.outTotal = total
}

// evict drops least-recently-used nodes once the node budget is exceeded.
func (g *Graph) evict() {
	if len(g.nodes) <= g.maxNodes {
		return
	}
	type cand struct {
		name string
		seq  uint64
	}
	cands := make([]cand, 0, len(g.nodes))
	for name, n := range g.nodes {
		cands = append(cands, cand{name, n.lastSeq})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq < cands[j].seq
		}
		return cands[i].name < cands[j].name
	})
	drop := len(g.nodes) - g.maxNodes
	for _, c := range cands[:drop] {
		if c.name == g.last || c.name == g.prev {
			continue
		}
		delete(g.nodes, c.name)
		for _, n := range g.nodes {
			if v, ok := n.out[c.name]; ok {
				delete(n.out, c.name)
				n.outTotal -= v
			}
		}
	}
}
