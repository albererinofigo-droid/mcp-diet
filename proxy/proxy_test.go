package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/albererinofigo-droid/mcp-diet/jsonrpc"
	"github.com/albererinofigo-droid/mcp-diet/proxy"
	"github.com/albererinofigo-droid/mcp-diet/prune"
	"github.com/albererinofigo-droid/mcp-diet/session"
)

// The proxy end-to-end tests run a real child process. Rather than shipping a
// second binary, the test executable re-executes itself in "mock MCP server"
// mode, which exercises the exact same spawn/pipe path a production run uses.
const mockEnv = "MCP_DIET_TEST_MOCK_SERVER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(mockEnv); mode != "" {
		mockServer(mode)
		return
	}
	os.Exit(m.Run())
}

// mockServer speaks just enough MCP to answer initialize, tools/list and
// tools/call. mode "nolistchanged" drops the listChanged capability.
func mockServer(mode string) {
	toolsRaw, err := os.ReadFile("../testdata/tools.json")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mock server:", err)
		os.Exit(1)
	}
	var fixture struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(toolsRaw, &fixture); err != nil {
		fmt.Fprintln(os.Stderr, "mock server:", err)
		os.Exit(1)
	}
	// The fixture on disk is indented for humans; stdio framing is one JSON
	// value per line, so the mock compacts it exactly like a real server does.
	tools := compactJSON(fixture.Result.Tools)

	listChanged := mode != "nolistchanged"
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for {
		line, err := in.ReadString('\n')
		if len(strings.TrimSpace(line)) > 0 {
			f := jsonrpc.Parse([]byte(strings.TrimRight(line, "\r\n")))
			switch {
			case f.Kind != jsonrpc.KindRequest:
				// notifications need no reply
			case f.Method == "initialize":
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"mock","version":"1.0"},"capabilities":{"tools":{"listChanged":%t}}}}`+"\n", f.ID, listChanged)
			case f.Method == "tools/list":
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}`+"\n", f.ID, tools)
			case f.Method == "tools/call":
				p, _ := mcpCallName(f.Params)
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"executed %s"}],"isError":false}}`+"\n", f.ID, p)
			case f.Method == "custom/passthrough":
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":{"untouched":true,"nested":{"z":1,"a":2}}}`+"\n", f.ID)
			default:
				fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`+"\n", f.ID)
			}
			out.Flush()
		}
		if err != nil {
			return
		}
	}
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		fmt.Fprintln(os.Stderr, "compact:", err)
		os.Exit(1)
	}
	return buf.Bytes()
}

func mcpCallName(params json.RawMessage) (string, bool) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", false
	}
	return p.Name, p.Name != ""
}

// harness drives a proxy over in-memory pipes.
type harness struct {
	t       *testing.T
	in      *io.PipeWriter
	out     *jsonrpc.Reader
	sess    *session.Session
	done    chan struct{}
	code    int
	runErr  error
	nextID  int
	logs    *strings.Builder
	stopCtx context.CancelFunc
}

func newHarness(t *testing.T, cfg prune.Config, mode string) *harness {
	t.Helper()
	clientR, clientW := io.Pipe()
	serverR, serverW := io.Pipe()

	sess := session.New(cfg)
	logs := &strings.Builder{}
	ctx, cancel := context.WithCancel(context.Background())

	h := &harness{
		t:       t,
		in:      clientW,
		out:     jsonrpc.NewReader(serverR, 0),
		sess:    sess,
		done:    make(chan struct{}),
		nextID:  1,
		logs:    logs,
		stopCtx: cancel,
	}

	env := append(os.Environ(), mockEnv+"="+mode)
	go func() {
		defer close(h.done)
		defer serverW.Close()
		h.code, h.runErr = proxy.Run(ctx, proxy.Options{
			Argv:      []string{os.Args[0], "-test.run=TestMain"},
			Env:       env,
			ClientIn:  clientR,
			ClientOut: serverW,
			ServerErr: os.Stderr,
			Session:   sess,
			Log:       proxy.NewLogger(logs, proxy.LevelDebug),
		})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientW.Close()
	})
	return h
}

func (h *harness) send(method string, params string) int {
	h.t.Helper()
	id := h.nextID
	h.nextID++
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q`, id, method)
	if params != "" {
		frame += `,"params":` + params
	}
	frame += "}\n"
	if _, err := io.WriteString(h.in, frame); err != nil {
		h.t.Fatalf("send %s: %v", method, err)
	}
	return id
}

// await reads frames until pred matches or the deadline expires.
func (h *harness) await(pred func(jsonrpc.Frame) bool) jsonrpc.Frame {
	h.t.Helper()
	type result struct {
		f   jsonrpc.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			raw, err := h.out.ReadFrame()
			if err != nil {
				ch <- result{err: err}
				return
			}
			f := jsonrpc.Parse(raw)
			if pred(f) {
				ch <- result{f: f}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			h.t.Fatalf("waiting for frame: %v\nproxy log:\n%s", r.err, h.logs.String())
		}
		return r.f
	case <-time.After(10 * time.Second):
		h.t.Fatalf("timed out waiting for frame\nproxy log:\n%s", h.logs.String())
		return jsonrpc.Frame{}
	}
}

func (h *harness) awaitResponse(id int) jsonrpc.Frame {
	h.t.Helper()
	want := fmt.Sprint(id)
	return h.await(func(f jsonrpc.Frame) bool {
		return f.Kind == jsonrpc.KindResponse && f.ID == want
	})
}

func (h *harness) close() (int, error) {
	h.t.Helper()
	_ = h.in.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Fatal("proxy did not shut down after client EOF")
	}
	return h.code, h.runErr
}

func toolsOf(t *testing.T, f jsonrpc.Frame) []map[string]any {
	t.Helper()
	var res struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		t.Fatalf("tools/list result is not valid JSON: %v", err)
	}
	return res.Tools
}

func classify(tools []map[string]any) (full, compressed []string) {
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		schema, _ := tool["inputSchema"].(map[string]any)
		if _, hasProps := schema["properties"]; hasProps {
			full = append(full, name)
		} else {
			compressed = append(compressed, name)
		}
	}
	return
}

func TestProxyPrunesToolsList(t *testing.T) {
	h := newHarness(t, prune.DefaultConfig(), "1")

	initID := h.send("initialize", `{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"1"}}`)
	h.awaitResponse(initID)

	listID := h.send("tools/list", `{}`)
	tools := toolsOf(t, h.awaitResponse(listID))

	full, compressed := classify(tools)
	if len(tools) != 28 {
		t.Fatalf("got %d tools, want all 28 forwarded", len(tools))
	}
	if len(full) != prune.DefaultConfig().TopN {
		t.Errorf("got %d full tools, want %d", len(full), prune.DefaultConfig().TopN)
	}
	if len(compressed) == 0 {
		t.Fatal("nothing was compressed")
	}

	agg := h.sess.Stats()
	if agg.TokenReduction() < 0.5 {
		t.Errorf("token reduction over the wire = %.1f%%, want >= 50%%", agg.TokenReduction()*100)
	}
	t.Logf("wire savings: tokens %d->%d (-%.1f%%), max prune %.3fms",
		agg.TokensBefore, agg.TokensAfter, agg.TokenReduction()*100, float64(agg.MaxNanos)/1e6)

	if code, err := h.close(); err != nil || code != 0 {
		t.Fatalf("proxy exited with code %d, err %v", code, err)
	}
}

func TestProxyRevealsToolOnExplicitIntent(t *testing.T) {
	cfg := prune.DefaultConfig()
	cfg.TopN = 3
	h := newHarness(t, cfg, "1")

	initID := h.send("initialize", `{"protocolVersion":"2025-06-18"}`)
	h.awaitResponse(initID)

	listID := h.send("tools/list", `{}`)
	_, compressed := classify(toolsOf(t, h.awaitResponse(listID)))
	if len(compressed) == 0 {
		t.Fatal("expected compressed tools")
	}
	target := compressed[len(compressed)-1]

	// The model reaches for a tool whose schema we hid.
	callID := h.send("tools/call", fmt.Sprintf(`{"name":%q,"arguments":{"title":"team sync"}}`, target))

	// The proxy must tell the client the tool list changed...
	h.await(func(f jsonrpc.Frame) bool {
		return f.Kind == jsonrpc.KindNotification && f.Method == "notifications/tools/list_changed"
	})
	h.awaitResponse(callID)

	// ...and the refreshed list must carry the full schema for that tool.
	list2 := h.send("tools/list", `{}`)
	full2, _ := classify(toolsOf(t, h.awaitResponse(list2)))
	found := false
	for _, name := range full2 {
		if name == target {
			found = true
		}
	}
	if !found {
		t.Errorf("tool %q was not restored to full fidelity after being called; full set = %v", target, full2)
	}

	if agg := h.sess.Stats(); agg.Reveals != 1 {
		t.Errorf("reveals = %d, want 1", agg.Reveals)
	}
	if _, err := h.close(); err != nil {
		t.Fatal(err)
	}
}

func TestProxySkipsNotificationWithoutCapability(t *testing.T) {
	cfg := prune.DefaultConfig()
	cfg.TopN = 3
	h := newHarness(t, cfg, "nolistchanged")

	initID := h.send("initialize", `{"protocolVersion":"2025-06-18"}`)
	h.awaitResponse(initID)
	listID := h.send("tools/list", `{}`)
	_, compressed := classify(toolsOf(t, h.awaitResponse(listID)))

	callID := h.send("tools/call", fmt.Sprintf(`{"name":%q,"arguments":{}}`, compressed[0]))
	f := h.await(func(f jsonrpc.Frame) bool { return true })
	if f.Kind == jsonrpc.KindNotification {
		t.Fatalf("proxy sent %s although the server never declared tools.listChanged", f.Method)
	}
	if f.ID != fmt.Sprint(callID) {
		t.Fatalf("unexpected frame %s", f.Raw)
	}
	if _, err := h.close(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyForwardsUnknownMethodsUntouched(t *testing.T) {
	h := newHarness(t, prune.DefaultConfig(), "1")
	id := h.send("custom/passthrough", `{"anything":[1,2,3]}`)
	f := h.awaitResponse(id)
	want := `{"untouched":true,"nested":{"z":1,"a":2}}`
	if string(f.Result) != want {
		t.Errorf("unrelated response was rewritten:\n got %s\nwant %s", f.Result, want)
	}
	if _, err := h.close(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyPassthroughModeIsByteIdentical(t *testing.T) {
	cfg := prune.DefaultConfig()
	cfg.Enabled = false
	h := newHarness(t, cfg, "1")

	initID := h.send("initialize", `{"protocolVersion":"2025-06-18"}`)
	h.awaitResponse(initID)
	listID := h.send("tools/list", `{}`)
	f := h.awaitResponse(listID)

	raw, err := os.ReadFile("../testdata/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(f.Result, &got); err != nil {
		t.Fatal(err)
	}
	if want := string(compactJSON(fixture.Result.Tools)); string(got.Tools) != want {
		t.Error("--no-prune mode altered the tools payload")
	}
	if _, err := h.close(); err != nil {
		t.Fatal(err)
	}
}

func TestProxyReportsMissingServerBinary(t *testing.T) {
	_, err := proxy.Run(context.Background(), proxy.Options{
		Argv:      []string{"definitely-not-a-real-binary-xyz"},
		ClientIn:  strings.NewReader(""),
		ClientOut: io.Discard,
		Session:   session.New(prune.DefaultConfig()),
	})
	if err == nil {
		t.Fatal("expected an error when the server command does not exist")
	}
}
