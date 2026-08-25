package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCustomMailboxMetadataIsBounded(t *testing.T) {
	database := testStore(t)
	user := createTestUser(t, database, "mailbox-limit@example.com", 1024*1024)
	ctx := context.Background()
	mailboxes, err := database.ListMailboxes(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index := len(mailboxes); index < maxMailboxesPerUser; index++ {
		if err := database.CreateMailbox(ctx, user.ID, fmt.Sprintf("Folder-%03d", index)); err != nil {
			t.Fatalf("create mailbox %d: %v", index, err)
		}
	}
	if err := database.CreateMailbox(ctx, user.ID, "one-too-many"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("mailbox count limit error=%v", err)
	}
	if err := database.CreateMailbox(ctx, user.ID, strings.Repeat("x", maxMailboxNameBytes+1)); err == nil {
		t.Fatal("oversized mailbox name was accepted")
	}
	if err := database.CreateMailbox(ctx, user.ID, "bad\nname"); err == nil {
		t.Fatal("control character in mailbox name was accepted")
	}
	if err := database.RenameMailbox(ctx, user.ID, "Folder-006", "   "); err == nil {
		t.Fatal("empty renamed mailbox was accepted")
	}
}

func TestIMAPDraftExpungePhysicallyDeletesLastEntryMessageAndAttachment(t *testing.T) {
	database := testStore(t)
	user := createTestUser(t, database, "draft-owner@example.com", 1024*1024)
	ctx := context.Background()
	raw := []byte("From: draft-owner@example.com\r\nTo: someone@example.net\r\nSubject: private draft\r\n\r\nunfinished")
	saved, err := database.SaveMessage(ctx, Message{
		RFCMessageID: "private-draft@example.com", Raw: raw, SizeBytes: int64(len(raw)), Direction: "draft",
	}, []Attachment{{Filename: "secret.txt", ContentType: "text/plain", SizeBytes: 6, SHA256: "test", Content: []byte("secret")}},
		[]Delivery{{UserID: user.ID, Mailbox: MailboxDrafts, Flags: []string{"\\Draft", "\\Deleted"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := database.IMAPGetMailbox(ctx, user.ID, MailboxDrafts)
	if err != nil {
		t.Fatal(err)
	}
	count, err := database.IMAPExpungeDeleted(ctx, user.ID, mailbox.ID)
	if err != nil || count != 1 {
		t.Fatalf("draft expunge count=%d err=%v", count, err)
	}
	for table, query := range map[string]string{
		"messages":        `SELECT COUNT(*) FROM messages WHERE id=?`,
		"mailbox_entries": `SELECT COUNT(*) FROM mailbox_entries WHERE message_id=?`,
		"attachments":     `SELECT COUNT(*) FROM attachments WHERE message_id=?`,
		"message_search":  `SELECT COUNT(*) FROM message_search WHERE message_id=?`,
	} {
		var remaining int
		if err := database.DB().QueryRowContext(ctx, query, saved.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("%s retained private draft rows: %d", table, remaining)
		}
	}
}

func TestIMAPDraftExpungeKeepsMessageWhileAnotherEntryExists(t *testing.T) {
	database := testStore(t)
	user := createTestUser(t, database, "draft-copy@example.com", 1024*1024)
	ctx := context.Background()
	raw := []byte("From: draft-copy@example.com\r\nSubject: copied draft\r\n\r\nunfinished")
	saved, err := database.SaveMessage(ctx, Message{RFCMessageID: "copied-draft@example.com", Raw: raw,
		SizeBytes: int64(len(raw)), Direction: "draft"}, nil, []Delivery{
		{UserID: user.ID, Mailbox: MailboxDrafts, Flags: []string{"\\Draft", "\\Deleted"}},
		{UserID: user.ID, Mailbox: MailboxArchive, Flags: []string{"\\Draft"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := database.IMAPGetMailbox(ctx, user.ID, MailboxDrafts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.IMAPExpungeDeleted(ctx, user.ID, drafts.ID); err != nil {
		t.Fatal(err)
	}
	exists, err := database.IMAPMessageExists(ctx, saved.ID)
	if err != nil || !exists {
		t.Fatalf("draft message with another entry was deleted: %v %v", exists, err)
	}
	var entries int
	if err := database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM mailbox_entries WHERE message_id=?`, saved.ID).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("unexpected remaining draft entries: %d", entries)
	}
}

func TestIMAPMoveDoesNotLeaveSoftExpungedDraftArchive(t *testing.T) {
	database := testStore(t)
	user := createTestUser(t, database, "moved-draft@example.com", 1024*1024)
	ctx := context.Background()
	raw := []byte("From: moved-draft@example.com\r\nSubject: moved draft\r\n\r\nunfinished")
	saved, err := database.SaveMessage(ctx, Message{RFCMessageID: "moved-draft@example.com", Raw: raw,
		SizeBytes: int64(len(raw)), Direction: "draft"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: MailboxDrafts, Flags: []string{"\\Draft"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := database.IMAPGetMailbox(ctx, user.ID, MailboxDrafts)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := database.IMAPListEntries(ctx, user.ID, drafts.ID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("draft source missing: %#v %v", entries, err)
	}
	if err := database.IMAPMoveEntries(ctx, user.ID, drafts.ID, MailboxArchive, entries); err != nil {
		t.Fatal(err)
	}
	var allEntries int
	if err := database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM mailbox_entries WHERE message_id=?`, saved.ID).Scan(&allEntries); err != nil {
		t.Fatal(err)
	}
	if allEntries != 1 {
		t.Fatalf("MOVE retained a private soft-expunged draft source: %d", allEntries)
	}
	archive, err := database.IMAPGetMailbox(ctx, user.ID, MailboxArchive)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := database.IMAPListEntries(ctx, user.ID, archive.ID)
	if err != nil || len(moved) != 1 {
		t.Fatalf("moved draft destination missing: %#v %v", moved, err)
	}
	if err := database.IMAPSetEntryFlags(ctx, user.ID, archive.ID, moved[0].ID, []string{"\\Draft", "\\Deleted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.IMAPExpungeDeleted(ctx, user.ID, archive.ID); err != nil {
		t.Fatal(err)
	}
	exists, err := database.IMAPMessageExists(ctx, saved.ID)
	if err != nil || exists {
		t.Fatalf("last moved draft copy remained archived: exists=%v err=%v", exists, err)
	}
}

func TestIMAPDeleteMailboxCleansOnlyOrphanDrafts(t *testing.T) {
	database := testStore(t)
	user := createTestUser(t, database, "deleted-mailbox@example.com", 1024*1024)
	ctx := context.Background()
	if err := database.CreateMailbox(ctx, user.ID, "Projects"); err != nil {
		t.Fatal(err)
	}
	orphanRaw := []byte("From: deleted-mailbox@example.com\r\nSubject: orphan draft\r\n\r\nunfinished")
	orphan, err := database.SaveMessage(ctx, Message{RFCMessageID: "orphan-draft@example.com", Raw: orphanRaw,
		SizeBytes: int64(len(orphanRaw)), Direction: "draft"},
		[]Attachment{{Filename: "private.txt", ContentType: "text/plain", SizeBytes: 6, SHA256: "private", Content: []byte("secret")}},
		[]Delivery{{UserID: user.ID, Mailbox: "Projects", Flags: []string{"\\Draft"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sharedRaw := []byte("From: deleted-mailbox@example.com\r\nSubject: shared draft\r\n\r\nunfinished")
	shared, err := database.SaveMessage(ctx, Message{RFCMessageID: "shared-draft@example.com", Raw: sharedRaw,
		SizeBytes: int64(len(sharedRaw)), Direction: "draft"}, nil, []Delivery{
		{UserID: user.ID, Mailbox: "Projects", Flags: []string{"\\Draft"}},
		{UserID: user.ID, Mailbox: MailboxArchive, Flags: []string{"\\Draft"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	receivedRaw := []byte("From: sender@outside.test\r\nSubject: retained message\r\n\r\nreceived")
	received, err := database.SaveMessage(ctx, Message{RFCMessageID: "retained-after-mailbox-delete@outside.test", Raw: receivedRaw,
		SizeBytes: int64(len(receivedRaw)), Direction: "inbound"}, nil,
		[]Delivery{{UserID: user.ID, Mailbox: "Projects"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := database.IMAPDeleteMailbox(ctx, user.ID, "Projects"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.IMAPGetMailbox(ctx, user.ID, "Projects"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted custom mailbox still exists: %v", err)
	}
	if exists, err := database.IMAPMessageExists(ctx, orphan.ID); err != nil || exists {
		t.Fatalf("orphan draft/raw survived mailbox deletion: exists=%v err=%v", exists, err)
	}
	var orphanAttachments int
	if err := database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE message_id=?`, orphan.ID).Scan(&orphanAttachments); err != nil {
		t.Fatal(err)
	}
	if orphanAttachments != 0 {
		t.Fatalf("orphan draft attachments survived cascade: %d", orphanAttachments)
	}
	if exists, err := database.IMAPMessageExists(ctx, shared.ID); err != nil || !exists {
		t.Fatalf("draft with another mailbox entry was deleted: exists=%v err=%v", exists, err)
	}
	if exists, err := database.IMAPMessageExists(ctx, received.ID); err != nil || !exists {
		t.Fatalf("immutable received message archive was deleted: exists=%v err=%v", exists, err)
	}
}
