package config

import (
	"os"
	"path/filepath"
	"testing"
)

func tempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("CUX_CONFIG_FILE", path)
	return path
}

func TestDefaultsLoaded_WhenFileMissing(t *testing.T) {
	tempConfig(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	def := Defaults()
	if c.Thresholds != def.Thresholds {
		t.Errorf("thresholds: got %+v, want %+v", c.Thresholds, def.Thresholds)
	}
	if c.Strategy.Kind != def.Strategy.Kind {
		t.Errorf("strategy.kind: got %q, want %q", c.Strategy.Kind, def.Strategy.Kind)
	}
	if c.AutoMessage != def.AutoMessage {
		t.Errorf("auto_message: got %q, want %q", c.AutoMessage, def.AutoMessage)
	}
	if c.UpdateCheck.Enabled {
		t.Error("update_check.enabled should default to false")
	}
	if c.UpdateCheck.CadenceHours != 24 {
		t.Errorf("update_check.cadence_hours = %d, want 24", c.UpdateCheck.CadenceHours)
	}
}

func TestPartialFileMergesWithDefaults(t *testing.T) {
	path := tempConfig(t)
	// Write a partial config: only override the strategy kind.
	if err := os.WriteFile(path, []byte(`{"strategy":{"kind":"balanced"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Strategy.Kind != "balanced" {
		t.Errorf("strategy.kind not loaded: %q", c.Strategy.Kind)
	}
	// Other fields should fall back to defaults.
	if c.AutoMessage != Defaults().AutoMessage {
		t.Errorf("auto_message lost defaults: got %q", c.AutoMessage)
	}
	if c.Thresholds.FiveHour != 90 {
		t.Errorf("thresholds.five_hour lost defaults: got %d", c.Thresholds.FiveHour)
	}
}

func TestSetAndSave(t *testing.T) {
	tempConfig(t)
	c := Defaults()

	c, err := Set(c, "thresholds.seven_day", "80")
	if err != nil {
		t.Fatal(err)
	}
	if c.Thresholds.SevenDay != 80 {
		t.Fatalf("thresholds.seven_day = %d, want 80", c.Thresholds.SevenDay)
	}

	c, err = Set(c, "strategy.kind", "MANUAL")
	if err != nil {
		t.Fatal(err)
	}
	if c.Strategy.Kind != "manual" {
		t.Fatalf("strategy.kind = %q, want manual", c.Strategy.Kind)
	}

	c, err = Set(c, "strategy.order", "a@x, b@x , c@x")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Strategy.Order) != 3 || c.Strategy.Order[0] != "a@x" || c.Strategy.Order[2] != "c@x" {
		t.Fatalf("strategy.order = %v", c.Strategy.Order)
	}

	c, err = Set(c, "strategy.order", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Strategy.Order) != 0 {
		t.Fatalf("strategy.order should be empty, got %v", c.Strategy.Order)
	}

	c, err = Set(c, "auto_resume", "off")
	if err != nil {
		t.Fatal(err)
	}
	if c.AutoResume {
		t.Fatal("auto_resume should be false")
	}

	c, err = Set(c, "auto_message", `""`)
	if err != nil {
		t.Fatal(err)
	}
	if c.AutoMessage != "" {
		t.Fatalf("auto_message = %q, want empty", c.AutoMessage)
	}

	c, err = Set(c, "update_check.enabled", "yes")
	if err != nil {
		t.Fatal(err)
	}
	if !c.UpdateCheck.Enabled {
		t.Fatal("update_check.enabled should be true")
	}

	c, err = Set(c, "update_check.cadence_hours", "12")
	if err != nil {
		t.Fatal(err)
	}
	if c.UpdateCheck.CadenceHours != 12 {
		t.Fatalf("update_check.cadence_hours = %d, want 12", c.UpdateCheck.CadenceHours)
	}

	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	// Round-trip
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Thresholds.SevenDay != 80 || got.Strategy.Kind != "manual" || got.AutoResume || got.AutoMessage != "" || !got.UpdateCheck.Enabled || got.UpdateCheck.CadenceHours != 12 {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

func TestSetRejectsBadInput(t *testing.T) {
	c := Defaults()
	cases := []struct{ key, val string }{
		{"thresholds.five_hour", "200"},
		{"thresholds.seven_day", "abc"},
		{"strategy.kind", "raid"},
		{"auto_resume", "maybe"},
		{"update_check.enabled", "maybe"},
		{"update_check.cadence_hours", "0"},
		{"poll_interval_seconds", "-5"},
		{"unknown.thing", "x"},
	}
	for _, tc := range cases {
		if _, err := Set(c, tc.key, tc.val); err == nil {
			t.Errorf("Set(%q, %q) should fail, didn't", tc.key, tc.val)
		}
	}
}

func TestResolvedStrategy(t *testing.T) {
	c := Defaults()
	c.Strategy.Kind = "balanced"
	if c.ResolvedStrategy().String() != "balanced" {
		t.Fatalf("ResolvedStrategy = %v", c.ResolvedStrategy())
	}
}
