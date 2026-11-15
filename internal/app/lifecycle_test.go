package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestDesktopInstanceProbe(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, application, dataDir string
		status                     int
		wantRunning, wantError     bool
	}{
		{"same instance", "LiteAgent", dir, 200, true, false},
		{"different folder", "LiteAgent", filepath.Join(dir, "other"), 200, false, true},
		{"another application", "Other", dir, 200, false, false},
		{"unhealthy", "LiteAgent", dir, 503, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/health" {
					t.Errorf("unexpected probe path %s", r.URL.Path)
				}
				sendJSON(w, tc.status, map[string]string{"application": tc.application, "data_dir": tc.dataDir})
			}))
			defer server.Close()
			running, err := existingDesktopInstance(context.Background(), server.URL, dir)
			if running != tc.wantRunning || (err != nil) != tc.wantError {
				t.Fatalf("running=%v err=%v", running, err)
			}
		})
	}
	t.Run("redirect is not followed", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed a redirect") }))
		defer target.Close()
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
		defer redirect.Close()
		if running, err := existingDesktopInstance(context.Background(), redirect.URL, dir); running || err != nil {
			t.Fatalf("running=%v err=%v", running, err)
		}
	})
}

func TestLocalServiceShutdown(t *testing.T) {
	dir := t.TempDir()
	service, err := startLocalService(context.Background(), "127.0.0.1:0", dir)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			service.Close()
		}
	}()
	base := "http://" + service.server.Addr
	if running, err := existingDesktopInstance(context.Background(), base, dir); !running || err != nil {
		t.Fatalf("health probe: running=%v err=%v", running, err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, tc := range []struct {
		header, origin string
		status         int
	}{
		{"", "", 403},
		{"1", "https://other.example", 403},
		{"1", "", 202},
	} {
		r, _ := http.NewRequest("POST", base+"/api/shutdown", nil)
		r.Header.Set("X-LiteAgent", tc.header)
		r.Header.Set("Origin", tc.origin)
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		}
		if tc.status == 202 {
			var result struct {
				Stopping bool `json:"stopping"`
			}
			if json.Unmarshal(body, &result) != nil || !result.Stopping {
				t.Fatalf("shutdown response was not delivered: %s", body)
			}
		} else if service.app.root.Err() != nil {
			t.Fatal("unauthorized request stopped the server")
		}
	}
	select {
	case <-service.app.root.Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the application")
	}
	service.Close()
	closed = true
	// Shutdown releases both the listening socket and the data directory lock.
	reopened, err := startLocalService(context.Background(), service.server.Addr, dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
