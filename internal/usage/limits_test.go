package usage

import "testing"

// Model-scoped weekly caps arrive only in limits[]; the endpoint's top-level
// seven_day_<model> keys are null. Each weekly_scoped entry naming a model
// becomes a seven_day_<model> window, and a top-level window at 0% with no
// reset is dropped as noise.
func TestLimitsArrayBecomesModelWindows(t *testing.T) {
	body := `{
	  "five_hour": {"utilization": 0, "resets_at": null},
	  "seven_day": {"utilization": 54, "resets_at": "2026-09-19T02:59:59Z"},
	  "seven_day_opus": null,
	  "nimbus_quill": {"utilization": 0, "resets_at": null},
	  "limits": [
	    {"kind": "session", "percent": 0, "resets_at": null, "scope": null},
	    {"kind": "weekly", "percent": 54, "resets_at": "2026-09-19T02:59:59Z", "scope": null},
	    {"kind": "weekly_scoped", "percent": 42, "resets_at": "2026-09-19T03:00:00Z",
	     "scope": {"model": {"display_name": "Fable"}}}
	  ]
	}`
	u, err := parseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	f := u.Models["seven_day_fable"]
	if f == nil || f.Utilization != 42 || f.ResetsAt == nil {
		t.Fatalf("seven_day_fable = %+v, want 42%% with a reset", f)
	}
	if _, ok := u.Models["limits"]; ok {
		t.Error("limits must not be stored as a window")
	}
	if _, ok := u.Models["nimbus_quill"]; ok {
		t.Error("an empty top-level window must be dropped")
	}
	if u.SevenDay == nil || u.SevenDay.Utilization != 54 {
		t.Errorf("seven_day = %+v, want 54", u.SevenDay)
	}
}
