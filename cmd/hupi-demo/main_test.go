package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientIP_TrustsTheRightmostForwardedForEntry is a real regression
// test for review finding B3: clientIP used to take the leftmost
// X-Forwarded-For entry, which is whatever the original client claimed
// — fully attacker-controlled under the common "append" reverse-proxy
// pattern, since a proxy appends its own observed address rather than
// replacing the header. That let a client fabricate an arbitrary
// X-Forwarded-For value and get a fresh rate-limit bucket on every
// request, defeating the per-IP limiter clientIP feeds entirely.
func TestClientIP_TrustsTheRightmostForwardedForEntry(t *testing.T) {
	tests := []struct {
		name string
		xff  string
		want string
	}{
		{
			name: "single entry (the common case: exactly one proxy hop)",
			xff:  "203.0.113.7",
			want: "203.0.113.7",
		},
		{
			name: "attacker-prepended entry followed by the proxy's real one",
			xff:  "1.2.3.4, 203.0.113.7",
			want: "203.0.113.7",
		},
		{
			name: "attacker sends several fabricated entries, proxy appends its own last",
			xff:  "1.2.3.4, 5.6.7.8, 9.9.9.9, 203.0.113.7",
			want: "203.0.113.7",
		},
		{
			name: "no surrounding whitespace variant still trims correctly",
			xff:  "1.2.3.4,203.0.113.7",
			want: "203.0.113.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Forwarded-For", tt.xff)

			got := clientIP(req)
			if got != tt.want {
				t.Errorf("clientIP() with X-Forwarded-For=%q = %q, want %q", tt.xff, got, tt.want)
			}
		})
	}
}

func TestClientIP_FallsBackToRemoteAddrWhenHeaderAbsent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "198.51.100.5:54321"

	got := clientIP(req)
	if got != "198.51.100.5" {
		t.Errorf("clientIP() with no X-Forwarded-For = %q, want %q", got, "198.51.100.5")
	}
}
