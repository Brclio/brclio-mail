package smtpserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/connlimit"
	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-smtp"
)

const (
	EndpointSMTP       = "smtp"
	EndpointSubmission = "submission"
	EndpointSMTPS      = "smtps"
)

type endpoint struct {
	name     string
	server   *smtp.Server
	listener net.Listener
}

// Server owns the SMTP, STARTTLS submission, and implicit-TLS SMTP listeners.
// Empty addresses disable individual listeners.
type Server struct {
	store     *store.Store
	config    config.Config
	tlsConfig *tls.Config

	mu        sync.Mutex
	started   bool
	closed    bool
	endpoints []*endpoint
	errors    chan error
}

func New(mailStore *store.Store, cfg config.Config) (*Server, error) {
	tlsConfig, err := loadTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewWithTLS(mailStore, cfg, tlsConfig)
}

// NewWithTLS accepts a dynamic TLS configuration, including an autocert
// GetCertificate callback. Static certificate users can keep calling New.
func NewWithTLS(mailStore *store.Store, cfg config.Config, tlsConfig *tls.Config) (*Server, error) {
	if mailStore == nil {
		return nil, errors.New("smtpserver: store is required")
	}
	tlsConfig = secureTLSConfig(tlsConfig)
	if cfg.SubmissionAddr != "" && tlsConfig == nil && !cfg.DevMode {
		return nil, errors.New("smtpserver: submission requires TLS certificates outside development mode")
	}
	if cfg.SMTPSAddr != "" && tlsConfig == nil {
		return nil, errors.New("smtpserver: SMTPS requires TLS certificates")
	}
	return &Server{store: mailStore, config: cfg, tlsConfig: tlsConfig, errors: make(chan error, 3)}, nil
}

// Start binds all configured listeners before it starts serving. A bind error
// therefore cannot leave a partially started mail service behind.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("smtpserver: already started")
	}
	if s.closed {
		return errors.New("smtpserver: already closed")
	}
	if s.config.DisableMailServers {
		s.started = true
		return nil
	}

	specs := []struct {
		name     string
		address  string
		mode     mode
		implicit bool
	}{
		{EndpointSMTP, s.config.SMTPAddr, modeInbound, false},
		{EndpointSubmission, s.config.SubmissionAddr, modeSubmission, false},
		{EndpointSMTPS, s.config.SMTPSAddr, modeSubmission, true},
	}
	sharedAuthLimiter := authlimit.NewDefault()
	sharedConnectionLimiter := connlimit.New(512, 64)
	for _, spec := range specs {
		if spec.address == "" {
			continue
		}
		listener, err := net.Listen("tcp", spec.address)
		if err != nil {
			s.closeBoundListeners()
			return fmt.Errorf("smtpserver: listen %s on %s: %w", spec.name, spec.address, err)
		}
		listener = connlimit.Wrap(listener, sharedConnectionLimiter)
		if spec.implicit {
			listener = tls.NewListener(listener, s.tlsConfig.Clone())
		}
		mailBackend := &backend{store: s.store, mode: spec.mode, maxMessageBytes: s.config.MaxMessageBytes,
			authLimiter: sharedAuthLimiter, endpoint: spec.name}
		mailServer := smtp.NewServer(mailBackend)
		mailServer.Addr = spec.address
		mailServer.Domain = s.config.Hostname
		mailServer.MaxMessageBytes = s.config.MaxMessageBytes
		mailServer.MaxRecipients = 100
		mailServer.ReadTimeout = 5 * time.Minute
		mailServer.WriteTimeout = 5 * time.Minute
		mailServer.AllowInsecureAuth = spec.mode == modeSubmission && s.config.DevMode && !spec.implicit
		if !spec.implicit && s.tlsConfig != nil {
			mailServer.TLSConfig = s.tlsConfig.Clone()
		}
		s.endpoints = append(s.endpoints, &endpoint{name: spec.name, server: mailServer, listener: listener})
	}
	for _, ep := range s.endpoints {
		go func(ep *endpoint) {
			if err := ep.server.Serve(ep.listener); err != nil && !errors.Is(err, net.ErrClosed) {
				select {
				case s.errors <- fmt.Errorf("smtpserver: %s listener: %w", ep.name, err):
				default:
				}
			}
		}(ep)
	}
	s.started = true
	return nil
}

// Errors reports asynchronous accept-loop failures. It is not closed by Close.
func (s *Server) Errors() <-chan error { return s.errors }

// Addr returns the bound address for an endpoint, which is useful when port 0
// is used by an integration test.
func (s *Server) Addr(name string) net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ep := range s.endpoints {
		if ep.name == name {
			return ep.listener.Addr()
		}
	}
	return nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	endpoints := append([]*endpoint(nil), s.endpoints...)
	s.mu.Unlock()

	var first error
	for _, ep := range endpoints {
		if err := ep.server.Close(); err != nil && !errors.Is(err, smtp.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
		if err := ep.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
	}
	return first
}

func (s *Server) closeBoundListeners() {
	for _, ep := range s.endpoints {
		_ = ep.listener.Close()
	}
	s.endpoints = nil
}

func loadTLSConfig(cfg config.Config) (*tls.Config, error) {
	if !cfg.TLSConfigured() {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		return nil, fmt.Errorf("smtpserver: load TLS key pair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func secureTLSConfig(source *tls.Config) *tls.Config {
	if source == nil {
		return nil
	}
	result := source.Clone()
	if result.MinVersion < tls.VersionTLS12 {
		result.MinVersion = tls.VersionTLS12
	}
	return result
}
