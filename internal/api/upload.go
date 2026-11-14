package api

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/transfer"
	"chunkdrop/internal/upload"
)

// Phone→PC push endpoints (M1, docs/PROTOCOL.md §4 upload table).
// The dashboard creates the slot (local-only + group key) and shows a
// QR/link to /u/#k=<upload-key>; the phone page then drives start /
// chunk PUTs / finish with that key. Possession proves access — the
// same model as receive links — and every chunk is hash-verified
// server-side before it is kept.

// handleUploadNew mints an upload slot (id + key) for the dashboard QR.
// Local-only + group key: creating upload slots is dashboard control.
func (s *Server) handleUploadNew(w http.ResponseWriter, r *http.Request) {
	if s.Uploads == nil || s.UploadDest == "" {
		http.Error(w, `{"error":"uploads unavailable"}`, http.StatusNotFound)
		return
	}
	up, err := s.Uploads.NewPending(s.UploadDest)
	if err != nil {
		http.Error(w, `{"error":"upload create"}`, http.StatusInternalServerError)
		return
	}
	s.Activity.Add(activity.KindInfo, "phone upload link opened — waiting for the sender")
	writeJSON(w, http.StatusOK, map[string]any{"id": up.ID, "key": up.KeyEncoded()})
}

// handleUploadClose removes an upload link (dashboard cancel button).
// The route is group-key gated (authStart); the upload key arrives in
// the body — the header is already spent authenticating the dashboard.
func (s *Server) handleUploadClose(w http.ResponseWriter, r *http.Request) {
	if s.Uploads == nil {
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeBody(r, w, &body, 4<<10); err != nil {
		return
	}
	ok := s.Uploads.Close(body.Key)
	writeJSON(w, http.StatusOK, map[string]any{"closed": ok})
}

// authUpload wraps the phone-driven upload endpoints with the upload
// key: constant-time compare over the live uploads (mirrors session
// auth), generic 401 otherwise. The id in the URL path must belong to
// the authenticated upload — a wrong pairing gets the generic 404.
func (s *Server) authUpload(h func(*upload.Upload, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Uploads == nil {
			http.Error(w, generic404, http.StatusNotFound)
			return
		}
		up, ok := s.Uploads.ByKey(r.Header.Get(HeaderKey))
		if !ok {
			http.Error(w, ErrNoSession, http.StatusUnauthorized)
			return
		}
		if id := r.PathValue("uploadId"); id != "" && !up.MatchesID(id) {
			http.Error(w, generic404, http.StatusNotFound)
			return
		}
		h(up, w, r)
	}
}

// maxUploadBytes returns the configured single-upload cap
// (engine.toml max_upload_bytes; 100 GiB default).
func (s *Server) maxUploadBytes() int64 {
	if s.MaxUploadBytes > 0 {
		return s.MaxUploadBytes
	}
	return 100 << 30
}

// handleUploadStart accepts the browser-built manifest: the engine
// validates every claim (paths through PROTOCOL.md §8, chunk math,
// hash shape, caps) before any byte is accepted.
func (s *Server) handleUploadStart(up *upload.Upload, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Files []upload.FileSpec `json:"files"`
	}
	if err := decodeBody(r, w, &body, 16<<20); err != nil {
		return
	}
	if _, err := s.Uploads.Start(up.KeyEncoded(), body.Files, s.maxUploadBytes()); err != nil {
		http.Error(w, quoteErr(err), http.StatusBadRequest)
		return
	}
	s.Activity.Add(activity.KindDown, fmt.Sprintf(
		"phone upload started: %d file(s), %s", len(up.Files), humanBytes(up.BytesTotal())))
	// The id is echoed so the phone can address the chunk/finish
	// routes; presenting it is checked against the key server-side.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": up.ID, "files": len(up.Files), "bytes": up.BytesTotal(),
	})
}

// handleUploadChunk verifies and stages one chunk. The body is capped
// at exactly one chunk (4 MiB) — anything larger is rejected before it
// can be buffered.
func (s *Server) handleUploadChunk(up *upload.Upload, w http.ResponseWriter, r *http.Request) {
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
	if f < 0 || f >= len(up.Files) || c < 0 || c >= up.Files[f].ChunkCount {
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	chunk, err := io.ReadAll(http.MaxBytesReader(w, r.Body, transfer.DefaultChunkSize))
	if err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	switch uerr := up.StoreChunk(f, c, chunk, r.Header.Get(ChunkHashHeader)); uerr {
	case nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case upload.ErrBadChunk:
		http.Error(w, `{"error":"bad chunk"}`, http.StatusBadRequest)
	case upload.ErrHashMismatch:
		http.Error(w, `{"error":"hash mismatch"}`, http.StatusBadRequest)
	default:
		http.Error(w, `{"error":"staging failed"}`, http.StatusInternalServerError)
	}
}

// handleUploadFinish verifies completeness and moves every staged file
// into the destination (atomic renames), then refreshes the sync state
// so mesh peers pick the files up. Idempotent: a retry after a lost
// response gets the same 200.
func (s *Server) handleUploadFinish(up *upload.Upload, w http.ResponseWriter, r *http.Request) {
	landed, err := up.Finish()
	if err != nil {
		http.Error(w, quoteErr(err), http.StatusBadRequest)
		return
	}
	if landed != nil {
		share := s.Sync != nil && sameDir(s.UploadDest, s.Sync.Dir)
		for _, l := range landed {
			if share {
				s.Sync.RecordUpload(l.Rel, l.Abs)
			}
			s.Activity.Add(activity.KindDown, fmt.Sprintf(
				"received %s (%s) via phone upload", l.Rel, humanBytes(l.Size)))
		}
		if !share && s.Sync != nil {
			s.Activity.Add(activity.KindInfo,
				"uploads land outside the sync folder — they stay on this PC (receive_dir set)")
		}
		up.Cleanup() // staging dir is empty now; keep the tree tidy
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "files": len(up.Files), "bytes": up.BytesTotal(),
	})
}

// sameDir compares two absolute paths (both engine-internal values).
func sameDir(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
