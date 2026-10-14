package orchestrator

import (
	"crypto/sha256"
	"fmt"
)

// Compute versions from the assets embedded in this binary, not build metadata.
var versionedAssets = func() map[string]string {
	urls := make(map[string]string)
	for _, name := range []string{"app.js", "style.css"} {
		path := "assets/" + name
		data, err := uiAssets.ReadFile(path)
		if err != nil {
			panic(err)
		}
		hash := sha256.Sum256(data)
		urls[name] = fmt.Sprintf("/%s?v=%x", path, hash[:8])
	}
	return urls
}()

func assetURL(name string) string {
	return versionedAssets[name]
}
