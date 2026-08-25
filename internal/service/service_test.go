package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/mailcore"
	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/store"
)

func TestLocalSendCreatesSentAndInboxSingleArchive(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	svc := New(db, config.Config{Hostname: "mail.example.com"})
	admin, _, err := svc.Setup(ctx, SetupRequest{Domain: "example.com", Email: "alice@example.com", Password: "correct horse battery staple"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := securityHash("another correct horse battery staple")
	bob, err := db.CreateUser(ctx, "bob@example.com", "Bob", hash, store.RoleUser, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	message, err := svc.Send(ctx, admin, mailcore.ComposeRequest{To: []string{bob.Email}, Subject: "hello", TextBody: "body"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	stats, _ := db.Stats(ctx)
	if stats.Messages != 1 || stats.UserCopies != 2 {
		t.Fatalf("unexpected archive/copies: %#v", stats)
	}
	if message.TransportStatus != "delivered" {
		t.Fatalf("status=%s", message.TransportStatus)
	}
	if items, _ := db.ListMessages(ctx, store.MessageQuery{UserID: bob.ID, Mailbox: store.MailboxInbox, Limit: 10}); len(items) != 1 {
		t.Fatalf("bob inbox=%d", len(items))
	}
}

func TestAdminViewRequiresReason(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	svc := New(db, config.Config{Hostname: "mail.example.com"})
	admin, _, err := svc.Setup(ctx, SetupRequest{Domain: "example.com", Email: "admin@example.com", Password: "correct horse battery staple"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	domains, _ := db.ListDomains(ctx)
	if err := db.SetDomainVerification(ctx, domains[0].ID, true); err != nil {
		t.Fatal(err)
	}
	message, err := svc.Send(ctx, admin, mailcore.ComposeRequest{To: []string{"outside@example.net"}, Subject: "archive", TextBody: "body"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AdminView(ctx, admin, message.ID, "x", "test"); err == nil {
		t.Fatal("short reason was accepted")
	}
	if _, err = svc.AdminView(ctx, admin, message.ID, "security review", "test"); err != nil {
		t.Fatal(err)
	}
	audit, _ := db.ListAudit(ctx, 20)
	found := false
	for _, event := range audit {
		if event.Action == "archive.message.view" && event.Reason == "security review" {
			found = true
		}
	}
	if !found {
		t.Fatal("archive view was not audited")
	}
}

type staticTXTResolver struct {
	records []string
	err     error
}

func (r staticTXTResolver) LookupTXT(context.Context, string) ([]string, error) {
	return r.records, r.err
}

func TestDomainOwnershipVerificationEnablesExternalSending(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	svc := New(db, config.Config{Hostname: "mail.example.com"})
	admin, domain, err := svc.Setup(ctx, SetupRequest{Domain: "example.com", Email: "admin@example.com", Password: "correct horse battery staple"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, admin, mailcore.ComposeRequest{To: []string{"outside@example.net"}, Subject: "blocked", TextBody: "body"}, "test"); !errors.Is(err, store.ErrDomainUnverified) {
		t.Fatalf("unverified domain external send error=%v", err)
	}
	svc.Resolver = staticTXTResolver{records: []string{"unrelated", domain.Verification}}
	verified, err := svc.VerifyDomain(ctx, domain.ID)
	if err != nil || verified.Status != "verified" || verified.VerifiedAt == nil {
		t.Fatalf("verification=%#v err=%v", verified, err)
	}
	if _, err := svc.Send(ctx, admin, mailcore.ComposeRequest{To: []string{"outside@example.net"}, Subject: "allowed", TextBody: "body"}, "test"); err != nil {
		t.Fatalf("verified domain external send failed: %v", err)
	}
}

func TestConcurrentSetupCreatesOnlyOneAdministrator(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	services := []*Service{
		New(db, config.Config{Hostname: "mail.example.com", SetupToken: "claim-once"}),
		New(db, config.Config{Hostname: "mail.example.com", SetupToken: "claim-once"}),
	}
	requests := []SetupRequest{
		{Domain: "one.example", Email: "admin@one.example", Password: "correct horse battery staple", SetupToken: "claim-once"},
		{Domain: "two.example", Email: "admin@two.example", Password: "correct horse battery staple", SetupToken: "claim-once"},
	}
	results := make(chan error, len(requests))
	var group sync.WaitGroup
	for index, request := range requests {
		group.Add(1)
		go func(svc *Service, request SetupRequest) {
			defer group.Done()
			_, _, setupErr := svc.Setup(context.Background(), request, "test")
			results <- setupErr
		}(services[index], request)
	}
	group.Wait()
	close(results)
	var successes, conflicts int
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, store.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected setup error: %v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	if count, err := db.CountUsers(context.Background()); err != nil || count != 1 {
		t.Fatalf("users=%d err=%v", count, err)
	}
}

func TestWebComposeHonorsFinalMIMEMessageLimit(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	svc := New(db, config.Config{Hostname: "mail.example.com", MaxMessageBytes: 1024, MaxArchiveBytes: 4096})
	admin, _, err := svc.Setup(ctx, SetupRequest{Domain: "example.com", Email: "admin@example.com", Password: "correct horse battery staple"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Send(ctx, admin, mailcore.ComposeRequest{To: []string{"outside@example.net"}, Subject: "large", TextBody: strings.Repeat("x", 2048)}, "test")
	if err == nil {
		t.Fatal("oversized composed MIME message was accepted")
	}
	stats, statErr := db.Stats(ctx)
	if statErr != nil || stats.Messages != 0 {
		t.Fatalf("oversized message was persisted: %#v %v", stats, statErr)
	}
}

func TestDraftReplacementIsAtomicAndUsesNetQuota(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	svc := New(db, config.Config{Hostname: "mail.example.com"})
	admin, _, err := svc.Setup(ctx, SetupRequest{Domain: "example.com", Email: "admin@example.com", Password: "correct horse battery staple"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.SaveDraft(ctx, admin, mailcore.ComposeRequest{Subject: "first", TextBody: strings.Repeat("x", 2048)}, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.GetUserByID(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	quota := fresh.UsedBytes
	if err := db.UpdateUser(ctx, admin.ID, store.UserUpdate{QuotaBytes: &quota}); err != nil {
		t.Fatal(err)
	}
	second, err := svc.SaveDraft(ctx, admin, mailcore.ComposeRequest{Subject: "second", TextBody: "short"}, first.ID, "test")
	if err != nil {
		t.Fatalf("smaller draft replacement at quota failed: %v", err)
	}
	if _, err := db.GetMessage(ctx, admin.ID, first.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replaced draft remained visible: %v", err)
	}
	if _, err := db.GetMessage(ctx, admin.ID, second.ID, false); err != nil {
		t.Fatalf("replacement draft missing: %v", err)
	}

	if _, err := db.DB().Exec(`CREATE TRIGGER reject_replacement_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(FAIL, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveDraft(ctx, admin, mailcore.ComposeRequest{Subject: "must roll back", TextBody: "x"}, second.ID, "test"); !errors.Is(err, store.ErrAuditFailed) {
		t.Fatalf("replacement audit failure=%v", err)
	}
	if retained, err := db.GetMessage(ctx, admin.ID, second.ID, false); err != nil || retained.Subject != "second" {
		t.Fatalf("failed replacement lost prior draft: %#v %v", retained, err)
	}
}

func TestSendingConsumesAutosavedDraftInSameTransaction(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	svc := New(db, config.Config{Hostname: "mail.example.com"})
	admin, _, err := svc.Setup(ctx, SetupRequest{Domain: "example.com", Email: "admin@example.com", Password: "correct horse battery staple"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	draft, err := svc.SaveDraft(ctx, admin, mailcore.ComposeRequest{To: []string{admin.Email}, Subject: "ready", TextBody: "send me"}, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, admin, mailcore.ComposeRequest{DraftID: draft.ID, To: []string{admin.Email}, Subject: "ready", TextBody: "send me"}, "test"); err != nil {
		t.Fatal(err)
	}
	var drafts int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE direction='draft'`).Scan(&drafts); err != nil || drafts != 0 {
		t.Fatalf("sent autosave draft rows=%d err=%v", drafts, err)
	}
	if items, err := db.ListMessages(ctx, store.MessageQuery{UserID: admin.ID, Mailbox: store.MailboxSent, Limit: 10}); err != nil || len(items) != 1 {
		t.Fatalf("sent mailbox=%d err=%v", len(items), err)
	}
}

func securityHash(password string) (string, error) { return security.HashPassword(password) }
