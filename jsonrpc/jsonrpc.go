// Package jsonrpc implements the minimal slice of JSON-RPC 2.0 that an MCP
// stdio proxy needs: newline-delimited framing, cheap classification of a
// frame (request / response / notification), and lossless pass-through of
// every field the proxy does not understand.
//
// The guiding rule of this package is transparency: a frame is only ever
// re-encoded when the caller actually rewrites it. Anything else is forwarded
// byte-for-byte.
package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Version is the only JSON-RPC version MCP speaks.
const Version = "2.0"

// Kind classifies a decoded frame.
type Kind uint8

const (
	// KindUnknown means the payload is not a JSON object we recognise
	// (malformed JSON, a bare value, ...). Such frames are forwarded as-is.
	KindUnknown Kind = iota
	// KindRequest is a call carrying both "method" and "id".
	KindRequest
	// KindNotification is a call carrying "method" but no "id".
	KindNotification
	// KindResponse carries "id" plus "result" or "error".
	KindResponse
	// KindBatch is a JSON array of frames.
	KindBatch
)

// Frame is a single decoded JSON-RPC message plus the raw bytes it came from.
//
// Raw never includes the trailing newline. ID keeps the wire representation of
// the identifier (JSON numbers and strings are both legal) so it can be used
// as a map key without lossy conversion.
type Frame struct {
	Raw    []byte
	Kind   Kind
	Method string
	ID     string
	HasErr bool
	// Params and Result alias into Raw; they are read-only views used for
	// inspection, never for re-encoding.
	Params json.RawMessage
	Result json.RawMessage
}

type envelope struct {
	Method *string         `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// Parse classifies raw without fully materialising the payload. It never
// fails: anything unrecognised is reported as KindUnknown so the caller can
// forward it untouched.
func Parse(raw []byte) Frame {
	f := Frame{Raw: raw, Kind: KindUnknown}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return f
	}
	if trimmed[0] == '[' {
		f.Kind = KindBatch
		return f
	}
	if trimmed[0] != '{' {
		return f
	}
	var env envelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return f
	}
	if env.ID != nil && !bytes.Equal(env.ID, []byte("null")) {
		f.ID = string(bytes.TrimSpace(env.ID))
	}
	f.Params = env.Params
	f.Result = env.Result
	switch {
	case env.Method != nil && f.ID != "":
		f.Kind = KindRequest
		f.Method = *env.Method
	case env.Method != nil:
		f.Kind = KindNotification
		f.Method = *env.Method
	case env.Result != nil || env.Error != nil:
		f.Kind = KindResponse
		f.HasErr = env.Error != nil
	}
	return f
}

// SplitBatch returns the elements of a JSON array frame. It returns nil when
// raw is not a well-formed array of objects.
func SplitBatch(raw []byte) []json.RawMessage {
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// Object decodes a frame into an ordered-insensitive generic map, keeping
// numbers verbatim (json.Number) so re-encoding cannot change 1e3 into 1000 or
// truncate an int64 through float64.
func Object(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// Encode marshals a generic frame back to a single line.
//
// Go's encoding/json emits map keys in sorted order and never re-indents
// json.RawMessage payloads, so the output is byte-stable for a given input:
// the property MCP clients need in order to keep provider-side prompt caches
// warm.
//
// HTML escaping is disabled on purpose. json.Marshal would rewrite '<', '>'
// and '&' as <, > and & — six bytes where the server sent one.
// That is both a transparency violation (the payload is no longer what the
// server produced) and, for schemas full of "Name <email>" style prose, a
// token cost the pruner exists to remove.
func Encode(m map[string]json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Notification builds a parameterless notification frame.
func Notification(method string) []byte {
	b, _ := json.Marshal(map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"` + Version + `"`),
		"method":  mustQuote(method),
	})
	return b
}

func mustQuote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ErrLineTooLong is returned when a frame exceeds the reader limit.
var ErrLineTooLong = errors.New("jsonrpc: frame exceeds maximum size")

// DefaultMaxFrameBytes bounds a single frame. Real tools/list payloads sit in
// the tens of kilobytes; the limit exists so a wedged peer cannot drive the
// proxy's memory use.
const DefaultMaxFrameBytes = 8 << 20 // 8 MiB

// Reader reads newline-delimited JSON frames.
type Reader struct {
	br  *bufio.Reader
	max int
}

// NewReader wraps r. max <= 0 selects DefaultMaxFrameBytes.
func NewReader(r io.Reader, max int) *Reader {
	if max <= 0 {
		max = DefaultMaxFrameBytes
	}
	// A small starting buffer keeps idle memory low; bufio grows on demand.
	return &Reader{br: bufio.NewReaderSize(r, 8<<10), max: max}
}

// ReadFrame returns the next frame without its trailing newline. Blank lines
// are skipped. It returns io.EOF when the stream ends.
func (r *Reader) ReadFrame() ([]byte, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return line, nil
	}
}

func (r *Reader) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if len(buf)+len(chunk) > r.max {
			return nil, ErrLineTooLong
		}
		if err == bufio.ErrBufferFull {
			buf = append(buf, chunk...)
			continue
		}
		if err != nil {
			if len(buf)+len(chunk) > 0 && err == io.EOF {
				// Final frame without a trailing newline.
				return append(buf, chunk...), nil
			}
			return nil, err
		}
		if buf == nil {
			// Fast path: ReadSlice's buffer is only valid until the next
			// read, so copy before handing it out.
			out := make([]byte, len(chunk))
			copy(out, chunk)
			return out, nil
		}
		return append(buf, chunk...), nil
	}
}

// Writer serialises frames to a stream. It is safe for concurrent use, which
// the proxy relies on: the server->client pump and the proxy's own injected
// notifications share one sink.
type Writer struct {
	w  io.Writer
	mu chan struct{}
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, mu: make(chan struct{}, 1)}
}

// WriteFrame appends a newline and flushes in a single write so frames from
// different goroutines can never interleave.
func (w *Writer) WriteFrame(frame []byte) error {
	w.mu <- struct{}{}
	defer func() { <-w.mu }()
	buf := make([]byte, 0, len(frame)+1)
	buf = append(buf, frame...)
	buf = append(buf, '\n')
	_, err := w.w.Write(buf)
	if err != nil {
		return err
	}
	if f, ok := w.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}
