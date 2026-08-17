package progress

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// clearCIEnv unsets CI markers for the duration of a test.
func clearCIEnv(t *testing.T) {
	t.Helper()
	ciEnvVars := []string{"CI", "CONTINUOUS_INTEGRATION", "GITHUB_ACTIONS", "GITLAB_CI",
		"CIRCLECI", "JENKINS_URL", "TRAVIS", "BITBUCKET_BUILD_NUMBER", "AZURE_PIPELINES"}
	original := make(map[string]string)
	for _, key := range ciEnvVars {
		if val, exists := os.LookupEnv(key); exists {
			original[key] = val
		}
		_ = os.Unsetenv(key)
	}
	t.Cleanup(func() {
		for _, key := range ciEnvVars {
			_ = os.Unsetenv(key)
		}
		for key, val := range original {
			_ = os.Setenv(key, val)
		}
	})
}

func TestPhaseAppendOnlyOnNonTTY(t *testing.T) {
	clearCIEnv(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Packaging repository", false, WithPhaseWriter(&buf))
	// Mutations must be harmless no-ops in append-only mode.
	p.SetDetail("12 files · 1.0 MB")
	p.StreamLine("main.go")
	p.SetVerbs([]string{"a", "b"})
	p.SetMessage("Packaging repository")
	p.Succeed("Repository packaged", "1.2s · 3.4 MB · 12 files")

	out := buf.String()
	if !strings.Contains(out, "Packaging repository...") {
		t.Errorf("status line missing from append-only output: %q", out)
	}
	if !strings.Contains(out, "✓ Repository packaged") {
		t.Errorf("receipt missing from append-only output: %q", out)
	}
	if !strings.Contains(out, "(1.2s · 3.4 MB · 12 files)") {
		t.Errorf("receipt extra missing: %q", out)
	}
	// No live-region control sequences may reach a non-TTY writer.
	for _, seq := range []string{"\033[2K", "\033[A", "\033[J", cursorHide, cursorShow} {
		if strings.Contains(out, seq) {
			t.Errorf("control sequence %q leaked to non-TTY writer: %q", seq, out)
		}
	}
	// The status must not be duplicated by Succeed.
	if n := strings.Count(out, "Packaging repository..."); n != 1 {
		t.Errorf("status line printed %d times, want 1: %q", n, out)
	}
}

func TestPhaseFailPrintsNoReceipt(t *testing.T) {
	clearCIEnv(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Uploading to Armis Cloud", false, WithPhaseWriter(&buf))
	p.Fail()
	p.Fail() // idempotent
	out := buf.String()
	if strings.Contains(out, "✓") {
		t.Errorf("Fail must not print a receipt: %q", out)
	}
	if !strings.Contains(out, "Uploading to Armis Cloud...") {
		t.Errorf("status line missing: %q", out)
	}
}

func TestPhaseSucceedThenFailIsNoop(t *testing.T) {
	clearCIEnv(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Retrieving results", false, WithPhaseWriter(&buf))
	p.Succeed("Results retrieved", "1.8s")
	before := buf.String()
	p.Fail() // deferred-Fail pattern: must not disturb the receipt
	if buf.String() != before {
		t.Errorf("Fail after Succeed changed output: %q -> %q", before, buf.String())
	}
}

func TestPhaseDisabledMatchesCIBehavior(t *testing.T) {
	clearCIEnv(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "Packaging repository", true, WithPhaseWriter(&buf))
	p.Succeed("Repository packaged", "1.0s")
	out := buf.String()
	if !strings.Contains(out, "Packaging repository...") || !strings.Contains(out, "✓ Repository packaged") {
		t.Errorf("disabled mode should still print transcript lines: %q", out)
	}
}

func TestPhaseContextCancellation(t *testing.T) {
	clearCIEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	p := StartPhase(ctx, "test", false, WithPhaseWriter(&buf))
	cancel()
	time.Sleep(50 * time.Millisecond)
	p.Fail() // must not hang or panic after context cancellation
}

func TestPhaseElapsed(t *testing.T) {
	clearCIEnv(t)

	var buf bytes.Buffer
	p := StartPhase(context.Background(), "test", true, WithPhaseWriter(&buf))
	time.Sleep(30 * time.Millisecond)
	if p.Elapsed() < 30*time.Millisecond {
		t.Errorf("Elapsed() = %v, want >= 30ms", p.Elapsed())
	}
	p.Fail()
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2 KB"},
		{44355092, "42.3 MB"},
		{1288490189, "1.2 GB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.n); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1204, "1,204"},
		{1234567, "1,234,567"},
	}
	for _, tt := range tests {
		if got := FormatCount(tt.n); got != tt.want {
			t.Errorf("FormatCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestCountingReader(t *testing.T) {
	var last int64
	r := NewCountingReader(strings.NewReader("hello world"), func(total int64) { last = total })
	buf := make([]byte, 4)
	total := 0
	for {
		n, err := r.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	if total != 11 || last != 11 {
		t.Errorf("read %d bytes, callback saw %d; want 11/11", total, last)
	}
}
