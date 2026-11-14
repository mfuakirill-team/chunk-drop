package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/session"
	"chunkdrop/internal/transfer"
)

// Receiver-side endpoints (docs/PROTOCOL.md §4).

// handleManifest serves the manifest JSON with gzip content-encoding.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	sess := fromSession(r)
	b, err := sess.Manifest.JSON()
	if err != nil {
		http.Error(w, `{"error":"manifest encode"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	gz := newGzipWriter(w)
	defer gz.Close()
	_, _ = gz.Write(b)
}

// handleChunk streams chunk c of file f. Unknown indices → 404 with a
// generic body — no path hints (SECURITY.md §2).
func (s *Server) handleChunk(w http.ResponseWriter, r *http.Request) {
	sess := fromSession(r)
	m := sess.Manifest

	f, err := strconv.Atoi(r.PathValue("f"))
	if err != nil {
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	c, err := strconv.Atoi(r.PathValue("c"))
	if err != nil {
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	file, ok := m.Find(f)
	if !ok || c < 0 || c >= file.ChunkCount {
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	w.Header().Set(ChunkHashHeader, file.Hashes[c])
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = m.ChunkStreamer(f, c, w) // headers are out; abort quietly on I/O error
}

// handleProgress stores the receiver's chunk bitmap POST (≤ 8 MiB body).
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	sess := fromSession(r)
	var p session.Progress
	if err := decodeBody(r, w, &p, 8<<20); err != nil {
		return
	}
	sess.SetProgress(p)
	s.Activity.Add(activity.KindUp, fmt.Sprintf("%s receiving %q — %s", p.Receiver, sess.Manifest.Files[0].Path, humanCount(p.Have)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCancel closes the session (receiver-initiated, idempotent).
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sess := fromSession(r)
	s.Activity.Add(activity.KindInfo, fmt.Sprintf("session %s cancelled by receiver", sess.ID))
	_ = s.Sessions.Cancel(sess.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSession returns session metadata + latest receiver progress —
// the sender dashboard polls this (same origin, session-key authed).
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess := fromSession(r)
	resp := map[string]any{"session": sess.Snapshot()}
	if sess.HasProgress() {
		resp["receiver"] = sess.Progress
		resp["progressAgoMs"] = int(sess.ProgressAge() / time.Millisecond)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStart creates a session from picked paths (sender dashboard,
// same origin, group-key gated — see authStart).
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths   []string `json:"paths"`
		Zip     bool     `json:"zip"`
		ZipPath string   `json:"zipPath"`
	}
	if err := decodeBody(r, w, &body, 4<<20); err != nil {
		return
	}
	if len(body.Paths) == 0 {
		http.Error(w, `{"error":"no paths"}`, http.StatusBadRequest)
		return
	}
	zipPath := ""
	if body.Zip {
		zipPath = body.ZipPath
		if zipPath == "" {
			zipPath = tempZipPath()
		}
	}
	m := transfer.NewManifest(session.NewID())
	if err := m.BuildFromPaths(body.Paths, transfer.BuildOptions{ZipPath: zipPath}); err != nil {
		http.Error(w, quoteErr(err), http.StatusBadRequest)
		return
	}
	sess, err := s.Sessions.NewSession(m)
	if err != nil {
		http.Error(w, `{"error":"session create"}`, http.StatusInternalServerError)
		return
	}
	total := m.TotalSize()
	s.Activity.Add(activity.KindUp, fmt.Sprintf("session started: %d file(s), %s — waiting for receiver", len(m.Files), humanBytes(total)))
	writeJSON(w, http.StatusOK, startResponse{
		SessionID: sess.ID,
		Key:       session.KeyEncoding(sess.Key),
		Manifest:  m,
	})
}

// humanBytes renders a size for activity lines (1.5 MB style).
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k*k*k:
		return fmt.Sprintf("%.1f TB", float64(n)/(k*k*k*k*k))
	case n >= k*k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k*k))
	case n >= k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

type startResponse struct {
	SessionID string             `json:"sessionId"`
	Key       string             `json:"key"`
	Manifest  *transfer.Manifest `json:"manifest"`
}

func decodeBody(r *http.Request, w http.ResponseWriter, v any, max int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	if err := dec.Decode(v); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"encode"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func quoteErr(err error) string {
	q, _ := json.Marshal(err.Error())
	return fmt.Sprintf(`{"error":%s}`, q)
}

func tempZipPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("chunkdrop-%d.zip", time.Now().UnixNano()))
}

// humanCount renders "N files, M chunks" from a progress bitmap.
func humanCount(have map[string]any) string {
	nf := len(have)
	nc := 0
	for _, v := range have {
		if l, ok := v.([]any); ok {
			nc += len(l)
		}
	}
	return fmt.Sprintf("%d files, %d chunks", nf, nc)
}
