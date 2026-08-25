package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Config struct {
	DataDir            string
	DatabasePath       string
	HTTPAddr           string
	HTTPSAddr          string
	ACMEHTTPAddr       string
	SMTPAddr           string
	SubmissionAddr     string
	SMTPSAddr          string
	IMAPAddr           string
	IMAPSAddr          string
	Hostname           string
	BaseURL            string
	TLSCertPath        string
	TLSKeyPath         string
	AutoTLS            bool
	ACMEEmail          string
	SetupToken         string
	BootstrapEmail     string
	BootstrapPassword  string
	RelayAddr          string
	RelayUsername      string
	RelayPassword      string
	RelayImplicitTLS   bool
	DirectDelivery     bool
	DevMode            bool
	DisableMailServers bool
	MaxMessageBytes    int64
	MaxArchiveBytes    int64
	MinFreeDiskBytes   int64
	SessionTTL         time.Duration
	QueuePollInterval  time.Duration
	BackupTimeout      time.Duration
	QueueMaxAttempts   int
}

func Load() (Config, error) {
	dataDir := env("BRCLIO_DATA_DIR", "./data")
	setupToken, err := secretEnv("BRCLIO_SETUP_TOKEN")
	if err != nil {
		return Config{}, err
	}
	bootstrapPassword, err := secretEnv("BRCLIO_BOOTSTRAP_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	relayPassword, err := secretEnv("BRCLIO_RELAY_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		DataDir:           dataDir,
		DatabasePath:      env("BRCLIO_DATABASE_PATH", filepath.Join(dataDir, "brclio-mail.db")),
		HTTPAddr:          env("BRCLIO_HTTP_ADDR", ":8080"),
		HTTPSAddr:         env("BRCLIO_HTTPS_ADDR", ":8443"),
		ACMEHTTPAddr:      env("BRCLIO_ACME_HTTP_ADDR", ":8080"),
		SMTPAddr:          env("BRCLIO_SMTP_ADDR", ":2525"),
		SubmissionAddr:    env("BRCLIO_SUBMISSION_ADDR", ":2587"),
		SMTPSAddr:         env("BRCLIO_SMTPS_ADDR", ":2465"),
		IMAPAddr:          env("BRCLIO_IMAP_ADDR", ""),
		IMAPSAddr:         env("BRCLIO_IMAPS_ADDR", ":2993"),
		Hostname:          strings.ToLower(env("BRCLIO_HOSTNAME", "mail.localhost")),
		BaseURL:           strings.TrimRight(env("BRCLIO_BASE_URL", "http://localhost:8080"), "/"),
		TLSCertPath:       os.Getenv("BRCLIO_TLS_CERT"),
		TLSKeyPath:        os.Getenv("BRCLIO_TLS_KEY"),
		ACMEEmail:         strings.TrimSpace(os.Getenv("BRCLIO_ACME_EMAIL")),
		SetupToken:        setupToken,
		BootstrapEmail:    strings.ToLower(strings.TrimSpace(os.Getenv("BRCLIO_BOOTSTRAP_EMAIL"))),
		BootstrapPassword: bootstrapPassword,
		RelayAddr:         strings.TrimSpace(os.Getenv("BRCLIO_RELAY_ADDR")),
		RelayUsername:     strings.TrimSpace(os.Getenv("BRCLIO_RELAY_USERNAME")),
		RelayPassword:     relayPassword,
	}
	if cfg.AutoTLS, err = envBool("BRCLIO_AUTO_TLS", false); err != nil {
		return Config{}, err
	}
	if cfg.RelayImplicitTLS, err = envBool("BRCLIO_RELAY_IMPLICIT_TLS", false); err != nil {
		return Config{}, err
	}
	if cfg.DirectDelivery, err = envBool("BRCLIO_DIRECT_DELIVERY", false); err != nil {
		return Config{}, err
	}
	if cfg.DevMode, err = envBool("BRCLIO_DEV_MODE", false); err != nil {
		return Config{}, err
	}
	if cfg.DisableMailServers, err = envBool("BRCLIO_DISABLE_MAIL_SERVERS", false); err != nil {
		return Config{}, err
	}
	if cfg.MaxMessageBytes, err = envInt64("BRCLIO_MAX_MESSAGE_BYTES", 25*1024*1024); err != nil {
		return Config{}, err
	}
	if cfg.MaxArchiveBytes, err = envInt64("BRCLIO_MAX_ARCHIVE_BYTES", 100*1024*1024*1024); err != nil {
		return Config{}, err
	}
	if cfg.MinFreeDiskBytes, err = envInt64("BRCLIO_MIN_FREE_DISK_BYTES", 1024*1024*1024); err != nil {
		return Config{}, err
	}
	if cfg.SessionTTL, err = envDuration("BRCLIO_SESSION_TTL", 7*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.QueuePollInterval, err = envDuration("BRCLIO_QUEUE_POLL_INTERVAL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.BackupTimeout, err = envDuration("BRCLIO_BACKUP_TIMEOUT", 2*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.BackupTimeout < time.Minute {
		return Config{}, errors.New("BRCLIO_BACKUP_TIMEOUT must be at least 1m")
	}
	queueMaxAttempts, err := envInt64("BRCLIO_QUEUE_MAX_ATTEMPTS", 12)
	if err != nil {
		return Config{}, err
	}
	cfg.QueueMaxAttempts = int(queueMaxAttempts)

	if cfg.MaxMessageBytes < 1024 {
		return Config{}, errors.New("BRCLIO_MAX_MESSAGE_BYTES must be at least 1024")
	}
	if cfg.MaxArchiveBytes < cfg.MaxMessageBytes {
		return Config{}, errors.New("BRCLIO_MAX_ARCHIVE_BYTES must be at least BRCLIO_MAX_MESSAGE_BYTES")
	}
	if cfg.MinFreeDiskBytes < 0 {
		return Config{}, errors.New("BRCLIO_MIN_FREE_DISK_BYTES cannot be negative")
	}
	if (cfg.TLSCertPath == "") != (cfg.TLSKeyPath == "") {
		return Config{}, errors.New("BRCLIO_TLS_CERT and BRCLIO_TLS_KEY must be set together")
	}
	if cfg.AutoTLS && cfg.TLSConfigured() {
		return Config{}, errors.New("BRCLIO_AUTO_TLS cannot be combined with BRCLIO_TLS_CERT/BRCLIO_TLS_KEY")
	}
	if cfg.AutoTLS && (cfg.ACMEEmail == "" || cfg.Hostname == "" || strings.HasSuffix(cfg.Hostname, ".localhost")) {
		return Config{}, errors.New("BRCLIO_AUTO_TLS requires BRCLIO_ACME_EMAIL and a public BRCLIO_HOSTNAME")
	}
	if cfg.BootstrapEmail != "" && cfg.BootstrapPassword == "" {
		return Config{}, errors.New("BRCLIO_BOOTSTRAP_PASSWORD is required with BRCLIO_BOOTSTRAP_EMAIL")
	}
	if cfg.BootstrapPassword != "" {
		if !utf8.ValidString(cfg.BootstrapPassword) {
			return Config{}, errors.New("BRCLIO_BOOTSTRAP_PASSWORD must be valid UTF-8")
		}
		if len(cfg.BootstrapPassword) > 1024 {
			return Config{}, errors.New("BRCLIO_BOOTSTRAP_PASSWORD must not exceed 1024 bytes")
		}
		if utf8.RuneCountInString(cfg.BootstrapPassword) < 12 {
			return Config{}, errors.New("BRCLIO_BOOTSTRAP_PASSWORD must contain at least 12 characters")
		}
	}
	if !cfg.DevMode && cfg.SetupToken == "" && cfg.BootstrapEmail == "" {
		return Config{}, errors.New("production startup requires BRCLIO_SETUP_TOKEN(_FILE) or bootstrap credentials")
	}
	if cfg.QueueMaxAttempts < 1 {
		return Config{}, errors.New("BRCLIO_QUEUE_MAX_ATTEMPTS must be at least 1")
	}
	if cfg.RelayUsername != "" && cfg.RelayAddr == "" {
		return Config{}, errors.New("BRCLIO_RELAY_ADDR is required with BRCLIO_RELAY_USERNAME")
	}
	if cfg.RelayPassword != "" && cfg.RelayUsername == "" {
		return Config{}, errors.New("BRCLIO_RELAY_USERNAME is required with BRCLIO_RELAY_PASSWORD")
	}
	return cfg, nil
}

func (c Config) TLSConfigured() bool { return c.TLSCertPath != "" && c.TLSKeyPath != "" }

func (c Config) MailTLSAvailable() bool { return c.TLSConfigured() || c.AutoTLS }

func (c Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.DatabasePath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	return nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	raw, exists := os.LookupEnv(key)
	if !exists {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", key, raw)
	}
	return parsed, nil
}

func envInt64(key string, fallback int64) (int64, error) {
	raw, exists := os.LookupEnv(key)
	if !exists {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a base-10 integer, got %q", key, raw)
	}
	return parsed, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, exists := os.LookupEnv(key)
	if !exists {
		return fallback, nil
	}
	value := strings.TrimSpace(raw)
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 30s or 168h, got %q", key, raw)
	}
	return parsed, nil
}

func secretEnv(key string) (string, error) {
	value := os.Getenv(key)
	file := strings.TrimSpace(os.Getenv(key + "_FILE"))
	if value != "" && file != "" {
		return "", fmt.Errorf("%s and %s_FILE cannot both be set", key, key)
	}
	if file == "" {
		return value, nil
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", key, err)
	}
	if len(content) > 1024*1024 {
		return "", fmt.Errorf("%s_FILE exceeds 1 MiB", key)
	}
	return strings.TrimRight(string(content), "\r\n"), nil
}
