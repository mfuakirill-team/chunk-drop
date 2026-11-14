// Package session implements per-transfer session lifecycle:
// RAM-only state, 16-byte random keys, 24 h expiry, cancel
// (docs/PROTOCOL.md §1, SECURITY.md §6).
package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"chunkdrop/internal/transfer"
)

const (
	// KeySize is the session key size in bytes (PROTOCOL.md §1).
	KeySize = 16
	// DefaultTTL is how long a session stays alive (24 h).
	DefaultTTL = 24 * time.Hour
)

var (
	// ErrNotFound maps to 401: unknown id or wrong key, same generic body.
	ErrNotFound = errors.New("session not found")
	// ErrExpired maps to 410.
	ErrExpired = errors.New("session expired")
)

// State of a session for API/UI consumption.
type State struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	FileCount   int       `json:"fileCount"`
	TotalBytes  int64     `json:"totalBytes"`
	PackedAsZip bool      `json:"packedAsZip"`
	Paths       []string  `json:"paths"`
}

// Progress is the receiver chunk bitmap POSTed to /api/progress.
type Progress struct {
	Receiver  string         `json:"receiver"` // free-form receiver label
	UpdatedAt time.Time      `json:"updatedAt"`
	Have      map[string]any `json:"have"` // fileIndex → []chunkIndex
}

// Session is one transfer attempt.
type Session struct {
	ID       string
	Key      []byte // 16 random bytes, base64url (22 chars) on the wire
	Manifest *transfer.Manifest

	CreatedAt time.Time
	ExpiresAt time.Time
	Cancelled bool

	// Receiver progress state, POSTed via /api/progress.
	Progress   Progress
	ProgressAt time.Time

	mu sync.Mutex
}

// Manager owns all live sessions. RAM-only by design: an engine restart
// kills every session (SECURITY.md key-rotation note relies on this).
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	ttl      time.Duration
}

// NewManager creates a session manager with the default 24 h TTL.
func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}, ttl: DefaultTTL}
}

// NewKey returns a fresh random 16-byte session key.
func NewKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// KeyEncoding returns the base64url form used in QR links and headers.
func KeyEncoding(k []byte) string { return base64.RawURLEncoding.EncodeToString(k) }

// KeyEncoded returns this session's key in wire form (QR/header).
func (s *Session) KeyEncoded() string { return KeyEncoding(s.Key) }

// KeyLen returns the expected encoded key length (22 chars for 16 bytes).
func KeyLen() int { return base64.RawURLEncoding.EncodedLen(KeySize) }

// NewSession creates and registers a session for the given manifest.
func (m *Manager) NewSession(manifest *transfer.Manifest) (*Session, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:        NewID(),
		Key:       key,
		Manifest:  manifest,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(m.ttl),
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s, nil
}

// ByKey finds the live session whose encoded key matches, trying every
// live session with a constant-time compare. Receiver requests carry
// only the key (the QR fragment), never the id.
func (m *Manager) ByKey(presentedKey string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var expired bool
	for _, s := range m.sessions {
		if time.Now().After(s.ExpiresAt) {
			expired = true
			continue
		}
		if subtle.ConstantTimeCompare([]byte(presentedKey), []byte(KeyEncoding(s.Key))) == 1 {
			if s.Cancelled {
				return nil, false
			}
			return s, true
		}
	}
	_ = expired
	return nil, false
}

// Get returns the live session for id — or ErrNotFound.
// Expired sessions are reaped lazily on every access.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	if s.Expired() {
		delete(m.sessions, id)
		return nil, ErrExpired
	}
	return s, nil
}

// ByKeyAll returns all live sessions (tests, dashboard listing).
func (m *Manager) ByKeyAll() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if !s.Cancelled && time.Now().Before(s.ExpiresAt) {
			out = append(out, s)
		}
	}
	return out
}

// Reap removes expired sessions (called from a ticker in the engine).
func (m *Manager) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.Expired() {
			delete(m.sessions, id)
		}
	}
}

// Count returns the number of live sessions (tests, UI).
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if !s.Expired() {
			n++
		}
	}
	return n
}

// Cancel closes the session immediately.
func (s *Session) Cancel() {
	s.mu.Lock()
	s.Cancelled = true
	s.mu.Unlock()
}

// Cancel removes the session from the manager.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.Cancel()
	delete(m.sessions, id)
	return nil
}

// Expired reports whether the session is past its TTL or cancelled.
func (s *Session) Expired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cancelled || time.Now().After(s.ExpiresAt)
}

// CheckAuth verifies a presented session key in constant time.
// Unknown ids also consume a comparison so a wrong id leaks nothing
// about which ids exist.
func (m *Manager) CheckAuth(id, presentedKey string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		// Burn a comparison against a dummy so timing is uniform.
		subtle.ConstantTimeCompare([]byte(presentedKey), []byte(KeyEncoding(make([]byte, KeySize))))
		return nil, ErrNotFound
	}
	if time.Now().After(s.ExpiresAt) || s.Cancelled {
		delete(m.sessions, id)
		if s.Cancelled {
			return nil, ErrNotFound
		}
		return nil, ErrExpired
	}
	if subtle.ConstantTimeCompare([]byte(presentedKey), []byte(KeyEncoding(s.Key))) != 1 {
		return nil, ErrNotFound
	}
	return s, nil
}

// Snapshot returns a copy of the session's public state.
func (s *Session) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{
		ID:          s.ID,
		CreatedAt:   s.CreatedAt,
		ExpiresAt:   s.ExpiresAt,
		FileCount:   s.Manifest.FileCount(),
		TotalBytes:  s.Manifest.TotalSize(),
		PackedAsZip: s.Manifest.PackedAsZip,
	}
	st.Paths = make([]string, 0, len(s.Manifest.Files))
	for _, f := range s.Manifest.Files {
		st.Paths = append(st.Paths, f.Path)
	}
	return st
}

// SetProgress stores the latest receiver bitmap POST.
func (s *Session) SetProgress(p Progress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.UpdatedAt = time.Now()
	s.Progress = p
	s.ProgressAt = p.UpdatedAt
}

// ProgressAge returns time since the last progress POST (UI display).
func (s *Session) ProgressAge() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ProgressAt.IsZero() {
		return 0
	}
	return time.Since(s.ProgressAt)
}

// HasProgress reports whether any progress was received yet.
func (s *Session) HasProgress() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ProgressAt.IsZero()
}
