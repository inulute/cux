package usage

import "testing"

// Untrimmed /api/oauth/usage body from a Max seat at its Fable cap
// (2026-09-23; spend and seven_day_breakdown objects shortened). The cap is
// reported once, in limits[]: no top-level seven_day_overage_included key, and
// seven_day_opus / seven_day_sonnet are null. It must land as exactly one
// model window, so `usage show` cannot print it twice.
func TestLiveFableCappedResponseYieldsOneModelWindow(t *testing.T) {
	body := `{
	  "five_hour": {"utilization": 2.0, "resets_at": "2026-09-23T12:59:59.593033+00:00", "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
	  "seven_day": {"utilization": 59.0, "resets_at": "2026-09-26T02:59:59.593054+00:00", "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
	  "seven_day_oauth_apps": null, "seven_day_opus": null, "seven_day_sonnet": null,
	  "seven_day_cowork": null, "seven_day_omelette": null, "tangelo": null,
	  "iguana_necktie": null, "omelette_promotional": null,
	  "nimbus_quill": {"utilization": 0.0, "resets_at": null, "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
	  "cinder_cove": null, "copper_kite": null, "harbor_lantern": null, "wattle_ember": null,
	  "amber_ladder": null, "juniper_tide": null, "cedar_ember": null, "amber_gauge": null,
	  "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null, "currency": null},
	  "limits": [
	    {"kind": "session", "group": "session", "percent": 2, "severity": "normal", "resets_at": "2026-09-23T12:59:59.593033+00:00", "scope": null, "is_active": false},
	    {"kind": "weekly_all", "group": "weekly", "percent": 59, "severity": "normal", "resets_at": "2026-09-26T02:59:59.593054+00:00", "scope": null, "is_active": false},
	    {"kind": "weekly_scoped", "group": "weekly", "percent": 100, "severity": "critical", "resets_at": "2026-09-26T02:59:59.593224+00:00",
	     "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": true}
	  ],
	  "spend": {"used": {"amount_minor": 0, "currency": "USD", "exponent": 2}, "limit": null, "percent": 0, "severity": "normal"},
	  "member_dashboard_available": false,
	  "seven_day_breakdown": {"as_of": "2026-09-23T10:02:29.630622+00:00", "rows": [{"key": "claude_code", "display_name": "Claude Code", "percent": 100}]}
	}`
	u, err := parseResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Models) != 1 {
		t.Fatalf("model windows = %v, want only seven_day_fable", u.Models)
	}
	f := u.Models["seven_day_fable"]
	if f == nil || f.Utilization != 100 || f.ResetsAt == nil {
		t.Fatalf("seven_day_fable = %+v, want 100%% with a reset", f)
	}
	if u.SevenDay == nil || u.SevenDay.Utilization != 59 {
		t.Errorf("seven_day = %+v, want 59", u.SevenDay)
	}
}
