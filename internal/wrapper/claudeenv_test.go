package wrapper

import (
	"strings"
	"testing"

	"github.com/inulute/cux/internal/config"
)

func envWarnConfig(reactive bool) *config.Config {
	c := config.Defaults()
	c.AutoSwitchOnRateLimit = reactive
	return &c
}

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestClaudeEnvWarnings(t *testing.T) {
	cases := []struct {
		name     string
		reactive bool
		environ  map[string]string
		want     []string
	}{
		{
			name:     "clean environment says nothing",
			reactive: true,
		},
		{
			name:     "watchdog set",
			reactive: true,
			environ:  map[string]string{envRetryWatchdog: "1"},
			want:     []string{envRetryWatchdog},
		},
		{
			// Claude Code tests this flag for JS truthiness, where "0" is a
			// non-empty string and therefore on. A user who set it to "0"
			// thinking it disabled is the one who most needs the warning.
			name:     "watchdog set to zero is still set",
			reactive: true,
			environ:  map[string]string{envRetryWatchdog: "0"},
			want:     []string{envRetryWatchdog},
		},
		{
			name:     "long retry ladder",
			reactive: true,
			environ:  map[string]string{envMaxRetries: "20"},
			want:     []string{envMaxRetries},
		},
		{
			name:     "short retry ladder is not worth a line",
			reactive: true,
			environ:  map[string]string{envMaxRetries: "3"},
		},
		{
			name:     "unparseable retry count is ignored",
			reactive: true,
			environ:  map[string]string{envMaxRetries: "lots"},
		},
		{
			name:     "both",
			reactive: true,
			environ:  map[string]string{envRetryWatchdog: "true", envMaxRetries: "20"},
			want:     []string{envRetryWatchdog, envMaxRetries},
		},
		{
			// Nothing to suppress, so nothing to say — otherwise this prints
			// on every launch for users who never wanted the reactive path.
			name:     "silent when the reactive path is off anyway",
			reactive: false,
			environ:  map[string]string{envRetryWatchdog: "1", envMaxRetries: "20"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeEnvWarnings(envWarnConfig(tc.reactive), env(tc.environ))
			if len(got) != len(tc.want) {
				t.Fatalf("claudeEnvWarnings = %q, want %d line(s)", got, len(tc.want))
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("line %d = %q, want it to name %s", i, got[i], want)
				}
			}
		})
	}
}

func TestClaudeEnvWarningsToleratesANilConfig(t *testing.T) {
	if got := claudeEnvWarnings(nil, env(map[string]string{envRetryWatchdog: "1"})); got != nil {
		t.Errorf("claudeEnvWarnings(nil) = %q, want none", got)
	}
}
