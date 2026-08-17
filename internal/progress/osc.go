// OSC (Operating System Command) integrations — the scan experience's
// "invisible superpowers" (spec features G1–G3, G5). Every emitter here is
// gated: sequences go only to real terminals outside CI, and the sequences
// with fragmented or conflicting support (taskbar progress, notifications)
// additionally require positive detection of a terminal known to implement
// them. OSC 9 in particular is a namespace collision — ConEmu/Windows
// Terminal use "9;4" for progress while iTerm2 treats OSC 9 as a desktop
// notification — so nothing here is ever blind-emitted.

package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// oscAllowed reports whether OSC sequences may be written to w at all.
func oscAllowed(w io.Writer) bool {
	return isTerminalWriter(w) && !IsCI()
}

// SetTitle sets the terminal tab/window title (G2): "armis · 02:47".
func SetTitle(w io.Writer, title string) {
	if !oscAllowed(w) {
		return
	}
	// armis:ignore cwe:253 reason:terminal control write; return value not actionable
	_, _ = fmt.Fprintf(w, "\033]0;%s\a", sanitizeOSC(title))
}

// ResetTitle clears the title set by SetTitle. Terminals cannot be queried
// for the previous title, so best effort is an empty title.
func ResetTitle(w io.Writer) {
	SetTitle(w, "")
}

// Hyperlink wraps text in an OSC 8 hyperlink (G1) when w is a terminal and
// url is non-empty; otherwise it returns text unchanged. Terminals without
// OSC 8 support render the plain text (tmux may strip the link itself).
func Hyperlink(w io.Writer, text, url string) string {
	if url == "" || !oscAllowed(w) {
		return text
	}
	return "\033]8;;" + sanitizeOSC(url) + "\033\\" + text + "\033]8;;\033\\"
}

// taskbarCapable reports positive detection of a terminal implementing the
// ConEmu OSC 9;4 progress protocol. Detection is deliberately allowlist-only.
func taskbarCapable() bool {
	return os.Getenv("WT_SESSION") != "" || os.Getenv("ConEmuANSI") != ""
}

// TaskbarProgress sets determinate taskbar progress 0–100 (G3).
func TaskbarProgress(w io.Writer, pct int) {
	if !oscAllowed(w) || !taskbarCapable() {
		return
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	// armis:ignore cwe:253 reason:terminal control write; return value not actionable
	_, _ = fmt.Fprintf(w, "\033]9;4;1;%d\a", pct)
}

// TaskbarBusy sets indeterminate taskbar progress (G3), for the opaque
// analysis wait.
func TaskbarBusy(w io.Writer) {
	if !oscAllowed(w) || !taskbarCapable() {
		return
	}
	// armis:ignore cwe:253 reason:terminal control write; return value not actionable
	_, _ = fmt.Fprint(w, "\033]9;4;3;0\a")
}

// TaskbarClear removes the taskbar progress state (G3).
func TaskbarClear(w io.Writer) {
	if !oscAllowed(w) || !taskbarCapable() {
		return
	}
	// armis:ignore cwe:253 reason:terminal control write; return value not actionable
	_, _ = fmt.Fprint(w, "\033]9;4;0;0\a")
}

// Notify emits a desktop notification (G5) using the detected terminal's
// dialect. Notification protocols are per-terminal (iTerm2: OSC 9, kitty:
// OSC 99, urxvt/foot: OSC 777); undetected terminals get nothing rather
// than a blind guess.
func Notify(w io.Writer, title, body string) {
	if !oscAllowed(w) {
		return
	}
	title = sanitizeOSC(title)
	body = sanitizeOSC(body)
	term := os.Getenv("TERM")
	switch {
	case os.Getenv("TERM_PROGRAM") == "iTerm.app":
		// armis:ignore cwe:253 reason:terminal control write; return value not actionable
		_, _ = fmt.Fprintf(w, "\033]9;%s: %s\a", title, body)
	case strings.Contains(term, "kitty"):
		// armis:ignore cwe:253 reason:terminal control write; return value not actionable
		_, _ = fmt.Fprintf(w, "\033]99;;%s: %s\033\\", title, body)
	case strings.Contains(term, "rxvt"), strings.Contains(term, "foot"):
		// armis:ignore cwe:253 reason:terminal control write; return value not actionable
		_, _ = fmt.Fprintf(w, "\033]777;notify;%s;%s\a", title, body)
	}
}

// ConsoleURL builds the Armis Cloud deep link for a scan, gated behind the
// ARMIS_CONSOLE_URL environment variable until the platform team confirms
// the canonical pattern (spec §6.2). A "{scan_id}" placeholder in the value
// is substituted; otherwise the ID is appended as a path segment.
func ConsoleURL(scanID string) string {
	base := strings.TrimSpace(os.Getenv("ARMIS_CONSOLE_URL"))
	if base == "" || scanID == "" {
		return ""
	}
	if strings.Contains(base, "{scan_id}") {
		return strings.ReplaceAll(base, "{scan_id}", scanID)
	}
	return strings.TrimRight(base, "/") + "/" + scanID
}

// sanitizeOSC strips characters that would terminate or corrupt an OSC
// string (ESC, BEL, control characters).
func sanitizeOSC(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
