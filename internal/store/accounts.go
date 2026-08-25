package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Brclio/brclio-mail/internal/security"
)

const (
	maxActiveWebSessions = 20
	maxSessionIPBytes    = 64
	maxUserAgentBytes    = 512
)

func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

func (s *Store) CreateUser(ctx context.Context, email, displayName, passwordHash, role string, quotaBytes int64) (User, error) {
	return s.createUser(ctx, email, displayName, passwordHash, role, quotaBytes, nil)
}

func (s *Store) CreateUserAudited(ctx context.Context, email, displayName, passwordHash, role string, quotaBytes int64, event AuditEvent) (User, error) {
	return s.createUser(ctx, email, displayName, passwordHash, role, quotaBytes, &event)
}

func (s *Store) createUser(ctx context.Context, email, displayName, passwordHash, role string, quotaBytes int64, event *AuditEvent) (User, error) {
	email = normalizeEmail(email)
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(parts[0], " <>(),:;\\\"") {
		return User{}, fmt.Errorf("invalid email address")
	}
	if role != RoleAdmin && role != RoleUser {
		return User{}, fmt.Errorf("invalid role")
	}
	if quotaBytes < 0 {
		return User{}, fmt.Errorf("quota must be non-negative")
	}
	domain, err := s.GetDomainByName(ctx, parts[1])
	if err != nil {
		return User{}, fmt.Errorf("domain: %w", err)
	}
	id, err := security.NewID("usr")
	if err != nil {
		return User{}, err
	}
	now := s.now()
	user := User{ID: id, DomainID: domain.ID, LocalPart: parts[0], Email: email,
		DisplayName: strings.TrimSpace(displayName), PasswordHash: passwordHash, Role: role,
		Status: StatusActive, AuthVersion: 1, ProtocolAuthVersion: 1,
		QuotaBytes: quotaBytes, CreatedAt: now, UpdatedAt: now}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO users
		(id,domain_id,local_part,email,display_name,password_hash,role,status,auth_version,protocol_auth_version,quota_bytes,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, user.ID, user.DomainID, user.LocalPart, user.Email, user.DisplayName,
		user.PasswordHash, user.Role, user.Status, user.AuthVersion, user.ProtocolAuthVersion,
		user.QuotaBytes, user.CreatedAt, user.UpdatedAt)
	if err != nil {
		return User{}, mapSQLError(err)
	}
	for index, name := range SystemMailboxes {
		mailboxID, err := security.NewID("mbx")
		if err != nil {
			return User{}, err
		}
		uidValidity := now.Unix() + int64(index)
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailboxes(id,user_id,name,uid_validity,uid_next,subscribed,created_at)
			VALUES(?,?,?,?,1,1,?)`, mailboxID, user.ID, name, uidValidity, now); err != nil {
			return User{}, err
		}
	}
	if event != nil {
		if event.TargetType == "" {
			event.TargetType = "user"
		}
		if event.TargetID == "" {
			event.TargetID = user.ID
		}
		if err := s.insertAudit(ctx, tx, *event); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func scanUser(scanner interface{ Scan(...any) error }) (User, error) {
	var user User
	var lastLogin sql.NullTime
	err := scanner.Scan(&user.ID, &user.DomainID, &user.LocalPart, &user.Email, &user.DisplayName,
		&user.PasswordHash, &user.Role, &user.Status, &user.AuthVersion, &user.ProtocolAuthVersion,
		&user.QuotaBytes, &user.CreatedAt, &user.UpdatedAt, &lastLogin, &user.UsedBytes)
	if err != nil {
		return User{}, mapSQLError(err)
	}
	if lastLogin.Valid {
		user.LastLoginAt = &lastLogin.Time
	}
	return user, nil
}

const userSelect = `SELECT u.id,u.domain_id,u.local_part,u.email,u.display_name,u.password_hash,u.role,u.status,
	u.auth_version,u.protocol_auth_version,u.quota_bytes,u.created_at,u.updated_at,u.last_login_at,
	COALESCE((SELECT SUM(m.size_bytes) FROM mailbox_entries me JOIN messages m ON m.id=me.message_id
	 WHERE me.user_id=u.id AND me.expunged_at IS NULL),0) AS used_bytes FROM users u`

func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE u.email=?`, normalizeEmail(email)))
}

func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userSelect+` WHERE u.id=?`, id))
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, userSelect+` ORDER BY u.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

type UserUpdate struct {
	DisplayName  *string
	Status       *string
	Role         *string
	QuotaBytes   *int64
	PasswordHash *string
}

func (s *Store) UpdateUser(ctx context.Context, id string, update UserUpdate) error {
	return s.updateUser(ctx, id, update, nil)
}

func (s *Store) UpdateUserAudited(ctx context.Context, id string, update UserUpdate, event AuditEvent) error {
	return s.updateUser(ctx, id, update, &event)
}

func (s *Store) updateUser(ctx context.Context, id string, update UserUpdate, event *AuditEvent) error {
	sets := []string{"updated_at=?"}
	args := []any{s.now()}
	if update.DisplayName != nil {
		sets = append(sets, "display_name=?")
		args = append(args, strings.TrimSpace(*update.DisplayName))
	}
	if update.Status != nil {
		if *update.Status != StatusActive && *update.Status != StatusSuspended {
			return fmt.Errorf("invalid status")
		}
		sets = append(sets, "status=?")
		args = append(args, *update.Status)
	}
	if update.Role != nil {
		if *update.Role != RoleAdmin && *update.Role != RoleUser {
			return fmt.Errorf("invalid role")
		}
		sets = append(sets, "role=?")
		args = append(args, *update.Role)
	}
	if update.QuotaBytes != nil {
		if *update.QuotaBytes < 0 {
			return fmt.Errorf("invalid quota")
		}
		sets = append(sets, "quota_bytes=?")
		args = append(args, *update.QuotaBytes)
	}
	if update.PasswordHash != nil {
		sets = append(sets, "password_hash=?")
		args = append(args, *update.PasswordHash)
	}
	if update.PasswordHash != nil || update.Status != nil {
		sets = append(sets, "auth_version=auth_version+1", "protocol_auth_version=protocol_auth_version+1")
	}
	args = append(args, id)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if update.Role != nil || update.Status != nil {
		var currentRole, currentStatus string
		if err = tx.QueryRowContext(ctx, `SELECT role,status FROM users WHERE id=?`, id).Scan(&currentRole, &currentStatus); err != nil {
			return mapSQLError(err)
		}
		nextRole, nextStatus := currentRole, currentStatus
		if update.Role != nil {
			nextRole = *update.Role
		}
		if update.Status != nil {
			nextStatus = *update.Status
		}
		if currentRole == RoleAdmin && currentStatus == StatusActive && (nextRole != RoleAdmin || nextStatus != StatusActive) {
			var activeAdmins int64
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&activeAdmins); err != nil {
				return err
			}
			if activeAdmins <= 1 {
				return ErrForbidden
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	if update.PasswordHash != nil || (update.Status != nil && *update.Status == StatusSuspended) {
		if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE app_passwords SET revoked_at=COALESCE(revoked_at,?) WHERE user_id=?`, s.now(), id); err != nil {
			return err
		}
	}
	if event != nil {
		if event.TargetType == "" {
			event.TargetType = "user"
		}
		if event.TargetID == "" {
			event.TargetID = id
		}
		if err = s.insertAudit(ctx, tx, *event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Authenticate(ctx context.Context, email, password string, allowAppPassword bool) (User, error) {
	user, err := s.GetUserByEmail(ctx, email)
	if err != nil || user.Status != StatusActive {
		return User{}, ErrInvalidCredentials
	}
	valid := security.VerifyPasswordContext(ctx, user.PasswordHash, password)
	if !valid && allowAppPassword {
		hash := security.TokenHash(password)
		var appPasswordID string
		err = s.db.QueryRowContext(ctx, `SELECT id FROM app_passwords WHERE user_id=? AND secret_hash=? AND revoked_at IS NULL`, user.ID, hash).Scan(&appPasswordID)
		if err == nil {
			valid = true
			_, _ = s.db.ExecContext(ctx, `UPDATE app_passwords SET last_used_at=? WHERE id=?`, s.now(), appPasswordID)
		}
	}
	if !valid {
		return User{}, ErrInvalidCredentials
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, s.now(), user.ID)
	return user, nil
}

func (s *Store) ResolveRecipient(ctx context.Context, address string) (User, error) {
	address = normalizeEmail(address)
	user, err := s.GetUserByEmail(ctx, address)
	if err == nil && user.Status == StatusActive {
		return user, nil
	}
	return scanUser(s.db.QueryRowContext(ctx, userSelect+`
		JOIN aliases a ON a.target_user_id=u.id WHERE a.address=? AND a.enabled=1 AND u.status='active'`, address))
}

func (s *Store) CreateSession(ctx context.Context, userID string, expectedVersion int64, tokenHash, ip, userAgent string, expiresAt time.Time) (Session, error) {
	id, err := security.NewID("ses")
	if err != nil {
		return Session{}, err
	}
	now := s.now()
	ip = truncateUTF8(strings.TrimSpace(ip), maxSessionIPBytes)
	userAgent = truncateUTF8(strings.TrimSpace(userAgent), maxUserAgentBytes)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, now); err != nil {
		return Session{}, err
	}
	// Keep room for this session by evicting the oldest rows beyond the newest
	// maxActiveWebSessions-1. This bounds metadata even if a client repeatedly
	// logs in without explicitly logging out.
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id IN (
		SELECT id FROM sessions WHERE user_id=? ORDER BY last_seen_at DESC,created_at DESC,id DESC LIMIT -1 OFFSET ?
	)`, userID, maxActiveWebSessions-1); err != nil {
		return Session{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO sessions
		(id,user_id,token_hash,auth_version,created_at,expires_at,last_seen_at,ip,user_agent)
		SELECT ?,u.id,?,u.auth_version,?,?,?,?,? FROM users u
		WHERE u.id=? AND u.status='active' AND u.auth_version=?`, id, tokenHash, now, expiresAt.UTC(), now, ip, userAgent,
		userID, expectedVersion)
	if err != nil {
		return Session{}, mapSQLError(err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return Session{}, ErrInvalidCredentials
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	return Session{ID: id, UserID: userID, TokenHash: tokenHash, AuthVersion: expectedVersion, ExpiresAt: expiresAt.UTC()}, nil
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes < 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Store) UserForSession(ctx context.Context, tokenHash string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, userSelect+`
		JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=? AND s.expires_at>? AND u.status='active'
		AND s.auth_version=u.auth_version`, tokenHash, s.now()))
	if err != nil {
		return User{}, ErrInvalidCredentials
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE token_hash=?`, s.now(), tokenHash)
	return user, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, s.now())
	return err
}

func (s *Store) CreateAppPassword(ctx context.Context, userID, name, hash string) (AppPassword, error) {
	id, err := security.NewID("app")
	if err != nil {
		return AppPassword{}, err
	}
	now := s.now()
	item := AppPassword{ID: id, UserID: userID, Name: strings.TrimSpace(name), SecretHash: hash, CreatedAt: now}
	if item.Name == "" {
		return AppPassword{}, fmt.Errorf("name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppPassword{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO app_passwords(id,user_id,name,secret_hash,created_at) VALUES(?,?,?,?,?)`,
		item.ID, item.UserID, item.Name, item.SecretHash, item.CreatedAt); err != nil {
		return AppPassword{}, mapSQLError(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET updated_at=? WHERE id=?`, now, userID)
	if err != nil {
		return AppPassword{}, err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return AppPassword{}, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return AppPassword{}, err
	}
	return item, nil
}

func (s *Store) ListAppPasswords(ctx context.Context, userID string) ([]AppPassword, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,name,secret_hash,created_at,last_used_at,revoked_at
		FROM app_passwords WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AppPassword
	for rows.Next() {
		var item AppPassword
		var lastUsed, revoked sql.NullTime
		if err := rows.Scan(&item.ID, &item.UserID, &item.Name, &item.SecretHash, &item.CreatedAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			item.LastUsedAt = &lastUsed.Time
		}
		if revoked.Valid {
			item.RevokedAt = &revoked.Time
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) RevokeAppPassword(ctx context.Context, userID, id string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE app_passwords SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, now, id, userID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET protocol_auth_version=protocol_auth_version+1,updated_at=? WHERE id=?`, now, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateAlias(ctx context.Context, address, targetEmail string) (Alias, error) {
	return s.createAlias(ctx, address, targetEmail, nil)
}

func (s *Store) CreateAliasAudited(ctx context.Context, address, targetEmail string, event AuditEvent) (Alias, error) {
	return s.createAlias(ctx, address, targetEmail, &event)
}

func (s *Store) createAlias(ctx context.Context, address, targetEmail string, event *AuditEvent) (Alias, error) {
	address = normalizeEmail(address)
	target, err := s.GetUserByEmail(ctx, targetEmail)
	if err != nil {
		return Alias{}, err
	}
	parts := strings.Split(address, "@")
	if len(parts) != 2 || parts[0] == "" {
		return Alias{}, fmt.Errorf("invalid alias")
	}
	if _, err := s.GetDomainByName(ctx, parts[1]); err != nil {
		return Alias{}, fmt.Errorf("alias domain: %w", err)
	}
	id, err := security.NewID("als")
	if err != nil {
		return Alias{}, err
	}
	item := Alias{ID: id, Address: address, Target: target.Email, Enabled: true, CreatedAt: s.now()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Alias{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO aliases(id,address,target_user_id,enabled,created_at) VALUES(?,?,?,1,?)`,
		item.ID, item.Address, target.ID, item.CreatedAt)
	if err != nil {
		return Alias{}, mapSQLError(err)
	}
	if event != nil {
		if event.TargetType == "" {
			event.TargetType = "alias"
		}
		if event.TargetID == "" {
			event.TargetID = item.ID
		}
		if err = s.insertAudit(ctx, tx, *event); err != nil {
			return Alias{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Alias{}, err
	}
	return item, nil
}

func (s *Store) ListAliases(ctx context.Context) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.address,u.email,a.enabled,a.created_at FROM aliases a
		JOIN users u ON u.id=a.target_user_id ORDER BY a.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Alias
	for rows.Next() {
		var item Alias
		if err := rows.Scan(&item.ID, &item.Address, &item.Target, &item.Enabled, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) EnsurePostmaster(ctx context.Context, domainName, passwordHash string, quotaBytes int64) (User, error) {
	email := "postmaster@" + normalizeEmail(domainName)
	user, err := s.GetUserByEmail(ctx, email)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return User{}, err
	}
	return s.CreateUser(ctx, email, "Postmaster", passwordHash, RoleAdmin, quotaBytes)
}
