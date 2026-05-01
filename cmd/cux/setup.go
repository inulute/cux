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

func installSlashCommand() error {
	dir := filepath.Join(paths.ClaudeDir(), "commands")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir %s: %w", dir, err)
	}
	dest := filepath.Join(dir, "switch.md")
	return atomicfile.Write(dest, switchSlashCommand, 0o600)
}
