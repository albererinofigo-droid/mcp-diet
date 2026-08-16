// Package proxy runs mcp-diet as a transparent stdio middleware between an
// MCP client and an MCP server.
//
// The proxy spawns the real server as a child process and pumps
// newline-delimited JSON-RPC frames in both directions. Only two kinds of
// frame are ever rewritten:
//
//   - a tools/list response, which is pruned;
//   - an injected notifications/tools/list_changed, emitted when the model
//     reaches for a tool whose schema is currently compressed.
//
// Everything else — initialize, resources, prompts, logging, progress,
// unknown extensions, malformed lines — is forwarded byte-for-byte.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/albererinofigo-droid/mcp-diet/jsonrpc"
	"github.com/albererinofigo-droid/mcp-diet/mcp"
	"github.com/albererinofigo-droid/mcp-diet/session"
)

// Options configures a proxy run.
type Options struct {
	// Argv is the upstream MCP server command. Argv[0] is the executable.
	Argv []string
	// Dir optionally sets the child's working directory.
	Dir string
	// Env optionally replaces the child's environment.
	Env []string
	// ClientIn / ClientOut are the transport towards the MCP client. On a
	// real run these are os.Stdin and os.Stdout.
	ClientIn  io.Reader
	ClientOut io.Writer
	// ServerErr receives the child's stderr; defaults to os.Stderr.
	ServerErr io.Writer
	// Session holds the pruning state.
	Session *session.Session
	// Log receives diagnostics (never the data channel).
	Log *Logger
	// MaxFrameBytes bounds a single JSON-RPC frame.
	MaxFrameBytes int
}

// Proxy is a single client<->server bridge.
type Proxy struct {
	opts Options
	// notifyPending is true once a list_changed notification has been sent
	// and not yet consumed by a fresh tools/list. It collapses bursts of
	// reveals into a single client refresh.
	notifyPending atomic.Bool
}

// Run starts the upstream server and pumps frames until either side closes.
// It returns the child's exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	if len(opts.Argv) == 0 {
		return 1, errors.New("proxy: empty server command")
	}
	if opts.Session == nil {
		return 1, errors.New("proxy: nil session")
	}
	if opts.ClientIn == nil {
		opts.ClientIn = os.Stdin
	}
	if opts.ClientOut == nil {
		opts.ClientOut = os.Stdout
	}
	if opts.ServerErr == nil {
		opts.ServerErr = os.Stderr
	}
	if opts.Log == nil {
		opts.Log = NewLogger(io.Discard, LevelOff)
	}

	p := &Proxy{opts: opts}

	cmd := exec.Command(opts.Argv[0], opts.Argv[1:]...)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	cmd.Stderr = opts.ServerErr

	serverIn, err := cmd.StdinPipe()
	if err != nil {
		return 1, fmt.Errorf("proxy: server stdin: %w", err)
	}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		return 1, fmt.Errorf("proxy: server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("proxy: start %q: %w", opts.Argv[0], err)
	}
	opts.Log.Infof("proxying %v (pid %d)", opts.Argv, cmd.Process.Pid)

	// Killing the child on context cancellation keeps Ctrl-C from leaving
	// orphaned server processes behind.
	stopWatch := make(chan struct{})
	var watchOnce sync.Once
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-stopWatch:
		}
	}()
	defer watchOnce.Do(func() { close(stopWatch) })

	toClient := jsonrpc.NewWriter(opts.ClientOut)
	toServer := jsonrpc.NewWriter(serverIn)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.pumpClient(opts.ClientIn, toServer, toClient)
		// EOF from the client means "session over": closing the child's
		// stdin is how MCP servers are asked to shut down cleanly.
		_ = serverIn.Close()
	}()

	p.pumpServer(serverOut, toClient)

	waitErr := cmd.Wait()
	watchOnce.Do(func() { close(stopWatch) })

	code := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
	} else if waitErr != nil {
		return 1, fmt.Errorf("proxy: wait: %w", waitErr)
	}

	agg := opts.Session.Stats()
	if agg.Lists > 0 {
		opts.Log.Infof("summary: lists=%d calls=%d reveals=%d tokens %d->%d (-%.1f%%) bytes %d->%d (-%.1f%%) max_prune=%.3fms",
			agg.Lists, agg.ToolCalls, agg.Reveals,
			agg.TokensBefore, agg.TokensAfter, agg.TokenReduction()*100,
			agg.BytesBefore, agg.BytesAfter, agg.ByteReduction()*100,
			float64(agg.MaxNanos)/1e6)
	}
	return code, nil
}

// pumpClient forwards client -> server frames, observing them on the way.
func (p *Proxy) pumpClient(in io.Reader, toServer, toClient *jsonrpc.Writer) {
	r := jsonrpc.NewReader(in, p.opts.MaxFrameBytes)
	for {
		raw, err := r.ReadFrame()
		if err != nil {
			if err != io.EOF {
				p.opts.Log.Errorf("client read: %v", err)
			}
			return
		}
		var reveal string
		for _, f := range explode(raw) {
			ev := p.opts.Session.ObserveClient(f)
			if ev.RevealedTool != "" {
				reveal = ev.RevealedTool
			}
		}
		if err := toServer.WriteFrame(raw); err != nil {
			p.opts.Log.Errorf("server write: %v", err)
			return
		}
		if reveal != "" {
			p.maybeNotify(reveal, toClient)
		}
	}
}

// pumpServer forwards server -> client frames, pruning tools/list responses.
func (p *Proxy) pumpServer(out io.Reader, toClient *jsonrpc.Writer) {
	r := jsonrpc.NewReader(out, p.opts.MaxFrameBytes)
	for {
		raw, err := r.ReadFrame()
		if err != nil {
			if err != io.EOF {
				p.opts.Log.Errorf("server read: %v", err)
			}
			return
		}
		raw = p.rewriteServerFrame(raw)
		if err := toClient.WriteFrame(raw); err != nil {
			p.opts.Log.Errorf("client write: %v", err)
			return
		}
	}
}

// rewriteServerFrame prunes a tools/list response, transparently handling the
// legacy batch form. Anything it cannot fully understand is returned as-is.
func (p *Proxy) rewriteServerFrame(raw []byte) []byte {
	f := jsonrpc.Parse(raw)
	if f.Kind == jsonrpc.KindBatch {
		items := jsonrpc.SplitBatch(raw)
		if items == nil {
			return raw
		}
		changed := false
		for i, item := range items {
			sub := jsonrpc.Parse(item)
			if !p.opts.Session.ObserveServer(sub) {
				continue
			}
			out, stats, ok := p.opts.Session.PruneToolsResult(item)
			if !ok {
				continue
			}
			p.logPrune(stats)
			items[i] = out
			changed = true
		}
		if !changed {
			return raw
		}
		p.notifyPending.Store(false)
		return mcp.JoinArray(items)
	}

	if !p.opts.Session.ObserveServer(f) {
		return raw
	}
	out, stats, ok := p.opts.Session.PruneToolsResult(raw)
	if !ok {
		p.opts.Log.Debugf("tools/list response left untouched (unrecognised shape)")
		return raw
	}
	p.logPrune(stats)
	// The client now holds a fresh list, so a future reveal deserves a new
	// notification.
	p.notifyPending.Store(false)
	return out
}

func (p *Proxy) logPrune(stats interface {
	TokenReduction() float64
	ByteReduction() float64
}) {
	if !p.opts.Log.Enabled(LevelInfo) {
		return
	}
	p.opts.Log.Infof("tools/list pruned: tokens -%.1f%% bytes -%.1f%%",
		stats.TokenReduction()*100, stats.ByteReduction()*100)
}

// maybeNotify tells the client its tool list changed so it re-fetches the full
// schema of a tool the model just reached for.
//
// It is a no-op when the upstream server never declared the listChanged
// capability: a client that did not opt in may ignore or reject the
// notification, and the pruner must not invent capabilities on the server's
// behalf.
func (p *Proxy) maybeNotify(tool string, toClient *jsonrpc.Writer) {
	if !p.opts.Session.Capabilities().ToolsListChanged {
		p.opts.Log.Debugf("reveal %q: server has no tools.listChanged capability, skipping refresh", tool)
		return
	}
	if p.notifyPending.Swap(true) {
		return
	}
	if err := toClient.WriteFrame(jsonrpc.Notification(mcp.NotifyToolsListChanged)); err != nil {
		p.opts.Log.Errorf("notify write: %v", err)
		return
	}
	p.opts.Log.Debugf("reveal %q: sent %s", tool, mcp.NotifyToolsListChanged)
}

// explode yields the frames carried by a line, flattening a batch.
func explode(raw []byte) []jsonrpc.Frame {
	f := jsonrpc.Parse(raw)
	if f.Kind != jsonrpc.KindBatch {
		return []jsonrpc.Frame{f}
	}
	items := jsonrpc.SplitBatch(raw)
	frames := make([]jsonrpc.Frame, 0, len(items))
	for _, item := range items {
		frames = append(frames, jsonrpc.Parse(item))
	}
	return frames
}
