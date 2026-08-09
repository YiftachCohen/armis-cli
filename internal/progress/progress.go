// Package progress provides progress indicators for CLI operations.
package progress

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ArmisSecurity/armis-cli/internal/output"
	"github.com/charmbracelet/lipgloss"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/term"
)

const (
	// DefaultSpinnerTimeout is the maximum time a spinner will run before auto-stopping.
	// This is a safety net to prevent indefinite goroutine leaks.
	DefaultSpinnerTimeout = 30 * time.Minute

	// spinnerFrameDelay is the delay between animation frames. 80ms (12.5 fps)
	// keeps the wave and shimmer fluid without noticeable CPU or write pressure.
	spinnerFrameDelay = 80 * time.Millisecond

	// ANSI escape sequences for cursor visibility control.
	// These are standard VT100/xterm sequences supported by all modern terminals.
	cursorHide = "\033[?25l"
	cursorShow = "\033[?25h"
)

// IsCI returns true if running in a CI environment.
func IsCI() bool {
	ciEnvVars := []string{
		"CI",
		"CONTINUOUS_INTEGRATION",
		"GITHUB_ACTIONS",
		"GITLAB_CI",
		"CIRCLECI",
		"JENKINS_URL",
		"TRAVIS",
		"BITBUCKET_BUILD_NUMBER",
		"AZURE_PIPELINES",
	}

	for _, envVar := range ciEnvVars {
		if os.Getenv(envVar) != "" {
			return true
		}
	}
	return false
}

// fdWriter is an interface for writers that can provide a file descriptor.
// *os.File implements this interface.
type fdWriter interface {
	Fd() uintptr
}

// isTerminalWriter reports whether the given writer is connected to a terminal.
func isTerminalWriter(w io.Writer) bool {
	if f, ok := w.(fdWriter); ok {
		return term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: Fd() returns uintptr which fits in int on all supported platforms
	}
	return false
}

// NewReader wraps a reader with a progress bar.
func NewReader(r io.Reader, size int64, description string, disabled bool) io.Reader {
	if disabled || IsCI() {
		return r
	}

	bar := progressbar.DefaultBytes(
		size,
		description,
	)

	reader := progressbar.NewReader(r, bar)
	return &reader
}

// NewWriter wraps a writer with a progress bar.
func NewWriter(w io.Writer, size int64, description string, disabled bool) io.Writer {
	if disabled || IsCI() {
		return w
	}

	bar := progressbar.DefaultBytes(
		size,
		description,
	)

	return io.MultiWriter(w, bar)
}

// Spinner displays an animated spinner with a message.
type Spinner struct {
	mu        sync.RWMutex
	message   string
	disabled  bool // Immutable after construction - safe to read without mutex
	stopChan  chan struct{}
	doneChan  chan struct{}
	startTime time.Time
	showTimer bool      // Immutable after construction - safe to read without mutex
	writer    io.Writer // Immutable after construction - safe to read without mutex

	// Fields for goroutine leak prevention
	ctx      context.Context    // Parent context for cancellation
	cancel   context.CancelFunc // Internal cancel function
	stopOnce sync.Once          // Ensures Stop() is idempotent
	started  bool               // Tracks if Start() was called
	timeout  time.Duration      // Maximum spinner lifetime (safety net)
}

// NewSpinner creates a new spinner with the given message.
// Uses DefaultSpinnerTimeout as a safety net to prevent goroutine leaks.
func NewSpinner(message string, disabled bool) *Spinner {
	return NewSpinnerWithTimeout(message, disabled, DefaultSpinnerTimeout)
}

// NewSpinnerWithTimeout creates a new spinner with a custom timeout.
// The timeout acts as a safety net - if Stop() is not called within this duration,
// the spinner will automatically stop to prevent goroutine leaks.
// A timeout of 0 means no automatic timeout (use with caution).
func NewSpinnerWithTimeout(message string, disabled bool, timeout time.Duration) *Spinner {
	// armis:ignore cwe:401 reason:Spinner goroutine has proper lifecycle via stopChan/doneChan and timeout safety net
	return &Spinner{
		message:   message,
		disabled:  disabled,
		stopChan:  make(chan struct{}),
		doneChan:  make(chan struct{}),
		startTime: time.Now(),
		showTimer: true,
		writer:    os.Stderr,
		timeout:   timeout,
	}
}

// NewSpinnerWithContext creates a new spinner that respects context cancellation.
// When the context is canceled, the spinner automatically stops.
// This is the recommended way to create spinners in operations that use context.
func NewSpinnerWithContext(ctx context.Context, message string, disabled bool) *Spinner {
	s := NewSpinnerWithTimeout(message, disabled, DefaultSpinnerTimeout)
	s.ctx = ctx
	return s
}

// SetWriter sets the output writer for the spinner (useful for testing).
// Must be called before Start to avoid data races.
func (s *Spinner) SetWriter(w io.Writer) {
	s.writer = w
}

// Start begins the spinner animation.
// The spinner will automatically stop if:
// - Stop() is called
// - The context (if provided) is canceled
// - The timeout (if set) is reached
func (s *Spinner) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return // Already started, no-op
	}
	s.started = true
	s.startTime = time.Now() // Reset start time on Start()
	// Recreate channels and reset stopOnce to ensure they are fresh. This guards against
	// future changes that might allow spinner reuse - closed channels cannot be reused in Go,
	// and an exhausted sync.Once would make subsequent Stop() calls no-ops.
	s.stopChan = make(chan struct{})
	s.doneChan = make(chan struct{})
	s.stopOnce = sync.Once{}

	// Capture values while holding mutex to avoid race conditions
	startTime := s.startTime
	message := s.message
	s.mu.Unlock()

	if s.disabled || IsCI() {
		// armis:ignore cwe:253 reason:fmt.Fprintf to stderr; return value not actionable for log-style output
		_, _ = fmt.Fprintf(s.writer, "%s (started at %s)\n", message, startTime.Format("15:04:05"))
		return
	}

	// Create internal context with timeout if configured
	var ctx context.Context
	var cancel context.CancelFunc

	if s.ctx != nil {
		// Use provided context as parent
		if s.timeout > 0 {
			ctx, cancel = context.WithTimeout(s.ctx, s.timeout)
		} else {
			ctx, cancel = context.WithCancel(s.ctx)
		}
	} else {
		// No parent context
		if s.timeout > 0 {
			ctx, cancel = context.WithTimeout(context.Background(), s.timeout)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
	}

	// Set cancel under mutex to avoid race with Stop()
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	// armis:ignore cwe:401 reason:goroutine has proper lifecycle via doneChan + cancel; stopped by Spinner.Stop()
	go func() {
		defer close(s.doneChan)
		defer cancel() // Ensure context is canceled when goroutine exits

		// Hide cursor during spinner animation on real terminals.
		// Skip for non-TTY writers (pipes, files, test buffers) to avoid garbage output.
		// Cursor hide/show are cursor control sequences (not color), so they work
		// even when --color=never. This matches clearLine() which uses \033[K.
		hideCursor := isTerminalWriter(s.writer)
		if hideCursor {
			// armis:ignore cwe:253 reason:fmt.Fprint to terminal for cursor control; return value not actionable
			_, _ = fmt.Fprint(s.writer, cursorHide)
			// armis:ignore cwe:253 reason:deferred fmt.Fprint for cursor restore; return value not actionable
			defer func() { _, _ = fmt.Fprint(s.writer, cursorShow) }()
		} // armis:ignore cwe:253

		i := 0
		ticker := time.NewTicker(spinnerFrameDelay)
		defer ticker.Stop()

		// clearLine returns the line-clear sequence.
		// Always use \r\033[K (carriage return + erase to EOL) to prevent
		// trailing characters when messages shrink. The \033[K (CSI K) sequence
		// is cursor control, not color, and works on all VT100-compatible terminals.
		clearLine := func() string {
			return "\r\033[K"
		}

		for {
			select {
			case <-s.stopChan:
				// armis:ignore cwe:253 reason:fmt.Fprint to terminal for line clearing; return value not actionable
				_, _ = fmt.Fprint(s.writer, clearLine())
				return
			case <-ctx.Done():
				// armis:ignore cwe:253 reason:fmt.Fprint to terminal for line clearing; return value not actionable
				_, _ = fmt.Fprint(s.writer, clearLine())
				return
			case <-ticker.C:
				elapsed := time.Since(startTime)
				s.mu.RLock()
				msg := s.message
				s.mu.RUnlock()
				styles := output.GetStyles()

				var timerStr string
				if s.showTimer {
					timerStr = "[" + formatDuration(elapsed) + "]"
				}
				msg = truncateMessage(msg, maxMessageRunes(s.writer, len(timerStr)))

				var frame strings.Builder
				frame.WriteString(clearLine())
				frame.WriteString(waveFrame(styles, i))
				frame.WriteByte(' ')
				frame.WriteString(shimmerText(styles, msg, i))
				if timerStr != "" {
					frame.WriteByte(' ')
					frame.WriteString(styles.SpinnerTimer.Render(timerStr))
				}
				// armis:ignore cwe:253 reason:fmt.Fprint to terminal for spinner output; return value not actionable
				_, _ = fmt.Fprint(s.writer, frame.String())
				i++
			}
		}
	}()
}

// Stop stops the spinner animation.
// Stop is safe to call multiple times - subsequent calls are no-ops.
// Stop is also safe to call if Start() was never called.
func (s *Spinner) Stop() {
	if s.disabled || IsCI() {
		return
	}

	s.stopOnce.Do(func() {
		s.mu.RLock()
		started := s.started
		cancel := s.cancel
		s.mu.RUnlock()

		if !started {
			return // Start() was never called
		}

		// Cancel the internal context first (belt and suspenders)
		if cancel != nil {
			cancel()
		}

		// Close stopChan to signal the goroutine
		close(s.stopChan)

		// Wait for the goroutine to finish with a timeout
		// This prevents indefinite blocking if something goes wrong
		select {
		case <-s.doneChan:
			// Goroutine exited cleanly
		case <-time.After(5 * time.Second):
			// Timeout waiting for goroutine - don't block indefinitely
		}
	})
}

// Update updates the spinner message.
func (s *Spinner) Update(message string) {
	s.mu.Lock()
	s.message = message
	s.mu.Unlock()
}

// GetElapsed returns the elapsed time since the spinner started.
func (s *Spinner) GetElapsed() time.Duration {
	s.mu.RLock()
	startTime := s.startTime
	s.mu.RUnlock()
	return time.Since(startTime)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

// --- Wave loader ---------------------------------------------------------
//
// The loader head is a small strip of braille density glyphs animated as a
// travelling sine wave. Each cell's amplitude picks both its glyph and a
// shade from the brand gradient (peaks are brightest), so the wave reads as
// a pulse of light moving through the strip. All shading goes through the
// central style set, which degrades to monochrome under --color=never while
// the wave shape keeps animating (glyph choice is not a color concern).

// waveWidth is the number of cells in the animated wave strip.
const waveWidth = 5

const (
	// waveSpeed is the phase advance per frame (radians); one full swell
	// takes ~1.1s at the 80ms frame delay.
	waveSpeed = 0.45
	// wavePhaseStep is the phase offset between adjacent cells, which gives
	// the wave its spatial slope.
	wavePhaseStep = 0.85
)

// waveGlyphs are braille density glyphs indexed by amplitude, flat first.
// U+2800 (blank braille) keeps the trough the same advance width as the
// other cells in monospace fonts.
var waveGlyphs = []rune{'⠀', '⣀', '⣤', '⣶', '⣿'}

// waveFrame renders one frame of the wave strip.
func waveFrame(styles *output.Styles, frame int) string {
	var b strings.Builder
	for j := 0; j < waveWidth; j++ {
		phase := float64(frame)*waveSpeed - float64(j)*wavePhaseStep
		level := int((math.Sin(phase) + 1) / 2 * float64(len(waveGlyphs)))
		if level >= len(waveGlyphs) {
			level = len(waveGlyphs) - 1
		}
		b.WriteString(waveStyle(styles, level).Render(string(waveGlyphs[level])))
	}
	return b.String()
}

// waveStyle maps a wave amplitude to a gradient shade (peaks brightest).
func waveStyle(styles *output.Styles, level int) lipgloss.Style {
	if len(styles.SpinnerWave) == 0 {
		return styles.SpinnerChar
	}
	idx := len(waveGlyphs) - 1 - level
	if idx >= len(styles.SpinnerWave) {
		idx = len(styles.SpinnerWave) - 1
	}
	return styles.SpinnerWave[idx]
}

// --- Shimmer -------------------------------------------------------------
//
// A highlight band sweeps across the message text, then rests before the
// next pass. Runes are grouped into contiguous same-intensity segments so a
// frame emits at most a handful of escape sequences.

const (
	// shimmerCoreRadius is the half-width (in runes) of the bright center.
	shimmerCoreRadius = 1
	// shimmerEdgeRadius is the half-width of the softer surrounding band.
	shimmerEdgeRadius = 3
	// shimmerRestCols is extra travel past the text end so the highlight
	// rests between sweeps instead of looping continuously.
	shimmerRestCols = 16
)

// shimmerText renders the message with the shimmer highlight for one frame.
func shimmerText(styles *output.Styles, msg string, frame int) string {
	runes := []rune(msg)
	if len(runes) == 0 {
		return msg
	}
	cycle := len(runes) + shimmerRestCols
	center := frame%cycle - shimmerEdgeRadius

	var b strings.Builder
	var seg strings.Builder
	segLevel := 0
	flush := func() {
		if seg.Len() == 0 {
			return
		}
		switch segLevel {
		case 2:
			b.WriteString(styles.SpinnerShimmerCore.Render(seg.String()))
		case 1:
			b.WriteString(styles.SpinnerShimmerEdge.Render(seg.String()))
		default:
			b.WriteString(styles.SpinnerText.Render(seg.String()))
		}
		seg.Reset()
	}
	for k, r := range runes {
		d := k - center
		if d < 0 {
			d = -d
		}
		level := 0
		switch {
		case d <= shimmerCoreRadius:
			level = 2
		case d <= shimmerEdgeRadius:
			level = 1
		}
		if level != segLevel {
			flush()
			segLevel = level
		}
		seg.WriteRune(r)
	}
	flush()
	return b.String()
}

// --- Width handling ------------------------------------------------------

// maxMessageRunes returns how many message runes fit on the current terminal
// line alongside the wave, spacing, and timer. Returns 0 (no limit) when the
// writer is not a terminal or its width cannot be determined. Called every
// frame so resizes are picked up; the underlying ioctl is cheap.
func maxMessageRunes(w io.Writer, timerLen int) int {
	f, ok := w.(fdWriter)
	if !ok {
		return 0
	}
	fd := int(f.Fd()) //nolint:gosec // G115: Fd() returns uintptr which fits in int on all supported platforms
	if !term.IsTerminal(fd) {
		return 0
	}
	cols, _, err := term.GetSize(fd)
	if err != nil || cols <= 0 {
		return 0
	}
	// wave + space + space-before-timer + timer + one column so the line
	// never touches the last cell (some terminals wrap eagerly there).
	avail := cols - waveWidth - 1 - 1 - timerLen - 1
	if avail < 1 {
		return 1
	}
	return avail
}

// truncateMessage trims msg to maxRunes, appending an ellipsis when it was
// cut. maxRunes <= 0 means unlimited. Assumes single-cell runes, which holds
// for the ASCII status messages this CLI emits.
func truncateMessage(msg string, maxRunes int) string {
	if maxRunes <= 0 {
		return msg
	}
	runes := []rune(msg)
	if len(runes) <= maxRunes {
		return msg
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}
