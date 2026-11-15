package creds

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The suffix is the first 8 hex digits of sha256 of the directory, the
// name Claude Code gives the Keychain item of a non-default config dir.
func TestLoginKeychainServiceIsSuffixedByTheDirectoryHash(t *testing.T) {
	if got, want := loginKeychainService("/home/user/.claude-work"), "Claude Code-credentials-174d0719"; got != want {
		t.Errorf("loginKeychainService = %q, want %q", got, want)
	}
}

func TestReadLoginAtReadsTheStagedFile(t *testing.T) {
	t.Setenv(envCredsBackend, "file")
	dir := t.TempDir()

	if _, err := ReadLoginAt(dir); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty dir: err = %v, want ErrNotFound", err)
	}

	mcpOnly := `{"mcpOAuth":{}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(mcpOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLoginAt(dir); !errors.Is(err, ErrNoAccountToken) {
		t.Fatalf("no account token: err = %v, want ErrNoAccountToken", err)
	}

	blob := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLoginAt(dir)
	if err != nil || got != blob {
		t.Fatalf("ReadLoginAt = %q, %v; want the staged blob", got, err)
	}
	// With the file backend there is no Keychain item to remove.
	if err := DeleteLoginAt(dir); err != nil {
		t.Errorf("DeleteLoginAt: %v", err)
	}
}
