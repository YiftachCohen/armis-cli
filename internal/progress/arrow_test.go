package progress

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ArmisSecurity/armis-cli/internal/model"
	"github.com/ArmisSecurity/armis-cli/internal/output"
)

func TestArrowDotsGeometry(t *testing.T) {
	if len(arrowDots) < 20 {
		t.Fatalf("arrow should have a meaningful dot count, got %d", len(arrowDots))
	}
	for i, d := range arrowDots {
		if d.x < 0 || d.x > 12 || d.y < 0 || d.y > 4.1 {
			t.Errorf("dot %d out of grid: (%f, %f)", i, d.x, d.y)
		}
		if d.u < 0 || d.u > 1 {
			t.Errorf("dot %d axis position out of [0,1]: %f", i, d.u)
		}
	}
}

func TestArrowFrameProgression(t *testing.T) {
	styles := output.NoColorStyles()
	early := arrowFrame(styles, 0)
	rest := arrowFrame(styles, arrowTotalFrames)
	if len([]rune(rest)) != 6 {
		t.Fatalf("resting frame should span 6 cells, got %d: %q", len([]rune(rest)), rest)
	}
	// The resting frame must contain braille dots; the very first frame is
	// mostly scatter and may be nearly empty.
	hasBraille := false
	for _, r := range rest {
		if r >= 0x2800 && r <= 0x28FF {
			hasBraille = true
		}
	}
	if !hasBraille {
		t.Errorf("resting frame has no braille dots: %q", rest)
	}
	if early == rest {
		t.Error("arrow should animate between first and last frame")
	}
	// Determinism: the same frame renders identically.
	first := arrowFrame(styles, 7)
	second := arrowFrame(styles, 7)
	if first != second {
		t.Error("arrowFrame must be deterministic per frame")
	}
}

func TestRenderFinaleCleanNonTTY(t *testing.T) {
	clearCIEnv(t)
	var buf bytes.Buffer
	RenderFinale(&buf, FinaleData{
		ScanID:     "scn_test",
		TotalStr:   "42s",
		Findings:   nil,
		BySeverity: map[model.Severity]int{},
	}, false)
	out := buf.String()
	if !strings.Contains(out, "No findings — all clear") {
		t.Errorf("clean finale missing all-clear: %q", out)
	}
	if !strings.Contains(out, "42s total") {
		t.Errorf("clean finale missing total: %q", out)
	}
	if strings.Contains(out, "\033[K") || strings.Contains(out, cursorHide) {
		t.Errorf("animation control codes leaked to non-TTY: %q", out)
	}
}

func TestRenderFinaleFindingsNonTTY(t *testing.T) {
	clearCIEnv(t)
	var buf bytes.Buffer
	findings := []model.Finding{
		{Severity: model.SeverityMedium, Title: "GPL-3.0 license", Package: "vendor/parser"},
		{Severity: model.SeverityCritical, Title: "Exposed AWS access key", File: ".env.backup", StartLine: 3},
		{Severity: model.SeverityHigh, Title: "Prototype pollution", Package: "lodash", Version: "4.17.20"},
	}
	RenderFinale(&buf, FinaleData{
		ScanID:   "scn_test",
		TotalStr: "1m 5s",
		Findings: findings,
		BySeverity: map[model.Severity]int{
			model.SeverityCritical: 1, model.SeverityHigh: 1, model.SeverityMedium: 1,
		},
	}, false)
	out := buf.String()

	// Reveal is severity-ordered: critical before high before medium.
	ci := strings.Index(out, "Exposed AWS access key")
	hi := strings.Index(out, "Prototype pollution")
	mi := strings.Index(out, "GPL-3.0 license")
	ordered := ci >= 0 && hi >= 0 && mi >= 0 && ci < hi && hi < mi
	if !ordered {
		t.Errorf("findings not revealed in severity order (c=%d h=%d m=%d): %q", ci, hi, mi, out)
	}
	if !strings.Contains(out, ".env.backup:3") {
		t.Errorf("finding location missing: %q", out)
	}
	if !strings.Contains(out, "lodash@4.17.20") {
		t.Errorf("package location missing: %q", out)
	}
	if !strings.Contains(out, "● 1 critical") || !strings.Contains(out, "● 1 high") {
		t.Errorf("severity summary missing: %q", out)
	}
	if !strings.Contains(out, "Scan complete") {
		t.Errorf("completion line missing: %q", out)
	}
}

func TestFindingLocation(t *testing.T) {
	tests := []struct {
		f    model.Finding
		want string
	}{
		{model.Finding{File: "a.go", StartLine: 7}, "a.go:7"},
		{model.Finding{File: "a.go"}, "a.go"},
		{model.Finding{Package: "lodash", Version: "1.2.3"}, "lodash@1.2.3"},
		{model.Finding{Package: "lodash"}, "lodash"},
		{model.Finding{}, ""},
	}
	for _, tt := range tests {
		if got := findingLocation(tt.f); got != tt.want {
			t.Errorf("findingLocation(%+v) = %q, want %q", tt.f, got, tt.want)
		}
	}
}
