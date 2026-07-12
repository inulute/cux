package transcripts

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/inulute/cux/internal/paths"
)

func writeSession(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assistantLine(ts string, in, out, cw, cr int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		ts, in, out, cw, cr)
}

func TestForDirAggregatesProjectSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := "/work/alpha"
	root := paths.ClaudeProjectsDir()
	enc := filepath.Base(paths.ProjectTranscriptDir(projectDir))

	// Session at the project root: two assistant turns, 30 minutes apart.
	writeSession(t, filepath.Join(root, enc), "s1.jsonl",
		`{"type":"user","timestamp":"2026-07-10T10:00:00Z"}`,
		assistantLine("2026-07-10T10:10:00Z", 1000, 200, 50, 5000),
		"not-json garbage line",
		assistantLine("2026-07-10T10:30:00Z", 2000, 300, 0, 7000),
	)
	// Session started in a SUBdirectory of the project (encoded suffix).
	writeSession(t, filepath.Join(root, enc+"-src"), "s2.jsonl",
		assistantLine("2026-07-11T09:00:00Z", 500, 100, 10, 100),
	)
	// Sibling directory that shares the prefix but not the boundary —
	// /work/alphabet must NOT count toward /work/alpha.
	writeSession(t, filepath.Join(root, filepath.Base(paths.ProjectTranscriptDir("/work/alphabet"))), "s3.jsonl",
		assistantLine("2026-07-11T09:00:00Z", 999999, 999999, 0, 0),
	)

	st, err := ForDir(projectDir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 2 || st.Turns != 3 {
		t.Errorf("sessions=%d turns=%d, want 2 and 3", st.Sessions, st.Turns)
	}
	if st.InputTokens != 3500 || st.OutputTokens != 600 {
		t.Errorf("in=%d out=%d, want 3500 and 600", st.InputTokens, st.OutputTokens)
	}
	if st.CacheCreationTokens != 60 || st.CacheReadTokens != 12100 {
		t.Errorf("cache write=%d read=%d, want 60 and 12100", st.CacheCreationTokens, st.CacheReadTokens)
	}
	if st.ActiveTime != 30*time.Minute {
		t.Errorf("active=%v, want 30m (s1 spans 30m, s2 spans 0)", st.ActiveTime)
	}
}

func TestForDirSinceWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := "/work/alpha"
	enc := filepath.Base(paths.ProjectTranscriptDir(projectDir))
	dir := filepath.Join(paths.ClaudeProjectsDir(), enc)

	writeSession(t, dir, "old.jsonl",
		assistantLine("2020-01-01T00:00:00Z", 1000, 1000, 0, 0),
		assistantLine(time.Now().UTC().Format(time.RFC3339), 10, 20, 0, 0),
	)

	st, err := ForDir(projectDir, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The 2020 line falls outside the window; the recent one counts.
	if st.Turns != 1 || st.InputTokens != 10 || st.OutputTokens != 20 {
		t.Errorf("turns=%d in=%d out=%d, want 1/10/20 (old line excluded)", st.Turns, st.InputTokens, st.OutputTokens)
	}
}

func TestForDirNoTranscriptsIsEmptyNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := ForDir("/nowhere", time.Time{})
	if err != nil || st.Sessions != 0 {
		t.Errorf("got (%+v, %v), want empty stats and nil error", st, err)
	}
}
