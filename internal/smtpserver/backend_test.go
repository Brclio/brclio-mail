package smtpserver

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-smtp"
)

const testPassword = "correct horse battery staple"

func smtpTestStore(t *testing.T) (*store.Store, store.User) {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	domain, err := database.CreateDomain(ctx, "example.com", "brclio", "public", "private")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetDomainVerification(ctx, domain.ID, true); err != nil {
		t.Fatal(err)
	}
	hash, err := security.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, err := database.CreateUser(ctx, "alice@example.com", "Alice", hash, store.RoleUser, 10*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	return database, user
}

func TestInboundRejectsOpenRelayAndDoesNotAdvertiseAuth(t *testing.T) {
	database, _ := smtpTestStore(t)
	session, err := (&backend{store: database, mode: modeInbound, maxMessageBytes: 1024 * 1024}).NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, supportsAuth := session.(smtp.AuthSession); supportsAuth {
		t.Fatal("port-25 session unexpectedly implements AUTH")
	}
	if err := session.Mail("sender@outside.test", nil); err != nil {
		t.Fatalf("valid null-trust envelope sender was rejected: %v", err)
	}
	err = session.Rcpt("victim@outside.test", nil)
	var smtpErr *smtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code != 550 {
		t.Fatalf("external relay recipient was not rejected with 550: %v", err)
	}
}

func TestInboundRejectsUnauthenticatedLocalHeaderFromSpoof(t *testing.T) {
	database, user := smtpTestStore(t)
	session, err := (&backend{store: database, mode: modeInbound, maxMessageBytes: 1024 * 1024}).NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Mail("attacker@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Rcpt(user.Email, nil); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: Alice <ALICE@EXAMPLE.COM>\r\nTo: alice@example.com\r\nSubject: impersonation\r\n\r\nnot really Alice")
	err = session.Data(bytes.NewReader(raw))
	var smtpErr *smtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code != 550 || smtpErr.EnhancedCode != (smtp.EnhancedCode{5, 7, 1}) {
		t.Fatalf("local-domain From spoof was not rejected: %v", err)
	}
	stats, err := database.Stats(context.Background())
	if err != nil || stats.Messages != 0 {
		t.Fatalf("rejected spoof was persisted: %#v err=%v", stats, err)
	}
	if err := session.Mail("attacker@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Rcpt(user.Email, nil); err != nil {
		t.Fatal(err)
	}
	duplicate := []byte("From: sender@outside.test\r\nFrom: alice@example.com\r\nTo: alice@example.com\r\nSubject: ambiguous\r\n\r\nbody")
	err = session.Data(bytes.NewReader(duplicate))
	if !errors.As(err, &smtpErr) || smtpErr.Code != 550 {
		t.Fatalf("duplicate From fields were not rejected: %v", err)
	}
}

func TestSubmissionRequiresAuthenticationAndOwnedFrom(t *testing.T) {
	database, user := smtpTestStore(t)
	ctx := context.Background()
	if _, err := database.CreateAlias(ctx, "hello@example.com", user.Email); err != nil {
		t.Fatal(err)
	}
	sessionValue, err := (&backend{store: database, mode: modeSubmission, maxMessageBytes: 1024 * 1024}).NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session := sessionValue.(*submissionSession)
	if err := session.Mail(user.Email, nil); !errors.Is(err, smtp.ErrAuthRequired) {
		t.Fatalf("submission accepted MAIL before AUTH: %v", err)
	}
	authenticator, err := session.Auth("PLAIN")
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := authenticator.Next([]byte("\x00" + user.Email + "\x00" + testPassword)); err != nil || !done {
		t.Fatalf("valid PLAIN authentication failed: done=%v err=%v", done, err)
	}
	if err := session.Mail("forged@example.com", nil); err == nil {
		t.Fatal("authenticated user was allowed to forge a local From address")
	}
	if err := session.Mail("HELLO@example.com", nil); err != nil {
		t.Fatalf("enabled owned alias was rejected: %v", err)
	}
	if err := session.Rcpt("recipient@outside.test", nil); err != nil {
		t.Fatalf("authenticated external recipient was rejected: %v", err)
	}
	forged := []byte("From: forged@example.com\r\nTo: recipient@outside.test\r\nSubject: forged\r\n\r\nhello")
	if err := session.Data(bytes.NewReader(forged)); err == nil {
		t.Fatal("submission accepted an unowned MIME From header")
	}
	stats, err := database.Stats(ctx)
	if err != nil || stats.Messages != 0 {
		t.Fatalf("rejected forged message was persisted: %#v err=%v", stats, err)
	}
	if err := session.Mail("HELLO@example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Rcpt("recipient@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: Alice via alias <hello@example.com>\r\nTo: recipient@outside.test\r\nBcc: hidden@example.net\r\n\t, second@example.net\r\nResent-Bcc: resent-hidden@example.net\r\n  , resent-second@example.net\r\nSubject: queued\r\n\r\nhello")
	if err := session.Data(bytes.NewReader(raw)); err != nil {
		t.Fatalf("authenticated submission failed: %v", err)
	}
	queue, err := database.ListQueue(ctx, 10)
	if err != nil || len(queue) != 1 || queue[0].Recipient != "recipient@outside.test" {
		t.Fatalf("external delivery was not queued: %#v err=%v", queue, err)
	}
	mailboxes, err := database.IMAPListMailboxes(ctx, user.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var sentID string
	for _, mailbox := range mailboxes {
		if mailbox.Name == store.MailboxSent {
			sentID = mailbox.ID
		}
	}
	entries, err := database.IMAPListEntries(ctx, user.ID, sentID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Sent copy missing: entries=%d err=%v", len(entries), err)
	}
	var storedRaw []byte
	var storedBCC string
	if err := database.DB().QueryRowContext(ctx, `SELECT raw,header_bcc_json FROM messages WHERE id=?`, entries[0].MessageID).Scan(&storedRaw, &storedBCC); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(storedRaw), []byte("bcc:")) || bytes.Contains(bytes.ToLower(storedRaw), []byte("resent-hidden")) || storedBCC != "[]" {
		t.Fatalf("Bcc or Resent-Bcc leaked into stored/delivered MIME: raw=%q header_bcc=%s", storedRaw, storedBCC)
	}
	audit, err := database.ListAudit(ctx, 10)
	if err != nil || len(audit) != 1 || audit[0].Action != "message.submit" || audit[0].ActorID != user.ID ||
		audit[0].IP != "unknown" || !strings.Contains(audit[0].Metadata, `"protocol":"smtp"`) ||
		!strings.Contains(audit[0].Metadata, `"endpoint":"submission"`) {
		t.Fatalf("submission audit missing or incomplete: %#v err=%v", audit, err)
	}
}

func TestDomainlessPostmasterAndRoleAddressesAreCaseInsensitive(t *testing.T) {
	database, _ := smtpTestStore(t)
	ctx := context.Background()
	hash, err := security.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	postmaster, err := database.CreateUser(ctx, "postmaster@example.com", "Postmaster", hash, store.RoleAdmin, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	address, resolved, err := database.ResolveSMTPRecipient(ctx, "PostMaster")
	if err != nil || address != "postmaster@example.com" || resolved.ID != postmaster.ID {
		t.Fatalf("domainless Postmaster resolution failed: %q %#v %v", address, resolved, err)
	}
	if _, err := database.CreateAlias(ctx, "AbUsE@example.com", postmaster.Email); err != nil {
		t.Fatal(err)
	}
	address, resolved, err = database.ResolveSMTPRecipient(ctx, "ABUSE@EXAMPLE.COM")
	if err != nil || address != "abuse@example.com" || resolved.ID != postmaster.ID {
		t.Fatalf("case-insensitive role alias resolution failed: %q %#v %v", address, resolved, err)
	}
	session, err := (&backend{store: database, mode: modeInbound, maxMessageBytes: 1024 * 1024}).NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Rcpt("PostMaster", nil); err != nil {
		t.Fatalf("SMTP RCPT TO:<PostMaster> was rejected: %v", err)
	}
	raw := []byte("From: Sender <sender@outside.test>\r\nTo: postmaster@example.com\r\nSubject: role delivery\r\n\r\nhello")
	if err := session.Data(bytes.NewReader(raw)); err != nil {
		t.Fatalf("domainless Postmaster delivery failed: %v", err)
	}
	inbox, err := database.IMAPGetMailbox(ctx, postmaster.ID, store.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := database.IMAPListEntries(ctx, postmaster.ID, inbox.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("Postmaster inbox delivery missing: %d %v", len(entries), err)
	}
}

func TestAuthenticationIsRateLimitedBeforeValidCredential(t *testing.T) {
	database, user := smtpTestStore(t)
	limiter := authlimit.New(authlimit.Options{MaxFailures: 1, Window: time.Minute, Block: time.Minute, MaxEntries: 20})
	mailBackend := &backend{store: database, mode: modeSubmission, maxMessageBytes: 1024 * 1024, authLimiter: limiter}
	firstValue, err := mailBackend.NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	first := firstValue.(*submissionSession)
	authenticator, err := first.Auth("PLAIN")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authenticator.Next([]byte("\x00" + user.Email + "\x00wrong-password")); err == nil {
		t.Fatal("invalid credential unexpectedly authenticated")
	}
	secondValue, err := mailBackend.NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	second := secondValue.(*submissionSession)
	authenticator, err = second.Auth("PLAIN")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authenticator.Next([]byte("\x00" + user.Email + "\x00" + testPassword)); err == nil {
		t.Fatal("blocked bucket reached credential verification")
	}
}

func TestAuditFailureRollsBackMessageBeforeTemporaryError(t *testing.T) {
	database, user := smtpTestStore(t)
	if _, err := database.DB().Exec(`CREATE TRIGGER reject_protocol_audit BEFORE INSERT ON audit_log
		BEGIN SELECT RAISE(FAIL, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	session, err := (&backend{store: database, mode: modeInbound, maxMessageBytes: 1024 * 1024}).NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Rcpt(user.Email, nil); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@outside.test\r\nTo: alice@example.com\r\nSubject: audit failure\r\n\r\nbody")
	err = session.Data(bytes.NewReader(raw))
	var smtpErr *smtp.SMTPError
	if !errors.As(err, &smtpErr) || smtpErr.Code != 451 {
		t.Fatalf("audit failure did not return temporary SMTP error: %v", err)
	}
	stats, err := database.Stats(context.Background())
	if err != nil || stats.Messages != 0 {
		t.Fatalf("audit failure left a retry-duplicating message: %#v err=%v", stats, err)
	}
}

func TestStripBccHeaderHandlesMixedLineEndings(t *testing.T) {
	raw := []byte("From: alice@example.com\nBcc: hidden@example.net\n\t, folded@example.net\r\nResent-Bcc: resent@example.net\r\n\nbody stays byte-for-byte\nBcc: this is body text")
	stripped := stripBccHeader(raw)
	header, body, ok := splitRawHeader(stripped)
	if !ok {
		t.Fatalf("stripped message has no header boundary: %q", stripped)
	}
	for _, line := range header {
		lower := bytes.ToLower(line)
		if bytes.HasPrefix(lower, []byte("bcc:")) || bytes.HasPrefix(lower, []byte("resent-bcc:")) || bytes.Contains(lower, []byte("folded@example.net")) {
			t.Fatalf("blind-copy header survived mixed line endings: %q", stripped)
		}
	}
	wantBody := "body stays byte-for-byte\nBcc: this is body text"
	if string(body) != wantBody {
		t.Fatalf("body changed while stripping headers: got=%q want=%q", body, wantBody)
	}
}

func TestSubmissionRechecksProtocolAuthVersionBeforeEveryTransactionStage(t *testing.T) {
	database, user := smtpTestStore(t)
	mailBackend := &backend{store: database, mode: modeSubmission, maxMessageBytes: 1024 * 1024}
	ctx := context.Background()

	mailApp, err := database.CreateAppPassword(ctx, user.ID, "rotate-before-mail", security.TokenHash("unused-1"))
	if err != nil {
		t.Fatal(err)
	}
	beforeMail := authenticatedSubmission(t, mailBackend, user.Email, testPassword)
	if err := database.RevokeAppPassword(ctx, user.ID, mailApp.ID); err != nil {
		t.Fatal(err)
	}
	if err := beforeMail.Mail(user.Email, nil); !errors.Is(err, smtp.ErrAuthRequired) {
		t.Fatalf("stale submission session reached MAIL: %v", err)
	}

	recipientApp, err := database.CreateAppPassword(ctx, user.ID, "rotate-before-rcpt", security.TokenHash("unused-2"))
	if err != nil {
		t.Fatal(err)
	}
	beforeRecipient := authenticatedSubmission(t, mailBackend, user.Email, testPassword)
	if err := beforeRecipient.Mail(user.Email, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeAppPassword(ctx, user.ID, recipientApp.ID); err != nil {
		t.Fatal(err)
	}
	if err := beforeRecipient.Rcpt("recipient@outside.test", nil); !errors.Is(err, smtp.ErrAuthRequired) {
		t.Fatalf("stale submission session reached RCPT: %v", err)
	}

	dataApp, err := database.CreateAppPassword(ctx, user.ID, "rotate-before-data", security.TokenHash("unused-3"))
	if err != nil {
		t.Fatal(err)
	}
	beforeData := authenticatedSubmission(t, mailBackend, user.Email, testPassword)
	if err := beforeData.Mail(user.Email, nil); err != nil {
		t.Fatal(err)
	}
	if err := beforeData.Rcpt("recipient@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeAppPassword(ctx, user.ID, dataApp.ID); err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: alice@example.com\r\nTo: recipient@outside.test\r\nSubject: stale auth\r\n\r\nbody")
	if err := beforeData.Data(bytes.NewReader(raw)); !errors.Is(err, smtp.ErrAuthRequired) {
		t.Fatalf("stale submission session reached DATA: %v", err)
	}
	stats, err := database.Stats(ctx)
	if err != nil || stats.Messages != 0 {
		t.Fatalf("stale DATA persisted a message: %#v err=%v", stats, err)
	}
}

func authenticatedSubmission(t *testing.T, mailBackend *backend, email, password string) *submissionSession {
	t.Helper()
	value, err := mailBackend.NewSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session := value.(*submissionSession)
	authenticator, err := session.Auth("PLAIN")
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := authenticator.Next([]byte("\x00" + email + "\x00" + password)); err != nil || !done {
		t.Fatalf("submission authentication failed: done=%v err=%v", done, err)
	}
	return session
}
