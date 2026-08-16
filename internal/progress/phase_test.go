package progress

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArmisSecurity/armis-cli/internal/cli"
	"github.com/ArmisSecurity/armis-cli/internal/output"
)

// ciEnvVarNames mirrors the list IsCI inspects.
var ciEnvVarNames = []string{
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

// clearCIEnv makes IsCI report false for the duration of the test. IsCI treats
// an empty value as unset, and t.Setenv restores the originals on cleanup.
func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range ciEnvVarNames {
		t.Setenv(key, "")
	}
}

// setCIEnv makes IsCI report true for the duration of the test.
func setCIEnv(t *testing.T) {
	t.Helper()
	clearCIEnv(t)
	t.Setenv("CI", "true")
}

// forceNoColor pins the style set to NoColorStyles so golden strings are stable
// regardless of the terminal the tests run in.
func forceNoColor(t *testing.T) {
	t.Helper()
	cli.InitColors(cli.ColorModeNever)
	output.SyncColors()
	t.Cleanup(func() {
		cli.InitColors(cli.ColorModeAuto)
		output.SyncColors()
	})
}

// newLivePhase builds a Phase with the live region flag forced on but backed by
// a disabled spinner, so the append-only output around the live region can be
// asserted without a pty. Start is deliberately not called: the spinner's start
// time is set at construction, and a disabled spinner never renders frames.
func newLivePhase(label string, w *bytes.Buffer) *Phase {
	return &Phase{
		label:   label,
		spinner: NewSpinnerWithTimeout(label, true, DefaultSpinnerTimeout),
		writer:  w,
		live:    true,
	}
}

func TestFormatElapsedShort(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"zero", 0, "0.0s"},
		{"negative clamps to zero", -5 * time.Second, "0.0s"},
		{"sub-second", 400 * time.Millisecond, "0.4s"},
		{"one decimal", 2400 * time.Millisecond, "2.4s"},
		{"rounds to one decimal", 2449 * time.Millisecond, "2.4s"},
		{"just under a minute", 59900 * time.Millisecond, "59.9s"},
		{"exactly a minute", time.Minute, "1m 00s"},
		{"minutes and seconds pad", 64 * time.Second, "1m 04s"},
		{"many minutes", 12*time.Minute + 34*time.Second, "12m 34s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatElapsedShort(tt.duration); got != tt.want {
				t.Errorf("formatElapsedShort(%v) = %q, want %q", tt.duration, got, tt.want)
			}
		})
	}
}

func TestReceiptLine(t *testing.T) {
	forceNoColor(t)
	styles := output.GetStyles()

	tests := []struct {
		name    string
		ok      bool
		label   string
		elapsed time.Duration
		extra   string
		want    string
	}{
		{
			name:    "success with extra",
			ok:      true,
			label:   "Repository packaged",
			elapsed: 2400 * time.Millisecond,
			extra:   "42.3 MB",
			want:    "✓ Repository packaged  (2.4s · 42.3 MB)",
		},
		{
			name:    "success without extra",
			ok:      true,
			label:   "Results retrieved",
			elapsed: 900 * time.Millisecond,
			want:    "✓ Results retrieved  (0.9s)",
		},
		{
			name:    "failure carries the reason",
			ok:      false,
			label:   "Analysis failed",
			elapsed: 95 * time.Second,
			extra:   "scan stopped",
			want:    "✗ Analysis failed  (1m 35s · scan stopped)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := receiptLine(styles, tt.ok, tt.label, tt.elapsed, tt.extra)
			if got != tt.want {
				t.Errorf("receiptLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPhaseComposedMessage(t *testing.T) {
	tests := []struct {
		name   string
		label  string
		detail string
		want   string
	}{
		{"label only", "Packaging repository", "", "Packaging repository"},
		{"label and detail", "Packaging repository", "1,204 files · 42.3 MB", "Packaging repository — 1,204 files · 42.3 MB"},
		{"empty detail is not separated", "Analyzing", "", "Analyzing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Phase{label: tt.label, detail: tt.detail}
			p.mu.Lock()
			got := p.composedLocked()
			p.mu.Unlock()
			if got != tt.want {
				t.Errorf("composedLocked() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPhaseLiveTransitionsFeedTheSpinner checks that SetDetail/SetMessage reach
// the spinner as a composed message. The spinner is what truncates that message
// to the terminal width, so composing here keeps Phase out of the render loop.
func TestPhaseLiveTransitionsFeedTheSpinner(t *testing.T) {
	var buf bytes.Buffer
	p := newLivePhase("Packaging repository", &buf)

	p.SetDetail("1,204 files · 42.3 MB")
	if got := spinnerMessage(p); got != "Packaging repository — 1,204 files · 42.3 MB" {
		t.Errorf("after SetDetail, spinner message = %q", got)
	}

	p.SetMessage("Uploading to Armis Cloud")
	if got := spinnerMessage(p); got != "Uploading to Armis Cloud — 1,204 files · 42.3 MB" {
		t.Errorf("after SetMessage, spinner message = %q (detail should be preserved)", got)
	}

	p.SetDetail("")
	if got := spinnerMessage(p); got != "Uploading to Armis Cloud" {
		t.Errorf("after clearing detail, spinner message = %q", got)
	}
}

func spinnerMessage(p *Phase) string {
	p.spinner.mu.RLock()
	defer p.spinner.mu.RUnlock()
	return p.spinner.message
}

// TestPhaseLiveSucceedOutput is the D3 golden: the muted status line stays in
// the transcript and the receipt appends beneath it.
func TestPhaseLiveSucceedOutput(t *testing.T) {
	forceNoColor(t)

	var buf bytes.Buffer
	p := newLivePhase("Packaging repository", &buf)
	p.SetDetail("1,204 files · 42.3 MB")
	p.Succeed("Repository packaged", "42.3 MB")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (muted status + receipt), got %d: %q", len(lines), buf.String())
	}
	if want := "Packaging repository — 1,204 files · 42.3 MB"; lines[0] != want {
		t.Errorf("status line = %q, want %q", lines[0], want)
	}
	if want := "✓ Repository packaged  (0.0s · 42.3 MB)"; lines[1] != want {
		t.Errorf("receipt line = %q, want %q", lines[1], want)
	}
}

func TestPhaseLiveFailOutput(t *testing.T) {
	forceNoColor(t)

	var buf bytes.Buffer
	p := newLivePhase("Analyzing repository", &buf)
	p.Fail("Analysis failed", "connection reset")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
	}
	if want := "Analyzing repository"; lines[0] != want {
		t.Errorf("status line = %q, want %q", lines[0], want)
	}
	if want := "✗ Analysis failed  (0.0s · connection reset)"; lines[1] != want {
		t.Errorf("receipt line = %q, want %q", lines[1], want)
	}
}

// TestStartPhaseNonInteractive covers the CI / non-TTY / --no-progress corners
// of the degradation matrix: the status prints once, the receipt prints, and
// nothing redraws.
func TestStartPhaseNonInteractive(t *testing.T) {
	tests := []struct {
		name     string
		setupEnv func(t *testing.T)
		disabled bool
	}{
		{"non-TTY writer", clearCIEnv, false},
		{"CI environment", setCIEnv, false},
		{"progress disabled", clearCIEnv, true},
		{"disabled in CI", setCIEnv, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv(t)
			forceNoColor(t)

			var buf bytes.Buffer
			p := StartPhase(context.Background(), "Packaging repository",
				WithPhaseWriter(&buf),
				WithPhaseDisabled(tt.disabled))

			if p.live {
				t.Fatal("phase should not be live for a non-terminal writer")
			}

			// Live-region mutators must be no-ops here.
			p.SetDetail("1,204 files · 42.3 MB")
			p.SetMessage("Uploading to Armis Cloud")

			p.Succeed("Repository packaged", "42.3 MB")

			out := buf.String()
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected 2 lines (status + receipt), got %d: %q", len(lines), out)
			}

			// Today's CI shape: "<label> (started at HH:MM:SS)".
			if !strings.HasPrefix(lines[0], "Packaging repository (started at ") {
				t.Errorf("status line = %q, want today's CI shape", lines[0])
			}
			if !strings.HasPrefix(lines[1], "✓ Repository packaged  (") {
				t.Errorf("receipt line = %q, want a receipt", lines[1])
			}
			if !strings.HasSuffix(lines[1], "· 42.3 MB)") {
				t.Errorf("receipt line = %q, want the extra appended", lines[1])
			}

			// The no-op mutators must not have leaked into the output.
			if strings.Contains(out, "1,204 files") || strings.Contains(out, "Uploading to Armis Cloud") {
				t.Errorf("SetDetail/SetMessage should be no-ops off-TTY, got %q", out)
			}
		})
	}
}

// TestPhaseNoControlSequencesOnNonTTY is the CI-log guarantee: no cursor codes,
// no line clears, no OSC sequences reach a non-terminal writer.
func TestPhaseNoControlSequencesOnNonTTY(t *testing.T) {
	clearCIEnv(t)
	forceNoColor(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Packaging repository", WithPhaseWriter(&buf))
	p.SetDetail("1,204 files")
	p.Succeed("Repository packaged", "42.3 MB")

	out := buf.String()
	forbidden := map[string]string{
		"\033[?25l": "cursor hide",
		"\033[?25h": "cursor show",
		"\r\033[K":  "line clear",
		"\033]":     "OSC sequence",
		"\033":      "any escape sequence",
	}
	for seq, name := range forbidden {
		if strings.Contains(out, seq) {
			t.Errorf("%s should not be written to a non-TTY writer, got %q", name, out)
		}
	}
}

func TestPhaseEndIsIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		end   func(p *Phase)
		lines int
	}{
		{"succeed then succeed", func(p *Phase) { p.Succeed("a", ""); p.Succeed("b", "") }, 2},
		{"succeed then fail", func(p *Phase) { p.Succeed("a", ""); p.Fail("b", "") }, 2},
		{"succeed then stop", func(p *Phase) { p.Succeed("a", ""); p.Stop() }, 2},
		{"stop then succeed", func(p *Phase) { p.Stop(); p.Succeed("a", "") }, 1},
		{"stop then stop", func(p *Phase) { p.Stop(); p.Stop() }, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearCIEnv(t)
			forceNoColor(t)

			var buf bytes.Buffer
			p := StartPhase(context.Background(), "Working", WithPhaseWriter(&buf))
			tt.end(p)

			lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
			if len(lines) != tt.lines {
				t.Errorf("expected %d lines, got %d: %q", tt.lines, len(lines), buf.String())
			}
		})
	}
}

// TestPhaseStopYieldsWithoutReceipt covers the output-ownership rule: Stop is
// how a caller hands stderr back before printing a warning.
func TestPhaseStopYieldsWithoutReceipt(t *testing.T) {
	clearCIEnv(t)
	forceNoColor(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Retrieving results", WithPhaseWriter(&buf))
	p.Stop()

	out := buf.String()
	if strings.Contains(out, "✓") || strings.Contains(out, "✗") {
		t.Errorf("Stop must not print a receipt, got %q", out)
	}
	if !strings.HasPrefix(out, "Retrieving results (started at ") {
		t.Errorf("status line should still be present, got %q", out)
	}
}

func TestPhaseElapsedAdvances(t *testing.T) {
	clearCIEnv(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Working", WithPhaseWriter(&buf))
	time.Sleep(20 * time.Millisecond)
	if p.Elapsed() <= 0 {
		t.Error("Elapsed() should advance while the phase runs")
	}
	p.Stop()
}

func TestPhaseConcurrentUpdates(t *testing.T) {
	clearCIEnv(t)
	forceNoColor(t)

	var buf bytes.Buffer
	p := newLivePhase("Working", &buf)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			p.SetDetail("detail")
		}(i)
		go func(n int) {
			defer wg.Done()
			p.SetMessage("message")
		}(i)
	}
	wg.Wait()

	// Concurrent enders must still yield exactly one receipt.
	wg.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer wg.Done()
			p.Succeed("Done", "")
		}()
	}
	wg.Wait()

	if got := strings.Count(buf.String(), "✓"); got != 1 {
		t.Errorf("expected exactly 1 receipt, got %d: %q", got, buf.String())
	}
}

// TestPhaseNoGoroutineLeak guards the lifecycle: a non-live phase must never
// start a render goroutine, and Stop must reap the one a live phase starts.
func TestPhaseNoGoroutineLeak(t *testing.T) {
	clearCIEnv(t)

	// Let any goroutines from earlier tests settle.
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		p := StartPhase(context.Background(), "Working", WithPhaseWriter(&buf))
		p.SetDetail("detail")
		p.Succeed("Done", "extra")
	}

	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after > before+2 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestPhaseContextCancellationStopsPhase(t *testing.T) {
	clearCIEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	p := StartPhase(ctx, "Working", WithPhaseWriter(&buf), WithPhaseTimeout(time.Second))
	cancel()

	// Stop must remain safe after the context has already gone away.
	p.Stop()
	p.Succeed("Done", "")

	if strings.Contains(buf.String(), "✓") {
		t.Error("Succeed after Stop should be a no-op")
	}
}
