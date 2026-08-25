package imapserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/store"
	imapserverlib "github.com/emersion/go-imap/server"
)

const (
	EndpointIMAP  = "imap"
	EndpointIMAPS = "imaps"
)

type endpoint struct {
	name     string
	listener net.Listener
}

// Server owns STARTTLS IMAP and implicit-TLS IMAPS listeners over the same
// SQLite-backed IMAP backend.
type Server struct {
	config    config.Config
	tlsConfig *tls.Config
	server    *imapserverlib.Server

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
		return nil, errors.New("imapserver: store is required")
	}
	tlsConfig = secureTLSConfig(tlsConfig)
	if cfg.IMAPAddr != "" && !cfg.DisableMailServers {
		if !cfg.DevMode || !isLoopbackListenAddress(cfg.IMAPAddr) || tlsConfig != nil {
			return nil, errors.New("imapserver: cleartext IMAP is limited to a loopback listener in development mode without STARTTLS; use IMAPS in production")
		}
	}
	if cfg.IMAPSAddr != "" && tlsConfig == nil {
		return nil, errors.New("imapserver: IMAPS requires TLS certificates")
	}
	guard := newPreAuthGuard(defaultGuardOptions(cfg.MaxMessageBytes))
	backend := &storeBackend{store: mailStore, maxMessageBytes: cfg.MaxMessageBytes, authLimiter: authlimit.NewDefault(), guard: guard}
	server := imapserverlib.New(backend)
	server.Enable(&imapGuardExtension{guard: guard})
	server.AllowInsecureAuth = cfg.DevMode
	server.AutoLogout = 30 * time.Minute
	server.MaxLiteralSize = uint32Limit(cfg.MaxMessageBytes)
	if tlsConfig != nil {
		server.TLSConfig = tlsConfig.Clone()
	}
	return &Server{config: cfg, tlsConfig: tlsConfig, server: server, errors: make(chan error, 2)}, nil
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("imapserver: already started")
	}
	if s.closed {
		return errors.New("imapserver: already closed")
	}
	if s.config.DisableMailServers {
		s.started = true
		return nil
	}
	specs := []struct {
		name     string
		address  string
		implicit bool
	}{
		{EndpointIMAP, s.config.IMAPAddr, false},
		{EndpointIMAPS, s.config.IMAPSAddr, true},
	}
	for _, spec := range specs {
		if spec.address == "" {
			continue
		}
		listener, err := net.Listen("tcp", spec.address)
		if err != nil {
			s.closeBoundListeners()
			return fmt.Errorf("imapserver: listen %s on %s: %w", spec.name, spec.address, err)
		}
		if spec.implicit {
			listener = tls.NewListener(listener, s.tlsConfig.Clone())
		}
		s.endpoints = append(s.endpoints, &endpoint{name: spec.name, listener: listener})
	}
	for _, ep := range s.endpoints {
		go func(ep *endpoint) {
			if err := s.server.Serve(ep.listener); err != nil && !errors.Is(err, net.ErrClosed) {
				select {
				case s.errors <- fmt.Errorf("imapserver: %s listener: %w", ep.name, err):
				default:
				}
			}
		}(ep)
	}
	s.started = true
	return nil
}

func (s *Server) Errors() <-chan error { return s.errors }

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
	if err := s.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		first = err
	}
	for _, ep := range endpoints {
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
		return nil, fmt.Errorf("imapserver: load TLS key pair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}, nil
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

func isLoopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
