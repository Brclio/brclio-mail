package queue

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type Options struct {
	Hostname         string
	PollInterval     time.Duration
	MaxAttempts      int
	RelayAddr        string
	RelayUsername    string
	RelayPassword    string
	RelayImplicitTLS bool
	DirectDelivery   bool
	BatchSize        int
}

type Deliverer interface {
	Deliver(ctx context.Context, message store.Message, recipient string) error
}

type Worker struct {
	store     *store.Store
	options   Options
	logger    *slog.Logger
	deliverer Deliverer
	wake      chan struct{}
	once      sync.Once
}

func New(db *store.Store, options Options, logger *slog.Logger) *Worker {
	if options.PollInterval <= 0 {
		options.PollInterval = 30 * time.Second
	}
	if options.MaxAttempts < 1 {
		options.MaxAttempts = 12
	}
	if options.BatchSize < 1 {
		options.BatchSize = 20
	}
	if logger == nil {
		logger = slog.Default()
	}
	w := &Worker{store: db, options: options, logger: logger, wake: make(chan struct{}, 1)}
	w.deliverer = &smtpDeliverer{store: db, options: options}
	return w
}

func (w *Worker) WithDeliverer(deliverer Deliverer) *Worker { w.deliverer = deliverer; return w }
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.options.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.runBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("queue batch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-w.wake:
		}
	}
}

func (w *Worker) runBatch(ctx context.Context) error {
	if recovered, err := w.store.RecoverStaleQueue(ctx, time.Now().UTC().Add(-30*time.Minute)); err != nil {
		return err
	} else if recovered > 0 {
		w.logger.Warn("recovered abandoned mail deliveries", "count", recovered)
	}
	items, err := w.store.QueueReady(ctx, w.options.BatchSize)
	if err != nil {
		return err
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !w.store.ClaimQueue(ctx, item.ID) {
			continue
		}
		item.Attempts++
		message, err := w.store.MessageRaw(ctx, item.MessageID)
		if err == nil {
			err = w.deliverer.Deliver(ctx, message, item.Recipient)
		}
		if err == nil {
			err = w.store.CompleteQueue(ctx, item.ID)
			if err == nil {
				w.logger.Info("mail delivered", "queue_id", item.ID, "message_id", item.MessageID, "recipient_domain", domainOf(item.Recipient))
			}
		} else {
			w.logger.Warn("mail delivery deferred", "queue_id", item.ID, "message_id", item.MessageID, "recipient_domain", domainOf(item.Recipient), "attempt", item.Attempts, "error", safeError(err))
			_ = w.store.RetryQueue(ctx, item.ID, item.Attempts, w.options.MaxAttempts, safeError(err))
		}
		_ = w.store.RefreshMessageTransportStatus(ctx, item.MessageID)
	}
	return nil
}

type smtpDeliverer struct {
	store   *store.Store
	options Options
}

func (d *smtpDeliverer) Deliver(ctx context.Context, message store.Message, recipient string) error {
	signed, err := d.sign(ctx, message)
	if err != nil {
		return err
	}
	if d.options.RelayAddr != "" {
		return d.deliverRelay(ctx, message.EnvelopeFrom, recipient, signed)
	}
	if !d.options.DirectDelivery {
		return errors.New("no smarthost configured and direct MX delivery is disabled")
	}
	return d.deliverMX(ctx, message.EnvelopeFrom, recipient, signed)
}

func (d *smtpDeliverer) sign(ctx context.Context, message store.Message) ([]byte, error) {
	address, err := mail.ParseAddress(message.EnvelopeFrom)
	if err != nil {
		return nil, fmt.Errorf("invalid envelope sender: %w", err)
	}
	domainName := domainOf(address.Address)
	domain, err := d.store.GetDomainByName(ctx, domainName)
	if err != nil {
		return nil, fmt.Errorf("load DKIM domain: %w", err)
	}
	block, _ := pem.Decode([]byte(domain.DKIMPrivateKey))
	if block == nil {
		return nil, errors.New("DKIM private key is missing")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse DKIM key: %w", err)
	}
	var output bytes.Buffer
	err = dkim.Sign(&output, bytes.NewReader(message.Raw), &dkim.SignOptions{Domain: domain.Name, Selector: domain.DKIMSelector,
		Signer: key, HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed,
		HeaderKeys: []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"}})
	if err != nil {
		return nil, fmt.Errorf("sign DKIM: %w", err)
	}
	return output.Bytes(), nil
}

func (d *smtpDeliverer) deliverRelay(ctx context.Context, from, recipient string, raw []byte) error {
	serverName, _, err := net.SplitHostPort(d.options.RelayAddr)
	if err != nil {
		return fmt.Errorf("invalid relay address: %w", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	var client *smtp.Client
	if d.options.RelayImplicitTLS {
		client, err = smtp.DialTLS(d.options.RelayAddr, tlsConfig)
	} else {
		client, err = smtp.DialStartTLS(d.options.RelayAddr, tlsConfig)
	}
	if err != nil {
		return fmt.Errorf("connect relay: %w", err)
	}
	defer client.Close()
	if d.options.Hostname != "" {
		if err = client.Hello(d.options.Hostname); err != nil {
			return err
		}
	}
	if d.options.RelayUsername != "" {
		auth := sasl.NewPlainClient("", d.options.RelayUsername, d.options.RelayPassword)
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("authenticate relay: %w", err)
		}
	}
	if err = client.SendMail(from, []string{recipient}, bytes.NewReader(raw)); err != nil {
		return err
	}
	return client.Quit()
}

func (d *smtpDeliverer) deliverMX(ctx context.Context, from, recipient string, raw []byte) error {
	domain := domainOf(recipient)
	mx, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err != nil {
		if _, hostErr := net.DefaultResolver.LookupHost(ctx, domain); hostErr != nil {
			return fmt.Errorf("lookup MX: %w", err)
		}
		mx = []*net.MX{{Host: domain + ".", Pref: 0}}
	}
	sort.SliceStable(mx, func(i, j int) bool { return mx[i].Pref < mx[j].Pref })
	var failures []string
	for _, record := range mx {
		host := strings.TrimSuffix(record.Host, ".")
		addr := net.JoinHostPort(host, "25")
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		client, connectErr := smtp.DialStartTLS(addr, tlsConfig)
		if connectErr != nil && strings.Contains(connectErr.Error(), "doesn't support STARTTLS") {
			client, connectErr = smtp.Dial(addr)
		}
		if connectErr != nil {
			failures = append(failures, host+": "+safeError(connectErr))
			continue
		}
		if d.options.Hostname != "" {
			_ = client.Hello(d.options.Hostname)
		}
		sendErr := client.SendMail(from, []string{recipient}, bytes.NewReader(raw))
		if sendErr == nil {
			sendErr = client.Quit()
		} else {
			_ = client.Close()
		}
		if sendErr == nil {
			return nil
		}
		failures = append(failures, host+": "+safeError(sendErr))
	}
	return fmt.Errorf("all MX deliveries failed: %s", strings.Join(failures, "; "))
}

func domainOf(address string) string {
	if at := strings.LastIndex(address, "@"); at >= 0 {
		return strings.ToLower(address[at+1:])
	}
	return ""
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

var _ io.Reader
var _ *rsa.PrivateKey
