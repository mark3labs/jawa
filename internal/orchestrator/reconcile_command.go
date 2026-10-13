package orchestrator

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// ReconcileCommand explicitly authorizes selection of one retained execution.
// It does not start dispatch, subscribe to results, or modify broker consumers.
func ReconcileCommand() *cobra.Command {
	var dir, task, run, attempt, broker, username, password string
	providerDefaults := map[string]string{}
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile a retained execution and independently verify PR readiness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, flag := range []struct{ name, value string }{
				{"data-dir", dir}, {"task", task}, {"run", run}, {"attempt", attempt}, {"nats-url", broker},
			} {
				if strings.TrimSpace(flag.value) == "" {
					return fmt.Errorf("reconcile: --%s must be nonempty", flag.name)
				}
			}
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			conn, err := nats.Connect(broker, nats.UserInfo(username, password), nats.NoReconnect())
			if err != nil {
				// URLs and broker errors can contain credentials; never echo them.
				return errors.New("reconcile: unable to connect to NATS; check URL and credentials")
			}
			defer conn.Close()
			s, err := OpenStore(filepath.Join(dir, "orchestrator.db"))
			if err != nil {
				return errors.New("reconcile: unable to open store")
			}
			defer func() { _ = s.Close() }()
			readProviderFlags(cmd, providerDefaults)
			s.runtimeDefaults = providerDefaults
			w := &Workflow{s: s, ctx: cmd.Context()}
			if err = w.RecoverResults(cmd.Context(), conn); err != nil {
				return errors.New("reconcile: unable to recover retained results")
			}
			pinned, readErr := scanAttempt(s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, task))
			if readErr != nil {
				return errors.New("reconcile: task not found")
			}
			if pinned.RunID != run || pinned.RemoteAttemptID != attempt || pinned.OutcomeJSON == "" {
				if err = w.Reconcile(task, run, attempt); err != nil {
					return err
				}
			}
			a, err := scanAttempt(s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, task))
			if err != nil {
				return errors.New("reconcile: unable to read selected attempt")
			}
			if a.RunState == "completed" && a.PRURL != "" {
				ok, verifyErr := ProviderVerifier(s, nil)(cmd.Context(), a)
				if verifyErr == nil && ok {
					if err = w.markReady(a); err != nil {
						return errors.New("reconcile: unable to record readiness")
					}
				} else {
					// Verification failure (including missing credentials) is expected.
					// Do not print provider errors or the untrusted agent report.
					_, err = fmt.Fprintln(cmd.OutOrStdout(), "Reconciled: blocked; provider readiness not verified.")
					return err
				}
			}
			a, err = scanAttempt(s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, task))
			if err != nil {
				return errors.New("reconcile: unable to read readiness")
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Reconciled: %s.\n", a.State)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "data-dir", orchestratorEnv("JAWA_ORCHESTRATOR_DATA_DIR", ""), "Persistent data directory (required)")
	cmd.Flags().StringVar(&task, "task", "", "Task identity to reconcile (required)")
	cmd.Flags().StringVar(&run, "run", "", "Retained run identity to select (required)")
	cmd.Flags().StringVar(&attempt, "attempt", "", "Retained remote attempt identity to select (required)")
	secretFlag(cmd, &broker, "nats-url", "NATS_URL", "")
	secretFlag(cmd, &username, "nats-username", "NATS_USERNAME", "")
	secretFlag(cmd, &password, "nats-password", "NATS_PASSWORD", "")
	providerFlags(cmd, providerDefaults)
	return cmd
}
