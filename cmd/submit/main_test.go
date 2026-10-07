package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	const key = "JAWA_TEST_DOTENV_LOAD"
	old, exists := os.LookupEnv(key)
	t.Cleanup(func() {
		if exists {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	_ = os.Unsetenv(key)
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(key+"=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(key) != "from-file" {
		t.Fatal("file value not loaded")
	}
	_ = os.Setenv(key, "from-shell")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(key) != "from-shell" {
		t.Fatal("existing environment overwritten")
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	if err := loadEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEnvFileInvalidDoesNotLeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("INVALID!KEY=private-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := loadEnvFile(path)
	if err == nil {
		t.Fatal("expected syntax error")
	}
	if strings.Contains(err.Error(), "private-secret") {
		t.Fatal("credential leaked")
	}
}
