package imapserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	"github.com/Brclio/brclio-mail/internal/security"
	mailstore "github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-imap"
)

func imapTestStore(t *testing.T) (*mailstore.Store, mailstore.User, mailstore.User) {
	t.Helper()
	database, err := mailstore.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	if _, err := database.CreateDomain(ctx, "example.com", "brclio", "public", "private"); err != nil {
		t.Fatal(err)
	}
	hash, err := security.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	first, err := database.CreateUser(ctx, "alice@example.com", "Alice", hash, mailstore.RoleUser, 10*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateUser(ctx, "bob@example.com", "Bob", hash, mailstore.RoleUser, 10*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	return database, first, second
}

func TestExpungeRetainsImmutableMessageAndOtherUserCopy(t *testing.T) {
	database, alice, bob := imapTestStore(t)
	ctx := context.Background()
	raw := []byte("From: sender@outside.test\r\nTo: alice@example.com\r\nSubject: retained\r\n\r\nbody")
	saved, err := database.SaveMessage(ctx, mailstore.Message{
		RFCMessageID: "retained@outside.test", EnvelopeFrom: "sender@outside.test",
		EnvelopeTo: []string{alice.Email, bob.Email}, Raw: raw, SizeBytes: int64(len(raw)), Direction: "inbound",
	}, nil, []mailstore.Delivery{
		{UserID: alice.ID, Mailbox: mailstore.MailboxInbox},
		{UserID: bob.ID, Mailbox: mailstore.MailboxInbox},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := &storeBackend{store: database, maxMessageBytes: 1024 * 1024}
	aliceUser := &user{backend: backend, account: alice}
	mailboxValue, err := aliceUser.GetMailbox(mailstore.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	mailbox := mailboxValue.(*mailbox)
	sequence := new(imap.SeqSet)
	sequence.AddNum(1)
	if err := mailbox.UpdateMessagesFlags(false, sequence, imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
		t.Fatal(err)
	}
	if err := mailbox.Expunge(); err != nil {
		t.Fatal(err)
	}
	aliceEntries, err := database.IMAPListEntries(ctx, alice.ID, mailbox.metadata.ID)
	if err != nil || len(aliceEntries) != 0 {
		t.Fatalf("expunged entry remains visible to owner: %d, %v", len(aliceEntries), err)
	}
	bobInbox, err := database.IMAPGetMailbox(ctx, bob.ID, mailstore.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	bobEntries, err := database.IMAPListEntries(ctx, bob.ID, bobInbox.ID)
	if err != nil || len(bobEntries) != 1 || bobEntries[0].MessageID != saved.ID {
		t.Fatalf("another user's copy was altered: %#v %v", bobEntries, err)
	}
	exists, err := database.IMAPMessageExists(ctx, saved.ID)
	if err != nil || !exists {
		t.Fatalf("immutable admin archive was deleted: exists=%v err=%v", exists, err)
	}
	var expunged int
	if err := database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM mailbox_entries WHERE id IN (
		SELECT id FROM mailbox_entries WHERE user_id=? AND message_id=?) AND expunged_at IS NOT NULL`, alice.ID, saved.ID).Scan(&expunged); err != nil {
		t.Fatal(err)
	}
	if expunged != 1 {
		t.Fatalf("owner entry was not soft-expunged: %d", expunged)
	}
}

func TestSeqSetStarUsesMailboxMaximum(t *testing.T) {
	set, err := imap.ParseSeqSet("100:*")
	if err != nil {
		t.Fatal(err)
	}
	if !seqSetContains(set, 50, 50) || seqSetContains(set, 49, 50) {
		t.Fatal("dynamic sequence set did not resolve * to the current maximum")
	}
}

func TestSystemMailboxesCannotBeDeletedOrRenamed(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	account := &user{backend: &storeBackend{store: database, maxMessageBytes: 1024 * 1024}, account: alice}
	for _, name := range mailstore.SystemMailboxes {
		if err := account.DeleteMailbox(name); err == nil {
			t.Fatalf("system mailbox %q was deletable", name)
		}
		if err := account.RenameMailbox(name, name+"-renamed"); err == nil {
			t.Fatalf("system mailbox %q was renameable", name)
		}
		if _, err := database.IMAPGetMailbox(context.Background(), alice.ID, name); err != nil {
			t.Fatalf("system mailbox %q disappeared: %v", name, err)
		}
	}
}

func TestAppendStatusFetchSearchAndCopyCustomMailbox(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	account := &user{backend: &storeBackend{store: database, maxMessageBytes: 1024 * 1024}, account: alice}
	if err := account.CreateMailbox("Projects/Brclio"); err != nil {
		t.Fatal(err)
	}
	listed, err := account.ListMailboxes(false)
	if err != nil {
		t.Fatal(err)
	}
	foundCustom := false
	for _, item := range listed {
		if item.Name() == "Projects/Brclio" {
			foundCustom = true
		}
	}
	if !foundCustom {
		t.Fatal("custom mailbox missing from LIST backend")
	}
	inboxValue, err := account.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	inbox := inboxValue.(*mailbox)
	raw := []byte("From: sender@outside.test\r\nTo: alice@example.com\r\nSubject: searchable project\r\nMessage-ID: <project@outside.test>\r\n\r\nproject body")
	if err := inbox.CreateMessage(nil, time.Time{}, bytes.NewReader(raw)); err != nil {
		t.Fatalf("APPEND backend failed: %v", err)
	}
	status, err := inbox.Status([]imap.StatusItem{imap.StatusMessages, imap.StatusUidNext, imap.StatusUidValidity, imap.StatusUnseen})
	if err != nil || status.Messages != 1 || status.UidNext != 2 || status.UidValidity == 0 || status.Unseen != 1 {
		t.Fatalf("unexpected STATUS: %#v err=%v", status, err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.Header.Set("Subject", "searchable")
	ids, err := inbox.SearchMessages(false, criteria)
	if err != nil || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("SEARCH failed: %#v err=%v", ids, err)
	}
	sequence := new(imap.SeqSet)
	sequence.AddNum(1)
	messages := make(chan *imap.Message, 1)
	if err := inbox.ListMessages(false, sequence, []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchRFC822Size, imap.FetchUid, imap.FetchRFC822}, messages); err != nil {
		t.Fatalf("FETCH backend failed: %v", err)
	}
	fetched := <-messages
	section, err := imap.ParseBodySectionName(imap.FetchRFC822)
	if err != nil {
		t.Fatal(err)
	}
	literal := fetched.GetBody(section)
	gotRaw, err := io.ReadAll(literal)
	if err != nil || !bytes.Equal(gotRaw, raw) || fetched.Uid != 1 || !hasFlag(fetched.Flags, imap.SeenFlag) {
		t.Fatalf("unexpected FETCH result: uid=%d flags=%v raw=%q err=%v", fetched.Uid, fetched.Flags, gotRaw, err)
	}
	if err := inbox.CopyMessages(false, sequence, "Projects/Brclio"); err != nil {
		t.Fatalf("COPY backend failed: %v", err)
	}
	custom, err := database.IMAPGetMailbox(context.Background(), alice.ID, "Projects/Brclio")
	if err != nil {
		t.Fatal(err)
	}
	copies, err := database.IMAPListEntries(context.Background(), alice.ID, custom.ID)
	if err != nil || len(copies) != 1 || copies[0].UID != 1 || !hasFlag(copies[0].Flags, imap.RecentFlag) {
		t.Fatalf("COPY did not allocate a destination entry: %#v err=%v", copies, err)
	}
	if err := inbox.MoveMessages(false, sequence, mailstore.MailboxArchive); err != nil {
		t.Fatalf("MOVE backend failed: %v", err)
	}
	inboxEntries, err := database.IMAPListEntries(context.Background(), alice.ID, inbox.metadata.ID)
	if err != nil || len(inboxEntries) != 0 {
		t.Fatalf("MOVE left source entry visible: %#v err=%v", inboxEntries, err)
	}
	archive, err := database.IMAPGetMailbox(context.Background(), alice.ID, mailstore.MailboxArchive)
	if err != nil {
		t.Fatal(err)
	}
	archiveEntries, err := database.IMAPListEntries(context.Background(), alice.ID, archive.ID)
	if err != nil || len(archiveEntries) != 1 || archiveEntries[0].UID != 1 {
		t.Fatalf("MOVE did not allocate destination UID: %#v err=%v", archiveEntries, err)
	}
	if err := account.RenameMailbox("Projects/Brclio", "Projects/Done"); err != nil {
		t.Fatalf("custom mailbox RENAME failed: %v", err)
	}
	if _, err := account.GetMailbox("Projects/Done"); err != nil {
		t.Fatalf("renamed custom mailbox missing: %v", err)
	}
	if err := account.DeleteMailbox("Projects/Done"); err != nil {
		t.Fatalf("custom mailbox DELETE failed: %v", err)
	}
}

func TestAppendDraftDirectionAndExpungeRemovesDraftArchive(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	account := &user{backend: &storeBackend{store: database, maxMessageBytes: 1024 * 1024}, account: alice}
	tests := []struct {
		mailbox string
		flags   []string
	}{
		{mailbox: mailstore.MailboxDrafts},
		{mailbox: mailstore.MailboxInbox, flags: []string{imap.DraftFlag}},
	}
	for index, test := range tests {
		mailboxValue, err := account.GetMailbox(test.mailbox)
		if err != nil {
			t.Fatal(err)
		}
		selected := mailboxValue.(*mailbox)
		raw := []byte(fmt.Sprintf("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: draft %d\r\n\r\nunfinished", index))
		if err := selected.CreateMessage(test.flags, time.Time{}, bytes.NewReader(raw)); err != nil {
			t.Fatalf("APPEND draft failed: %v", err)
		}
		entries, err := database.IMAPListEntries(context.Background(), alice.ID, selected.metadata.ID)
		if err != nil || len(entries) != 1 {
			t.Fatalf("draft entry missing: %#v %v", entries, err)
		}
		var direction string
		if err := database.DB().QueryRow(`SELECT direction FROM messages WHERE id=?`, entries[0].MessageID).Scan(&direction); err != nil {
			t.Fatal(err)
		}
		if direction != "draft" {
			t.Fatalf("APPEND %s flags=%v direction=%q", test.mailbox, test.flags, direction)
		}
		sequence := new(imap.SeqSet)
		sequence.AddNum(1)
		if err := selected.UpdateMessagesFlags(false, sequence, imap.AddFlags, []string{imap.DeletedFlag}); err != nil {
			t.Fatal(err)
		}
		if err := selected.Expunge(); err != nil {
			t.Fatal(err)
		}
		exists, err := database.IMAPMessageExists(context.Background(), entries[0].MessageID)
		if err != nil || exists {
			t.Fatalf("expunged draft remains in administrator archive: %v %v", exists, err)
		}
	}
}

func TestAppendAuditUsesLoginIPAndServerCreationTime(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	mailBackend := &storeBackend{store: database, maxMessageBytes: 1024 * 1024}
	loggedIn, err := mailBackend.Login(&imap.ConnInfo{RemoteAddr: &net.TCPAddr{
		IP: net.ParseIP("203.0.113.42"), Port: 143,
	}}, alice.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	inboxValue, err := loggedIn.GetMailbox(mailstore.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	clientDate := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.FixedZone("client", 8*60*60))
	raw := []byte("From: sender@outside.test\r\nTo: alice@example.com\r\nSubject: audited append\r\n\r\nbody")
	before := time.Now().UTC()
	if err := inboxValue.CreateMessage(nil, clientDate, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	inbox, err := database.IMAPGetMailbox(context.Background(), alice.ID, mailstore.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := database.IMAPListEntries(context.Background(), alice.ID, inbox.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("APPEND entry missing: %#v err=%v", entries, err)
	}
	var receivedAt, createdAt time.Time
	if err := database.DB().QueryRow(`SELECT received_at,created_at FROM messages WHERE id=?`, entries[0].MessageID).
		Scan(&receivedAt, &createdAt); err != nil {
		t.Fatal(err)
	}
	if !receivedAt.Equal(clientDate.UTC()) {
		t.Fatalf("APPEND internal date=%s want=%s", receivedAt, clientDate.UTC())
	}
	if createdAt.Before(before.Add(-time.Second)) || createdAt.After(after.Add(time.Second)) || createdAt.Equal(clientDate.UTC()) {
		t.Fatalf("client controlled archival creation time: created=%s client=%s window=%s..%s",
			createdAt, clientDate.UTC(), before, after)
	}

	draftsValue, err := loggedIn.GetMailbox(mailstore.MailboxDrafts)
	if err != nil {
		t.Fatal(err)
	}
	if err := draftsValue.CreateMessage([]string{imap.DraftFlag}, time.Time{}, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	audit, err := database.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var appendAudit, draftAudit bool
	for _, event := range audit {
		if event.ActorID != alice.ID || event.IP != "203.0.113.42" || !strings.Contains(event.Metadata, `"protocol":"imap"`) {
			continue
		}
		appendAudit = appendAudit || event.Action == "message.append" && event.TargetID == entries[0].MessageID
		draftAudit = draftAudit || event.Action == "draft.save"
	}
	if !appendAudit || !draftAudit {
		t.Fatalf("IMAP APPEND audit incomplete: append=%t draft=%t events=%#v", appendAudit, draftAudit, audit)
	}
}

func TestAppendAuditFailureRollsBackMessage(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	if _, err := database.DB().Exec(`CREATE TRIGGER reject_imap_append_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='message.append' BEGIN SELECT RAISE(FAIL, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	account := &user{backend: &storeBackend{store: database, maxMessageBytes: 1024 * 1024}, account: alice, remoteIP: "192.0.2.10"}
	inbox, err := account.GetMailbox(mailstore.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@outside.test\r\nTo: alice@example.com\r\nSubject: rejected audit\r\n\r\nbody")
	if err := inbox.CreateMessage(nil, time.Time{}, bytes.NewReader(raw)); !errors.Is(err, mailstore.ErrAuditFailed) {
		t.Fatalf("APPEND audit failure was not propagated: %v", err)
	}
	var messages int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("unaudited IMAP APPEND committed: %d", messages)
	}
}

func TestIMAPAuthenticationIsRateLimited(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	limiter := authlimit.New(authlimit.Options{MaxFailures: 1, Window: time.Minute, Block: time.Minute, MaxEntries: 20})
	mailBackend := &storeBackend{store: database, maxMessageBytes: 1024 * 1024, authLimiter: limiter}
	if _, err := mailBackend.Login(nil, alice.Email, "wrong-password"); err == nil {
		t.Fatal("invalid IMAP credential unexpectedly authenticated")
	}
	if _, err := mailBackend.Login(nil, alice.Email, "correct horse battery staple"); err == nil {
		t.Fatal("blocked IMAP bucket reached credential verification")
	}
}

func TestExistingIMAPSessionFailsClosedAfterSuspension(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	account := &user{backend: &storeBackend{store: database, maxMessageBytes: 1024 * 1024}, account: alice}
	mailboxValue, err := account.GetMailbox(mailstore.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	inbox := mailboxValue.(*mailbox)
	suspended := mailstore.StatusSuspended
	if err := database.UpdateUser(context.Background(), alice.ID, mailstore.UserUpdate{Status: &suspended}); err != nil {
		t.Fatal(err)
	}
	if _, err := account.ListMailboxes(false); !errors.Is(err, mailstore.ErrForbidden) {
		t.Fatalf("suspended session retained LIST access: %v", err)
	}
	if _, err := inbox.Status([]imap.StatusItem{imap.StatusMessages}); !errors.Is(err, mailstore.ErrForbidden) {
		t.Fatalf("suspended session retained STATUS access: %v", err)
	}
	raw := []byte("From: alice@example.com\r\nSubject: blocked append\r\n\r\nbody")
	if err := inbox.CreateMessage(nil, time.Time{}, bytes.NewReader(raw)); !errors.Is(err, mailstore.ErrForbidden) {
		t.Fatalf("suspended session retained APPEND access: %v", err)
	}
	active := mailstore.StatusActive
	if err := database.UpdateUser(context.Background(), alice.ID, mailstore.UserUpdate{Status: &active}); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Status([]imap.StatusItem{imap.StatusMessages}); !errors.Is(err, mailstore.ErrForbidden) {
		t.Fatalf("restored account revived a pre-suspension session: %v", err)
	}
}

func TestExistingIMAPSessionFailsClosedAfterPasswordChange(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	mailBackend := &storeBackend{store: database, maxMessageBytes: 1024 * 1024}
	loggedIn, err := mailBackend.Login(nil, alice.Email, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := security.HashPassword("rotated password")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateUser(context.Background(), alice.ID, mailstore.UserUpdate{PasswordHash: &newHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := loggedIn.ListMailboxes(false); !errors.Is(err, mailstore.ErrForbidden) {
		t.Fatalf("password rotation left the existing IMAP session active: %v", err)
	}
}

func TestExistingIMAPSessionFailsClosedAfterAppPasswordRevocation(t *testing.T) {
	database, alice, _ := imapTestStore(t)
	secret := "imap-app-password"
	app, err := database.CreateAppPassword(context.Background(), alice.ID, "phone", security.TokenHash(secret))
	if err != nil {
		t.Fatal(err)
	}
	mailBackend := &storeBackend{store: database, maxMessageBytes: 1024 * 1024}
	loggedIn, err := mailBackend.Login(nil, alice.Email, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RevokeAppPassword(context.Background(), alice.ID, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := loggedIn.ListMailboxes(false); !errors.Is(err, mailstore.ErrForbidden) {
		t.Fatalf("app-password revocation left the existing IMAP session active: %v", err)
	}
}
