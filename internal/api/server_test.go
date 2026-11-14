package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chunkdrop/internal/session"
	"chunkdrop/internal/transfer"
)

// Auth + endpoint behavior tests (docs/M0-TASKS.md steps 5–6,
// SECURITY.md §6: constant-time compare, generic 401s, no path hints).

func newTestServer(t *testing.T) (*Server, *session.Session, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := transfer.NewManifest("test-session")
	if err := m.BuildFromPaths([]string{filepath.Join(dir, "a.bin")}, transfer.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	sm := session.NewManager()
	s, err := sm.NewSession(m)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(sm, nil)
	return srv, s, s.KeyEncoded()
}

func get(t *testing.T, h http.Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set(HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthWrongKey(t *testing.T) {
	srv, _, key := newTestServer(t)
	h := srv.Handler()
	for _, bad := range []string{"", "wrongwrongwrongwron", key[:len(key)-1] + "X"} {
		rec := get(t, h, "/api/manifest", bad)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", bad, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "a.bin") || strings.Contains(rec.Body.String(), "test-session") {
			t.Errorf("401 body leaks session data: %q", rec.Body.String())
		}
	}
}

func TestManifestEndpoint(t *testing.T) {
	srv, _, key := newTestServer(t)
	rec := get(t, srv.Handler(), "/api/manifest", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("content-encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var m transfer.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if m.SessionID != "test-session" || len(m.Files) != 1 {
		t.Errorf("manifest = %s", b)
	}
	// SourcePath must never be serialized.
	if bytes.Contains(b, []byte("SourcePath")) || bytes.Contains(b, []byte("absPath")) {
		t.Error("source path leaked in manifest")
	}
}

func TestChunkEndpoint(t *testing.T) {
	srv, _, key := newTestServer(t)
	h := srv.Handler()
	rec := get(t, h, "/api/chunk/0/0", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello world" {
		t.Errorf("chunk bytes = %q", rec.Body.String())
	}
	if want := srv.Sessions.Count(); want == 0 {
		t.Error("no sessions")
	}
	// Hash header must match the bytes.
	sess := mustSession(t, srv)
	if want := sess.Manifest.Files[0].Hashes[0]; rec.Header().Get(ChunkHashHeader) != want {
		t.Errorf("X-Chunk-Hash = %q, want %q", rec.Header().Get(ChunkHashHeader), want)
	}
	// Out-of-range indices → generic 404.
	for _, p := range []string{"/api/chunk/9/0", "/api/chunk/0/9", "/api/chunk/-1/0", "/api/chunk/a/b"} {
		rec := get(t, h, p, key)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func mustSession(t *testing.T, srv *Server) *session.Session {
	t.Helper()
	// There is exactly one session in these tests: fetch it by key scan.
	for _, s := range srv.Sessions.ByKeyAll() {
		return s
	}
	t.Fatal("no session found")
	return nil
}

func TestProgressAndSession(t *testing.T) {
	srv, _, key := newTestServer(t)
	h := srv.Handler()
	body := `{"receiver":"iPhone","have":{"0":[0]}}`
	rec := post(t, h, "/api/progress", key, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("progress status = %d", rec.Code)
	}
	rec = get(t, h, "/api/session", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d", rec.Code)
	}
	var resp struct {
		Receiver struct {
			Receiver string `json:"receiver"`
		} `json:"receiver"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Receiver.Receiver != "iPhone" {
		t.Errorf("receiver = %+v", resp)
	}
}

func TestCancel(t *testing.T) {
	srv, _, key := newTestServer(t)
	h := srv.Handler()
	if rec := post(t, h, "/api/cancel", key, ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d", rec.Code)
	}
	// Cancelled session → 401, not 410 (id no longer exists).
	if rec := get(t, h, "/api/manifest", key); rec.Code != http.StatusUnauthorized {
		t.Errorf("post-cancel manifest = %d, want 401", rec.Code)
	}
}

// UI mounting: dashboard at /, receive page under /r/ (prefix
// stripped), API routes unaffected. Regression for the single-mount
// bug where the dashboard URL printed in the banner served 404.
func TestUIMounts(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "path="+r.URL.Path)
	})
	srv := NewServer(session.NewManager(), stub)
	h := srv.Handler()

	if rec := get(t, h, "/", ""); rec.Code != http.StatusOK ||
		rec.Body.String() != "path=/" {
		t.Errorf("GET / = %d %q, want 200 \"path=/\"", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/r/", ""); rec.Code != http.StatusOK ||
		rec.Body.String() != "path=" {
		t.Errorf("GET /r/ = %d %q, want 200 \"path=\" (empty; FileServerFS treats \"\" as \"/\" → index.html)",
			rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/r/assets/app.js", ""); rec.Code != http.StatusOK ||
		rec.Body.String() != "path=assets/app.js" {
		t.Errorf("GET /r/assets/app.js = %d %q", rec.Code, rec.Body.String())
	}
	// API routes must keep priority over the catch-all "/" UI mount.
	if rec := get(t, h, "/api/manifest", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/manifest (no key) = %d, want 401", rec.Code)
	}
	// Nil UI → explicit 404, not a panic.
	nilUI := NewServer(session.NewManager(), nil).Handler()
	if rec := get(t, nilUI, "/", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET / with nil UI = %d, want 404", rec.Code)
	}
}

func TestStartRequiresGroupKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	// No group key configured → 401 (fail closed).
	if rec := post(t, h, "/api/start", "anything", `{"paths":["/tmp"]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("start without group key = %d, want 401", rec.Code)
	}
	srv.GroupKey = "group-secret-group-secret-group!!"
	if rec := post(t, h, "/api/start", "wrong", `{"paths":["/tmp"]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("start with wrong group key = %d, want 401", rec.Code)
	}
	if rec := post(t, h, "/api/start", "group-secret-group-secret-group!!", `{"paths":[]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("start with no paths = %d, want 400", rec.Code)
	}
}
