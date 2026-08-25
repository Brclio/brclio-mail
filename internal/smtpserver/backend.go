package smtpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	"github.com/Brclio/brclio-mail/internal/mailcore"
	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type mode uint8

const (
	modeInbound mode = iota
	modeSubmission
)

type backend struct {
	store           *store.Store
	mode            mode
	maxMessageBytes int64
	authLimiter     *authlimit.Limiter
	limiterOnce     sync.Once
	endpoint        string
}

var _ smtp.Backend = (*backend)(nil)
var _ smtp.Session = (*inboundSession)(nil)
var _ smtp.AuthSession = (*submissionSession)(nil)

func (b *backend) NewSession(connection *smtp.Conn) (smtp.Session, error) {
	b.limiterOnce.Do(func() {
		if b.authLimiter == nil {
			b.authLimiter = authlimit.NewDefault()
		}
	})
	remoteIP := "unknown"
	if connection != nil && connection.Conn() != nil {
		remoteIP = authlimit.RemoteIP(connection.Conn().RemoteAddr())
	}
	endpoint := b.endpoint
	if endpoint == "" {
		endpoint = EndpointSMTP
		if b.mode == modeSubmission {
			endpoint = EndpointSubmission
		}
	}
	tx := &transactionSession{store: b.store, mode: b.mode, maxMessageBytes: b.maxMessageBytes,
		authLimiter: b.authLimiter, remoteIP: remoteIP, endpoint: endpoint}
	if b.mode == modeSubmission {
		return &submissionSession{transactionSession: tx}, nil
	}
	// Keep inbound as a distinct concrete type: if it implemented AuthSession,
	// go-smtp would advertise AUTH on the port-25 listener.
	return &inboundSession{transactionSession: tx}, nil
}

type recipient struct {
	address string
	user    store.User
	local   bool
}

type transactionSession struct {
	store           *store.Store
	mode            mode
	maxMessageBytes int64
	authenticated   *store.User
	authLimiter     *authlimit.Limiter
	remoteIP        string
	endpoint        string
	from            string
	recipients      []recipient
}

type inboundSession struct{ *transactionSession }
type submissionSession struct{ *transactionSession }

func (s *submissionSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *submissionSession) Auth(mechanism string) (sasl.Server, error) {
	if !strings.EqualFold(mechanism, sasl.Plain) {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		account := authlimit.NormalizeAccount(username)
		if !s.authLimiter.Allow(s.remoteIP, account) {
			return smtp.ErrAuthFailed
		}
		if identity != "" && !strings.EqualFold(identity, username) {
			s.authLimiter.Failure(s.remoteIP, account)
			return smtp.ErrAuthFailed
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		user, err := s.store.Authenticate(ctx, account, password, true)
		if err != nil {
			s.authLimiter.Failure(s.remoteIP, account)
			return smtp.ErrAuthFailed
		}
		s.authLimiter.Success(account)
		s.authenticated = &user
		return nil
	}), nil
}

func (s *transactionSession) Mail(from string, _ *smtp.MailOptions) error {
	s.from = ""
	s.recipients = nil
	if s.mode == modeSubmission {
		if err := s.requireActiveAuthentication(); err != nil {
			return err
		}
		allowed, err := s.store.SMTPFromAllowed(context.Background(), s.authenticated.ID, from)
		if err != nil {
			return temporarySMTPError("could not validate sender")
		}
		if !allowed {
			return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "sender address is not owned by authenticated user"}
		}
	}
	if from != "" {
		parsed, err := mail.ParseAddress(from)
		if err != nil || !strings.Contains(parsed.Address, "@") {
			return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 1, 7}, Message: "invalid reverse-path"}
		}
		s.from = strings.ToLower(parsed.Address)
	}
	return nil
}

func (s *transactionSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.mode == modeSubmission {
		if err := s.requireActiveAuthentication(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	canonical, user, err := s.store.ResolveSMTPRecipient(ctx, to)
	if err == nil {
		if !s.hasRecipient(canonical) {
			s.recipients = append(s.recipients, recipient{address: canonical, user: user, local: true})
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return temporarySMTPError("could not resolve recipient")
	}
	if s.mode == modeInbound {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such local recipient"}
	}

	canonical, local, err := s.store.SMTPLocalDomain(ctx, to)
	if err != nil {
		return temporarySMTPError("could not resolve recipient domain")
	}
	if local {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such local recipient"}
	}
	parsed, parseErr := mail.ParseAddress(to)
	if parseErr != nil || !strings.Contains(parsed.Address, "@") {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "invalid recipient address"}
	}
	canonical = strings.ToLower(parsed.Address)
	if !s.hasRecipient(canonical) {
		s.recipients = append(s.recipients, recipient{address: canonical})
	}
	return nil
}

func (s *transactionSession) Data(reader io.Reader) error {
	if s.mode == modeSubmission {
		if err := s.requireActiveAuthentication(); err != nil {
			return err
		}
	}
	if len(s.recipients) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "no valid recipients"}
	}
	raw, err := readMessage(reader, s.maxMessageBytes)
	if err != nil {
		if errors.Is(err, errMessageTooLarge) {
			return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "message exceeds fixed maximum size"}
		}
		return temporarySMTPError("could not read message")
	}
	if s.mode == modeSubmission {
		if err := s.validateHeaderFrom(raw); err != nil {
			return err
		}
		raw = stripBccHeader(raw)
	} else if err := s.validateInboundHeaderFrom(raw); err != nil {
		return err
	}

	envelopeRecipients := make([]string, 0, len(s.recipients))
	deliveries := make([]store.Delivery, 0, len(s.recipients)+1)
	queued := make([]string, 0, len(s.recipients))
	deliveryKeys := make(map[string]struct{})
	for _, rcpt := range s.recipients {
		envelopeRecipients = append(envelopeRecipients, rcpt.address)
		if rcpt.local {
			key := rcpt.user.ID + "\x00" + store.MailboxInbox
			if _, exists := deliveryKeys[key]; !exists {
				deliveryKeys[key] = struct{}{}
				deliveries = append(deliveries, store.Delivery{UserID: rcpt.user.ID, Mailbox: store.MailboxInbox, Flags: []string{"\\Recent"}})
			}
		} else {
			queued = append(queued, rcpt.address)
		}
	}
	direction := "inbound"
	if s.mode == modeSubmission {
		direction = "outbound"
		key := s.authenticated.ID + "\x00" + store.MailboxSent
		if _, exists := deliveryKeys[key]; !exists {
			deliveries = append(deliveries, store.Delivery{UserID: s.authenticated.ID, Mailbox: store.MailboxSent, Flags: []string{"\\Seen"}})
		}
	}
	message, attachments, err := mailcore.Parse(raw, mailcore.Envelope{From: s.from, To: envelopeRecipients, Direction: direction})
	if err != nil {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "malformed MIME message"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	metadata, _ := json.Marshal(map[string]string{"endpoint": s.endpoint, "protocol": "smtp"})
	event := store.AuditEvent{Action: "message.receive", TargetType: "message",
		IP: s.remoteIP, Metadata: string(metadata)}
	if s.mode == modeSubmission {
		event.Action = "message.submit"
		event.ActorID = s.authenticated.ID
	}
	_, err = s.store.SaveMessageAudited(ctx, message, attachments, deliveries, queued, event)
	if err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) {
			return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "mailbox quota exceeded"}
		}
		return temporarySMTPError("message storage failed")
	}
	return nil
}

func (s *transactionSession) Reset() {
	s.from = ""
	s.recipients = nil
}

func (s *transactionSession) Logout() error {
	s.Reset()
	s.authenticated = nil
	return nil
}

func (s *transactionSession) requireActiveAuthentication() error {
	if s.authenticated == nil {
		return smtp.ErrAuthRequired
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	account, err := s.store.GetUserByID(ctx, s.authenticated.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.authenticated = nil
			return smtp.ErrAuthRequired
		}
		return temporarySMTPError("could not validate authenticated account")
	}
	if account.Status != store.StatusActive || account.ProtocolAuthVersion != s.authenticated.ProtocolAuthVersion {
		s.authenticated = nil
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *transactionSession) hasRecipient(address string) bool {
	for _, item := range s.recipients {
		if strings.EqualFold(item.address, address) {
			return true
		}
	}
	return false
}

func (s *transactionSession) validateHeaderFrom(raw []byte) error {
	fromValues, err := rawHeaderValues(raw, "From")
	if err != nil {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "malformed message headers"}
	}
	if len(fromValues) != 1 {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "exactly one From header is required"}
	}
	addresses, err := mail.ParseAddressList(fromValues[0])
	if err != nil || len(addresses) != 1 {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "exactly one valid From address is required"}
	}
	allowed, err := s.store.SMTPFromAllowed(context.Background(), s.authenticated.ID, addresses[0].Address)
	if err != nil {
		return temporarySMTPError("could not validate message From header")
	}
	if !allowed {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "From header is not owned by authenticated user"}
	}
	return nil
}

func (s *transactionSession) validateInboundHeaderFrom(raw []byte) error {
	fromValues, err := rawHeaderValues(raw, "From")
	if err != nil {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "malformed message headers"}
	}
	if len(fromValues) != 1 {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "inbound mail requires exactly one From header"}
	}
	addresses, err := mail.ParseAddressList(fromValues[0])
	if err != nil || len(addresses) == 0 {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "inbound mail requires a valid From address"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, address := range addresses {
		_, local, err := s.store.SMTPLocalDomain(ctx, address.Address)
		if err != nil {
			return temporarySMTPError("could not validate message From domain")
		}
		if local {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "unauthenticated inbound mail cannot use a local From domain"}
		}
	}
	return nil
}

func rawHeaderValues(raw []byte, name string) ([]string, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var result []string
	for key, values := range parsed.Header {
		if strings.EqualFold(key, name) {
			result = append(result, values...)
		}
	}
	return result, nil
}

// stripBccHeader removes every Bcc and Resent-Bcc field, including folded
// continuation lines, while preserving all other raw MIME bytes and the body.
// Envelope recipients remain authoritative for blind-copy delivery.
func stripBccHeader(raw []byte) []byte {
	lines, body, ok := splitRawHeader(raw)
	if !ok {
		return raw
	}
	kept := make([][]byte, 0, len(lines))
	skipping := false
	removed := false
	for _, line := range lines {
		continuation := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		if continuation {
			if skipping {
				continue
			}
		} else {
			skipping = false
			if colon := bytes.IndexByte(line, ':'); colon > 0 {
				name := strings.TrimSpace(string(line[:colon]))
				if strings.EqualFold(name, "Bcc") || strings.EqualFold(name, "Resent-Bcc") {
					skipping = true
					removed = true
					continue
				}
			}
		}
		kept = append(kept, line)
	}
	if !removed {
		return raw
	}
	// Canonical CRLF output prevents mixed-line-ending boundaries from leaving
	// a hidden header behind while keeping the body bytes untouched.
	var output bytes.Buffer
	for _, line := range kept {
		output.Write(line)
		output.WriteString("\r\n")
	}
	output.WriteString("\r\n")
	output.Write(body)
	result := output.Bytes()
	return result
}

// splitRawHeader follows net/textproto's line model: LF terminates a line and
// an optional preceding CR is not part of its content. This recognizes CRLF,
// LF, and mixed line endings without scanning into the message body.
func splitRawHeader(raw []byte) ([][]byte, []byte, bool) {
	var lines [][]byte
	for offset := 0; offset < len(raw); {
		relativeEnd := bytes.IndexByte(raw[offset:], '\n')
		if relativeEnd < 0 {
			return nil, nil, false
		}
		lineEnd := offset + relativeEnd
		line := raw[offset:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		next := lineEnd + 1
		if len(line) == 0 {
			return lines, raw[next:], true
		}
		lines = append(lines, line)
		offset = next
	}
	return nil, nil, false
}

var errMessageTooLarge = errors.New("message too large")

func readMessage(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 25 * 1024 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errMessageTooLarge
	}
	return data, nil
}

func temporarySMTPError(message string) error {
	return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: fmt.Sprintf("%s; try again later", message)}
}
