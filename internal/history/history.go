// Package history is the persistent log of every account swap cux
// performs. Each Append writes one Entry to a JSON file under the cux
// runtime directory. The file is capped at MaxEntries; appending past
// the cap drops the oldest entries to keep the file size bounded.
//
// The log is for the user's benefit — `cux history` shows it — and
// for postmortems when something goes wrong. It is not consulted by
// the swap or strategy logic.
package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// MaxEntries is the cap. Older entries are dropped on append.
const MaxEntries = 1000

const fileName = "swap-history.json"

// Trigger labels every entry with what caused the swap. Keep these
// stable — they end up in the user's history output and any tooling
// they write against it.
type Trigger string

const (
	TriggerManual    Trigger = "manual"     // user typed /switch or `cux switch`
	TriggerThreshold Trigger = "threshold"  // wrapper saw cached usage cross a cap
	TriggerRateLimit Trigger = "rate-limit" // PostToolUseFailure hook fired
	TriggerRebalance Trigger = "rebalance"  // drain mode hopped back to priority
	// TriggerModelSwitch is the one entry that does not change account: a
	// model-specific cap answered by changing model on the same seat. Kept
	// in the same history so the choice is as auditable as every other swap.
	TriggerModelSwitch Trigger = "model-switch"
)

// Entry is one swap event. Usage fields are best-effort: if cached
// usage is not yet available we leave them zero, which downstream code
// treats as "no data, do not display."
type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	From      string    `json:"from"` // email of source account
	To        string    `json:"to"`   // email of target account
	Trigger   Trigger   `json:"trigger"`
	Reason    string    `json:"reason,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	CWD       string    `json:"cwd,omitempty"`

	FromUsage5h float64 `json:"fromUsage5h,omitempty"`
	FromUsage7d float64 `json:"fromUsage7d,omitempty"`
	ToUsage5h   float64 `json:"toUsage5h,omitempty"`
	ToUsage7d   float64 `json:"toUsage7d,omitempty"`
}

// Append writes one entry, dropping the oldest entries if the file
// has hit MaxEntries. Atomic — readers never see a partial file.
func Append(e Entry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.Trigger == "" {
		return errors.New("history: empty trigger")
	}

	entries, err := load()
	if err != nil {
		return err
	}
	entries = append(entries, e)
	if len(entries) > MaxEntries {
		entries = entries[len(entries)-MaxEntries:]
	}
	return save(entries)
}

// Tail returns the most recent n entries (oldest first). n <= 0
// returns every entry on disk.
func Tail(n int) ([]Entry, error) {
	entries, err := load()
	if err != nil {
		return nil, err
	}
	if n > 0 && len(entries) > n {
		return entries[len(entries)-n:], nil
	}
	return entries, nil
}

// Clear deletes every recorded entry. Idempotent — clearing an empty
// or non-existent log is fine.
func Clear() error {
	if err := os.Remove(filePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("history: remove: %w", err)
	}
	return nil
}

// --- internals -----------------------------------------------------------

func filePath() string {
	return filepath.Join(paths.RuntimeDir(), fileName)
}

func load() ([]Entry, error) {
	b, err := os.ReadFile(filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("history: read: %w", err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("history: parse: %w", err)
	}
	return entries, nil
}

func save(entries []Entry) error {
	if err := os.MkdirAll(paths.RuntimeDir(), 0o700); err != nil {
		return fmt.Errorf("history: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("history: marshal: %w", err)
	}
	return atomicfile.Write(filePath(), data, 0o600)
}
