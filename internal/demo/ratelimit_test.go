package demo

import (
	"testing"
	"time"
)

func TestIPRateLimiter_AllowsUpToMaxThenDenies(t *testing.T) {
	l := NewIPRateLimiter(3, time.Hour)

	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("call %d: expected allowed", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("4th call: expected denied")
	}
}

func TestIPRateLimiter_TracksIPsIndependently(t *testing.T) {
	l := NewIPRateLimiter(1, time.Hour)

	if !l.Allow("1.1.1.1") {
		t.Fatal("first IP's first call: expected allowed")
	}
	if !l.Allow("2.2.2.2") {
		t.Fatal("second IP's first call: expected allowed (independent of the first IP)")
	}
	if l.Allow("1.1.1.1") {
		t.Fatal("first IP's second call: expected denied")
	}
}

func TestIPRateLimiter_WindowExpires(t *testing.T) {
	l := NewIPRateLimiter(1, 10*time.Millisecond)

	if !l.Allow("1.2.3.4") {
		t.Fatal("first call: expected allowed")
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("second call within window: expected denied")
	}
	time.Sleep(20 * time.Millisecond)
	if !l.Allow("1.2.3.4") {
		t.Fatal("call after window elapsed: expected allowed")
	}
}
