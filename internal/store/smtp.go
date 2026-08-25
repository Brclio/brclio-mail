package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

var roleLocalParts = map[string]struct{}{
	"postmaster": {},
	"abuse":      {},
	"security":   {},
	"hostmaster": {},
}

// ResolveSMTPRecipient canonicalizes a local SMTP recipient and resolves it to
// an active user. RFC 5321's domain-less Postmaster form is mapped to the first
// configured local domain. Role local-parts are always compared case
// insensitively, as are users and aliases in the schema.
func (s *Store) ResolveSMTPRecipient(ctx context.Context, value string) (string, User, error) {
	address, err := smtpAddress(value)
	if err != nil {
		return "", User{}, err
	}
	if !strings.Contains(address, "@") {
		if !strings.EqualFold(address, "postmaster") {
			return "", User{}, ErrNotFound
		}
		var domain string
		if err := s.db.QueryRowContext(ctx, `SELECT name FROM domains WHERE status='verified' ORDER BY created_at,id LIMIT 1`).Scan(&domain); err != nil {
			return "", User{}, mapSQLError(err)
		}
		address = "postmaster@" + strings.ToLower(domain)
	}

	local, domain, ok := strings.Cut(address, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", User{}, fmt.Errorf("invalid SMTP recipient")
	}
	local = strings.ToLower(local)
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	if _, isRole := roleLocalParts[local]; isRole {
		address = local + "@" + domain
	} else {
		address = strings.ToLower(local + "@" + domain)
	}

	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM domains WHERE name=? COLLATE NOCASE AND status='verified'`, domain).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return address, User{}, ErrNotFound
		}
		return address, User{}, err
	}
	user, err := s.ResolveRecipient(ctx, address)
	if err != nil {
		return address, User{}, err
	}
	return address, user, nil
}

// SMTPFromAllowed reports whether address belongs to userID either as the
// primary address or as an enabled alias. It deliberately rejects the null
// reverse-path for authenticated submission.
func (s *Store) SMTPFromAllowed(ctx context.Context, userID, value string) (bool, error) {
	address, err := smtpAddress(value)
	if err != nil || address == "" || !strings.Contains(address, "@") {
		return false, nil
	}
	address = strings.ToLower(strings.TrimSuffix(address, "."))
	_, domainName, ok := strings.Cut(address, "@")
	if !ok {
		return false, nil
	}
	var domainVerified int
	if err = s.db.QueryRowContext(ctx, `SELECT 1 FROM domains WHERE name=? COLLATE NOCASE AND status='verified'`, domainName).Scan(&domainVerified); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var allowed int
	err = s.db.QueryRowContext(ctx, `SELECT 1 WHERE EXISTS(
			SELECT 1 FROM users u WHERE u.id=? AND u.email=? COLLATE NOCASE AND u.status='active'
		) OR EXISTS(
			SELECT 1 FROM aliases a JOIN users u ON u.id=a.target_user_id
			WHERE u.id=? AND a.address=? COLLATE NOCASE AND a.enabled=1 AND u.status='active'
	)`, userID, address, userID, address).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && allowed == 1, err
}

// SMTPLocalDomain reports whether an address has a syntactically valid domain
// managed by this store. It does not assert that the local-part is deliverable.
func (s *Store) SMTPLocalDomain(ctx context.Context, value string) (string, bool, error) {
	address, err := smtpAddress(value)
	if err != nil {
		return "", false, err
	}
	_, domain, ok := strings.Cut(address, "@")
	if !ok || domain == "" || strings.Contains(domain, "@") {
		return address, false, nil
	}
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	var exists int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM domains WHERE name=? COLLATE NOCASE`, domain).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return strings.ToLower(address), false, nil
	}
	return strings.ToLower(address), err == nil && exists == 1, err
}

func smtpAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	if strings.EqualFold(value, "postmaster") {
		return "postmaster", nil
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil {
		return "", fmt.Errorf("invalid SMTP address: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(parsed.Address)), nil
}
