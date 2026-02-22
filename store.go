package overseer

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// OpenDB opens the SQLite database at path and configures WAL mode + busy timeout.
// Pass ":memory:" for an in-memory database (used in tests).
//
// WAL journal mode and a 5 s busy timeout are always enabled so that multiple
// processes sharing the same file do not deadlock under concurrent load.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("overseer: failed to set pragma %q: %w", pragma, err)
		}
	}

	return db, nil
}
