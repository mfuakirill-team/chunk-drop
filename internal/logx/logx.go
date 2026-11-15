// Package logx is the engine's leveled logger: debug/info/warn/error,
// gated by -log-level / -debug (docs/USAGE.md §3). Every failure and
// every transfer step lands here — nothing fails silently.
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Level is a log severity. Debug is off unless explicitly enabled —
// it is chatty (every HTTP request, every chunk).
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var current atomic.Int32

func init() { current.Store(int32(LevelInfo)) }

// SetLevel sets the minimum level that is printed.
func SetLevel(l Level) { current.Store(int32(l)) }

// Current returns the active level.
func Current() Level { return Level(current.Load()) }

// ParseLevel maps a -log-level value to a Level (case-insensitive).
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", s)
	}
}

// tag is the component the line came from ("http", "upload", "sync"…).
func emit(l Level, tag, format string, args ...any) {
	if int32(l) < current.Load() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	var line string
	switch l {
	case LevelDebug:
		line = fmt.Sprintf("engine: [debug] %s: %s", tag, msg)
	case LevelInfo:
		line = fmt.Sprintf("engine: %s: %s", tag, msg)
	case LevelWarn:
		line = fmt.Sprintf("engine: [warn] %s: %s", tag, msg)
	default:
		line = fmt.Sprintf("engine: [error] %s: %s", tag, msg)
	}
	_, _ = fmt.Fprintln(os.Stderr, line)
}

// Debug logs at debug level (only with -debug / -log-level debug).
func Debug(tag, format string, args ...any) { emit(LevelDebug, tag, format, args...) }

// Info logs an ordinary progress step.
func Info(tag, format string, args ...any) { emit(LevelInfo, tag, format, args...) }

// Warn logs a recovered/unusual condition worth seeing by default.
func Warn(tag, format string, args ...any) { emit(LevelWarn, tag, format, args...) }

// Error logs a failure — always visible.
func Error(tag, format string, args ...any) { emit(LevelError, tag, format, args...) }

// Adapter returns a log.Printf-style function that logs at the given
// level under a tag — for components configured with a Logf field.
func Adapter(l Level, tag string) func(format string, args ...any) {
	return func(format string, args ...any) { emit(l, tag, format, args...) }
}

// Nowf is a convenience for elapsed-time prefixes in debug lines.
func Nowf() string { return time.Now().Format("15:04:05.000") }
