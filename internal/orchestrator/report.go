package orchestrator

import (
	"fmt"
)

// projectPRURL derives the link from project configuration, never agent input.
func projectPRURL(provider, repo string, number int) (string, error) {
	host, owner, name, err := providerRepository(repo)
	if err != nil {
		return "", err
	}
	kind := "pull"
	switch provider {
	case "github":
		if host != "github.com" {
			return "", fmt.Errorf("provider: only github.com is supported")
		}
	case "forgejo":
		kind = "pulls"
	default:
		return "", fmt.Errorf("provider: unsupported provider")
	}
	if number < 1 {
		return "", fmt.Errorf("provider: invalid PR number")
	}
	return fmt.Sprintf("https://%s/%s/%s/%s/%d", host, owner, name, kind, number), nil
}
