// Package transcripts derives per-project usage statistics from the
// session transcripts Claude Code already writes under
// ~/.claude/projects. cux collects nothing itself: token counts and
// timestamps come straight from the JSONL lines Claude Code persists
// for --resume, so the numbers exist for every past session too.
package transcripts

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inulute/cux/internal/paths"
)

// Stats aggregates one project's transcript activity.
type Stats struct {
	Sessions            int
	Turns               int // assistant messages that carried usage
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	ActiveTime          time.Duration // per session: last minus first timestamp, summed
	FirstAt             time.Time
	LastAt              time.Time
}

// transcriptLine is the narrow slice of a transcript entry we read.
// Everything else in the line is ignored on purpose — the format
// belongs to Claude Code and grows fields freely.
type transcriptLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ForDir scans every transcript belonging to projectDir — sessions
// started in the directory itself and in any subdirectory — and sums
// their usage. since limits the window; pass the zero time for
// everything.
//
// Claude Code encodes a session's working directory into the
// transcript folder name by replacing path separators with '-'. That
// encoding is lossy (a literal '-' is indistinguishable from a
// separator), so subdirectory matching is a boundary-aware prefix
// heuristic on the encoded name: exact match, or prefix followed by
// '-'. A sibling like /work/alphabet never matches /work/alpha, but a
// directory literally named "/work/alpha-bet" would — acceptable for
// statistics, and the same trade-off Claude Code itself makes.
func ForDir(projectDir string, since time.Time) (Stats, error) {
	var st Stats
	root := paths.ClaudeProjectsDir()
	prefix := filepath.Base(paths.ProjectTranscriptDir(projectDir))

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil // no transcripts yet — empty stats, not an error
		}
		return st, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != prefix && !strings.HasPrefix(name, prefix+"-") {
			continue
		}
		files, err := filepath.Glob(filepath.Join(root, name, "*.jsonl"))
		if err != nil {
			continue
		}
		for _, f := range files {
			scanSession(f, since, &st)
		}
	}
	return st, nil
}

// scanSession folds one transcript file into st. Lines that fail to
// parse are skipped: the file is another program's live data.
func scanSession(path string, since time.Time, st *Stats) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()

	if !since.IsZero() {
		if info, err := fh.Stat(); err == nil && info.ModTime().Before(since) {
			return // whole session predates the window
		}
	}

	var first, last time.Time
	r := bufio.NewReaderSize(fh, 256*1024)
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 {
			var line transcriptLine
			if json.Unmarshal(raw, &line) == nil && !line.Timestamp.IsZero() {
				if since.IsZero() || !line.Timestamp.Before(since) {
					if first.IsZero() || line.Timestamp.Before(first) {
						first = line.Timestamp
					}
					if line.Timestamp.After(last) {
						last = line.Timestamp
					}
					if line.Type == "assistant" {
						u := line.Message.Usage
						if u.InputTokens+u.OutputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens > 0 {
							st.Turns++
							st.InputTokens += u.InputTokens
							st.OutputTokens += u.OutputTokens
							st.CacheCreationTokens += u.CacheCreationInputTokens
							st.CacheReadTokens += u.CacheReadInputTokens
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			break
		}
	}
	if first.IsZero() {
		return // nothing inside the window
	}
	st.Sessions++
	st.ActiveTime += last.Sub(first)
	if st.FirstAt.IsZero() || first.Before(st.FirstAt) {
		st.FirstAt = first
	}
	if last.After(st.LastAt) {
		st.LastAt = last
	}
}
