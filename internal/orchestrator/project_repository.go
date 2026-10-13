package orchestrator

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var repositoryNamePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// projectRepositoryURL expands the project form's owner/repo into a clone URL.
// Full repository URLs remain supported by the legacy repo form field.
func projectRepositoryURL(provider, name, host string) (string, error) {
	parts := strings.Split(strings.TrimSpace(name), "/")
	if len(parts) != 2 {
		return "", errors.New("repository name must be owner/repo")
	}
	for _, part := range parts {
		if !repositoryNamePart.MatchString(part) || part == "." || part == ".." {
			return "", errors.New("repository name must be owner/repo using letters, numbers, dots, underscores or hyphens")
		}
	}
	switch provider {
	case "github":
		host = "https://github.com"
	case "forgejo":
		host = strings.TrimSpace(host)
	default:
		return "", errors.New("invalid provider")
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(host, " \t\r\n") {
		return "", errors.New("Forgejo instance URL must be HTTPS at the host root (ports supported); HTTP and deployment subpaths are unsupported")
	}
	repo := strings.TrimRight(u.String(), "/") + "/" + strings.Join(parts, "/")
	if _, _, _, err := providerRepository(repo); err != nil {
		return "", err
	}
	return repo, nil
}
