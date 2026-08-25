package store

import (
	"context"
	"fmt"
)

const schemaVersion = 4

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMP NOT NULL);`); err != nil {
		return fmt.Errorf("create migration registry: %w", err)
	}
	var currentVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&currentVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if currentVersion > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", currentVersion, schemaVersion)
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS domains (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE COLLATE NOCASE,
			status TEXT NOT NULL DEFAULT 'pending',
			verification_token TEXT NOT NULL,
			dkim_selector TEXT NOT NULL DEFAULT 'brclio',
			dkim_public_key TEXT NOT NULL DEFAULT '',
			dkim_private_key TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL,
			verified_at TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			domain_id TEXT NOT NULL REFERENCES domains(id),
			local_part TEXT NOT NULL COLLATE NOCASE,
			email TEXT NOT NULL UNIQUE COLLATE NOCASE,
			display_name TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL CHECK (role IN ('admin','user')),
			status TEXT NOT NULL CHECK (status IN ('active','suspended')),
			quota_bytes INTEGER NOT NULL CHECK (quota_bytes >= 0),
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			last_login_at TIMESTAMP,
			UNIQUE(domain_id, local_part)
		);`,
		`CREATE TABLE IF NOT EXISTS aliases (
			id TEXT PRIMARY KEY,
			address TEXT NOT NULL UNIQUE COLLATE NOCASE,
			target_user_id TEXT NOT NULL REFERENCES users(id),
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token_hash TEXT NOT NULL UNIQUE,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			last_seen_at TIMESTAMP NOT NULL,
			ip TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS app_passwords (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			secret_hash TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			last_used_at TIMESTAMP,
			revoked_at TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY,
			rfc_message_id TEXT NOT NULL,
			thread_key TEXT NOT NULL DEFAULT '',
			envelope_from TEXT NOT NULL DEFAULT '',
			envelope_to_json TEXT NOT NULL DEFAULT '[]',
			header_from TEXT NOT NULL DEFAULT '',
			header_to_json TEXT NOT NULL DEFAULT '[]',
			header_cc_json TEXT NOT NULL DEFAULT '[]',
			header_bcc_json TEXT NOT NULL DEFAULT '[]',
			reply_to TEXT NOT NULL DEFAULT '',
			subject TEXT NOT NULL DEFAULT '',
			text_body TEXT NOT NULL DEFAULT '',
			html_body TEXT NOT NULL DEFAULT '',
			snippet TEXT NOT NULL DEFAULT '',
			raw BLOB NOT NULL,
			size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
			attachment_count INTEGER NOT NULL DEFAULT 0,
			direction TEXT NOT NULL CHECK (direction IN ('inbound','outbound','draft','append')),
			transport_status TEXT NOT NULL DEFAULT 'received',
			sent_at TIMESTAMP,
			received_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_received_at ON messages(received_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_rfc_id ON messages(rfc_message_id);`,
		`CREATE TABLE IF NOT EXISTS attachments (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			filename TEXT NOT NULL,
			content_type TEXT NOT NULL,
			content_id TEXT NOT NULL DEFAULT '',
			disposition TEXT NOT NULL DEFAULT 'attachment',
			size_bytes INTEGER NOT NULL,
			sha256 TEXT NOT NULL,
			content BLOB NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);`,
		`CREATE TABLE IF NOT EXISTS mailboxes (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL COLLATE NOCASE,
			uid_validity INTEGER NOT NULL,
			uid_next INTEGER NOT NULL DEFAULT 1,
			subscribed INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL,
			UNIQUE(user_id, name)
		);`,
		`CREATE TABLE IF NOT EXISTS mailbox_entries (
			id TEXT PRIMARY KEY,
			mailbox_id TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			message_id TEXT NOT NULL REFERENCES messages(id),
			uid INTEGER NOT NULL,
			flags_json TEXT NOT NULL DEFAULT '[]',
			internal_date TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL,
			deleted_at TIMESTAMP,
			expunged_at TIMESTAMP,
			UNIQUE(mailbox_id, uid)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_mailbox_entries_user_box ON mailbox_entries(user_id, mailbox_id, expunged_at, uid);`,
		`CREATE INDEX IF NOT EXISTS idx_mailbox_entries_message ON mailbox_entries(message_id);`,
		`CREATE TABLE IF NOT EXISTS outgoing_queue (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL REFERENCES messages(id),
			recipient TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('queued','delivering','delivered','failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt TIMESTAMP NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			delivered_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			UNIQUE(message_id, recipient)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_queue_ready ON outgoing_queue(status, next_attempt);`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id TEXT PRIMARY KEY,
			actor_id TEXT REFERENCES users(id),
			action TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			ip TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created_at ON audit_log(created_at DESC);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS message_search USING fts5(
			message_id UNINDEXED,
			subject,
			header_from,
			recipients,
			text_body,
			tokenize='unicode61 remove_diacritics 2'
		);`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO message_search(message_id, subject, header_from, recipients, text_body)
			VALUES (new.id, new.subject, new.header_from, new.header_to_json || ' ' || new.header_cc_json, new.text_body);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
			DELETE FROM message_search WHERE message_id = old.id;
		END;`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
			DELETE FROM message_search WHERE message_id = old.id;
			INSERT INTO message_search(message_id, subject, header_from, recipients, text_body)
			VALUES (new.id, new.subject, new.header_from, new.header_to_json || ' ' || new.header_cc_json, new.text_body);
		END;`,
		`CREATE TRIGGER IF NOT EXISTS messages_raw_immutable BEFORE UPDATE OF raw ON messages
			WHEN old.direction <> 'draft' AND new.raw IS NOT old.raw BEGIN
				SELECT RAISE(ABORT, 'retained message raw MIME is immutable');
			END;`,
		`CREATE TRIGGER IF NOT EXISTS messages_retained BEFORE DELETE ON messages
			WHEN old.direction <> 'draft' BEGIN
				SELECT RAISE(ABORT, 'retained correspondence cannot be deleted');
			END;`,
	}

	if currentVersion < 1 {
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema migration v1: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`); err != nil {
			return fmt.Errorf("record schema migration v1: %w", err)
		}
	}
	if currentVersion < 2 {
		var hasAuthVersion int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('users') WHERE name='auth_version'`).Scan(&hasAuthVersion); err != nil {
			return fmt.Errorf("inspect schema migration v2: %w", err)
		}
		if hasAuthVersion == 0 {
			if _, err := tx.ExecContext(ctx,
				`ALTER TABLE users ADD COLUMN auth_version INTEGER NOT NULL DEFAULT 1 CHECK (auth_version >= 1)`); err != nil {
				return fmt.Errorf("apply schema migration v2: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (2, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`); err != nil {
			return fmt.Errorf("record schema migration v2: %w", err)
		}
	}
	if currentVersion < 3 {
		var hasSessionAuthVersion int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='auth_version'`).Scan(&hasSessionAuthVersion); err != nil {
			return fmt.Errorf("inspect schema migration v3: %w", err)
		}
		if hasSessionAuthVersion == 0 {
			if _, err := tx.ExecContext(ctx,
				`ALTER TABLE sessions ADD COLUMN auth_version INTEGER NOT NULL DEFAULT 1 CHECK (auth_version >= 1)`); err != nil {
				return fmt.Errorf("apply schema migration v3: %w", err)
			}
		}
		// Pre-v3 sessions did not capture the authentication snapshot version.
		// They cannot be proven current, so fail closed once during upgrade.
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
			return fmt.Errorf("invalidate pre-v3 sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (3, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`); err != nil {
			return fmt.Errorf("record schema migration v3: %w", err)
		}
	}
	if currentVersion < 4 {
		var hasProtocolAuthVersion int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('users') WHERE name='protocol_auth_version'`).Scan(&hasProtocolAuthVersion); err != nil {
			return fmt.Errorf("inspect schema migration v4: %w", err)
		}
		if hasProtocolAuthVersion == 0 {
			if _, err := tx.ExecContext(ctx,
				`ALTER TABLE users ADD COLUMN protocol_auth_version INTEGER NOT NULL DEFAULT 1 CHECK (protocol_auth_version >= 1)`); err != nil {
				return fmt.Errorf("apply schema migration v4: %w", err)
			}
		}
		// v3 used auth_version for both Web and mail protocols. Carry that
		// generation forward so existing accounts retain monotonic snapshots.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET protocol_auth_version=auth_version`); err != nil {
			return fmt.Errorf("initialize schema migration v4: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (4, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`); err != nil {
			return fmt.Errorf("record schema migration v4: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}
