// The Armis arrow one-shot and the scan finale (spec features F1/F2, E2+E3,
// F3). The arrow is a line-size braille dot animation: dots assemble from a
// scatter into the arrow's gesture, a band of light sweeps up its axis, and
// it comes to rest — played exactly once, at completion. The finale then
// paces the findings reveal and severity count-up. All of it is stderr
// presentation; results on stdout are untouched.

package progress

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ArmisSecurity/armis-cli/internal/model"
	"github.com/ArmisSecurity/armis-cli/internal/output"
)

// arrowDot is one dot of the arrow: target position on a 12x4 dot grid
// (2x4 dots per braille cell, 6 cells wide) and its normalized position u
// along the arrow's rising axis (0 = tail, 1 = tip), used for the assemble
// stagger and the light sweep. Geometry per the approved prototype
// (docs/design/scan-experience-prototype.html) — the arrow's *gesture*:
// long rising flank, apex, short drop to the tip.
type arrowDot struct{ x, y, u float64 }

var arrowDots = buildArrowDots()

func buildArrowDots() []arrowDot {
	path := [][2]float64{{0.6, 3.2}, {7.3, 0.2}, {11.4, 2.3}}
	var pts [][2]float64
	for s := 0; s < len(path)-1; s++ {
		x1, y1 := path[s][0], path[s][1]
		x2, y2 := path[s+1][0], path[s+1][1]
		n := int(math.Round(math.Hypot(x2-x1, y2-y1) / 0.45))
		if n < 2 {
			n = 2
		}
		start := 0
		if s > 0 {
			start = 1
		}
		for k := start; k <= n; k++ {
			u := float64(k) / float64(n)
			pts = append(pts, [2]float64{x1 + (x2-x1)*u, y1 + (y2-y1)*u})
		}
	}
	// Thicken the long rising flank so it reads as a stroke, not a wire.
	for i := len(pts) - 1; i >= 0; i-- {
		if pts[i][1] < 2.5 {
			pts = append(pts, [2]float64{pts[i][0] + 0.5, pts[i][1] + 0.85})
		}
	}
	// Normalize each dot's position along the rising axis (x - 0.6y).
	minU, maxU := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		u := p[0] - p[1]*0.6
		minU = math.Min(minU, u)
		maxU = math.Max(maxU, u)
	}
	dots := make([]arrowDot, len(pts))
	for i, p := range pts {
		dots[i] = arrowDot{x: p[0], y: p[1], u: (p[0] - p[1]*0.6 - minU) / (maxU - minU)}
	}
	return dots
}

// brailleBits maps (x%2, y%4) sub-dot coordinates to braille bit values.
var brailleBits = [2][4]int{{1, 2, 4, 64}, {8, 16, 32, 128}}

// dotRand is a deterministic pseudo-random in [0,1) so a given frame is
// stable across renders (mirrors the prototype's hash).
func dotRand(i, salt int) float64 {
	x := math.Sin(float64(i)*127.1+float64(salt)*311.7) * 43758.5453
	return x - math.Floor(x)
}

func smoothstep(u float64) float64 {
	if u < 0 {
		return 0
	}
	if u > 1 {
		return 1
	}
	return u * u * (3 - 2*u)
}

// arrowFrame renders the arrow at animation time lt (in frames): assemble
// over ~16 frames with per-dot stagger, light sweep over frames 20–32,
// rest thereafter.
func arrowFrame(styles *output.Styles, lt float64) string {
	var code [6]int
	var val [6]float64
	for i, d := range arrowDots {
		a := smoothstep((lt/16 - d.u*0.55) / 0.45)
		if a <= 0.03 {
			continue
		}
		sa := dotRand(i, 3) * 2 * math.Pi
		sr := 5 * (0.8 + dotRand(i, 5)*0.6)
		x := d.x*a + (6+math.Cos(sa)*sr)*(1-a)
		y := d.y*a + (2+math.Sin(sa)*sr*0.5)*(1-a)
		v := 0.55 * (0.3 + 0.7*a)
		if sw := (lt - 20) / 12; sw > 0 && sw < 1 && math.Abs(d.u-sw) < 0.14 {
			v = 1
		}
		xi, yi := int(math.Round(x)), int(math.Round(y))
		if xi < 0 || xi > 11 || yi < 0 || yi > 3 {
			continue
		}
		ci := xi >> 1
		code[ci] |= brailleBits[xi&1][yi&3]
		if v > val[ci] {
			val[ci] = v
		}
	}
	var b strings.Builder
	for c := 0; c < 6; c++ {
		if code[c] == 0 {
			b.WriteByte(' ')
			continue
		}
		glyph := string(rune(0x2800 + code[c]))
		switch {
		case val[c] > 0.85:
			b.WriteString(styles.ArrowBright.Render(glyph))
		case val[c] > 0.5:
			b.WriteString(styles.ArrowMid.Render(glyph))
		default:
			b.WriteString(styles.ArrowDim.Render(glyph))
		}
	}
	return b.String()
}

// arrowFrameDelay is the arrow's own tick — 40ms so the ~40-frame one-shot
// lands in about 1.6 seconds.
const arrowFrameDelay = 40 * time.Millisecond

// arrowTotalFrames covers assemble + sweep + a short rest.
const arrowTotalFrames = 40

// playArrow renders the one-shot on w (assumed TTY) and leaves the final
// arrow + text as a permanent line.
func playArrow(w io.Writer, text string) {
	styles := output.GetStyles()
	// armis:ignore cwe:253 reason:cursor control write; return value not actionable
	_, _ = fmt.Fprint(w, cursorHide)
	for lt := 0; lt <= arrowTotalFrames; lt++ {
		// The label brightens from muted to bold as the arrow assembles.
		label := styles.MutedText.Render(text)
		if lt > 14 {
			label = styles.Bold.Render(text)
		}
		// armis:ignore cwe:253 reason:animation frame write; return value not actionable
		_, _ = fmt.Fprintf(w, "\r\033[K%s %s", arrowFrame(styles, float64(lt)), label)
		time.Sleep(arrowFrameDelay)
	}
	// armis:ignore cwe:253 reason:cursor restore + newline; return value not actionable
	_, _ = fmt.Fprint(w, cursorShow, "\n")
}

// FinaleData carries everything the completion sequence needs.
type FinaleData struct {
	ScanID     string
	TotalStr   string // preformatted total elapsed, e.g. "2m 30s"
	Findings   []model.Finding
	BySeverity map[model.Severity]int
}

// severityRank orders the reveal: critical first.
var severityRank = map[model.Severity]int{
	model.SeverityCritical: 0,
	model.SeverityHigh:     1,
	model.SeverityMedium:   2,
	model.SeverityLow:      3,
	model.SeverityInfo:     4,
}

// revealCap bounds the staggered teaser; the full report follows on stdout.
const revealCap = 4

// RenderFinale plays the completion sequence on w (stderr): the arrow
// one-shot (F1 — every completion), then for scans with findings a held
// beat and the staggered dot-prefix reveal (E2+E3) and severity count-up
// (F3); clean scans get the green all-clear beside the arrow (F2). In CI,
// non-TTY, or disabled mode everything prints instantly with no animation.
func RenderFinale(w io.Writer, d FinaleData, disabled bool) {
	styles := output.GetStyles()
	tty := !disabled && !IsCI() && isTerminalWriter(w)
	clean := len(d.Findings) == 0

	if clean {
		text := "No findings — all clear"
		if tty {
			playArrow(w, text)
			// Rewrite the resting line with the final styled form.
			// armis:ignore cwe:253 reason:transcript write; return value not actionable
			_, _ = fmt.Fprintf(w, "\033[A\r\033[K%s %s%s\n",
				arrowFrameFinal(styles), styles.SuccessText.Render(text),
				styles.MutedText.Render("  "+d.TotalStr+" total"))
		} else {
			// armis:ignore cwe:253 reason:transcript write; return value not actionable
			_, _ = fmt.Fprintf(w, "%s%s\n", styles.SuccessText.Render("✓ "+text),
				styles.MutedText.Render("  "+d.TotalStr+" total"))
		}
		return
	}

	completeText := "Scan complete"
	if tty {
		playArrow(w, completeText+styles.MutedText.Render("  "+d.TotalStr+" total"))
		time.Sleep(800 * time.Millisecond) // the held beat (E2)
	} else {
		// armis:ignore cwe:253 reason:transcript write; return value not actionable
		_, _ = fmt.Fprintf(w, "%s%s\n", styles.SpinnerText.Render("✓ "+completeText),
			styles.MutedText.Render("  "+d.TotalStr+" total"))
	}

	// Staggered dot-prefix reveal of the top findings (E2+E3).
	sorted := make([]model.Finding, len(d.Findings))
	copy(sorted, d.Findings)
	sort.SliceStable(sorted, func(i, j int) bool {
		return severityRank[sorted[i].Severity] < severityRank[sorted[j].Severity]
	})
	shown := len(sorted)
	if shown > revealCap {
		shown = revealCap
	}
	// armis:ignore cwe:253 reason:transcript write; return value not actionable
	_, _ = fmt.Fprintln(w)
	for i := 0; i < shown; i++ {
		f := sorted[i]
		sev := styles.GetSeverityText(f.Severity)
		// armis:ignore cwe:253 reason:transcript write; return value not actionable
		_, _ = fmt.Fprintf(w, "%s%s %s\n", sev.Render("● "),
			sev.Render(fmt.Sprintf("%-9s", strings.ToUpper(string(f.Severity)))),
			styles.Bold.Render(f.Title))
		if loc := findingLocation(f); loc != "" {
			// armis:ignore cwe:253 reason:transcript write; return value not actionable
			_, _ = fmt.Fprintf(w, "            %s\n", styles.MutedText.Render(loc))
		}
		if tty && i < shown-1 {
			time.Sleep(700 * time.Millisecond)
		}
	}
	if rest := len(sorted) - shown; rest > 0 {
		// armis:ignore cwe:253 reason:transcript write; return value not actionable
		_, _ = fmt.Fprintf(w, "%s\n", styles.MutedText.Render(fmt.Sprintf("… and %d more below", rest)))
	}

	// Severity count-up summary (F3).
	// armis:ignore cwe:253 reason:transcript write; return value not actionable
	_, _ = fmt.Fprintln(w)
	if tty {
		const steps = 12
		for s := 1; s <= steps; s++ {
			// armis:ignore cwe:253 reason:animation frame write; return value not actionable
			_, _ = fmt.Fprintf(w, "\r\033[K%s", severitySummaryLine(styles, d.BySeverity, float64(s)/steps))
			time.Sleep(60 * time.Millisecond)
		}
		// armis:ignore cwe:253 reason:transcript write; return value not actionable
		_, _ = fmt.Fprintln(w)
	} else {
		// armis:ignore cwe:253 reason:transcript write; return value not actionable
		_, _ = fmt.Fprintf(w, "%s\n", severitySummaryLine(styles, d.BySeverity, 1))
	}
}

// arrowFrameFinal renders the arrow at rest (post-animation form).
func arrowFrameFinal(styles *output.Styles) string {
	return arrowFrame(styles, arrowTotalFrames)
}

// severitySummaryLine renders "● N critical  ● N high ..." with counts
// scaled by progress (0..1] for the count-up animation.
func severitySummaryLine(styles *output.Styles, bySev map[model.Severity]int, progress float64) string {
	order := []struct {
		sev   model.Severity
		label string
	}{
		{model.SeverityCritical, "critical"},
		{model.SeverityHigh, "high"},
		{model.SeverityMedium, "medium"},
		{model.SeverityLow, "low"},
		{model.SeverityInfo, "info"},
	}
	u := 1 - progress
	eased := 1 - u*u*u
	var parts []string
	for _, o := range order {
		n := bySev[o.sev]
		if n == 0 {
			continue
		}
		shown := int(math.Ceil(float64(n) * eased))
		if shown > n {
			shown = n
		}
		st := styles.GetSeverityText(o.sev)
		parts = append(parts, st.Render(fmt.Sprintf("● %d %s", shown, o.label)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "   ")
}

// findingLocation summarizes where a finding lives, for the reveal line.
func findingLocation(f model.Finding) string {
	switch {
	case f.File != "" && f.StartLine > 0:
		return fmt.Sprintf("%s:%d", f.File, f.StartLine)
	case f.File != "":
		return f.File
	case f.Package != "" && f.Version != "":
		return f.Package + "@" + f.Version
	case f.Package != "":
		return f.Package
	default:
		return ""
	}
}
