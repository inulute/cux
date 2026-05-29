package hookinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAddsStopFailureRateLimitHook(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	changed, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(changed, "StopFailure") {
		t.Fatalf("Install changed events should include StopFailure, got %v", changed)
	}

	b, err := os.ReadFile(filepath.Join(tmp, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	entries := settings.Hooks["StopFailure"]
	if len(entries) != 1 || len(entries[0].Hooks) != 1 {
		t.Fatalf("StopFailure hook not installed correctly: %#v", entries)
	}
	if !strings.Contains(entries[0].Hooks[0].Command, "cux hook rate-limit") {
		t.Fatalf("StopFailure should run rate-limit hook, got %q", entries[0].Hooks[0].Command)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
