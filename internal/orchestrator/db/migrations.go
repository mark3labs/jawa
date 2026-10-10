// Package db owns the embedded, forward-only database migration history.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate adopts unversioned legacy databases as well as creating new ones.
// The caller must use a single persistent connection, and must not serve queries
// until this returns. Foreign keys are disabled outside Goose's transactions so
// rebuilding a parent table cannot cascade-delete workflow or planning history.
func Migrate(ctx context.Context, database *sql.DB) (err error) {
	if _, err = database.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := database.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
		if err == nil {
			err = restoreErr
		}
	}()
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, database, files,
		goose.WithDisableGlobalRegistry(true),
		goose.WithGoMigrations(
			goose.NewGoMigration(2, &goose.GoFunc{RunTx: upgradeLegacyColumns}, nil),
			goose.NewGoMigration(4, &goose.GoFunc{RunTx: addPlanningStatus}, nil),
		))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

func upgradeLegacyColumns(ctx context.Context, tx *sql.Tx) error {
	for _, m := range []struct{ table, column, definition string }{
		{"admin", "username", "TEXT NOT NULL DEFAULT 'admin'"},
		{"projects", "provider", "TEXT NOT NULL DEFAULT ''"},
		{"projects", "repo", "TEXT NOT NULL DEFAULT ''"},
		{"projects", "base_branch", "TEXT NOT NULL DEFAULT ''"},
		{"cards", "issue_url", "TEXT NOT NULL DEFAULT ''"},
	} {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, m.table, m.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m.table, m.column, m.definition)); err != nil {
				return err
			}
		}
	}
	return nil
}

func addPlanningStatus(ctx context.Context, tx *sql.Tx) error {
	// Save explicit indexes and triggers; implicit PK indexes are recreated by DDL.
	rows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_schema WHERE tbl_name='cards' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name`)
	if err != nil {
		return err
	}
	var ddl []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			_ = rows.Close()
			return err
		}
		ddl = append(ddl, statement)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Do NOT rename the old table: SQLite would rewrite child FK targets. Copy,
	// drop, then rename the replacement while enforcement is disabled instead.
	_, err = tx.ExecContext(ctx, `CREATE TABLE cards_planning_upgrade (
 id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK(status IN ('Todo','Planning','Building','Done')),
 position INTEGER NOT NULL CHECK(position>=0), created_at INTEGER NOT NULL,
 issue_url TEXT NOT NULL DEFAULT '');
 INSERT INTO cards_planning_upgrade(id,project_id,title,description,status,position,created_at,issue_url)
 SELECT id,project_id,title,description,status,position,created_at,issue_url FROM cards;
 DROP TABLE cards;
 ALTER TABLE cards_planning_upgrade RENAME TO cards;`)
	if err != nil {
		return err
	}
	for _, statement := range ddl {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// Validate before Goose commits either the rebuild or its version record.
	rows, err = tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("foreign key violation after cards rebuild")
	}
	return rows.Err()
}
