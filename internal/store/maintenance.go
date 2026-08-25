package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IntegrityCheck runs SQLite's full consistency check and verifies that no
// foreign-key violations are present.
func (s *Store) IntegrityCheck(ctx context.Context) error {
	return integrityCheck(ctx, s.db)
}

// Backup creates a consistent, compact SQLite snapshot using VACUUM INTO,
// validates it, and atomically moves it into place. Existing files are never
// overwritten.
func (s *Store) Backup(ctx context.Context, destination string) error {
	destination, err := filepath.Abs(strings.TrimSpace(destination))
	if err != nil || destination == "" {
		return errors.New("invalid backup destination")
	}
	if _, err = os.Stat(destination); err == nil {
		return fmt.Errorf("backup destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}

	placeholder, err := os.CreateTemp(filepath.Dir(destination), ".brclio-mail-backup-*.sqlite")
	if err != nil {
		return fmt.Errorf("reserve backup path: %w", err)
	}
	temporary := placeholder.Name()
	if err = placeholder.Close(); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("close backup placeholder: %w", err)
	}
	if err = os.Remove(temporary); err != nil {
		return fmt.Errorf("prepare backup path: %w", err)
	}
	defer os.Remove(temporary)

	if _, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, temporary); err != nil {
		return fmt.Errorf("create sqlite snapshot: %w", err)
	}
	if err = os.Chmod(temporary, 0o600); err != nil {
		return fmt.Errorf("secure backup permissions: %w", err)
	}
	backupDB, err := sql.Open("sqlite", temporary)
	if err != nil {
		return fmt.Errorf("open backup for validation: %w", err)
	}
	defer backupDB.Close()
	if err = integrityCheck(ctx, backupDB); err != nil {
		return fmt.Errorf("validate backup: %w", err)
	}
	if err = backupDB.Close(); err != nil {
		return fmt.Errorf("close validated backup: %w", err)
	}
	// A hard-link publish is atomic and fails if destination appeared during
	// backup creation; unlike POSIX rename it can never replace that file.
	if err = os.Link(temporary, destination); err != nil {
		return fmt.Errorf("publish backup: %w", err)
	}
	if err = os.Remove(temporary); err != nil {
		return fmt.Errorf("remove backup staging link: %w", err)
	}
	return nil
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("sqlite integrity check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("sqlite integrity check returned %q", result)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("sqlite foreign-key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("sqlite foreign-key check found violations")
	}
	return rows.Err()
}
