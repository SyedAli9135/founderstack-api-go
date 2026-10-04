package runs

import "testing"

func TestStreamLimiter_BoundsPerUserAndPerOrgAndFreesSlots(t *testing.T) {
	l := newStreamLimiter()
	for i := 0; i < maxStreamsPerUser; i++ {
		if !l.acquire("u1", "org") {
			t.Fatalf("stream %d refused, want it allowed", i+1)
		}
	}
	if l.acquire("u1", "org") {
		t.Fatal("a user's 11th stream was allowed")
	}
	if !l.acquire("u2", "org") {
		t.Fatal("another user in the same org was refused")
	}
	l.release("u1", "org")
	if !l.acquire("u1", "org") {
		t.Fatal("a freed slot wasn't reusable")
	}

	// Fill the org with many users (each below the per-user cap).
	l2 := newStreamLimiter()
	allowed := 0
	for i := 0; i < 100; i++ {
		if l2.acquire("user-"+string(rune('a'+i%26))+string(rune('A'+i/26)), "big-org") {
			allowed++
		}
	}
	if allowed != maxStreamsPerOrg {
		t.Fatalf("org allowed %d streams, want %d", allowed, maxStreamsPerOrg)
	}
	// Maps don't grow once everything is released.
	l3 := newStreamLimiter()
	l3.acquire("u", "o")
	l3.release("u", "o")
	if len(l3.perUser) != 0 || len(l3.perOrg) != 0 {
		t.Fatal("released slots left entries behind")
	}
}
