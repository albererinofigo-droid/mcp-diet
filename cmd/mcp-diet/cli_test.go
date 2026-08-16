package main

import (
	"os"
	"strings"
	"testing"
)

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`npx -y @modelcontextprotocol/server-filesystem /tmp`, []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"}},
		{`  uvx   mcp-server-git   `, []string{"uvx", "mcp-server-git"}},
		{`node "/Applications/My Server/index.js" --port 8080`, []string{"node", "/Applications/My Server/index.js", "--port", "8080"}},
		{`sh -c 'echo hello world'`, []string{"sh", "-c", "echo hello world"}},
		{`prog --json="{\"a\":1}"`, []string{"prog", `--json={"a":1}`}},
		{`prog a\ b`, []string{"prog", "a b"}},
		{`prog ''`, []string{"prog", ""}},
	}
	for _, tc := range cases {
		got, err := shellSplit(tc.in)
		if err != nil {
			t.Errorf("shellSplit(%q): %v", tc.in, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("shellSplit(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellSplitErrors(t *testing.T) {
	for _, in := range []string{`prog "unterminated`, `prog 'nope`, ``, `   `} {
		if got, err := shellSplit(in); err == nil {
			t.Errorf("shellSplit(%q) = %q, want an error", in, got)
		}
	}
}

func TestResolveServerArgv(t *testing.T) {
	got, err := resolveServerArgv(`node server.js --flag`, nil)
	if err != nil || strings.Join(got, " ") != "node server.js --flag" {
		t.Errorf("--server form: %q %v", got, err)
	}

	got, err = resolveServerArgv("", []string{"node", "server.js"})
	if err != nil || strings.Join(got, " ") != "node server.js" {
		t.Errorf("trailing form: %q %v", got, err)
	}

	if _, err := resolveServerArgv("node a.js", []string{"node", "b.js"}); err == nil {
		t.Error("expected an error when both forms are used")
	}
	if _, err := resolveServerArgv("", nil); err == nil {
		t.Error("expected an error when no command is given")
	}
}

func TestRunVersionAndHelp(t *testing.T) {
	if code := run([]string{"version"}); code != 0 {
		t.Errorf("version exited %d", code)
	}
	if code := run([]string{"help"}); code != 0 {
		t.Errorf("help exited %d", code)
	}
	if code := run(nil); code != 2 {
		t.Errorf("no arguments exited %d, want 2", code)
	}
}

func TestAnalyzeCommand(t *testing.T) {
	if code := runAnalyze([]string{"../../testdata/tools.json"}); code != 0 {
		t.Errorf("analyze exited %d", code)
	}
	if code := runAnalyze([]string{"--json", "--top-n", "4", "--calls", "git_diff,git_commit", "../../testdata/tools.json"}); code != 0 {
		t.Errorf("analyze --json exited %d", code)
	}
	if code := runAnalyze([]string{"/nonexistent/tools.json"}); code == 0 {
		t.Error("analyze accepted a missing file")
	}
	if code := runAnalyze(nil); code != 2 {
		t.Error("analyze without a file should exit 2")
	}
}

func TestExtractToolsAcceptsEveryShape(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	full, err := ExtractTools(raw)
	if err != nil {
		t.Fatalf("full response: %v", err)
	}
	if len(full) != 28 {
		t.Fatalf("got %d tools", len(full))
	}

	bare := []byte(`{"tools":[{"name":"a","inputSchema":{"type":"object"}}]}`)
	if got, err := ExtractTools(bare); err != nil || len(got) != 1 {
		t.Errorf("bare result: %v %d", err, len(got))
	}

	arr := []byte(`[{"name":"a","inputSchema":{"type":"object"}}]`)
	if got, err := ExtractTools(arr); err != nil || len(got) != 1 {
		t.Errorf("array: %v %d", err, len(got))
	}

	if _, err := ExtractTools([]byte(`{"unrelated":true}`)); err == nil {
		t.Error("expected an error for a payload with no tools")
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" a , b ,, c "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("got %q", got)
	}
	if got := splitList("  "); got != nil {
		t.Errorf("got %q, want nil", got)
	}
}

func TestAnalyzeAcceptsFlagsAfterThePath(t *testing.T) {
	// Go's flag package stops at the first positional argument; the analyze
	// command re-parses so both orderings work.
	if code := runAnalyze([]string{"../../testdata/tools.json", "--top-n", "5", "--calls", "git_status,git_diff"}); code != 0 {
		t.Errorf("flags after the path exited %d", code)
	}
	if code := runAnalyze([]string{"a.json", "b.json"}); code != 2 {
		t.Errorf("two paths exited %d, want 2", code)
	}
}
