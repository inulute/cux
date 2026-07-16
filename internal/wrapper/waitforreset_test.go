package wrapper

import (
	"testing"

	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// TestAllTokensExpired pins waitForReset's only remaining hard exit:
// waiting is declared futile ONLY when every pooled account's
// credentials are known-expired. Anything ambiguous (missing usage
// data, one healthy token) must keep the wait alive — the old
// fixed-attempt version turned a normal all-exhausted night into four
// short waits and a dead session.
func TestAllTokensExpired(t *testing.T) {
	accounts := map[int]store.Account{
		1: {Slot: 1, Email: "a@x.test"},
		2: {Slot: 2, Email: "b@x.test"},
	}

	t.Run("all expired → futile", func(t *testing.T) {
		cache := usage.Cache{
			"a@x.test": {TokenExpired: true},
			"b@x.test": {TokenExpired: true},
		}
		if !allTokensExpired(accounts, cache) {
			t.Error("want true when every account needs a login")
		}
	})

	t.Run("one live token → keep waiting", func(t *testing.T) {
		cache := usage.Cache{
			"a@x.test": {TokenExpired: true},
			"b@x.test": {TokenExpired: false},
		}
		if allTokensExpired(accounts, cache) {
			t.Error("a single healthy token means the wait can succeed")
		}
	})

	t.Run("missing usage data counts as healable", func(t *testing.T) {
		cache := usage.Cache{"a@x.test": {TokenExpired: true}}
		if allTokensExpired(accounts, cache) {
			t.Error("an account the next refresh may fill in must not end the wait")
		}
	})

	t.Run("empty pool → nothing to wait for", func(t *testing.T) {
		if !allTokensExpired(map[int]store.Account{}, usage.Cache{}) {
			t.Error("want true for an empty pool")
		}
	})
}
