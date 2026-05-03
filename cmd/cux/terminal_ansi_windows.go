//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func enableANSIOutput() bool {
	handle := windows.Handle(os.Stdout.Fd())

	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}

	mode |= windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	return windows.SetConsoleMode(handle, mode) == nil
}

// enableUnicodeOutput switches the console to UTF-8 (code page 65001) so that
// box-drawing and other non-ASCII characters render correctly. It is a
// best-effort call; failure is silently ignored.
func enableUnicodeOutput() { _ = windows.SetConsoleOutputCP(65001) }
