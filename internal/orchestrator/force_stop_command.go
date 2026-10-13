package orchestrator

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// ForceStopCommand is the out-of-band escape hatch for when the dashboard is
// unavailable. It needs only the data directory, so it works while the agent
// or broker is down; it does not contact the agent. Planning cards are
// cancelled back to Todo; build cards are force-stopped in their lane.
func ForceStopCommand() *cobra.Command {
	var dir, card string
	cmd := &cobra.Command{
		Use:   "force-stop",
		Short: "Release a stuck card without waiting for its agent",
		Long: "Release a stuck card without waiting for its agent.\n\n" +
			"JAWA stops tracking the card's run and ignores later reports from it. The agent is not\n" +
			"contacted and may still be running; it can still push commits or update its pull request.\n" +
			"Afterwards Retry, Reset to Todo and Delete are available in the dashboard.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(dir) == "" || strings.TrimSpace(card) == "" {
				return errors.New("force-stop: --data-dir and --card are required")
			}
			s, err := OpenStore(filepath.Join(dir, "orchestrator.db"))
			if err != nil {
				return errors.New("force-stop: unable to open store")
			}
			defer func() { _ = s.Close() }()
			if err = migrateWorkflow(s); err != nil {
				return errors.New("force-stop: unable to migrate workflow")
			}
			id, status, err := resolveCard(s, card)
			if err != nil {
				return err
			}
			w := &Workflow{s: s, ctx: cmd.Context()}
			if status == "Planning" {
				err = w.CancelPlanning(id)
			} else {
				err = w.ForceStop(id)
			}
			if err != nil {
				return fmt.Errorf("force-stop: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Force-stopped card %s (%s).\n", id, status)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "data-dir", orchestratorEnv("JAWA_ORCHESTRATOR_DATA_DIR", ""), "Persistent data directory (required)")
	cmd.Flags().StringVar(&card, "card", "", "Card ID, unique ID prefix, or board code such as JAW-F4AD (required)")
	return cmd
}

// resolveCard accepts a full ID, a unique prefix, or the board's JAW-XXXX code.
func resolveCard(s *Store, ref string) (string, string, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	ref = strings.TrimPrefix(ref, "jaw-")
	if ref == "" || strings.ContainsAny(ref, "%_") {
		return "", "", errors.New("force-stop: invalid card reference")
	}
	rows, err := s.db.Query(`SELECT id,status FROM cards WHERE id=? OR id LIKE ?||'%' LIMIT 2`, ref, ref)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rows.Close() }()
	var id, status string
	n := 0
	for rows.Next() {
		if err = rows.Scan(&id, &status); err != nil {
			return "", "", err
		}
		n++
	}
	if err = rows.Err(); err != nil {
		return "", "", err
	}
	switch n {
	case 0:
		return "", "", errors.New("force-stop: card not found")
	case 1:
		return id, status, nil
	}
	return "", "", errors.New("force-stop: card reference is ambiguous; use more of the ID")
}
