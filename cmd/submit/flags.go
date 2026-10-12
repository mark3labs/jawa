package main

import (
	"errors"
	"flag"
	"time"
)

type submitConfig struct {
	id, text, root, agent, url, username, password string
	timeout                                        time.Duration
}

func parseFlags(args []string, getenv func(string) string) (submitConfig, error) {
	var c submitConfig
	env := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.StringVar(&c.id, "id", "", "Task ID (reuse only for the same task)")
	fs.StringVar(&c.text, "text", "", "Task instructions")
	fs.StringVar(&c.root, "root", env("JAWA_NATS_ROOT_SUBJECT", "bonnie"), "NATS root subject (JAWA_NATS_ROOT_SUBJECT)")
	fs.StringVar(&c.agent, "agent", "", "Target agent ID; empty uses the shared queue")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Minute, "Maximum time to wait; does not cancel the agent")
	for _, spec := range []struct {
		target              *string
		name, env, fallback string
	}{
		{&c.url, "nats-url", "NATS_URL", "nats://127.0.0.1:4222"},
		{&c.username, "nats-username", "NATS_USERNAME", ""},
		{&c.password, "nats-password", "NATS_PASSWORD", ""},
	} {
		fs.StringVar(spec.target, spec.name, env(spec.env, spec.fallback), "NATS connection setting ("+spec.env+"); default not displayed")
		fs.Lookup(spec.name).DefValue = ""
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, errors.New("submit does not accept positional arguments")
	}
	return c, nil
}
