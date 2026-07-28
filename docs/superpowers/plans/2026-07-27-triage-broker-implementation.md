# Triage Broker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make triage produce a *proposed-effects batch* that a controller-side broker deterministically shape-checks and applies under its own bot PAT — so the agent pod holds no forge credentials and posts nothing itself.

**Architecture:** The agent writes `[{comment,body},{label,add}]` JSON to its own work bead (`gonk.effects` metadata key) and posts nothing. `gonk-gate dispatch` creates that pool-routed work bead directly (recording its id on `beadstore.Record.WorkBeadID`), injects context via `template_overrides.initial_message`, and later `gonk-sweep` follows `WorkBeadID` to the batch, validates its shape via a new `pkg/effects`, and applies via the existing `pkg/glab` client. Escalation is cost-class-gated in the meter (free local, gated cloud).

**Tech Stack:** Go (`go test`, `go vet`, staticcheck), the gonk controller (`cmd/gonk-gate`), `pkg/beadstore` (bd/Dolt), `pkg/opercfg`+`pkg/rung`+`pkg/meterapi` (the ladder), `pkg/glab` (GitLab client), the Gas City k8s session provider, Helm chart.

**Spec:** `docs/superpowers/specs/2026-07-27-triage-broker-design.md` — read it before starting. Section refs below (§N) point there.

---

## File Structure

**New:**
- `pkg/effects/effects.go` — `Effect`, `Batch` types + JSON (un)marshal (§5).
- `pkg/effects/shape.go` — `Shape` (per-kind `{min,max}`) + `Validate(batch, shape)` (§6).
- `pkg/effects/shape_load.go` — load `effect-shape.toml` from the baked pack (§6.1).
- `pkg/effects/*_test.go` — table-driven tests for the above.
- `pack/agents/triage/effect-shape.toml` — `comment {1,1}`, `label {0,N}` (§6.1).

**Modified:**
- `pkg/opercfg/opercfg.go:73` (`RungSpec`) — add `MaxTurns`; scope gains a default-off cloud allowance (§7.1, §9).
- `pkg/rung/decide.go` — cost-class escalation rule (§7.1).
- `pkg/beadstore/store.go:42` (`Record`) — add `WorkBeadID string` (§8, §9).
- `cmd/gonk-gate/dispatch.go` — create the pool-routed work bead directly, record `WorkBeadID`, inject via `initial_message` (§4.1, §4.2 step 2).
- `cmd/gonk-gate/sweep.go` — read `WorkBeadID` → `gonk.effects` → validate → apply/escalate/reject (§4.2 steps 4–5, §7).
- `images/agent/entrypoint.sh` + `pack/agents/triage/agent.toml` — agent emits the batch, posts nothing (§9).
- `chart/gonk/templates/workload-gonk-controller.yaml` + agent wiring — drop the agent-pod PAT/CA (§9).

**Design boundaries:** `pkg/effects` is pure and dependency-free (types + validator + loader) — it never touches GitLab or bd, so it is trivially unit-testable and independently landable. Apply (the GitLab write) lives in `sweep.go` using the existing `pkg/glab` client, not in `pkg/effects`.

---

## Phase 0 — S0 gating spike (BLOCKS the return-channel work)

> §12 S0. The 2026-07-26 proof exercised the agent pod *reading/binding* a bead, never *writing* one. Everything from Phase 3 onward assumes a session pod's baked `bd` can write a metadata key the controller reads back from Dolt. Prove it before building on it.

### Task 0.1: Prove pod `bd`→Dolt write round-trip

**Files:** none (manual spike in the live throwaway e2e; record the result in this plan).

- [ ] **Step 1:** In the running e2e namespace, exec into a live triage agent pod and, from inside it, write a metadata key on its own bound work bead:
  `bd update <work-bead-id> --set-metadata gonk.effects='{"probe":"ok"}'`
- [ ] **Step 2:** From the controller pod (`cd /city`), read it back: `bd show <work-bead-id>` and confirm `gonk.effects` shows `{"probe":"ok"}`.
- [ ] **Step 3:** Measure the metadata-value size ceiling: write a ~16 KB value and confirm round-trip (or record the failing size). This fixes §5.1's size question.
- [ ] **Step 4:** Record the outcome in this file under "S0 result." **If write fails:** stop and switch the return channel to the fallback (broker reads via the loopback supervisor API, or agent writes via `gc session submit`-style back-channel) — that changes Phase 4/5, so resolve it here first.

**S0 result (2026-07-27): FAIL as designed → return channel redesigned.** The
agent pod has no working `bd` (`no beads database found`) and no `gc`; the store
topology is split (controller reads local `/city/.beads/dolt`; the pod only has
env for the dolt *server* `gonk-dolt.svc:3306`). So the agent CANNOT write its
batch to a bead from the pod. **Resolution:** the return channel becomes the
agent's **session output**, read controller-side via `gc session peek`/`logs`
(both verified present on the controller). This removes the pod's dependence on
`bd` entirely and is gated, with the inject side, behind the new **S1** channel
spike below. Phases 3/4 are rewritten against S1's outcome — do S1 before them.

### Task 0.2 (S1): Channel spike — inject + return without pod bd

> Supersedes the bd return channel. Gates Phases 3–6. The plan reviewer
> independently flagged that `pkg/beadstore` has no routable-work-bead creation
> API and the pool↔bead binding is undocumented graph.v2 machinery — so BOTH
> directions need proving before building.

- [ ] **Step 1:** From the controller, prove a controller-owned mechanism can
  spawn a triage agent session it can correlate by id AND read its output:
  evaluate `gc session new --template triage` (returns a session id) vs the
  current pool/formula binding. Confirm the controller can deliver a prompt to
  that session (`gc session submit`, or an `initial_message` at create) and read
  the agent's final output back via `gc session peek`/`logs`.
- [ ] **Step 2:** Prove the round-trip end-to-end: deliver a known prompt →
  agent (opencode) emits a known JSON batch as its final output → controller
  reads that exact JSON back. Pin the read command and how the batch is fenced in
  the output (e.g. a sentinel-delimited final block) so parsing is deterministic.
- [ ] **Step 3:** Decide correlation: session-id ↔ `beadstore.Record`
  (`WorkBeadID` becomes `SessionID` if we drive sessions directly, or stays a
  work-bead id if the formula path is kept). Record the decision here.
- [ ] **Step 4:** Record the chosen inject + return mechanism. **Phases 3.2, 4.1,
  4.2, and 5.1 are authored against this result** — do not build them until S1 is
  recorded.

**S1 result (2026-07-27):**
- **Inject + correlation:** `gc session new <template> --json --no-attach` creates a
  controller-owned session and returns its **session id** — that is the
  correlation key (so `beadstore.Record` gains `SessionID`, not a work-bead id).
  This avoids the undocumented pool↔bead graph.v2 binding entirely. Prompt
  delivery to that session: `gc session submit <id> "<prompt>"` (grant-signed, the
  same X-GC-City-Write path `gcapi` already uses for RunOrder) or an initial
  message at create.
- **Return:** `gc session logs <id> --json` reads the session's **structured JSONL
  transcript** (persisted, parseable — `peek` reads only the live tmux pane and is
  NOT used). The agent emits its proposed-effects batch as its final message,
  sentinel-fenced (`GONK_BATCH_START`/`GONK_BATCH_END`), and the controller
  extracts + `effects.ParseBatch`es it. The **pod needs no `bd`/`gc`** — the
  controller reads the transcript. This is the definitive replacement for the
  dead bd return channel (S0).
- **RESIDUAL (fold into Phase 3, task C2a):** live-confirm that opencode's final
  message actually lands in the `gc session logs --json` transcript and the
  fenced batch is extractable, against the pinned opencode version. Narrow,
  well-defined; do it as the first concrete integration step before wiring sweep.
- `gcapi` needs new client methods (`SessionNew`, `SessionSubmit`, and a session
  read) over the same signed API surface as `RunOrder` — add them in Phase 3.

**C2a follow-up (2026-07-27) — the return read is NOT yet pinned; resolve by
build-and-observe, not manual spike.** Live probing found:
- `gc session logs <id>` needs a **session_key** (a manually-created session
  lacked one and it errored "workdir fallback ambiguous"); the on-disk `.jsonl`
  under `/city/.gc/runtime` is the **reconciler trace**, NOT opencode output.
- `gc session list --json` carries a **`last_output`** field per session (empty
  for idle sessions) — a candidate structured read of the agent's final output.
- `gc session peek --json` reads the **live tmux pane** (ephemeral; empty for
  dormant sessions).
None of these could be CONFIRMED to capture opencode's fenced batch, because
producing output requires driving a session with a real prompt via grant-signed
`gc session submit` — which is the C2 inject code itself (chicken-and-egg). So:
**C2 builds the grant-signed `gcapi` session methods first (reusing the exact
X-GC-City-Write signing `RunOrder` already does; find the session API paths in
gascity `internal/api`), then the very first integration run OBSERVES which of
{`last_output`, `gc session logs` with a real session_key, `peek`} actually holds
the batch — and THAT observation pins the read for C4 and the emit format for C5.**
Do not build C5 (agent emit format) until this observation is recorded here.

**Gas City session API paths (from `internal/api/client.go` at GASCITY_REF):**
- **Submit prompt:** `POST /v0/city/{city}/session/{id}/messages` (the SendMessage
  path; grant-signed like RunOrder).
- **Return read:** `GET /v0/city/{city}/session/{id}?peek=true&peekLines=N` →
  `GetSession` returns a `SessionView` including the **last-output preview**. This
  is the clean structured return read — a single authenticated GET, NOT the
  flaky `gc session logs` (which needs a session_key and reads reconciler traces).
  The one thing C2's first run must confirm: `peekLines` large enough to hold the
  whole sentinel-fenced batch (it is a *preview*; size it, or confirm no
  truncation).
- **List/state:** `GET /v0/city/{city}/sessions` (state/template filters).
- **REMAINING C2 UNKNOWN — session create/correlate.** No direct `CreateSession`
  was found in `client.go`; `gc session new` may hit a create route not yet
  located, OR the broker reuses the existing pool/formula spawn and correlates by
  reading `GET /sessions` for the session bound to this dispatch. C2's first task:
  locate the create route in gascity `internal/api` (grep the `session.create`
  handler + the `gc session new` command's client call) OR settle on
  pool-spawn+correlate. This is the last integration unknown; everything else in
  C2 is specified.

**C2 client methods (confirmed in gascity `internal/api/client.go`):**
- Inject/submit: `SubmitSession(id, message string, intent SubmitIntent)` →
  `POST /v0/city/{city}/session/{id}/messages`. gonk's `pkg/gcapi` adds a thin
  wrapper reusing the exact X-GC-City-Write signing it already does for RunOrder.
- Return read: `GetSession(id, peek=true, peekLines)` → `SessionView` with the
  last-output preview. gonk's `pkg/gcapi` adds a `GetSessionOutput` wrapper.
- **OPEN DESIGN QUESTION — create/correlate.** There is NO simple `CreateSession`
  REST call; `gc session new` (cmd_session.go) goes through
  `config.ResolveSessionCreateTransport` (transport resolution), not a plain POST.
  Two viable approaches, decide in C2:
  1. **Reuse pool-spawn + correlate-by-marker (preferred first attempt).** Keep
     the existing supervisor pool spawning the triage session; dispatch stamps a
     UNIQUE marker (e.g. the bead anchor as a session alias / metadata) and finds
     the session via `ListSessions`, recording its id in `Record.SessionID`. No
     new create path; correlation is explicit, not heuristic.
  2. Drive the transport create path directly (heavier; couples gonk to
     `ResolveSessionCreateTransport`).
  Resolve this first in C2, then submit+read are already specified above.

**Return-read observation:** _(fill in from the first C2 integration run:
does GetSession peek hold the full fenced batch at peekLines=N; exact read call)_

---

## Phase 1 — `pkg/effects` (independently landable, pure Go, TDD)

### Task 1.1: Effect and Batch types

**Files:**
- Create: `pkg/effects/effects.go`
- Test: `pkg/effects/effects_test.go`

- [ ] **Step 1: Write the failing test** — a batch round-trips through JSON and rejects unknown kinds.

```go
package effects

import "testing"

func TestBatchUnmarshalKnownKinds(t *testing.T) {
	raw := `{"effects":[{"kind":"comment","body":"hi"},{"kind":"label","add":["gonk::bug"]}]}`
	b, err := ParseBatch([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	if len(b.Effects) != 2 || b.Effects[0].Kind != KindComment || b.Effects[1].Kind != KindLabel {
		t.Fatalf("unexpected batch: %+v", b)
	}
}

func TestBatchRejectsUnknownKind(t *testing.T) {
	if _, err := ParseBatch([]byte(`{"effects":[{"kind":"nuke"}]}`)); err == nil {
		t.Fatal("want error for unknown effect kind")
	}
}

func TestBatchRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseBatch([]byte(`{not json`)); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./pkg/effects/ -run TestBatch -v` → FAIL (undefined).
- [ ] **Step 3: Implement** `pkg/effects/effects.go`:

```go
// Package effects is the proposed-effects contract: the typed batch an agent
// returns instead of making live GitLab writes, plus its shape validator.
// It is pure -- no GitLab, no bd -- so the broker's decision is deterministic
// and reproducible in tests.
package effects

import (
	"encoding/json"
	"fmt"
)

type Kind string

const (
	KindComment Kind = "comment"
	KindLabel   Kind = "label"
	KindNewIssue Kind = "new_issue" // reserved: contract admits it, apply not built (spec N1)
	// code/merge_request/commit are reserved for Phase 5 and deliberately NOT
	// defined here yet -- naming them is a spec-level forward-compat note, and a
	// constant with no validator/apply would invite half-support.
)

var knownKinds = map[Kind]bool{KindComment: true, KindLabel: true, KindNewIssue: true}

// Effect is one intended externally-visible change. Fields are a superset;
// which are meaningful depends on Kind (validated per-kind elsewhere).
type Effect struct {
	Kind   Kind     `json:"kind"`
	Body   string   `json:"body,omitempty"`   // comment
	Add    []string `json:"add,omitempty"`    // label
	Remove []string `json:"remove,omitempty"` // label
	Title  string   `json:"title,omitempty"`  // new_issue
	// TargetIID names an existing resource this effect acts on (0 = the session's
	// primary target, bound from injected context in shape.go).
	TargetIID int64 `json:"target_iid,omitempty"`
}

// Batch is the ordered list a run returns.
type Batch struct {
	Effects []Effect `json:"effects"`
}

// ParseBatch decodes and structurally validates a batch. It rejects invalid
// JSON and unknown effect kinds -- a run that cannot produce a valid batch
// fails; it never half-applies.
func ParseBatch(raw []byte) (Batch, error) {
	var b Batch
	dec := json.NewDecoder(bytes.NewReader(raw)) // import "bytes"
	// DisallowUnknownFields is CORRECT and intentional: Effect is a superset
	// struct, so {"kind":"comment","body":"x"} decodes cleanly and fields like
	// `add`/`title` are known struct fields (never wrongly rejected); a genuinely
	// unknown key IS rejected. Do not remove it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Batch{}, fmt.Errorf("effects: parse batch: %w", err)
	}
	for i, e := range b.Effects {
		if !knownKinds[e.Kind] {
			return Batch{}, fmt.Errorf("effects: effect %d has unknown kind %q", i, e.Kind)
		}
	}
	return b, nil
}
```

(Add a tiny `bytesReader` helper or use `bytes.NewReader`; import `bytes`.)

- [ ] **Step 4: Run to verify it passes** — `go test ./pkg/effects/ -run TestBatch -v` → PASS.
- [ ] **Step 5: Commit** — `git add pkg/effects/ && git commit -m "feat(effects): typed proposed-effects batch + JSON contract"`

### Task 1.2: The deterministic shape validator

**Files:**
- Create: `pkg/effects/shape.go`
- Test: `pkg/effects/shape_test.go`

- [ ] **Step 1: Write the failing test** — every §6.2 safety case as an explicit case.

```go
package effects

import "testing"

func triageShape() Shape {
	return Shape{Kinds: map[Kind]Card{KindComment: {1, 1}, KindLabel: {0, N}}}
}

func TestShapeAcceptsExactTriage(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindLabel, Add: []string{"gonk::bug"}}}}
	if err := Validate(b, triageShape()); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}

func TestShapeRejectsSecondComment(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "a"}, {Kind: KindComment, Body: "b"}}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("want reject: comment {1,1} but got 2")
	}
}

func TestShapeRejectsForbiddenKind(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindNewIssue, Title: "t"}}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("want reject: new_issue not allowed by triage shape")
	}
}

func TestShapeRejectsMissingRequiredComment(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindLabel, Add: []string{"gonk::bug"}}}}
	if err := Validate(b, triageShape()); err == nil {
		t.Fatal("want reject: comment min 1 but got 0")
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./pkg/effects/ -run TestShape -v` → FAIL.
- [ ] **Step 3: Implement** `pkg/effects/shape.go`:

```go
package effects

import (
	"fmt"
	"sort"
)

// N is the "unbounded" max in a Card (any count >= min is allowed).
const N = int(^uint(0) >> 1) // max int

// Card is a per-kind cardinality: how many of this kind a batch may contain.
type Card struct{ Min, Max int }

// Shape is a task profile's expected-effect-shape: a per-kind cardinality map.
// A kind absent from the map is forbidden ({0,0}).
type Shape struct{ Kinds map[Kind]Card }

// Validate is the deterministic alignment gate -- no model in the loop. It
// checks per-kind counts against the shape. The first violation is returned so
// the caller can record it (spec §6.3).
func Validate(b Batch, s Shape) error {
	counts := map[Kind]int{}
	for _, e := range b.Effects {
		counts[e.Kind]++
	}
	// Any kind present that the shape does not allow (default {0,0}).
	for k, n := range counts {
		card, ok := s.Kinds[k]
		if !ok {
			return fmt.Errorf("effects: kind %q is forbidden by this shape (got %d)", k, n)
		}
		if n > card.Max {
			return fmt.Errorf("effects: kind %q count %d exceeds max %d", k, n, card.Max)
		}
	}
	// Any required kind (min>=1) missing or under-count.
	kinds := make([]Kind, 0, len(s.Kinds))
	for k := range s.Kinds {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, k := range kinds {
		if counts[k] < s.Kinds[k].Min {
			return fmt.Errorf("effects: kind %q count %d below min %d", k, counts[k], s.Kinds[k].Min)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./pkg/effects/ -run TestShape -v` → PASS.
- [ ] **Step 5: Commit** — `git commit -am "feat(effects): deterministic per-kind cardinality shape gate"`

### Task 1.3: Target-binding

**Files:** `pkg/effects/shape.go` (extend), `pkg/effects/shape_test.go` (extend).

- [ ] **Step 1: Failing test** — `ValidateTargets(batch, allowedIIDs)` rejects an effect naming an iid not in the injected context; accepts `TargetIID==0` (the primary target).

```go
func TestTargetBindingRejectsUnhandedResource(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x", TargetIID: 999}}}
	if err := ValidateTargets(b, map[int64]bool{11: true}); err == nil {
		t.Fatal("want reject: effect targets iid 999 not in context {11}")
	}
}
func TestTargetBindingAcceptsPrimaryAndHanded(t *testing.T) {
	b := Batch{Effects: []Effect{{Kind: KindComment, Body: "x"}, {Kind: KindLabel, Add: []string{"l"}, TargetIID: 11}}}
	if err := ValidateTargets(b, map[int64]bool{11: true}); err != nil {
		t.Fatalf("want accept, got %v", err)
	}
}
```

- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** `ValidateTargets` in `shape.go`: iterate effects; if `TargetIID != 0 && !allowed[TargetIID]` return an error naming the kind and iid. (Create-type kinds like `new_issue` bind to a parent — for this slice, accept `TargetIID==0` as "the primary target"; parent-binding detail is deferred with `new_issue`'s apply path.)
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -am "feat(effects): target-binding against injected context"`

### Task 1.4: Load `effect-shape.toml` from the baked pack

**Files:** `pkg/effects/shape_load.go`, `pkg/effects/shape_load_test.go`, `pack/agents/triage/effect-shape.toml`.

- [ ] **Step 1: Failing test** — `LoadShape(dir, "triage")` reads `agents/triage/effect-shape.toml` and yields `comment {1,1}, label {0,N}`. Use a `t.TempDir()` fixture.

```go
func TestLoadShapeTriage(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "agents/triage/effect-shape.toml", `
[comment]
min = 1
max = 1
[label]
min = 0
max = -1   # -1 => unbounded (N)
`)
	s, err := LoadShape(dir, "triage")
	if err != nil { t.Fatal(err) }
	if s.Kinds[KindComment] != (Card{1, 1}) { t.Fatalf("comment card %+v", s.Kinds[KindComment]) }
	if s.Kinds[KindLabel].Max != N { t.Fatalf("label max %d want N", s.Kinds[KindLabel].Max) }
}
```

- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** `LoadShape(packDir, agent string) (Shape, error)` using `github.com/BurntSushi/toml` (already a dep — see `go.mod`). Read `filepath.Join(packDir, "agents", agent, "effect-shape.toml")`; map `max = -1` to `N`; unknown kind keys are an error (fail loud). Document: **the broker reads this from the baked pack `/opt/gonk/pack`, never the `/city` copy** (§6.1 — avoids `gc init` stripping).
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5:** Create `pack/agents/triage/effect-shape.toml` with the real triage shape. Add a pack test asserting it loads.
- [ ] **Step 6: Commit** — `git commit -am "feat(effects): effect-shape.toml loader + triage shape"`

---

## Phase 2 — Meter cost-class escalation (independently landable, pure Go, TDD)

> §7.1. Money authority stays in the meter, deterministic. This phase merges on its own.

### Task 2.1: Per-rung turn cap

**Files:** `pkg/opercfg/opercfg.go` (RungSpec + validation), `pkg/opercfg/opercfg_test.go`.

- [ ] **Step 1: Failing test** — a rung with `max_turns` parses; a cloud rung with no `max_turns` inherits a stingier default than local (assert cloud default < local default).
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** `MaxTurns int` (yaml `max_turns`) on `RungSpec`. When unset, default from `Kind`: a `defaultLocalTurns` and a smaller `defaultCloudTurns` (constants; document that cloud is deliberately stingier, §7.1). Update the JSON schema `pkg/opercfg/gonk-operator.v1.schema.json` and `drift_test.go` if it snapshots the schema.
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -am "feat(opercfg): per-rung max_turns, cloud stingier than local"`

### Task 2.2: Default-off cloud allowance on the scope

**Files:** `pkg/opercfg/opercfg.go` (instance/scope config), tests.

- [ ] **Step 1: Failing test** — the operator config has `CloudAllowed()` returning **false by default**; setting an explicit allowance flag makes it true.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** a `cloud_allowance` block on the instance scope (default off). For this slice a single boolean gate is enough; the instance/namespace/project hierarchy is reserved (§7.1) — leave a documented extension point, do not build the hierarchy.
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -am "feat(opercfg): default-off cloud allowance gate"`

### Task 2.3: The escalation rule in `rung.Decide`

**Files:** `pkg/rung/decide.go`, `pkg/rung/decide_test.go`.

- [ ] **Step 1: Failing tests** — driving `Decide` on an escalation (a re-sling that must climb the ladder):
  - next rung is **local** → granted, and the decision is flagged as a logged escalation.
  - next rung is **cloud**, allowance **off** → **defer/deny to needs-human**, no reservation minted.
  - next rung is **cloud**, allowance **on** → granted with the stingier cloud turn cap.
  - local ladder exhausted, no cloud allowance → needs-human.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** the cost-class gate in the escalation branch of `Decide`: when the chosen next rung has `Kind==KindCloud`, require `cfg.CloudAllowed()` (or a per-request human grant); otherwise return the defer/deny decision that maps to `needs-human` (reuse the existing Deny→`StateNeedsHuman` path in `dispatch.go` — do NOT invent a new outcome). Local→local escalations pass through and are marked for logging. Carry `MaxTurns` into the decision so the session gets the rung's turn cap. **Do not change the budget arithmetic** (N4) — this gate sits in front of the existing reservation logic. **Note (reviewer):** the `Reason` set in `decide.go:48-61` is a bounded wire-contract/Prometheus-label list — add a new reason value (e.g. `cloud-not-allowed`) there, and add a field to `rung.Input` to carry `cfg.CloudAllowed()` into `Decide` (it has none today). Both are small but required.
- [ ] **Step 4: Run → PASS.** Also run the full `pkg/rung` + `pkg/meterapi` suites to confirm no regression.
- [ ] **Step 5: Commit** — `git commit -am "feat(rung): cost-class-gated escalation (free local, gated cloud)"`

---

## Phase 3 — Correlation + inject (AUTHORED AGAINST S1)

> Depends on **S1** (Task 0.2). The bd return channel is dead (S0); the inject
> mechanism and correlation key come from S1's recorded result. Task 3.2 below is
> written for the "keep the formula, add correlation" path; if S1 chooses direct
> `gc session new`, rewrite 3.2 to record the session id instead. **Reviewer
> gap to close in S1:** there is no `pkg/beadstore` API to create a routable work
> bead, and `pkg/gcapi` exposes only `RunOrder` — S1 must pin the concrete
> mechanism (exact `gc`/`bd` argv, whether graph.v2 control refs are needed) or
> choose the session-driven path that avoids bead creation entirely.

### Task 3.1: Add `WorkBeadID` to `beadstore.Record`

**Files:** `pkg/beadstore/store.go:42`, `pkg/beadstore/*_test.go`.

- [ ] **Step 1: Failing test** — a `Record` with `WorkBeadID` set round-trips through `Put`/`Get` (bd store), and an existing record with an empty `WorkBeadID` still loads.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** `WorkBeadID string` on `Record` with a doc comment (the pool work bead the session binds to; stamped at inject; read by sweep). Ensure the bd serialization (metadata mapping in `pkg/beadstore/bd.go`) writes/reads it; empty is valid.
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -am "feat(beadstore): Record.WorkBeadID for dispatch<->sweep correlation"`

### Task 3.2: Dispatch creates the pool-routed work bead directly

**Files:** `cmd/gonk-gate/dispatch.go`, `cmd/gonk-gate/dispatch_test.go`.

> Replaces `d.GC.RunOrder("gonk-triage", vars)` (the formula pour) with direct `bd` bead creation. Keep the meter `Decide` gate exactly as-is; only the pour step changes.

- [ ] **Step 1: Failing test** — with a stub bd store + stub GC, `runDispatch` on a `run` decision creates a task bead with `gc.routed_to=triage`, stamps its id on the stored `Record.WorkBeadID`, and does NOT call `RunOrder`.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement:** after `DecisionRun`, create the work bead via the beadstore/bd (title from the issue, `gc.routed_to=triage`, the rendered `initial_message` under `template_overrides` — see Phase 4 for the render), capture the returned bead id into `base.WorkBeadID`, then `Store.Put`. Remove the `RunOrder`/formula-vars block. Leave `pack/orders/gonk-triage.toml` dormant (a later cleanup task may delete it).
- [ ] **Step 4: Run → PASS** (`go test ./cmd/gonk-gate/...`).
- [ ] **Step 5: Commit** — `git commit -am "feat(dispatch): create pool-routed work bead directly, record WorkBeadID"`

---

## Phase 4 — The broker: inject (dispatch) + validate/apply (sweep)

> §4.2, §7, §8. Depends on Phases 1, 3.

### Task 4.1: Context fetch + render + inject

**Files:** `cmd/gonk-gate/dispatch.go` (+ a small `broker_inject.go` if dispatch grows unwieldy), tests.

- [ ] **Step 1: Failing test** — given a stub GitLab client returning an issue body+labels, the inject step renders an `initial_message` that (a) contains the issue context, (b) instructs "write your proposed-effects JSON to the `gonk.effects` metadata key on your bead; call no external API", and (c) is size-capped (§11 OQ5 — truncate over-cap with a marker).
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** the fetch (reuse `pkg/glab` read for issue body/labels; `.agent/`/`.gonk.yml` thin loader) + a render func producing the `initial_message` string, attached to the work bead created in Task 3.2. Keep the prompt text in a template file under the pack for diffability.
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -am "feat(broker): fetch+render+inject triage context via initial_message"`

### Task 4.2: Sweep reads the batch, validates, applies/escalates/rejects

**Files:** `cmd/gonk-gate/sweep.go`, `cmd/gonk-gate/sweep_test.go`, **`pkg/glab/notes.go` (add `CreateIssueNote`)** — the reviewer confirmed `pkg/glab` has no comment-POST method today (`notes.go` is read-only; `write.go` has labels but no note-create), so the broker's comment-apply needs a new `CreateIssueNote(projectID, iid, body)` write method. Labels use the existing `write.go` `AddIssueLabel`.

> Today sweep verifies triage by reading the comment marker back from GitLab (`artifact.go`). This inverts it: read the batch from the bead, shape-check, apply.

- [ ] **Step 1: Failing tests** (stub GitLab + stub bd), one per §7 row:
  - valid in-shape batch → broker posts exactly the comment (with the `<!-- gonk:bead:… -->` marker it stamps) + labels; asserts the exact `pkg/glab` calls.
  - out-of-shape batch (second comment) → **no** GitLab calls; records a shape violation. (Mutation check: deleting the `effects.Validate` call makes this test fail — T5.)
  - no `gonk.effects` key / invalid JSON → no apply; emits the escalate signal.
- [ ] **Step 2: Run → FAIL.**
- [ ] **Step 3: Implement** in sweep's finished-session handling: load `Record`, follow `WorkBeadID`, read `gonk.effects`, `effects.ParseBatch` → `effects.Validate(batch, LoadShape(...))` → `effects.ValidateTargets(batch, injectedIIDs)`. On pass, apply each effect via `pkg/glab` (`notes.go` for comment — broker stamps the marker; labels via the labels API), then set outcome as today. On shape-fail, apply nothing + record violation → decide per §7 (local re-sling; cloud→needs-human). On no-batch → escalate signal (re-sling through dispatch, which re-decides at the meter — reuse the existing re-sling path).
- [ ] **Step 4: Run → PASS.**
- [ ] **Step 5: Commit** — `git commit -am "feat(broker): sweep validates the batch and applies effects under the bot PAT"`

---

## Phase 5 — Harness + pack + chart: agent emits, pod loses its creds

> §9. Depends on S0, Phase 1.

### Task 5.1: Agent emits the batch, posts nothing

**Files:** `images/agent/entrypoint.sh`, `pack/agents/triage/agent.toml`, the triage prompt template.

- [ ] **Step 1:** Rewrite the injected triage prompt so opencode's job is: read the injected context, decide the comment + labels, and **write** `{"effects":[…]}` to its work bead via `bd update <own-bead> --set-metadata gonk.effects='…'`; explicitly **do not** call `glab`/any external API. (The agent still has `bd`; it no longer needs `glab`.)
- [ ] **Step 2:** In `entrypoint.sh`, **remove** the glab auth setup and the `GONK_BOT_TOKEN`/`GITLAB_TOKEN` handling; keep only the LiteLLM key materialization. Remove the `SSL_CERT_FILE`/`GIT_SSL_CAINFO` orac-CA env from the agent image (it never reaches the forge now). Keep `git`/`bd`/`glab` binaries (thin superset).
- [ ] **Step 3:** Verify with `test/stubmodel` (T3): a stub-model session emits a known batch to its bead; assert the controller can read it. (Reuses S0's write path.)
- [ ] **Step 4: Commit** — `git commit -am "feat(agent): emit proposed-effects batch to bead; remove forge creds/CA from pod"`

### Task 5.2: Chart drops the agent-pod PAT/CA wiring

**Files:** `chart/gonk/templates/workload-gonk-controller.yaml` (the dispatch cred injection into the agent config), any agent-pod secret/env for the bot token + CA.

- [ ] **Step 1:** Remove the bootstrap injection of `GONK_BOT_TOKEN`/glab creds into the per-agent `[env]` and any orac-CA path meant for the agent pod. The controller keeps its own PAT+CA (unchanged).
- [ ] **Step 2:** Regenerate golden chart snapshots (`go test -tags chart ./internal/charttest/ -run Golden -update`) and eyeball the diff — it should be pure *removal* of agent-pod creds.
- [ ] **Step 3: Commit** — `git commit -am "chore(chart): stop injecting forge creds/CA into agent pods"`

---

## Phase 6 — Triage port + the zero-creds proof (e2e)

> §12 T4/T5. Depends on all prior phases.

### Task 6.1: End-to-end proof in the throwaway namespace

- [ ] **Step 1:** Build+push controller+agent images (local build → in-cluster registry port-forward, per the plan doc's recorded recipe), `helm upgrade`.
- [ ] **Step 2:** File a fresh triage issue. Confirm: dispatch creates the work bead + records `WorkBeadID`; the agent session writes `gonk.effects`; sweep reads it, shape-passes, and **the broker posts** the identical comment + labels + marker as before — with the agent pod having **no PAT mounted** (assert the mount/env is absent in the running pod).
- [ ] **Step 3 (safety case, T5):** Inject a deliberately out-of-shape batch (e.g., a stub-model prompt that emits two comments) and confirm the broker posts nothing and records a shape violation.
- [ ] **Step 4 (escalation, T6):** Confirm a no-batch run re-slings on the local ladder and lands `needs-human` when exhausted; confirm a cloud rung with the default (no allowance) denies to `needs-human` rather than spending.
- [ ] **Step 5:** Record results in `docs/superpowers/plans/2026-07-19-minimal-opencode-leg.md` (the running log) and close the relevant beads.

---

## Notes for the implementer

- **TDD everywhere it's unit-testable** (Phases 1–3): red → green → commit, one behavior per test. Phases 4–6 mix unit tests (stub GitLab/bd) with live e2e verification.
- **`go vet ./...` and staticcheck** must stay clean (project rule); run before each commit.
- **Frequent commits** — each task ends in a commit. Never batch phases into one commit.
- **Do not touch money arithmetic** (spec N4). The cost-class gate sits *in front of* the existing reservation logic.
- **`pkg/effects` stays pure** — no GitLab, no bd imports. Apply lives in `sweep.go`.
- Reference @docs/superpowers/specs/2026-07-27-triage-broker-design.md for any "why".
