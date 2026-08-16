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

// Phase is one unit of the scan transcript: a live status line while work is in
// flight, followed by a permanent receipt once it lands.
//
//	p := progress.StartPhase(ctx, "Packaging repository", progress.WithPhaseDisabled(noProgress))
//	defer p.Stop()
//	p.SetDetail("1,204 files · 42.3 MB")
//	p.SetMessage("Uploading to Armis Cloud")
//	p.Succeed("Repository packaged", "42.3 MB")
//
// Phase is a thin wrapper over Spinner and deliberately owns no rendering of its
// own: the head glyph, frame cadence, timer and width truncation all stay in the
// Spinner render loop. Phase only composes the message string it hands to
// Spinner.Update and prints the append-only lines around the live region.
//
// # Rendering contract
//
// Interactive (TTY, not CI, not disabled): a single-line live region redrawn in
// place by the Spinner, carrying `<label> — <detail>` plus the `[mm:ss]` timer.
// On Succeed/Fail the live region is erased and two plain lines are appended:
// the muted status line (so the transcript remembers what ran) and the receipt.
// Both scroll naturally.
//
// Non-interactive (CI, non-TTY, or --no-progress): StartPhase prints the status
// once, Succeed/Fail print the receipt, and SetMessage/SetDetail are no-ops. The
// status line is produced by the disabled Spinner, so today's CI output shape is
// preserved by construction rather than by duplication.
//
// # Output ownership rule
//
// While a Phase live region is active it is the sole owner of stderr. Any other
// write — a cli.PrintWarningf, a debug dump — lands in the middle of the line
// being redrawn and corrupts it. Callers must therefore yield the region via
// Succeed, Fail or Stop before writing anything else to stderr; every scan call
// site already does this, and Stop exists to make the quiet-abort path explicit.
//
// This is a convention, not an enforced lock, and it is sufficient only because
// the live region is one line: a stray write is overwritten by the next frame
// within a frame delay. When the region grows to multiple lines (the stream and
// activity-feed rungs) the convention must be upgraded to a real lock. Note that
// the lock cannot live here — internal/progress imports internal/output which
// imports internal/cli, so internal/cli cannot import back. internal/cli is the
// only cycle-free home for it, and the Spinner render goroutine will have to
// take it too.
//
// Phase is safe for concurrent use. Succeed, Fail and Stop are idempotent and
// mutually exclusive: the first one to run ends the phase.
type Phase struct {
	mu     sync.Mutex
	label  string
	detail string

	spinner *Spinner

	// Immutable after construction - safe to read without mutex.
	writer io.Writer
	live   bool

	endOnce sync.Once
}

// phaseDetailSeparator joins the phase label and its detail on the live line.
const phaseDetailSeparator = " — "

// phaseConfig holds StartPhase options before the Phase is built.
type phaseConfig struct {
	disabled bool
	writer   io.Writer
	timeout  time.Duration
}

// PhaseOption configures a Phase at construction time.
type PhaseOption func(*phaseConfig)

// WithPhaseDisabled turns off the live region (the --no-progress path). The
// phase still prints its status line and receipt.
func WithPhaseDisabled(disabled bool) PhaseOption {
	return func(c *phaseConfig) { c.disabled = disabled }
}

// WithPhaseWriter sets the output writer. Defaults to os.Stderr; stdout is
// reserved for scan results.
func WithPhaseWriter(w io.Writer) PhaseOption {
	return func(c *phaseConfig) { c.writer = w }
}

// WithPhaseTimeout overrides the live-region safety timeout that prevents
// goroutine leaks. Defaults to DefaultSpinnerTimeout.
func WithPhaseTimeout(d time.Duration) PhaseOption {
	return func(c *phaseConfig) { c.timeout = d }
}

// StartPhase begins a phase and starts its live region.
//
// The live region runs only on a real terminal outside CI and when not
// disabled; everywhere else the phase degrades to append-only output. The
// context cancels the live region, and the timeout is a safety net against
// leaked render goroutines.
func StartPhase(ctx context.Context, label string, opts ...PhaseOption) *Phase {
	cfg := phaseConfig{
		writer:  os.Stderr,
		timeout: DefaultSpinnerTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// The live region needs a real terminal: CI logs, pipes and files get
	// append-only output instead of in-place redraws.
	live := !cfg.disabled && !IsCI() && isTerminalWriter(cfg.writer)

	// A non-live phase is backed by a disabled Spinner, which prints the status
	// line once and never starts a goroutine.
	spinner := NewSpinnerWithTimeout(label, !live, cfg.timeout)
	spinner.ctx = ctx
	spinner.SetWriter(cfg.writer)

	p := &Phase{
		label:   label,
		spinner: spinner,
		writer:  cfg.writer,
		live:    live,
	}
	spinner.Start()
	return p
}

// SetDetail updates the trailing detail on the live line, e.g. a running file
// or byte count. No-op when the phase is not live.
func (p *Phase) SetDetail(detail string) {
	if !p.live {
		return
	}
	p.mu.Lock()
	p.detail = detail
	message := p.composedLocked()
	p.mu.Unlock()

	p.spinner.Update(message)
}

// SetMessage swaps the live line's label, keeping any detail in place. No-op
// when the phase is not live.
func (p *Phase) SetMessage(message string) {
	if !p.live {
		return
	}
	p.mu.Lock()
	p.label = message
	composed := p.composedLocked()
	p.mu.Unlock()

	p.spinner.Update(composed)
}

// Succeed ends the phase with a green receipt: `✓ <label>  (<elapsed> · <extra>)`.
// extra may be empty, in which case only the elapsed time is shown.
func (p *Phase) Succeed(label, extra string) {
	p.finish(true, label, extra)
}

// Fail ends the phase with a red receipt in the same shape as Succeed. It
// reports that this phase did not complete; returning the error is still the
// caller's job.
func (p *Phase) Fail(label, extra string) {
	p.finish(false, label, extra)
}

// Stop ends the phase without printing anything, erasing the live region. Use
// it to hand stderr back before printing a warning or an error, and as a
// `defer p.Stop()` guard so a phase is never left running on an early return.
// It is a no-op once the phase has ended.
func (p *Phase) Stop() {
	p.endOnce.Do(func() {
		p.spinner.Stop()
	})
}

// Elapsed returns how long the phase has been running.
func (p *Phase) Elapsed() time.Duration {
	return p.spinner.GetElapsed()
}

// finish erases the live region and appends the transcript lines.
func (p *Phase) finish(ok bool, label, extra string) {
	p.endOnce.Do(func() {
		elapsed := p.spinner.GetElapsed()

		p.mu.Lock()
		status := p.composedLocked()
		p.mu.Unlock()

		// Erases the live region on a TTY; a no-op for a disabled spinner, whose
		// status line was already appended by Start.
		p.spinner.Stop()

		styles := output.GetStyles()
		if p.live {
			// D3 append-memory: the line that was live stays in the transcript.
			p.printLine(styles.MutedText.Render(status))
		}
		p.printLine(receiptLine(styles, ok, label, elapsed, extra))
	})
}

// composedLocked builds the live message from the label and detail.
// Callers must hold p.mu.
func (p *Phase) composedLocked() string {
	if p.detail == "" {
		return p.label
	}
	return p.label + phaseDetailSeparator + p.detail
}

// printLine appends one line to the phase writer.
func (p *Phase) printLine(line string) {
	// armis:ignore cwe:253 reason:fmt.Fprintln to stderr; return value not actionable for log-style output
	_, _ = fmt.Fprintln(p.writer, line)
}

// receiptLine renders the permanent record of a finished phase:
//
//	✓ Repository packaged  (2.4s · 42.3 MB)
//
// The icon carries the outcome color and the parenthetical is muted, so the
// label is what the eye lands on. Under NoColorStyles every style is a no-op
// and the line degrades to plain text with the glyphs intact.
func receiptLine(styles *output.Styles, ok bool, label string, elapsed time.Duration, extra string) string {
	icon := styles.SuccessText.Render(output.IconSuccess)
	if !ok {
		icon = styles.ErrorText.Render(output.IconFailure)
	}

	meta := formatElapsedShort(elapsed)
	if extra != "" {
		meta += " · " + extra
	}

	var b strings.Builder
	b.WriteString(icon)
	b.WriteByte(' ')
	b.WriteString(label)
	b.WriteString("  ")
	b.WriteString(styles.MutedText.Render("(" + meta + ")"))
	return b.String()
}

// formatElapsedShort renders a receipt duration: sub-minute durations keep one
// decimal ("2.4s") because packaging and fetch phases are often over in a
// couple of seconds and a whole-second rounding reads as suspiciously round.
// Longer durations switch to "1m 04s", where tenths are noise.
func formatElapsedShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
}
