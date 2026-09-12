package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The endpoint reports windows cux has never heard of, and #52 is what
// happens when it drops them: the seat reads as having room in every window
// cux knows, gets picked as the healthiest in the pool, and refuses the next
// call. Anything shaped like a window has to survive the parse.
func TestParseResponseKeepsEveryWindowItIsGiven(t *testing.T) {
	body := []byte(`{
		"five_hour":                   {"utilization": 10},
		"seven_day":                   {"utilization": 20},
		"seven_day_opus":              {"utilization": 30},
		"seven_day_sonnet":            {"utilization": 40},
		"seven_day_overage_included":  {"utilization": 100},
		"overage":                     {"utilization": 55},
		"seven_day_unreleased_model":  {"utilization": 60},
		"account_uuid":                "not-a-window",
		"organization":                {"name": "acme"},
		"some_count":                  7
	}`)

	u, err := parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 10 {
		t.Errorf("five_hour = %+v, want 10", u.FiveHour)
	}
	if u.SevenDay == nil || u.SevenDay.Utilization != 20 {
		t.Errorf("seven_day = %+v, want 20", u.SevenDay)
	}
	// The account-wide pair has its own fields and must not be duplicated
	// into the model set, or every seat would read as model-capped the
	// moment its weekly window filled.
	for _, reserved := range []string{keyFiveHour, keySevenDay} {
		if _, dup := u.Models[reserved]; dup {
			t.Errorf("%s must not also appear as a model window", reserved)
		}
	}
	want := map[string]float64{
		"seven_day_opus":             30,
		"seven_day_sonnet":           40,
		"seven_day_overage_included": 100, // the Fable window; its API name shares no prefix with its label
		"overage":                    55,  // outside the seven_day_ family entirely
		"seven_day_unreleased_model": 60,  // a model family that needs no cux release
	}
	if len(u.Models) != len(want) {
		t.Fatalf("models = %v, want %d windows", u.Models, len(want))
	}
	for name, util := range want {
		if got := u.Models[name]; got == nil || got.Utilization != util {
			t.Errorf("models[%q] = %+v, want %v", name, got, util)
		}
	}
}

// Fields that are not windows must not become windows. A string, a number or
// an unrelated object at 100%-of-nothing would each read as a hard cap.
func TestParseResponseIgnoresFieldsThatAreNotWindows(t *testing.T) {
	u, err := parseResponse([]byte(`{
		"five_hour":    {"utilization": 10},
		"account_uuid": "u-1",
		"limits":       {"max": 100},
		"nested":       {"window": {"utilization": 99}},
		"nothing":      null
	}`))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(u.Models) != 0 {
		t.Errorf("models = %v, want none — nothing there is a window", u.Models)
	}
}

// The on-disk format is the compatibility contract: a user may downgrade cux
// without being asked to throw away their cache, so a reading written by this
// build has to stay readable by one that knew only the fixed pair.
func TestAccountUsageStaysFlatOnDisk(t *testing.T) {
	reset := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	u := AccountUsage{
		FiveHour: &Window{Utilization: 10, ResetsAt: &reset},
		Models: map[string]*Window{
			"seven_day_opus":             {Utilization: 30},
			"seven_day_overage_included": {Utilization: 100},
		},
		PolledAt: reset,
	}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"five_hour":`, `"seven_day_opus":`, `"seven_day_overage_included":`, `"polled_at":`} {
		if !strings.Contains(got, want) {
			t.Errorf("serialized form %s is missing top-level %s", got, want)
		}
	}
	if strings.Contains(got, `"models"`) {
		t.Errorf("model windows must stay at the top level, got %s", got)
	}
	// A nil window is an absent key, not a null one: a null may read as
	// "present, zero" to a stricter decoder.
	if strings.Contains(got, "null") || strings.Contains(got, `"seven_day"`) {
		t.Errorf("absent windows must be omitted entirely, got %s", got)
	}

	var back AccountUsage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.FiveHour == nil || back.FiveHour.Utilization != 10 || !back.FiveHour.ResetsAt.Equal(reset) {
		t.Errorf("five_hour did not round-trip: %+v", back.FiveHour)
	}
	if back.SevenDay != nil {
		t.Errorf("absent seven_day came back as %+v", back.SevenDay)
	}
	if len(back.Models) != 2 || back.Models["seven_day_opus"].Utilization != 30 {
		t.Errorf("models did not round-trip: %v", back.Models)
	}
	if !back.PolledAt.Equal(u.PolledAt) {
		t.Errorf("polled_at = %v, want %v", back.PolledAt, u.PolledAt)
	}
}

// The other direction of the same contract: a cache written by v0.3.12 has
// to keep working across the upgrade, without a refresh to repopulate it.
func TestAccountUsageReadsACacheWrittenByTheFixedPairBuild(t *testing.T) {
	var c Cache
	if err := json.Unmarshal([]byte(`{
		"uuid-1|org-1": {
			"five_hour":        {"utilization": 26},
			"seven_day":        {"utilization": 57},
			"seven_day_opus":   {"utilization": 100},
			"polled_at":        "2026-09-12T10:00:00Z",
			"token_expired":    true
		}
	}`), &c); err != nil {
		t.Fatalf("unmarshal legacy cache: %v", err)
	}

	u, ok := c["uuid-1|org-1"]
	if !ok {
		t.Fatal("legacy cache entry did not survive the read")
	}
	if u.FiveHour == nil || u.FiveHour.Utilization != 26 || u.SevenDay.Utilization != 57 {
		t.Errorf("account-wide windows = %+v / %+v, want 26 / 57", u.FiveHour, u.SevenDay)
	}
	if w := u.Models["seven_day_opus"]; w == nil || w.Utilization != 100 {
		t.Errorf("legacy model window = %+v, want 100", w)
	}
	if !u.TokenExpired {
		t.Error("token_expired did not survive the read")
	}
	if u.PolledAt.IsZero() {
		t.Error("polled_at did not survive the read — the entry would read as never polled")
	}
}
