package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"chunkdrop/internal/device"
	"chunkdrop/internal/hubclient"
)

// /api/hub (M2 step ①): dashboard view of the hub connection.
func TestHubEndpoint(t *testing.T) {
	const groupKey = "group-secret-group-secret-group!!"

	t.Run("no hub configured", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.GroupKey = "group-secret-group-secret-group!!"
		h := srv.Handler()
		rec := reqKey(t, h, http.MethodGet, "/api/hub", groupKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Enabled {
			t.Error("enabled=true with no hub configured")
		}
	})

	t.Run("hub configured", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.GroupKey = "group-secret-group-secret-group!!"
		id, err := device.LoadOrCreate(t.TempDir(), "test-pc")
		if err != nil {
			t.Fatal(err)
		}
		// Not started: the endpoint must still answer (disconnected).
		srv.Hub = hubclient.New("ws://hub.example/hub/ws", groupKey, id, nil)
		h := srv.Handler()
		rec := reqKey(t, h, http.MethodGet, "/api/hub", groupKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Enabled   bool     `json:"enabled"`
			URL       string   `json:"url"`
			Connected bool     `json:"connected"`
			Devices   []string `json:"devices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Enabled || out.URL != "ws://hub.example/hub/ws" || out.Connected {
			t.Errorf("body = %s", rec.Body.String())
		}
	})

	t.Run("group key required", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.GroupKey = "group-secret-group-secret-group!!"
		srv.Hub = nil
		h := srv.Handler()
		rec := reqKey(t, h, http.MethodGet, "/api/hub", "wrong-key-wrong-key-wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong key = %d, want 401", rec.Code)
		}
	})
}
