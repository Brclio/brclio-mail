package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/mailcore"
	"github.com/Brclio/brclio-mail/internal/security"
	"github.com/Brclio/brclio-mail/internal/store"
)

const DefaultQuotaBytes = int64(5 * 1024 * 1024 * 1024)

type Service struct {
	Store    *store.Store
	Config   config.Config
	setup    sync.Mutex
	Resolver TXTResolver
}

type TXTResolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}

func New(db *store.Store, cfg config.Config) *Service {
	db.SetStorageLimits(cfg.MaxArchiveBytes, cfg.MinFreeDiskBytes)
	return &Service{Store: db, Config: cfg, Resolver: net.DefaultResolver}
}

type SetupRequest struct {
	Domain      string `json:"domain"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
	SetupToken  string `json:"setupToken"`
}

func (s *Service) Setup(ctx context.Context, request SetupRequest, ip string) (store.User, store.Domain, error) {
	// Brclio Mail is deliberately single-node. Serialize first-run setup so two
	// simultaneous requests cannot both claim the initial administrator.
	s.setup.Lock()
	defer s.setup.Unlock()
	count, err := s.Store.CountUsers(ctx)
	if err != nil {
		return store.User{}, store.Domain{}, err
	}
	if count != 0 {
		return store.User{}, store.Domain{}, store.ErrConflict
	}
	if s.Config.SetupToken != "" && !security.EqualToken(request.SetupToken, s.Config.SetupToken) {
		return store.User{}, store.Domain{}, store.ErrForbidden
	}
	domainName := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(request.Domain)), ".")
	if request.Email == "" {
		request.Email = "postmaster@" + domainName
	}
	if !strings.HasSuffix(strings.ToLower(request.Email), "@"+domainName) {
		return store.User{}, store.Domain{}, errors.New("administrator email must belong to the configured domain")
	}
	passwordHash, err := security.HashPassword(request.Password)
	if err != nil {
		return store.User{}, store.Domain{}, err
	}
	publicKey, privateKey, err := GenerateDKIMKey()
	if err != nil {
		return store.User{}, store.Domain{}, err
	}
	user, domain, err := s.Store.Initialize(ctx, store.InitialSetup{
		DomainName: domainName, DKIMSelector: "brclio", DKIMPublicKey: publicKey, DKIMPrivateKey: privateKey,
		Email: request.Email, DisplayName: request.DisplayName, PasswordHash: passwordHash, QuotaBytes: DefaultQuotaBytes, IP: ip,
	})
	return user, domain, err
}

func (s *Service) Bootstrap(ctx context.Context) error {
	if s.Config.BootstrapEmail == "" {
		return nil
	}
	count, err := s.Store.CountUsers(ctx)
	if err != nil || count > 0 {
		return err
	}
	parts := strings.Split(s.Config.BootstrapEmail, "@")
	if len(parts) != 2 {
		return errors.New("invalid BRCLIO_BOOTSTRAP_EMAIL")
	}
	_, _, err = s.Setup(ctx, SetupRequest{Domain: parts[1], Email: s.Config.BootstrapEmail,
		DisplayName: "Administrator", Password: s.Config.BootstrapPassword, SetupToken: s.Config.SetupToken}, "bootstrap")
	return err
}

func (s *Service) CreateDomain(ctx context.Context, name string) (store.Domain, error) {
	return s.createDomain(ctx, name, nil)
}

func (s *Service) CreateDomainAudited(ctx context.Context, name string, event store.AuditEvent) (store.Domain, error) {
	return s.createDomain(ctx, name, &event)
}

func (s *Service) createDomain(ctx context.Context, name string, event *store.AuditEvent) (store.Domain, error) {
	publicKey, privateKey, err := GenerateDKIMKey()
	if err != nil {
		return store.Domain{}, err
	}
	if event != nil {
		return s.Store.CreateDomainAudited(ctx, name, "brclio", publicKey, privateKey, *event)
	}
	return s.Store.CreateDomain(ctx, name, "brclio", publicKey, privateKey)
}

func GenerateDKIMKey() (publicKey, privateKey string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	privateDER := x509.MarshalPKCS1PrivateKey(key)
	privateKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privateDER}))
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	publicKey = base64.StdEncoding.EncodeToString(publicDER)
	return publicKey, privateKey, nil
}

func (s *Service) Send(ctx context.Context, actor store.User, request mailcore.ComposeRequest, ip string) (store.Message, error) {
	request.From = actor.Email
	raw, err := mailcore.Compose(request, s.Config.Hostname, time.Now().UTC())
	if err != nil {
		return store.Message{}, err
	}
	if int64(len(raw)) > s.maxMessageBytes() {
		return store.Message{}, fmt.Errorf("message exceeds the configured size limit")
	}
	recipients := append(append(append([]string{}, request.To...), request.CC...), request.BCC...)
	envelopeRecipients := canonicalRecipients(recipients)
	parsed, attachments, err := mailcore.Parse(raw, mailcore.Envelope{From: actor.Email, To: envelopeRecipients, Direction: "outbound"})
	if err != nil {
		return store.Message{}, err
	}
	parsed.TransportStatus = "queued"
	deliveries := []store.Delivery{{UserID: actor.ID, Mailbox: store.MailboxSent, Flags: []string{"\\Seen"}}}
	queue := []string{}
	deliveredUsers := map[string]bool{}
	for _, recipient := range envelopeRecipients {
		user, resolveErr := s.Store.ResolveRecipient(ctx, recipient)
		if resolveErr == nil {
			if !deliveredUsers[user.ID] {
				deliveries = append(deliveries, store.Delivery{UserID: user.ID, Mailbox: store.MailboxInbox})
				deliveredUsers[user.ID] = true
			}
			continue
		}
		if !errors.Is(resolveErr, store.ErrNotFound) {
			return store.Message{}, resolveErr
		}
		queue = append(queue, recipient)
	}
	if len(queue) == 0 {
		parsed.TransportStatus = "delivered"
	} else if !s.Config.DevMode {
		_, senderDomain, _ := strings.Cut(actor.Email, "@")
		domain, domainErr := s.Store.GetDomainByName(ctx, senderDomain)
		if domainErr != nil {
			return store.Message{}, domainErr
		}
		if domain.Status != "verified" {
			return store.Message{}, store.ErrDomainUnverified
		}
	}
	event := store.AuditEvent{ActorID: actor.ID, Action: "message.submit", TargetType: "message", IP: ip}
	if request.DraftID != "" {
		return s.Store.SaveMessageReplacingDraftAudited(ctx, actor.ID, request.DraftID, parsed, attachments, deliveries, queue, event)
	}
	return s.Store.SaveMessageAudited(ctx, parsed, attachments, deliveries, queue, event)
}

func (s *Service) VerifyDomain(ctx context.Context, id string) (store.Domain, error) {
	return s.verifyDomain(ctx, id, nil)
}

func (s *Service) VerifyDomainAudited(ctx context.Context, id string, event store.AuditEvent) (store.Domain, error) {
	return s.verifyDomain(ctx, id, &event)
}

func (s *Service) verifyDomain(ctx context.Context, id string, event *store.AuditEvent) (store.Domain, error) {
	domain, err := s.Store.GetDomainByID(ctx, id)
	if err != nil {
		return store.Domain{}, err
	}
	resolver := s.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	records, err := resolver.LookupTXT(ctx, "_brclio-mail."+domain.Name)
	if err != nil {
		return store.Domain{}, fmt.Errorf("lookup ownership TXT record: %w", err)
	}
	verified := false
	for _, record := range records {
		if security.EqualToken(strings.TrimSpace(record), domain.Verification) {
			verified = true
			break
		}
	}
	if !verified {
		return store.Domain{}, store.ErrDomainUnverified
	}
	if event != nil {
		err = s.Store.SetDomainVerificationAudited(ctx, domain.ID, true, *event)
	} else {
		err = s.Store.SetDomainVerification(ctx, domain.ID, true)
	}
	if err != nil {
		return store.Domain{}, err
	}
	return s.Store.GetDomainByID(ctx, domain.ID)
}

func (s *Service) SaveDraft(ctx context.Context, actor store.User, request mailcore.ComposeRequest, replaceID, ip string) (store.Message, error) {
	request.From = actor.Email
	request.AllowNoRecipients = true
	raw, err := mailcore.Compose(request, s.Config.Hostname, time.Now().UTC())
	if err != nil {
		return store.Message{}, err
	}
	if int64(len(raw)) > s.maxMessageBytes() {
		return store.Message{}, fmt.Errorf("message exceeds the configured size limit")
	}
	recipients := canonicalRecipients(append(append(append([]string{}, request.To...), request.CC...), request.BCC...))
	parsed, attachments, err := mailcore.Parse(raw, mailcore.Envelope{From: actor.Email, To: recipients, Direction: "draft"})
	if err != nil {
		return store.Message{}, err
	}
	parsed.TransportStatus = "draft"
	deliveries := []store.Delivery{{UserID: actor.ID, Mailbox: store.MailboxDrafts, Flags: []string{"\\Draft", "\\Seen"}}}
	event := store.AuditEvent{ActorID: actor.ID, Action: "draft.save", TargetType: "message", IP: ip}
	if replaceID != "" {
		return s.Store.SaveMessageReplacingDraftAudited(ctx, actor.ID, replaceID, parsed, attachments, deliveries, nil, event)
	}
	return s.Store.SaveMessageAudited(ctx, parsed, attachments, deliveries, nil, event)
}

func (s *Service) maxMessageBytes() int64 {
	if s.Config.MaxMessageBytes > 0 {
		return s.Config.MaxMessageBytes
	}
	return 25 * 1024 * 1024
}

func (s *Service) Receive(ctx context.Context, envelopeFrom string, recipients []string, raw []byte, ip string) (store.Message, error) {
	deliveries := []store.Delivery{}
	deliveredUsers := map[string]bool{}
	for _, recipient := range canonicalRecipients(recipients) {
		user, err := s.Store.ResolveRecipient(ctx, recipient)
		if err != nil {
			return store.Message{}, err
		}
		if !deliveredUsers[user.ID] {
			deliveries = append(deliveries, store.Delivery{UserID: user.ID, Mailbox: store.MailboxInbox})
			deliveredUsers[user.ID] = true
		}
	}
	parsed, attachments, err := mailcore.Parse(raw, mailcore.Envelope{From: envelopeFrom, To: recipients, Direction: "inbound"})
	if err != nil {
		return store.Message{}, err
	}
	return s.Store.SaveMessageAudited(ctx, parsed, attachments, deliveries, nil,
		store.AuditEvent{Action: "message.receive", TargetType: "message", IP: ip})
}

func canonicalRecipients(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		address, err := mail.ParseAddress(value)
		if err != nil {
			continue
		}
		normalized := strings.ToLower(address.Address)
		if !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	return result
}

func (s *Service) AdminView(ctx context.Context, actor store.User, messageID, reason, ip string) (store.Message, error) {
	if actor.Role != store.RoleAdmin {
		return store.Message{}, store.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 4 {
		return store.Message{}, errors.New("a review reason of at least 4 characters is required")
	}
	message, err := s.Store.GetMessage(ctx, "", messageID, true)
	if err != nil {
		return store.Message{}, err
	}
	if err := s.Store.Audit(ctx, store.AuditEvent{ActorID: actor.ID, Action: "archive.message.view", TargetType: "message", TargetID: message.ID, Reason: reason, IP: ip}); err != nil {
		return store.Message{}, fmt.Errorf("record mandatory archive audit event: %w", err)
	}
	return message, nil
}
