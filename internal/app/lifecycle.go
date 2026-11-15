package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

func (a *App) shutdownServer(w http.ResponseWriter, r *http.Request) {
	if a.shutdown == nil {
		fail(w, 503, errors.New("shutdown is unavailable in this instance"))
		return
	}
	sendJSON(w, 202, map[string]bool{"stopping": true})
	_ = http.NewResponseController(w).Flush()
	a.shutdown()
}

// Probe only our loopback service; never follow a redirect from an occupied port.
func existingDesktopInstance(ctx context.Context, base, dir string) (bool, error) {
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, "GET", base+"/api/health", nil)
	if err != nil {
		return false, err
	}
	response, err := client.Do(r)
	if err != nil {
		return false, nil
	}
	defer response.Body.Close()
	var health struct {
		Application string `json:"application"`
		DataDir     string `json:"data_dir"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&health) != nil || health.Application != "LiteAgent" {
		return false, nil
	}
	if !strings.EqualFold(filepath.Clean(health.DataDir), filepath.Clean(dir)) {
		return false, errors.New("LiteAgent is already running on this port with another data folder. Quit that instance or choose another --port")
	}
	return true, nil
}
