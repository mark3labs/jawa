package orchestrator

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestAssetVersions(t *testing.T) {
	a := testApp(t)
	for _, name := range []string{"app.js", "style.css"} {
		data, err := uiAssets.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		want := fmt.Sprintf("/assets/%s?v=%x", name, hash[:8])
		if got := assetURL(name); got != want {
			t.Fatalf("assetURL(%q) = %q, want %q", name, got, want)
		}
		for _, path := range []string{want, "/assets/" + name} {
			response := request(a, "GET", path, nil)
			status(t, response, 200)
			if response.Header().Get("Cache-Control") != "no-cache" {
				t.Errorf("%s must revalidate", path)
			}
			if response.Body.String() != string(data) {
				t.Errorf("%s returned incorrect asset", path)
			}
		}
	}
}
