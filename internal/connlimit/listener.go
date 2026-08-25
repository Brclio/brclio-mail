// Package connlimit bounds concurrent TCP connections across one or more
// listeners. Rejected sockets are closed before a protocol parser or TLS
// handshake allocates per-connection state.
package connlimit

import (
	"net"
	"sync"
)

type Limiter struct {
	mu        sync.Mutex
	globalMax int
	perIPMax  int
	global    int
	byIP      map[string]int
}

func New(globalMax, perIPMax int) *Limiter {
	return &Limiter{globalMax: globalMax, perIPMax: perIPMax, byIP: make(map[string]int)}
}

func (l *Limiter) acquire(address net.Addr) (string, bool) {
	ip := remoteIP(address)
	l.mu.Lock()
	defer l.mu.Unlock()
	if (l.globalMax > 0 && l.global >= l.globalMax) || (l.perIPMax > 0 && l.byIP[ip] >= l.perIPMax) {
		return ip, false
	}
	l.global++
	l.byIP[ip]++
	return ip, true
}

func (l *Limiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global > 0 {
		l.global--
	}
	if l.byIP[ip] <= 1 {
		delete(l.byIP, ip)
	} else {
		l.byIP[ip]--
	}
}

func remoteIP(address net.Addr) string {
	if address == nil {
		return "unknown"
	}
	if host, _, err := net.SplitHostPort(address.String()); err == nil {
		return host
	}
	return address.String()
}

type listener struct {
	net.Listener
	limiter *Limiter
}

// Wrap applies a shared limiter to listener. Multiple endpoints can share the
// same Limiter so the global cap covers all of them together.
func Wrap(source net.Listener, limiter *Limiter) net.Listener {
	if source == nil || limiter == nil {
		return source
	}
	return &listener{Listener: source, limiter: limiter}
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip, allowed := l.limiter.acquire(connection.RemoteAddr())
		if !allowed {
			_ = connection.Close()
			continue
		}
		return &trackedConn{Conn: connection, limiter: l.limiter, ip: ip}, nil
	}
}

type trackedConn struct {
	net.Conn
	limiter *Limiter
	ip      string
	once    sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.limiter.release(c.ip) })
	return err
}
