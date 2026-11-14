package session

import (
	"testing"
	"time"

	"chunkdrop/internal/transfer"
)

func newTestManifest(t *testing.T) *transfer.Manifest {
	t.Helper()
	m := transfer.NewManifest("test")
	m.Files = []transfer.File{{
		Index:      0,
		Path:       "hello.bin",
		Size:       1 << 20,
		ChunkCount: 1,
		Hashes:     []string{"00"},
	}}
	return m
}

func TestNewSessionKey(t *testing.T) {
	m := NewManager()
	s, err := m.NewSession(newTestManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Key) != KeySize {
		t.Errorf("key size = %d", len(s.Key))
	}
	if enc := KeyEncoding(s.Key); len(enc) != KeyLen() {
		t.Errorf("encoded key len = %d, want %d", len(enc), KeyLen())
	}
	if ttl := s.ExpiresAt.Sub(s.CreatedAt); ttl < DefaultTTL-time.Second || ttl > DefaultTTL+time.Second {
		t.Errorf("ttl = %v", ttl)
	}
	if m.Count() != 1 {
		t.Errorf("count = %d", m.Count())
	}
}

func TestCheckAuth(t *testing.T) {
	m := NewManager()
	s, _ := m.NewSession(newTestManifest(t))
	key := KeyEncoding(s.Key)

	got, err := m.CheckAuth(s.ID, key)
	if err != nil || got.ID != s.ID {
		t.Fatalf("correct key rejected: %v", err)
	}

	if _, err := m.CheckAuth(s.ID, "wrong-key-wrong-key!!"); err == nil {
		t.Error("wrong key accepted")
	}
	if _, err := m.CheckAuth("no-such-id", key); err == nil {
		t.Error("unknown id accepted")
	}
	if _, err := m.CheckAuth(s.ID, ""); err == nil {
		t.Error("empty key accepted")
	}
}

func TestExpiry(t *testing.T) {
	m := NewManager()
	s, _ := m.NewSession(newTestManifest(t))
	// Fast-forward.
	s.mu.Lock()
	s.ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if _, err := m.Get(s.ID); err != ErrExpired {
		t.Errorf("Get on expired = %v, want ErrExpired", err)
	}
	if m.Count() != 0 {
		t.Errorf("count after expiry = %d", m.Count())
	}
}

func TestCancel(t *testing.T) {
	m := NewManager()
	s, _ := m.NewSession(newTestManifest(t))
	if err := m.Cancel(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(s.ID); err != ErrNotFound {
		t.Errorf("Get after cancel = %v, want ErrNotFound", err)
	}
	if err := m.Cancel(s.ID); err == nil {
		t.Error("double cancel should fail")
	}
}

func TestProgress(t *testing.T) {
	m := NewManager()
	s, _ := m.NewSession(newTestManifest(t))
	if s.HasProgress() {
		t.Error("no progress expected at start")
	}
	s.SetProgress(Progress{Receiver: "iPhone", Have: map[string]any{"0": []any{0}}})
	if !s.HasProgress() {
		t.Error("progress missing after set")
	}
	if s.Progress.Receiver != "iPhone" {
		t.Errorf("receiver = %q", s.Progress.Receiver)
	}
}

func TestSnapshot(t *testing.T) {
	m := NewManager()
	s, _ := m.NewSession(newTestManifest(t))
	st := s.Snapshot()
	if st.ID != s.ID || st.FileCount != 1 || st.TotalBytes != 1<<20 {
		t.Errorf("snapshot = %+v", st)
	}
	if len(st.Paths) != 1 || st.Paths[0] != "hello.bin" {
		t.Errorf("paths = %v", st.Paths)
	}
}
