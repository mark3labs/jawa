package orchestrator

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
	client "github.com/mark3labs/bonnie/client/nats"
)

func TestCancellationLoggingOmitsRemoteDetails(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Default()
	log.SetDefault(log.NewWithOptions(&output, log.Options{Level: log.DebugLevel}))
	t.Cleanup(func() { log.SetDefault(previous) })

	w := &Workflow{s: s, ctx: t.Context()}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "remote-secret-should-not-be-logged"
	a.Error, a.Result, a.TaskJSON, a.OutcomeJSON, a.PRURL = secret, secret, secret, secret, secret
	// The persisted notice is intentionally not safe for logging. Only IDs,
	// states and fixed operational messages should appear in the output.
	if err := w.cancelRecordFailure(a, "uncertain", secret); err == nil {
		t.Fatal("expected unconfirmed cancellation error")
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("sensitive attempt detail logged: %s", output.String())
	}
	if !strings.Contains(output.String(), "workflow cancellation could not be confirmed") || !strings.Contains(output.String(), a.ID) {
		t.Fatalf("missing cancellation metadata: %s", output.String())
	}
}

func TestResyncLoggingRejectsRemoteError(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	previous := log.Default()
	log.SetDefault(log.NewWithOptions(&output, log.Options{Level: log.DebugLevel}))
	t.Cleanup(func() { log.SetDefault(previous) })

	w := &Workflow{s: s, ctx: t.Context()}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "provider-token-in-remote-error"
	if err := w.applyStatus(a, client.Status{Version: 1, Error: secret}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "workflow status transition") {
		t.Fatalf("invalid status logged as transition: %s", output.String())
	}
}
