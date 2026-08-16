package mcp_test

import (
	"encoding/json"
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/mcp"
)

func TestParseToolsKeepsRawBytes(t *testing.T) {
	raw := json.RawMessage(`[{"name":"a","description":"d","inputSchema":{"type":"object","properties":{"x":{"type":"string"}}},"annotations":{"readOnlyHint":true}}]`)
	tools, err := mcp.ParseTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools", len(tools))
	}
	if tools[0].Name != "a" || tools[0].Description != "d" {
		t.Errorf("metadata not decoded: %+v", tools[0])
	}
	var want, got any
	_ = json.Unmarshal(raw, &want)
	_ = json.Unmarshal(mcp.JoinArray([]json.RawMessage{tools[0].Raw}), &got)
	if string(tools[0].Raw) == "" {
		t.Error("raw bytes were dropped")
	}
}

func TestParseToolsSurvivesMalformedEntries(t *testing.T) {
	raw := json.RawMessage(`[{"name":"ok","inputSchema":{}},"not-an-object",{"name":"also-ok","inputSchema":{}}]`)
	tools, err := mcp.ParseTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3 (malformed entries are kept verbatim)", len(tools))
	}
	if tools[1].Name != "" {
		t.Errorf("malformed entry got a name: %q", tools[1].Name)
	}
	if string(tools[1].Raw) != `"not-an-object"` {
		t.Errorf("malformed entry was rewritten: %s", tools[1].Raw)
	}
}

func TestParseToolsRejectsNonArray(t *testing.T) {
	if _, err := mcp.ParseTools(json.RawMessage(`{"nope":1}`)); err == nil {
		t.Error("expected an error for a non-array payload")
	}
}

func TestJoinArrayRoundTrip(t *testing.T) {
	entries := []json.RawMessage{[]byte(`{"a":1}`), []byte(`{"b":2}`)}
	out := mcp.JoinArray(entries)
	if string(out) != `[{"a":1},{"b":2}]` {
		t.Fatalf("got %s", out)
	}
	if string(mcp.JoinArray(nil)) != `[]` {
		t.Errorf("empty join = %s, want []", mcp.JoinArray(nil))
	}
}

func TestParseToolsCall(t *testing.T) {
	p, ok := mcp.ParseToolsCall(json.RawMessage(`{"name":"fs_read_file","arguments":{"path":"/tmp/x"}}`))
	if !ok || p.Name != "fs_read_file" {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	if _, ok := mcp.ParseToolsCall(nil); ok {
		t.Error("nil params accepted")
	}
	if _, ok := mcp.ParseToolsCall(json.RawMessage(`{"arguments":{}}`)); ok {
		t.Error("params without a name accepted")
	}
}

func TestParseInitializeResult(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"capabilities":{"tools":{"listChanged":true}}}`, true},
		{`{"capabilities":{"tools":{"listChanged":false}}}`, false},
		{`{"capabilities":{"tools":{}}}`, false},
		{`{"capabilities":{}}`, false},
		{`garbage`, false},
	}
	for _, tc := range cases {
		if got := mcp.ParseInitializeResult(json.RawMessage(tc.raw)).ToolsListChanged; got != tc.want {
			t.Errorf("%s -> %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestCollectStringsIsBounded(t *testing.T) {
	raw := json.RawMessage(`{"a":"one","b":["two","three"],"c":{"d":"four"},"e":42,"f":null}`)
	got := mcp.CollectStrings(raw, nil, 100)
	// Keys and values both count.
	want := map[string]bool{"a": true, "one": true, "b": true, "two": true, "three": true, "c": true, "d": true, "four": true, "e": true, "f": true}
	for _, s := range got {
		delete(want, s)
	}
	if len(want) != 0 {
		t.Errorf("missing strings: %v (got %v)", want, got)
	}

	limited := mcp.CollectStrings(raw, nil, 3)
	if len(limited) > 3 {
		t.Errorf("limit ignored: %d strings", len(limited))
	}
}

func TestCollectStringsIsDeterministic(t *testing.T) {
	raw := json.RawMessage(`{"z":"1","a":"2","m":"3","b":{"y":"4","x":"5"}}`)
	first := mcp.CollectStrings(raw, nil, 100)
	for i := 0; i < 50; i++ {
		got := mcp.CollectStrings(raw, nil, 100)
		if len(got) != len(first) {
			t.Fatalf("length changed between runs")
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d differs at %d: %q != %q", i, j, got[j], first[j])
			}
		}
	}
}
