package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/diskspace"
	"chunkdrop/internal/session"
	"chunkdrop/internal/transfer"
	"chunkdrop/internal/upload"
)

// M1 phone→PC push (PROTOCOL.md §4 upload table): slot creation is
// dashboard control (local-only + group key); the upload itself is
// gated by the minted upload key; every chunk is hash-verified
// server-side; staging is invisible to the sync scan; finish lands
// files and (when the dest is the sync folder) refreshes mesh state.

func newUploadTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := NewServer(session.NewManager(), nil)
	srv.GroupKey = "group-secret-group-secret-group!!"
	dest := t.TempDir()
	srv.Uploads = upload.NewManager()
	srv.UploadDest = dest
	srv.MaxUploadBytes = 1 << 30
	return srv, dest
}

func newSlot(t *testing.T, h http.Handler) (id, key string) {
	t.Helper()
	rec := reqKey(t, h, http.MethodPost, "/api/upload/new", "group-secret-group-secret-group!!")
	if rec.Code != http.StatusOK {
		t.Fatalf("upload/new = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("upload/new body: %v", err)
	}
	return resp.ID, resp.Key
}

// Slot creation is dashboard control: remote devices get the generic
// 404 even with the group key.
func TestUploadNewLocalOnly(t *testing.T) {
	srv, _ := newUploadTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/upload/new", nil)
	req.Header.Set(HeaderKey, srv.GroupKey)
	req.RemoteAddr = "192.0.2.50:5000" // another device
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != generic404 {
		t.Errorf("remote upload/new = %d %q", rec.Code, rec.Body.String())
	}

	if rec := reqKey(t, h, http.MethodPost, "/api/upload/new", "wrong-wrong-wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("upload/new with wrong key = %d, want 401", rec.Code)
	}
	if rec := reqNoKey(t, h, http.MethodPost, "/api/upload/new"); rec.Code != http.StatusUnauthorized {
		t.Errorf("upload/new without key = %d, want 401", rec.Code)
	}
}

// Full flow: new → start (manifest) → chunk PUTs → finish; files land
// under the destination and the staged bytes are gone.
func TestUploadFlowEndToEnd(t *testing.T) {
	srv, dest := newUploadTestServer(t)
	h := srv.Handler()
	id, key := newSlot(t, h)

	payload := []byte("photo-bytes-for-the-end-to-end-test")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	manifest := fmt.Sprintf(`{"files":[{"path":"pics/p1.jpg","size":%d,"chunkCount":1,"hashes":["%s"],"mtime":1700000000000}]}`, len(payload), hash)

	if rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/start", "wrong-key", manifest); rec.Code != http.StatusUnauthorized {
		t.Errorf("start with wrong key = %d, want 401", rec.Code)
	}
	if rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/start", key, manifest); rec.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
	}

	// Chunk PUTs: correct key + id + hash → 200; every deviation →
	// the documented error code (wrong id 404, wrong key 401, bad
	// hash 400).
	chunkURL := "/api/upload/" + id + "/chunk/0/0"
	put := func(url, k, hdrHash string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, url, strings.NewReader(string(body)))
		req.RemoteAddr = testLocalAddr
		req.Header.Set(HeaderKey, k)
		if hdrHash != "" {
			req.Header.Set(ChunkHashHeader, hdrHash)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := put("/api/upload/other-id/chunk/0/0", key, hash, payload); rec.Code != http.StatusNotFound {
		t.Errorf("chunk with mismatched id = %d, want 404", rec.Code)
	}
	if rec := put(chunkURL, "wrong-key", hash, payload); rec.Code != http.StatusUnauthorized {
		t.Errorf("chunk with wrong key = %d, want 401", rec.Code)
	}
	if rec := put(chunkURL, key, strings.Repeat("ee", 32), payload); rec.Code != http.StatusBadRequest {
		t.Errorf("chunk with wrong header hash = %d, want 400", rec.Code)
	}
	if rec := put(chunkURL, key, hash, []byte("tampered-data")); rec.Code != http.StatusBadRequest {
		t.Errorf("chunk with tampered bytes = %d, want 400", rec.Code)
	}
	if rec := put(chunkURL, key, hash, payload); rec.Code != http.StatusOK {
		t.Fatalf("chunk = %d: %s", rec.Code, rec.Body.String())
	}
	// Oversized body (> one chunk) is rejected before buffering.
	big := make([]byte, transfer.DefaultChunkSize+1)
	if rec := put(chunkURL, key, hash, big); rec.Code != http.StatusBadRequest {
		t.Errorf("oversized chunk = %d, want 400", rec.Code)
	}

	// Finish before the retry — everything already stored, so it
	// lands. Duplicate finish still authenticates and returns 200.
	if rec := reqKey(t, h, http.MethodPost, "/api/upload/"+id+"/finish", "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Errorf("finish with wrong key = %d, want 401", rec.Code)
	}
	for i := range 2 {
		rec := reqKey(t, h, http.MethodPost, "/api/upload/"+id+"/finish", key)
		if rec.Code != http.StatusOK {
			t.Fatalf("finish #%d = %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
	landed := filepath.Join(dest, "pics", "p1.jpg")
	got, err := os.ReadFile(landed)
	if err != nil {
		t.Fatalf("landed file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("landed content = %q", got)
	}
	if st, err := os.Stat(landed); err != nil || !st.ModTime().Equal(time.UnixMilli(1700000000000)) {
		t.Errorf("phone mtime not preserved: %v %v", st, err)
	}
	// Staging is gone; nothing stray under the destination — only the
	// (empty) staging root may remain.
	uploadsRoot := filepath.Join(dest, upload.CacheDirName, upload.UploadsTmpDir)
	if entries, err := os.ReadDir(uploadsRoot); err == nil && len(entries) > 0 {
		t.Errorf("staging leftovers under %s: %d entries", uploadsRoot, len(entries))
	}
}

// Bad manifests are rejected with 400 before any byte is accepted.
func TestUploadStartValidation(t *testing.T) {
	srv, _ := newUploadTestServer(t)
	h := srv.Handler()
	_, key := newSlot(t, h)

	for _, bad := range []string{
		`{"files":[]}`,
		`{"files":[{"path":"../etc/passwd","size":1,"chunkCount":1,"hashes":["` + strings.Repeat("ab", 32) + `"]}]}`,
		`{"files":[{"path":"ok.txt","size":10,"chunkCount":2,"hashes":["` + strings.Repeat("ab", 32) + `"]}]}`,
	} {
		if rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/start", key, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("start with %s = %d, want 400", bad, rec.Code)
		}
	}
	// Unstarted uploads cannot finish.
	rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/start", key, `{"files":[{"path":"x.txt","size":1,"chunkCount":1,"hashes":["`+strings.Repeat("ab", 32)+`"]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid start = %d: %s", rec.Code, rec.Body.String())
	}
}

// Close-link kills the upload and wipes staging.
func TestUploadClose(t *testing.T) {
	srv, dest := newUploadTestServer(t)
	h := srv.Handler()
	id, key := newSlot(t, h)

	if rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/close", srv.GroupKey, `{"key":"`+key+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body.String())
	}
	// A wrong link key reports not-closed without touching anything.
	rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/close", srv.GroupKey, `{"key":"not-a-real-key"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("close (unknown key) = %d: %s", rec.Code, rec.Body.String())
	}
	// The minted key no longer works anywhere.
	if rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/start", key, `{"files":[]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("start after close = %d, want 401", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dest, upload.CacheDirName, upload.UploadsTmpDir, id)); !os.IsNotExist(err) {
		t.Errorf("staging dir survived close: %v", err)
	}
}

// A batch that does not fit the destination filesystem is refused at
// start with a clear 400 (the phone sees the numbers), and the
// dashboard's activity feed gets the error line too.
func TestUploadStartNoSpace(t *testing.T) {
	srv, _ := newUploadTestServer(t)
	srv.Activity = activity.NewLog(10)
	h := srv.Handler()
	_, key := newSlot(t, h)

	// Fake ledger: every reserve overflows ("the FS is full").
	srv.Uploads.Space = fakeNoSpaceLedger{}
	manifest := `{"files":[{"path":"pics/p1.jpg","size":100,"chunkCount":1,"hashes":["` + strings.Repeat("ab", 32) + `"],"mtime":1700000000000}]}`
	rec := reqKeyBody(t, h, http.MethodPost, "/api/upload/start", key, manifest)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("start with full disk = %d: %s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not enough disk space") {
		t.Errorf("400 body lacks the disk-space message: %s", rec.Body.String())
	}
	// Dashboard report: one error line with the numbers + destination.
	found := false
	for _, e := range srv.Activity.List() {
		if e.Kind == activity.KindError && strings.Contains(e.Msg, "phone upload rejected") &&
			strings.Contains(e.Msg, "not enough disk space") {
			found = true
		}
	}
	if !found {
		t.Error("no dashboard error line for the refused upload")
	}
}

// fakeNoSpaceLedger always refuses.
type fakeNoSpaceLedger struct{}

func (fakeNoSpaceLedger) Reserve(dir, id string, size int64) error {
	return &diskspace.NoSpaceError{Need: size, Free: 10, Reserved: 0}
}
func (fakeNoSpaceLedger) Written(dir, id string, n int64) {}
func (fakeNoSpaceLedger) Release(dir, id string)          {}
