package tlsconfig

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Brclio/brclio-mail/internal/config"
	"golang.org/x/crypto/acme/autocert"
)

type Provider struct {
	TLSConfig           *tls.Config
	HTTPChallenge       http.Handler
	Automatic           bool
	CertificateCacheDir string
}

// New builds one TLS configuration shared by HTTPS, Submission, SMTPS and
// IMAPS. It supports either mounted certificate files or automatic ACME.
func New(cfg config.Config) (*Provider, error) {
	if cfg.TLSConfigured() {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertPath, cfg.TLSKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load TLS key pair: %w", err)
		}
		return &Provider{TLSConfig: hardened(&tls.Config{Certificates: []tls.Certificate{certificate}})}, nil
	}
	if !cfg.AutoTLS {
		return &Provider{}, nil
	}
	cacheDir := filepath.Join(cfg.DataDir, "acme")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ACME certificate cache: %w", err)
	}
	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cacheDir),
		HostPolicy: autocert.HostWhitelist(cfg.Hostname),
		Email:      cfg.ACMEEmail,
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimRight(cfg.BaseURL, "/") + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
	return &Provider{
		TLSConfig:           hardened(manager.TLSConfig()),
		HTTPChallenge:       manager.HTTPHandler(fallback),
		Automatic:           true,
		CertificateCacheDir: cacheDir,
	}, nil
}

func hardened(value *tls.Config) *tls.Config {
	clone := value.Clone()
	clone.MinVersion = tls.VersionTLS12
	return clone
}
