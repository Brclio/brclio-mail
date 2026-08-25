package store

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/Brclio/brclio-mail/internal/security"
)

type InitialSetup struct {
	DomainName     string
	DKIMSelector   string
	DKIMPublicKey  string
	DKIMPrivateKey string
	Email          string
	DisplayName    string
	PasswordHash   string
	QuotaBytes     int64
	IP             string
}

// Initialize creates the first domain, administrator, system mailboxes, role
// aliases and audit record in one transaction. The SQLite connection uses
// BEGIN IMMEDIATE, so two processes sharing the same local database cannot both
// claim first-run setup.
func (s *Store) Initialize(ctx context.Context, input InitialSetup) (User, Domain, error) {
	domainName := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(input.DomainName)), ".")
	if domainName == "" || !strings.Contains(domainName, ".") || strings.ContainsAny(domainName, " @/\\") {
		return User{}, Domain{}, fmt.Errorf("invalid domain name")
	}
	email := normalizeEmail(input.Email)
	parsed, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(parsed.Address, email) {
		return User{}, Domain{}, fmt.Errorf("invalid administrator email")
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[0] == "" || !strings.EqualFold(parts[1], domainName) {
		return User{}, Domain{}, fmt.Errorf("administrator email must belong to the configured domain")
	}
	if input.QuotaBytes < 0 {
		return User{}, Domain{}, fmt.Errorf("quota must be non-negative")
	}
	selector := strings.TrimSpace(input.DKIMSelector)
	if selector == "" {
		selector = "brclio"
	}
	domainID, err := security.NewID("dom")
	if err != nil {
		return User{}, Domain{}, err
	}
	userID, err := security.NewID("usr")
	if err != nil {
		return User{}, Domain{}, err
	}
	verification, err := security.RandomToken(24)
	if err != nil {
		return User{}, Domain{}, err
	}
	now := s.now()
	domain := Domain{ID: domainID, Name: domainName, Status: "pending", Verification: verification,
		DKIMSelector: selector, DKIMPublicKey: input.DKIMPublicKey, DKIMPrivateKey: input.DKIMPrivateKey, CreatedAt: now}
	user := User{ID: userID, DomainID: domain.ID, LocalPart: parts[0], Email: email, DisplayName: strings.TrimSpace(input.DisplayName),
		PasswordHash: input.PasswordHash, Role: RoleAdmin, Status: StatusActive, AuthVersion: 1, ProtocolAuthVersion: 1,
		QuotaBytes: input.QuotaBytes, CreatedAt: now, UpdatedAt: now}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, Domain{}, err
	}
	defer tx.Rollback()
	var count int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return User{}, Domain{}, err
	}
	if count != 0 {
		return User{}, Domain{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO domains
		(id,name,status,verification_token,dkim_selector,dkim_public_key,dkim_private_key,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, domain.ID, domain.Name, domain.Status, domain.Verification, domain.DKIMSelector,
		domain.DKIMPublicKey, domain.DKIMPrivateKey, domain.CreatedAt); err != nil {
		return User{}, Domain{}, mapSQLError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO users
		(id,domain_id,local_part,email,display_name,password_hash,role,status,auth_version,protocol_auth_version,quota_bytes,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, user.ID, user.DomainID, user.LocalPart, user.Email, user.DisplayName,
		user.PasswordHash, user.Role, user.Status, user.AuthVersion, user.ProtocolAuthVersion,
		user.QuotaBytes, user.CreatedAt, user.UpdatedAt); err != nil {
		return User{}, Domain{}, mapSQLError(err)
	}
	for index, name := range SystemMailboxes {
		mailboxID, idErr := security.NewID("mbx")
		if idErr != nil {
			return User{}, Domain{}, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO mailboxes(id,user_id,name,uid_validity,uid_next,subscribed,created_at)
			VALUES(?,?,?,?,1,1,?)`, mailboxID, user.ID, name, now.Unix()+int64(index), now); err != nil {
			return User{}, Domain{}, err
		}
	}
	for _, role := range []string{"postmaster", "abuse", "security", "hostmaster", "dmarc", "tlsrpt"} {
		address := role + "@" + domain.Name
		if strings.EqualFold(address, user.Email) {
			continue
		}
		aliasID, idErr := security.NewID("als")
		if idErr != nil {
			return User{}, Domain{}, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO aliases(id,address,target_user_id,enabled,created_at) VALUES(?,?,?,1,?)`,
			aliasID, address, user.ID, now); err != nil {
			return User{}, Domain{}, mapSQLError(err)
		}
	}
	auditID, err := security.NewID("aud")
	if err != nil {
		return User{}, Domain{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_log
		(id,actor_id,action,target_type,target_id,reason,metadata_json,ip,created_at) VALUES(?,?,?,?,?,'','{}',?,?)`,
		auditID, user.ID, "system.setup", "domain", domain.ID, input.IP, now); err != nil {
		return User{}, Domain{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, Domain{}, err
	}
	return user, domain, nil
}
