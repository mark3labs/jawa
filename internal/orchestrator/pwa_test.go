package orchestrator

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicPWAAssets(t *testing.T) {
	a := &app{} // Public assets must not depend on a database or login.
	for _, path := range []string{"/sw.js", "/assets/manifest.webmanifest", "/assets/offline.html", "/assets/icon-180.png", "/assets/icon-192.png", "/assets/icon-512.png", "/assets/icon-maskable-512.png"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if path == "/sw.js" {
				if !strings.Contains(w.Header().Get("Content-Type"), "javascript") || w.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("worker headers: %v", w.Header())
				}
				if strings.Contains(w.Body.String(), "__ASSET_VERSION__") {
					t.Fatal("unbuilt worker")
				}
			}
			if strings.HasSuffix(path, ".png") {
				cfg, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				size := 512
				if strings.Contains(path, "180") {
					size = 180
				}
				if strings.Contains(path, "192") {
					size = 192
				}
				if cfg.Width != size || cfg.Height != size {
					t.Fatalf("icon dimensions: %v", cfg)
				}
			}
			if strings.HasSuffix(path, ".webmanifest") {
				if w.Header().Get("Content-Type") != "application/manifest+json" {
					t.Fatal(w.Header())
				}
				var m struct {
					StartURL string `json:"start_url"`
					Scope    string
					Display  string
					Icons    []struct {
						Src     string
						Purpose string
					}
				}
				if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
					t.Fatal(err)
				}
				if m.StartURL != "/board" || m.Scope != "/" || m.Display != "standalone" || len(m.Icons) != 3 {
					t.Fatalf("manifest: %+v", m)
				}
				for _, icon := range m.Icons {
					if _, err := uiAssets.ReadFile(strings.TrimPrefix(icon.Src, "/")); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestPWAMetadataOnAuthPages(t *testing.T) {
	w := httptest.NewRecorder()
	authPageWithRequest(w, httptest.NewRequest(http.MethodGet, "/login", nil), "login", "")
	for _, want := range []string{`rel="manifest"`, `rel="apple-touch-icon"`, `name="theme-color"`, `viewport-fit=cover`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
}
