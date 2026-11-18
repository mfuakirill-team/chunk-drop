// Package ratemeter tracks per-peer transfer rates over a rolling
// window: the dashboard's Clients card shows download/upload KB/s per
// client (M2 step ④ rework). Counters are bytes; Add is called at the
// points where bytes actually move — the engine's chunk handler
// (serving = upload from this engine's view) and the syncmesh puller
// (writing = download).
//
// The window is a short fixed slice of (time, bytes) buckets per peer:
// cheap, allocation-light, and the 3 s UI poll always has fresh data.
package ratemeter

import (
	"sync"
	"time"
)

// DefaultWindow is the period Rates averages over.
const DefaultWindow = 5 * time.Second

// bucket is one time-slice of bytes for one peer.
type bucket struct {
	at    time.Time
	bytes int64
}

// Meter holds per-peer rolling byte counters.
type Meter struct {
	mu      sync.Mutex
	window  time.Duration
	peers   map[string][]bucket
	maxBkts int
}

// New returns a Meter averaging over window (<=0 → DefaultWindow).
func New(window time.Duration) *Meter {
	if window <= 0 {
		window = DefaultWindow
	}
	// 2 buckets/second is plenty for a rate readout.
	return &Meter{window: window, peers: map[string][]bucket{}, maxBkts: int(window/(500*time.Millisecond)) + 2}
}

// Add records n bytes transferred for a peer (empty id is ignored —
// unidentified conns have no row to attribute to).
func (m *Meter) Add(id string, n int64) {
	if id == "" || n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	bk := m.peers[id]
	// Reuse the newest bucket when it is fresh; otherwise start one.
	if ln := len(bk); ln > 0 && now.Sub(bk[ln-1].at) < 500*time.Millisecond {
		bk[ln-1].bytes += n
		bk[ln-1].at = now
	} else {
		bk = append(bk, bucket{at: now, bytes: n})
		if len(bk) > m.maxBkts {
			bk = bk[len(bk)-m.maxBkts:]
		}
	}
	m.peers[id] = bk
}

// Rates snapshots bytes-per-second per peer over the window, dropping
// peers with no bytes inside it (idle rows show "–" in the UI, not 0).
// The returned map is a copy; callers may hold it freely.
func (m *Meter) Rates() map[string]float64 {
	cutoff := time.Now().Add(-m.window)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.peers))
	for id, bk := range m.peers {
		var bytes int64
		earliest := time.Time{}
		for _, b := range bk {
			if b.at.Before(cutoff) {
				continue
			}
			bytes += b.bytes
			if earliest.IsZero() || b.at.Before(earliest) {
				earliest = b.at
			}
		}
		if bytes == 0 {
			delete(m.peers, id) // idle peer: forget its history
			continue
		}
		span := m.window
		if !earliest.IsZero() {
			if s := time.Since(earliest) + 500*time.Millisecond; s < span {
				span = s
			}
		}
		out[id] = float64(bytes) / span.Seconds()
	}
	return out
}
