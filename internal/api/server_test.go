package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"chunkdrop/internal/discovery"
	"chunkdrop/internal/session"
	"chunkdrop/internal/syncmesh"
	"chunkdrop/internal/transfer"
	"chunkdrop/internal/webdist"
	"chunkdrop/internal/webui"
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
	srv.GroupKey = "" // receiver endpoints only; start/pick/sync stay closed
	return srv, s, s.KeyEncoded()
}

func get(t *testing.T, h http.Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = testLocalAddr
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
	req.RemoteAddr = testLocalAddr
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

// TestEngineExtrasGated: pick / peers / sync routes must be closed to
// receivers (401) and open to a group member (#1, #6, #7).
func TestEngineExtrasGated(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.GroupKey = "group-secret-group-secret-group!!"
	syncDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(syncDir, "note.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := syncmesh.NewLocal(syncDir, "testbox", "eng1")
	if err != nil {
		t.Fatal(err)
	}
	srv.Sync = local
	srv.Discovery = discoveryServiceStub{}
	h := srv.Handler()

	// wrong/no key → 401 on every extra route
	for _, req := range []struct {
		method, path string
	}{
		{"POST", "/api/pick/files"},
		{"POST", "/api/pick/folder"},
		{"GET", "/api/peers"},
		{"GET", "/api/sync/state"},
		{"POST", "/api/sync/start"},
	} {
		if rec := reqNoKey(t, h, req.method, req.path); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without key = %d, want 401", req.method, req.path, rec.Code)
		}
		if rec := reqKey(t, h, req.method, req.path, "wrong-wrong-wrong-wrong-wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong key = %d, want 401", req.method, req.path, rec.Code)
		}
	}

	// group member: peers list works, sync state lists the file,
	// sync start serves a session manifest for the file.
	if rec := reqKey(t, h, "GET", "/api/peers", srv.GroupKey); rec.Code != http.StatusOK {
		t.Fatalf("peers = %d", rec.Code)
	}
	rec := reqKey(t, h, "GET", "/api/sync/state", srv.GroupKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync state = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"note.txt"`) {
		t.Errorf("sync state missing note.txt: %s", rec.Body.String())
	}
	rec = reqKeyBody(t, h, "POST", "/api/sync/start", srv.GroupKey, `{"rels":["note.txt"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync start = %d: %s", rec.Code, rec.Body.String())
	}
	var sr struct {
		SessionID string             `json:"sessionId"`
		Key       string             `json:"key"`
		Manifest  *transfer.Manifest `json:"manifest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Key == "" || sr.Manifest == nil || len(sr.Manifest.Files) != 1 || sr.Manifest.Files[0].Path != "note.txt" {
		t.Fatalf("sync start resp = %+v", sr)
	}
	// the served session is fetchable via the receiver wire
	rec = reqKey(t, h, "GET", "/api/manifest", sr.Key)
	if rec.Code != http.StatusOK {
		t.Errorf("manifest via sync session key = %d", rec.Code)
	}
	rec = reqKey(t, h, "GET", "/api/chunk/0/0", sr.Key)
	if rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Errorf("chunk via sync session = %d %q", rec.Code, rec.Body.String())
	}

	// picker routes: Picker == nil → 404 with a clear body
	if rec := reqKey(t, h, "POST", "/api/pick/files", srv.GroupKey); rec.Code != http.StatusNotFound {
		t.Errorf("pick files without picker = %d, want 404", rec.Code)
	}
}

func reqNoKey(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, h, method, path, "", "")
}

func reqKey(t *testing.T, h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, h, method, path, key, "")
}

func reqKeyBody(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, h, method, path, key, body)
}

func doReq(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = testLocalAddr
	if key != "" {
		req.Header.Set(HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type discoveryServiceStub struct{}

func (discoveryServiceStub) Self() discovery.Beacon {
	return discovery.Beacon{ID: "self", Name: "stub", Port: 8090}
}
func (discoveryServiceStub) Table() *discovery.Table {
	return &discovery.Table{}
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

// testLocalAddr marks test requests as coming from the engine machine:
// the dashboard is local-only, so ordinary tests must look local.
const testLocalAddr = "127.0.0.1:1234"

// The dashboard controls this PC (arbitrary paths, native dialogs,
// local file names), so other devices get the generic 404 on every
// dashboard/control route — even with the correct group key. The
// receiver surface and the engine-to-engine sync mesh stay reachable.
func TestDashboardLocalOnly(t *testing.T) {
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ui")
	})
	srv := NewServer(session.NewManager(), stub)
	srv.GroupKey = "group-secret-group-secret-group!!"
	srv.Discovery = discoveryServiceStub{}
	dir := t.TempDir()
	local, err := syncmesh.NewLocal(dir, "testbox", "eng1")
	if err != nil {
		t.Fatal(err)
	}
	srv.Sync = local
	h := srv.Handler()

	do := func(method, path, remote, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = remote
		if key != "" {
			req.Header.Set(HeaderKey, key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const remote = "192.0.2.50:5000"

	// Dashboard/control routes: invisible from another device.
	for _, c := range []struct{ method, path string }{
		{"GET", "/"},
		{"POST", "/api/start"},
		{"POST", "/api/pick/files"},
		{"POST", "/api/pick/folder"},
		{"GET", "/api/peers"},
		{"GET", "/api/activity"},
	} {
		if rec := do(c.method, c.path, remote, srv.GroupKey); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s from %s = %d, want 404", c.method, c.path, remote, rec.Code)
		}
	}

	// Loopback works.
	if rec := do("GET", "/api/peers", "127.0.0.1:5000", srv.GroupKey); rec.Code != http.StatusOK {
		t.Errorf("loopback peers = %d, want 200", rec.Code)
	}
	// So does the host's own LAN address (the local browser's source).
	oldAddrs := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("203.0.113.7"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	t.Cleanup(func() { interfaceAddrs = oldAddrs })
	if rec := do("GET", "/api/activity", "203.0.113.7:5000", srv.GroupKey); rec.Code != http.StatusOK {
		t.Errorf("own LAN address activity = %d, want 200", rec.Code)
	}

	// Receiving from other devices is unchanged: the /r/ page and the
	// sync mesh endpoints stay open (group/session keys still gate them).
	if rec := do("GET", "/r/", remote, ""); rec.Code != http.StatusOK {
		t.Errorf("remote /r/ = %d, want 200", rec.Code)
	}
	// The receive page references its bundle by absolute path, so the
	// static files must stay public — otherwise a phone gets the HTML
	// and 404s for every asset (blank page).
	for _, p := range []string{"/assets/index.js", "/icons/icon-192.png", "/manifest.webmanifest"} {
		if rec := do("GET", p, remote, ""); rec.Code != http.StatusOK {
			t.Errorf("remote %s = %d, want 200 (receive page assets must load)", p, rec.Code)
		}
	}
	if rec := do("GET", "/api/sync/state", remote, srv.GroupKey); rec.Code != http.StatusOK {
		t.Errorf("remote sync state = %d, want 200 (mesh must keep working)", rec.Code)
	}

	// Opt-in escape hatch: allow_remote_dashboard re-opens the dashboard
	// (still group-key gated) for headless engines like a NAS.
	srv.AllowRemoteDashboard = true
	if rec := do("GET", "/api/peers", remote, srv.GroupKey); rec.Code != http.StatusOK {
		t.Errorf("allow_remote_dashboard peers = %d, want 200", rec.Code)
	}
	if rec := do("GET", "/api/peers", remote, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("allow_remote_dashboard without key = %d, want 401", rec.Code)
	}
}

// Regression for the blank receive page: the receive page is public,
// but its bundle is referenced by absolute path (/assets/…), which used
// to fall into the local-only catch-all — a phone got the HTML and 404
// for every asset. Serve the real embedded app, hit /r/ from a remote
// address, and fetch every asset the page references.
func TestReceivePageAssetsLoadRemotely(t *testing.T) {
	if !webdist.Built() {
		t.Skip("embedded web bundle not built yet (run make web) — fresh clone")
	}
	srv := NewServer(session.NewManager(), webui.Handler())
	srv.GroupKey = "group-secret-group-secret-group!!"
	h := srv.Handler()

	fetch := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.0.2.50:5000" // another device on the LAN
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	page := fetch("/r/")
	if page.Code != http.StatusOK {
		t.Fatalf("remote GET /r/ = %d, want 200", page.Code)
	}
	refs := assetRefRE.FindAllStringSubmatch(page.Body.String(), -1)
	if len(refs) == 0 {
		t.Fatalf("receive page references no assets: %.200q", page.Body.String())
	}
	for _, m := range refs {
		if rec := fetch(m[1]); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("remote GET %s = %d (%d bytes), want 200 with content",
				m[1], rec.Code, rec.Body.Len())
		}
	}
}

var assetRefRE = regexp.MustCompile(`(?:src|href)="(/assets/[^"]+|/icons/[^"]+|/manifest\.webmanifest)"`)

// M1 per-receiver ZIP (PROTOCOL.md §3): a mobile receiver with a
// many-small selection is served one store-ZIP while desktop receivers
// keep the tree — same session, built lazily exactly once.
func TestManifestZipPerReceiver(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "photos")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	const n = 60 // > zipAutoFiles
	for i := range n {
		if err := os.WriteFile(
			filepath.Join(folder, fmt.Sprintf("p%03d.jpg", i)),
			[]byte(fmt.Sprintf("photo-%03d", i)), 0o644,
		); err != nil {
			t.Fatal(err)
		}
	}
	m := transfer.NewManifest("zip-test")
	if err := m.BuildFromPaths([]string{folder}, transfer.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	sm := session.NewManager()
	sess, err := sm.NewSession(m)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(sm, nil)
	srv.GroupKey = ""
	h := srv.Handler()

	getManifest := func(ua, query string) *transfer.Manifest {
		req := httptest.NewRequest(http.MethodGet, "/api/manifest"+query, nil)
		req.Header.Set(HeaderKey, sess.KeyEncoded())
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("manifest = %d", rec.Code)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		var got transfer.Manifest
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("manifest json: %v", err)
		}
		return &got
	}

	const desktop = "Mozilla/5.0 (X11; Linux x86_64) Chrome/120"
	const phone = "Mozilla/5.0 (iPhone; CPU iPhone OS 17) Mobile Safari"

	// Desktop receiver: tree.
	if dm := getManifest(desktop, ""); dm.PackedAsZip || len(dm.Files) != n {
		t.Errorf("desktop manifest: packed=%v files=%d", dm.PackedAsZip, len(dm.Files))
	}
	// Mobile receiver: packed variant, single ZIP file.
	pm := getManifest(phone, "")
	if !pm.PackedAsZip || len(pm.Files) != 1 {
		t.Fatalf("mobile manifest: packed=%v files=%d", pm.PackedAsZip, len(pm.Files))
	}
	if pm.Files[0].ChunkCount == 0 {
		t.Error("packed variant has no chunks")
	}
	// The packed session's chunk endpoint serves the archive bytes.
	req := httptest.NewRequest(http.MethodGet, "/api/chunk/0/0", nil)
	req.Header.Set(HeaderKey, sess.KeyEncoded())
	req.Header.Set("User-Agent", phone)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Errorf("packed chunk = %d %q…, want ZIP bytes", rec.Code, rec.Body.String()[:min(2, rec.Body.Len())])
	}
	// Built once: a second mobile request must reuse the cached variant.
	if pm2 := getManifest(phone, ""); pm2.Files[0].Size != pm.Files[0].Size {
		t.Error("second mobile request rebuilt the ZIP variant")
	}
	// Opt-out (?zip=0) and force (?zip=1) work regardless of UA.
	if om := getManifest(phone, "?zip=0"); om.PackedAsZip || len(om.Files) != n {
		t.Errorf("?zip=0 on mobile: packed=%v files=%d", om.PackedAsZip, len(om.Files))
	}
	if fm := getManifest(desktop, "?zip=1"); !fm.PackedAsZip || len(fm.Files) != 1 {
		t.Errorf("?zip=1 on desktop: packed=%v files=%d", fm.PackedAsZip, len(fm.Files))
	}
	// The desktop manifest stays the tree after the variant exists.
	if dm := getManifest(desktop, ""); dm.PackedAsZip || len(dm.Files) != n {
		t.Errorf("desktop manifest after mobile request: packed=%v files=%d", dm.PackedAsZip, len(dm.Files))
	}
}

// The session-level cache: PackedZip builds at most once, even when the
// first build fails (consistent error, no repeated disk hammering).
func TestPackedZipBuiltOnce(t *testing.T) {
	m := transfer.NewManifest("pz")
	sm := session.NewManager()
	sess, err := sm.NewSession(m)
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	build := func() (*transfer.Manifest, error) {
		builds++
		return transfer.NewManifest("pz-packed"), nil
	}
	a, _ := sess.PackedZip(build)
	b, _ := sess.PackedZip(build)
	if builds != 1 || a != b {
		t.Errorf("builds=%d, same pointer=%v", builds, a == b)
	}
	// A failed build is cached as an error for the session lifetime.
	sess2, _ := sm.NewSession(transfer.NewManifest("pz2"))
	wantErr := fmt.Errorf("disk full")
	sess2.PackedZip(func() (*transfer.Manifest, error) { return nil, wantErr })
	if _, err := sess2.PackedZip(func() (*transfer.Manifest, error) {
		builds++
		return transfer.NewManifest("x"), nil
	}); err != wantErr {
		t.Errorf("second call err = %v, want the cached %v", err, wantErr)
	}
}
