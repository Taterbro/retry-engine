package main

import (
	"database/sql"
	"log"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver with database/sql
)

// initDB opens (or creates) the SQLite file and runs migrations.
func initDB(path string) *sql.DB {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	// SQLite only allows one writer at a time. Limiting to one connection
	// prevents "database is locked" errors from concurrent goroutines.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	return db
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS requests (
			id            TEXT PRIMARY KEY,
			url           TEXT    NOT NULL,
			method        TEXT    NOT NULL,
			body          TEXT,
			status        TEXT    NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			max_retries   INTEGER NOT NULL DEFAULT 5,
			backoff_ms    INTEGER NOT NULL DEFAULT 1000,
			next_retry_at TEXT,       -- ISO-8601 timestamp, NULLable
			last_error    TEXT,
			result        TEXT,
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS attempts (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id   TEXT    NOT NULL REFERENCES requests(id),
			attempt_num  INTEGER NOT NULL,
			status_code  INTEGER,     -- NULL on network/build error
			error        TEXT,
			response     TEXT,
			attempted_at TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_requests_status_next
			ON requests (status, next_retry_at);
	`)
	return err
}
