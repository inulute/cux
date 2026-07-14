package wrapper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/usage"
)

// isolateLive sets up a temp HOME with a fake live login plus managed
// state, mirroring what another concurrent session leaves behind after
// swapping the live account.
func isolateLive(t *testing.T, liveEmail string, cache usage.Cache) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("CUX_CREDS_BACKEND", "file")
	t.Setenv("CUX_CONFIG_FILE", filepath.Join(tmp, "config.json"))

	claudeDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeJSON := `{"oauthAccount":{"emailAddress":"` + liveEmail + `","accountUuid":"u-live"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"), []byte(claudeJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	state := &store.State{
		ActiveSlot: 2,
		Sequence:   []int{1, 2},
		Accounts: map[int]store.Account{
			1: {Slot: 1, Email: "limited@x.test"},
			2: {Slot: 2, Email: "fresh@x.test"},
		},
	}
	if err := state.Save(); err != nil {
		t.Fatal(err)
	}
	if err := usage.SaveCache(cache); err != nil {
		t.Fatal(err)
	}
}

func TestLiveAccountWithCapacity(t *testing.T) {
	cfg := config.Defaults()

	t.Run("healthy live account skips the swap", func(t *testing.T) {
		// Another session already swapped live to fresh@x.test.
		isolateLive(t, "fresh@x.test", usage.Cache{
			"limited@x.test": hookStyleUsage(100, 40),
			"fresh@x.test":   hookStyleUsage(5, 10),
		})
		acct, ok := liveAccountWithCapacity(&cfg)
		if !ok || acct.Email != "fresh@x.test" {
			t.Errorf("got (%q, %v), want (fresh@x.test, true)", acct.Email, ok)
		}
	})

	t.Run("exhausted live account does not skip", func(t *testing.T) {
		isolateLive(t, "limited@x.test", usage.Cache{
			"limited@x.test": hookStyleUsage(100, 40),
			"fresh@x.test":   hookStyleUsage(5, 10),
		})
		if _, ok := liveAccountWithCapacity(&cfg); ok {
			t.Error("expected ok=false for an exhausted live account")
		}
	})

	t.Run("no usage data means no skip", func(t *testing.T) {
		isolateLive(t, "fresh@x.test", usage.Cache{})
		if _, ok := liveAccountWithCapacity(&cfg); ok {
			t.Error("expected ok=false without usage data")
		}
	})

	t.Run("unmanaged live account does not skip", func(t *testing.T) {
		isolateLive(t, "stranger@x.test", usage.Cache{
			"fresh@x.test": hookStyleUsage(5, 10),
		})
		if _, ok := liveAccountWithCapacity(&cfg); ok {
			t.Error("expected ok=false for an unmanaged live account")
		}
	})

	t.Run("expired token does not skip", func(t *testing.T) {
		u := hookStyleUsage(5, 10)
		u.TokenExpired = true
		isolateLive(t, "fresh@x.test", usage.Cache{"fresh@x.test": u})
		if _, ok := liveAccountWithCapacity(&cfg); ok {
			t.Error("expected ok=false for an expired token")
		}
	})
}

func hookStyleUsage(five, seven float64) usage.AccountUsage {
	return usage.AccountUsage{
		FiveHour: &usage.Window{Utilization: five},
		SevenDay: &usage.Window{Utilization: seven},
	}
}
