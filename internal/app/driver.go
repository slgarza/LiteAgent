package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var browserVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)

func findBrowser(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", err
		}
		return configured, nil
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	paths := []string{}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root != "" {
				paths = append(paths, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	}
	if runtime.GOOS == "darwin" {
		paths = append(paths, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("Chrome was not found; install it or set its executable path in Settings")
}

func binaryVersion(ctx context.Context, path string, browser bool) string {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" && browser {
		// Paths are passed via an environment variable, never interpolated into PowerShell.
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-Item -LiteralPath $env:LITEAGENT_BROWSER_BINARY).VersionInfo.ProductVersion")
		cmd.Env = append(os.Environ(), "LITEAGENT_BROWSER_BINARY="+path)
	} else {
		cmd = exec.CommandContext(ctx, path, "--version")
	}
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return browserVersionPattern.FindString(string(b))
}

func ensureDriver(ctx context.Context, c Config, dir string) (string, string, error) {
	browser, err := findBrowser(c.BrowserPath)
	if err != nil {
		return "", "", err
	}
	if c.DriverPath != "" {
		return c.DriverPath, browser, nil
	}
	version := binaryVersion(ctx, browser, true)
	if version == "" {
		return "", "", errors.New("could not detect the Chrome version; configure ChromeDriver manually")
	}
	name := "chromedriver"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cached := filepath.Join(dir, "drivers", version, name)
	if _, err = os.Stat(cached); err == nil {
		return cached, browser, nil
	}
	if p, e := exec.LookPath(name); e == nil {
		v := binaryVersion(ctx, p, false)
		if strings.Join(strings.Split(v, ".")[:min(3, len(strings.Split(v, ".")))], ".") == strings.Join(strings.Split(version, ".")[:3], ".") {
			return p, browser, nil
		}
	}
	platform := ""
	switch runtime.GOOS {
	case "linux":
		if runtime.GOARCH == "amd64" {
			platform = "linux64"
		}
	case "windows":
		platform = "win64"
		if runtime.GOARCH == "386" {
			platform = "win32"
		}
	case "darwin":
		platform = "mac-x64"
		if runtime.GOARCH == "arm64" {
			platform = "mac-arm64"
		}
	}
	if platform == "" {
		return "", "", errors.New("automatic ChromeDriver download is unavailable on this platform; set its path manually")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var catalog struct {
		Builds map[string]struct {
			Downloads map[string][]struct {
				Platform string `json:"platform"`
				URL      string `json:"url"`
			} `json:"downloads"`
		} `json:"builds"`
	}
	metadata, err := download(ctx, "https://googlechromelabs.github.io/chrome-for-testing/latest-patch-versions-per-build-with-downloads.json", 16<<20)
	if err != nil {
		return "", "", fmt.Errorf("could not resolve ChromeDriver: %w", err)
	}
	if err = json.Unmarshal(metadata, &catalog); err != nil {
		return "", "", err
	}
	build := strings.Join(strings.Split(version, ".")[:3], ".")
	downloadURL := ""
	for _, d := range catalog.Builds[build].Downloads["chromedriver"] {
		if d.Platform == platform {
			downloadURL = d.URL
			break
		}
	}
	u, _ := url.Parse(downloadURL)
	if u == nil || u.Scheme != "https" || u.Host != "storage.googleapis.com" || !strings.HasPrefix(u.Path, "/chrome-for-testing-public/") {
		return "", "", fmt.Errorf("no compatible ChromeDriver was found for Chrome %s; update Chrome or specify a driver", version)
	}
	archive, err := download(ctx, downloadURL, 40<<20)
	if err != nil {
		return "", "", err
	}
	if err = os.MkdirAll(filepath.Dir(cached), 0700); err != nil {
		return "", "", err
	}
	zipFile, err := os.CreateTemp(filepath.Dir(cached), "driver-*.zip")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(zipFile.Name())
	_, err = zipFile.Write(archive)
	closeErr := zipFile.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", "", err
	}
	z, err := zip.OpenReader(zipFile.Name())
	if err != nil {
		return "", "", err
	}
	defer z.Close()
	for _, f := range z.File {
		if filepath.Base(f.Name) != name {
			continue
		}
		if f.UncompressedSize64 > 100<<20 {
			return "", "", errors.New("ChromeDriver exceeds the size limit")
		}
		r, err := f.Open()
		if err != nil {
			return "", "", err
		}
		defer r.Close()
		out, err := os.CreateTemp(filepath.Dir(cached), "driver-*")
		if err != nil {
			return "", "", err
		}
		defer os.Remove(out.Name())
		_, err = io.Copy(out, io.LimitReader(r, 100<<20))
		if err == nil {
			err = out.Chmod(0700)
		}
		ce := out.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return "", "", err
		}
		if err = os.Rename(out.Name(), cached); err != nil {
			return "", "", err
		}
		return cached, browser, nil
	}
	return "", "", errors.New("the downloaded archive does not contain ChromeDriver")
}

func download(ctx context.Context, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("download exceeds the size limit")
	}
	return b, err
}
