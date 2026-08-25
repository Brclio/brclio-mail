package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Brclio/brclio-mail/internal/security"
)

type Delivery struct {
	UserID  string
	Mailbox string
	Flags   []string
}

// archiveStorageSQL mirrors estimatedMessageStorage for rows already in
// SQLite. It includes the raw MIME, decoded attachment BLOBs, duplicated body
// projections, a conservative FTS allowance, and fixed row/page overhead.
const archiveStorageSQL = `SELECT
	COALESCE((SELECT SUM(length(raw) + length(text_body) + length(html_body) + length(snippet)
		+ 4 * (length(text_body) + length(subject) + length(header_from) + length(header_to_json) + length(header_cc_json))
		+ 16384) FROM messages),0)
	+ COALESCE((SELECT SUM(length(content) + 1024) FROM attachments),0)`

const (
	maxMailboxesPerUser = 100
	maxMailboxNameBytes = 255
)

func (s *Store) SaveMessage(ctx context.Context, message Message, attachments []Attachment, deliveries []Delivery, queueRecipients []string) (Message, error) {
	return s.saveMessage(ctx, message, attachments, deliveries, queueRecipients, nil, nil)
}

// SaveMessageAudited commits an SMTP message and its transport audit event in
// one SQLite transaction. A failed audit insert therefore cannot leave a
// committed message that the sender is told to retry.
func (s *Store) SaveMessageAudited(ctx context.Context, message Message, attachments []Attachment, deliveries []Delivery, queueRecipients []string, event AuditEvent) (Message, error) {
	return s.saveMessage(ctx, message, attachments, deliveries, queueRecipients, nil, &event)
}

// SaveMessageReplacingDraftAudited atomically removes a private draft owned by
// actorUserID, stores its replacement (or final sent message), and writes the
// audit event. A failed quota check, message insert, or audit write restores the
// previous draft with the transaction rollback.
func (s *Store) SaveMessageReplacingDraftAudited(ctx context.Context, actorUserID, replaceDraftID string, message Message, attachments []Attachment, deliveries []Delivery, queueRecipients []string, event AuditEvent) (Message, error) {
	replacement := &draftReplacement{userID: actorUserID, draftID: replaceDraftID}
	return s.saveMessage(ctx, message, attachments, deliveries, queueRecipients, replacement, &event)
}

type draftReplacement struct {
	userID  string
	draftID string
}

func (s *Store) saveMessage(ctx context.Context, message Message, attachments []Attachment, deliveries []Delivery, queueRecipients []string, replacement *draftReplacement, event *AuditEvent) (Message, error) {
	if message.ID == "" {
		id, err := security.NewID("msg")
		if err != nil {
			return Message{}, err
		}
		message.ID = id
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = s.now()
	}
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = message.CreatedAt
	}
	if message.SizeBytes == 0 {
		message.SizeBytes = int64(len(message.Raw))
	}
	if message.TransportStatus == "" {
		message.TransportStatus = "received"
	}
	storageBytes := estimatedMessageStorage(message, attachments)
	if err := s.ensureDiskReserve(storageBytes); err != nil {
		return Message{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if replacement != nil && replacement.draftID != "" {
		var oldMessageID string
		err = tx.QueryRowContext(ctx, `SELECT m.id FROM messages m
			JOIN mailbox_entries me ON me.message_id=m.id
			WHERE me.user_id=? AND me.expunged_at IS NULL AND m.direction='draft' AND (m.id=? OR me.id=?)
			AND NOT EXISTS (SELECT 1 FROM mailbox_entries other WHERE other.message_id=m.id AND other.user_id<>?)
			ORDER BY me.created_at DESC LIMIT 1`, replacement.userID, replacement.draftID, replacement.draftID, replacement.userID).Scan(&oldMessageID)
		if err != nil {
			return Message{}, mapSQLError(err)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM mailbox_entries WHERE message_id=? AND user_id=?`, oldMessageID, replacement.userID); err != nil {
			return Message{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND direction='draft'`, oldMessageID); err != nil {
			return Message{}, err
		}
	}
	if s.archiveLimit > 0 {
		var archived int64
		if err := tx.QueryRowContext(ctx, archiveStorageSQL).Scan(&archived); err != nil {
			return Message{}, err
		}
		if archived > s.archiveLimit || storageBytes > s.archiveLimit-archived {
			return Message{}, ErrArchiveFull
		}
	}
	deliveryCounts := make(map[string]int64, len(deliveries))
	for _, delivery := range deliveries {
		deliveryCounts[delivery.UserID]++
	}
	for userID, copies := range deliveryCounts {
		var quota, used int64
		err := tx.QueryRowContext(ctx, `SELECT u.quota_bytes,COALESCE(SUM(CASE WHEN me.expunged_at IS NULL THEN m.size_bytes ELSE 0 END),0)
			FROM users u LEFT JOIN mailbox_entries me ON me.user_id=u.id LEFT JOIN messages m ON m.id=me.message_id
			WHERE u.id=? AND u.status='active' GROUP BY u.id`, userID).Scan(&quota, &used)
		if err != nil {
			return Message{}, mapSQLError(err)
		}
		if quota > 0 && used+message.SizeBytes*copies > quota {
			return Message{}, ErrQuotaExceeded
		}
	}
	envelopeTo, _ := json.Marshal(message.EnvelopeTo)
	headerTo, _ := json.Marshal(message.HeaderTo)
	headerCC, _ := json.Marshal(message.HeaderCC)
	headerBCC, _ := json.Marshal(message.HeaderBCC)
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,rfc_message_id,thread_key,envelope_from,envelope_to_json,
		header_from,header_to_json,header_cc_json,header_bcc_json,reply_to,subject,text_body,html_body,snippet,
		raw,size_bytes,attachment_count,direction,transport_status,sent_at,received_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, message.ID, message.RFCMessageID, message.ThreadKey,
		message.EnvelopeFrom, string(envelopeTo), message.HeaderFrom, string(headerTo), string(headerCC), string(headerBCC),
		message.ReplyTo, message.Subject, message.TextBody, message.HTMLBody, message.Snippet, message.Raw, message.SizeBytes,
		message.AttachmentCount, message.Direction, message.TransportStatus, nullTime(message.SentAt), message.ReceivedAt, message.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	for index := range attachments {
		attachment := &attachments[index]
		if attachment.ID == "" {
			attachment.ID, err = security.NewID("att")
			if err != nil {
				return Message{}, err
			}
		}
		attachment.MessageID = message.ID
		_, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,message_id,filename,content_type,content_id,disposition,size_bytes,sha256,content)
			VALUES(?,?,?,?,?,?,?,?,?)`, attachment.ID, attachment.MessageID, attachment.Filename, attachment.ContentType,
			attachment.ContentID, attachment.Disposition, attachment.SizeBytes, attachment.SHA256, attachment.Content)
		if err != nil {
			return Message{}, err
		}
	}
	for _, delivery := range deliveries {
		if delivery.Mailbox == "" {
			delivery.Mailbox = MailboxInbox
		}
		if err := saveMailboxEntry(ctx, tx, delivery, message.ID, message.ReceivedAt); err != nil {
			return Message{}, err
		}
	}
	for _, recipient := range queueRecipients {
		queueID, idErr := security.NewID("que")
		if idErr != nil {
			return Message{}, idErr
		}
		now := s.now()
		_, err = tx.ExecContext(ctx, `INSERT INTO outgoing_queue(id,message_id,recipient,status,attempts,next_attempt,created_at,updated_at)
			VALUES(?,?,?,'queued',0,?,?,?)`, queueID, message.ID, normalizeEmail(recipient), now, now, now)
		if err != nil {
			return Message{}, mapSQLError(err)
		}
	}
	if event != nil {
		if event.TargetType == "" {
			event.TargetType = "message"
		}
		if event.TargetID == "" {
			event.TargetID = message.ID
		}
		if err := s.insertAudit(ctx, tx, *event); err != nil {
			return Message{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	return message, nil
}

func estimatedMessageStorage(message Message, attachments []Attachment) int64 {
	rawBytes := int64(len(message.Raw))
	if message.SizeBytes > rawBytes {
		rawBytes = message.SizeBytes
	}
	storedBodyBytes := int64(len(message.TextBody) + len(message.HTMLBody) + len(message.Snippet))
	searchableBytes := int64(len(message.TextBody) + len(message.Subject) + len(message.HeaderFrom))
	for _, value := range append(append([]string{}, message.HeaderTo...), message.HeaderCC...) {
		searchableBytes += int64(len(value) + 3)
	}
	total := rawBytes + storedBodyBytes + saturatingMultiply(searchableBytes, 4) + 16384
	for _, attachment := range attachments {
		contentBytes := int64(len(attachment.Content))
		if attachment.SizeBytes > contentBytes {
			contentBytes = attachment.SizeBytes
		}
		if contentBytes > maxInt64-1024 || total > maxInt64-contentBytes-1024 {
			return maxInt64
		}
		total += contentBytes + 1024
	}
	return total
}

func saveMailboxEntry(ctx context.Context, tx *sql.Tx, delivery Delivery, messageID string, internalDate time.Time) error {
	var mailboxID string
	var uidNext int64
	err := tx.QueryRowContext(ctx, `SELECT id,uid_next FROM mailboxes WHERE user_id=? AND name=? COLLATE NOCASE`,
		delivery.UserID, delivery.Mailbox).Scan(&mailboxID, &uidNext)
	if err != nil {
		return mapSQLError(err)
	}
	entryID, err := security.NewID("ent")
	if err != nil {
		return err
	}
	flags, _ := json.Marshal(normalizeFlags(delivery.Flags))
	_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_entries(id,mailbox_id,user_id,message_id,uid,flags_json,internal_date,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, entryID, mailboxID, delivery.UserID, messageID, uidNext, string(flags), internalDate, time.Now().UTC())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE mailboxes SET uid_next=uid_next+1 WHERE id=?`, mailboxID)
	return err
}

func normalizeFlags(flags []string) []string {
	allowed := map[string]string{"\\seen": "\\Seen", "\\answered": "\\Answered", "\\flagged": "\\Flagged", "\\deleted": "\\Deleted", "\\draft": "\\Draft", "\\recent": "\\Recent"}
	seen := map[string]bool{}
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		if canonical, ok := allowed[strings.ToLower(flag)]; ok && !seen[canonical] {
			seen[canonical] = true
			result = append(result, canonical)
		}
	}
	return result
}

func (s *Store) ListMailboxes(ctx context.Context, userID string) ([]MailboxSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mb.name,COUNT(me.id),
		COALESCE(SUM(CASE WHEN instr(lower(me.flags_json),'\\seen')=0 THEN 1 ELSE 0 END),0)
		FROM mailboxes mb LEFT JOIN mailbox_entries me ON me.mailbox_id=mb.id AND me.expunged_at IS NULL
		WHERE mb.user_id=? GROUP BY mb.id ORDER BY CASE mb.name
		WHEN 'INBOX' THEN 0 WHEN 'Drafts' THEN 1 WHEN 'Sent' THEN 2 WHEN 'Archive' THEN 3 WHEN 'Junk' THEN 4 WHEN 'Trash' THEN 5 ELSE 6 END, mb.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MailboxSummary
	for rows.Next() {
		var item MailboxSummary
		if err := rows.Scan(&item.Name, &item.Total, &item.Unread); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

type MessageQuery struct {
	UserID  string
	Mailbox string
	Search  string
	Limit   int
	Offset  int
	Admin   bool
}

func (s *Store) ListMessages(ctx context.Context, query MessageQuery) ([]Message, error) {
	if query.Limit < 1 || query.Limit > 200 {
		query.Limit = 50
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	args := []any{}
	where := []string{}
	join := ""
	orderBy := "m.received_at DESC,me.id DESC"
	snippetProjection := "m.snippet"
	selectUser := `,'' as mailbox,'[]' as flags,NULL as deleted_at,NULL as expunged_at,'' as entry_id,0 as uid`
	if query.Admin {
		where = append(where, "m.direction<>'draft'")
		snippetProjection = "''"
		orderBy = "m.created_at DESC,m.id DESC"
	} else {
		join = ` JOIN mailbox_entries me ON me.message_id=m.id JOIN mailboxes mb ON mb.id=me.mailbox_id`
		where = append(where, "me.user_id=?", "me.expunged_at IS NULL")
		args = append(args, query.UserID)
		if query.Mailbox != "" && query.Mailbox != "Starred" {
			where = append(where, "mb.name=? COLLATE NOCASE")
			args = append(args, query.Mailbox)
		}
		if query.Mailbox == "Starred" {
			where = append(where, `instr(lower(me.flags_json),'\\flagged')>0`)
		}
		selectUser = `,mb.name,me.flags_json,me.deleted_at,me.expunged_at,me.id,me.uid`
	}
	if strings.TrimSpace(query.Search) != "" {
		if query.Admin {
			// The administrator archive index is deliberately metadata-only. A
			// body search would let an administrator infer message contents one
			// keyword at a time without passing through the reason-gated,
			// audited detail endpoint.
			pattern := "%" + escapeLike(strings.ToLower(strings.TrimSpace(query.Search))) + "%"
			where = append(where, `(lower(m.subject) LIKE ? ESCAPE '\' OR lower(m.header_from) LIKE ? ESCAPE '\'
				OR lower(m.header_to_json) LIKE ? ESCAPE '\' OR lower(m.header_cc_json) LIKE ? ESCAPE '\')`)
			args = append(args, pattern, pattern, pattern, pattern)
		} else {
			join += ` JOIN message_search ON message_search.message_id=m.id`
			where = append(where, "message_search MATCH ?")
			args = append(args, ftsQuery(query.Search))
		}
	}
	if len(where) == 0 {
		where = append(where, "1=1")
	}
	// A list request must never materialize full bodies or raw MIME. At the
	// maximum page size that could otherwise allocate several gigabytes.
	sqlText := `SELECT m.id,m.rfc_message_id,m.thread_key,m.envelope_from,m.envelope_to_json,m.header_from,
		m.header_to_json,m.header_cc_json,m.header_bcc_json,m.reply_to,m.subject,'' AS text_body,'' AS html_body,` + snippetProjection + `,
		zeroblob(0) AS raw,m.size_bytes,m.attachment_count,m.direction,m.transport_status,m.sent_at,m.received_at,m.created_at` +
		selectUser + ` FROM messages m` + join + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
	args = append(args, query.Limit, query.Offset)
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Message
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		message.Raw = nil
		if query.Admin {
			// Archive search is metadata-only. Reading body content requires the
			// audited, reason-gated archive detail endpoint.
			message.Snippet = ""
			message.EnvelopeTo = nil
			message.HeaderBCC = nil
		} else {
			// SMTP envelope recipients can include Bcc addresses that must not be
			// disclosed to another recipient's mailbox view.
			message.EnvelopeTo = nil
			message.HeaderBCC = nil
		}
		result = append(result, message)
	}
	return result, rows.Err()
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func (s *Store) GetMessage(ctx context.Context, userID, messageID string, admin bool) (Message, error) {
	join := ""
	where := `m.id=?`
	args := []any{messageID}
	selectUser := `,'' as mailbox,'[]' as flags,NULL as deleted_at,NULL as expunged_at,'' as entry_id,0 as uid`
	orderColumn := "m.created_at"
	if admin {
		where += ` AND m.direction<>'draft'`
	} else {
		join = ` JOIN mailbox_entries me ON me.message_id=m.id JOIN mailboxes mb ON mb.id=me.mailbox_id`
		where = `(m.id=? OR me.id=?) AND me.user_id=? AND me.expunged_at IS NULL`
		args = []any{messageID, messageID, userID}
		selectUser = `,mb.name,me.flags_json,me.deleted_at,me.expunged_at,me.id,me.uid`
		orderColumn = "me.created_at"
	}
	row := s.db.QueryRowContext(ctx, `SELECT m.id,m.rfc_message_id,m.thread_key,m.envelope_from,m.envelope_to_json,m.header_from,
		m.header_to_json,m.header_cc_json,m.header_bcc_json,m.reply_to,m.subject,m.text_body,m.html_body,m.snippet,
		m.raw,m.size_bytes,m.attachment_count,m.direction,m.transport_status,m.sent_at,m.received_at,m.created_at`+
		selectUser+` FROM messages m`+join+` WHERE `+where+` ORDER BY `+orderColumn+` DESC LIMIT 1`, args...)
	message, err := scanMessage(row)
	if err != nil {
		return Message{}, err
	}
	if !admin {
		message.EnvelopeTo = nil
		message.HeaderBCC = nil
	}
	return message, nil
}

func scanMessage(scanner interface{ Scan(...any) error }) (Message, error) {
	var item Message
	var envelopeTo, headerTo, headerCC, headerBCC, flags string
	var sentAt, deletedAt, expungedAt sql.NullTime
	err := scanner.Scan(&item.ID, &item.RFCMessageID, &item.ThreadKey, &item.EnvelopeFrom, &envelopeTo, &item.HeaderFrom,
		&headerTo, &headerCC, &headerBCC, &item.ReplyTo, &item.Subject, &item.TextBody, &item.HTMLBody, &item.Snippet, &item.Raw,
		&item.SizeBytes, &item.AttachmentCount, &item.Direction, &item.TransportStatus, &sentAt, &item.ReceivedAt, &item.CreatedAt,
		&item.UserMailbox, &flags, &deletedAt, &expungedAt, &item.MailboxEntryID, &item.UID)
	if err != nil {
		return Message{}, mapSQLError(err)
	}
	_ = json.Unmarshal([]byte(envelopeTo), &item.EnvelopeTo)
	_ = json.Unmarshal([]byte(headerTo), &item.HeaderTo)
	_ = json.Unmarshal([]byte(headerCC), &item.HeaderCC)
	_ = json.Unmarshal([]byte(headerBCC), &item.HeaderBCC)
	_ = json.Unmarshal([]byte(flags), &item.UserFlags)
	if sentAt.Valid {
		item.SentAt = &sentAt.Time
	}
	if deletedAt.Valid {
		item.UserDeletedAt = &deletedAt.Time
	}
	if expungedAt.Valid {
		item.UserExpungedAt = &expungedAt.Time
	}
	return item, nil
}

func (s *Store) ListAttachments(ctx context.Context, messageID string) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,message_id,filename,content_type,content_id,disposition,size_bytes,sha256
		FROM attachments WHERE message_id=? ORDER BY rowid`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.ContentID, &a.Disposition, &a.SizeBytes, &a.SHA256); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) GetAttachment(ctx context.Context, userID, attachmentID string, admin bool) (Attachment, error) {
	join := ""
	where := "a.id=?"
	args := []any{attachmentID}
	if !admin {
		join = ` JOIN mailbox_entries me ON me.message_id=a.message_id`
		where += ` AND me.user_id=? AND me.expunged_at IS NULL`
		args = append(args, userID)
	}
	var a Attachment
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.message_id,a.filename,a.content_type,a.content_id,a.disposition,a.size_bytes,a.sha256,a.content FROM attachments a`+join+` WHERE `+where+` LIMIT 1`, args...).Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.ContentID, &a.Disposition, &a.SizeBytes, &a.SHA256, &a.Content)
	return a, mapSQLError(err)
}

func (s *Store) UpdateFlags(ctx context.Context, userID, entryOrMessageID string, flags []string) error {
	encoded, _ := json.Marshal(normalizeFlags(flags))
	result, err := s.db.ExecContext(ctx, `UPDATE mailbox_entries SET flags_json=? WHERE id=(
		SELECT id FROM mailbox_entries WHERE user_id=? AND expunged_at IS NULL AND (id=? OR message_id=?) ORDER BY created_at DESC LIMIT 1
	)`, string(encoded), userID, entryOrMessageID, entryOrMessageID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MoveMessage(ctx context.Context, userID, entryOrMessageID, mailbox string) error {
	mailbox = strings.TrimSpace(mailbox)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var entryID, sourceMailboxID string
	if err = tx.QueryRowContext(ctx, `SELECT id,mailbox_id FROM mailbox_entries WHERE user_id=? AND expunged_at IS NULL
		AND (id=? OR message_id=?) ORDER BY created_at DESC LIMIT 1`, userID, entryOrMessageID, entryOrMessageID).Scan(&entryID, &sourceMailboxID); err != nil {
		return mapSQLError(err)
	}
	var targetMailboxID string
	var targetUID int64
	if err = tx.QueryRowContext(ctx, `SELECT id,uid_next FROM mailboxes WHERE user_id=? AND name=? COLLATE NOCASE`, userID, mailbox).Scan(&targetMailboxID, &targetUID); err != nil {
		return mapSQLError(err)
	}
	now := s.now()
	if sourceMailboxID == targetMailboxID {
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_entries SET deleted_at=CASE WHEN ?='Trash' THEN ? ELSE NULL END WHERE id=?`, mailbox, now, entryID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_entries SET mailbox_id=?,uid=?,deleted_at=CASE WHEN ?='Trash' THEN ? ELSE NULL END WHERE id=?`,
			targetMailboxID, targetUID, mailbox, now, entryID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE mailboxes SET uid_next=uid_next+1 WHERE id=?`, targetMailboxID)
		}
	}
	if err != nil {
		return mapSQLError(err)
	}
	return tx.Commit()
}

func (s *Store) ExpungeMessage(ctx context.Context, userID, entryOrMessageID string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var entryID, messageID, direction string
	if err = tx.QueryRowContext(ctx, `SELECT me.id,m.id,m.direction FROM messages m JOIN mailbox_entries me ON me.message_id=m.id
		WHERE me.user_id=? AND me.expunged_at IS NULL AND (me.id=? OR m.id=?) ORDER BY me.created_at DESC LIMIT 1`,
		userID, entryOrMessageID, entryOrMessageID).Scan(&entryID, &messageID, &direction); err != nil {
		return mapSQLError(err)
	}
	if direction == "draft" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM mailbox_entries WHERE id=? AND user_id=?`, entryID, userID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND direction='draft'
			AND NOT EXISTS (SELECT 1 FROM mailbox_entries WHERE message_id=messages.id)`, messageID); err != nil {
			return err
		}
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE mailbox_entries SET expunged_at=?,deleted_at=COALESCE(deleted_at,?) WHERE id=? AND user_id=? AND expunged_at IS NULL`, now, now, entryID, userID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) CreateMailbox(ctx context.Context, userID, name string) error {
	name, err := validMailboxName(name)
	if err != nil {
		return err
	}
	id, err := security.NewID("mbx")
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count, maxValidity int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(uid_validity),?) FROM mailboxes WHERE user_id=?`, s.now().Unix(), userID).Scan(&count, &maxValidity); err != nil {
		return err
	}
	if count >= maxMailboxesPerUser {
		return ErrResourceLimit
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO mailboxes(id,user_id,name,uid_validity,uid_next,subscribed,created_at) VALUES(?,?,?,?,1,1,?)`, id, userID, name, maxValidity+1, s.now()); err != nil {
		return mapSQLError(err)
	}
	return tx.Commit()
}

func (s *Store) RenameMailbox(ctx context.Context, userID, oldName, newName string) error {
	if strings.EqualFold(oldName, MailboxInbox) {
		return ErrForbidden
	}
	newName, err := validMailboxName(newName)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE mailboxes SET name=? WHERE user_id=? AND name=? COLLATE NOCASE`, newName, userID, oldName)
	if err != nil {
		return mapSQLError(err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func validMailboxName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("mailbox name is required")
	}
	if len(name) > maxMailboxNameBytes {
		return "", fmt.Errorf("mailbox name exceeds %d bytes", maxMailboxNameBytes)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errors.New("mailbox name contains control characters")
	}
	return name, nil
}

func (s *Store) DeleteMailbox(ctx context.Context, userID, name string) error {
	return s.IMAPDeleteMailbox(ctx, userID, name)
}

func (s *Store) SetMailboxSubscribed(ctx context.Context, userID, name string, subscribed bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE mailboxes SET subscribed=? WHERE user_id=? AND name=? COLLATE NOCASE`, subscribed, userID, name)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

func ftsQuery(value string) string {
	words := strings.Fields(value)
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.ReplaceAll(word, `"`, `""`)
		quoted = append(quoted, `"`+word+`"*`)
	}
	if len(quoted) == 0 {
		return `""`
	}
	return strings.Join(quoted, " AND ")
}
func (s *Store) QueueReady(ctx context.Context, limit int) ([]QueueItem, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,message_id,recipient,status,attempts,next_attempt,last_error,delivered_at,created_at,updated_at FROM outgoing_queue WHERE status='queued' AND next_attempt<=? ORDER BY next_attempt LIMIT ?`, s.now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanQueue(rows)
}
func (s *Store) ListQueue(ctx context.Context, limit int) ([]QueueItem, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,message_id,recipient,status,attempts,next_attempt,last_error,delivered_at,created_at,updated_at FROM outgoing_queue ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanQueue(rows)
}
func scanQueue(rows *sql.Rows) ([]QueueItem, error) {
	var result []QueueItem
	for rows.Next() {
		var item QueueItem
		var delivered sql.NullTime
		if err := rows.Scan(&item.ID, &item.MessageID, &item.Recipient, &item.Status, &item.Attempts, &item.NextAttempt, &item.LastError, &delivered, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if delivered.Valid {
			item.DeliveredAt = &delivered.Time
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (s *Store) ClaimQueue(ctx context.Context, id string) bool {
	result, err := s.db.ExecContext(ctx, `UPDATE outgoing_queue SET status='delivering',attempts=attempts+1,updated_at=? WHERE id=? AND status='queued'`, s.now(), id)
	if err != nil {
		return false
	}
	rows, _ := result.RowsAffected()
	return rows == 1
}
func (s *Store) CompleteQueue(ctx context.Context, id string) error {
	now := s.now()
	_, err := s.db.ExecContext(ctx, `UPDATE outgoing_queue SET status='delivered',delivered_at=?,updated_at=?,last_error='' WHERE id=?`, now, now, id)
	return err
}
func (s *Store) RetryQueue(ctx context.Context, id string, attempts, maxAttempts int, lastError string) error {
	status := "queued"
	if attempts >= maxAttempts {
		status = "failed"
	}
	// RFC 5321 recommends a retry interval of at least 30 minutes and a
	// give-up window of at least 4–5 days. Twelve attempts with this capped
	// exponential schedule span more than seven days.
	exponent := min(max(attempts-1, 0), 6)
	delay := 30 * time.Minute * time.Duration(1<<exponent)
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outgoing_queue SET status=?,next_attempt=?,last_error=?,updated_at=? WHERE id=?`, status, s.now().Add(delay), truncate(lastError, 2000), s.now(), id)
	return err
}
func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n]
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Store) MessageRaw(ctx context.Context, id string) (Message, error) {
	item, err := s.GetMessage(ctx, "", id, true)
	if err != nil {
		return Message{}, err
	}
	return item, nil
}
