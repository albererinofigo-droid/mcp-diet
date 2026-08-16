package prune

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Weights control how the relevance score is composed. All contributions are
// normalised to [0,1] before weighting, so the weights are directly
// comparable.
type Weights struct {
	// Successor: P(next = tool | last executed tool). The strongest signal
	// once a workflow has started.
	Successor float64 `json:"successor"`
	// Successor2: same, one step further back. Catches A -> B -> A loops.
	Successor2 float64 `json:"successor2"`
	// Recency: how recently the tool itself ran.
	Recency float64 `json:"recency"`
	// Frequency: share of all executions in this session.
	Frequency float64 `json:"frequency"`
	// Lexical: overlap between the tool's name/description and the terms
	// seen in recent traffic (arguments, results, prompts).
	Lexical float64 `json:"lexical"`
	// Revealed: the model has shown explicit intent to use this tool.
	Revealed float64 `json:"revealed"`
}

// Config is the full tuning surface of the pruner. Zero values are not
// meaningful; start from DefaultConfig.
type Config struct {
	// Enabled turns pruning off entirely while keeping the proxy transparent.
	Enabled bool `json:"enabled"`
	// TopN is how many tools keep their full schema.
	TopN int `json:"topN"`
	// MaxDescriptionChars caps the description of a compressed tool.
	MaxDescriptionChars int `json:"maxDescriptionChars"`
	// MinTools disables pruning for small tool lists, where the overhead of
	// a reveal round-trip outweighs the savings.
	MinTools int `json:"minTools"`
	// Pinned tools always keep their full schema. Supports a trailing '*'
	// wildcard ("github_*").
	Pinned []string `json:"pinned"`
	// DropBelow removes tools scoring below this threshold instead of
	// compressing them. 0 (the default) never drops a tool: a compressed
	// tool is still callable, a dropped one is not.
	DropBelow float64 `json:"dropBelow"`
	// PreserveOrder keeps the upstream tool ordering. This matters for
	// prompt caching: a stable prefix keeps the provider-side cache warm,
	// while re-ranking on every turn would invalidate it.
	PreserveOrder bool `json:"preserveOrder"`
	// RevealSteps is how many tool executions a revealed tool stays at full
	// fidelity.
	RevealSteps int `json:"revealSteps"`
	// RecencyHalfLife is the number of executions after which the recency
	// contribution halves.
	RecencyHalfLife float64 `json:"recencyHalfLife"`
	// ContextTerms bounds the lexical window.
	ContextTerms int `json:"contextTerms"`
	// TermDecay multiplies existing term weights each time new context
	// arrives (0 < d < 1).
	TermDecay float64 `json:"termDecay"`
	// MaxNodes / MaxEdgesPerNode bound the state engine's memory.
	MaxNodes        int `json:"maxNodes"`
	MaxEdgesPerNode int `json:"maxEdgesPerNode"`

	Weights Weights `json:"weights"`
}

// DefaultConfig returns the recommended starting point: aggressive enough to
// halve a large tool list, conservative enough that no tool ever becomes
// uncallable.
func DefaultConfig() Config {
	return Config{
		Enabled:             true,
		TopN:                8,
		MaxDescriptionChars: 100,
		MinTools:            6,
		DropBelow:           0,
		PreserveOrder:       true,
		RevealSteps:         12,
		RecencyHalfLife:     4,
		ContextTerms:        256,
		TermDecay:           0.72,
		MaxNodes:            512,
		MaxEdgesPerNode:     32,
		Weights: Weights{
			Successor:  1.00,
			Successor2: 0.35,
			Recency:    0.45,
			Frequency:  0.30,
			Lexical:    0.55,
			Revealed:   2.00,
		},
	}
}

// Normalize clamps out-of-range values so a hand-written config file can never
// put the pruner into an unsafe state.
func (c *Config) Normalize() {
	if c.TopN < 0 {
		c.TopN = 0
	}
	if c.MaxDescriptionChars < 0 {
		c.MaxDescriptionChars = 0
	}
	if c.MinTools < 0 {
		c.MinTools = 0
	}
	if c.RevealSteps < 0 {
		c.RevealSteps = 0
	}
	if c.RecencyHalfLife <= 0 {
		c.RecencyHalfLife = 4
	}
	if c.ContextTerms <= 0 {
		c.ContextTerms = 256
	}
	if c.TermDecay <= 0 || c.TermDecay >= 1 {
		c.TermDecay = 0.72
	}
	if c.DropBelow < 0 {
		c.DropBelow = 0
	}
}

// LoadConfig reads a JSON config file on top of DefaultConfig, so a partial
// file only overrides what it mentions.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Normalize()
	return cfg, nil
}

// isPinned reports whether name matches any pin pattern.
func (c *Config) isPinned(name string) bool {
	for _, p := range c.Pinned {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
			continue
		}
		if p == name {
			return true
		}
	}
	return false
}
