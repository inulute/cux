// Package hookinstall manages cux's entries in Claude Code's
// per-user settings file (~/.claude/settings.json).
//
// The file is *user-owned* — other tools and the user themselves
// register their own hooks there. This package therefore only ever
// touches entries it can identify as cux's own (by the literal
// substring "cux " or "/cux " in the command). Everything else is
// preserved verbatim through a round-trip via map[string]json.RawMessage.
package hookinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// CuxBinary is the literal command we install in settings.json. We use
// the bare name "cux" so it resolves through PATH; setupCheckPATH warns
// the user if cux is not findable.
const CuxBinary = "cux"

// hookSpec describes one hook ccux registers.
type hookSpec struct {
	Event   string // "Stop", "SessionStart", "PostToolUseFailure", "UserPromptSubmit"
	Subcmd  string // "stop", "session-start", "rate-limit", "prompt-submit"
	Timeout int    // seconds
}

var specs = []hookSpec{
	{Event: "UserPromptSubmit", Subcmd: "prompt-submit", Timeout: 20},
	{Event: "Stop", Subcmd: "stop", Timeout: 10},
	{Event: "SessionStart", Subcmd: "session-start", Timeout: 5},
	{Event: "PostToolUseFailure", Subcmd: "rate-limit", Timeout: 5},
}

// Install adds (or refreshes) cux's hook entries in settings.json.
// Returns the set of hook events whose entries were actually added or
// updated, so the CLI can show meaningful output.
func Install() ([]string, error) {
	settings, err := loadSettings(true)
	if err != nil {
		return nil, err
	}

	hooks := getHooksMap(settings)
	var changed []string
	for _, s := range specs {
		if upsertHook(hooks, s) {
			changed = append(changed, s.Event)
		}
	}
	settings["hooks"] = mustJSON(hooks)

	if len(changed) == 0 {
		return nil, nil
	}
	return changed, writeSettings(settings)
}

// Installed reports whether all cux hook events are present in Claude's
// settings file. It is intentionally tolerant of older cux hook shapes:
// if an event contains a cux-owned command, setup has at least been run.
func Installed() (bool, error) {
	settings, err := loadSettings(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	hooks := getHooksMap(settings)
	for _, s := range specs {
		entries := hooks[s.Event]
		found := false
		for _, e := range entries {
			if isCuxEntry(e) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// Uninstall removes only cux's hook entries. Other tools' entries and
// any non-hook keys are preserved.
func Uninstall() ([]string, error) {
	settings, err := loadSettings(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	hooks := getHooksMap(settings)
	if hooks == nil {
		return nil, nil
	}
	var removed []string
	for _, s := range specs {
		if removeHook(hooks, s.Event) {
			removed = append(removed, s.Event)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	settings["hooks"] = mustJSON(hooks)
	return removed, writeSettings(settings)
}

// VerifyOnPATH reports whether the literal CuxBinary is resolvable via
// $PATH. The hook commands installed in settings.json rely on this;
// `cux setup` calls VerifyOnPATH and surfaces a warning if it fails.
func VerifyOnPATH() (string, error) {
	resolved, err := exec.LookPath(CuxBinary)
	if err != nil {
		return "", fmt.Errorf("`%s` not found on PATH — install cux first or add ~/.local/bin to PATH", CuxBinary)
	}
	return resolved, nil
}

// --- internals ------------------------------------------------------------

// loadSettings parses ~/.claude/settings.json into a map of raw values.
// If the file is missing and createIfMissing is true, returns an empty
// map ready to be written back.
func loadSettings(createIfMissing bool) (map[string]json.RawMessage, error) {
	path := settingsPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if createIfMissing {
				return map[string]json.RawMessage{}, nil
			}
			return nil, err
		}
		return nil, fmt.Errorf("hookinstall: read %s: %w", path, err)
	}
	if len(b) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(b, &settings); err != nil {
		return nil, fmt.Errorf("hookinstall: parse %s: %w", path, err)
	}
	return settings, nil
}

func writeSettings(settings map[string]json.RawMessage) error {
	dir := filepath.Dir(settingsPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("hookinstall: mkdir %s: %w", dir, err)
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("hookinstall: marshal: %w", err)
	}
	// settings.json is mode 0644 in claude-code's own writes; we match.
	return atomicfile.Write(settingsPath(), out, 0o644)
}

// getHooksMap returns the parsed "hooks" object, decoding from raw on
// the fly. We work with map[string][]json.RawMessage so we don't
// mangle other tools' entries.
func getHooksMap(settings map[string]json.RawMessage) map[string][]json.RawMessage {
	result := map[string][]json.RawMessage{}
	raw, ok := settings["hooks"]
	if !ok || len(raw) == 0 {
		return result
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return result
	}
	for k, v := range asMap {
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			continue
		}
		result[k] = arr
	}
	return result
}

// upsertHook adds (or replaces) a cux entry under the given event key.
// Returns true if it changed anything.
func upsertHook(hooks map[string][]json.RawMessage, s hookSpec) bool {
	desired := buildHookEntry(s)
	desiredJSON, _ := json.Marshal(desired)

	existing := hooks[s.Event]
	out := existing[:0:0]
	hadCux := false
	for _, e := range existing {
		if isCuxEntry(e) {
			hadCux = true
			// Replace stale cux entry with the current desired form;
			// drop the old one (don't append it back).
			continue
		}
		out = append(out, e)
	}
	out = append(out, json.RawMessage(desiredJSON))
	hooks[s.Event] = out

	// "changed" if the cux entry was missing OR if its bytes differ
	// from the desired form. Recompute by checking against original.
	if !hadCux {
		return true
	}
	for _, e := range existing {
		if isCuxEntry(e) && string(e) == string(desiredJSON) {
			// We had the exact desired entry already; replacing was a
			// no-op — but the array order may have shifted because we
			// dropped + re-appended. Treat as no change.
			return false
		}
	}
	return true
}

// removeHook strips every cux entry from one event, deleting the event
// key entirely if no entries remain.
func removeHook(hooks map[string][]json.RawMessage, event string) bool {
	existing, ok := hooks[event]
	if !ok {
		return false
	}
	out := existing[:0:0]
	for _, e := range existing {
		if isCuxEntry(e) {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = out
	}
	return len(out) != len(existing)
}

func buildHookEntry(s hookSpec) map[string]interface{} {
	return map[string]interface{}{
		"matcher": ".*",
		"hooks": []map[string]interface{}{
			{
				"type":    "command",
				"command": fmt.Sprintf("%s hook %s", CuxBinary, s.Subcmd),
				"timeout": s.Timeout,
			},
		},
	}
}

// isCuxEntry returns true if the JSON-encoded entry contains "cux "
// (with trailing space) or "/cux " — the two literal forms the
// command field can take.
func isCuxEntry(e json.RawMessage) bool {
	s := string(e)
	return strings.Contains(s, `"`+CuxBinary+` `) || strings.Contains(s, `/`+CuxBinary+` `)
}

func mustJSON(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func settingsPath() string {
	return filepath.Join(paths.ClaudeDir(), "settings.json")
}
