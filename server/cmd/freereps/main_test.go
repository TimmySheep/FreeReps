package main

import (
	"testing"
	"time"
)

func TestRederiveRange(t *testing.T) {
	day := func(s string) time.Time {
		parsed, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		return parsed
	}

	t.Run("explicit days cover the last one whole", func(t *testing.T) {
		from, to, err := rederiveRange("2026-11-01", "2026-11-12")
		if err != nil {
			t.Fatalf("rederiveRange: %v", err)
		}
		if !from.Equal(day("2026-11-01")) {
			t.Errorf("from = %v, want 2026-11-01 00:00 UTC", from)
		}
		// The night of the 12th ends on the 13th, so the whole 12th is read.
		if want := day("2026-11-13"); !to.Equal(want) {
			t.Errorf("to = %v, want %v (exclusive end past the named day)", to, want)
		}
	})

	t.Run("empty flags leave both ends open", func(t *testing.T) {
		from, to, err := rederiveRange("", "")
		if err != nil {
			t.Fatalf("rederiveRange: %v", err)
		}
		if want := day("1970-01-01"); !from.Equal(want) {
			t.Errorf("from = %v, want %v", from, want)
		}
		if !to.After(time.Now().UTC()) {
			t.Errorf("to = %v, want a bound past now", to)
		}
	})

	t.Run("a day that is not a date is rejected", func(t *testing.T) {
		if _, _, err := rederiveRange("12.11.2026", ""); err == nil {
			t.Error("accepted -rederive-from 12.11.2026")
		}
		if _, _, err := rederiveRange("", "yesterday"); err == nil {
			t.Error("accepted -rederive-to yesterday")
		}
	})

	t.Run("a reversed range is rejected", func(t *testing.T) {
		if _, _, err := rederiveRange("2026-11-12", "2026-11-01"); err == nil {
			t.Error("accepted a range that ends before it starts")
		}
		if _, _, err := rederiveRange("2026-11-12", "2026-11-12"); err != nil {
			t.Errorf("rejected a single day: %v", err)
		}
	})
}
