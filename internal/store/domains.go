package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Brclio/brclio-mail/internal/security"
)

func (s *Store) CreateDomain(ctx context.Context, name, selector, publicKey, privateKey string) (Domain, error) {
	return s.createDomain(ctx, name, selector, publicKey, privateKey, nil)
}

func (s *Store) CreateDomainAudited(ctx context.Context, name, selector, publicKey, privateKey string, event AuditEvent) (Domain, error) {
	return s.createDomain(ctx, name, selector, publicKey, privateKey, &event)
}

func (s *Store) createDomain(ctx context.Context, name, selector, publicKey, privateKey string, event *AuditEvent) (Domain, error) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" || !strings.Contains(name, ".") || strings.ContainsAny(name, " @/\\") {
		return Domain{}, fmt.Errorf("invalid domain name")
	}
	if selector == "" {
		selector = "brclio"
	}
	id, err := security.NewID("dom")
	if err != nil {
		return Domain{}, err
	}
	verification, err := security.RandomToken(24)
	if err != nil {
		return Domain{}, err
	}
	now := s.now()
	domain := Domain{
		ID: id, Name: name, Status: "pending", Verification: verification,
		DKIMSelector: selector, DKIMPublicKey: publicKey, DKIMPrivateKey: privateKey, CreatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Domain{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO domains
		(id,name,status,verification_token,dkim_selector,dkim_public_key,dkim_private_key,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, domain.ID, domain.Name, domain.Status, domain.Verification,
		domain.DKIMSelector, domain.DKIMPublicKey, domain.DKIMPrivateKey, domain.CreatedAt)
	if err != nil {
		return Domain{}, mapSQLError(err)
	}
	if event != nil {
		if event.TargetType == "" {
			event.TargetType = "domain"
		}
		if event.TargetID == "" {
			event.TargetID = domain.ID
		}
		if err = s.insertAudit(ctx, tx, *event); err != nil {
			return Domain{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Domain{}, err
	}
	return domain, nil
}

func (s *Store) ListDomains(ctx context.Context) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,status,verification_token,dkim_selector,
		dkim_public_key,dkim_private_key,created_at,verified_at FROM domains ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Domain
	for rows.Next() {
		var item Domain
		var verified sql.NullTime
		if err := rows.Scan(&item.ID, &item.Name, &item.Status, &item.Verification, &item.DKIMSelector,
			&item.DKIMPublicKey, &item.DKIMPrivateKey, &item.CreatedAt, &verified); err != nil {
			return nil, err
		}
		if verified.Valid {
			item.VerifiedAt = &verified.Time
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetDomainByName(ctx context.Context, name string) (Domain, error) {
	var item Domain
	var verified sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id,name,status,verification_token,dkim_selector,
		dkim_public_key,dkim_private_key,created_at,verified_at FROM domains WHERE name=?`,
		strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")).Scan(
		&item.ID, &item.Name, &item.Status, &item.Verification, &item.DKIMSelector,
		&item.DKIMPublicKey, &item.DKIMPrivateKey, &item.CreatedAt, &verified)
	if err != nil {
		return Domain{}, mapSQLError(err)
	}
	if verified.Valid {
		item.VerifiedAt = &verified.Time
	}
	return item, nil
}

func (s *Store) GetDomainByID(ctx context.Context, id string) (Domain, error) {
	var item Domain
	var verified sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id,name,status,verification_token,dkim_selector,
		dkim_public_key,dkim_private_key,created_at,verified_at FROM domains WHERE id=?`, id).Scan(
		&item.ID, &item.Name, &item.Status, &item.Verification, &item.DKIMSelector,
		&item.DKIMPublicKey, &item.DKIMPrivateKey, &item.CreatedAt, &verified)
	if err != nil {
		return Domain{}, mapSQLError(err)
	}
	if verified.Valid {
		item.VerifiedAt = &verified.Time
	}
	return item, nil
}

func (s *Store) SetDomainVerification(ctx context.Context, id string, verified bool) error {
	return s.setDomainVerification(ctx, id, verified, nil)
}

func (s *Store) SetDomainVerificationAudited(ctx context.Context, id string, verified bool, event AuditEvent) error {
	return s.setDomainVerification(ctx, id, verified, &event)
}

func (s *Store) setDomainVerification(ctx context.Context, id string, verified bool, event *AuditEvent) error {
	status := "pending"
	var at any
	if verified {
		status = "verified"
		at = s.now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE domains SET status=?, verified_at=? WHERE id=?`, status, at, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	if event != nil {
		if event.TargetType == "" {
			event.TargetType = "domain"
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

func (s *Store) DomainExists(ctx context.Context, name string) (bool, error) {
	var value int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM domains WHERE name=?`, normalizeEmail(name)).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DNSExpectedRecords(domain Domain, hostname, baseURL string) []map[string]string {
	_ = baseURL
	spf := "v=spf1 mx -all"
	dmarc := "v=DMARC1; p=none; rua=mailto:dmarc@" + domain.Name + "; adkim=r; aspf=r"
	result := []map[string]string{
		{"type": "A/AAAA", "host": hostname, "value": "<your-server-public-IP>", "purpose": "mail server address"},
		{"type": "MX", "host": domain.Name, "value": "10 " + hostname + ".", "purpose": "inbound mail routing"},
		{"type": "TXT", "host": domain.Name, "value": spf, "purpose": "SPF sender policy"},
		{"type": "TXT", "host": domain.DKIMSelector + "._domainkey." + domain.Name, "value": "v=DKIM1; k=rsa; p=" + domain.DKIMPublicKey, "purpose": "DKIM public key"},
		{"type": "TXT", "host": "_dmarc." + domain.Name, "value": dmarc, "purpose": "DMARC policy"},
		{"type": "TXT", "host": "_brclio-mail." + domain.Name, "value": domain.Verification, "purpose": "domain ownership"},
		{"type": "PTR", "host": "<your-server-public-IP>", "value": hostname, "purpose": "reverse DNS; configure with hosting provider"},
		{"type": "TXT", "host": "_smtp._tls." + domain.Name, "value": "v=TLSRPTv1; rua=mailto:tlsrpt@" + domain.Name, "purpose": "TLS aggregate reports"},
		{"type": "SRV", "host": "_submission._tcp." + domain.Name, "value": "0 1 587 " + hostname + ".", "purpose": "mail client submission discovery"},
		{"type": "SRV", "host": "_imaps._tcp." + domain.Name, "value": "0 1 993 " + hostname + ".", "purpose": "mail client IMAPS discovery"},
	}
	return result
}
