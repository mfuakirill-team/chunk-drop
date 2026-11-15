package api

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"net/http"

	"chunkdrop/internal/logx"
	"chunkdrop/internal/session"
)

// Auth middleware — docs/PROTOCOL.md §1: every authenticated request
// carries X-ChunkDrop-Key, compared in constant time; wrong key → 401
// with a generic body, expired session → 410. Unknown ids get the same
// uniform 401 so nothing leaks about which sessions exist.

// auth wraps a receiver endpoint with session-key auth.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.authAny(w, r)
		if !ok {
			return
		}
		h(w, r.WithContext(withSession(r.Context(), sess)))
	}
}

// authAny authenticates a receiver request. Receivers present only the
// session key (read from the QR fragment by the page), never the id —
// so the engine scans live sessions with a constant-time compare per
// candidate. Fine at private-group scale (a handful of live transfers).
func (s *Server) authAny(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	key := r.Header.Get(HeaderKey)
	if key == "" {
		logx.Warn("auth", "401 on %s: no key header (from %s)", r.URL.Path, r.RemoteAddr)
		http.Error(w, ErrNoSession, http.StatusUnauthorized)
		return nil, false
	}
	// The dashboard (same origin) may send the id as well; try it first.
	if id := r.Header.Get("X-ChunkDrop-Session"); id != "" {
		sess, err := s.Sessions.CheckAuth(id, key)
		if err == nil {
			return sess, true
		}
		if err == session.ErrExpired {
			logx.Warn("auth", "410 on %s: session %s expired", r.URL.Path, id)
			http.Error(w, ErrGone, http.StatusGone)
			return nil, false
		}
		// fall through: stale/wrong id but the key may still match a session
	}
	if sess, ok := s.Sessions.ByKey(key); ok {
		return sess, true
	}
	logx.Warn("auth", "401 on %s: wrong/unknown session key (from %s)", r.URL.Path, r.RemoteAddr)
	http.Error(w, ErrNoSession, http.StatusUnauthorized)
	return nil, false
}

// authStart gates session creation (sender dashboard) behind the group
// key from engine config — LAN strangers must not be able to point a
// session at arbitrary filesystem paths (SECURITY.md §6).
func (s *Server) authStart(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.GroupKey == "" {
			logx.Warn("auth", "401 on %s: no group key configured", r.URL.Path)
			http.Error(w, ErrNoSession, http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderKey)), []byte(s.GroupKey)) != 1 {
			logx.Warn("auth", "401 on %s: wrong group key (from %s)", r.URL.Path, r.RemoteAddr)
			http.Error(w, ErrNoSession, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

type ctxKey struct{}

// withSession puts the authenticated session into the request context.
func withSession(ctx context.Context, s *session.Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// fromSession pulls the session back out (nil-safe).
func fromSession(r *http.Request) *session.Session {
	s, _ := r.Context().Value(ctxKey{}).(*session.Session)
	return s
}

// newGzipWriter wraps w in a gzip writer (manifest endpoint).
func newGzipWriter(w http.ResponseWriter) *gzip.Writer {
	return gzip.NewWriter(w)
}
