package connlimit

import (
	"net"
	"testing"
)

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

func TestLimiterEnforcesGlobalAndPerIPAndReleases(t *testing.T) {
	limiter := New(2, 1)
	ipOne, ok := limiter.acquire(testAddr("192.0.2.1:1000"))
	if !ok {
		t.Fatal("first IP was rejected")
	}
	if _, ok := limiter.acquire(testAddr("192.0.2.1:1001")); ok {
		t.Fatal("per-IP connection cap was bypassed")
	}
	ipTwo, ok := limiter.acquire(testAddr("192.0.2.2:1000"))
	if !ok {
		t.Fatal("second IP was rejected before global cap")
	}
	if _, ok := limiter.acquire(testAddr("192.0.2.3:1000")); ok {
		t.Fatal("global connection cap was bypassed")
	}
	limiter.release(ipOne)
	if _, ok := limiter.acquire(testAddr("192.0.2.3:1000")); !ok {
		t.Fatal("released global slot was not reusable")
	}
	limiter.release(ipTwo)
}

func TestTrackedConnectionReleasesOnlyOnce(t *testing.T) {
	limiter := New(1, 1)
	ip, ok := limiter.acquire(testAddr("192.0.2.1:1000"))
	if !ok {
		t.Fatal("could not reserve test slot")
	}
	client, server := net.Pipe()
	defer client.Close()
	connection := &trackedConn{Conn: server, limiter: limiter, ip: ip}
	_ = connection.Close()
	_ = connection.Close()
	if _, ok := limiter.acquire(testAddr("192.0.2.2:1000")); !ok {
		t.Fatal("tracked connection did not release its slot")
	}
}
