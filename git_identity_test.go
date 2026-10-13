package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/sandbox"
)

var gitIdentityKeys = []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"}

func TestCodingEnvGitIdentity(t *testing.T) {
	for _, tc := range []struct {
		label, name, email string
		unset              bool
	}{
		{label: "unset", unset: true},
		{label: "empty"},
		{label: "both", name: "Agent", email: "agent@example.com"},
		{label: "name only", name: "Agent"},
		{label: "email only", email: "agent@example.com"},
		{label: "spaces preserved", name: "  Jawa Agent  ", email: "agent@example.com"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Setenv("JAWA_GIT_USER_NAME", tc.name)
			t.Setenv("JAWA_GIT_USER_EMAIL", tc.email)
			if tc.unset {
				for _, key := range []string{"JAWA_GIT_USER_NAME", "JAWA_GIT_USER_EMAIL"} {
					if err := os.Unsetenv(key); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, key := range gitIdentityKeys {
				t.Setenv(key, "inherited-"+key)
			}
			env := codingEnv()
			for _, key := range gitIdentityKeys {
				want := tc.name
				if strings.HasSuffix(key, "EMAIL") {
					want = tc.email
				}
				got, present := env[key]
				if got != want || present != (want != "") {
					t.Errorf("%s = %q, present %v; want %q, present %v", key, got, present, want, want != "")
				}
				if os.Getenv(key) != "inherited-"+key {
					t.Errorf("codingEnv mutated %s", key)
				}
			}
		})
	}
}

// Exercise ordinary commits with isolated Git configuration, both directly and
// through BONNIE's environment wrapper and actual Local backend.
func TestGitIdentityCommits(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed; cannot verify commit identity")
	}
	for _, backend := range []string{"subprocess", "local"} {
		t.Run(backend, func(t *testing.T) {
			for _, tc := range []struct {
				label, name, email string
				inherited          bool
			}{
				{label: "both override inherited and repository", name: "Jawa Agent", email: "agent@example.com", inherited: true},
				{label: "name only repository fallback", name: "Jawa Agent"},
				{label: "email only repository fallback", email: "agent@example.com"},
				{label: "name only inherited fallback", name: "Jawa Agent", inherited: true},
				{label: "email only inherited fallback", email: "agent@example.com", inherited: true},
				{label: "repository fallback"},
				{label: "inherited fallback", inherited: true},
			} {
				t.Run(tc.label, func(t *testing.T) {
					t.Setenv("GITHUB_TOKEN", "")
					t.Setenv("JAWA_GIT_USER_NAME", tc.name)
					t.Setenv("JAWA_GIT_USER_EMAIL", tc.email)
					dir, home := t.TempDir(), t.TempDir()
					global := filepath.Join(home, "gitconfig")
					if err := os.WriteFile(global, []byte("[user]\n\tname = Global\n\temail = global@example.com\n"), 0600); err != nil {
						t.Fatal(err)
					}
					// Start from a whitelist rather than inheriting host Git settings.
					base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home,
						"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + global}
					if tc.inherited {
						base = append(base, "GIT_AUTHOR_NAME=Inherited Author", "GIT_COMMITTER_NAME=Inherited Committer",
							"GIT_AUTHOR_EMAIL=author@example.com", "GIT_COMMITTER_EMAIL=committer@example.com")
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					direct := func(env []string, args ...string) string {
						t.Helper()
						cmd := exec.CommandContext(ctx, git, args...)
						cmd.Dir, cmd.Env = dir, env
						cmd.WaitDelay = time.Second
						out, err := cmd.CombinedOutput()
						if err != nil {
							t.Fatalf("git %v: %v\n%s", args, err, out)
						}
						return string(out)
					}
					direct(base, "init", "--template=")
					direct(base, "config", "user.name", "Repository")
					direct(base, "config", "user.email", "repository@example.com")
					config := filepath.Join(dir, ".git", "config")
					before, err := os.ReadFile(config)
					if err != nil {
						t.Fatal(err)
					}
					env := codingEnv()
					run := func(args ...string) string {
						merged := append([]string(nil), base...)
						for key, value := range env {
							merged = append(merged, key+"="+value)
						}
						return direct(merged, args...)
					}
					if backend == "local" {
						// Local inherits the server environment. Neutralize all inherited
						// Git variables before applying the isolated base environment.
						for _, entry := range os.Environ() {
							key, _, _ := strings.Cut(entry, "=")
							if strings.HasPrefix(key, "GIT_") || key == "EMAIL" {
								t.Setenv(key, "")
								if err := os.Unsetenv(key); err != nil {
									t.Fatal(err)
								}
							}
						}
						localEnv := base
						p := sandbox.EnvInjected(sandbox.Local(sandbox.WithLocalRoot(dir), sandbox.WithLocalSharedDirectory()), env)
						sb, err := p.Open(ctx, "git-identity")
						if err != nil {
							t.Fatal(err)
						}
						defer sb.Close()
						run = func(args ...string) string {
							t.Helper()
							result, err := sb.Exec(ctx, sandbox.Command{Args: append([]string{git}, args...), Env: localEnv, Timeout: 10 * time.Second})
							if err != nil {
								t.Fatal(err)
							}
							if !result.OK() {
								t.Fatalf("git %v: %s", args, result.Stderr)
							}
							return result.Stdout
						}
					}
					run("-c", "commit.gpgsign=false", "-c", "core.hooksPath="+home, "commit", "--allow-empty", "-m", "identity regression")
					an, cn := "Repository", "Repository"
					ae, ce := "repository@example.com", "repository@example.com"
					if tc.inherited {
						an, cn = "Inherited Author", "Inherited Committer"
						ae, ce = "author@example.com", "committer@example.com"
					}
					if tc.name != "" {
						an, cn = tc.name, tc.name
					}
					if tc.email != "" {
						ae, ce = tc.email, tc.email
					}
					want := strings.Join([]string{an, ae, cn, ce}, "\n") + "\n"
					if got := run("log", "-1", "--format=%an%n%ae%n%cn%n%ce"); got != want {
						t.Fatalf("identity = %q; want %q", got, want)
					}
					after, err := os.ReadFile(config)
					if err != nil || string(after) != string(before) {
						t.Fatalf("repository configuration changed: %v", err)
					}
					if got, err := os.ReadFile(global); err != nil || string(got) != "[user]\n\tname = Global\n\temail = global@example.com\n" {
						t.Fatalf("global configuration changed: %v", err)
					}
				})
			}
		})
	}
}
