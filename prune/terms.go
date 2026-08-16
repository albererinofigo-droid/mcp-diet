package prune

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// stopwords are terms that carry no discriminative signal in tool
// descriptions or arguments. Kept small on purpose: over-filtering hurts more
// than a few useless terms.
var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "this": {}, "that": {},
	"from": {}, "into": {}, "your": {}, "you": {}, "are": {}, "was": {},
	"will": {}, "can": {}, "all": {}, "any": {}, "not": {}, "use": {},
	"used": {}, "using": {}, "when": {}, "then": {}, "than": {}, "its": {},
	"has": {}, "have": {}, "must": {}, "should": {}, "may": {}, "one": {},
	"two": {}, "new": {}, "each": {}, "per": {}, "via": {}, "out": {},
	"only": {}, "also": {}, "more": {}, "most": {}, "some": {}, "such": {},
	"true": {}, "false": {}, "null": {}, "string": {}, "number": {},
	"object": {}, "array": {}, "boolean": {}, "type": {}, "properties": {},
	"required": {}, "description": {}, "optional": {},
}

// Tokenize lowercases s, splits it on non-word characters, and also splits
// camelCase and snake_case identifiers — which is exactly what MCP tool names
// look like. Terms shorter than three bytes and stopwords are dropped.
//
// Terms are sliced out of s and only copied when they actually need
// lowercasing, so tokenizing an already-lowercase identifier allocates
// nothing beyond the destination slice.
func Tokenize(s string, dst []string) []string {
	if s == "" {
		return dst
	}
	// Cheap guard against dumping a whole file body into the term window.
	const maxScan = 4096
	if len(s) > maxScan {
		s = s[:maxScan]
	}
	i := 0
	for i < len(s) {
		r, sz := decodeRune(s[i:])
		if !isTermRune(r) {
			i += sz
			continue
		}
		seg := i
		prevUpper := unicode.IsUpper(r)
		i += sz
		for i < len(s) {
			r2, sz2 := decodeRune(s[i:])
			if !isTermRune(r2) {
				break
			}
			if up := unicode.IsUpper(r2); up != prevUpper {
				if up {
					// lower -> upper marks a camelCase boundary
					dst = emitTerm(dst, s[seg:i])
					seg = i
				}
				prevUpper = up
			}
			i += sz2
		}
		dst = emitTerm(dst, s[seg:i])
	}
	return dst
}

// isTermRune deliberately excludes '_': tool names are snake_case, and
// "fs_read_file" only matches conversational context once it is split into
// "read" and "file". EstimateTokens keeps '_' as a word byte, because a BPE
// tokenizer does too — the two predicates answer different questions.
func isTermRune(r rune) bool {
	if r < utf8.RuneSelf {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func emitTerm(dst []string, seg string) []string {
	if len(seg) < 3 {
		return dst
	}
	t := lowerASCII(seg)
	if _, bad := stopwords[t]; bad {
		return dst
	}
	return append(dst, t)
}

// lowerASCII avoids a copy for the common case of an already-lowercase ASCII
// term.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			return strings.ToLower(s)
		}
		if c >= 'A' && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

func decodeRune(s string) (rune, int) {
	if c := s[0]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	return utf8.DecodeRuneInString(s)
}

// TermWindow is a bounded, exponentially decaying bag of terms describing
// what the agent has been doing recently.
//
// It replaces the vector store a semantic router would use: for tool
// selection, exact lexical overlap between the live traffic and a tool's
// name/description is both cheaper and — because tool names are not natural
// language — usually sharper.
type TermWindow struct {
	weights map[string]float64
	total   float64
	max     int
	decay   float64
}

// NewTermWindow returns a window holding at most max terms.
func NewTermWindow(max int, decay float64) *TermWindow {
	if max <= 0 {
		max = 256
	}
	if decay <= 0 || decay >= 1 {
		decay = 0.72
	}
	return &TermWindow{weights: make(map[string]float64, max), max: max, decay: decay}
}

// Len is the number of distinct terms currently held.
func (w *TermWindow) Len() int { return len(w.weights) }

// Empty reports whether the window carries no signal yet.
func (w *TermWindow) Empty() bool { return len(w.weights) == 0 }

// Add decays every existing term and inserts the new ones at weight 1.
// Duplicated terms inside a single Add do not stack, so a document repeating
// one word cannot dominate the window.
func (w *TermWindow) Add(terms []string) {
	if len(terms) == 0 {
		return
	}
	w.total = 0
	for k, v := range w.weights {
		v *= w.decay
		if v < 1e-4 {
			delete(w.weights, k)
			continue
		}
		w.weights[k] = v
		w.total += v
	}
	for _, t := range terms {
		if _, seen := w.weights[t]; seen && w.weights[t] >= 1 {
			continue
		}
		w.total += 1 - w.weights[t]
		w.weights[t] = 1
	}
	w.trim()
}

// trim evicts the weakest terms once the window overflows. Ties break on the
// term itself so the result does not depend on Go's map iteration order.
func (w *TermWindow) trim() {
	if len(w.weights) <= w.max {
		return
	}
	type kv struct {
		k string
		v float64
	}
	all := make([]kv, 0, len(w.weights))
	for k, v := range w.weights {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	w.total = 0
	kept := make(map[string]float64, w.max)
	for _, e := range all[:w.max] {
		kept[e.k] = e.v
		w.total += e.v
	}
	w.weights = kept
}

// saturation controls how fast the lexical score approaches 1. With k = 2,
// one strong term scores 0.33, two score 0.50, five score 0.71 — enough
// spread to rank tools without letting a verbose description win by volume.
const saturation = 2.0

// Score maps the matched term weight into [0,1). Repeated terms in the input
// count once, so a long description cannot inflate its own score.
func (w *TermWindow) Score(terms []string) float64 {
	if w.total == 0 || len(terms) == 0 {
		return 0
	}
	// Deduplication is a linear scan rather than a set: term lists are a few
	// dozen entries at most, and this keeps the hot path allocation-free.
	var hit float64
	for i, t := range terms {
		dup := false
		for j := 0; j < i; j++ {
			if terms[j] == t {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		hit += w.weights[t]
	}
	if hit == 0 {
		return 0
	}
	return hit / (hit + saturation)
}
