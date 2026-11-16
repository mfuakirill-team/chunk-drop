package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/diskspace"
	"chunkdrop/internal/logx"
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
		logx.Warn("upload", "upload/new refused: uploads unavailable (no dest/manager)")
		http.Error(w, `{"error":"uploads unavailable"}`, http.StatusNotFound)
		return
	}
	up, err := s.Uploads.NewPending(s.UploadDest)
	if err != nil {
		logx.Error("upload", "slot creation failed: %v", err)
		http.Error(w, `{"error":"upload create"}`, http.StatusInternalServerError)
		return
	}
	logx.Info("upload", "slot %s minted (key %s…), waiting for the phone", up.ID, up.KeyEncoded()[:6])
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
	if ok {
		logx.Info("upload", "link closed by the dashboard; staging wiped")
	} else {
		logx.Debug("upload", "close: no live upload for the presented key")
	}
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
			logx.Warn("upload", "401 on %s: wrong/unknown upload key (from %s)", r.URL.Path, r.RemoteAddr)
			http.Error(w, ErrNoSession, http.StatusUnauthorized)
			return
		}
		if id := r.PathValue("uploadId"); id != "" && !up.MatchesID(id) {
			logx.Warn("upload", "404 on %s: uploadId %q does not match the key's slot %s", r.URL.Path, id, up.ID)
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
		// A disk-space rejection is worth a dashboard line: the phone
		// sees the 400 body, but the PC side should know too (with
		// the numbers — the message carries need/free/reserved).
		var nse *diskspace.NoSpaceError
		if errors.As(err, &nse) {
			s.Activity.Add(activity.KindError, fmt.Sprintf(
				"phone upload rejected: %s (destination %s)", nse.Error(), up.Dest))
		}
		logx.Warn("upload", "start rejected for slot %s: %v", up.ID, err)
		http.Error(w, quoteErr(err), http.StatusBadRequest)
		return
	}
	s.Activity.Add(activity.KindDown, fmt.Sprintf(
		"phone upload started: %d file(s), %s", len(up.Files), humanBytes(up.BytesTotal())))
	logx.Info("upload", "slot %s started: %d file(s), %s — staging %s",
		up.ID, len(up.Files), humanBytes(up.BytesTotal()), up.Staging)
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
		logx.Warn("upload", "404 on chunk: bad file index %q", r.PathValue("f"))
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	c, err := strconv.Atoi(r.PathValue("c"))
	if err != nil {
		logx.Warn("upload", "404 on chunk: bad chunk index %q", r.PathValue("c"))
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	if f < 0 || f >= len(up.Files) || c < 0 || c >= up.Files[f].ChunkCount {
		logx.Warn("upload", "404 on chunk: file %d/%d, chunk %d/%d out of range (slot %s)",
			f, len(up.Files), c, up.Files[min(max(f, 0), len(up.Files)-1)].ChunkCount, up.ID)
		http.Error(w, generic404, http.StatusNotFound)
		return
	}
	chunk, err := io.ReadAll(http.MaxBytesReader(w, r.Body, transfer.DefaultChunkSize))
	if err != nil {
		logx.Warn("upload", "chunk f=%d c=%d: body read failed (size cap): %v", f, c, err)
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	switch uerr := up.StoreChunk(f, c, chunk, r.Header.Get(ChunkHashHeader)); uerr {
	case nil:
		logx.Debug("upload", "chunk f=%d c=%d stored (%s bytes)", f, c, humanBytes(int64(len(chunk))))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case upload.ErrBadChunk:
		logx.Warn("upload", "chunk f=%d c=%d rejected: bad chunk (size %d, want %d)",
			f, c, len(chunk), up.ExpectedChunkSize(f, c))
		http.Error(w, `{"error":"bad chunk"}`, http.StatusBadRequest)
	case upload.ErrHashMismatch:
		logx.Warn("upload", "chunk f=%d c=%d rejected: hash mismatch (slot %s)", f, c, up.ID)
		http.Error(w, `{"error":"hash mismatch"}`, http.StatusBadRequest)
	default:
		logx.Error("upload", "chunk f=%d c=%d staging failed: %v", f, c, uerr)
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
		logx.Warn("upload", "finish refused for slot %s: %v", up.ID, err)
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
			logx.Info("upload", "landed %s -> %s (%s)", l.Rel, l.Abs, humanBytes(l.Size))
		}
		if !share && s.Sync != nil {
			s.Activity.Add(activity.KindInfo,
				"uploads land outside the sync folder — they stay on this PC (receive_dir set)")
		}
		up.Cleanup() // staging dir is empty now; keep the tree tidy
		logx.Info("upload", "slot %s finished: %d file(s), %s total",
			up.ID, len(up.Files), humanBytes(up.BytesTotal()))
	} else {
		logx.Debug("upload", "finish: duplicate (slot %s already finished) — 200 again", up.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "files": len(up.Files), "bytes": up.BytesTotal(),
	})
}

// sameDir compares two absolute paths (both engine-internal values).
func sameDir(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
