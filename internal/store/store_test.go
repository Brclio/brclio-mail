package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Brclio/brclio-mail/internal/security"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createTestUser(t *testing.T, db *Store, email string, quota int64) User {
	t.Helper()
	ctx := context.Background()
	domain := email[len(email)-len("example.com"):]
	if _, err := db.GetDomainByName(ctx, domain); errors.Is(err, ErrNotFound) {
		if _, err = db.CreateDomain(ctx, domain, "brclio", "public", "private"); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := security.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, email, "Test", hash, RoleUser, quota)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func TestUserExpungePreservesAdminArchiveAndReleasesQuota(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	message := Message{RFCMessageID: "one@example.com", EnvelopeTo: []string{"alice@example.com", "hidden@example.net"}, HeaderBCC: []string{"hidden@example.net"}, Subject: "needle subject", TextBody: "searchable body", Snippet: "searchable body", Raw: []byte("From: sender@example.net\r\nTo: alice@example.com\r\nSubject: needle subject\r\n\r\nsearchable body"), SizeBytes: 100, Direction: "inbound", TransportStatus: "received"}
	saved, err := db.SaveMessage(ctx, message, nil, []Delivery{{UserID: user.ID, Mailbox: MailboxInbox}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := db.GetUserByID(ctx, user.ID)
	if fresh.UsedBytes != 100 {
		t.Fatalf("used bytes=%d", fresh.UsedBytes)
	}
	results, err := db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxInbox, Search: "needle", Limit: 10})
	if err != nil || len(results) != 1 {
		t.Fatalf("search=%d err=%v", len(results), err)
	}
	if len(results[0].EnvelopeTo) != 0 || len(results[0].HeaderBCC) != 0 {
		t.Fatal("mailbox list leaked SMTP envelope or Bcc recipients")
	}
	archiveList, err := db.ListMessages(ctx, MessageQuery{Admin: true, Search: "needle", Limit: 10})
	if err != nil || len(archiveList) != 1 {
		t.Fatalf("archive search=%d err=%v", len(archiveList), err)
	}
	if archiveList[0].Snippet != "" || archiveList[0].TextBody != "" || archiveList[0].HTMLBody != "" || len(archiveList[0].EnvelopeTo) != 0 || len(archiveList[0].HeaderBCC) != 0 {
		t.Fatal("archive metadata list leaked body or hidden-recipient content before an audited view")
	}
	bodyOnly, err := db.ListMessages(ctx, MessageQuery{Admin: true, Search: "searchable body", Limit: 10})
	if err != nil || len(bodyOnly) != 0 {
		t.Fatalf("archive metadata search exposed a body match: count=%d err=%v", len(bodyOnly), err)
	}
	visible, err := db.GetMessage(ctx, user.ID, saved.ID, false)
	if err != nil || len(visible.EnvelopeTo) != 0 || len(visible.HeaderBCC) != 0 {
		t.Fatalf("mailbox detail leaked SMTP envelope or Bcc recipients: %#v %v", visible, err)
	}
	if err = db.MoveMessage(ctx, user.ID, saved.ID, MailboxTrash); err != nil {
		t.Fatal(err)
	}
	if err = db.ExpungeMessage(ctx, user.ID, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetMessage(ctx, user.ID, saved.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user can still read expunged message: %v", err)
	}
	archived, err := db.GetMessage(ctx, "", saved.ID, true)
	if err != nil || string(archived.Raw) != string(message.Raw) {
		t.Fatalf("admin archive lost raw MIME: %v", err)
	}
	if len(archived.EnvelopeTo) != 2 || len(archived.HeaderBCC) != 1 {
		t.Fatal("admin archive lost retained envelope metadata")
	}
	fresh, _ = db.GetUserByID(ctx, user.ID)
	if fresh.UsedBytes != 0 {
		t.Fatalf("quota was not released: %d", fresh.UsedBytes)
	}
}

func TestQuotaRejectIsAtomic(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 50)
	_, err := db.SaveMessage(ctx, Message{RFCMessageID: "too-big", Raw: make([]byte, 51), SizeBytes: 51, Direction: "inbound"}, nil, []Delivery{{UserID: user.ID, Mailbox: MailboxInbox}}, nil)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected quota error, got %v", err)
	}
	stats, _ := db.Stats(ctx)
	if stats.Messages != 0 {
		t.Fatalf("partial message was committed: %d", stats.Messages)
	}
}

func TestQuotaCountsMultipleCopiesForTheSameUser(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 150)
	_, err := db.SaveMessage(ctx, Message{RFCMessageID: "self-send", Raw: make([]byte, 100), SizeBytes: 100, Direction: "outbound"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxSent}, {UserID: user.ID, Mailbox: MailboxInbox}}, nil)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected duplicate-copy quota error, got %v", err)
	}
}

func TestArchiveLimitStillAppliesAfterUserExpunge(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	first := Message{RFCMessageID: "retained", Raw: make([]byte, 100), SizeBytes: 100, Direction: "inbound"}
	second := Message{RFCMessageID: "over-limit", Raw: make([]byte, 51), SizeBytes: 51, Direction: "inbound"}
	db.SetArchiveLimit(estimatedMessageStorage(first, nil) + estimatedMessageStorage(second, nil) - 1)
	message, err := db.SaveMessage(ctx, first, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxInbox}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ExpungeMessage(ctx, user.ID, message.ID); err != nil {
		t.Fatal(err)
	}
	_, err = db.SaveMessage(ctx, second, nil, nil, nil)
	if !errors.Is(err, ErrArchiveFull) {
		t.Fatalf("expected archive limit error, got %v", err)
	}
}

func TestArchiveLimitIncludesDecodedAttachmentStorage(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	message := Message{RFCMessageID: "attachment", Raw: make([]byte, 200), SizeBytes: 200, Direction: "inbound"}
	attachments := []Attachment{{Filename: "large.bin", ContentType: "application/octet-stream", SizeBytes: 4096, Content: make([]byte, 4096)}}
	db.SetArchiveLimit(estimatedMessageStorage(message, attachments) - 1)
	if _, err := db.SaveMessage(ctx, message, attachments, nil, nil); !errors.Is(err, ErrArchiveFull) {
		t.Fatalf("attachment storage bypassed archive limit: %v", err)
	}
	stats, err := db.Stats(ctx)
	if err != nil || stats.Messages != 0 {
		t.Fatalf("archive limit failure was not atomic: %#v %v", stats, err)
	}
}

func TestDiskReserveRejectsBeforeWriting(t *testing.T) {
	db := testStore(t)
	db.SetStorageLimits(0, maxInt64)
	message := Message{RFCMessageID: "low-disk", Raw: make([]byte, 100), Direction: "inbound"}
	if _, err := db.SaveMessage(context.Background(), message, nil, nil, nil); !errors.Is(err, ErrArchiveFull) {
		t.Fatalf("low-disk reserve did not reject message: %v", err)
	}
}

func TestDraftIsPrivateAndPhysicallyRemovedOnExpunge(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	draft, err := db.SaveMessage(ctx, Message{RFCMessageID: "draft", Subject: "unfinished secret", Raw: []byte("Subject: unfinished secret\r\n\r\nbody"), Direction: "draft"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxDrafts}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items, err := db.ListMessages(ctx, MessageQuery{Admin: true, Limit: 10}); err != nil || len(items) != 0 {
		t.Fatalf("admin archive included a draft: %#v %v", items, err)
	}
	if _, err := db.GetMessage(ctx, "", draft.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin could open a draft: %v", err)
	}
	if err := db.ExpungeMessage(ctx, user.ID, draft.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=?`, draft.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("draft raw was retained: count=%d err=%v", count, err)
	}
}

func TestMailboxMutationsAffectOneEntryAndAllocateNewUIDs(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 4096)
	message, err := db.SaveMessage(ctx, Message{RFCMessageID: "self", Raw: make([]byte, 100), SizeBytes: 100, Direction: "outbound"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxSent}, {UserID: user.ID, Mailbox: MailboxInbox}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent, _ := db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxSent, Limit: 10})
	inbox, _ := db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxInbox, Limit: 10})
	if len(sent) != 1 || len(inbox) != 1 || sent[0].MailboxEntryID == inbox[0].MailboxEntryID {
		t.Fatalf("self-send entries sent=%#v inbox=%#v", sent, inbox)
	}
	if err := db.UpdateFlags(ctx, user.ID, sent[0].MailboxEntryID, []string{"\\Seen", "\\Flagged"}); err != nil {
		t.Fatal(err)
	}
	freshInbox, err := db.GetMessage(ctx, user.ID, inbox[0].MailboxEntryID, false)
	if err != nil || len(freshInbox.UserFlags) != 0 {
		t.Fatalf("flag update crossed mailbox copies: %#v %v", freshInbox.UserFlags, err)
	}
	if err := db.MoveMessage(ctx, user.ID, sent[0].MailboxEntryID, MailboxTrash); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxInbox, Limit: 10}); len(remaining) != 1 {
		t.Fatalf("moving Sent copy changed Inbox: %d", len(remaining))
	}
	trash, _ := db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxTrash, Limit: 10})
	if len(trash) != 1 {
		t.Fatalf("trash entries=%d", len(trash))
	}
	second, err := db.SaveMessage(ctx, Message{RFCMessageID: "second", Raw: make([]byte, 80), SizeBytes: 80, Direction: "inbound"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxInbox}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondInbox, err := db.GetMessage(ctx, user.ID, second.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveMessage(ctx, user.ID, secondInbox.MailboxEntryID, MailboxTrash); err != nil {
		t.Fatal(err)
	}
	trash, _ = db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxTrash, Limit: 10})
	if len(trash) != 2 || trash[0].UID == trash[1].UID {
		t.Fatalf("target mailbox UIDs are invalid: %#v", trash)
	}
	if err := db.ExpungeMessage(ctx, user.ID, trash[0].MailboxEntryID); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := db.ListMessages(ctx, MessageQuery{UserID: user.ID, Mailbox: MailboxTrash, Limit: 10}); len(remaining) != 1 {
		t.Fatalf("expunge affected multiple entries: %d", len(remaining))
	}
	if _, err := db.GetMessage(ctx, "", message.ID, true); err != nil {
		t.Fatalf("admin archive lost self-send raw: %v", err)
	}
}

func TestAliasCannotAuthenticateButResolvesRecipient(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	if _, err := db.CreateAlias(ctx, "hello@example.com", user.Email); err != nil {
		t.Fatal(err)
	}
	resolved, err := db.ResolveRecipient(ctx, "HELLO@example.com")
	if err != nil || resolved.ID != user.ID {
		t.Fatalf("alias did not resolve: %#v %v", resolved, err)
	}
	if _, err = db.Authenticate(ctx, "hello@example.com", "correct horse battery staple", true); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("alias unexpectedly authenticated: %v", err)
	}
}

func TestAuditPaginationAndActionFilterKeepOlderEventsReachable(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	for index := 0; index < 5; index++ {
		action := "message.append"
		if index == 2 {
			action = "archive.message.view"
		}
		if err := db.Audit(ctx, AuditEvent{Action: action, TargetType: "message", TargetID: fmt.Sprintf("message-%d", index),
			CreatedAt: time.Unix(int64(index+1), 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := db.ListAuditPage(ctx, 2, 0, "")
	if err != nil || len(first) != 2 {
		t.Fatalf("first audit page=%d err=%v", len(first), err)
	}
	second, err := db.ListAuditPage(ctx, 2, 2, "")
	if err != nil || len(second) != 2 || first[0].ID == second[0].ID {
		t.Fatalf("second audit page=%#v err=%v", second, err)
	}
	views, err := db.ListAuditPage(ctx, 10, 0, "archive.message.view")
	if err != nil || len(views) != 1 || views[0].TargetID != "message-2" {
		t.Fatalf("filtered audit=%#v err=%v", views, err)
	}
}

func TestSMTPFromRequiresTheAliasDomainToBeVerified(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	primary, err := db.GetDomainByName(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetDomainVerification(ctx, primary.ID, true); err != nil {
		t.Fatal(err)
	}
	secondary, err := db.CreateDomain(ctx, "pending.example", "brclio", "public", "private")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateAlias(ctx, "alice@pending.example", user.Email); err != nil {
		t.Fatal(err)
	}
	if allowed, err := db.SMTPFromAllowed(ctx, user.ID, "alice@pending.example"); err != nil || allowed {
		t.Fatalf("pending alias domain was allowed: allowed=%t err=%v", allowed, err)
	}
	if err = db.SetDomainVerification(ctx, secondary.ID, true); err != nil {
		t.Fatal(err)
	}
	if allowed, err := db.SMTPFromAllowed(ctx, user.ID, "alice@pending.example"); err != nil || !allowed {
		t.Fatalf("verified alias domain was rejected: allowed=%t err=%v", allowed, err)
	}
}

func TestAppPasswordCannotReplacePrimaryPasswordForWebLogin(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	secret := "brclio-mail-client-secret"
	if _, err := db.CreateAppPassword(ctx, user.ID, "Thunderbird", security.TokenHash(secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Authenticate(ctx, user.Email, secret, false); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("app password unexpectedly authenticated for web: %v", err)
	}
	if _, err := db.Authenticate(ctx, user.Email, secret, true); err != nil {
		t.Fatalf("app password did not authenticate for a mail client: %v", err)
	}
	if _, err := db.Authenticate(ctx, user.Email, "correct horse battery staple", false); err != nil {
		t.Fatalf("primary password did not authenticate for web: %v", err)
	}
}

func TestPasswordResetRevokesSessionsAndAppPasswords(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	appSecret := "brclio-mail-client-secret"
	if _, err := db.CreateAppPassword(ctx, user.ID, "phone", security.TokenHash(appSecret)); err != nil {
		t.Fatal(err)
	}
	authenticated, err := db.Authenticate(ctx, user.Email, "correct horse battery staple", false)
	if err != nil {
		t.Fatal(err)
	}
	sessionSecret := "long-random-session-secret"
	if _, err := db.CreateSession(ctx, user.ID, authenticated.AuthVersion, security.TokenHash(sessionSecret), "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	newHash, err := security.HashPassword("new correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUser(ctx, user.ID, UserUpdate{PasswordHash: &newHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserForSession(ctx, security.TokenHash(sessionSecret)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old session remained valid: %v", err)
	}
	if _, err := db.Authenticate(ctx, user.Email, appSecret, true); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old app password remained valid: %v", err)
	}
}

func TestWebAndProtocolAuthVersionsAdvanceOnRelevantChanges(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "auth-version@example.com", 1024)
	if user.AuthVersion != 1 || user.ProtocolAuthVersion != 1 {
		t.Fatalf("new user versions web=%d protocol=%d", user.AuthVersion, user.ProtocolAuthVersion)
	}
	login, err := db.Authenticate(ctx, user.Email, "correct horse battery staple", true)
	if err != nil || login.AuthVersion != 1 || login.ProtocolAuthVersion != 1 {
		t.Fatalf("authentication snapshot versions web=%d protocol=%d err=%v",
			login.AuthVersion, login.ProtocolAuthVersion, err)
	}
	app, err := db.CreateAppPassword(ctx, user.ID, "phone", security.TokenHash("app-secret"))
	if err != nil {
		t.Fatal(err)
	}
	assertAuthVersions(t, db, user.ID, 1, 1)
	if err := db.RevokeAppPassword(ctx, user.ID, app.ID); err != nil {
		t.Fatal(err)
	}
	assertAuthVersions(t, db, user.ID, 1, 2)
	if err := db.RevokeAppPassword(ctx, user.ID, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke returned %v", err)
	}
	assertAuthVersions(t, db, user.ID, 1, 2)

	newHash, err := security.HashPassword("new credential")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUser(ctx, user.ID, UserUpdate{PasswordHash: &newHash}); err != nil {
		t.Fatal(err)
	}
	assertAuthVersions(t, db, user.ID, 2, 3)
	suspended := StatusSuspended
	if err := db.UpdateUser(ctx, user.ID, UserUpdate{Status: &suspended}); err != nil {
		t.Fatal(err)
	}
	assertAuthVersions(t, db, user.ID, 3, 4)
	active := StatusActive
	if err := db.UpdateUser(ctx, user.ID, UserUpdate{Status: &active}); err != nil {
		t.Fatal(err)
	}
	assertAuthVersions(t, db, user.ID, 4, 5)
	login, err = db.Authenticate(ctx, user.Email, "new credential", true)
	if err != nil || login.AuthVersion != 4 || login.ProtocolAuthVersion != 5 {
		t.Fatalf("refreshed authentication snapshot versions web=%d protocol=%d err=%v",
			login.AuthVersion, login.ProtocolAuthVersion, err)
	}
}

func TestCreateSessionRejectsStaleAuthenticationSnapshot(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "session-race@example.com", 1024)
	snapshot, err := db.Authenticate(ctx, user.Email, "correct horse battery staple", false)
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := security.HashPassword("rotated before session insert")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUser(ctx, user.ID, UserUpdate{PasswordHash: &newHash}); err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateSession(ctx, user.ID, snapshot.AuthVersion, security.TokenHash("stale-session"),
		"127.0.0.1", "test", time.Now().Add(time.Hour))
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("stale authentication snapshot created a session: %v", err)
	}
	var sessions int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id=?`, user.ID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("stale session row was committed: %d", sessions)
	}
}

func TestWebSessionsAreCleanedCappedAndMetadataTruncated(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "bounded-sessions@example.com", 1024)
	lastSecret := ""
	for index := 0; index < maxActiveWebSessions+5; index++ {
		secret := fmt.Sprintf("session-%d", index)
		lastSecret = secret
		if _, err := db.CreateSession(ctx, user.ID, user.AuthVersion, security.TokenHash(secret), strings.Repeat("1", 100), strings.Repeat("界", 400), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	var count, maxIP, maxAgent int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*),MAX(length(CAST(ip AS BLOB))),MAX(length(CAST(user_agent AS BLOB))) FROM sessions WHERE user_id=?`, user.ID).Scan(&count, &maxIP, &maxAgent); err != nil {
		t.Fatal(err)
	}
	if count != maxActiveWebSessions || maxIP > maxSessionIPBytes || maxAgent > maxUserAgentBytes {
		t.Fatalf("bounded sessions count=%d ip=%d agent=%d", count, maxIP, maxAgent)
	}
	if _, err := db.UserForSession(ctx, security.TokenHash(lastSecret)); err != nil {
		t.Fatalf("newest capped session was evicted: %v", err)
	}
	if _, err := db.CreateSession(ctx, user.ID, user.AuthVersion, security.TokenHash("already-expired"), "test", "test", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteExpiredSessions(ctx); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE token_hash=?`, security.TokenHash("already-expired")).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired sessions=%d err=%v", expired, err)
	}
}

func TestAppPasswordLifecycleDoesNotInvalidateWebSession(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "versioned-session@example.com", 1024)
	snapshot, err := db.Authenticate(ctx, user.Email, "correct horse battery staple", false)
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := security.TokenHash("versioned-session")
	if _, err := db.CreateSession(ctx, user.ID, snapshot.AuthVersion, tokenHash,
		"127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateAppPassword(ctx, user.ID, "new client", security.TokenHash("new-client-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserForSession(ctx, tokenHash); err != nil {
		t.Fatalf("creating an app password invalidated the initiating Web session: %v", err)
	}
	if err := db.RevokeAppPassword(ctx, user.ID, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserForSession(ctx, tokenHash); err != nil {
		t.Fatalf("revoking an app password invalidated the Web session: %v", err)
	}
}

func TestSchemaV1FixtureUpgradesAndStillAuthenticates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schema-v1.db")
	initial, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.CreateDomain(ctx, "example.com", "brclio", "public", "private"); err != nil {
		initial.Close()
		t.Fatal(err)
	}
	hash, err := security.HashPassword("v1 credential")
	if err != nil {
		initial.Close()
		t.Fatal(err)
	}
	legacy, err := initial.CreateUser(ctx, "legacy@example.com", "Legacy", hash, RoleUser, 1024)
	if err != nil {
		initial.Close()
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE users DROP COLUMN protocol_auth_version`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN auth_version`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE users DROP COLUMN auth_version`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version>=2`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open v1 fixture: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	login, err := upgraded.Authenticate(ctx, legacy.Email, "v1 credential", true)
	if err != nil || login.ID != legacy.ID || login.AuthVersion != 1 || login.ProtocolAuthVersion != 1 {
		t.Fatalf("v1 user failed after upgrade: %#v err=%v", login, err)
	}
	var versions string
	if err := upgraded.DB().QueryRowContext(ctx,
		`SELECT GROUP_CONCAT(version, ',') FROM (SELECT version FROM schema_migrations ORDER BY version)`).Scan(&versions); err != nil || versions != "1,2,3,4" {
		t.Fatalf("schema migration sequence=%q err=%v", versions, err)
	}
	var sessionVersionColumn int
	if err := upgraded.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='auth_version'`).Scan(&sessionVersionColumn); err != nil || sessionVersionColumn != 1 {
		t.Fatalf("sessions.auth_version columns=%d err=%v", sessionVersionColumn, err)
	}
	var protocolVersionColumn int
	if err := upgraded.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name='protocol_auth_version'`).Scan(&protocolVersionColumn); err != nil || protocolVersionColumn != 1 {
		t.Fatalf("users.protocol_auth_version columns=%d err=%v", protocolVersionColumn, err)
	}
}

func assertAuthVersions(t *testing.T, db *Store, userID string, wantWeb, wantProtocol int64) {
	t.Helper()
	user, err := db.GetUserByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.AuthVersion != wantWeb || user.ProtocolAuthVersion != wantProtocol {
		t.Fatalf("auth versions web=%d protocol=%d want web=%d protocol=%d",
			user.AuthVersion, user.ProtocolAuthVersion, wantWeb, wantProtocol)
	}
}

func TestLastActiveAdministratorCannotBeDemotedOrSuspended(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.CreateDomain(ctx, "example.com", "brclio", "public", "private"); err != nil {
		t.Fatal(err)
	}
	hash, err := security.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.CreateUser(ctx, "admin@example.com", "Admin", hash, RoleAdmin, 1024)
	if err != nil {
		t.Fatal(err)
	}
	userRole := RoleUser
	if err := db.UpdateUser(ctx, admin.ID, UserUpdate{Role: &userRole}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("last admin demotion error=%v", err)
	}
	suspended := StatusSuspended
	if err := db.UpdateUser(ctx, admin.ID, UserUpdate{Status: &suspended}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("last admin suspension error=%v", err)
	}
	if _, err := db.CreateUser(ctx, "second@example.com", "Second", hash, RoleAdmin, 1024); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUser(ctx, admin.ID, UserUpdate{Role: &userRole}); err != nil {
		t.Fatalf("demotion with a second active admin failed: %v", err)
	}
}

func TestBackupIsValidatedAndNeverOverwrites(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	user := createTestUser(t, db, "alice@example.com", 1024)
	if _, err := db.SaveMessage(ctx, Message{RFCMessageID: "backup-test", Raw: []byte("Subject: backup\r\n\r\nbody"), Direction: "inbound"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxInbox}}, nil); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshots", "mail.sqlite")
	if err := db.Backup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode=%o", info.Mode().Perm())
	}
	backupDB, err := Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()
	stats, err := backupDB.Stats(ctx)
	if err != nil || stats.Messages != 1 {
		t.Fatalf("backup messages=%d err=%v", stats.Messages, err)
	}
	if err := db.Backup(ctx, destination); err == nil {
		t.Fatal("backup unexpectedly overwrote an existing file")
	}
}

func TestCancelledBackupLeavesNoPublishedOrTemporaryFiles(t *testing.T) {
	db := testStore(t)
	directory := filepath.Join(t.TempDir(), "cancelled-backup")
	destination := filepath.Join(directory, "mail.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Backup(ctx, destination); err == nil {
		t.Fatal("cancelled backup unexpectedly succeeded")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled backup left artifacts: %#v", entries)
	}
}

func TestAbandonedQueueClaimIsRecovered(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	message, err := db.SaveMessage(ctx, Message{RFCMessageID: "queued", EnvelopeFrom: "sender@example.com", Raw: []byte("Subject: queued\r\n\r\nbody"), Direction: "outbound"}, nil, nil, []string{"outside@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	items, err := db.ListQueue(ctx, 10)
	if err != nil || len(items) != 1 || items[0].MessageID != message.ID {
		t.Fatalf("queue=%#v err=%v", items, err)
	}
	if !db.ClaimQueue(ctx, items[0].ID) {
		t.Fatal("could not claim queued delivery")
	}
	if ready, _ := db.QueueReady(ctx, 10); len(ready) != 0 {
		t.Fatal("delivering item should not be ready")
	}
	recovered, err := db.RecoverStaleQueue(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	if ready, _ := db.QueueReady(ctx, 10); len(ready) != 1 {
		t.Fatalf("recovered queue ready=%d", len(ready))
	}
}

func TestRetainedRawCannotBeUpdatedOrDeleted(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	message, err := db.SaveMessage(ctx, Message{RFCMessageID: "immutable", Raw: []byte("Subject: original\r\n\r\nbody"), Direction: "inbound"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET raw=? WHERE id=?`, []byte("changed"), message.ID); err == nil {
		t.Fatal("retained raw MIME was mutable")
	}
	if _, err := db.DB().ExecContext(ctx, `DELETE FROM messages WHERE id=?`, message.ID); err == nil {
		t.Fatal("retained correspondence was deletable")
	}
}

func TestNewerSchemaVersionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(999,?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(path); err == nil {
		reopened.Close()
		t.Fatal("newer schema unexpectedly opened with an older binary")
	}
}
