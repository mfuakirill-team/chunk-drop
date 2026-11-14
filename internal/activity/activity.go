// Package activity is the engine's concise activity feed: what is
// uploading (live sessions with receivers), what was downloaded or
// deleted (sync mesh), what failed. RAM-only ring buffer; the
// dashboard polls it (GET /api/activity) to show "the system is
// working" without reading engine console logs.
package activity

import (
	"sync"
	"time"
)

// Event kinds — the UI maps them to arrows/colors.
const (
	KindUp    = "up"    // serving a session / sync serving started
	KindDown  = "down"  // sync pulled a file from peers
	KindInfo  = "info"  // session started, cancelled, config notes
	KindError = "error" // failures (pull failed, peer unreachable)
)

// Event is one activity line.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`
	Msg  string    `json:"msg"`
}

// Log is a fixed-size ring buffer of events, safe for concurrent use.
type Log struct {
	mu     sync.Mutex
	events []Event
	max    int
}

// NewLog creates a log keeping the last max events.
func NewLog(max int) *Log {
	if max <= 0 {
		max = 200
	}
	return &Log{max: max}
}

// Add appends an event (kind see Kind* constants).
func (l *Log) Add(kind, msg string) {
	if l == nil {
		return // nil log = disabled, callers need no guards
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, Event{Time: time.Now(), Kind: kind, Msg: msg})
	if len(l.events) > l.max {
		l.events = l.events[len(l.events)-l.max:]
	}
}

// List returns a copy of the events, oldest first.
func (l *Log) List() []Event {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}
