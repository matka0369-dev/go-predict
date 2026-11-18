package handlers

import (
	"regexp"
	"testing"
)

func TestNewShortCode(t *testing.T) {
	re := regexp.MustCompile(`^[ABCDEFGHJKMNPQRSTUVWXYZ23456789]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		c := newShortCode()
		if !re.MatchString(c) {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 495 {
		t.Errorf("codes collide far too often: %d unique of 500", len(seen))
	}
}
