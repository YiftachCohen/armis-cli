# Scan Experience — Implementation Spec

Status: **set 3 implemented on this branch** (`internal/progress`: `phase.go`,
`arrow.go`, `osc.go`, `format.go`; call sites in `internal/scan/{repo,image}`).
Remaining: C4/C2's honest analysis feed awaits the backend events endpoint (§6.1);
G1 activates via `ARMIS_CONSOLE_URL` once the platform confirms the pattern (§6.2).
Branch: `claude/cli-loader-animation-8ohucn`
Prototypes (living spec — the visual source of truth):
- **`docs/design/scan-experience-prototype.html`** — in-repo, self-contained; open in
  any browser. Plays the full two-act experience as three preset "sets" with per-effect
  toggles, plus every piece animating in isolation. The JS renderers are labeled with
  the same codes as §5 (A1, B5, C1, …) and are the executable definition of each
  effect's glyphs, colors, and timing — port from them directly.
- Hosted copies (same content, may be inaccessible to headless agents):
  set player https://claude.ai/code/artifact/f614c285-b6e4-49be-8a03-eedc2f869f4b ·
  full 45-variant catalog https://claude.ai/code/artifact/0e0dce38-bfa4-4b7a-9f30-97e314d14fbc

This document gives an implementing agent everything needed to build the redesigned
`armis scan` terminal experience in Go. Read it together with `CLAUDE.md`.

---

## 1. Design philosophy (how decisions were made)

The design converged through many rejected iterations. The rules that survived:

1. **Forward motion over loops.** The transcript accumulates like a well-kept log.
   Status lines print and *stay* (CI-log style); receipts append beneath them.
   In-place animation is allowed only for the *live* (bottom-most) region and for
   brief one-shots — never ambient loops that outlive their moment.
2. **Truth first.** Every number and string on screen must come from real data.
   Where a desired effect needs data the backend doesn't provide yet (see §6),
   the feature ships degraded or waits — it is never faked.
3. **Reactive endings.** The experience ends differently depending on what was
   found. Findings get gravity (staggered reveal, no brand moment). A clean scan
   gets the reward (the Armis arrow one-shot + green all-clear).
4. **stdout stays pure.** All of this renders to stderr. Scan results/JSON on
   stdout are untouched. (Existing convention — see CLAUDE.md.)
5. **Identical-ish everywhere.** CI/non-TTY gets the same *lines* without
   animation: statuses print once, receipts print on completion, findings print
   instantly. Only pacing and redraws are TTY-gated.

## 2. Current code — what exists and what to remove

| Area | File(s) | State |
|---|---|---|
| Spinner (classic braille frames, goroutine lifecycle, ctx/timeout, cursor hide, CI detection) | `internal/progress/progress.go` | **Keep the machinery.** Lifecycle (Start/Stop/Update, stopOnce, doneChan, timeout safety net, TTY detection) is solid and tested. |
| Wave + shimmer renderer (5-cell braille wave, shimmer sweep, width truncation) | `internal/progress/progress.go` (commit `7c21a94`) | **Remove/supersede.** The final design uses the classic single-glyph spinner (A1), not the wave. Keep `truncateMessage`/`maxMessageRunes` width handling — reuse it. |
| Wave/shimmer styles (`SpinnerWave`, `SpinnerShimmerCore/Edge`, wave color vars) | `internal/output/styles.go` | Remove with the wave, or leave dormant. Keep `SpinnerChar`, `SpinnerText`, `SpinnerTimer`. |
| Scanner call sites | `internal/scan/repo/repo.go` (~lines 159–285), `internal/scan/image/image.go` (~lines 155–218) | Rework to the new phase API (§4). Current flow: spinner per phase → `Stop()` → ad-hoc `fmt.Fprintf` lines. |
| Backend statuses | `internal/scan/status.go` `FormatScanStatus` | Only 5 statuses exist: `INITIATED`, `IN_PROGRESS`, `COMPLETED`, `FAILED`, `STOPPED`. This is the entire honest vocabulary for the analysis wait. |
| Findings/output | `internal/output/` (human formatter, severity styles/badges) | Findings data (severity, location, fix — `model.Finding`, 23 fields) is complete. Reveal pacing (§5 E2/E3) wraps what already prints. |
| Color/TTY gates | `internal/cli` (`ColorsEnabled`), `progress.IsCI`, `isTerminalWriter` | Reuse as the gates for all animation. |
| Upload progress | `progress.NewReader`/`NewWriter` (progressbar/v3) | Source of truth for uploaded bytes (C1 upload counter). Replace the visual with C1's counter line; keep byte accounting. |

## 3. The composition model

The prototype defines **core features** (always on) and **toggleable features**
(user is still finalizing; implement each independently guarded so the final
chip selection maps to config). Presets ("sets") are just bundles of toggles —
do not hard-code them.

Suggested config surface (order of preference): hard-code the final selection as
defaults; expose overrides via env `ARMIS_UX_*` or a single `--plain` escape
hatch. Do NOT build a full config system for this.

## 4. New progress API (internal/progress)

Replace per-call-site spinner juggling with a phase-oriented renderer:

```go
// Phase is one unit of the scan transcript.
p := progress.StartPhase(ctx, "Packaging repository", opts...)
p.SetDetail("1,204 files · 42.3 MB")   // C1 counter text, re-rendered in place
p.StreamLine("internal/api/client.go") // C3/C4: pushes into the live sub-region
p.SetMessage("Scanning for secrets")   // B9/status change: swaps live message
p.Succeed("Repository packaged", "2.4s · 42.3 MB · 1,204 files") // D3 receipt
// or p.Fail(...)
```

Rendering contract:
- **Live region** = the phase's status line + up to N stream lines below it
  (N=2 packaging stream, N=4 feed). Redraw with `\r` (single line) or
  `ESC[nA` cursor-up (multi-line). Hide cursor while a live region is active
  (existing `cursorHide/Show` code).
- **On Succeed:** erase live region, print the *muted* status line
  (append-memory, D3), then the receipt line `✓ <label>  (<extra>)` —
  both plain prints that scroll away naturally.
- **Non-TTY/CI:** `StartPhase` prints the status once; `Succeed` prints the
  receipt; everything else is a no-op. This preserves today's CI output shape.
- Timer `[mm:ss]` on the live line only (existing `formatDuration`).
- Keep the 30-min safety timeout and context cancellation semantics.
- **Live-region discipline (correctness-critical):** cursor-up arithmetic breaks
  if any live line soft-wraps, so every live line MUST be width-truncated
  (reuse the existing `maxMessageRunes`/`truncateMessage` logic) — and the
  renderer must be the *only* writer to stderr while a region is active. Route
  `cli.PrintWarning`-style messages through the renderer (erase region → print
  warning → repaint region) or queue them until the phase ends.

## 5. Feature specs

Colors below refer to `internal/output/styles.go` tokens (Tailwind palette,
AdaptiveColor light/dark). "Ready" = implementable now with local/existing data.

**Normative rule:** where this document, the current Go code, and
`scan-experience-prototype.html` disagree on any visual detail (glyphs, hex
values, frame timing, easing), **the prototype wins** — its renderers are the
approved look. Do not assume an existing Go implementation of a similarly-named
effect already matches; diff it against the prototype's numbers first.

### Core (always on)

| Code | Feature | Spec | Data | Status |
|---|---|---|---|---|
| **C1** | Live counter | Status line detail: packaging `— <files> files · <MB> MB` ticking from the tar walk; upload `— <sent> / <total> MB` from the progress reader. | Tar walk needs a per-file callback added to `tarGzDirectory` (count + cumulative size). Upload bytes already tracked. | **Ready** (small plumbing) |
| **D3** | Append trail | On phase end: muted status line stays, `✓ label  (elapsed · extra)` appends. Green ✓ = `SuccessText` style; extra in `MutedText`. | Client-side timings. | **Ready** |
| **E2+E3** | Findings reveal | On TTY with findings: hold ~800ms of stillness after "Results retrieved", then per finding (critical→high→…): print `● SEVERITY  Title` (dot + label in severity color, E3 style) then its detail line, ~1.5s stagger between findings. Full existing formatter output follows. Non-TTY: everything instant. | `model.Finding` — complete. | **Ready** |
| **F2** | All-clear reward | Zero findings: arrow one-shot (below) + `No findings — all clear  <files> · <packages>` in green. | Findings count; files/pkgs from scan summary if available, else omit. | **Ready** |
| **F3** | Severity count-up | Summary line `● N critical  ● N high  ● N medium` counts up over ~800ms (ease-out) on TTY; instant otherwise. | Findings. | **Ready** |
| **G1** | Clickable scan ID | Wrap scan ID (and report URL) in OSC 8: `ESC]8;;<url>ESC\<text>ESC]8;;ESC\`. TTY-only; plain text otherwise. | **Console URL pattern unconfirmed** — ask platform team. Code is 3 lines. | **Blocked (external)** |
| **G2** | Tab title | While scanning: `ESC]0;armis · MM:SSBEL` every second; restore on exit (best effort: emit empty title). TTY-only. | Local. | **Ready** |

### Toggleable (build each behind its own guard)

| Code | Feature | Spec | Status |
|---|---|---|---|
| **A1** | Spinner head | Frames `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, prefix of the live line; off = two-space indent. **Not identical to the legacy spinner — two deliberate deltas** (this is what makes the prototype's version look better): (1) frame period **80ms**, not the legacy 100ms; (2) color is **AdaptiveColor{Light: `#7C3AED`, Dark: `#A78BFA`}** (violet-600 / violet-400) + bold — the legacy `SpinnerChar` uses `#7C3AED` on both themes, which reads heavy on dark backgrounds. Update `SpinnerChar` in `styles.go` accordingly (scan the few other `colorAccent` uses before touching the shared var — safest is a dedicated adaptive color for the spinner). | **Ready** (small delta from existing) |
| **B5** | Breathing ellipsis | Trailing 3 dots cycle 0→3 (~640ms/step): newest dot bright accent, older mid-violet, absent dots dim. Replaces static `...`. | **Ready** |
| **B9** | Narrative verbs | Live message rotates through phase verbs during `IN_PROGRESS`. **Honest version requires backend phase strings** (§6). Interim: ship off, or use only the real `FormatScanStatus` transitions. | **Needs backend / decision** |
| **C3** | Packaging stream | ≤2 lines under the packaging status: most recent files from the tar walk callback; newest bottom (`✓` green + muted text), older dim. | **Ready** (same callback as C1) |
| **C2** | Telemetry row | One line under analysis status: `files · pkgs · findings` counters, changed value flashes accent. files/pkgs real post-packaging; **live findings count needs backend events**. | **Partial — needs backend for the interesting part** |
| **C4** | Activity feed | ≤4 item lines under analysis status, newest bottom-bright, aging to dim; items = real scan events. | **Blocked on backend events endpoint** (§6) |
| **D2** | Trail sparkle | Single `✦` (hue-drifting violet) appended to a receipt for ~800ms after landing. Currently ditched — implement last or never. | Ready, deprioritized |
| **F1** | Arrow at every completion | Arrow one-shot + `Scan complete  <total>s` even when findings exist (mutually exclusive with F2-only philosophy; default off pending decision). | **Ready — needs decision** |
| **G3** | Taskbar progress | OSC 9;4: `ESC]9;4;1;<pct>BEL` during determinate phases (upload), `ESC]9;4;3;0BEL` (indeterminate) during analysis, clear `ESC]9;4;0;0BEL` on exit. **Emit only on positive detection** (`WT_SESSION` or `ConEmuANSI` env): OSC 9 is a namespace collision — iTerm2 historically treats OSC 9 as a desktop notification, and while modern terminals special-case `4;`-prefixed payloads, older ones may pop a garbage notification. Never blind-emit. | **Ready (gated)** |
| **G5** | Desktop notification | On completion if scan ran > ~60s, dispatch by detected terminal: iTerm2 (`TERM_PROGRAM=iTerm.app`) → `ESC]9;<msg>BEL`; kitty → OSC 99; urxvt/foot → `ESC]777;notify;<title>;<body>BEL`. Default **off** for undetected terminals — do not spray unknown OSC at unknown emulators. | **Ready (gated, default off)** |

### The Armis arrow one-shot (used by F1/F2)

Line-size (6 cells × 1 row) braille dot animation, ~1.2s, once:
- Dot targets: the arrow *gesture* — polyline `(0.6,3.2) → (7.3,0.2) → (11.4,2.3)`
  sampled every ~0.45 units, plus a thickening pass (`y<2.5` points duplicated at
  `(+0.5, +0.85)`), on a 12×4 dot grid (2×4 dots per braille cell).
- Each dot: eased assemble from a scattered ring position (deterministic
  pseudo-random by index; stagger by position along the arrow axis), then one
  light-sweep along the axis (frames ~20–32), then rest at base brightness.
- Colors: brightness ramp violet-300→700 (`#C4B5FD`, `#A78BFA`, `#8B5CF6`).
- Full geometry source of truth: `docs/assets/logo-dark.svg` (the `#4E1683`
  path); the avatar-size variant (48×44 grid, exact silhouette with the concave
  circle notch) exists in the prototype JS if ever wanted.
- TTY-only; non-TTY prints the text line instantly.

## 6. External dependencies / asks

1. **Backend scan-events endpoint** (unblocks honest B9/C2/C4): a paginated or
   streamed feed of analysis events per scan: `{ts, kind, label}` where kind ∈
   {phase, package_checked, secret_candidate, taint_path, finding}. Until it
   exists, analysis shows only the real `FormatScanStatus` transitions.
   *File as a platform ticket; the prototype's set 3 is the motivation demo.*
2. **Armis Cloud scan URL pattern** (unblocks G1): confirm the console deep-link
   for a scan/tenant ID.
3. Nothing else requires anyone outside this repo.

## 7. Degradation matrix (acceptance gate — test all of these)

| Environment | Behavior |
|---|---|
| TTY + colors | Full experience per enabled toggles. |
| `--color=never` / `NO_COLOR` | Same lines & pacing, monochrome (styles already degrade via `NoColorStyles`). Spinner/ellipsis still animate (glyphs are not color). |
| CI (`IsCI()`) / non-TTY | Append-only: status once per phase, receipts, findings instant, no redraws, no cursor codes, no OSC. Matches today's disabled-spinner shape. |
| Narrow terminal | Live line truncated with `…` (reuse existing width logic). Stream/feed lines truncated the same way. |
| `scan image` | Same phases; packaging → "Exporting image" (docker/podman export doesn't expose per-file progress — C1 shows elapsed only, C3 off). |

## 8. Suggested implementation order

1. **PR 1 — progress core:** phase API (§4), A1 restored as head, D3 receipts,
   B5 ellipsis, C1 counters + tar-walk callback, call-site rework in
   `repo.go`/`image.go`. Remove wave/shimmer. This is the bulk.
2. **PR 2 — endings:** E2+E3 reveal pacing, F2 all-clear + arrow one-shot, F3
   count-up. Touches `internal/output` + a small `internal/progress/arrow.go`.
3. **PR 3 — ambient:** G2 title, G3 taskbar, G5 notification, G1 (once URL is
   confirmed), each ~20 lines with a shared OSC helper that no-ops off-TTY.
4. **Later / blocked:** C3+C2 (ready but pending final toggle decision),
   C4 + honest B9 (await backend events endpoint).

## 9. Conventions & testing (repo rules)

- Error wrapping `fmt.Errorf("context: %w", err)`; conventional commits;
  golangci-lint v2 clean (`make lint`); all output to stderr.
- Extend `internal/progress/progress_test.go` patterns: table-driven; assert
  no cursor/OSC sequences reach non-TTY writers (existing tests show how);
  goroutine-leak test must keep passing; golden-string tests for receipt and
  reveal lines with `NoColorStyles`.
- Race detector runs on Linux CI — the phase renderer goroutine must pass
  `-race` (see CLAUDE.md gotchas).
- Do not regress the PPSC-895 ingest flow; this work is presentation-only.
