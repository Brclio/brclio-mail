package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		"BRCLIO_BACKUP_TIMEOUT",
		"BRCLIO_MAX_ARCHIVE_BYTES", "BRCLIO_QUEUE_MAX_ATTEMPTS",
		"BRCLIO_MIN_FREE_DISK_BYTES",
	} {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
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
	if cfg.AutoTLS || cfg.MaxMessageBytes != 25*1024*1024 || cfg.SessionTTL != 7*24*time.Hour || cfg.BackupTimeout != 2*time.Hour {
		t.Fatalf("unset typed environment variables did not use defaults: %#v", cfg)
	}
}

func TestLoadRejectsExplicitInvalidTypedEnvironment(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		kind  string
	}{
		{name: "boolean", key: "BRCLIO_AUTO_TLS", value: "tru", kind: "boolean"},
		{name: "integer", key: "BRCLIO_MAX_MESSAGE_BYTES", value: "25MiB", kind: "base-10 integer"},
		{name: "duration", key: "BRCLIO_SESSION_TTL", value: "one week", kind: "duration"},
		{name: "explicit empty", key: "BRCLIO_QUEUE_POLL_INTERVAL", value: " ", kind: "duration"},
		{name: "backup duration", key: "BRCLIO_BACKUP_TIMEOUT", value: "forever", kind: "duration"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv("BRCLIO_SETUP_TOKEN", "one-time-setup-token")
			t.Setenv(test.key, test.value)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected %s=%q to fail", test.key, test.value)
			}
			if !strings.Contains(err.Error(), test.key) || !strings.Contains(err.Error(), test.kind) || !strings.Contains(err.Error(), test.value) {
				t.Fatalf("unclear typed environment error: %v", err)
			}
		})
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

func TestLoadCountsBootstrapPasswordCharacters(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("BRCLIO_BOOTSTRAP_EMAIL", "admin@example.com")
	t.Setenv("BRCLIO_BOOTSTRAP_PASSWORD", "密码密码密码密码")
	if _, err := Load(); err == nil {
		t.Fatal("four multibyte characters bypassed the bootstrap password minimum")
	}
	t.Setenv("BRCLIO_BOOTSTRAP_PASSWORD", "密码密码密码密码密码密码")
	if _, err := Load(); err != nil {
		t.Fatalf("twelve-character bootstrap password was rejected: %v", err)
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
