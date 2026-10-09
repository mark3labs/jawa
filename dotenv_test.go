package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	const key = "JAWA_NATS_WORKER_ID"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(key+"=jawa-dotenv\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if natsConfig().WorkerID != "jawa-dotenv" {
		t.Fatal("worker ID not loaded from dotenv")
	}
	t.Setenv(key, "jawa-shell")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if natsConfig().WorkerID != "jawa-shell" {
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
