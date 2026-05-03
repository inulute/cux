package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/inulute/cux/internal/config"
)

func TestPrintSetupConfigSummaryCanRenderWithoutANSI(t *testing.T) {
	withANSIState(t, false)
	setTheme("claude")

	out := captureStdout(t, func() {
		printSetupConfigSummary(config.Defaults())
	})

	if strings.Contains(out, "\x1b[") {
		t.Fatalf("summary contained ANSI escape bytes: %q", out)
	}
	if strings.Contains(out, "[38;2;") {
		t.Fatalf("summary contained literal truecolor bytes: %q", out)
	}
	if !strings.Contains(out, ":: C O N F I G   P R E V I E W ::") {
		t.Fatalf("summary header missing from output: %q", out)
	}
}

func TestStripANSIHandlesTruecolorSequences(t *testing.T) {
	got := stripANSI("\x1b[38;2;193;95;60mVALUE\x1b[0m")
	if got != "VALUE" {
		t.Fatalf("stripANSI() = %q, want VALUE", got)
	}
}

func TestRenderSupportIncludesURL(t *testing.T) {
	out := renderSupport(false)
	if !strings.Contains(out, donateURL) {
		t.Fatalf("support output missing URL: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain support output contained ANSI escape bytes: %q", out)
	}
}

func withANSIState(t *testing.T, enabled bool) {
	t.Helper()

	oldEnabled := ansiEnabled
	oldReset := colorReset
	oldBold := colorBold
	oldTeal := colorTeal
	oldGray := colorGray
	oldYellow := colorYellow
	oldGreen := colorGreen

	t.Cleanup(func() {
		ansiEnabled = oldEnabled
		colorReset = oldReset
		colorBold = oldBold
		colorTeal = oldTeal
		colorGray = oldGray
		colorYellow = oldYellow
		colorGreen = oldGreen
	})

	if enabled {
		ansiEnabled = true
		setTheme("default")
		return
	}
	disableANSI()
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}
