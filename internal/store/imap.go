package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Brclio/brclio-mail/internal/security"
)

// IMAPMailbox is the stable metadata needed to expose a mailbox through IMAP.
type IMAPMailbox struct {
	ID          string
	UserID      string
	Name        string
	UIDValidity uint32
	UIDNext     uint32
	Subscribed  bool
}

// IMAPEntry is one user's mutable mailbox view of an immutable message.
type IMAPEntry struct {
	ID           string
	MailboxID    string
	UserID       string
	MessageID    string
	UID          uint32
	Flags        []string
	InternalDate time.Time
	Raw          []byte
	SizeBytes    int64
}

func (s *Store) IMAPListMailboxes(ctx context.Context, userID string, subscribed bool) ([]IMAPMailbox, error) {
	query := `SELECT id,user_id,name,uid_validity,uid_next,subscribed FROM mailboxes WHERE user_id=?`
	if subscribed {
		query += ` AND subscribed=1`
	}
	query += ` ORDER BY CASE name WHEN 'INBOX' THEN 0 ELSE 1 END,name COLLATE NOCASE`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IMAPMailbox
	for rows.Next() {
		var item IMAPMailbox
		var validity, next int64
		if err := rows.Scan(&item.ID, &item.UserID, &item.Name, &validity, &next, &item.Subscribed); err != nil {
			return nil, err
		}
		item.UIDValidity = clampUint32(validity)
		item.UIDNext = clampUint32(next)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) IMAPGetMailbox(ctx context.Context, userID, name string) (IMAPMailbox, error) {
	var item IMAPMailbox
	var validity, next int64
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,name,uid_validity,uid_next,subscribed
		FROM mailboxes WHERE user_id=? AND name=? COLLATE NOCASE`, userID, strings.TrimSpace(name)).Scan(
		&item.ID, &item.UserID, &item.Name, &validity, &next, &item.Subscribed)
	if err != nil {
		return IMAPMailbox{}, mapSQLError(err)
	}
	item.UIDValidity = clampUint32(validity)
	item.UIDNext = clampUint32(next)
	return item, nil
}

func (s *Store) IMAPListEntries(ctx context.Context, userID, mailboxID string) ([]IMAPEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT me.id,me.mailbox_id,me.user_id,me.message_id,me.uid,
		me.flags_json,me.internal_date,m.size_bytes
		FROM mailbox_entries me JOIN messages m ON m.id=me.message_id
		WHERE me.user_id=? AND me.mailbox_id=? AND me.expunged_at IS NULL ORDER BY me.uid`, userID, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []IMAPEntry
	for rows.Next() {
		var item IMAPEntry
		var uid int64
		var flags string
		if err := rows.Scan(&item.ID, &item.MailboxID, &item.UserID, &item.MessageID, &uid,
			&flags, &item.InternalDate, &item.SizeBytes); err != nil {
			return nil, err
		}
		item.UID = clampUint32(uid)
		_ = json.Unmarshal([]byte(flags), &item.Flags)
		result = append(result, item)
	}
	return result, rows.Err()
}

// IMAPEntryRaw loads raw MIME for one currently visible entry. Metadata-only
// mailbox listings intentionally omit raw bytes so STATUS/SELECT cannot load a
// whole large mailbox into memory.
func (s *Store) IMAPEntryRaw(ctx context.Context, userID, mailboxID, entryID string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT m.raw FROM mailbox_entries me JOIN messages m ON m.id=me.message_id
		WHERE me.id=? AND me.user_id=? AND me.mailbox_id=? AND me.expunged_at IS NULL`,
		entryID, userID, mailboxID).Scan(&raw)
	if err != nil {
		return nil, mapSQLError(err)
	}
	return raw, nil
}

func (s *Store) IMAPSetEntryFlags(ctx context.Context, userID, mailboxID, entryID string, flags []string) error {
	normalized := normalizeFlags(flags)
	encoded, _ := json.Marshal(normalized)
	var deletedAt any
	if containsFlag(normalized, "\\Deleted") {
		deletedAt = s.now()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE mailbox_entries SET flags_json=?,
		deleted_at=CASE WHEN ? IS NOT NULL THEN COALESCE(deleted_at,?) ELSE NULL END
		WHERE id=? AND user_id=? AND mailbox_id=? AND expunged_at IS NULL`,
		string(encoded), deletedAt, deletedAt, entryID, userID, mailboxID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) IMAPCopyEntries(ctx context.Context, userID, destination string, entries []IMAPEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var mailboxID string
	var uidNext int64
	if err := tx.QueryRowContext(ctx, `SELECT id,uid_next FROM mailboxes WHERE user_id=? AND name=? COLLATE NOCASE`,
		userID, destination).Scan(&mailboxID, &uidNext); err != nil {
		return mapSQLError(err)
	}
	var quota, used int64
	if err := tx.QueryRowContext(ctx, `SELECT u.quota_bytes,
		COALESCE(SUM(CASE WHEN me.expunged_at IS NULL THEN m.size_bytes ELSE 0 END),0)
		FROM users u LEFT JOIN mailbox_entries me ON me.user_id=u.id
		LEFT JOIN messages m ON m.id=me.message_id WHERE u.id=? GROUP BY u.id`, userID).Scan(&quota, &used); err != nil {
		return mapSQLError(err)
	}
	var additional int64
	for _, entry := range entries {
		additional += entry.SizeBytes
	}
	if quota > 0 && used+additional > quota {
		return ErrQuotaExceeded
	}

	now := s.now()
	for _, entry := range entries {
		entryID, err := security.NewID("ent")
		if err != nil {
			return err
		}
		flags := append([]string(nil), entry.Flags...)
		if !containsFlag(flags, "\\Recent") {
			flags = append(flags, "\\Recent")
		}
		encoded, _ := json.Marshal(normalizeFlags(flags))
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_entries
			(id,mailbox_id,user_id,message_id,uid,flags_json,internal_date,created_at)
			VALUES(?,?,?,?,?,?,?,?)`, entryID, mailboxID, userID, entry.MessageID, uidNext,
			string(encoded), entry.InternalDate, now)
		if err != nil {
			return err
		}
		uidNext++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET uid_next=? WHERE id=?`, uidNext, mailboxID); err != nil {
		return err
	}
	return tx.Commit()
}

// IMAPDeleteMailbox removes one custom mailbox while preserving immutable
// received/sent messages for administrator audit. Drafts are private working
// state: any draft message that loses its final mailbox entry is deleted in the
// same transaction, with attachments removed by the foreign-key cascade.
func (s *Store) IMAPDeleteMailbox(ctx context.Context, userID, name string) error {
	name = strings.TrimSpace(name)
	for _, systemName := range SystemMailboxes {
		if strings.EqualFold(name, systemName) {
			return ErrForbidden
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var mailboxID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM mailboxes WHERE user_id=? AND name=? COLLATE NOCASE`,
		userID, name).Scan(&mailboxID); err != nil {
		return mapSQLError(err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.id FROM messages m
		JOIN mailbox_entries me ON me.message_id=m.id
		WHERE me.mailbox_id=? AND me.user_id=? AND m.direction='draft'`, mailboxID, userID)
	if err != nil {
		return err
	}
	var draftIDs []string
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			rows.Close()
			return err
		}
		draftIDs = append(draftIDs, messageID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailboxes WHERE id=? AND user_id=?`, mailboxID, userID); err != nil {
		return err
	}
	for _, messageID := range draftIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND direction='draft'
			AND NOT EXISTS (SELECT 1 FROM mailbox_entries WHERE message_id=messages.id)`, messageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// IMAPMoveEntries creates destination entries with new destination UIDs and
// removes only the selected source entries in one transaction. Archive-bearing
// messages are soft-expunged; private draft source entries are physically
// removed so a later EXPUNGE of the destination can delete the last draft copy.
func (s *Store) IMAPMoveEntries(ctx context.Context, userID, sourceMailboxID, destination string, entries []IMAPEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var destinationID string
	var uidNext int64
	if err := tx.QueryRowContext(ctx, `SELECT id,uid_next FROM mailboxes WHERE user_id=? AND name=? COLLATE NOCASE`,
		userID, destination).Scan(&destinationID, &uidNext); err != nil {
		return mapSQLError(err)
	}
	now := s.now()
	for _, selected := range entries {
		var messageID, flagsJSON, direction string
		var internalDate time.Time
		err := tx.QueryRowContext(ctx, `SELECT me.message_id,me.flags_json,me.internal_date,m.direction
			FROM mailbox_entries me JOIN messages m ON m.id=me.message_id
			WHERE me.id=? AND me.user_id=? AND me.mailbox_id=? AND me.expunged_at IS NULL`, selected.ID, userID, sourceMailboxID).Scan(
			&messageID, &flagsJSON, &internalDate, &direction)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var flags []string
		_ = json.Unmarshal([]byte(flagsJSON), &flags)
		if !containsFlag(flags, "\\Recent") {
			flags = append(flags, "\\Recent")
		}
		encoded, _ := json.Marshal(normalizeFlags(flags))
		entryID, err := security.NewID("ent")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_entries
			(id,mailbox_id,user_id,message_id,uid,flags_json,internal_date,created_at)
			VALUES(?,?,?,?,?,?,?,?)`, entryID, destinationID, userID, messageID, uidNext, string(encoded), internalDate, now); err != nil {
			return err
		}
		uidNext++
		if direction == "draft" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_entries
				WHERE id=? AND user_id=? AND mailbox_id=? AND expunged_at IS NULL`, selected.ID, userID, sourceMailboxID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE mailbox_entries SET expunged_at=?,deleted_at=COALESCE(deleted_at,?)
				WHERE id=? AND user_id=? AND mailbox_id=? AND expunged_at IS NULL`, now, now, selected.ID, userID, sourceMailboxID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET uid_next=? WHERE id=?`, uidNext, destinationID); err != nil {
		return err
	}
	return tx.Commit()
}

// IMAPExpungeDeleted hides non-draft entries carrying \Deleted while retaining
// their immutable message for administrator audit. Draft entries are physically
// removed; when the last draft entry disappears, its message and cascading
// attachments are removed too, because drafts are not administrator archive.
func (s *Store) IMAPExpungeDeleted(ctx context.Context, userID, mailboxID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT me.id,me.message_id,me.flags_json,m.direction
		FROM mailbox_entries me JOIN messages m ON m.id=me.message_id
		WHERE me.user_id=? AND me.mailbox_id=? AND me.expunged_at IS NULL ORDER BY me.uid`, userID, mailboxID)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		entryID   string
		messageID string
		draft     bool
	}
	var candidates []candidate
	for rows.Next() {
		var id, messageID, flagsJSON, direction string
		if err := rows.Scan(&id, &messageID, &flagsJSON, &direction); err != nil {
			rows.Close()
			return 0, err
		}
		var flags []string
		_ = json.Unmarshal([]byte(flagsJSON), &flags)
		if containsFlag(flags, "\\Deleted") {
			candidates = append(candidates, candidate{entryID: id, messageID: messageID, draft: direction == "draft"})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, tx.Commit()
	}
	now := s.now()
	var count int64
	for _, item := range candidates {
		if item.draft {
			result, err := tx.ExecContext(ctx, `DELETE FROM mailbox_entries
				WHERE id=? AND user_id=? AND mailbox_id=? AND expunged_at IS NULL`, item.entryID, userID, mailboxID)
			if err != nil {
				return 0, err
			}
			rows, _ := result.RowsAffected()
			count += rows
			if rows == 1 {
				var remaining int64
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailbox_entries WHERE message_id=?`, item.messageID).Scan(&remaining); err != nil {
					return 0, err
				}
				if remaining == 0 {
					if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND direction='draft'`, item.messageID); err != nil {
						return 0, err
					}
				}
			}
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE mailbox_entries SET expunged_at=?,deleted_at=COALESCE(deleted_at,?)
			WHERE id=? AND user_id=? AND mailbox_id=? AND expunged_at IS NULL`, now, now, item.entryID, userID, mailboxID)
		if err != nil {
			return 0, err
		}
		rows, _ := result.RowsAffected()
		count += rows
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) IMAPMessageExists(ctx context.Context, messageID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE id=?`, messageID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && exists == 1, err
}

func clampUint32(value int64) uint32 {
	if value < 0 {
		return 0
	}
	if value > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func containsFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}
