// Package mcp holds the small subset of the Model Context Protocol the pruner
// has to understand: the shape of a tools/list result, the shape of a
// tools/call request, and the server capabilities that tell us whether we may
// ask a client to refresh its tool list.
package mcp

import (
	"bytes"
	"encoding/json"
)

// Method names used by the proxy.
const (
	MethodInitialize       = "initialize"
	MethodToolsList        = "tools/list"
	MethodToolsCall        = "tools/call"
	MethodPromptsGet       = "prompts/get"
	MethodResourcesRead    = "resources/read"
	MethodCompletionCmpl   = "completion/complete"
	MethodSamplingCreate   = "sampling/createMessage"
	NotifyToolsListChanged = "notifications/tools/list_changed"
)

// Tool is a decoded tools/list entry.
//
// Raw is the exact JSON the upstream server produced. The pruner forwards Raw
// untouched for tools it keeps at full fidelity, which guarantees both perfect
// schema preservation and byte-stable output.
type Tool struct {
	Raw         json.RawMessage
	Name        string
	Title       string
	Description string
	// SchemaBytes is the encoded size of inputSchema, used to rank how much a
	// tool costs to keep.
	SchemaBytes int
}

type toolMeta struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ParseTools decodes the "tools" array of a tools/list result.
func ParseTools(raw json.RawMessage) ([]Tool, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	tools := make([]Tool, 0, len(entries))
	for _, e := range entries {
		var m toolMeta
		if err := json.Unmarshal(e, &m); err != nil {
			// A malformed entry is kept verbatim and never compressed.
			tools = append(tools, Tool{Raw: e})
			continue
		}
		tools = append(tools, Tool{
			Raw:         e,
			Name:        m.Name,
			Title:       m.Title,
			Description: m.Description,
			SchemaBytes: len(m.InputSchema),
		})
	}
	return tools, nil
}

// JoinArray re-assembles a JSON array from raw elements without re-encoding
// them, which keeps every untouched element byte-identical to the original.
func JoinArray(entries []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(e)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// ToolsCallParams is the payload of a tools/call request.
type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ParseToolsCall extracts the tool name and arguments of a tools/call request.
func ParseToolsCall(params json.RawMessage) (ToolsCallParams, bool) {
	if len(params) == 0 {
		return ToolsCallParams{}, false
	}
	var p ToolsCallParams
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return ToolsCallParams{}, false
	}
	return p, true
}

// ServerCapabilities is the fragment of an initialize result the proxy cares
// about.
type ServerCapabilities struct {
	ToolsListChanged bool
}

type initializeResult struct {
	Capabilities struct {
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
}

// ParseInitializeResult reports whether the server can notify clients that its
// tool list changed. Without that capability the proxy must not rely on a
// client re-issuing tools/list, and therefore prunes conservatively.
func ParseInitializeResult(result json.RawMessage) ServerCapabilities {
	var r initializeResult
	if err := json.Unmarshal(result, &r); err != nil {
		return ServerCapabilities{}
	}
	return ServerCapabilities{ToolsListChanged: r.Capabilities.Tools != nil && r.Capabilities.Tools.ListChanged}
}

// CollectStrings walks an arbitrary JSON value and appends every string it
// finds (object keys included) to dst, stopping once dst reaches limit.
//
// This is how the pruner harvests "what is the agent talking about right now"
// from tool arguments and tool results without an LLM call or an embedding
// lookup.
func CollectStrings(raw json.RawMessage, dst []string, limit int) []string {
	if len(raw) == 0 || len(dst) >= limit {
		return dst
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return dst
	}
	return collect(v, dst, limit)
}

func collect(v any, dst []string, limit int) []string {
	if len(dst) >= limit {
		return dst
	}
	switch t := v.(type) {
	case string:
		dst = append(dst, t)
	case []any:
		for _, e := range t {
			dst = collect(e, dst, limit)
			if len(dst) >= limit {
				return dst
			}
		}
	case map[string]any:
		// Sorted iteration is not required for correctness here (the term
		// window is a set), but it keeps behaviour reproducible.
		keys := sortedKeys(t)
		for _, k := range keys {
			dst = append(dst, k)
			if len(dst) >= limit {
				return dst
			}
			dst = collect(t[k], dst, limit)
			if len(dst) >= limit {
				return dst
			}
		}
	}
	return dst
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort: maps here are tiny (tool argument objects)
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
