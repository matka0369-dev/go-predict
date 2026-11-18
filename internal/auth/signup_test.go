package auth

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"9876543210", "919876543210", true},      // bare 10 digits get the default country code
		{"98765 43210", "919876543210", true},     // spaces
		{"+91 98765-43210", "919876543210", true}, // plus and dash
		{"919876543210", "919876543210", true},    // already has it
		{"123", "", false},                        // too short
		{"1234567890123456", "", false},           // too long
		{"abcdefghij", "", false},                 // no digits
	}
	for _, c := range cases {
		got, ok := normalizePhone(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizePhone(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUsernameBase(t *testing.T) {
	if got := usernameBase("Ramesh Kumar", "919876544821"); got != "rameshkumar_4821" {
		t.Errorf("got %q", got)
	}
	if got := usernameBase("రమేష్", "919876544821"); got != "player_4821" {
		t.Errorf("non-latin name should fall back, got %q", got)
	}
	if got := usernameBase("A very long name indeed yes", "919876544821"); len(got) > 14+5 {
		t.Errorf("username too long: %q", got)
	}
}

func TestThrottleCountsAndResets(t *testing.T) {
	th := newThrottle(50 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if !th.allow("k", 3) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if th.allow("k", 3) {
		t.Error("4th attempt in the window should be refused")
	}
	if !th.allow("other", 3) {
		t.Error("a different key has its own allowance")
	}
	time.Sleep(60 * time.Millisecond)
	if !th.allow("k", 3) {
		t.Error("the allowance should reset after the window")
	}
}

func TestClientIPPrefersForwardedFor(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if got := clientIP(r); got != "10.0.0.1" {
		t.Errorf("got %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("got %q", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := clientIP(r); got != "10.0.0.1" {
		t.Errorf("garbage header should fall back, got %q", got)
	}
}
