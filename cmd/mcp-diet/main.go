// Command mcp-diet is a transparent stdio proxy that shrinks the tool
// schemas an MCP server advertises, cutting the tokens a model spends on
// function calling.
//
// Usage:
//
//	mcp-diet --server "npx -y @modelcontextprotocol/server-filesystem /tmp"
//	mcp-diet -- npx -y @modelcontextprotocol/server-filesystem /tmp
//	mcp-diet analyze tools.json
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/albererinofigo-droid/mcp-diet/proxy"
	"github.com/albererinofigo-droid/mcp-diet/prune"
	"github.com/albererinofigo-droid/mcp-diet/session"
)

// version is overridden at build time with -ldflags "-X main.version=v0.1.0".
// When the binary comes from `go install ...@v1.2.3` instead, no ldflags are
// applied, so fall back to the module version the toolchain recorded.
var version = ""

func init() {
	if version != "" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
		return
	}
	version = "dev"
}

const usage = `mcp-diet %s — token-pruning proxy for MCP servers

USAGE
  mcp-diet [flags] --server "<command>"
  mcp-diet [flags] -- <command> [args...]
  mcp-diet analyze <file|-> [flags]
  mcp-diet version

FLAGS
  --server <cmd>      upstream MCP server command line (shell-style quoting)
  --config <path>     JSON config file
  --top-n <n>         tools keeping their full schema (default %d)
  --max-desc <n>      description budget for compressed tools (default %d)
  --min-tools <n>     skip pruning for lists this small (default %d)
  --drop-below <f>    drop instead of compress below this score (default 0, never drop)
  --pin <name>        never prune this tool; repeatable, trailing '*' allowed
  --no-prune          forward everything untouched (measurement baseline)
  --log <level>       off|error|info|debug (default off)
  --stats             print a savings summary to stderr on exit

NOTES
  Diagnostics always go to stderr; stdout carries protocol frames only.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) > 0 {
		switch argv[0] {
		case "analyze":
			return runAnalyze(argv[1:])
		case "version", "--version", "-version":
			fmt.Println("mcp-diet", version)
			return 0
		case "help", "--help", "-h":
			printUsage()
			return 0
		}
	}

	fs := flag.NewFlagSet("mcp-diet", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = printUsage

	var pins stringList
	var (
		server     = fs.String("server", "", "")
		configPath = fs.String("config", "", "")
		topN       = fs.Int("top-n", -1, "")
		maxDesc    = fs.Int("max-desc", -1, "")
		minTools   = fs.Int("min-tools", -1, "")
		dropBelow  = fs.Float64("drop-below", -1, "")
		noPrune    = fs.Bool("no-prune", false, "")
		logLevel   = fs.String("log", "off", "")
		stats      = fs.Bool("stats", false, "")
	)
	fs.Var(&pins, "pin", "")

	if err := fs.Parse(argv); err != nil {
		return 2
	}

	serverArgv, err := resolveServerArgv(*server, fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-diet: %v\n\n", err)
		printUsage()
		return 2
	}

	cfg := prune.DefaultConfig()
	if *configPath != "" {
		loaded, err := prune.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp-diet: %v\n", err)
			return 1
		}
		cfg = loaded
	}
	if *topN >= 0 {
		cfg.TopN = *topN
	}
	if *maxDesc >= 0 {
		cfg.MaxDescriptionChars = *maxDesc
	}
	if *minTools >= 0 {
		cfg.MinTools = *minTools
	}
	if *dropBelow >= 0 {
		cfg.DropBelow = *dropBelow
	}
	if len(pins) > 0 {
		cfg.Pinned = append(cfg.Pinned, pins...)
	}
	if *noPrune {
		cfg.Enabled = false
	}
	cfg.Normalize()

	level, err := proxy.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-diet: %v\n", err)
		return 2
	}
	if *stats && level < proxy.LevelInfo {
		level = proxy.LevelInfo
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess := session.New(cfg)
	code, err := proxy.Run(ctx, proxy.Options{
		Argv:      serverArgv,
		ClientIn:  os.Stdin,
		ClientOut: os.Stdout,
		ServerErr: os.Stderr,
		Session:   sess,
		Log:       proxy.NewLogger(os.Stderr, level),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp-diet: %v\n", err)
		return 1
	}
	return code
}

// resolveServerArgv accepts either --server "cmd args" or a trailing
// "-- cmd args", but not both.
func resolveServerArgv(server string, rest []string) ([]string, error) {
	hasServer := strings.TrimSpace(server) != ""
	switch {
	case hasServer && len(rest) > 0:
		return nil, fmt.Errorf("use either --server or a trailing command, not both")
	case hasServer:
		return shellSplit(server)
	case len(rest) > 0:
		return rest, nil
	}
	return nil, fmt.Errorf("no MCP server command given")
}

func printUsage() {
	def := prune.DefaultConfig()
	fmt.Fprintf(os.Stderr, usage, version, def.TopN, def.MaxDescriptionChars, def.MinTools)
}

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}
