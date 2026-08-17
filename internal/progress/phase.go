// Phase implements the scan experience's live region: one status line
// (spinner head, bold message, optional detail counter, breathing ellipsis,
// elapsed timer) with up to N stream lines beneath it, redrawn in place on a
// TTY and degrading to append-only printed lines everywhere else.
//
// Lifecycle mirrors Spinner: a render goroutine with a stop channel, context
// cancellation, an idempotent finish, and a safety timeout. On Succeed the
// live region is erased and replaced by two permanent transcript lines: the
// muted status ("<message>...") and the ✓ receipt — the append-trail design
// (spec features C1/C3/B5/B9/D3).

package progress

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ArmisSecurity/armis-cli/internal/output"
)

// Phase renders one phase of the scan transcript.
type Phase struct {
	mu      sync.Mutex
	message string
	detail  string
	verbs   []string
	stream  []string // ring of recent stream lines, newest last

	writer      io.Writer
	disabled    bool
	streamDepth int
	titleClock  time.Time // zero = no tab-title updates
	start       time.Time
	verbStart   time.Time

	stopChan chan struct{}
	doneChan chan struct{}
	stopOnce sync.Once
	cancel   context.CancelFunc
	finished bool
	animated bool // true when the render goroutine was started

	prevLines int // lines drawn by the previous frame (render goroutine only)
}

// verbRotatePeriod is how long each narrative verb holds before rotating.
const verbRotatePeriod = 4 * time.Second

// titleUpdatePeriod is how often the terminal tab title is refreshed.
const titleUpdatePeriod = time.Second

// PhaseOption configures a Phase.
type PhaseOption func(*Phase)

// WithStreamDepth enables the stream sub-region with up to n lines (C3/C4).
func WithStreamDepth(n int) PhaseOption {
	return func(p *Phase) { p.streamDepth = n }
}

// WithPhaseWriter sets the output writer (default os.Stderr; for tests).
func WithPhaseWriter(w io.Writer) PhaseOption {
	return func(p *Phase) { p.writer = w }
}

// WithTitleClock enables tab-title updates (G2) showing elapsed time since
// the given start (typically the whole scan's start, not this phase's).
func WithTitleClock(start time.Time) PhaseOption {
	return func(p *Phase) { p.titleClock = start }
}

// StartPhase begins a phase. On a TTY it starts the live renderer; in CI,
// non-TTY, or when disabled it prints the status line once and returns a
// Phase whose mutating methods are no-ops until Succeed/Fail.
func StartPhase(ctx context.Context, message string, disabled bool, opts ...PhaseOption) *Phase {
	p := &Phase{
		message:  message,
		writer:   os.Stderr,
		disabled: disabled,
		start:    time.Now(),
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}

	if p.disabled || IsCI() || !isTerminalWriter(p.writer) {
		// Append-only degradation: the status prints once; Succeed prints the
		// receipt. This matches CI log expectations exactly.
		// armis:ignore cwe:253 reason:fmt.Fprintf to stderr for log-style output; return value not actionable
		_, _ = fmt.Fprintf(p.writer, "%s...\n", message)
		close(p.doneChan)
		return p
	}

	var rctx context.Context
	rctx, p.cancel = context.WithTimeout(ctx, DefaultSpinnerTimeout)
	p.animated = true

	// armis:ignore cwe:401 reason:goroutine has proper lifecycle via doneChan + cancel; stopped by finish()
	go p.run(rctx)
	return p
}

// run is the render loop. It owns cursor state and the live region.
func (p *Phase) run(ctx context.Context) {
	defer close(p.doneChan)
	defer p.cancel()

	// armis:ignore cwe:253 reason:cursor control to terminal; return value not actionable
	_, _ = fmt.Fprint(p.writer, cursorHide)
	// armis:ignore cwe:253 reason:deferred cursor restore; return value not actionable
	defer func() { _, _ = fmt.Fprint(p.writer, cursorShow) }()

	ticker := time.NewTicker(spinnerFrameDelay)
	defer ticker.Stop()
	lastTitle := time.Time{}

	for tick := 0; ; tick++ {
		select {
		case <-p.stopChan:
			p.clearRegion()
			return
		case <-ctx.Done():
			p.clearRegion()
			return
		case <-ticker.C:
			// armis:ignore cwe:253 reason:frame write to terminal; return value not actionable
			_, _ = fmt.Fprint(p.writer, p.renderFrame(tick))
			if !p.titleClock.IsZero() && time.Since(lastTitle) >= titleUpdatePeriod {
				lastTitle = time.Now()
				SetTitle(p.writer, "armis · "+formatDuration(time.Since(p.titleClock)))
			}
		}
	}
}

// renderFrame builds one full redraw of the live region. The cursor is at
// the start of the status line before and after every frame.
func (p *Phase) renderFrame(tick int) string {
	p.mu.Lock()
	msg := p.currentMessageLocked()
	detail := p.detail
	stream := make([]string, len(p.stream))
	copy(stream, p.stream)
	p.mu.Unlock()

	styles := output.GetStyles()
	elapsed := time.Since(p.start)
	timerStr := "[" + formatDuration(elapsed) + "]"

	// Width budget: spinner(1) + space + base + dots(3) + " — " + detail +
	// space + timer + margin. Truncation is correctness, not cosmetics —
	// a soft-wrapped live line breaks the cursor-up arithmetic.
	cols := terminalCols(p.writer)
	if cols > 0 {
		budget := cols - 1 - 1 - 3 - 1 - len(timerStr) - 1
		if detail != "" {
			budget -= len(detail) + 3
		}
		if budget < 8 { // desperate terminals: sacrifice the detail
			detail = ""
			budget = cols - 1 - 1 - 3 - 1 - len(timerStr) - 1
		}
		msg = truncateMessage(msg, budget)
	}

	var status strings.Builder
	status.WriteString(styles.SpinnerChar.Render(spinnerFrames[tick%len(spinnerFrames)]))
	status.WriteByte(' ')
	status.WriteString(styles.Bold.Render(msg))
	if detail != "" {
		status.WriteString(styles.FooterSeparator.Render(" — "))
		status.WriteString(styles.SpinnerText.Render(detail))
	}
	status.WriteString(renderEllipsis(styles, tick))
	status.WriteByte(' ')
	status.WriteString(styles.SpinnerTimer.Render(timerStr))

	lines := []string{status.String()}
	for i, s := range stream {
		s = truncateMessage(s, maxStreamRunes(cols))
		if i == len(stream)-1 {
			lines = append(lines, "  "+styles.SuccessText.Render("✓ ")+styles.MutedText.Render(s))
		} else {
			lines = append(lines, "  "+styles.FooterSeparator.Render("✓ "+s))
		}
	}

	var b strings.Builder
	b.WriteString("\r")
	for i, ln := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("\033[2K")
		b.WriteString(ln)
	}
	// Erase leftovers if the region shrank, then park the cursor back at the
	// start of the status line so the next frame (or clearRegion) can redraw.
	extra := p.prevLines - len(lines)
	for i := 0; i < extra; i++ {
		b.WriteString("\n\033[2K")
	}
	if extra < 0 {
		extra = 0
	}
	if up := len(lines) - 1 + extra; up > 0 {
		fmt.Fprintf(&b, "\033[%dA", up)
	}
	b.WriteString("\r")
	p.prevLines = len(lines)
	return b.String()
}

// clearRegion erases the live region. Cursor ends at the region's first
// column, ready for permanent transcript lines. Render-goroutine only.
func (p *Phase) clearRegion() {
	// Cursor sits at the start of the status line; ED(0) clears everything
	// from there to the end of the screen — the whole region in one go.
	// armis:ignore cwe:253 reason:terminal control write; return value not actionable
	_, _ = fmt.Fprint(p.writer, "\r\033[J")
	p.prevLines = 0
}

// currentMessageLocked resolves the live message: rotating narrative verbs
// when set (B9), otherwise the last SetMessage value. Caller holds p.mu.
func (p *Phase) currentMessageLocked() string {
	if len(p.verbs) == 0 {
		return p.message
	}
	idx := int(time.Since(p.verbStart)/verbRotatePeriod) % len(p.verbs)
	return p.verbs[idx]
}

// SetMessage replaces the live message (and stops verb rotation).
func (p *Phase) SetMessage(msg string) {
	p.mu.Lock()
	p.message = msg
	p.verbs = nil
	p.mu.Unlock()
}

// SetVerbs starts rotating the live message through the given phrases (B9).
// The list is cosmetic narration for an opaque backend wait — callers must
// only use phrases that describe work the backend genuinely performs.
func (p *Phase) SetVerbs(verbs []string) {
	p.mu.Lock()
	if len(verbs) > 0 {
		p.verbs = verbs
		p.verbStart = time.Now()
	}
	p.mu.Unlock()
}

// SetDetail sets the counter segment of the status line (C1), e.g.
// "1,204 files · 42.3 MB".
func (p *Phase) SetDetail(detail string) {
	p.mu.Lock()
	p.detail = detail
	p.mu.Unlock()
}

// StreamLine pushes a line into the stream sub-region (C3/C4). No-op when
// the phase has no stream depth or is not animating.
func (p *Phase) StreamLine(line string) {
	if p.streamDepth <= 0 {
		return
	}
	p.mu.Lock()
	p.stream = append(p.stream, line)
	if len(p.stream) > p.streamDepth {
		p.stream = p.stream[len(p.stream)-p.streamDepth:]
	}
	p.mu.Unlock()
}

// Elapsed returns time since the phase started.
func (p *Phase) Elapsed() time.Duration {
	return time.Since(p.start)
}

// Succeed finishes the phase: the live region is replaced by the permanent
// muted status line and the ✓ receipt (D3 append trail). In append-only mode
// (CI/non-TTY/disabled) the status already printed at StartPhase, so only
// the receipt is emitted.
func (p *Phase) Succeed(label, extra string) {
	p.stopOnce.Do(func() {
		p.teardown()
		styles := output.GetStyles()
		if p.animated {
			p.mu.Lock()
			msg := p.message
			p.mu.Unlock()
			// armis:ignore cwe:253 reason:fmt.Fprintf to stderr for transcript output; return value not actionable
			_, _ = fmt.Fprintf(p.writer, "%s\n", styles.MutedText.Render(msg+"..."))
		}
		// armis:ignore cwe:253 reason:fmt.Fprintf to stderr for transcript output; return value not actionable
		_, _ = fmt.Fprintf(p.writer, "%s%s%s\n",
			styles.SuccessText.Render("✓ "),
			styles.SpinnerText.Render(label),
			styles.MutedText.Render("  ("+extra+")"))
	})
}

// Fail finishes the phase without a receipt; the caller reports the error.
// Safe to defer: a no-op after Succeed.
func (p *Phase) Fail() {
	p.stopOnce.Do(p.teardown)
}

// teardown stops the render goroutine (if any) and waits for it to release
// the live region and restore the cursor.
func (p *Phase) teardown() {
	p.finished = true
	if !p.animated {
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	close(p.stopChan)
	select {
	case <-p.doneChan:
	case <-time.After(5 * time.Second):
	}
}

// maxStreamRunes is the width budget for a stream line: two-space indent,
// "✓ " marker, and the no-touch margin column.
func maxStreamRunes(cols int) int {
	if cols <= 0 {
		return 0
	}
	avail := cols - 2 - 2 - 1
	if avail < 1 {
		return 1
	}
	return avail
}
