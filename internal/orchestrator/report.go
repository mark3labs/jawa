package orchestrator

import (
	"fmt"
)

// projectPRURL derives the link from project configuration, never agent input.
func projectPRURL(provider, repo string, number int) (string, error) {
	endpoint, err := resolveProviderEndpoint(provider, repo, nil)
	if err != nil {
		return "", fmt.Errorf("provider: %w", err)
	}
	if number < 1 {
		return "", fmt.Errorf("provider: invalid PR number")
	}
	return fmt.Sprintf("%s/%s/%d", endpoint.WebBase, endpoint.PullKind, number), nil
}
