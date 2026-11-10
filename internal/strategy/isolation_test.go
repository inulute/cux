package strategy

import (
	"os"
	"testing"
)

// TestMain keeps this package's tests off the developer's real Claude Code
// login. Tests isolate HOME, but cux follows CLAUDE_CONFIG_DIR ahead of HOME,
// so an exported value would point them at a real profile.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CLAUDE_CONFIG_DIR")
	_ = os.Unsetenv("CUX_SCOPE")
	_ = os.Unsetenv("CUX_SHARED_CONFIG_DIR")
	os.Exit(m.Run())
}
