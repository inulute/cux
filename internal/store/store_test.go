package store

import "testing"

func TestCacheKeyDistinguishesSeats(t *testing.T) {
	// Different accounts inside the same organization must not share a key.
	a := Account{Email: "a@corp.test", UUID: "uuid-a", OrgUUID: "org-1"}
	b := Account{Email: "b@corp.test", UUID: "uuid-b", OrgUUID: "org-1"}
	if a.CacheKey() == b.CacheKey() {
		t.Errorf("same-org accounts share cache key %q", a.CacheKey())
	}

	// The same email in two organizations must not share a key either.
	pro := Account{Email: "me@x.test", UUID: "uuid-me", OrgUUID: "org-personal"}
	team := Account{Email: "me@x.test", UUID: "uuid-me", OrgUUID: "org-team"}
	if pro.CacheKey() == team.CacheKey() {
		t.Errorf("same-email cross-org accounts share cache key %q", pro.CacheKey())
	}
}

func TestCacheKeyLegacyFallbacks(t *testing.T) {
	// Pre-UUID slots keep the org key; pre-org slots keep the email key.
	orgOnly := Account{Email: "a@x.test", OrgUUID: "org-1"}
	if got := orgOnly.CacheKey(); got != "org-1" {
		t.Errorf("org-only account: got %q, want %q", got, "org-1")
	}
	emailOnly := Account{Email: "a@x.test"}
	if got := emailOnly.CacheKey(); got != "a@x.test" {
		t.Errorf("email-only account: got %q, want %q", got, "a@x.test")
	}
}

func TestFindByIdentityPersonalVsOrgSeat(t *testing.T) {
	s := &State{Accounts: map[int]Account{
		1: {Slot: 1, Email: "me@x.test", UUID: "u-me"},                      // personal: no org
		2: {Slot: 2, Email: "me@x.test", UUID: "u-me", OrgUUID: "org-corp"}, // org seat
	}}
	if got := s.FindByIdentity("me@x.test", ""); got != 1 {
		t.Errorf("personal login resolved to slot %d, want 1", got)
	}
	if got := s.FindByIdentity("me@x.test", "org-corp"); got != 2 {
		t.Errorf("org login resolved to slot %d, want 2", got)
	}
}

func TestCacheKeyPersonalAccountFallsBackToEmail(t *testing.T) {
	personal := Account{Email: "me@x.test", UUID: "u-me"} // no OrgUUID
	if got := personal.CacheKey(); got != "me@x.test" {
		t.Errorf("personal CacheKey = %q, want email fallback", got)
	}
}
