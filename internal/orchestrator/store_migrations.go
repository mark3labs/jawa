package orchestrator

import (
	"context"
	"database/sql"

	storedb "jawa/internal/orchestrator/db"
)

func migrateDatabase(db *sql.DB) error {
	return storedb.Migrate(context.Background(), db)
}
