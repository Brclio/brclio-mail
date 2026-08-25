package imapserver

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Brclio/brclio-mail/internal/authlimit"
	imapserverlib "github.com/emersion/go-imap/server"
)

const (
	maxIMAPCommandLineBytes       = 64 * 1024
	maxIMAPPreAuthLiteralBytes    = 8 * 1024
	maxIMAPPreAuthCommandBytes    = 64 * 1024
	maxIMAPConnectionsPerIP       = 32
	maxIMAPConnectionsGlobal      = 512
	imapPreAuthReadTimeout        = 30 * time.Second
	defaultGuardMessageLimitBytes = 25 * 1024 * 1024
)

var (
	errIMAPCommandLineTooLong     = errors.New("imap command line exceeds limit")
	errIMAPLiteralTooLarge        = errors.New("imap literal exceeds current authentication-state limit")
	errIMAPPreAuthCommandTooLarge = errors.New("imap pre-auth command exceeds cumulative limit")
	errIMAPConnectionLimit        = errors.New("imap connection limit exceeded")
)

type guardOptions struct {
	maxLineBytes         int
	maxPreAuthLiteral    uint64
	maxPreAuthCommand    uint64
	maxAuthenticatedLit  uint64
	maxConnectionsPerIP  int
	maxConnectionsGlobal int
	preAuthTimeout       time.Duration
}

func defaultGuardOptions(maxMessageBytes int64) guardOptions {
	maximum := uint64(defaultGuardMessageLimitBytes)
	if maxMessageBytes > 0 {
		maximum = uint64(maxMessageBytes)
	}
	if maximum > uint64(^uint32(0)) {
		maximum = uint64(^uint32(0))
	}
	return guardOptions{
		maxLineBytes:         maxIMAPCommandLineBytes,
		maxPreAuthLiteral:    maxIMAPPreAuthLiteralBytes,
		maxPreAuthCommand:    maxIMAPPreAuthCommandBytes,
		maxAuthenticatedLit:  maximum,
		maxConnectionsPerIP:  maxIMAPConnectionsPerIP,
		maxConnectionsGlobal: maxIMAPConnectionsGlobal,
		preAuthTimeout:       imapPreAuthReadTimeout,
	}
}

type preAuthGuard struct {
	options guardOptions

	mu          sync.Mutex
	connections map[string]*guardedIMAPConn
	byIP        map[string]int
}

func newPreAuthGuard(options guardOptions) *preAuthGuard {
	if options.maxLineBytes < 1 {
		options.maxLineBytes = maxIMAPCommandLineBytes
	}
	if options.maxPreAuthLiteral == 0 {
		options.maxPreAuthLiteral = maxIMAPPreAuthLiteralBytes
	}
	if options.maxPreAuthCommand == 0 {
		options.maxPreAuthCommand = maxIMAPPreAuthCommandBytes
	}
	if options.maxAuthenticatedLit == 0 {
		options.maxAuthenticatedLit = defaultGuardMessageLimitBytes
	}
	if options.maxConnectionsPerIP < 1 {
		options.maxConnectionsPerIP = maxIMAPConnectionsPerIP
	}
	if options.maxConnectionsGlobal < 1 {
		options.maxConnectionsGlobal = maxIMAPConnectionsGlobal
	}
	if options.preAuthTimeout <= 0 {
		options.preAuthTimeout = imapPreAuthReadTimeout
	}
	return &preAuthGuard{options: options, connections: make(map[string]*guardedIMAPConn), byIP: make(map[string]int)}
}

func (g *preAuthGuard) admit(conn net.Conn) (*guardedIMAPConn, error) {
	ip := authlimit.RemoteIP(conn.RemoteAddr())
	key := imapConnectionKey(conn.RemoteAddr(), conn.LocalAddr())
	g.mu.Lock()
	if len(g.connections) >= g.options.maxConnectionsGlobal || g.byIP[ip] >= g.options.maxConnectionsPerIP {
		g.mu.Unlock()
		return nil, errIMAPConnectionLimit
	}
	if _, exists := g.connections[key]; exists {
		g.mu.Unlock()
		return nil, errIMAPConnectionLimit
	}
	deadline := time.Now().Add(g.options.preAuthTimeout)
	guarded := &guardedIMAPConn{
		Conn: conn, guard: g, key: key, ip: ip, reader: bufio.NewReaderSize(conn, 4096),
		preAuthDeadline: deadline, maxLineBytes: g.options.maxLineBytes,
		maxPreAuthLiteral: g.options.maxPreAuthLiteral, maxPreAuthCommand: g.options.maxPreAuthCommand,
		maxAuthenticatedLit: g.options.maxAuthenticatedLit,
	}
	g.connections[key] = guarded
	g.byIP[ip]++
	g.mu.Unlock()
	if err := conn.SetReadDeadline(deadline); err != nil {
		_ = guarded.Close()
		return nil, err
	}
	return guarded, nil
}

func (g *preAuthGuard) authenticate(remote, local net.Addr) {
	key := imapConnectionKey(remote, local)
	g.mu.Lock()
	conn := g.connections[key]
	g.mu.Unlock()
	if conn != nil {
		conn.markAuthenticated()
	}
}

func (g *preAuthGuard) release(conn *guardedIMAPConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if current := g.connections[conn.key]; current != conn {
		return
	}
	delete(g.connections, conn.key)
	if g.byIP[conn.ip] <= 1 {
		delete(g.byIP, conn.ip)
	} else {
		g.byIP[conn.ip]--
	}
}

func imapConnectionKey(remote, local net.Addr) string {
	return imapAddressKey(remote) + "\x00" + imapAddressKey(local)
}

func imapAddressKey(address net.Addr) string {
	if address == nil {
		return ""
	}
	return address.Network() + "\x00" + address.String()
}

type guardedIMAPConn struct {
	net.Conn
	guard *preAuthGuard
	key   string
	ip    string

	reader              *bufio.Reader
	readMu              sync.Mutex
	pending             []byte
	literalRemaining    uint64
	preAuthDeadline     time.Time
	maxLineBytes        int
	maxPreAuthLiteral   uint64
	maxPreAuthCommand   uint64
	preAuthCommandBytes uint64
	maxAuthenticatedLit uint64
	authenticated       atomic.Bool
	closed              atomic.Bool
}

func (c *guardedIMAPConn) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(c.pending) > 0 {
		n := copy(buffer, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if c.literalRemaining > 0 {
		maximum := len(buffer)
		if uint64(maximum) > c.literalRemaining {
			maximum = int(c.literalRemaining)
		}
		n, err := c.reader.Read(buffer[:maximum])
		c.literalRemaining -= uint64(n)
		return n, err
	}
	line, literalSize, err := c.readValidatedLine()
	if err != nil {
		return 0, err
	}
	c.literalRemaining = literalSize
	n := copy(buffer, line)
	if n < len(line) {
		c.pending = append(c.pending[:0], line[n:]...)
	}
	return n, nil
}

func (c *guardedIMAPConn) readValidatedLine() ([]byte, uint64, error) {
	line := make([]byte, 0, 4096)
	for {
		character, err := c.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, 0, io.EOF
			}
			return nil, 0, err
		}
		if len(line) >= c.maxLineBytes {
			_ = c.Close()
			return nil, 0, errIMAPCommandLineTooLong
		}
		line = append(line, character)
		if character != '\n' {
			continue
		}
		literalSize, literal, literalErr := imapLiteralAtLineEnd(line)
		if literalErr != nil {
			_ = c.Close()
			return nil, 0, literalErr
		}
		if literal {
			maximum := c.maxAuthenticatedLit
			if !c.authenticated.Load() {
				maximum = c.maxPreAuthLiteral
			}
			if literalSize > maximum {
				_ = c.Close()
				return nil, 0, errIMAPLiteralTooLarge
			}
			if !c.authenticated.Load() && !c.addPreAuthCommandBytes(uint64(len(line))+literalSize) {
				_ = c.Close()
				return nil, 0, errIMAPPreAuthCommandTooLarge
			}
			return line, literalSize, nil
		}
		if !c.authenticated.Load() {
			if !c.addPreAuthCommandBytes(uint64(len(line))) {
				_ = c.Close()
				return nil, 0, errIMAPPreAuthCommandTooLarge
			}
			c.preAuthCommandBytes = 0
		}
		return line, 0, nil
	}
}

func (c *guardedIMAPConn) addPreAuthCommandBytes(additional uint64) bool {
	if additional > c.maxPreAuthCommand-c.preAuthCommandBytes {
		return false
	}
	c.preAuthCommandBytes += additional
	return true
}

func imapLiteralAtLineEnd(line []byte) (uint64, bool, error) {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) < 3 || line[len(line)-1] != '}' {
		return 0, false, nil
	}
	open := bytes.LastIndexByte(line, '{')
	if open <= 0 || (line[open-1] != ' ' && line[open-1] != '(') {
		return 0, false, nil
	}
	digits := line[open+1 : len(line)-1]
	if len(digits) > 0 && digits[len(digits)-1] == '+' {
		digits = digits[:len(digits)-1]
	}
	if len(digits) == 0 {
		return 0, false, nil
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, false, nil
		}
	}
	size, err := strconv.ParseUint(string(digits), 10, 64)
	if err != nil {
		return 0, true, errIMAPLiteralTooLarge
	}
	return size, true, nil
}

func (c *guardedIMAPConn) markAuthenticated() {
	if c.authenticated.CompareAndSwap(false, true) {
		_ = c.Conn.SetReadDeadline(time.Time{})
	}
}

func (c *guardedIMAPConn) cappedReadDeadline(requested time.Time) time.Time {
	if c.authenticated.Load() {
		return requested
	}
	if requested.IsZero() || requested.After(c.preAuthDeadline) {
		return c.preAuthDeadline
	}
	return requested
}

func (c *guardedIMAPConn) SetDeadline(deadline time.Time) error {
	readErr := c.Conn.SetReadDeadline(c.cappedReadDeadline(deadline))
	writeErr := c.Conn.SetWriteDeadline(deadline)
	if readErr != nil {
		return readErr
	}
	return writeErr
}

func (c *guardedIMAPConn) SetReadDeadline(deadline time.Time) error {
	return c.Conn.SetReadDeadline(c.cappedReadDeadline(deadline))
}

func (c *guardedIMAPConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.guard.release(c)
	}
	return c.Conn.Close()
}

type imapGuardExtension struct{ guard *preAuthGuard }

var _ imapserverlib.ConnExtension = (*imapGuardExtension)(nil)

func (e *imapGuardExtension) Capabilities(imapserverlib.Conn) []string    { return nil }
func (e *imapGuardExtension) Command(string) imapserverlib.HandlerFactory { return nil }

func (e *imapGuardExtension) NewConn(connection imapserverlib.Conn) imapserverlib.Conn {
	err := connection.Upgrade(func(raw net.Conn) (net.Conn, error) {
		guarded, err := e.guard.admit(raw)
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		return guarded, nil
	})
	if err != nil {
		_ = connection.Close()
	}
	return connection
}
