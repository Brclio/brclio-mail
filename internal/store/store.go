package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrQuotaExceeded      = errors.New("mailbox quota exceeded")
	ErrArchiveFull        = errors.New("archive storage limit exceeded")
	ErrDomainUnverified   = errors.New("domain ownership is not verified")
	ErrAuditFailed        = errors.New("mandatory audit write failed")
	ErrResourceLimit      = errors.New("resource limit exceeded")
	ErrForbidden          = errors.New("forbidden")
)

const maxInt64 = int64(^uint64(0) >> 1)

type Store struct {
	db            *sql.DB
	now           func() time.Time
	sqliteVersion string
	databasePath  string
	archiveLimit  int64
	minFreeDisk   int64
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	var sqliteVersion string
	if err := db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("read sqlite version: %w", err)
	}
	if !sqliteVersionAtLeast(sqliteVersion, 3, 51, 3) {
		db.Close()
		return nil, fmt.Errorf("unsafe sqlite version %s: Brclio Mail requires 3.51.3 or newer because of the WAL-reset corruption fix", sqliteVersion)
	}
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }, sqliteVersion: sqliteVersion, databasePath: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("secure sqlite database permissions: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Close() error          { return s.db.Close() }
func (s *Store) DB() *sql.DB           { return s.db }
func (s *Store) SQLiteVersion() string { return s.sqliteVersion }

// SetArchiveLimit caps immutable message storage. Set it once during startup,
// before accepting mail. A value of zero disables the cap for library tests.
func (s *Store) SetArchiveLimit(bytes int64) { s.archiveLimit = bytes }

// SetStorageLimits sets both the conservative SQLite archive budget and the
// amount of filesystem space that must remain free while accepting a message.
func (s *Store) SetStorageLimits(archiveBytes, minFreeDiskBytes int64) {
	s.archiveLimit = archiveBytes
	s.minFreeDisk = minFreeDiskBytes
}

func normalizeEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func mapSQLError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func sqliteVersionAtLeast(value string, wantMajor, wantMinor, wantPatch int) bool {
	var major, minor, patch int
	if _, err := fmt.Sscanf(value, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false
	}
	if major != wantMajor {
		return major > wantMajor
	}
	if minor != wantMinor {
		return minor > wantMinor
	}
	return patch >= wantPatch
}
