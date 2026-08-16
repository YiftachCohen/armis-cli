# Scan Events — Backend Requirements (CLI ask)

Status: **proposal, ready to be turned into a platform ticket**
Requested by: Armis CLI (`armis scan repo` / `armis scan image`)
Motivation: `docs/design/scan-experience-spec.md` §6.1 and §6.2
Audience: Project-Moose / ingest platform team

---

## 0. TL;DR — two asks

| # | Ask | Unblocks | Size on our side |
|---|---|---|---|
| **A** | A per-scan **events + counters** feed for a scan in flight (proposed: `include_events` on the existing status poll, §3) | Spec features B9 (narrative verbs), C2 (telemetry row), C4 (activity feed) | ~1 day, all presentation |
| **B** | The **Armis Cloud console deep-link URL pattern** for a scan ID, per region (§6) | Spec feature G1 (clickable scan ID) | ~3 lines |

Ask B is a question, not a build. It can be answered in a Slack reply today.

Ask A is the only thing standing between the redesigned scan experience and its
most-wanted moment: the 30-second-to-10-minute analysis wait, during which the
CLI currently has *nothing true to say*.

---

## 1. What the CLI can see today

This section is the evidence base. Everything below was read out of the current
`internal/api` / `internal/model` code, not assumed.

### 1.1 The scan lifecycle as the CLI experiences it

```
POST /api/v1/ingest/presigned-url   → scan_id reserved                (instant)
POST <s3 presigned>                 → tarball uploaded                (seconds; local byte accounting possible)
POST /api/v1/ingest/scan            → PENDING_UPLOAD → INITIATED      (instant)
GET  /api/v1/ingest/status/         → polled every 5s                 ← THE BLIND WINDOW
GET  /api/v1/ingest/normalized-results (cursor-paged)                 (only after COMPLETED)
```

The blind window is the whole point of this document. It is minutes long, and
the CLI's only input during it is a 5-second poll (`repo.Scanner.pollInterval`,
default `5 * time.Second`) of one endpoint.

### 1.2 Everything the poll returns

`model.IngestStatusData` — the complete payload the CLI receives per poll:

```go
type IngestStatusData struct {
    ArtifactType   string  // "repo" | "image"   — known before the scan starts
    CompletedAt    *string // set only at the end
    ExpirationTime string
    FileBytes      int64   // the tarball we ourselves uploaded
    FileName       string  // the tarball we ourselves named
    LastError      *string // set only on FAILED
    ScanID         string
    ScanStatus     string  // ← the only field that changes mid-scan
    ScanType       string
    StartedAt      string
    TenantID       string
    UpdatedAt      string  // ← see open question Q1
}
```

Of twelve fields, **eleven are either constant for the whole scan or set only at
its end**. `ScanStatus` is the sole progress signal.

### 1.3 The entire honest vocabulary

`internal/scan/status.go::FormatScanStatus` maps `ScanStatus` to text. The
switch has five arms:

| `ScanStatus` | Rendered today |
|---|---|
| `INITIATED` | "Scan initiated, preparing analysis..." |
| `IN_PROGRESS` | caller-supplied, e.g. "Scanning for security issues..." |
| `COMPLETED` | "Scan completed, preparing results..." |
| `FAILED` | "Scan encountered an error" |
| `STOPPED` | "Scan was stopped" |

Three of those five are terminal. So for a scan that behaves, the CLI has
**exactly two** distinguishable mid-flight states, and one of them
(`INITIATED`) usually lasts a second or two.

A four-minute analysis therefore has one honest message for its entire
duration. Design rule #2 of the spec is "Truth first — every number and string
on screen must come from real data", which means we cannot cover that window
with invented verbs. That constraint is what this ask exists to relieve.

### 1.4 Findings are all-or-nothing

`GET /api/v1/ingest/normalized-results` is cursor-paginated
(`model.Pagination{NextCursor *string, Limit int}`) but is only meaningful after
`COMPLETED`. There is no partial-results read. So the CLI cannot say "3 findings
so far" — it goes from 0 knowledge to the full set in one step.

---

## 2. What each blocked feature needs, and what it would render

### B9 — Narrative verbs (`Needs backend`)

**Renders:** the live status line's message swaps as the pipeline moves, instead
of holding one string for minutes.

```
⠹ Resolving dependencies                      [00:42]
⠸ Checking packages against advisories        [01:07]
⠼ Scanning for secrets                        [01:58]
⠴ Validating reachability                     [02:31]
```

**Needs:** a coarse, ordered **phase** signal with a stable enum — perhaps 4–8
values for the whole pipeline. This is the smallest possible version of this
ask, and on its own it delivers most of the perceived value. If the platform
team can ship only one thing, **ship the phase signal.**

Critically: an enum, *not* prose. The CLI owns wording, styling, truncation and
color. A backend that sends `"Scanning for secrets..."` has taken over CLI
presentation and we would have to re-map it back to an enum anyway to style it.

### C2 — Telemetry row (`Partial`)

**Renders:** one line beneath the analysis status, with the changed value
flashing accent:

```
⠹ Analyzing repository                        [01:12]
  1,204 files · 87 packages · 3 findings
```

**Needs:** live **counters**. `files` we know locally from the tar walk;
`packages` and `findings` we do not — and the findings count is the number a
user actually leans in for. Counters must be **exact**, not derived by the CLI
summing a sampled event stream (see §3.4 — this is why the proposal separates
counters from events).

### C4 — Activity feed (`Blocked`)

**Renders:** up to four item lines under the analysis status, newest at the
bottom in bright text, aging upward to dim, replaced in place:

```
⠼ Analyzing repository                        [02:03]
  ✓ requests 2.31.0 — no known advisories
  ✓ urllib3 2.0.7 — no known advisories
  ! django 3.2.18 — 2 advisories
  ✓ internal/api/client.go — no secrets
```

**Needs:** an **event tail** — individual, timestamped, orderable items with
enough structure to style per kind. This is the feature the `{ts, kind, label}`
shape in spec §6.1 was aimed at, and it is the one where that shape needs the
most revision (§3.3).

---

## 3. Proposed contract

### 3.1 Shape: polled, not streamed — and folded into the existing poll

**Recommendation: extend `GET /api/v1/ingest/status/` with two optional query
parameters rather than adding a streamed endpoint.**

```
GET /api/v1/ingest/status/?tenant_id=<t>&scan_id=<s>
    &include_events=true          # opt-in; absent ⇒ byte-identical response to today
    &events_cursor=<seq>          # high-water mark the client has already rendered
    &events_limit=20              # optional, server caps
```

Justification — polled over SSE:

1. **The poll loop already exists** and is hardened: `WaitForIngest` runs a
   5-second ticker with a timeout context, and the underlying
   `internal/httpclient` retries 5xx with exponential backoff. SSE would need a
   *third* HTTP client (the general client has a 60s timeout that a long-lived
   stream would trip; the upload client has none but no retry), plus reconnect
   logic, `Last-Event-ID` resumption, and heartbeat handling. That is real
   machinery to maintain for a ≤4-line decorative feed.
2. **Corporate proxies buffer SSE.** This CLI runs behind enterprise egress
   proxies by design; a buffered stream degrades to *worse* than polling
   (bursty, late) while looking like it works.
3. **Degradation is trivial when polled.** A 404/400 on one poll disables the
   feature and the scan continues untouched. A dead stream is ambiguous — is
   the scan stalled or the connection?
4. **5-second granularity is enough.** The feed replaces ≤4 lines and the
   spinner reads as alive on its own. Sub-second event latency buys nothing.

Justification — folded into `status/` rather than a sibling endpoint:

- **Atomic consistency.** Status and events come from one read, so the CLI can
  never render an event from a phase the status hasn't reached, or show
  `COMPLETED` while four "checking packages" lines sit above it. Two endpoints
  on one ticker interleave and we would have to reconcile.
- **Half the requests.** The poll is every 5s for the life of the scan across
  every CI job running `armis scan`. Doubling that request volume for
  decoration is a poor trade.
- **One auth path, one retry path, one timeout budget.**

**Acceptable alternative** if the team prefers separation of concerns: a sibling
`GET /api/v1/ingest/events` with the same schema, which the CLI calls on the
same ticker. We would accept the interleaving and clamp events to the last-seen
status. State the preference in the ticket; we will build to either.

**Not acceptable:** making `include_events` the default, or changing the
existing response shape. Older CLIs must keep working byte-for-byte.

### 3.2 Response schema

Additive block on the existing response. Everything under `events` is new;
`data` is untouched.

```jsonc
{
  "data": [ { /* ...existing IngestStatusData, unchanged... */ } ],

  "events": {
    "phase": "DEPENDENCY_ANALYSIS",   // current coarse phase (enum, §3.3)
    "phase_index": 3,                 // 1-based, for "step 3 of 6"
    "phase_count": 6,                 // total phases for THIS scan's pipeline

    "counters": {                     // EXACT, monotonic, cumulative (§3.4)
      "files_scanned":    1204,
      "packages_scanned": 87,
      "findings":         3,
      "findings_by_severity": { "critical": 0, "high": 1, "medium": 2, "low": 0 }
    },

    "items": [                        // bounded, may be sampled (§3.4)
      {
        "seq":  418,                  // monotonic per scan_id — cursor + dedup key
        "ts":   "2026-08-16T09:14:02.117Z",
        "kind": "package_checked",
        "subject": "django",          // ≤128 chars, safe to render (§4.1)
        "detail": "3.2.18",           // ≤128 chars, optional
        "outcome": "flagged",         // "ok" | "flagged" | "skipped" | "error"
        "severity": "high"            // optional; only when outcome=flagged
      }
    ],

    "next_cursor": 418,               // pass back as events_cursor
    "dropped": 0,                     // items suppressed by sampling since last cursor
    "complete": false                 // true ⇒ no further events for this scan
  }
}
```

### 3.3 Event kinds — validating and revising the spec's proposal

Spec §6.1 proposes `{ts, kind, label}` with
`kind ∈ {phase, package_checked, secret_candidate, taint_path, finding}`.

**What is right:** the *kind set* is well chosen. It maps to the pipeline stages
the CLI already knows exist from `internal/scan/finding_type.go`
(VULNERABILITY / SCA / SECRET / MISCONFIG / LICENSE) and it is small and closed.
Keep it, with one addition and one removal.

**What needs to change — three revisions:**

**(1) `label` → structured fields.** A pre-rendered human string is the wrong
primitive, for four reasons:

- *It hands CLI presentation to the backend.* We style by outcome and severity
  (`SuccessText` green ✓, severity dots, `MutedText` for aging lines) and
  truncate to terminal width with `…`. Given `"django 3.2.18 — 2 advisories"` we
  would have to parse it back apart to do any of that. Given
  `{subject, detail, outcome, severity}` we compose it.
- *It cannot be counted or deduped* except by string equality.
- *It is the natural place for a secret to leak.* A field called `label` on a
  `secret_candidate` event invites `"AWS key AKIA... in config.py"`. Structured
  fields with a documented contract (§4.1) make the safe thing the default.
- *It is unbounded.* `subject`/`detail` with explicit ≤128-char caps are
  enforceable at the edge.

**(2) `phase` should not be an event kind — it should be response state.** As an
event, a phase transition is something the CLI can *miss*: if a poll's page is
full, or an event is sampled away, or the CLI starts mid-scan, the current phase
is unknown until the next transition — which may be minutes out. As a top-level
field it is always correct and is self-healing after any gap. Phases are state,
not occurrences. Hence `events.phase` in §3.2 and no `phase` kind.

**(3) `ts` is insufficient as a cursor — add `seq`.** Timestamps collide at
millisecond resolution under parallel workers, and clock skew between workers
can produce non-monotonic ordering. A `seq` monotonic per `scan_id` gives an
unambiguous cursor, a stable dedup key, and gap detection for free. Keep `ts`
for display and diagnostics; cursor on `seq`.

**Resulting kind enum:**

| `kind` | `subject` | `detail` | Emitted when |
|---|---|---|---|
| `package_checked` | package name | version | an SCA lookup resolves |
| `secret_candidate` | **file path only** | rule name, e.g. `aws-access-key` | a secret rule matches — **never the matched value** (§4.1) |
| `taint_path` | sink file path | rule/CWE id | reachability analysis resolves a path |
| `finding` | short finding title | category | a finding is confirmed |
| `file_scanned` | file path | — | *(new, optional)* a source file completes analysis — the highest-volume kind; sample aggressively or omit |

CLI is forward-compatible: **unknown `kind` values are dropped silently, not
rendered.** Adding a kind later is not a breaking change and needs no CLI
release.

### 3.4 Why counters and items are separate — the truth-first constraint

This is the single most important structural point in the proposal.

The activity feed (C4) renders ≤4 lines and is decorative: it may be sampled,
throttled, or lossy with no harm done. The telemetry row (C2) shows
**numbers a user will believe and act on**, and spec design rule #2 forbids
anything on screen that isn't real.

If the CLI derived counts by summing a sampled event stream, every dropped event
would silently produce a wrong number — "2 findings" when there are 5. That is
exactly the fabrication the design rule prohibits, and it would fail silently
and unfalsifiably in the field.

So: **`counters` are server-computed, exact, cumulative and monotonic** — cheap
for the backend (they exist in the pipeline already) and never reconstructed
client-side. **`items` are a best-effort sampled tail**, and `dropped` tells us
when sampling occurred so we can *avoid* implying the feed is exhaustive. Each
side gets the guarantee it actually needs.

### 3.5 Ordering, dedup and pagination semantics

Requested guarantees, in priority order:

1. **`seq` is strictly increasing per `scan_id`.** Never reused, never
   renumbered.
2. **No backfill below the high-water mark.** A response with
   `next_cursor: 418` must not later yield an event with `seq: 400`. If the
   backend cannot promise this (e.g. parallel workers publishing out of band),
   say so — the CLI will simply drop `seq <= last_seen`, but then `dropped`
   becomes unreliable and we will not render exhaustiveness claims.
3. **At-least-once is fine; at-most-once is not required.** Because `seq` is
   stable, duplicates are free for us to discard. Do not add delivery machinery
   to prevent them.
4. **`events_cursor` is exclusive** — return events with `seq > cursor`.
   Omitted cursor ⇒ return the most recent page (**not** the oldest — the CLI
   renders a tail, and a scan that has run for four minutes should not replay
   from the beginning).
5. **`complete: true` is terminal** and must accompany or precede the terminal
   `ScanStatus`. After it, the CLI stops requesting events.
6. **Idempotent GET.** `internal/httpclient` retries 5xx automatically; a retried
   poll must not advance server-side state or consume events.

### 3.6 Interaction with the existing poll loop

- The CLI sends `include_events=true` only when it intends to render (TTY, not
  CI, feature enabled). CI and `--no-progress` runs will never ask for events —
  so the added backend load is **zero for the CI-heavy majority of traffic**.
- Feature detection is **once per scan, not per poll**: the first poll requests
  events; if the response has no `events` key, or returns 400/404/501, the CLI
  disables the feature for the rest of that scan and never asks again.
- Events must never affect scan control flow. `WaitForIngest` decides purely on
  `ScanStatus`; the events block is read-only decoration and a malformed or
  missing one must not fail the scan (§4.3).
- Optional but welcome: a `poll_after_ms` hint in the response so the backend
  can slow us down under load. We currently hard-code 5s.

---

## 4. Non-negotiables from the CLI side

These are the conditions under which we can ship this at all. Each one is a
property of the *terminal* as an output device, not a preference.

### 4.1 Events must be safe to render verbatim

Terminal output is not HTML — it is a command channel. A string printed to a
terminal can retitle the window, move the cursor, or clear the screen. So:

- **No secret values, ever.** A `secret_candidate` event carries the file path
  and the rule name. It must **not** carry the matched string, the surrounding
  line, or a snippet. There is no redaction level that makes shipping the value
  worthwhile — the CLI only needs to say "found something in this file".
  (For defense in depth the CLI will run `internal/util.MaskSecretInLine` over
  every rendered event field regardless. That is a backstop, not permission.)
- **No customer source code.** Snippets belong in the final findings payload,
  which is already masked and reviewed. Not in a live feed.
- **Printable, non-control text only.** `subject` and `detail` must contain no
  ESC (`0x1B`), no C0/C1 control characters, no ANSI/OSC sequences. The CLI will
  strip them anyway, but a backend that forwards a filename from a hostile repo
  unfiltered is an injection vector into every user's terminal *and* into CI
  logs. Filenames come from user-controlled tarballs, so this is a live path,
  not a theoretical one.
- **Bounded length:** ≤128 chars per field, enforced server-side. The CLI
  truncates to terminal width regardless (`truncateMessage`).

### 4.2 Volume must be bounded

- Page size capped server-side (we request ≤20; the display shows 4).
- The `events` block should stay under ~8 KB. For scale: this is fetched every
  5 s for the life of every interactive scan.
- `file_scanned` on a 50,000-file monorepo must not attempt to be exhaustive.
  Sample it and report `dropped`.

### 4.3 It must degrade, silently, in three directions

1. **Old backend, new CLI:** `include_events` unrecognized ⇒ the parameter is
   ignored and the response is today's response. The CLI sees no `events` key,
   disables the feature, and shows exactly today's experience. **No warning, no
   error, no visible difference.** Many customers run pinned backend versions.
2. **New backend, old CLI:** the additive `events` key is ignored by
   `encoding/json` unmarshalling into the existing struct. Already safe — do not
   change `data`'s shape to accommodate events.
3. **Events broken at runtime** (500s, malformed JSON, absurd values): the scan
   proceeds to completion normally. An events failure must never fail, stall, or
   slow a scan. We will treat any events error as "feature off from here".

### 4.4 Forward compatibility

Unknown `kind`, `outcome`, `severity` or `phase` values must be safe to add
without a CLI release. The CLI drops unknown `kind`s and falls back to neutral
styling for unknown `outcome`/`severity`. Please do not reuse an existing value
with new meaning — that we cannot defend against.

### 4.5 Scoping and latency

- Scoped by `tenant_id` + `scan_id` exactly like the existing status call; same
  authz path, no new surface.
- Adding events must not slow the status poll. Target p95 < 500 ms for the
  combined response. If exact counters are expensive to compute per poll, a
  cached value refreshed every ~2 s is fine — say so and we will not imply
  the numbers are instantaneous.

---

## 5. What we can ship with **no** backend change

Deliberately explicit, so this ticket does not become a blocker for the redesign.
The following are all local or already-available data, and are proceeding now:

| Feature | Source | Notes |
|---|---|---|
| **D3 receipts** (`✓ Repository packaged  (2.4s · 42.3 MB)`) | Client-side timings | Shipping in this track. |
| **Phase API + live region** (spec §4) | Local | Shipping in this track. |
| **C1 packaging counters** (`1,204 files · 42.3 MB`) | Tar walk callback — local | Note: must be added to **both** `tarGzDirectory` *and* `tarGzFiles`; `--include-files` uses the latter. |
| **C3 packaging stream** | Same tar callback | Fully honest, fully local. |
| **Upload byte counter** | Local byte accounting | ⚠️ Spec §2/§5-C1 claims `progress.NewReader`/`NewWriter` already track this. They are **dead code** — no scan path calls them; `repo.go`/`image.go` pass a bare `*os.File` to `StartIngest`. This needs wiring, not reuse. |
| **B9-lite** | Real `FormatScanStatus` transitions | Honest but sparse: ~2 mid-flight messages, one of them momentary. This is the degraded fallback and it is what the ask above exists to improve. |
| **Elapsed timer, `[mm:ss]`** | Local | Already exists. |
| **E2/E3 findings reveal, F2 all-clear, F3 count-up** | `model.Finding` — complete | Post-completion; needs nothing. |
| **G2 tab title, G3 taskbar, G5 notification** | Local | OSC sequences, TTY-gated. |

C2's telemetry row ships **partially**: `files · MB` during packaging and upload
(local, exact), and is simply **absent during analysis** rather than showing a
frozen or invented number. C4 does not ship at all. Both become real with Ask A.

---

## 6. Ask B — Armis Cloud console deep-link (spec §6.2, unblocks G1)

The CLI wants to wrap the printed scan ID in an OSC 8 hyperlink so the user can
click through to the scan in the console. TTY-only; plain text everywhere else.
The code is three lines. **We just need the URL pattern.**

Specifically:

1. **The template.** Something of the form
   `https://<console-host>/<path>?scan_id=<scan_id>` — we need the exact path
   and which parameters are required.
2. **Does it need the tenant?** The CLI has `tenant_id` (from the `customer_id`
   JWT claim under JWT auth, or `ARMIS_TENANT_ID` under Basic). If the console
   URL needs a tenant *slug* rather than the ID, we cannot build it — tell us
   and we will drop G1 rather than guess.
3. **The per-region host mapping.** The CLI already routes the API per region
   (`internal/auth/client.go::RegionalBaseURL`):

   | Region | API host | Console host? |
   |---|---|---|
   | `us1` (primary) | `https://moose.armis.com` | **?** |
   | `eu1` | `https://eu.moose.armis.com` | **?** |
   | `--dev` | `https://moose-dev.armis.com` | **?** |

   If the console host is derivable from the API host by a simple rule, that
   rule is the whole answer.
4. **Is the link durable?** Does the scan remain viewable after
   `expiration_time` passes? If links rot, we would rather not print one.

If the answer is "there is no per-scan console view yet", that is a perfectly
good answer — G1 comes off the board and no one spends time on it.

---

## 7. Open questions for the platform team

| # | Question | Why it matters |
|---|---|---|
| **Q1** | Does `IngestStatusData.UpdatedAt` advance *during* `IN_PROGRESS`, or only on state transitions? | If it advances per internal pipeline step, it is a heartbeat we already receive — worth a partial B9 with **zero** API change. Cheapest possible win; please answer this one first. |
| **Q2** | The scan dispatches to **Prefect or SQS** (per `CLAUDE.md`). For the Prefect path, task-run state changes already exist as structured events. Is a read-only projection of those the cheapest implementation of §3.2? | May turn "build an events system" into "expose an existing one". |
| **Q3** | If SQS-dispatched scans have no equivalent event source, will `events` be absent for them? | Fine by us — the CLI degrades per §4.3 — but we need to know it is *expected* absence, not a bug, so we do not chase it. |
| **Q4** | Are `phase` values stable across `artifact_type` (`repo` vs `image`)? | `phase_count` differs per pipeline; the CLI renders "step N of M" and must not show a repo-shaped progression for an image scan. |
| **Q5** | Is there any per-scan cost/quota concern with a 5-second poll carrying counters? | Determines whether we need `poll_after_ms` (§3.6) in v1 or can defer it. |

---

## 8. Minimum viable version

If the full proposal is too large for one cycle, this is the priority order.
Each step is independently shippable and independently useful:

1. **`events.phase` + `phase_index`/`phase_count` only.** No items, no counters.
   Unblocks B9 — the highest-value feature — and is likely a small projection of
   existing pipeline state. **If only one thing ships, ship this.**
2. **`events.counters`.** Unblocks C2 with exact, defensible numbers.
3. **`events.items` + cursor.** Unblocks C4, the most expensive and least
   essential piece.

The CLI will implement each behind its own capability check, so partial delivery
is genuinely useful and never a half-broken screen — a missing block simply
means that feature stays off.

---

## Appendix A — Go types the CLI would add

For estimation. These land in `internal/model/` alongside `IngestStatusData`.

```go
// ScanEvents is the optional live-progress block on the ingest status response.
// Absent on backends that predate the events feed — see §4.3.
type ScanEvents struct {
    Phase      string      `json:"phase"`
    PhaseIndex int         `json:"phase_index"`
    PhaseCount int         `json:"phase_count"`
    Counters   ScanCounters `json:"counters"`
    Items      []ScanEvent `json:"items"`
    NextCursor int64       `json:"next_cursor"`
    Dropped    int         `json:"dropped"`
    Complete   bool        `json:"complete"`
}

type ScanCounters struct {
    FilesScanned       int            `json:"files_scanned"`
    PackagesScanned    int            `json:"packages_scanned"`
    Findings           int            `json:"findings"`
    FindingsBySeverity map[string]int `json:"findings_by_severity"`
}

type ScanEvent struct {
    Seq      int64  `json:"seq"`
    TS       string `json:"ts"`
    Kind     string `json:"kind"`
    Subject  string `json:"subject"`
    Detail   string `json:"detail,omitempty"`
    Outcome  string `json:"outcome,omitempty"`
    Severity string `json:"severity,omitempty"`
}

// On IngestStatusResponse, purely additive:
//   Events *ScanEvents `json:"events,omitempty"`
```

## Appendix B — CLI-side acceptance tests for this contract

So the platform team knows what we will assert against a mock:

1. Response with no `events` key ⇒ identical output to today; no warning.
2. `events` present, `items` empty ⇒ phase + counters render; feed region absent.
3. `seq` gap (backfill) ⇒ out-of-order events dropped, no panic, no duplicate line.
4. `subject` containing `\x1b]0;pwn\x07` ⇒ escape stripped; terminal title
   unchanged. (Adversarial: a filename from a hostile repo.)
5. `kind: "some_future_kind"` ⇒ dropped silently; other items still render.
6. 500 on the events-bearing poll ⇒ scan completes normally, feature disabled.
7. `dropped > 0` ⇒ no UI text claims the feed is exhaustive.
8. `--color=never`, CI, non-TTY ⇒ `include_events` never sent at all.
