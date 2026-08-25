package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/httpapi"
	"github.com/Brclio/brclio-mail/internal/imapserver"
	"github.com/Brclio/brclio-mail/internal/queue"
	"github.com/Brclio/brclio-mail/internal/service"
	"github.com/Brclio/brclio-mail/internal/smtpserver"
	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/Brclio/brclio-mail/internal/tlsconfig"
	webassets "github.com/Brclio/brclio-mail/web"
)

var (
	version   = "0.1.0-dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if command == "version" || command == "--version" || command == "-v" {
		fmt.Printf("brclio-mail %s (commit %s, built %s)\n", version, commit, buildDate)
		return nil
	}
	if command == "help" || command == "--help" || command == "-h" {
		printUsage()
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	switch command {
	case "serve":
		if len(args) != 0 {
			return errors.New("serve accepts no positional arguments")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return serve(ctx, cfg, db, logger)
	case "doctor":
		if len(args) != 0 {
			return errors.New("doctor accepts no positional arguments")
		}
		return doctor(context.Background(), cfg, db)
	case "backup":
		if len(args) != 1 {
			return errors.New("usage: brclio-mail backup /path/to/new-backup.sqlite")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := db.Backup(ctx, args[0]); err != nil {
			return err
		}
		absolute, _ := filepath.Abs(args[0])
		fmt.Printf("backup created and verified: %s\n", absolute)
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func serve(parent context.Context, cfg config.Config, db *store.Store, logger *slog.Logger) error {
	publicURL, err := url.Parse(cfg.BaseURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return errors.New("BRCLIO_BASE_URL must be an absolute http or https URL")
	}
	if publicURL.Scheme != "http" && publicURL.Scheme != "https" {
		return errors.New("BRCLIO_BASE_URL must use http or https")
	}
	if !cfg.DevMode && publicURL.Scheme != "https" {
		return errors.New("production mode requires an https BRCLIO_BASE_URL")
	}
	if err := db.DeleteExpiredSessions(parent); err != nil {
		return fmt.Errorf("delete expired web sessions: %w", err)
	}

	svc := service.New(db, cfg)
	if err := svc.Bootstrap(parent); err != nil {
		return fmt.Errorf("bootstrap administrator: %w", err)
	}
	tlsProvider, err := tlsconfig.New(cfg)
	if err != nil {
		return err
	}
	if !cfg.DevMode && tlsProvider.TLSConfig == nil {
		return errors.New("production mode requires BRCLIO_AUTO_TLS or BRCLIO_TLS_CERT/BRCLIO_TLS_KEY")
	}

	mailCfg := cfg
	if cfg.DevMode && tlsProvider.TLSConfig == nil {
		// Implicit-TLS listeners cannot operate without a certificate. Development
		// can still opt into cleartext Submission/IMAP on loopback addresses.
		mailCfg.SMTPSAddr = ""
		mailCfg.IMAPSAddr = ""
	}

	var smtpService *smtpserver.Server
	var imapService *imapserver.Server
	if !cfg.DisableMailServers {
		smtpService, err = smtpserver.NewWithTLS(db, mailCfg, tlsProvider.TLSConfig)
		if err != nil {
			return err
		}
		imapService, err = imapserver.NewWithTLS(db, mailCfg, tlsProvider.TLSConfig)
		if err != nil {
			return err
		}
	}

	api := httpapi.New(cfg, db, svc, logger, webassets.Handler())
	api.Version = version
	appServer := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	appAddress := cfg.HTTPAddr
	useHTTPS := publicURL.Scheme == "https"
	if useHTTPS {
		appAddress = cfg.HTTPSAddr
		appServer.TLSConfig = tlsProvider.TLSConfig.Clone()
	}
	appListener, err := net.Listen("tcp", appAddress)
	if err != nil {
		return fmt.Errorf("listen web on %s: %w", appAddress, err)
	}
	defer appListener.Close()

	var redirectServer *http.Server
	var redirectListener net.Listener
	if useHTTPS {
		redirectAddress := cfg.HTTPAddr
		redirectHandler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			target := strings.TrimRight(cfg.BaseURL, "/") + r.URL.RequestURI()
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
		}))
		if tlsProvider.Automatic {
			redirectAddress = cfg.ACMEHTTPAddr
			redirectHandler = tlsProvider.HTTPChallenge
		}
		redirectListener, err = net.Listen("tcp", redirectAddress)
		if err != nil {
			return fmt.Errorf("listen HTTP redirect/ACME on %s: %w", redirectAddress, err)
		}
		defer redirectListener.Close()
		redirectServer = &http.Server{Handler: redirectHandler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	}

	if smtpService != nil {
		if err := smtpService.Start(); err != nil {
			return err
		}
		defer smtpService.Close()
	}
	if imapService != nil {
		if err := imapService.Start(); err != nil {
			return err
		}
		defer imapService.Close()
	}

	runCtx, cancel := context.WithCancel(parent)
	defer cancel()
	errCh := make(chan error, 6)
	go cleanExpiredSessions(runCtx, db, logger)
	go func() {
		var serveErr error
		if useHTTPS {
			serveErr = appServer.ServeTLS(appListener, "", "")
		} else {
			serveErr = appServer.Serve(appListener)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("web server: %w", serveErr)
		}
	}()
	if redirectServer != nil {
		go func() {
			if serveErr := redirectServer.Serve(redirectListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- fmt.Errorf("HTTP redirect/ACME server: %w", serveErr)
			}
		}()
	}
	if smtpService != nil {
		go forwardError(runCtx, errCh, smtpService.Errors())
	}
	if imapService != nil {
		go forwardError(runCtx, errCh, imapService.Errors())
	}
	worker := queue.New(db, queue.Options{
		Hostname:         cfg.Hostname,
		PollInterval:     cfg.QueuePollInterval,
		MaxAttempts:      cfg.QueueMaxAttempts,
		RelayAddr:        cfg.RelayAddr,
		RelayUsername:    cfg.RelayUsername,
		RelayPassword:    cfg.RelayPassword,
		RelayImplicitTLS: cfg.RelayImplicitTLS,
		DirectDelivery:   cfg.DirectDelivery,
	}, logger)
	go func() {
		if runErr := worker.Run(runCtx); runErr != nil && !errors.Is(runErr, context.Canceled) {
			errCh <- fmt.Errorf("delivery queue: %w", runErr)
		}
	}()

	logger.Info("Brclio Mail started", "version", version, "web", appAddress, "hostname", cfg.Hostname,
		"tls", tlsProvider.TLSConfig != nil, "auto_tls", tlsProvider.Automatic, "mail_servers", !cfg.DisableMailServers)
	var runErr error
	select {
	case <-parent.Done():
	case runErr = <-errCh:
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	if redirectServer != nil {
		_ = redirectServer.Shutdown(shutdownCtx)
	}
	if err := appServer.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutdown web server: %w", err)
	}
	if smtpService != nil {
		_ = smtpService.Close()
	}
	if imapService != nil {
		_ = imapService.Close()
	}
	logger.Info("Brclio Mail stopped")
	return runErr
}

func cleanExpiredSessions(ctx context.Context, db *store.Store, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := db.DeleteExpiredSessions(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("could not delete expired web sessions", "error", err)
			}
		}
	}
}

func forwardError(ctx context.Context, destination chan<- error, source <-chan error) {
	select {
	case err := <-source:
		if err != nil {
			destination <- err
		}
	case <-ctx.Done():
	}
}

func doctor(ctx context.Context, cfg config.Config, db *store.Store) error {
	if err := db.IntegrityCheck(ctx); err != nil {
		return err
	}
	users, err := db.CountUsers(ctx)
	if err != nil {
		return err
	}
	result := map[string]any{
		"status":              "ok",
		"version":             version,
		"sqliteVersion":       db.SQLiteVersion(),
		"databasePath":        cfg.DatabasePath,
		"initialized":         users > 0,
		"users":               users,
		"developmentMode":     cfg.DevMode,
		"mailServersDisabled": cfg.DisableMailServers,
		"tlsMode":             tlsMode(cfg),
		"deliveryMode":        deliveryMode(cfg),
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func tlsMode(cfg config.Config) string {
	if cfg.AutoTLS {
		return "acme"
	}
	if cfg.TLSConfigured() {
		return "files"
	}
	return "none"
}

func deliveryMode(cfg config.Config) string {
	if cfg.RelayAddr != "" {
		return "smarthost"
	}
	if cfg.DirectDelivery {
		return "direct-mx"
	}
	return "disabled"
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: brclio-mail [serve|doctor|backup PATH|version]")
}
