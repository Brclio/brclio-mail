package imapserver

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Brclio/brclio-mail/internal/config"
	"github.com/Brclio/brclio-mail/internal/store"
	"github.com/emersion/go-imap/client"
)

func TestPreAuthCommandLineLimitClosesConnectionAndServerRecovers(t *testing.T) {
	server, _, _, address := startGuardTestServer(t, false)
	connection, reader := dialGuardTestConnection(t, address)
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	type writeResult struct {
		n   int
		err error
	}
	writeDone := make(chan writeResult, 1)
	go func() {
		n, err := io.WriteString(connection, "a "+strings.Repeat("x", maxIMAPCommandLineBytes))
		writeDone <- writeResult{n: n, err: err}
	}()
	started := time.Now()
	_, readErr := reader.ReadByte()
	if readErr == nil {
		t.Fatal("oversized unterminated command line did not close the connection")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		var result writeResult
		select {
		case result = <-writeDone:
		default:
		}
		t.Fatalf("oversized command line was not rejected promptly: elapsed=%s read=%v write=%+v", elapsed, readErr, result)
	}
	_ = connection.Close()
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("oversized command writer did not unblock after connection close")
	}

	recovered, _ := dialGuardTestConnection(t, address)
	_ = recovered.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestImplicitTLSRejectsLargePreAuthLiteralButAllowsAuthenticatedAppend(t *testing.T) {
	server, database, account, address := startGuardTestServer(t, true)
	clientTLS := &tls.Config{InsecureSkipVerify: true} // test-only self-signed certificate
	connection, err := tls.Dial("tcp", address, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if greeting, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(greeting, "* OK ") {
		t.Fatalf("unexpected IMAPS greeting %q: %v", greeting, err)
	}
	started := time.Now()
	_, _ = io.WriteString(connection, "a LOGIN {26214400}\r\n")
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("large pre-auth LOGIN literal received a continuation instead of connection close")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("large pre-auth literal was not rejected promptly: %s", elapsed)
	}
	_ = connection.Close()

	mailClient, err := client.DialTLS(address, clientTLS)
	if err != nil {
		t.Fatalf("server did not recover after rejected TLS connection: %v", err)
	}
	t.Cleanup(func() { _ = mailClient.Terminate() })
	if err := mailClient.Login(account.Email, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte("From: sender@outside.test\r\nTo: alice@example.com\r\nSubject: guarded append\r\n\r\n"),
		bytes.Repeat([]byte("a"), maxIMAPPreAuthLiteralBytes+1024)...)
	if err := mailClient.Append(store.MailboxInbox, nil, time.Time{}, bytes.NewBuffer(raw)); err != nil {
		t.Fatalf("authenticated APPEND above the pre-auth literal cap was rejected: %v", err)
	}
	inbox, err := database.IMAPGetMailbox(t.Context(), account.ID, store.MailboxInbox)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := database.IMAPListEntries(t.Context(), account.ID, inbox.ID)
	if err != nil || len(entries) != 1 || entries[0].SizeBytes <= maxIMAPPreAuthLiteralBytes {
		t.Fatalf("authenticated APPEND missing: %#v err=%v", entries, err)
	}
	if err := mailClient.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPreAuthDeadlineCannotBeExtendedAndClearsAfterAuthentication(t *testing.T) {
	guard := newPreAuthGuard(guardOptions{
		maxLineBytes: 1024, maxPreAuthLiteral: 128, maxAuthenticatedLit: 4096,
		maxConnectionsPerIP: 2, preAuthTimeout: 80 * time.Millisecond,
	})
	serverSide, clientSide := net.Pipe()
	guarded, err := guard.admit(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSide.Close()
	if err := guarded.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := guarded.Read(make([]byte, 32)); err == nil {
		t.Fatal("pre-auth read did not hit its absolute deadline")
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("unexpected pre-auth deadline duration: %s", elapsed)
	}
	_ = guarded.Close()

	serverSide, clientSide = net.Pipe()
	guarded, err = guard.admit(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	defer guarded.Close()
	defer clientSide.Close()
	guard.authenticate(guarded.RemoteAddr(), guarded.LocalAddr())
	time.Sleep(100 * time.Millisecond)
	go func() { _, _ = io.WriteString(clientSide, "a NOOP\r\n") }()
	buffer := make([]byte, 32)
	n, err := guarded.Read(buffer)
	if err != nil || string(buffer[:n]) != "a NOOP\r\n" {
		t.Fatalf("authenticated connection retained pre-auth deadline: data=%q err=%v", buffer[:n], err)
	}
}

func TestPreAuthCommandCumulativeLiteralBudget(t *testing.T) {
	guard := newPreAuthGuard(guardOptions{
		maxLineBytes: 1024, maxPreAuthLiteral: 8, maxPreAuthCommand: 32,
		maxAuthenticatedLit: 4096, maxConnectionsPerIP: 2, preAuthTimeout: time.Second,
	})
	serverSide, clientSide := net.Pipe()
	guarded, err := guard.admit(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	defer guarded.Close()
	defer clientSide.Close()
	go func() { _, _ = io.WriteString(clientSide, "a LOGIN {8}\r\n12345678 {8}\r\n") }()

	firstLine := make([]byte, len("a LOGIN {8}\r\n"))
	if _, err := io.ReadFull(guarded, firstLine); err != nil {
		t.Fatalf("first literal marker was unexpectedly rejected: %v", err)
	}
	firstLiteral := make([]byte, 8)
	if _, err := io.ReadFull(guarded, firstLiteral); err != nil {
		t.Fatalf("first literal was unexpectedly rejected: %v", err)
	}
	if _, err := guarded.Read(make([]byte, 64)); !errors.Is(err, errIMAPPreAuthCommandTooLarge) {
		t.Fatalf("cumulative pre-auth literal budget was not enforced: %v", err)
	}
}

func TestConcurrentConnectionsAreCappedPerRemoteIP(t *testing.T) {
	server, _, _, address := startGuardTestServer(t, false)
	connections := make([]net.Conn, 0, maxIMAPConnectionsPerIP)
	for index := 0; index < maxIMAPConnectionsPerIP; index++ {
		connection, _ := dialGuardTestConnection(t, address)
		connections = append(connections, connection)
	}
	t.Cleanup(func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	})

	rejected, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = rejected.SetReadDeadline(time.Now().Add(time.Second))
	if greeting, err := bufio.NewReader(rejected).ReadString('\n'); err == nil || greeting != "" {
		t.Fatalf("33rd connection was not rejected: greeting=%q err=%v", greeting, err)
	}
	_ = rejected.Close()

	_ = connections[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	var replacement net.Conn
	for time.Now().Before(deadline) {
		candidate, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			continue
		}
		_ = candidate.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		greeting, readErr := bufio.NewReader(candidate).ReadString('\n')
		if readErr == nil && strings.HasPrefix(greeting, "* OK ") {
			replacement = candidate
			break
		}
		_ = candidate.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if replacement == nil {
		t.Fatal("connection slot was not released after close")
	}
	_ = replacement.Close()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalConnectionLimitAndCloseRelease(t *testing.T) {
	guard := newPreAuthGuard(guardOptions{
		maxLineBytes: 1024, maxPreAuthLiteral: 128, maxAuthenticatedLit: 4096,
		maxConnectionsPerIP: 2, maxConnectionsGlobal: 2, preAuthTimeout: time.Second,
	})
	serverOne, clientOne := net.Pipe()
	serverTwo, clientTwo := net.Pipe()
	serverThree, clientThree := net.Pipe()
	defer clientOne.Close()
	defer clientTwo.Close()
	defer clientThree.Close()
	first, err := guard.admit(&addressedConn{Conn: serverOne,
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1001}, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.100"), Port: 993}})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := guard.admit(&addressedConn{Conn: serverTwo,
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 1002}, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.100"), Port: 993}})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	thirdRaw := &addressedConn{Conn: serverThree,
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.3"), Port: 1003}, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.100"), Port: 993}}
	if _, err := guard.admit(thirdRaw); !errors.Is(err, errIMAPConnectionLimit) {
		t.Fatalf("global connection cap was not enforced: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := guard.admit(thirdRaw)
	if err != nil {
		t.Fatalf("global connection slot was not released: %v", err)
	}
	_ = third.Close()
}

func startGuardTestServer(t *testing.T, implicitTLS bool) (*Server, *store.Store, store.User, string) {
	t.Helper()
	database, account, _ := imapTestStore(t)
	cfg := config.Config{MaxMessageBytes: 64 * 1024}
	var (
		server *Server
		err    error
	)
	if implicitTLS {
		cfg.IMAPSAddr = "127.0.0.1:0"
		server, err = NewWithTLS(database, cfg, &tls.Config{Certificates: []tls.Certificate{guardTestCertificate(t)}})
	} else {
		cfg.IMAPAddr = "127.0.0.1:0"
		cfg.DevMode = true
		server, err = New(database, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	endpoint := EndpointIMAP
	if implicitTLS {
		endpoint = EndpointIMAPS
	}
	return server, database, account, server.Addr(endpoint).String()
}

func dialGuardTestConnection(t *testing.T, address string) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	greeting, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(greeting, "* OK ") {
		_ = connection.Close()
		t.Fatalf("unexpected IMAP greeting %q: %v", greeting, err)
	}
	_ = connection.SetReadDeadline(time.Time{})
	return connection, reader
}

func guardTestCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
}

type addressedConn struct {
	net.Conn
	remote net.Addr
	local  net.Addr
}

func (c *addressedConn) RemoteAddr() net.Addr { return c.remote }
func (c *addressedConn) LocalAddr() net.Addr  { return c.local }
