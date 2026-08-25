package authlimit

import (
	"testing"
	"time"
)

func TestDualBucketsAndSuccessOnlyClearsAccount(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	limiter := New(Options{MaxFailures: 2, Window: time.Minute, Block: 5 * time.Minute, MaxEntries: 20, Now: func() time.Time { return now }})
	if !limiter.Allow("192.0.2.10:2525", " Alice@Example.COM ") {
		t.Fatal("first attempt unexpectedly blocked")
	}
	limiter.Failure("192.0.2.10", "alice@example.com")
	if !limiter.Allow("192.0.2.10", "ALICE@example.com") {
		t.Fatal("attempt below threshold unexpectedly blocked")
	}
	limiter.Failure("192.0.2.10", "alice@example.com")
	if limiter.Allow("192.0.2.10", "alice@example.com") {
		t.Fatal("threshold did not block matching IP/account")
	}
	limiter.Success("ALICE@example.com")
	if limiter.Allow("192.0.2.10", "different@example.com") {
		t.Fatal("account success incorrectly cleared the IP bucket")
	}
	if limiter.Allow("192.0.2.11", "alice@example.com") == false {
		t.Fatal("normalized account bucket was not cleared by success")
	}
}

func TestAccountBucketBlocksAcrossIPsAndExpires(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	limiter := New(Options{MaxFailures: 1, Window: time.Minute, Block: 2 * time.Minute, MaxEntries: 20, SweepEvery: time.Second, Now: func() time.Time { return now }})
	if !limiter.Allow("192.0.2.20", "victim@example.com") {
		t.Fatal("first attempt unexpectedly blocked")
	}
	limiter.Failure("192.0.2.20", "victim@example.com")
	if limiter.Allow("192.0.2.21", "VICTIM@example.com") {
		t.Fatal("account bucket did not block a second IP")
	}
	now = now.Add(3 * time.Minute)
	if !limiter.Allow("192.0.2.21", "victim@example.com") {
		t.Fatal("expired block was not cleaned")
	}
}

func TestCapacityIsBoundedAndFailsClosedUntilExpiry(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	limiter := New(Options{MaxFailures: 3, Window: time.Minute, Block: time.Minute, MaxEntries: 4, SweepEvery: time.Second, Now: func() time.Time { return now }})
	if !limiter.Allow("192.0.2.1", "one@example.com") || !limiter.Allow("192.0.2.2", "two@example.com") {
		t.Fatal("capacity was exhausted too early")
	}
	if limiter.Allow("192.0.2.3", "three@example.com") {
		t.Fatal("limiter did not fail closed at its entry cap")
	}
	if len(limiter.buckets) > 4 {
		t.Fatalf("entry cap exceeded: %d", len(limiter.buckets))
	}
	now = now.Add(2 * time.Minute)
	if !limiter.Allow("192.0.2.3", "three@example.com") {
		t.Fatal("expired entries were not reclaimed")
	}
	if len(limiter.buckets) > 4 {
		t.Fatalf("entry cap exceeded after sweep: %d", len(limiter.buckets))
	}
}

func TestSuccessfulAttemptStillChargesIP(t *testing.T) {
	limiter := New(Options{MaxFailures: 5, Window: time.Minute, Block: time.Minute, MaxEntries: 20,
		MaxIPAttempts: 1, IPAttemptWindow: time.Minute, IPAttemptBlock: time.Minute})
	if !limiter.Allow("192.0.2.30", "valid@example.com") {
		t.Fatal("first attempt unexpectedly blocked")
	}
	limiter.Success("valid@example.com")
	if limiter.Allow("192.0.2.30", "other@example.com") {
		t.Fatal("successful authentication incorrectly avoided the IP attempt budget")
	}
}
