package usage

import "testing"

// Display names carry a version and the version moves, so the key has to be
// the family — otherwise a seat capped on Fable keys seven_day_fable one week
// and "seven_day_claude fable 5.1" the next, and nothing matches either.
func TestModelWindowKey(t *testing.T) {
	cases := map[string]string{
		"Fable":            "seven_day_fable",
		"Fable 5.1":        "seven_day_fable",
		"Claude Fable 5.1": "seven_day_fable",
		"  opus  ":         "seven_day_opus",
		"Sonnet":           "seven_day_sonnet",
		// Not a family cux knows: it still gets a window, just under its own
		// slugged name, so a seat capped on it still ranks (#52).
		"Nimbus Quill": "seven_day_nimbus_quill",
		"":             "",
		"   ":          "",
		"5.1":          "seven_day_5_1",
	}
	for in, want := range cases {
		if got := modelWindowKey(in); got != want {
			t.Errorf("modelWindowKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// A limits[] entry and a top-level key can name the same window. `range raw`
// is map order, so whichever wins has to be decided outside that loop or the
// cached number changes from run to run.
func TestLimitsWinsOverTopLevelDeterministically(t *testing.T) {
	body := `{
	  "seven_day_fable": {"utilization": 11, "resets_at": null},
	  "limits": [{"kind": "weekly_scoped", "percent": 42, "resets_at": null,
	              "scope": {"model": {"display_name": "Claude Fable 5.1"}}}]
	}`
	for i := 0; i < 200; i++ {
		u, err := parseResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		w := u.Models["seven_day_fable"]
		if w == nil || w.Utilization != 42 {
			t.Fatalf("run %d: seven_day_fable = %+v, want 42 from limits[]", i, w)
		}
		if len(u.Models) != 1 {
			t.Fatalf("run %d: the same window is keyed %d ways: %v", i, len(u.Models), u.Models)
		}
	}
}
