package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeDirFollowsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	t.Setenv(EnvClaudeConfigDir, "")
	if got, want := ClaudeDir(), filepath.Join(home, ".claude"); got != want {
		t.Fatalf("ClaudeDir() = %q, want %q", got, want)
	}

	profile := filepath.Join(home, ".claude-work")
	t.Setenv(EnvClaudeConfigDir, profile)
	if got := ClaudeDir(); got != profile {
		t.Fatalf("ClaudeDir() = %q, want %q", got, profile)
	}
	if got, want := ClaudeCredentials(), filepath.Join(profile, ".credentials.json"); got != want {
		t.Fatalf("ClaudeCredentials() = %q, want %q", got, want)
	}
	if got, want := ClaudeProjectsDir(), filepath.Join(profile, "projects"); got != want {
		t.Fatalf("ClaudeProjectsDir() = %q, want %q", got, want)
	}
}

// Under CLAUDE_CONFIG_DIR the identity file is always the directory's own:
// falling back to $HOME/.claude.json would name the default login.
func TestClaudeConfigNeverFallsBackOutOfClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvClaudeConfigDir, "")
	if got, want := ClaudeConfig(), filepath.Join(home, ".claude.json"); got != want {
		t.Fatalf("default ClaudeConfig() = %q, want the legacy sibling %q", got, want)
	}

	profile := filepath.Join(home, ".claude-work")
	t.Setenv(EnvClaudeConfigDir, profile)
	if got, want := ClaudeConfig(), filepath.Join(profile, ".claude.json"); got != want {
		t.Fatalf("ClaudeConfig() = %q, want %q", got, want)
	}
}
