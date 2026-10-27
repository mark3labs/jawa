package main

import (
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/spf13/cobra"
	"testing"
)

func TestConcurrencyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		env     string
		args    []string
		want    int
		invalid bool
	}{
		{want: 4}, {env: "6", want: 6}, {env: "6", args: []string{"--concurrency", "8"}, want: 8},
		{env: "invalid", args: []string{"--concurrency", "2"}, want: 2},
		{env: "0", invalid: true}, {args: []string{"--concurrency", "1025"}, invalid: true},
		{args: []string{"--concurrency", ""}, invalid: true},
	} {
		t.Run(tc.env+"/"+string(rune(tc.want+'0')), func(t *testing.T) {
			t.Setenv("JAWA_CONCURRENCY", tc.env)
			called := false
			cmd := &cobra.Command{Use: "jawa", SilenceUsage: true, SilenceErrors: true, RunE: func(*cobra.Command, []string) error { return nil }}
			servingFlags(cmd, func(_ string, cfg natschannel.Config) {
				called = true
				if cfg.Concurrency != tc.want {
					t.Fatalf("got %d want %d", cfg.Concurrency, tc.want)
				}
			})
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if (err != nil) != tc.invalid || called == tc.invalid {
				t.Fatalf("err=%v called=%v", err, called)
			}
		})
	}
}
