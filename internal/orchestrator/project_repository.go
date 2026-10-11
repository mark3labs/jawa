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
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(host, " \t\r\n") {
		return "", errors.New("forgejo URL must be an http or https URL without credentials, query or fragment")
	}
	// Keep deployment subpaths while avoiding ambiguous escaped or traversal paths.
	for part := range strings.SplitSeq(strings.Trim(u.EscapedPath(), "/"), "/") {
		if part != "" && (!repositoryNamePart.MatchString(part) || part == "." || part == "..") {
			return "", errors.New("invalid Forgejo URL path")
		}
	}
	return strings.TrimRight(u.String(), "/") + "/" + strings.Join(parts, "/"), nil
}
