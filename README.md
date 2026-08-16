# mcp-diet

**Put your MCP servers on a token diet.**

A transparent stdio proxy that sits between an MCP client and an MCP server and
cuts the tokens your agent spends on tool schemas — typically **50–78% fewer
tokens per `tools/list`**, added latency **under 1 ms**, no LLM call, no vector
database, no config required.

[![Go Reference](https://pkg.go.dev/badge/github.com/albererinofigo-droid/mcp-diet.svg)](https://pkg.go.dev/github.com/albererinofigo-droid/mcp-diet)
[![ci](https://github.com/albererinofigo-droid/mcp-diet/actions/workflows/ci.yml/badge.svg)](https://github.com/albererinofigo-droid/mcp-diet/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/albererinofigo-droid/mcp-diet)](https://goreportcard.com/report/github.com/albererinofigo-droid/mcp-diet)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

```sh
# before: 7,079 tokens of tool schema on every single turn
# after:  3,074
mcp-diet --server "npx -y @modelcontextprotocol/server-filesystem /srv"
```

```
Claude Desktop ──stdio──▶ mcp-diet ──stdio──▶ your MCP server
                          ▲
                          └── ranks tools, compresses the ones the agent
                              is not about to use, restores them on demand
```

Every agent turn re-sends the full tool list. A single MCP server with 28 tools
costs about 7,000 tokens of schema on every request; three servers wired into
one client routinely cost 20,000. Most of it describes tools the model was
never going to call on that turn.

`mcp-diet` keeps the tools that matter right now at full fidelity and
compresses the rest to a name plus a one-line description. Nothing becomes
uncallable: the moment the model reaches for a compressed tool, the proxy
restores its full schema.

---

## Install

```sh
go install github.com/albererinofigo-droid/mcp-diet/cmd/mcp-diet@latest
```

or build from source:

```sh
git clone https://github.com/albererinofigo-droid/mcp-diet
cd mcp-diet
make build      # ./bin/mcp-diet
```

Requires Go 1.23+. No dependencies outside the standard library.

## Use

Wrap any stdio MCP server command:

```sh
mcp-diet --server "npx -y @modelcontextprotocol/server-filesystem /srv"
mcp-diet -- uvx mcp-server-git --repository /srv/app
```

In a client config, replace the command with `mcp-diet` and move the original
one into `--server`:

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "mcp-diet",
      "args": [
        "--top-n", "8",
        "--server", "npx -y @modelcontextprotocol/server-filesystem /srv"
      ]
    }
  }
}
```

Nothing else changes. The proxy speaks the same protocol on both sides and
forwards everything it does not need to touch byte-for-byte.

### See the savings before you commit

`analyze` runs the exact production algorithm against a captured `tools/list`
payload, with no server involved:

```sh
mcp-diet analyze testdata/tools.json
mcp-diet analyze testdata/tools.json --calls git_diff,git_commit --top-n 6
```

```
TOOL                          TIER        SCORE  SAVED
fs_read_file                  full        0.000  -
fs_write_file                 compressed  0.000  605 B
...
tools       28 (full 8, compressed 20, dropped 0)
bytes       26283 -> 10943  (-58.4%)
est tokens  7079 -> 3074  (-56.6%)
prune time  0.455 ms
```

Add `--json` for machine-readable output including the per-tool score
breakdown.

---

## How it decides

There is no model in the loop. Ranking is a weighted sum of five signals, each
normalised to `[0,1]`, computed from state the proxy already sees on the wire.

**1. Transition probability (`successor`, weight 1.00).** The state engine
maintains a bounded directed weighted graph over tool executions: an edge
`A → B` counts how often `B` ran immediately after `A` in this session. The
score of a candidate is `P(next = candidate | last executed tool)`. After a few
turns this alone predicts most workflows — `git_diff` reliably precedes
`git_commit`, `fs_read_file` precedes `fs_edit_file`.

The graph is deliberately **not acyclic**: agent workflows loop, and
`read → edit → read` is signal, not noise.

**2. Second-order transition (`successor2`, 0.35).** The same probability
conditioned on the tool before last, which catches `A → B → A` alternation.

**3. Recency (`recency`, 0.45).** `2^(-steps_since_last_use / halfLife)`.

**4. Frequency (`frequency`, 0.30).** The tool's share of all executions in
this session.

**5. Lexical overlap (`lexical`, 0.55).** A bounded, exponentially decaying bag
of terms harvested from live traffic — tool call arguments, tool results,
prompt and resource requests — scored against each tool's name and description.
Tool names are `snake_case` identifiers, so the tokenizer splits `snake_case`
and `camelCase` alike: `fs_read_file` matches an agent that has been talking
about *reading* a *file*.

This is the job a semantic router would hand to an embedding model and a vector
store. For tool selection, exact term overlap is both sharper (tool names are
not prose) and about four orders of magnitude cheaper.

**Explicit intent (`revealed`, 2.00).** When the model calls a tool, that tool
is pinned at full fidelity for the next `revealSteps` executions. This is what
makes compression safe rather than lossy.

Ties break on the tool's original position, so the ranking is fully
reproducible.

## What compression actually does

A tool outside the top-N keeps everything the model needs to *name* it and
loses everything it only needs to *call* it:

```jsonc
// before — 605 bytes
{
  "name": "fs_write_file",
  "description": "Create a new file or completely overwrite an existing file with new content. Use with caution as it will overwrite existing files without warning. Handles text content with proper encoding.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path":    { "type": "string",  "description": "Destination path of the file to write." },
      "content": { "type": "string",  "description": "Full textual content to write into the file." },
      "encoding":{ "type": "string",  "enum": ["utf-8","utf-16","latin-1"], "default": "utf-8" },
      "createDirectories": { "type": "boolean", "default": false },
      "mode":    { "type": "string",  "description": "POSIX file mode in octal." }
    },
    "required": ["path","content"],
    "additionalProperties": false
  }
}

// after — 152 bytes
{
  "description": "Create a new file or completely overwrite an existing file with new content. Use with…",
  "inputSchema": { "type": "object" },
  "name": "fs_write_file"
}
```

- The name is preserved exactly.
- The description is truncated to `maxDescriptionChars` (default 100) on a word
  boundary, rune-safe.
- `inputSchema` becomes the permissive `{"type":"object"}` rather than being
  deleted — an absent `inputSchema` is invalid per the MCP schema, and an empty
  object schema satisfies every client validator for 19 bytes.
- `required`, `outputSchema` and `annotations` are dropped until the tool is
  revealed.
- If the compressed form would not be smaller, the original is kept.

### The reveal loop

```
model calls a compressed tool
        │
        ├─▶ proxy marks it revealed and forwards the call unchanged
        │
        └─▶ proxy emits notifications/tools/list_changed to the client
                    │
                    └─▶ client re-requests tools/list
                                │
                                └─▶ that tool now carries its full schema
```

The notification is only sent when the upstream server declared
`capabilities.tools.listChanged` at initialize time. The proxy never invents
capabilities on a server's behalf; without it, compression still applies and
the reveal takes effect on the client's next natural `tools/list`.

## Deterministic output, warm caches

Prompt caching on both Anthropic and OpenAI keys off an exact prefix. A pruner
that re-ranks tools on every turn would invalidate that cache and cost more
than it saves. So:

- **Upstream order is preserved** by default (`preserveOrder: true`). Only tool
  *contents* change, never their positions.
- **Kept tools are forwarded as the exact upstream bytes** — never decoded and
  re-encoded.
- **Compressed tools are built with fixed key order** and a hand-rolled string
  encoder, so identical inputs give identical bytes.
- **HTML escaping is off.** `encoding/json` would rewrite `<`, `>` and `&` as
  six-byte `\uXXXX` escapes — a payload the server never sent, and a token cost
  in every schema containing `Name <email>`.

`TestPruneIsDeterministic` replays the same event sequence 50 times from a cold
state and asserts byte equality.

## Measured

Apple M5, Go 1.26, fixture of 28 realistic MCP tool schemas (26 KB).

| Metric | Value |
| --- | --- |
| Token reduction, warm session | **54–58%** |
| Token reduction, cold start (no history) | **57%** |
| Token reduction, 500-tool list | **78%** |
| Prune latency, 28 tools | 76 µs mean, **0.50 ms worst** |
| Full frame rewrite (decode + prune + re-encode), 26 KB | **0.44 ms** mean |
| Prune latency, 500 tools | 1.3 ms mean, **3.2 ms worst** |
| Allocations per prune, 28 tools | 134 |
| Token estimation throughput | 1.07 GB/s, zero allocations |
| Retained session state after 20k tool calls | **2.1 MiB** |
| Process RSS, idle | ~7 MiB |
| Process RSS, streaming 40 MB of tool JSON | ~13 MiB |

Reproduce:

```sh
make bench          # benchmarks
make test           # includes the latency and memory budget tests
```

Two honesty notes on those numbers:

- **Token counts are estimates.** `EstimateTokens` is a heuristic (word runs at
  ~4 chars/token, punctuation runs at ~2), not a BPE tokenizer — it ships no
  vocabulary file. It is checked against `cl100k_base` reference counts in
  `prune/tokens_test.go` and stays within a 0.6×–1.8× band. Both sides of the
  before/after ratio carry the same bias, so the *reduction* is trustworthy
  even where the absolute count is not. Byte counts are exact.
- **The <10 MB memory target holds for the pruner's own state, not for total
  process RSS.** Retained state is ~2 MiB and test-enforced under 10 MiB; the
  Go runtime itself accounts for the remaining ~7 MiB floor. Lowering `GOGC`
  saves about 1.5 MiB at a measurable latency cost, so the default is left
  alone.

## Configuration

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--server <cmd>` | — | Upstream MCP server command (shell-style quoting) |
| `--top-n <n>` | 8 | Tools keeping their full schema |
| `--max-desc <n>` | 100 | Description budget for compressed tools |
| `--min-tools <n>` | 6 | Skip pruning for lists this small |
| `--drop-below <f>` | 0 | Drop instead of compress below this score |
| `--pin <name>` | — | Never prune this tool; repeatable, trailing `*` allowed |
| `--no-prune` | off | Forward everything untouched (measurement baseline) |
| `--log <level>` | off | `off` \| `error` \| `info` \| `debug` |
| `--stats` | off | Print a savings summary to stderr on exit |
| `--config <path>` | — | JSON config file |

A config file overrides only the keys it mentions — see
[`mcp-diet.example.json`](mcp-diet.example.json).

**On `--drop-below`.** The default is `0`, which never removes a tool. Dropping
is strictly more dangerous than compressing: a compressed tool can still be
named by the model and recovered, a dropped one cannot. Enable it only when you
have measured that the tail is genuinely dead weight.

Diagnostics always go to **stderr**. On a stdio transport, stdout carries
protocol frames; a stray log line there corrupts the session.

## Design notes

```
cmd/mcp-diet/   CLI: flag parsing, shell-style command splitting, analyze
proxy/            process spawn, bidirectional frame pumps, reveal notifications
session/          per-connection state, wire observation, tools/list rewriting
prune/            ranking, schema compression, token estimation, term window
graph/            bounded tool-transition graph
jsonrpc/          newline-delimited framing, classification, lossless re-encode
mcp/              the slice of MCP the proxy must understand
```

Deliberate choices:

- **Blocking goroutines, no async runtime.** Two pumps and a child process.
  Nothing here benefits from an event loop.
- **Raw bytes wherever possible.** Tools are carried as `json.RawMessage`;
  numbers are decoded with `UseNumber` so re-encoding cannot turn `1e10` into
  `10000000000` or round an int64 through a float.
- **Fail open.** Every rewrite path returns the original frame if anything is
  unexpected: malformed JSON, an unknown result shape, an error response, a
  batch it cannot split. A proxy that does not understand a message must not
  rewrite it.
- **Bounded everything.** Graph nodes, edges per node, term window size, frame
  size, strings harvested per frame. Eviction is LRU with name tie-breaks, so
  two sessions fed the same events end up in the same state.

### Limits and non-goals

- **stdio only.** SSE and streamable-HTTP transports are not implemented yet;
  the pruning core is transport-agnostic and reusable as a library.
- **Cold start has no history.** The first `tools/list` of a session ranks on
  lexical signal alone and falls back to upstream order. It still halves the
  payload, but accuracy improves after the first few tool calls.
- **Pagination.** Each `tools/list` page is pruned independently, so the top-N
  budget applies per page.
- **Duplicate JSON-RPC ids.** A client that reuses an in-flight request id gets
  correct pass-through, not pruning. Unique ids are a protocol requirement.
- **Tool calls are recorded as intent, not as success.** A failed call still
  shapes the graph.

## Testing

```sh
make test          # unit + end-to-end, with -race
make cover         # coverage report
```

The end-to-end tests spawn a real child process — the test binary re-executes
itself as a mock MCP server — and drive the proxy over real pipes, covering
initialize, capability negotiation, pruning, the reveal loop, pass-through
fidelity and clean shutdown.

## License

MIT. See [LICENSE](LICENSE).
