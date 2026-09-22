package store

import "testing"

// The live token yields uuid|org; a seat added before cux captured UUIDs
// stores none, so its CacheKey() is its email. Unless the live login is
// resolved back to its stored seat, the two spellings never match — rotation
// then fails to exclude the seat the session is on and /switch swaps to
// itself ("claude@x → claude@x, resuming…").
func TestLiveSeatResolvesAcrossKeySpellings(t *testing.T) {
	st := &State{
		ActiveSlot: 2,
		Accounts: map[int]Account{
			1: {Slot: 1, Email: "Rishabh.anand@nineti.de"},
			2: {Slot: 2, Email: "claude@nineti.de"},
		},
	}

	seat, ok := st.LiveSeat("claude@nineti.de", "acct-uuid|org-uuid")
	if !ok || seat.Slot != 2 {
		t.Fatalf("got (slot %d, %v), want the stored seat 2", seat.Slot, ok)
	}
	if seat.CacheKey() != "claude@nineti.de" {
		t.Errorf("CacheKey() = %q, want the store's own spelling", seat.CacheKey())
	}

	// Case differences in the email must not defeat it.
	if seat, ok := st.LiveSeat("CLAUDE@nineti.de", "acct-uuid|org-uuid"); !ok || seat.Slot != 2 {
		t.Errorf("got (slot %d, %v), want slot 2 regardless of case", seat.Slot, ok)
	}
}

// When the store does have the UUID, the key matches outright and ActiveSlot
// is never consulted — so a stale ActiveSlot cannot mislabel the live seat.
func TestLiveSeatPrefersTheKeyOverActiveSlot(t *testing.T) {
	st := &State{
		ActiveSlot: 1, // stale
		Accounts: map[int]Account{
			1: {Slot: 1, Email: "a@x.test", UUID: "u-a", OrgUUID: "o-a"},
			2: {Slot: 2, Email: "b@x.test", UUID: "u-b", OrgUUID: "o-b"},
		},
	}
	seat, ok := st.LiveSeat("b@x.test", "u-b|o-b")
	if !ok || seat.Slot != 2 {
		t.Fatalf("got (slot %d, %v), want slot 2 from the key", seat.Slot, ok)
	}
}

// A live login that is not managed here must not be mapped onto whatever
// ActiveSlot happens to say.
func TestLiveSeatRejectsAnUnmanagedLogin(t *testing.T) {
	st := &State{
		ActiveSlot: 1,
		Accounts:   map[int]Account{1: {Slot: 1, Email: "a@x.test"}},
	}
	if seat, ok := st.LiveSeat("stranger@x.test", "u-s|o-s"); ok {
		t.Errorf("got (slot %d, true), want no match", seat.Slot)
	}
}

// The cold-cache case: before LiveSeat, callers guessed the live seat's key
// by looking for it in the usage cache. A fresh install has no cache, so the
// guess kept the token's uuid|org key, matched no candidate, and rotation
// offered the seat the session was already on.
func TestLiveSeatWorksWithNoUsageCache(t *testing.T) {
	st := &State{
		ActiveSlot: 2,
		Accounts: map[int]Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
		},
	}
	seat, ok := st.LiveSeat("b@x.test", "uuid|org")
	if !ok || seat.CacheKey() != "b@x.test" {
		t.Fatalf("got (%q, %v), want the store's own key", seat.CacheKey(), ok)
	}
}
