package wrapper

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/inulute/cux/internal/paths"
)

// sessionHasUserMessage reports whether the transcript of sessionID holds at
// least one line a person actually sent.
//
// An idle migration relaunches claude, and a window nobody has typed into has
// nothing to carry over: resumableSession cannot resume it, so every such
// migration opened a fresh, empty session beside the one it closed. On the
// 2026-09 history that was 7 of the 12 idle swaps. A session with no user
// message is not worth moving, so the idle path leaves it where it is; the
// first prompt still reaches the UserPromptSubmit and turn-end paths, which
// migrate it when it matters.
//
// A missing transcript counts as "no message": Claude Code writes the file
// when the first message is sent, and an empty id means SessionStart has not
// reported yet. Lines Claude Code records as user-role but marks isMeta
// (caveats, injected context) are not something the person typed.
func sessionHasUserMessage(cwd, sessionID string) bool {
	if cwd == "" || sessionID == "" {
		return false
	}
	for _, path := range transcriptCandidates(cwd, sessionID) {
		if transcriptHasUserMessage(path) {
			return true
		}
	}
	return false
}

// transcriptCandidates lists where the transcript may live. paths.ProjectTranscriptDir
// only swaps '/' for '-', but Claude Code folds every non-alphanumeric byte
// (so ~/.codex/x is -Users-me--codex-x). Looking in both keeps a working
// directory with a dot, underscore or space from reading as "never typed into",
// which would switch idle migration off for it altogether.
func transcriptCandidates(cwd, sessionID string) []string {
	first := transcriptPath(cwd, sessionID)
	encoded := make([]byte, 0, len(cwd))
	for i := 0; i < len(cwd); i++ {
		c := cwd[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			encoded = append(encoded, c)
		} else {
			encoded = append(encoded, '-')
		}
	}
	second := filepath.Join(paths.ClaudeProjectsDir(), string(encoded), sessionID+".jsonl")
	if second == first {
		return []string{first}
	}
	return []string{first, second}
}

func transcriptHasUserMessage(path string) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	r := bufio.NewReaderSize(fh, 256*1024)
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 {
			var line struct {
				Type   string `json:"type"`
				IsMeta bool   `json:"isMeta"`
			}
			if json.Unmarshal(raw, &line) == nil && line.Type == "user" && !line.IsMeta {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
}

// idleSessionHasUserMessage is the step-loop form of sessionHasUserMessage:
// the session id is guarded by mu and the transcript lives under the
// wrapper's working directory.
func idleSessionHasUserMessage(mu *sync.Mutex, sessionID *string) bool {
	mu.Lock()
	sid := *sessionID
	mu.Unlock()
	cwd, _ := os.Getwd()
	return sessionHasUserMessage(cwd, sid)
}
