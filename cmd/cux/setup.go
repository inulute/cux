package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/inulute/cux/internal/atomicfile"
	"github.com/inulute/cux/internal/paths"
)

// switchSlashCommand is the markdown body installed at
// ~/.claude/commands/switch.md. Embedding keeps the binary self-contained
// so `cux setup` works on a freshly downloaded binary with no source tree.
//
//go:embed slashcmd_switch.md
var switchSlashCommand []byte

//go:embed slashcmd_cux_add.md
var cuxAddSlashCommand []byte

//go:embed slashcmd_cux_list.md
var cuxListSlashCommand []byte

//go:embed slashcmd_cux_status.md
var cuxStatusSlashCommand []byte

//go:embed slashcmd_cux_support.md
var cuxSupportSlashCommand []byte

//go:embed slashcmd_cux_switch.md
var cuxSwitchSlashCommand []byte

//go:embed slashcmd_cux_config.md
var cuxConfigSlashCommand []byte

//go:embed slashcmd_cux_remove.md
var cuxRemoveSlashCommand []byte

//go:embed slashcmd_cux_usage_refresh.md
var cuxUsageRefreshSlashCommand []byte

func installSlashCommand() error {
	dir := filepath.Join(paths.ClaudeDir(), "commands")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir %s: %w", dir, err)
	}
	if err := atomicfile.Write(filepath.Join(dir, "switch.md"), switchSlashCommand, 0o600); err != nil {
		return err
	}

	cuxDir := filepath.Join(dir, "cux")
	if err := os.MkdirAll(cuxDir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir %s: %w", cuxDir, err)
	}
	commands := map[string][]byte{
		"add.md":           cuxAddSlashCommand,
		"config.md":        cuxConfigSlashCommand,
		"list.md":          cuxListSlashCommand,
		"status.md":        cuxStatusSlashCommand,
		"support.md":       cuxSupportSlashCommand,
		"switch.md":        cuxSwitchSlashCommand,
		"remove.md":        cuxRemoveSlashCommand,
		"usage-refresh.md": cuxUsageRefreshSlashCommand,
	}
	for name, body := range commands {
		if err := atomicfile.Write(filepath.Join(cuxDir, name), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}
