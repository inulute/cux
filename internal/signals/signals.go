// Package signals is the file-based message bus between Claude Code's
// hooks (writers) and the cux wrapper (reader).
//
// Each signal lives at:
//
//	$BACKUP_ROOT/runtime/signals/{wrapperPID}-{name}
//
// Hooks write a signal as a JSON document (atomically, mode 0600).
// The wrapper polls the directory every ~250 ms; the *presence* of a
// file is the event, not its mtime. The wrapper deletes the file when
// it consumes the event, so a second occurrence within the same turn
// is recorded as another file write — never coalesced.
//
// The PID-namespaced filename means two wrappers running in different
// terminals never observe each other's signals.
package signals

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// Name identifies the kind of signal. Keep these as exported constants
// rather than free strings so hooks and the wrapper share the same set.
type Name string

const (
	SessionStarted  Name = "session-started"
	Stopped         Name = "stopped"
	RateLimited     Name = "rate-limited"
	SwitchRequested Name = "switch-requested"
	TurnFailed      Name = "turn-failed"
)

// Payloads. Match claude-revolver's shapes for hook-emitted signals so
// behavior under real Claude Code stays familiar.

type SessionStartedPayload struct {
	SessionID string    `json:"sessionId"`
	CWD       string    `json:"cwd,omitempty"`
	Source    string    `json:"source,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type StoppedPayload struct {
	SessionID string    `json:"sessionId,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type RateLimitedPayload struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message,omitempty"`
}

// SwitchRequestedPayload carries an optional explicit target. An empty
// Target means "rotate to the next account per the configured strategy."
type SwitchRequestedPayload struct {
	Target        string    `json:"target,omitempty"`
	ResumeMessage string    `json:"resumeMessage,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

// TurnFailedPayload marks a turn that died on a non-rate-limit API
// error after Claude Code exhausted its own retries.
type TurnFailedPayload struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message,omitempty"`
}

// Dir returns the absolute signal directory. Callers that intend to
// write must MkdirAll this themselves; readers can tolerate absence.
func Dir() string {
	return filepath.Join(paths.RuntimeDir(), "signals")
}

// Path is the absolute path of one signal for a given wrapper PID.
func Path(wrapperPID int, name Name) string {
	return filepath.Join(Dir(), fmt.Sprintf("%d-%s", wrapperPID, name))
}

// Write serialises payload and writes it atomically at mode 0600.
// payload may be nil — the file is then a zero-byte marker.
func Write(wrapperPID int, name Name, payload interface{}) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return fmt.Errorf("signals: mkdir: %w", err)
	}
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("signals: marshal %s: %w", name, err)
		}
	}
	return atomicfile.Write(Path(wrapperPID, name), data, 0o600)
}

// Read returns true if the signal exists, with its payload bytes (which
// may be empty for marker-only signals). Callers should call Consume
// after acting on it.
func Read(wrapperPID int, name Name) (bytes []byte, present bool, err error) {
	b, err := os.ReadFile(Path(wrapperPID, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("signals: read %s: %w", name, err)
	}
	return b, true, nil
}

// Consume removes the signal file. Idempotent.
func Consume(wrapperPID int, name Name) error {
	if err := os.Remove(Path(wrapperPID, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("signals: remove %s: %w", name, err)
	}
	return nil
}

// CleanupForPID removes every signal belonging to a wrapper PID. Used
// at wrapper startup (paranoid leftovers from a crashed prior run) and
// at clean shutdown.
func CleanupForPID(wrapperPID int) {
	prefix := strconv.Itoa(wrapperPID) + "-"
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if hasPrefix(e.Name(), prefix) {
			_ = os.Remove(filepath.Join(Dir(), e.Name()))
		}
	}
}

func hasPrefix(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return s[:len(p)] == p
}

// DecodeSessionStarted is a convenience for the wrapper.
func DecodeSessionStarted(b []byte) (SessionStartedPayload, error) {
	var p SessionStartedPayload
	if len(b) == 0 {
		return p, nil
	}
	err := json.Unmarshal(b, &p)
	return p, err
}

// DecodeStopped is a convenience for the wrapper.
func DecodeStopped(b []byte) (StoppedPayload, error) {
	var p StoppedPayload
	if len(b) == 0 {
		return p, nil
	}
	err := json.Unmarshal(b, &p)
	return p, err
}

// DecodeSwitchRequested is a convenience for the wrapper.
func DecodeSwitchRequested(b []byte) (SwitchRequestedPayload, error) {
	var p SwitchRequestedPayload
	if len(b) == 0 {
		return p, nil
	}
	err := json.Unmarshal(b, &p)
	return p, err
}

// DecodeRateLimited is a convenience for the wrapper.
func DecodeRateLimited(b []byte) (RateLimitedPayload, error) {
	var p RateLimitedPayload
	if len(b) == 0 {
		return p, nil
	}
	err := json.Unmarshal(b, &p)
	return p, err
}

// DecodeTurnFailed is a convenience for the wrapper.
func DecodeTurnFailed(b []byte) (TurnFailedPayload, error) {
	var p TurnFailedPayload
	if len(b) == 0 {
		return p, nil
	}
	err := json.Unmarshal(b, &p)
	return p, err
}
