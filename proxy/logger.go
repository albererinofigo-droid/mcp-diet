package proxy

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// Level controls diagnostic verbosity.
type Level uint8

// Log levels, lowest to highest.
const (
	LevelOff Level = iota
	LevelError
	LevelInfo
	LevelDebug
)

// ParseLevel maps a CLI string to a Level.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "silent":
		return LevelOff, nil
	case "error":
		return LevelError, nil
	case "info":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	}
	return LevelOff, fmt.Errorf("unknown log level %q (want off|error|info|debug)", s)
}

// Logger writes diagnostics to a sink that is never the MCP data channel.
//
// This matters: on stdio transports the proxy's stdout carries protocol
// frames, so a stray log line there corrupts the session. Every diagnostic
// goes to stderr.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
}

// NewLogger returns a logger writing to w at the given level.
func NewLogger(w io.Writer, level Level) *Logger {
	return &Logger{w: w, level: level}
}

// Enabled reports whether a level would be emitted.
func (l *Logger) Enabled(lv Level) bool {
	return l != nil && l.w != nil && lv <= l.level
}

func (l *Logger) logf(lv Level, format string, args ...any) {
	if !l.Enabled(lv) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "[mcp-diet] "+format+"\n", args...)
}

// Errorf logs at error level.
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

// Infof logs at info level.
func (l *Logger) Infof(format string, args ...any) { l.logf(LevelInfo, format, args...) }

// Debugf logs at debug level.
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
