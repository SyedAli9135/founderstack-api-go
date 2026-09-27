package reports

import (
	"testing"
	"time"
)

func TestWindow_UsesTheWorkspaceTimezoneAndIsHalfOpen(t *testing.T) {
	karachi, err := time.LoadLocation("Asia/Karachi")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, karachi)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, karachi)
	start, end := window(from, to, karachi)
	if got := start.UTC(); !got.Equal(time.Date(2026, 8, 31, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %v, want Aug 31 19:00 UTC (Sep 1 00:00 Karachi)", got)
	}
	if got := end.UTC(); !got.Equal(time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %v, want Sep 30 19:00 UTC (Oct 1 00:00 Karachi, exclusive)", got)
	}
}

func TestWindow_SingleDay(t *testing.T) {
	d := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	start, end := window(d, d, time.UTC)
	if end.Sub(start) != 24*time.Hour {
		t.Fatalf("single-day window = %v, want 24h", end.Sub(start))
	}
}

func TestStatus(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		revoked bool
		expires time.Time
		want    string
	}{
		{false, now.Add(time.Hour), "active"},
		{false, now, "expired"},
		{false, now.Add(-time.Hour), "expired"},
		{true, now.Add(time.Hour), "revoked"},
		{true, now.Add(-time.Hour), "revoked"},
	}
	for _, c := range cases {
		if got := status(c.revoked, c.expires, now); got != c.want {
			t.Errorf("status(%v, %v) = %q, want %q", c.revoked, c.expires, got, c.want)
		}
	}
}

func TestNewShareToken_IsURLSafeAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := newShareToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 32 {
			t.Fatalf("len = %d, want 32", len(tok))
		}
		for _, r := range tok {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				t.Fatalf("token %q has a non-URL-safe character %q", tok, r)
			}
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
}
