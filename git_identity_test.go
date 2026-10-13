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

func TestCodingEnvGitIdentity(t *testing.T) {
	for _, tc := range []struct {
		label, name, email string
		unset              bool
	}{
		{label: "unset", unset: true},
		{label: "empty"},
		{label: "both", name: "Bot", email: "bot@example.com"},
		{label: "name only", name: "Bot"},
		{label: "email only", email: "bot@example.com"},
		{label: "spaces preserved", name: "  Jawa Bot  ", email: "bot@example.com"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Setenv("JAWA_GIT_USER_NAME", tc.name)
			t.Setenv("JAWA_GIT_USER_EMAIL", tc.email)
			if tc.unset {
				if err := os.Unsetenv("JAWA_GIT_USER_NAME"); err != nil {
					t.Fatal(err)
				}
				if err := os.Unsetenv("JAWA_GIT_USER_EMAIL"); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
				t.Setenv(key, "inherited")
			}
			env := codingEnv()
			for key, want := range map[string]string{
				"GIT_AUTHOR_NAME": tc.name, "GIT_COMMITTER_NAME": tc.name,
				"GIT_AUTHOR_EMAIL": tc.email, "GIT_COMMITTER_EMAIL": tc.email,
			} {
				got, present := env[key]
				if got != want || present != (want != "") {
					t.Errorf("%s = %q (present %v), want %q", key, got, present, want)
				}
				if os.Getenv(key) != "inherited" {
					t.Errorf("codingEnv mutated process variable %s", key)
				}
			}
		})
	}
}

// Exercise real Git commits through both a hermetic subprocess and BONNIE's
// environment wrapper used by WithSandboxEnv. All repositories/configs are temporary.
func TestGitIdentityCommits(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed: cannot verify commit metadata")
	}
	// Local inherits the process environment. Remove Git controls so it cannot
	// access host repositories/configs, and restore them when the test finishes.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "EMAIL" {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GITHUB_TOKEN", "")
	for _, backend := range []string{"exec", "local"} {
		for _, tc := range []struct {
			label, name, email, wantName, wantEmail string
			inherited                               bool
		}{
			{"both override inherited", "Jawa Bot", "bot@example.com", "Jawa Bot", "bot@example.com", true},
			{"name only", "Jawa Bot", "", "Jawa Bot", "repo@example.com", false},
			{"email only", "", "bot@example.com", "Repo User", "bot@example.com", false},
			{"repository fallback", "", "", "Repo User", "repo@example.com", false},
			{"inherited fallback", "", "", "Inherited User", "inherited@example.com", true},
			{"partial inherited fallback", "Jawa Bot", "", "Jawa Bot", "inherited@example.com", true},
		} {
			t.Run(backend+"/"+tc.label, func(t *testing.T) {
				t.Setenv("JAWA_GIT_USER_NAME", tc.name)
				t.Setenv("JAWA_GIT_USER_EMAIL", tc.email)
				dir, home := t.TempDir(), t.TempDir()
				base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home,
					"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"), "GIT_TERMINAL_PROMPT=0"}
				if tc.inherited {
					base = append(base, "GIT_AUTHOR_NAME=Inherited User", "GIT_COMMITTER_NAME=Inherited User",
						"GIT_AUTHOR_EMAIL=inherited@example.com", "GIT_COMMITTER_EMAIL=inherited@example.com")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				var sb sandbox.Sandbox
				if backend == "local" {
					provider := sandbox.EnvInjected(sandbox.Local(sandbox.WithLocalRoot(dir), sandbox.WithLocalSharedDirectory()), codingEnv())
					sb, err = provider.Open(ctx, "git-identity-test")
					if err != nil {
						t.Fatal(err)
					}
					defer sb.Close()
				}
				run := func(args ...string) string {
					t.Helper()
					if sb != nil {
						result, err := sb.Exec(ctx, sandbox.Command{Args: append([]string{git}, args...), Env: base})
						if err != nil || !result.OK() {
							t.Fatalf("Git %v: result %+v, error %v", args, result, err)
						}
						return strings.TrimSpace(result.Stdout)
					}
					cmd := exec.CommandContext(ctx, git, args...)
					cmd.Dir, cmd.Env = dir, append([]string{}, base...)
					for key, value := range codingEnv() {
						cmd.Env = append(cmd.Env, key+"="+value)
					}
					output, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("Git %v: %v: %s", args, err, output)
					}
					return strings.TrimSpace(string(output))
				}
				run("init", "--template=", ".")
				run("config", "user.name", "Repo User")
				run("config", "user.email", "repo@example.com")
				run("config", "commit.gpgsign", "false")
				configPath := filepath.Join(dir, ".git", "config")
				before, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				run("commit", "--allow-empty", "-m", "identity regression")
				want := strings.Join([]string{tc.wantName, tc.wantEmail, tc.wantName, tc.wantEmail}, "\n")
				if got := run("log", "-1", "--format=%an%n%ae%n%cn%n%ce"); got != want {
					t.Fatalf("commit identity = %q, want %q", got, want)
				}
				after, err := os.ReadFile(configPath)
				if err != nil || string(before) != string(after) {
					t.Fatalf("repository config changed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(home, "gitconfig")); !os.IsNotExist(err) {
					t.Fatalf("unexpected global config write: %v", err)
				}
			})
		}
	}
}
