package imapserver

import (
	"bufio"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/emersion/go-imap/client"
)

func TestServerStartAndClose(t *testing.T) {
	database, account, _ := imapTestStore(t)
	server, err := New(database, config.Config{
		IMAPAddr:        "127.0.0.1:0",
		MaxMessageBytes: 1024 * 1024,
		DevMode:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	address := server.Addr(EndpointIMAP)
	if address == nil {
		t.Fatal("IMAP endpoint did not bind")
	}
	connection, err := net.DialTimeout("tcp", address.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	greeting, err := bufio.NewReader(connection).ReadString('\n')
	_ = connection.Close()
	if err != nil || !strings.HasPrefix(greeting, "* OK ") {
		t.Fatalf("unexpected IMAP greeting %q: %v", greeting, err)
	}
	mailClient, err := client.Dial(address.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := mailClient.Login(account.Email, "correct horse battery staple"); err != nil {
		_ = mailClient.Terminate()
		t.Fatalf("IMAP LOGIN failed: %v", err)
	}
	status, err := mailClient.Select("INBOX", false)
	if err != nil || status.Name != "INBOX" {
		_ = mailClient.Terminate()
		t.Fatalf("IMAP SELECT failed: %#v %v", status, err)
	}
	if err := mailClient.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewWithTLSAcceptsDynamicAutoTLSConfigAndRaisesMinimumVersion(t *testing.T) {
	database, _, _ := imapTestStore(t)
	server, err := NewWithTLS(database, config.Config{
		IMAPSAddr:       "127.0.0.1:0",
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

func TestCleartextIMAPIsRestrictedToDevelopmentLoopbackWithoutSTARTTLS(t *testing.T) {
	database, _, _ := imapTestStore(t)
	tests := []struct {
		name      string
		config    config.Config
		tlsConfig *tls.Config
	}{
		{name: "production loopback", config: config.Config{IMAPAddr: "127.0.0.1:0", MaxMessageBytes: 1024 * 1024}},
		{name: "development public", config: config.Config{IMAPAddr: "0.0.0.0:0", DevMode: true, MaxMessageBytes: 1024 * 1024}},
		{name: "development STARTTLS", config: config.Config{IMAPAddr: "127.0.0.1:0", DevMode: true, MaxMessageBytes: 1024 * 1024}, tlsConfig: &tls.Config{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWithTLS(database, test.config, test.tlsConfig); err == nil {
				t.Fatal("unsafe cleartext/STARTTLS IMAP listener configuration was accepted")
			}
		})
	}
	if _, err := NewWithTLS(database, config.Config{
		IMAPAddr: "[::1]:0", DevMode: true, MaxMessageBytes: 1024 * 1024,
	}, nil); err != nil {
		t.Fatalf("development loopback/no-TLS listener was rejected: %v", err)
	}
}
