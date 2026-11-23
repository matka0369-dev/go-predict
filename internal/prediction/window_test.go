package prediction

import (
	"errors"
	"testing"
	"time"
)

func TestCheckWindow(t *testing.T) {
	opens := time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC)
	closes := opens.Add(time.Hour)
	at := func(d time.Duration) time.Time { return opens.Add(d) }

	cases := []struct {
		name string
		typ  Type
		now  time.Time
		want error
	}{
		{"open bet long before open", OpenSingle, at(-5 * time.Hour), nil},
		{"open bet just inside the buffer", OpenSingle, at(-90 * time.Second), nil},
		{"open bet in the last minute", OpenSingle, at(-30 * time.Second), ErrPastCutoff},
		{"open bet after open time", OpenSinglePana, at(time.Second), ErrPastCutoff},
		{"jodi follows the open side", Jodi, at(time.Second), ErrPastCutoff},
		{"close bet before open time", CloseSingle, at(-time.Second), ErrNotYetOpen},
		{"close bet long before open", CloseDoublePana, at(-5 * time.Hour), ErrNotYetOpen},
		{"close bet exactly at open", CloseSingle, at(0), nil},
		{"close bet mid-game", CloseSinglePana, at(30 * time.Minute), nil},
		{"close bet in the last minute", CloseSingle, closes.Add(-30 * time.Second), ErrPastCutoff},
		{"close bet after close", CloseSingle, closes.Add(time.Minute), ErrPastCutoff},
	}
	for _, c := range cases {
		if got := checkWindow(c.typ, c.now, opens, closes); !errors.Is(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
