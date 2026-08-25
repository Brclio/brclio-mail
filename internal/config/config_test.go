package config

import (
	"os"
	"path/filepath"
	"testing"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BRCLIO_DATA_DIR", "BRCLIO_DATABASE_PATH", "BRCLIO_HTTP_ADDR", "BRCLIO_HTTPS_ADDR", "BRCLIO_ACME_HTTP_ADDR",
		"BRCLIO_SMTP_ADDR", "BRCLIO_SUBMISSION_ADDR", "BRCLIO_SMTPS_ADDR", "BRCLIO_IMAP_ADDR", "BRCLIO_IMAPS_ADDR",
		"BRCLIO_HOSTNAME", "BRCLIO_BASE_URL", "BRCLIO_TLS_CERT", "BRCLIO_TLS_KEY", "BRCLIO_AUTO_TLS", "BRCLIO_ACME_EMAIL",
		"BRCLIO_SETUP_TOKEN", "BRCLIO_SETUP_TOKEN_FILE", "BRCLIO_BOOTSTRAP_EMAIL", "BRCLIO_BOOTSTRAP_PASSWORD",
		"BRCLIO_BOOTSTRAP_PASSWORD_FILE", "BRCLIO_RELAY_ADDR", "BRCLIO_RELAY_USERNAME", "BRCLIO_RELAY_PASSWORD",
		"BRCLIO_RELAY_PASSWORD_FILE", "BRCLIO_RELAY_IMPLICIT_TLS", "BRCLIO_DIRECT_DELIVERY", "BRCLIO_DEV_MODE",
		"BRCLIO_DISABLE_MAIL_SERVERS", "BRCLIO_MAX_MESSAGE_BYTES", "BRCLIO_SESSION_TTL", "BRCLIO_QUEUE_POLL_INTERVAL",
		"BRCLIO_MAX_ARCHIVE_BYTES", "BRCLIO_QUEUE_MAX_ATTEMPTS",
		"BRCLIO_MIN_FREE_DISK_BYTES",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadReadsSecretsFromFiles(t *testing.T) {
	cleanEnvironment(t)
	secretPath := filepath.Join(t.TempDir(), "setup-token")
	if err := os.WriteFile(secretPath, []byte("one-time-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRCLIO_SETUP_TOKEN_FILE", secretPath)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SetupToken != "one-time-secret" {
		t.Fatalf("setup token=%q", cfg.SetupToken)
	}
	if cfg.IMAPSAddr != ":2993" || cfg.SMTPSAddr != ":2465" || cfg.IMAPAddr != "" {
		t.Fatalf("unexpected secure mail defaults: %#v", cfg)
	}
	if cfg.MinFreeDiskBytes != 1024*1024*1024 {
		t.Fatalf("unexpected disk reserve: %d", cfg.MinFreeDiskBytes)
	}
}

func TestLoadRejectsConflictingSecretSources(t *testing.T) {
	cleanEnvironment(t)
	secretPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secretPath, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRCLIO_SETUP_TOKEN", "from-env")
	t.Setenv("BRCLIO_SETUP_TOKEN_FILE", secretPath)
	if _, err := Load(); err == nil {
		t.Fatal("expected conflicting secret sources to fail")
	}
}

func TestLoadValidatesAutomaticTLS(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("BRCLIO_AUTO_TLS", "true")
	t.Setenv("BRCLIO_SETUP_TOKEN", "one-time-setup-token")
	if _, err := Load(); err == nil {
		t.Fatal("expected auto TLS without a public hostname and contact to fail")
	}
	t.Setenv("BRCLIO_HOSTNAME", "mail.example.com")
	t.Setenv("BRCLIO_ACME_EMAIL", "postmaster@example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MailTLSAvailable() {
		t.Fatal("auto TLS should make mail TLS available")
	}
}
