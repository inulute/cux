package wrapper

import "testing"

func TestResumableSession(t *testing.T) {
	cases := []struct {
		name       string
		sessionID  string
		argv       []string
		autoResume bool
		hadTurns   bool
		wantSID    string
		wantOK     bool
	}{
		{
			name:      "turn completed resumes the hook-reported session",
			sessionID: "sid-1", argv: []string{"--verbose"},
			autoResume: true, hadTurns: true,
			wantSID: "sid-1", wantOK: true,
		},
		{
			name:      "auto_resume off never resumes",
			sessionID: "sid-1", argv: []string{"--resume", "sid-1"},
			autoResume: false, hadTurns: true,
			wantSID: "", wantOK: false,
		},
		{
			name:      "fresh session with no completed turn has nothing to resume",
			sessionID: "sid-1", argv: []string{"--verbose"},
			autoResume: true, hadTurns: false,
			wantSID: "", wantOK: false,
		},
		{
			// The overnight-picker regression: a relaunch that was itself
			// resuming dies before completing a turn (second rate limit in
			// a row). Its transcript exists — keep resuming it.
			name:      "consecutive swap without a completed turn keeps resuming",
			sessionID: "sid-1", argv: []string{"--resume", "sid-1", "Go continue."},
			autoResume: true, hadTurns: false,
			wantSID: "sid-1", wantOK: true,
		},
		{
			name:      "session id recovered from argv when the hook missed it",
			sessionID: "", argv: []string{"--resume", "sid-from-argv"},
			autoResume: true, hadTurns: false,
			wantSID: "sid-from-argv", wantOK: true,
		},
		{
			name:      "bare --resume with no known session cannot resume",
			sessionID: "", argv: []string{"--dangerously-skip-permissions", "--resume"},
			autoResume: true, hadTurns: false,
			wantSID: "", wantOK: false,
		},
		{
			name:      "flag after bare --resume is not mistaken for a session id",
			sessionID: "", argv: []string{"--resume", "--verbose"},
			autoResume: true, hadTurns: false,
			wantSID: "", wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sid, ok := resumableSession(c.sessionID, c.argv, c.autoResume, c.hadTurns)
			if sid != c.wantSID || ok != c.wantOK {
				t.Errorf("resumableSession(%q, %v, %v, %v) = (%q, %v), want (%q, %v)",
					c.sessionID, c.argv, c.autoResume, c.hadTurns, sid, ok, c.wantSID, c.wantOK)
			}
		})
	}
}
