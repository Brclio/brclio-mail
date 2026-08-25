package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Brclio/brclio-mail/internal/security"
)

type auditExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) Audit(ctx context.Context, event AuditEvent) error {
	return s.insertAudit(ctx, s.db, event)
}

func (s *Store) insertAudit(ctx context.Context, execer auditExecer, event AuditEvent) error {
	if event.ID == "" {
		id, err := security.NewID("aud")
		if err != nil {
			return err
		}
		event.ID = id
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.now()
	}
	if event.Metadata == "" {
		event.Metadata = "{}"
	}
	var actor any
	if event.ActorID != "" {
		actor = event.ActorID
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO audit_log
		(id,actor_id,action,target_type,target_id,reason,metadata_json,ip,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		event.ID, actor, event.Action, event.TargetType, event.TargetID, event.Reason, event.Metadata, event.IP, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuditFailed, err)
	}
	return nil
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	return s.ListAuditPage(ctx, limit, 0, "")
}

func (s *Store) ListAuditPage(ctx context.Context, limit, offset int, action string) ([]AuditEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where := ""
	args := []any{}
	if action != "" {
		where = " WHERE a.action=?"
		args = append(args, action)
	}
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,COALESCE(a.actor_id,''),COALESCE(u.email,''),a.action,
		a.target_type,a.target_id,a.reason,a.metadata_json,a.ip,a.created_at FROM audit_log a
		LEFT JOIN users u ON u.id=a.actor_id`+where+` ORDER BY a.created_at DESC,a.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AuditEvent
	for rows.Next() {
		var item AuditEvent
		if err := rows.Scan(&item.ID, &item.ActorID, &item.ActorEmail, &item.Action, &item.TargetType, &item.TargetID,
			&item.Reason, &item.Metadata, &item.IP, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Stats(ctx context.Context) (SystemStats, error) {
	var stats SystemStats
	queries := []struct {
		dst   *int64
		query string
	}{
		{&stats.Domains, `SELECT COUNT(*) FROM domains`},
		{&stats.Users, `SELECT COUNT(*) FROM users`},
		{&stats.ActiveUsers, `SELECT COUNT(*) FROM users WHERE status='active'`},
		{&stats.Messages, `SELECT COUNT(*) FROM messages WHERE direction<>'draft'`},
		{&stats.UserCopies, `SELECT COUNT(*) FROM mailbox_entries WHERE expunged_at IS NULL`},
		{&stats.ArchivedBytes, `SELECT COALESCE(SUM(size_bytes),0) FROM messages WHERE direction<>'draft'`},
		{&stats.EstimatedStorageBytes, archiveStorageSQL},
		{&stats.Queued, `SELECT COUNT(*) FROM outgoing_queue WHERE status IN ('queued','delivering')`},
		{&stats.Failed, `SELECT COUNT(*) FROM outgoing_queue WHERE status='failed'`},
	}
	for _, query := range queries {
		if err := s.db.QueryRowContext(ctx, query.query).Scan(query.dst); err != nil && err != sql.ErrNoRows {
			return stats, err
		}
	}
	return stats, nil
}
