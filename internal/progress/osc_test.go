package progress

import (
	"bytes"
	"strings"
	"testing"
)

// All OSC emitters must stay silent on non-TTY writers regardless of env —
// the primary safety property (escape bytes in logs are the failure mode).
func TestOSCSilentOnNonTTY(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("WT_SESSION", "some-session")
	t.Setenv("TERM_PROGRAM", "iTerm.app")

	var buf bytes.Buffer
	SetTitle(&buf, "armis · 00:10")
	ResetTitle(&buf)
	TaskbarProgress(&buf, 50)
	TaskbarBusy(&buf)
	TaskbarClear(&buf)
	Notify(&buf, "Armis", "scan complete")
	if buf.Len() != 0 {
		t.Errorf("OSC output leaked to non-TTY writer: %q", buf.String())
	}
}

func TestHyperlinkFallsBackToPlainText(t *testing.T) {
	clearCIEnv(t)
	var buf bytes.Buffer
	if got := Hyperlink(&buf, "scn_123", "https://example.com/scn_123"); got != "scn_123" {
		t.Errorf("non-TTY hyperlink should be plain text, got %q", got)
	}
	if got := Hyperlink(&buf, "scn_123", ""); got != "scn_123" {
		t.Errorf("empty URL should return plain text, got %q", got)
	}
}

func TestConsoleURL(t *testing.T) {
	t.Run("unset env yields empty", func(t *testing.T) {
		t.Setenv("ARMIS_CONSOLE_URL", "")
		if got := ConsoleURL("scn_1"); got != "" {
			t.Errorf("ConsoleURL with unset env = %q, want empty", got)
		}
	})
	t.Run("placeholder substitution", func(t *testing.T) {
		t.Setenv("ARMIS_CONSOLE_URL", "https://cloud.armis.com/scans/{scan_id}/report")
		if got := ConsoleURL("scn_1"); got != "https://cloud.armis.com/scans/scn_1/report" {
			t.Errorf("placeholder substitution failed: %q", got)
		}
	})
	t.Run("appended path segment", func(t *testing.T) {
		t.Setenv("ARMIS_CONSOLE_URL", "https://cloud.armis.com/scans/")
		if got := ConsoleURL("scn_1"); got != "https://cloud.armis.com/scans/scn_1" {
			t.Errorf("append form failed: %q", got)
		}
	})
	t.Run("empty scan id yields empty", func(t *testing.T) {
		t.Setenv("ARMIS_CONSOLE_URL", "https://cloud.armis.com")
		if got := ConsoleURL(""); got != "" {
			t.Errorf("empty scan id should yield empty URL, got %q", got)
		}
	})
}

func TestSanitizeOSC(t *testing.T) {
	in := "title\x1b]0;evil\x07more\nlines"
	got := sanitizeOSC(in)
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("control character %q survived sanitization: %q", r, got)
		}
	}
	if !strings.Contains(got, "title") || !strings.Contains(got, "more") {
		t.Errorf("sanitizeOSC dropped legitimate text: %q", got)
	}
}

func TestTaskbarCapableDetection(t *testing.T) {
	t.Setenv("WT_SESSION", "")
	t.Setenv("ConEmuANSI", "")
	if taskbarCapable() {
		t.Error("taskbarCapable must be false without positive detection")
	}
	t.Setenv("WT_SESSION", "abc")
	if !taskbarCapable() {
		t.Error("taskbarCapable should detect Windows Terminal via WT_SESSION")
	}
}
