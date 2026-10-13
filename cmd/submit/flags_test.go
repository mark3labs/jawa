package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

func TestConnectionFlags(t *testing.T) {
	values := map[string]string{"NATS_URL": "nats://env:secret@localhost:4222", "NATS_USERNAME": "env-user", "NATS_PASSWORD": "env-password", "JAWA_NATS_ROOT_SUBJECT": "env-root"}
	for k, v := range values {
		t.Setenv(k, v)
	}
	c, err := parseFlags(nil, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.url != values["NATS_URL"] || c.username != "env-user" || c.password != "env-password" || c.root != "env-root" {
		t.Fatalf("env defaults ignored")
	}
	c, err = parseFlags([]string{"-nats-url=nats://localhost:1234", "-nats-username=cli-user", "-nats-password=", "-root=cli-root"}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if c.url != "nats://localhost:1234" || c.username != "cli-user" || c.password != "" || c.root != "cli-root" {
		t.Fatal("CLI override ignored")
	}
	for k, v := range values {
		if os.Getenv(k) != v {
			t.Fatalf("%s mutated", k)
		}
	}
	c, err = parseFlags(nil, func(string) string { return "" })
	if err != nil || c.url != "nats://127.0.0.1:4222" || c.root != "bonnie" {
		t.Fatal("fallback defaults changed")
	}
}

func TestHelpRedactsConnectionDefaults(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	_, parseErr := parseFlags([]string{"-help"}, func(string) string { return "credential-secret" })
	os.Stderr = old
	_ = w.Close()
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if parseErr != flag.ErrHelp {
		t.Fatalf("help: %v", parseErr)
	}
	// The root subject is not secret; only credential-bearing flags must redact defaults.
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, "NATS connection setting") && strings.Contains(line, "credential-secret") {
			t.Fatal("help leaked credential")
		}
	}
}
