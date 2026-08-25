// Package authlimit provides a small, bounded-memory authentication failure
// limiter shared by the SMTP and IMAP protocol frontends.
package authlimit

import (
	"net"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxFailures = 5
	defaultWindow      = 10 * time.Minute
	defaultBlock       = 15 * time.Minute
	defaultMaxEntries  = 16 * 1024
	defaultIPAttempts  = 60
	defaultIPWindow    = time.Minute
	defaultIPBlock     = time.Minute
)

type Options struct {
	MaxFailures     int
	Window          time.Duration
	Block           time.Duration
	MaxEntries      int
	SweepEvery      time.Duration
	Now             func() time.Time
	MaxIPAttempts   int
	IPAttemptWindow time.Duration
	IPAttemptBlock  time.Duration
}

type bucket struct {
	failures            int
	failureWindowStart  time.Time
	failureBlockedUntil time.Time
	attempts            int
	attemptWindowStart  time.Time
	attemptBlockedUntil time.Time
	touchedAt           time.Time
}

// Limiter maintains independent buckets for normalized remote IPs and
// normalized account names. It fails closed for previously unseen keys when
// the entry cap is exhausted, preventing attacker-controlled key churn from
// making memory grow without bound.
type Limiter struct {
	mu              sync.Mutex
	buckets         map[string]*bucket
	maxFailures     int
	window          time.Duration
	block           time.Duration
	retention       time.Duration
	maxEntries      int
	sweepEvery      time.Duration
	nextSweep       time.Time
	now             func() time.Time
	maxIPAttempts   int
	ipAttemptWindow time.Duration
	ipAttemptBlock  time.Duration
}

func New(options Options) *Limiter {
	if options.MaxFailures < 1 {
		options.MaxFailures = defaultMaxFailures
	}
	if options.Window <= 0 {
		options.Window = defaultWindow
	}
	if options.Block <= 0 {
		options.Block = defaultBlock
	}
	if options.MaxEntries < 2 {
		options.MaxEntries = defaultMaxEntries
	}
	if options.SweepEvery <= 0 {
		options.SweepEvery = time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MaxIPAttempts < 1 {
		options.MaxIPAttempts = defaultIPAttempts
	}
	if options.IPAttemptWindow <= 0 {
		options.IPAttemptWindow = defaultIPWindow
	}
	if options.IPAttemptBlock <= 0 {
		options.IPAttemptBlock = defaultIPBlock
	}
	retention := options.Window
	if options.Block > retention {
		retention = options.Block
	}
	if options.IPAttemptWindow > retention {
		retention = options.IPAttemptWindow
	}
	if options.IPAttemptBlock > retention {
		retention = options.IPAttemptBlock
	}
	return &Limiter{
		buckets:         make(map[string]*bucket),
		maxFailures:     options.MaxFailures,
		window:          options.Window,
		block:           options.Block,
		retention:       retention,
		maxEntries:      options.MaxEntries,
		sweepEvery:      options.SweepEvery,
		now:             options.Now,
		maxIPAttempts:   options.MaxIPAttempts,
		ipAttemptWindow: options.IPAttemptWindow,
		ipAttemptBlock:  options.IPAttemptBlock,
	}
}

func NewDefault() *Limiter { return New(Options{}) }

// Allow reserves both keys before credential hashing and reports whether both
// buckets currently permit an attempt. Every allowed attempt, including a
// successful login, is charged to the IP bucket. Call Failure after a rejected
// credential to additionally charge the account bucket, and Success after a
// valid credential.
func (l *Limiter) Allow(remoteIP, account string) bool {
	now := l.now().UTC()
	keys := []string{ipKey(remoteIP), accountKey(account)}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepIfDue(now, false)

	missing := 0
	for _, key := range keys {
		if _, exists := l.buckets[key]; !exists {
			missing++
		}
	}
	if len(l.buckets)+missing > l.maxEntries {
		l.sweepIfDue(now, true)
		missing = 0
		for _, key := range keys {
			if _, exists := l.buckets[key]; !exists {
				missing++
			}
		}
		if len(l.buckets)+missing > l.maxEntries {
			return false
		}
	}
	for _, key := range keys {
		entry := l.buckets[key]
		if entry == nil {
			entry = &bucket{failureWindowStart: now, attemptWindowStart: now, touchedAt: now}
			l.buckets[key] = entry
		}
		if now.Before(entry.failureBlockedUntil) || now.Before(entry.attemptBlockedUntil) {
			return false
		}
		if now.Sub(entry.failureWindowStart) >= l.window {
			entry.failures = 0
			entry.failureWindowStart = now
		}
		if now.Sub(entry.attemptWindowStart) >= l.ipAttemptWindow {
			entry.attempts = 0
			entry.attemptWindowStart = now
		}
	}
	for _, key := range keys {
		l.buckets[key].touchedAt = now
	}
	ipEntry := l.buckets[keys[0]]
	ipEntry.attempts++
	if ipEntry.attempts >= l.maxIPAttempts {
		// The current attempt is allowed; the block applies before the next
		// expensive password hash.
		ipEntry.attemptBlockedUntil = now.Add(l.ipAttemptBlock)
	}
	return true
}

// Failure records a failed credential against both failure buckets. The remote
// IP's total-attempt budget was already charged by Allow.
func (l *Limiter) Failure(remoteIP, account string) {
	now := l.now().UTC()
	keys := []string{ipKey(remoteIP), accountKey(account)}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		entry := l.buckets[key]
		if entry == nil {
			// A caller should invoke Allow first. Still retain the failure when
			// capacity permits, without violating the global memory bound.
			if len(l.buckets) >= l.maxEntries {
				continue
			}
			entry = &bucket{failureWindowStart: now, attemptWindowStart: now}
			l.buckets[key] = entry
		}
		if now.Sub(entry.failureWindowStart) >= l.window {
			entry.failures = 0
			entry.failureWindowStart = now
		}
		entry.failures++
		entry.touchedAt = now
		if entry.failures >= l.maxFailures {
			entry.failureBlockedUntil = now.Add(l.block)
		}
	}
}

// Success clears only the normalized account bucket. The IP bucket is
// intentionally retained so one successful credential cannot reset failures
// accumulated while attacking other accounts from the same source.
func (l *Limiter) Success(account string) {
	l.mu.Lock()
	delete(l.buckets, accountKey(account))
	l.mu.Unlock()
}

func (l *Limiter) sweepIfDue(now time.Time, force bool) {
	if !force && !l.nextSweep.IsZero() && now.Before(l.nextSweep) {
		return
	}
	for key, entry := range l.buckets {
		if !now.Before(entry.failureBlockedUntil) && !now.Before(entry.attemptBlockedUntil) && now.Sub(entry.touchedAt) >= l.retention {
			delete(l.buckets, key)
		}
	}
	l.nextSweep = now.Add(l.sweepEvery)
}

func NormalizeAccount(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func NormalizeIP(value string) string {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")
	if parsed := net.ParseIP(value); parsed != nil {
		return parsed.String()
	}
	if value == "" {
		return "unknown"
	}
	return strings.ToLower(value)
}

func RemoteIP(address net.Addr) string {
	if address == nil {
		return "unknown"
	}
	return NormalizeIP(address.String())
}

func ipKey(value string) string      { return "ip:" + NormalizeIP(value) }
func accountKey(value string) string { return "account:" + NormalizeAccount(value) }
