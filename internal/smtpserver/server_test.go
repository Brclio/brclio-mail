package smtpserver

import (
	"bufio"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Brclio/brclio-mail/internal/config"
)

func TestServerStartAndClose(t *testing.T) {
	database, _ := smtpTestStore(t)
	server, err := New(database, config.Config{
		SMTPAddr:        "127.0.0.1:0",
		Hostname:        "mail.example.com",
		MaxMessageBytes: 1024 * 1024,
		DevMode:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	address := server.Addr(EndpointSMTP)
	if address == nil {
		t.Fatal("SMTP endpoint did not bind")
	}
	connection, err := net.DialTimeout("tcp", address.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	greeting, err := bufio.NewReader(connection).ReadString('\n')
	_ = connection.Close()
	if err != nil || !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("unexpected SMTP greeting %q: %v", greeting, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewWithTLSAcceptsDynamicAutoTLSConfigAndRaisesMinimumVersion(t *testing.T) {
	database, _ := smtpTestStore(t)
	server, err := NewWithTLS(database, config.Config{
		SubmissionAddr:  "127.0.0.1:0",
		Hostname:        "mail.example.com",
		AutoTLS:         true,
		MaxMessageBytes: 1024 * 1024,
	}, &tls.Config{MinVersion: tls.VersionTLS10})
	if err != nil {
		t.Fatal(err)
	}
	if server.tlsConfig == nil || server.tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("dynamic TLS minimum was not hardened: %#v", server.tlsConfig)
	}
}

func TestSMTPAndSubmissionEndpointsShareLimiter(t *testing.T) {
	database, _ := smtpTestStore(t)
	server, err := New(database, config.Config{
		SMTPAddr:        "127.0.0.1:0",
		SubmissionAddr:  "127.0.0.1:0",
		Hostname:        "mail.example.com",
		MaxMessageBytes: 1024 * 1024,
		DevMode:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if len(server.endpoints) != 2 {
		t.Fatalf("unexpected endpoints: %d", len(server.endpoints))
	}
	first := server.endpoints[0].server.Backend.(*backend).authLimiter
	second := server.endpoints[1].server.Backend.(*backend).authLimiter
	if first == nil || first != second {
		t.Fatal("SMTP endpoints do not share one authentication limiter")
	}
}
