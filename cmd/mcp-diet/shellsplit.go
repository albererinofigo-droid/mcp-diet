package main

import (
	"fmt"
	"strings"
)

// shellSplit splits a command line the way a POSIX shell would tokenise it,
// minus expansions: it honours single quotes, double quotes and backslash
// escapes and collapses unquoted whitespace.
//
// It exists so that --server "npx -y @scope/pkg --flag 'a b'" behaves the way
// a user copying a line out of their MCP client config expects, without
// handing the string to a real shell.
func shellSplit(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
	)
	const (
		plain = iota
		single
		double
	)
	state := plain

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch state {
		case plain:
			switch {
			case c == '\\' && i+1 < len(runes):
				i++
				cur.WriteRune(runes[i])
				started = true
			case c == '\'':
				state = single
				started = true
			case c == '"':
				state = double
				started = true
			case c == ' ' || c == '\t' || c == '\n' || c == '\r':
				if started {
					args = append(args, cur.String())
					cur.Reset()
					started = false
				}
			default:
				cur.WriteRune(c)
				started = true
			}
		case single:
			if c == '\'' {
				state = plain
				continue
			}
			cur.WriteRune(c)
		case double:
			switch {
			case c == '\\' && i+1 < len(runes) && isDoubleEscapable(runes[i+1]):
				i++
				cur.WriteRune(runes[i])
			case c == '"':
				state = plain
			default:
				cur.WriteRune(c)
			}
		}
	}
	if state != plain {
		return nil, fmt.Errorf("unterminated quote in server command")
	}
	if started {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("empty server command")
	}
	return args, nil
}

// isDoubleEscapable mirrors the shell rule that inside double quotes only a
// handful of characters are escapable; everything else keeps the backslash.
func isDoubleEscapable(r rune) bool {
	switch r {
	case '"', '\\', '$', '`', '\n':
		return true
	}
	return false
}
