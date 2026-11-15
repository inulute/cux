package main

import (
	"slices"
	"testing"
)

// The login child must store its result in the staging dir and nowhere
// else, and must not authenticate from a token in the environment.
func TestLoginEnvPointsClaudeAtTheStagingDir(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"CLAUDE_CONFIG_DIR=/home/user/.claude",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=/home/user/.claude",
		"CLAUDE_CODE_OAUTH_TOKEN=x",
		"ANTHROPIC_API_KEY=x",
		"CLAUDE_CONFIG_DIR_EXTRA=kept",
		"Anthropic_Auth_Token=x",
	}
	got := loginEnv(environ, "/stage")
	want := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR_EXTRA=kept", "CLAUDE_CONFIG_DIR=/stage"}
	if !slices.Equal(got, want) {
		t.Errorf("loginEnv = %q, want %q", got, want)
	}
}
