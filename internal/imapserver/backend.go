package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	"github.com/Brclio/brclio-mail/internal/mailcore"
	mailstore "github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-message"
	messageTextProto "github.com/emersion/go-message/textproto"
)

var supportedFlags = []string{
	imap.SeenFlag,
	imap.AnsweredFlag,
	imap.FlaggedFlag,
	imap.DeletedFlag,
	imap.DraftFlag,
	imap.RecentFlag,
}

var permanentFlags = []string{
	imap.SeenFlag,
	imap.AnsweredFlag,
	imap.FlaggedFlag,
	imap.DeletedFlag,
	imap.DraftFlag,
}

type storeBackend struct {
	store           *mailstore.Store
	maxMessageBytes int64
	authLimiter     *authlimit.Limiter
	guard           *preAuthGuard
	limiterOnce     sync.Once
}

var _ backend.AppendLimitBackend = (*storeBackend)(nil)
var _ backend.AppendLimitUser = (*user)(nil)
var _ backend.MoveMailbox = (*mailbox)(nil)

func (b *storeBackend) Login(connection *imap.ConnInfo, username, password string) (backend.User, error) {
	b.limiterOnce.Do(func() {
		if b.authLimiter == nil {
			b.authLimiter = authlimit.NewDefault()
		}
	})
	remoteIP := "unknown"
	if connection != nil {
		remoteIP = authlimit.RemoteIP(connection.RemoteAddr)
	}
	accountName := authlimit.NormalizeAccount(username)
	if !b.authLimiter.Allow(remoteIP, accountName) {
		return nil, backend.ErrInvalidCredentials
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	account, err := b.store.Authenticate(ctx, accountName, password, true)
	if err != nil {
		b.authLimiter.Failure(remoteIP, accountName)
		return nil, backend.ErrInvalidCredentials
	}
	b.authLimiter.Success(accountName)
	if b.guard != nil && connection != nil {
		b.guard.authenticate(connection.RemoteAddr, connection.LocalAddr)
	}
	return &user{backend: b, account: account, remoteIP: remoteIP}, nil
}

func (b *storeBackend) CreateMessageLimit() *uint32 {
	limit := uint32Limit(b.maxMessageBytes)
	return &limit
}

type user struct {
	backend  *storeBackend
	account  mailstore.User
	remoteIP string
}

func (u *user) Username() string { return u.account.Email }

func (u *user) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	if err := u.ensureActive(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, err := u.backend.store.IMAPListMailboxes(ctx, u.account.ID, subscribed)
	if err != nil {
		return nil, err
	}
	result := make([]backend.Mailbox, 0, len(items))
	for _, item := range items {
		result = append(result, &mailbox{user: u, metadata: item})
	}
	return result, nil
}

func (u *user) GetMailbox(name string) (backend.Mailbox, error) {
	if err := u.ensureActive(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	item, err := u.backend.store.IMAPGetMailbox(ctx, u.account.ID, imap.CanonicalMailboxName(name))
	if errors.Is(err, mailstore.ErrNotFound) {
		return nil, backend.ErrNoSuchMailbox
	}
	if err != nil {
		return nil, err
	}
	return &mailbox{user: u, metadata: item}, nil
}

func (u *user) CreateMailbox(name string) error {
	if err := u.ensureActive(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := u.backend.store.CreateMailbox(ctx, u.account.ID, imap.CanonicalMailboxName(name))
	if errors.Is(err, mailstore.ErrConflict) {
		return backend.ErrMailboxAlreadyExists
	}
	return err
}

func (u *user) DeleteMailbox(name string) error {
	if err := u.ensureActive(); err != nil {
		return err
	}
	name = imap.CanonicalMailboxName(name)
	if isSystemMailbox(name) {
		return mailstore.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := u.backend.store.IMAPDeleteMailbox(ctx, u.account.ID, name)
	if errors.Is(err, mailstore.ErrNotFound) {
		return backend.ErrNoSuchMailbox
	}
	return err
}

func (u *user) RenameMailbox(existingName, newName string) error {
	if err := u.ensureActive(); err != nil {
		return err
	}
	existingName = imap.CanonicalMailboxName(existingName)
	newName = imap.CanonicalMailboxName(newName)
	if isSystemMailbox(existingName) {
		return mailstore.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := u.backend.store.RenameMailbox(ctx, u.account.ID, existingName, newName)
	if errors.Is(err, mailstore.ErrNotFound) {
		return backend.ErrNoSuchMailbox
	}
	if errors.Is(err, mailstore.ErrConflict) {
		return backend.ErrMailboxAlreadyExists
	}
	return err
}

func (u *user) Logout() error { return nil }

func (u *user) CreateMessageLimit() *uint32 { return u.backend.CreateMessageLimit() }

func (u *user) ensureActive() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	account, err := u.backend.store.GetUserByID(ctx, u.account.ID)
	if err != nil {
		if errors.Is(err, mailstore.ErrNotFound) {
			return mailstore.ErrForbidden
		}
		return err
	}
	if account.Status != mailstore.StatusActive || account.ProtocolAuthVersion != u.account.ProtocolAuthVersion {
		return mailstore.ErrForbidden
	}
	return nil
}

type mailbox struct {
	user     *user
	metadata mailstore.IMAPMailbox
}

func (m *mailbox) Name() string { return m.metadata.Name }

func (m *mailbox) Info() (*imap.MailboxInfo, error) {
	if err := m.user.ensureActive(); err != nil {
		return nil, err
	}
	return &imap.MailboxInfo{Name: m.metadata.Name, Delimiter: "/"}, nil
}

func (m *mailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	metadata, entries, err := m.snapshot()
	if err != nil {
		return nil, err
	}
	status := imap.NewMailboxStatus(metadata.Name, items)
	status.Flags = append([]string(nil), supportedFlags...)
	status.PermanentFlags = append([]string(nil), permanentFlags...)
	for index, entry := range entries {
		if !hasFlag(entry.Flags, imap.SeenFlag) {
			status.Unseen++
			if status.UnseenSeqNum == 0 {
				status.UnseenSeqNum = uint32(index + 1)
			}
		}
		if hasFlag(entry.Flags, imap.RecentFlag) {
			status.Recent++
		}
	}
	status.Messages = uint32(len(entries))
	status.UidNext = metadata.UIDNext
	status.UidValidity = metadata.UIDValidity
	status.AppendLimit = uint32Limit(m.user.backend.maxMessageBytes)
	return status, nil
}

func (m *mailbox) SetSubscribed(subscribed bool) error {
	if err := m.user.ensureActive(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return m.user.backend.store.SetMailboxSubscribed(ctx, m.user.account.ID, m.metadata.Name, subscribed)
}

func (m *mailbox) Check() error { return m.user.ensureActive() }

func (m *mailbox) ListMessages(uid bool, seqset *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)
	_, entries, err := m.snapshot()
	if err != nil {
		return err
	}
	maxSeq, maxUID := maxima(entries)
	for index := range entries {
		entry := &entries[index]
		seqNum := uint32(index + 1)
		identifier, maximum := seqNum, maxSeq
		if uid {
			identifier, maximum = entry.UID, maxUID
		}
		if !seqSetContains(seqset, identifier, maximum) {
			continue
		}
		fetched, err := m.fetch(entry, seqNum, items)
		if err != nil {
			return err
		}
		ch <- fetched
	}
	return nil
}

func (m *mailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	_, entries, err := m.snapshot()
	if err != nil {
		return nil, err
	}
	maxSeq, maxUID := maxima(entries)
	normalized := normalizeCriteria(criteria, maxSeq, maxUID)
	result := make([]uint32, 0)
	for index, entry := range entries {
		seqNum := uint32(index + 1)
		raw, err := m.raw(entry)
		if err != nil {
			return nil, err
		}
		entity, err := message.Read(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		matched, err := backendutil.Match(entity, seqNum, entry.UID, entry.InternalDate, entry.Flags, normalized)
		if err != nil {
			return nil, err
		}
		if matched {
			if uid {
				result = append(result, entry.UID)
			} else {
				result = append(result, seqNum)
			}
		}
	}
	return result, nil
}

func (m *mailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if err := m.user.ensureActive(); err != nil {
		return err
	}
	raw, err := readLiteral(body, m.user.backend.maxMessageBytes)
	if err != nil {
		if errors.Is(err, errTooBig) {
			return backend.ErrTooBig
		}
		return err
	}
	direction := "append"
	if strings.EqualFold(m.metadata.Name, mailstore.MailboxDrafts) || hasFlag(flags, imap.DraftFlag) {
		direction = "draft"
	}
	parsed, attachments, err := mailcore.Parse(raw, mailcore.Envelope{Direction: direction})
	if err != nil {
		return err
	}
	if date.IsZero() {
		date = time.Now().UTC()
	}
	parsed.ReceivedAt = date.UTC()
	// IMAP's APPEND date is mailbox metadata supplied by the client. It must
	// never control the server-side archival creation timestamp.
	parsed.CreatedAt = time.Time{}
	if !hasFlag(flags, imap.RecentFlag) {
		flags = append(append([]string(nil), flags...), imap.RecentFlag)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	remoteIP := m.user.remoteIP
	if remoteIP == "" {
		remoteIP = "unknown"
	}
	metadata, _ := json.Marshal(map[string]string{"protocol": "imap"})
	action := "message.append"
	if direction == "draft" {
		action = "draft.save"
	}
	_, err = m.user.backend.store.SaveMessageAudited(ctx, parsed, attachments,
		[]mailstore.Delivery{{UserID: m.user.account.ID, Mailbox: m.metadata.Name, Flags: flags}}, nil,
		mailstore.AuditEvent{ActorID: m.user.account.ID, Action: action, TargetType: "message", IP: remoteIP, Metadata: string(metadata)})
	if errors.Is(err, mailstore.ErrQuotaExceeded) {
		return backend.ErrTooBig
	}
	return err
}

func (m *mailbox) UpdateMessagesFlags(uid bool, seqset *imap.SeqSet, operation imap.FlagsOp, flags []string) error {
	_, entries, err := m.snapshot()
	if err != nil {
		return err
	}
	maxSeq, maxUID := maxima(entries)
	for index, entry := range entries {
		identifier, maximum := uint32(index+1), maxSeq
		if uid {
			identifier, maximum = entry.UID, maxUID
		}
		if !seqSetContains(seqset, identifier, maximum) {
			continue
		}
		updated := backendutil.UpdateFlags(append([]string(nil), entry.Flags...), operation, canonicalFlags(flags))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = m.user.backend.store.IMAPSetEntryFlags(ctx, m.user.account.ID, m.metadata.ID, entry.ID, updated)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *mailbox) CopyMessages(uid bool, seqset *imap.SeqSet, destination string) error {
	entries, err := m.selectedEntries(uid, seqset)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = m.user.backend.store.IMAPCopyEntries(ctx, m.user.account.ID, imap.CanonicalMailboxName(destination), entries)
	if errors.Is(err, mailstore.ErrNotFound) {
		return backend.ErrNoSuchMailbox
	}
	return err
}

func (m *mailbox) MoveMessages(uid bool, seqset *imap.SeqSet, destination string) error {
	entries, err := m.selectedEntries(uid, seqset)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = m.user.backend.store.IMAPMoveEntries(ctx, m.user.account.ID, m.metadata.ID, imap.CanonicalMailboxName(destination), entries)
	if errors.Is(err, mailstore.ErrNotFound) {
		return backend.ErrNoSuchMailbox
	}
	return err
}

func (m *mailbox) Expunge() error {
	if err := m.user.ensureActive(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.user.backend.store.IMAPExpungeDeleted(ctx, m.user.account.ID, m.metadata.ID)
	return err
}

func (m *mailbox) snapshot() (mailstore.IMAPMailbox, []mailstore.IMAPEntry, error) {
	if err := m.user.ensureActive(); err != nil {
		return mailstore.IMAPMailbox{}, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	metadata, err := m.user.backend.store.IMAPGetMailbox(ctx, m.user.account.ID, m.metadata.Name)
	if err != nil {
		return mailstore.IMAPMailbox{}, nil, err
	}
	entries, err := m.user.backend.store.IMAPListEntries(ctx, m.user.account.ID, metadata.ID)
	if err != nil {
		return mailstore.IMAPMailbox{}, nil, err
	}
	m.metadata = metadata
	return metadata, entries, nil
}

func (m *mailbox) selectedEntries(uid bool, seqset *imap.SeqSet) ([]mailstore.IMAPEntry, error) {
	_, entries, err := m.snapshot()
	if err != nil {
		return nil, err
	}
	maxSeq, maxUID := maxima(entries)
	result := make([]mailstore.IMAPEntry, 0)
	for index, entry := range entries {
		identifier, maximum := uint32(index+1), maxSeq
		if uid {
			identifier, maximum = entry.UID, maxUID
		}
		if seqSetContains(seqset, identifier, maximum) {
			result = append(result, entry)
		}
	}
	return result, nil
}

func (m *mailbox) fetch(entry *mailstore.IMAPEntry, seqNum uint32, items []imap.FetchItem) (*imap.Message, error) {
	markSeen := false
	for _, item := range items {
		section, err := imap.ParseBodySectionName(item)
		if err == nil && !section.Peek {
			markSeen = true
		}
	}
	if markSeen && !hasFlag(entry.Flags, imap.SeenFlag) {
		entry.Flags = append(entry.Flags, imap.SeenFlag)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := m.user.backend.store.IMAPSetEntryFlags(ctx, m.user.account.ID, m.metadata.ID, entry.ID, entry.Flags)
		cancel()
		if err != nil {
			return nil, err
		}
	}

	needsRaw := false
	for _, item := range items {
		if item == imap.FetchEnvelope || item == imap.FetchBody || item == imap.FetchBodyStructure {
			needsRaw = true
			break
		}
		if _, err := imap.ParseBodySectionName(item); err == nil {
			needsRaw = true
			break
		}
	}
	var raw []byte
	var err error
	if needsRaw {
		raw, err = m.raw(*entry)
		if err != nil {
			return nil, err
		}
	}

	fetched := imap.NewMessage(seqNum, items)
	for _, item := range items {
		switch item {
		case imap.FetchEnvelope:
			header, _, err := headerAndBody(raw)
			if err != nil {
				return nil, err
			}
			fetched.Envelope, err = backendutil.FetchEnvelope(header)
			if err != nil {
				return nil, err
			}
		case imap.FetchBody, imap.FetchBodyStructure:
			header, body, err := headerAndBody(raw)
			if err != nil {
				return nil, err
			}
			fetched.BodyStructure, err = backendutil.FetchBodyStructure(header, body, item == imap.FetchBodyStructure)
			if err != nil {
				return nil, err
			}
		case imap.FetchFlags:
			fetched.Flags = append([]string(nil), entry.Flags...)
		case imap.FetchInternalDate:
			fetched.InternalDate = entry.InternalDate
		case imap.FetchRFC822Size:
			fetched.Size = uint32Limit(entry.SizeBytes)
		case imap.FetchUid:
			fetched.Uid = entry.UID
		default:
			section, err := imap.ParseBodySectionName(item)
			if err != nil {
				continue
			}
			header, body, err := headerAndBody(raw)
			if err != nil {
				return nil, err
			}
			literal, err := backendutil.FetchBodySection(header, body, section)
			if err != nil {
				return nil, err
			}
			fetched.Body[section] = literal
		}
	}
	return fetched, nil
}

func (m *mailbox) raw(entry mailstore.IMAPEntry) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.user.backend.store.IMAPEntryRaw(ctx, m.user.account.ID, m.metadata.ID, entry.ID)
}

func headerAndBody(raw []byte) (messageTextProto.Header, io.Reader, error) {
	body := bufio.NewReader(bytes.NewReader(raw))
	header, err := messageTextProto.ReadHeader(body)
	return header, body, err
}

func maxima(entries []mailstore.IMAPEntry) (uint32, uint32) {
	maxSeq := uint32(len(entries))
	var maxUID uint32
	for _, entry := range entries {
		if entry.UID > maxUID {
			maxUID = entry.UID
		}
	}
	return maxSeq, maxUID
}

func seqSetContains(set *imap.SeqSet, value, maximum uint32) bool {
	if set == nil || maximum == 0 {
		return false
	}
	for _, item := range set.Set {
		start, stop := item.Start, item.Stop
		if start == 0 {
			start = maximum
		}
		if stop == 0 {
			stop = maximum
		}
		if start > stop {
			start, stop = stop, start
		}
		if value >= start && value <= stop {
			return true
		}
	}
	return false
}

func normalizeCriteria(criteria *imap.SearchCriteria, maxSeq, maxUID uint32) *imap.SearchCriteria {
	if criteria == nil {
		return imap.NewSearchCriteria()
	}
	clone := *criteria
	clone.Header = make(map[string][]string, len(criteria.Header))
	for key, values := range criteria.Header {
		clone.Header[key] = append([]string(nil), values...)
	}
	clone.SeqNum = normalizeSeqSet(criteria.SeqNum, maxSeq)
	clone.Uid = normalizeSeqSet(criteria.Uid, maxUID)
	clone.Not = make([]*imap.SearchCriteria, len(criteria.Not))
	for index, nested := range criteria.Not {
		clone.Not[index] = normalizeCriteria(nested, maxSeq, maxUID)
	}
	clone.Or = make([][2]*imap.SearchCriteria, len(criteria.Or))
	for index, pair := range criteria.Or {
		clone.Or[index] = [2]*imap.SearchCriteria{
			normalizeCriteria(pair[0], maxSeq, maxUID),
			normalizeCriteria(pair[1], maxSeq, maxUID),
		}
	}
	return &clone
}

func normalizeSeqSet(set *imap.SeqSet, maximum uint32) *imap.SeqSet {
	if set == nil {
		return nil
	}
	result := new(imap.SeqSet)
	if maximum == 0 {
		return result
	}
	for _, item := range set.Set {
		start, stop := item.Start, item.Stop
		if start == 0 {
			start = maximum
		}
		if stop == 0 {
			stop = maximum
		}
		result.AddRange(start, stop)
	}
	return result
}

func canonicalFlags(flags []string) []string {
	result := make([]string, 0, len(flags))
	for _, flag := range flags {
		canonical := imap.CanonicalFlag(flag)
		for _, supported := range supportedFlags {
			if strings.EqualFold(canonical, supported) && !hasFlag(result, supported) {
				result = append(result, supported)
			}
		}
	}
	return result
}

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}

func isSystemMailbox(name string) bool {
	for _, systemName := range mailstore.SystemMailboxes {
		if strings.EqualFold(name, systemName) {
			return true
		}
	}
	return false
}

var errTooBig = errors.New("literal too big")

func readLiteral(literal imap.Literal, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		maximum = 25 * 1024 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(literal, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errTooBig
	}
	return data, nil
}

func uint32Limit(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}
