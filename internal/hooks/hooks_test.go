package hooks

import (
	"strings"
	"testing"
)

func TestRenderPromptSupportIncludesURL(t *testing.T) {
	out := renderPromptSupport()
	if !strings.Contains(out, "https://support.inulute.com") {
		t.Fatalf("support output missing URL: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("prompt support output contained ANSI escape bytes: %q", out)
	}
}
