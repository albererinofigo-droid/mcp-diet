package jsonrpc_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/albererinofigo-droid/mcp-diet/jsonrpc"
)

func TestParseClassification(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		kind   jsonrpc.Kind
		method string
		id     string
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, jsonrpc.KindRequest, "tools/list", "1"},
		{"string id", `{"jsonrpc":"2.0","id":"abc","method":"tools/call"}`, jsonrpc.KindRequest, "tools/call", `"abc"`},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, jsonrpc.KindNotification, "notifications/initialized", ""},
		{"response", `{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`, jsonrpc.KindResponse, "", "7"},
		{"error response", `{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"nope"}}`, jsonrpc.KindResponse, "", "7"},
		{"null id notification", `{"jsonrpc":"2.0","id":null,"method":"x"}`, jsonrpc.KindNotification, "x", ""},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"a"}]`, jsonrpc.KindBatch, "", ""},
		{"garbage", `not json at all`, jsonrpc.KindUnknown, "", ""},
		{"truncated", `{"jsonrpc":"2.0","id":1,`, jsonrpc.KindUnknown, "", ""},
		{"bare value", `42`, jsonrpc.KindUnknown, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := jsonrpc.Parse([]byte(tc.raw))
			if f.Kind != tc.kind {
				t.Errorf("kind = %v, want %v", f.Kind, tc.kind)
			}
			if f.Method != tc.method {
				t.Errorf("method = %q, want %q", f.Method, tc.method)
			}
			if f.ID != tc.id {
				t.Errorf("id = %q, want %q", f.ID, tc.id)
			}
		})
	}
}

func TestParseFlagsErrorResponses(t *testing.T) {
	f := jsonrpc.Parse([]byte(`{"jsonrpc":"2.0","id":7,"error":{"code":-1,"message":"x"}}`))
	if !f.HasErr {
		t.Error("error response was not flagged")
	}
}

func TestReaderFrames(t *testing.T) {
	input := "{\"a\":1}\n\n  \n{\"b\":2}\n{\"c\":3}"
	r := jsonrpc.NewReader(strings.NewReader(input), 0)
	var got []string
	for {
		frame, err := r.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		got = append(got, string(frame))
	}
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if len(got) != len(want) {
		t.Fatalf("got %d frames %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReaderHandlesFramesLargerThanTheBuffer(t *testing.T) {
	big := `{"payload":"` + strings.Repeat("x", 200000) + `"}`
	r := jsonrpc.NewReader(strings.NewReader(big+"\n"), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != big {
		t.Fatalf("large frame was corrupted (%d bytes read, %d expected)", len(frame), len(big))
	}
}

func TestReaderEnforcesMaxFrame(t *testing.T) {
	r := jsonrpc.NewReader(strings.NewReader(strings.Repeat("x", 5000)+"\n"), 1024)
	if _, err := r.ReadFrame(); err != jsonrpc.ErrLineTooLong {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestReaderKeepsFramesIndependent(t *testing.T) {
	// ReadSlice reuses its internal buffer; frames must be copied out.
	r := jsonrpc.NewReader(strings.NewReader("{\"a\":1}\n{\"b\":2}\n"), 0)
	first, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if string(first) != `{"a":1}` {
		t.Fatalf("first frame was clobbered by the second read: %q", first)
	}
}

func TestWriterIsFrameAtomic(t *testing.T) {
	var buf syncBuffer
	w := jsonrpc.NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]int{"n": i})
			if err := w.WriteFrame(payload); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 32 {
		t.Fatalf("got %d lines, want 32", len(lines))
	}
	for _, line := range lines {
		var probe map[string]int
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("interleaved write produced invalid JSON: %q", line)
		}
	}
}

func TestObjectPreservesNumbers(t *testing.T) {
	raw := []byte(`{"big":12345678901234567890,"exp":1e3,"nested":{"a":[1,2,3]}}`)
	obj, err := jsonrpc.Object(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsonrpc.Encode(obj)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"12345678901234567890", "1e3"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("re-encoding lost %s: %s", want, out)
		}
	}
}

func TestEncodeIsStable(t *testing.T) {
	raw := []byte(`{"z":1,"a":2,"m":{"q":3,"b":4}}`)
	var first string
	for i := 0; i < 100; i++ {
		obj, err := jsonrpc.Object(raw)
		if err != nil {
			t.Fatal(err)
		}
		out, err := jsonrpc.Encode(obj)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(out)
			continue
		}
		if string(out) != first {
			t.Fatalf("encoding is not stable: %q != %q", out, first)
		}
	}
}

func TestEncodeDoesNotHTMLEscape(t *testing.T) {
	// json.Marshal would rewrite "<email>" as a \\u003c escape: six bytes
	// where the server sent one, and a payload the client never asked for.
	raw := []byte(`{"description":"Author in the form Name <email> & co"}`)
	obj, err := jsonrpc.Object(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsonrpc.Encode(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("<email> & co")) {
		t.Fatalf("payload was HTML-escaped: %s", out)
	}
	for _, escaped := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if bytes.Contains(out, []byte(escaped)) {
			t.Fatalf("payload contains %s: %s", escaped, out)
		}
	}
	if bytes.HasSuffix(out, []byte("\n")) {
		t.Error("encoded frame must not carry a trailing newline")
	}
}

func TestNotification(t *testing.T) {
	n := jsonrpc.Notification("notifications/tools/list_changed")
	f := jsonrpc.Parse(n)
	if f.Kind != jsonrpc.KindNotification || f.Method != "notifications/tools/list_changed" {
		t.Fatalf("bad notification frame: %s", n)
	}
}

func TestSplitBatch(t *testing.T) {
	items := jsonrpc.SplitBatch([]byte(`[{"id":1},{"id":2}]`))
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if jsonrpc.SplitBatch([]byte(`{"id":1}`)) != nil {
		t.Error("SplitBatch accepted a non-array")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
