package store

import (
	"context"
	"time"
)

// RecoverStaleQueue requeues deliveries whose worker lease was abandoned by a
// crash. Active SMTP delivery is bounded by protocol timeouts, so callers use a
// lease comfortably longer than the maximum normal attempt.
func (s *Store) RecoverStaleQueue(ctx context.Context, staleBefore time.Time) (int64, error) {
	now := s.now()
	result, err := s.db.ExecContext(ctx, `UPDATE outgoing_queue SET status='queued',next_attempt=?,
		last_error='previous delivery worker stopped before completion',updated_at=?
		WHERE status='delivering' AND updated_at<?`, now, now, staleBefore.UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) RefreshMessageTransportStatus(ctx context.Context, messageID string) error {
	var queued, delivering, failed, delivered int64
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN status='queued' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='delivering' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='delivered' THEN 1 ELSE 0 END),0)
		FROM outgoing_queue WHERE message_id=?`, messageID).Scan(&queued, &delivering, &failed, &delivered); err != nil {
		return err
	}
	status := "delivered"
	if queued+delivering > 0 {
		status = "queued"
	} else if failed > 0 {
		if delivered > 0 {
			status = "partial"
		} else {
			status = "failed"
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET transport_status=? WHERE id=?`, status, messageID)
	return err
}
