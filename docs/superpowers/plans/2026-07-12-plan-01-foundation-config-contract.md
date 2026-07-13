# Plan 01: Monorepo Foundation & Config Contract

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the `gonk` monorepo with CI and the two foundational contracts every other component depends on: the `.gonk.yml` config library (parse, validate, precedence-resolve) and the attribution-tags contract.

**Architecture:** Single Go module at the repo root; services are `cmd/` binaries added by later plans; shared contracts live in `pkg/`. The JSON Schema embedded in `pkg/gonkcfg` is the single source of truth for `.gonk.yml`; the copy in `docs/schemas/` is a published artifact gated by a drift test. Budget precedence is a pure function (instance -> group -> project, ceilings tighten-only), fully table-tested with no infrastructure.

**Tech Stack:** Go 1.26 (owner: 1.26 minimum), `gopkg.in/yaml.v3`, `github.com/santhosh-tekuri/jsonschema/v6`, golangci-lint v2, GitLab CI.

**Amendment (owner decision, 2026-07-13): Go 1.26 minimum, and the lint image
must be bumped with it.** The plan originally specified `go 1.24`. Bumping
`go.mod` to `go 1.26` breaks the pinned lint image: `golangci-lint:v2.1.6` is
built with go1.24.2 and refuses to run at all against a newer module —
`can't load config: the Go language version (go1.24) used to build golangci-lint
is lower than the targeted Go version (1.26)` (exit 3, reproduced in the actual
container). The lint image must therefore be built with a Go >= the `go`
directive. Pinned to `golangci-lint:v2.12.2` (built with go1.26.2) and
`golang:1.26`. Bonus: v2.12.2 is also the local gate's version, so the
local/CI lint skew noted during execution is closed rather than merely recorded.

**Spec:** `docs/superpowers/specs/2026-07-12-gonk-stack-design.md` (sections 5.4, 6.1, 10.1). Layout note: the spec's 7.1 sketch shows `intake/` and `meter/` roots; this plan locks the Go-conventional equivalent — one module, `cmd/gonk-intake` + `cmd/gonk-meter` (added in Plans 02/03), shared `pkg/`. Recorded as ADR-001.

**Settled with owner (2026-07-12):** Go module path is `gitlab.orac.local/agentic/gonk-project` — development happens in-cluster on the self-hosted GitLab. Open-sourcing is a deliberate future action; the module path gets rewritten to a public home (e.g. `github.com/leftathome/gonk`) at that time, as a single find-replace. Origin remote: `https://gitlab.orac.local/agentic/gonk-project`.

---

## File Structure

```
go.mod                                     module gitlab.orac.local/agentic/gonk-project
.gitignore, .golangci.yml, .gitlab-ci.yml  repo hygiene + CI
README.md                                  what gonk is, naming note, repo map
PLAN.md                                    plan index + live progress (house rule)
docs/adr/ADR-001-monorepo-go-layout.md     layout decision
docs/schemas/gonk-config.v1.schema.json    published schema (copy; drift-gated)
pkg/gonkcfg/
  gonk-config.v1.schema.json               canonical schema (embedded)
  schema.go                                embed + Validate(raw []byte)
  tokens.go                                TokenQuantity + suffix grammar
  config.go                                Policy/ProjectConfig types + Load()
  resolve.go                               Resolve(instance, group, project) -> Effective
  *_test.go, testdata/                     table tests, fixtures, schema checksum
pkg/atags/
  tags.go                                  attribution tags contract + metadata mapping
  tags_test.go
```

---

### Task 1: Repo scaffold, CI, docs skeleton

**Files:** Create: `go.mod`, `.gitignore`, `.golangci.yml`, `.gitlab-ci.yml`, `README.md`, `PLAN.md`, `docs/adr/ADR-001-monorepo-go-layout.md`

- [x] **Step 1: Create `go.mod`**

```
module gitlab.orac.local/agentic/gonk-project

go 1.26
```

- [x] **Step 2: Create `.gitignore`**

```
/bin/
*.out
.idea/
.vscode/
```

- [x] **Step 3: Create `.golangci.yml`**

```yaml
version: "2"
run:
  timeout: 5m
linters:
  enable:
    - staticcheck
    - govet
    - errcheck
    - ineffassign
    - unused
    - misspell
```

(`version: "2"` is required by golangci-lint v2.x; omitting it makes v2 refuse the config.)

**Amendment (found in execution, 2026-07-12):** this config is blind to
formatting. golangci-lint v2 moved formatters out of the `linters` block, so
`golangci-lint run` reported `0 issues` on a file `gofmt` flags — and
unformatted code (from this plan's own code blocks) duly reached the branch.
Add a `formatters` block, which makes the lint gate load-bearing for
formatting; verified by watching it go red on the drifted file and green once
formatted:

```yaml
formatters:
  enable:
    - gofmt
```

- [x] **Step 4: Create `.gitlab-ci.yml`**

```yaml
stages: [lint, test]

lint:
  stage: lint
  image: golangci/golangci-lint:v2.12.2  # pin full tag; must be v2.x to match .golangci.yml AND built with Go >= go.mod's directive
  script:
    - golangci-lint run ./...

test:
  stage: test
  image: golang:1.26
  script:
    - go vet ./...
    - go test ./... -race -count=1
```

- [x] **Step 5: Create `README.md`**

```markdown
# gonk

A local software factory for self-hosted GitLab: Gas City orchestration,
opencode agent sessions in Kubernetes, LiteLLM-routed local-first inference,
and hard per-project token/cost budgets.

Design spec: `docs/superpowers/specs/2026-07-12-gonk-stack-design.md`
Plan index: `PLAN.md`

## Why "gonk"?

In Night City slang a gonk is a meathead — and this bot is a tireless one:
not brilliant, never idle, gets it done. Naming software after a mild
pejorative has precedent (see: git).

## Layout

- `pkg/gonkcfg` — `.gonk.yml` contract: schema, parsing, budget precedence
- `pkg/atags` — attribution tags contract (request metadata -> spend ledger)
- `cmd/` — services (gonk-intake, gonk-meter; later plans)
- `pack/`, `images/`, `chart/`, `test/` — later plans
```

- [x] **Step 6: Create `PLAN.md`**

```markdown
# gonk plan index

Spec: docs/superpowers/specs/2026-07-12-gonk-stack-design.md

| Plan | Scope | Status |
|---|---|---|
| 01 foundation & config contract | scaffold, CI, gonkcfg, atags | in progress |
| 02 gitlab-intake | webhooks, reconciliation, onboarding MR | not started |
| 03 gonk-meter | rung policy, key provisioning, ledger | not started |
| 04 pack & images | agents/formulas/orders, docker images | not started |
| 05 chart | Helm chart, BYO seams | not started |
| 06 e2e harness | kind + gitlab-ce + stub model, kill tests | not started |

Update the Status column as tasks complete (house rule: progress lives here).
```

- [x] **Step 7: Create `docs/adr/ADR-001-monorepo-go-layout.md`**

```markdown
# ADR-001: Single Go module, cmd/ + pkg/ layout

Status: accepted 2026-07-12

Spec 7.1 sketches intake/ and meter/ as top-level roots. We use one Go module
at the repo root with cmd/gonk-intake and cmd/gonk-meter and shared pkg/
libraries instead. Rationale: the .gonk.yml schema, attribution tags, and both
services must change atomically (spec goal: schemas evolve together); one
module means one go.sum, one CI cache, and cross-package refactors in one
commit. pack/, images/, chart/, test/ keep their spec names (not Go code).
```

- [x] **Step 8: Verify and commit**

Run: `go build ./...`
Expected: exits 0 with a "matched no packages" warning (no Go files exist yet).
Correction (found in execution): `go vet ./...` exits **1** ("no packages to
vet") on a module containing zero .go files — this is toolchain behavior, not a
defect, and it resolves the moment Task 2 adds a real package. Do not run `go
vet` as a gate at this step, and do not add a placeholder .go file to appease it.
Note: do not push to CI until after Task 2 — both `go vet` and golangci-lint
fail on a repo with zero Go files.

```bash
git add -A && git commit -m "chore: scaffold monorepo, CI, plan index (ADR-001)"
```

---

### Task 2: TokenQuantity and the suffix grammar

**Files:** Create: `pkg/gonkcfg/tokens.go`, `pkg/gonkcfg/tokens_test.go`

**Amendment (found in execution, 2026-07-12):** the `UnmarshalYAML` below is
buggy as written and was corrected during Task 2. It decides "this node is an
integer" by testing whether `node.Decode(&int64)` succeeds — but yaml.v3
*succeeds* on a `!!float` node by truncating it. Verified: `1.5` -> `1`,
`1e3` -> `1000`, both with a nil error. For a tool enforcing hard token
budgets, `monthly_tokens: 1.5` silently becoming a 1-token budget is
unacceptable. The corrected implementation dispatches on `node.ShortTag()`
(`!!int` -> int path, `!!str` -> `ParseTokenQuantity`, `!!null` -> unset, all
else -> error naming the tag). The root cause of the miss: the plan's test
table only covers `ParseTokenQuantity` (the string API) and never exercised
`UnmarshalYAML` at all, so Task 2 also adds a `TestUnmarshalYAML` covering
int/string/unquoted-scalar/negative/float/exponent/integral-float/null/bool/
non-scalar. Note `Load` (Task 4) schema-validates before decoding, so this is
defense-in-depth — but the type must be sound on its own, since the schema is
the only other thing between a typo and a wrong budget.

- [x] **Step 1: Write the failing test**

`pkg/gonkcfg/tokens_test.go`:

```go
package gonkcfg

import "testing"

func TestParseTokenQuantity(t *testing.T) {
	cases := []struct {
		in      string
		want    TokenQuantity
		wantErr bool
	}{
		{"0", 0, false},
		{"12345", 12345, false},
		{"50K", 50_000, false},
		{"50M", 50_000_000, false},
		{"2G", 2_000_000_000, false},
		{"", 0, true},
		{"-5", 0, true},
		{"50m", 0, true},   // lowercase suffix rejected
		{"1.5M", 0, true},  // fractional rejected
		{"M", 0, true},
		{"50MB", 0, true},
		{"9223372036854775807K", 0, true}, // overflow
	}
	for _, c := range cases {
		got, err := ParseTokenQuantity(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("ParseTokenQuantity(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseTokenQuantity(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/gonkcfg/ -run TestParseTokenQuantity -v`
Expected: FAIL (compile error: undefined TokenQuantity/ParseTokenQuantity)

- [x] **Step 3: Implement**

`pkg/gonkcfg/tokens.go`:

```go
// Package gonkcfg is the contract for .gonk.yml: schema validation, typed
// loading, and instance -> group -> project precedence resolution.
package gonkcfg

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// TokenQuantity is a token count. In YAML it is either a non-negative
// integer or a string matching ^[0-9]+[KMG]?$ (decimal multipliers 1e3,
// 1e6, 1e9). The string form exists so ".gonk.yml" authors can write "50M"
// without YAML numeric-parsing ambiguity (spec 5.4).
type TokenQuantity int64

var multipliers = map[byte]int64{'K': 1e3, 'M': 1e6, 'G': 1e9}

func ParseTokenQuantity(s string) (TokenQuantity, error) {
	if s == "" {
		return 0, fmt.Errorf("token quantity: empty")
	}
	mult := int64(1)
	digits := s
	if m, ok := multipliers[s[len(s)-1]]; ok {
		mult, digits = m, s[:len(s)-1]
	}
	if digits == "" || strings.TrimLeft(digits, "0123456789") != "" {
		return 0, fmt.Errorf("token quantity %q: want ^[0-9]+[KMG]?$", s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("token quantity %q: %w", s, err)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("token quantity %q: overflow", s)
	}
	return TokenQuantity(n * mult), nil
}

// UnmarshalYAML accepts integer or suffixed-string scalars.
func (q *TokenQuantity) UnmarshalYAML(node *yaml.Node) error {
	var i int64
	if err := node.Decode(&i); err == nil {
		if i < 0 {
			return fmt.Errorf("token quantity: negative")
		}
		*q = TokenQuantity(i)
		return nil
	}
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("token quantity: want integer or string")
	}
	v, err := ParseTokenQuantity(s)
	if err != nil {
		return err
	}
	*q = v
	return nil
}
```

- [x] **Step 4: Fetch deps, run test to verify it passes**

Run: `go get gopkg.in/yaml.v3 && go test ./pkg/gonkcfg/ -run TestParseTokenQuantity -v`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(gonkcfg): TokenQuantity with K/M/G suffix grammar"
```

---

### Task 3: Canonical JSON Schema + validator

**Files:** Create: `pkg/gonkcfg/gonk-config.v1.schema.json`, `pkg/gonkcfg/schema.go`, `pkg/gonkcfg/schema_test.go`

**Amendment (found in execution, 2026-07-12) — `Validate` must reject non-finite
floats before calling the validator.** `Load` as originally written **panics**
(nil-pointer dereference) on `budget: { monthly_cost_usd: .nan }`, `.inf`, or
`-.inf`. Root cause is upstream: `jsonschema/v6@v6.0.2` (`validator.go:515-524`)
builds a `big.Rat` via `new(big.Rat).SetString(fmt.Sprintf("%v", v))`, which
returns **nil** for a non-finite float, discards the `ok`, and then calls `.Cmp`
on the nil. Our `"minimum": 0` on `monthly_cost_usd` is the trigger; the token
fields are not exposed because their `oneOf[integer,string]` rejects the float
before `minimum` runs. **`.gonk.yml` is untrusted, project-authored repo
content**, so this is a one-line remote crash of the process that enforces every
project's budget and kill switch. Fix: `Validate` walks the decoded document and
rejects any non-finite float64 at any path (JSON has no NaN/Inf, so such a
document is never valid) before handing it to the validator. Related, in Task 5:
a NaN ceiling that reaches the min-fold resolves to `+Inf` = **unlimited**,
because every NaN comparison is false and the fold silently skips it — the
resolver now fails closed on a non-finite ceiling instead. Also add
`"uniqueItems": true` to `ladder`: `[glm, glm]` validated and produced a
duplicated rung, which would make escalation retry the same rung.

- [x] **Step 1: Write the schema**

`pkg/gonkcfg/gonk-config.v1.schema.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://gitlab.orac.local/agentic/gonk-project/-/raw/main/docs/schemas/gonk-config.v1.schema.json",
  "title": "gonk project configuration (.gonk.yml), schema version 1",
  "type": "object",
  "additionalProperties": false,
  "required": ["version", "enabled"],
  "properties": {
    "version": { "const": 1 },
    "enabled": { "type": "boolean" },
    "actions": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "triage": { "type": "boolean" },
        "pipelines": { "type": "boolean" },
        "features": { "type": "boolean" }
      }
    },
    "schedule": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "quiet_hours": {
          "type": "string",
          "pattern": "^([01][0-9]|2[0-3]):[0-5][0-9]-([01][0-9]|2[0-3]):[0-5][0-9]$"
        },
        "timezone": { "type": "string", "minLength": 1 }
      }
    },
    "budget": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "monthly_cost_usd": { "type": "number", "minimum": 0 },
        "monthly_tokens": { "$ref": "#/$defs/tokenQuantity" },
        "per_task_tokens": { "$ref": "#/$defs/tokenQuantity" }
      }
    },
    "ladder": {
      "type": "array",
      "minItems": 1,
      "items": { "type": "string", "pattern": "^[a-z0-9][a-z0-9-]*$" }
    },
    "continuity": { "enum": ["resume", "fresh"] },
    "triage": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "label_prefix": { "type": "string", "minLength": 1 },
        "respond_to_mentions": { "type": "boolean" }
      }
    },
    "provenance": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "commit_trailers": { "type": "boolean" },
        "include_usage": { "type": "boolean" }
      }
    }
  },
  "$defs": {
    "tokenQuantity": {
      "oneOf": [
        { "type": "integer", "minimum": 0 },
        { "type": "string", "pattern": "^[0-9]+[KMG]?$" }
      ]
    }
  }
}
```

- [x] **Step 2: Write the failing test**

`pkg/gonkcfg/schema_test.go`:

```go
package gonkcfg

import (
	"strings"
	"testing"
)

const validYAML = `
version: 1
enabled: true
actions: { triage: true, pipelines: false, features: false }
schedule: { quiet_hours: "22:00-07:00", timezone: "America/New_York" }
budget: { monthly_cost_usd: 0, monthly_tokens: "50M", per_task_tokens: "2M" }
ladder: [qwen-local]
continuity: resume
triage: { label_prefix: "gonk::", respond_to_mentions: true }
provenance: { commit_trailers: true, include_usage: false }
`

func TestValidateAcceptsSpecExample(t *testing.T) {
	if err := Validate([]byte(validYAML)); err != nil {
		t.Fatalf("Validate(spec example) = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"missing version":    "enabled: true",
		"wrong version":      "version: 2\nenabled: true",
		"missing enabled":    "version: 1",
		"unknown key":        "version: 1\nenabled: true\nbanana: true",
		"bad continuity":     "version: 1\nenabled: true\ncontinuity: sometimes",
		"bad quiet_hours":    "version: 1\nenabled: true\nschedule: { quiet_hours: \"25:00-07:00\" }",
		"bad token suffix":   "version: 1\nenabled: true\nbudget: { monthly_tokens: \"50MB\" }",
		"negative cost":      "version: 1\nenabled: true\nbudget: { monthly_cost_usd: -1 }",
		"empty ladder":       "version: 1\nenabled: true\nladder: []",
		"uppercase rung":     "version: 1\nenabled: true\nladder: [Qwen]",
		"not yaml":           "{{{{",
	}
	for name, doc := range cases {
		if err := Validate([]byte(doc)); err == nil {
			t.Errorf("%s: Validate accepted %q, want error", name, strings.TrimSpace(doc))
		}
	}
}
```

- [x] **Step 3: Run test to verify it fails**

Run: `go test ./pkg/gonkcfg/ -run 'TestValidate' -v`
Expected: FAIL (undefined: Validate)

- [x] **Step 4: Implement**

`pkg/gonkcfg/schema.go`:

```go
package gonkcfg

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// SchemaVersion is the .gonk.yml schema major version this package speaks.
const SchemaVersion = 1

//go:embed gonk-config.v1.schema.json
var schemaJSON []byte

var compiled = mustCompile()

func mustCompile() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("gonkcfg: embedded schema unreadable: %v", err))
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("gonk-config.v1.schema.json", doc); err != nil {
		panic(err)
	}
	s, err := c.Compile("gonk-config.v1.schema.json")
	if err != nil {
		panic(fmt.Sprintf("gonkcfg: embedded schema invalid: %v", err))
	}
	return s
}

// Validate checks raw .gonk.yml bytes against the v1 JSON Schema.
// It does not apply defaults or precedence; see Load and Resolve.
func Validate(raw []byte) error {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf(".gonk.yml: not valid YAML: %w", err)
	}
	if err := compiled.Validate(doc); err != nil {
		return fmt.Errorf(".gonk.yml: %w", err)
	}
	return nil
}
```

Note for implementer: `jsonschema/v6.Validate` requires JSON-shaped values
(`map[string]any`, `[]any`, numbers, strings, bools). `yaml.v3` produces
`map[string]any` for string-keyed maps, which is what all valid `.gonk.yml`
documents contain; non-string keys will fail validation with a type error,
which is the desired behavior. If v6's API differs at implementation time
(check go.sum'd version), adapt inside this file only — the exported
`Validate([]byte) error` signature is the contract.

- [x] **Step 5: Fetch dep, run tests**

Run: `go get github.com/santhosh-tekuri/jsonschema/v6 && go test ./pkg/gonkcfg/ -run 'TestValidate' -v`
Expected: PASS (all reject cases produce errors; spec example accepted)

- [x] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(gonkcfg): embedded v1 JSON Schema and Validate"
```

---

### Task 4: Typed config + Load [done]

**Files:** Create: `pkg/gonkcfg/config.go`, `pkg/gonkcfg/config_test.go`

- [x] **Step 1: Write the failing test**

`pkg/gonkcfg/config_test.go`:

```go
package gonkcfg

import "testing"

func TestLoadSpecExample(t *testing.T) {
	pc, err := Load([]byte(validYAML))
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if pc.Version != 1 || pc.Enabled == nil || !*pc.Enabled {
		t.Fatalf("version/enabled wrong: %+v", pc)
	}
	if pc.Actions.Triage == nil || !*pc.Actions.Triage {
		t.Fatalf("actions.triage wrong: %+v", pc.Actions)
	}
	if pc.Budget.MonthlyTokens == nil || *pc.Budget.MonthlyTokens != 50_000_000 {
		t.Fatalf("monthly_tokens wrong: %+v", pc.Budget)
	}
	if got := pc.Ladder; len(got) != 1 || got[0] != "qwen-local" {
		t.Fatalf("ladder wrong: %v", got)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	if _, err := Load([]byte("version: 1")); err == nil {
		t.Fatal("Load accepted schema-invalid doc")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/gonkcfg/ -run 'TestLoad' -v`
Expected: FAIL (undefined: Load)

- [x] **Step 3: Implement**

`pkg/gonkcfg/config.go`:

```go
package gonkcfg

import "gopkg.in/yaml.v3"

// Policy is one layer of gonk configuration. All fields are optional
// (pointers/nil slices); Resolve folds layers into an Effective config.
// The same shape serves instance defaults, group overrides, and the
// project's .gonk.yml.
type Policy struct {
	Enabled    *bool             `yaml:"enabled"`
	Actions    ActionsPolicy     `yaml:"actions"`
	Schedule   *Schedule         `yaml:"schedule"`
	Budget     BudgetPolicy      `yaml:"budget"`
	Ladder     []string          `yaml:"ladder"`
	Continuity *string           `yaml:"continuity"`
	Triage     TriagePolicy      `yaml:"triage"`
	Provenance ProvenancePolicy  `yaml:"provenance"`
}

type ActionsPolicy struct {
	Triage    *bool `yaml:"triage"`
	Pipelines *bool `yaml:"pipelines"`
	Features  *bool `yaml:"features"`
}

type Schedule struct {
	QuietHours string `yaml:"quiet_hours"`
	Timezone   string `yaml:"timezone"`
}

type BudgetPolicy struct {
	MonthlyCostUSD *float64       `yaml:"monthly_cost_usd"`
	MonthlyTokens  *TokenQuantity `yaml:"monthly_tokens"`
	PerTaskTokens  *TokenQuantity `yaml:"per_task_tokens"`
}

type TriagePolicy struct {
	LabelPrefix       *string `yaml:"label_prefix"`
	RespondToMentions *bool   `yaml:"respond_to_mentions"`
}

type ProvenancePolicy struct {
	CommitTrailers *bool `yaml:"commit_trailers"`
	IncludeUsage   *bool `yaml:"include_usage"`
}

// ProjectConfig is a parsed, schema-valid .gonk.yml.
type ProjectConfig struct {
	Version int    `yaml:"version"`
	Policy  `yaml:",inline"`
}

// Load validates raw .gonk.yml bytes against the schema, then decodes them.
func Load(raw []byte) (*ProjectConfig, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var pc ProjectConfig
	if err := yaml.Unmarshal(raw, &pc); err != nil {
		return nil, err
	}
	return &pc, nil
}
```

- [x] **Step 4: Run tests**

Run: `go test ./pkg/gonkcfg/ -v`
Expected: PASS (all tests so far)

- [x] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(gonkcfg): typed Policy/ProjectConfig and Load"
```

Also added an authorized third test, `TestLoadRejectsFractionalTokens`
(Task 3 re-review carry-forward), proving `Load` rejects `monthly_tokens`/
`per_task_tokens` given as `2.0` or `1.5` even though the JSON Schema alone
accepts `2.0` as a zero-fraction integer — `TokenQuantity.UnmarshalYAML`'s
tag dispatch is the second layer of defense, and `Load` must run both.

---

### Task 5: Precedence resolver (tighten-only budgets)

**Files:** Create: `pkg/gonkcfg/resolve.go`, `pkg/gonkcfg/resolve_test.go`

Semantics (spec 5.4, settled in design review):

- `enabled`: project must say true AND no coarser layer says false (kill switch).
- actions: effective = project value (unset -> false) AND group/instance allow
  (unset -> allow). Conservative: nothing runs unless the project opts in.
- budget: effective ceiling = MIN over layers where set; all unset = unlimited.
- ladder: base list = most specific layer that sets one; then intersect
  (preserving base order) with each coarser layer that sets one (allow-lists).
- schedule, continuity, triage, provenance: most specific set value wins.
  Defaults: continuity=resume, label_prefix="gonk::", respond_to_mentions=true,
  commit_trailers=true, include_usage=false, schedule=none.

**Amendment (owner decision, 2026-07-12): an empty effective ladder fails
closed.** Execution surfaced a hole the plan's semantics did not cover: the fold
can produce a zero-rung ladder two ways — layers set ladders whose intersection
is empty (project wants `opus`, instance allows only `qwen-local`), or no layer
sets a ladder at all. As originally written, `Resolve` returned `Enabled: true`
with zero rungs, leaving every downstream consumer to remember a `len(Ladder)==0`
check (and a naive `Ladder == nil` check misses it, because an empty intersection
is a non-nil zero-length slice). Owner's call: a project that cannot run any rung
is definitionally not runnable, so `Resolve` says so once. `Effective` gains a
`DisabledReason string` with the invariant *non-empty iff `Enabled == false`*,
and an empty ladder (either cause) sets `Enabled = false`. Reason precedence:
instance veto -> group veto -> project not enabled -> ladder empty, so a project
killed by its instance is not told to blame its ladder. Spec 5.4 keeps the
one-line rule; ADR-002 is the precise contract, and it records this.

- [x] **Step 1: Write the failing test**

`pkg/gonkcfg/resolve_test.go`:

```go
package gonkcfg

import (
	"math"
	"reflect"
	"testing"
)

func b(v bool) *bool             { return &v }
func f(v float64) *float64       { return &v }
func tq(v TokenQuantity) *TokenQuantity { return &v }
func s(v string) *string         { return &v }

func project(mut func(*ProjectConfig)) ProjectConfig {
	pc := ProjectConfig{Version: 1, Policy: Policy{
		Enabled: b(true),
		Actions: ActionsPolicy{Triage: b(true)},
		Ladder:  []string{"qwen-local"},
	}}
	if mut != nil {
		mut(&pc)
	}
	return pc
}

func TestResolveDefaults(t *testing.T) {
	eff := Resolve(Policy{}, Policy{}, project(nil))
	if !eff.Enabled || !eff.Actions.Triage || eff.Actions.Pipelines {
		t.Fatalf("enabled/actions wrong: %+v", eff)
	}
	if eff.Continuity != "resume" {
		t.Fatalf("continuity default wrong: %q", eff.Continuity)
	}
	if eff.Triage.LabelPrefix != "gonk::" || !eff.Triage.RespondToMentions {
		t.Fatalf("triage defaults wrong: %+v", eff.Triage)
	}
	if !eff.Provenance.CommitTrailers || eff.Provenance.IncludeUsage {
		t.Fatalf("provenance defaults wrong: %+v", eff.Provenance)
	}
	if !math.IsInf(eff.Budget.MonthlyCostUSD, 1) {
		t.Fatalf("unset cost should be unlimited: %v", eff.Budget.MonthlyCostUSD)
	}
	if eff.Budget.MonthlyTokens != math.MaxInt64 {
		t.Fatalf("unset tokens should be unlimited: %v", eff.Budget.MonthlyTokens)
	}
}

func TestResolveKillSwitchAndActionAND(t *testing.T) {
	eff := Resolve(Policy{Enabled: b(false)}, Policy{}, project(nil))
	if eff.Enabled {
		t.Fatal("instance kill switch ignored")
	}
	eff = Resolve(Policy{}, Policy{Actions: ActionsPolicy{Triage: b(false)}}, project(nil))
	if eff.Actions.Triage {
		t.Fatal("group action veto ignored")
	}
	eff = Resolve(Policy{}, Policy{}, project(func(pc *ProjectConfig) {
		pc.Actions = ActionsPolicy{} // project silent on all actions
	}))
	if eff.Actions.Triage {
		t.Fatal("project must opt in to actions")
	}
}

func TestResolveBudgetTightenOnly(t *testing.T) {
	inst := Policy{Budget: BudgetPolicy{MonthlyCostUSD: f(100), MonthlyTokens: tq(1_000_000_000)}}
	grp := Policy{Budget: BudgetPolicy{MonthlyCostUSD: f(10)}}
	proj := project(func(pc *ProjectConfig) {
		pc.Budget = BudgetPolicy{MonthlyCostUSD: f(50), MonthlyTokens: tq(50_000_000)}
	})
	eff := Resolve(inst, grp, proj)
	if eff.Budget.MonthlyCostUSD != 10 { // min(100, 10, 50)
		t.Fatalf("cost = %v, want 10", eff.Budget.MonthlyCostUSD)
	}
	if eff.Budget.MonthlyTokens != 50_000_000 { // min(1G, unset, 50M)
		t.Fatalf("tokens = %v, want 50M", eff.Budget.MonthlyTokens)
	}
}

func TestResolveLadderIntersection(t *testing.T) {
	inst := Policy{Ladder: []string{"qwen-local", "glm", "sonnet"}}
	grp := Policy{Ladder: []string{"qwen-local", "glm"}}
	proj := project(func(pc *ProjectConfig) {
		pc.Ladder = []string{"glm", "qwen-local", "opus"}
	})
	eff := Resolve(inst, grp, proj)
	want := []string{"glm", "qwen-local"} // project order, opus not allowed upstream
	if !reflect.DeepEqual(eff.Ladder, want) {
		t.Fatalf("ladder = %v, want %v", eff.Ladder, want)
	}
	// project silent -> inherits group's list
	eff = Resolve(inst, grp, project(func(pc *ProjectConfig) { pc.Ladder = nil }))
	if !reflect.DeepEqual(eff.Ladder, []string{"qwen-local", "glm"}) {
		t.Fatalf("inherited ladder = %v", eff.Ladder)
	}
}

func TestResolveMostSpecificWins(t *testing.T) {
	inst := Policy{Continuity: s("fresh"), Schedule: &Schedule{QuietHours: "01:00-02:00", Timezone: "UTC"}}
	proj := project(func(pc *ProjectConfig) { pc.Continuity = s("resume") })
	eff := Resolve(inst, Policy{}, proj)
	if eff.Continuity != "resume" {
		t.Fatalf("continuity = %q", eff.Continuity)
	}
	if eff.Schedule == nil || eff.Schedule.QuietHours != "01:00-02:00" {
		t.Fatalf("schedule not inherited: %+v", eff.Schedule)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/gonkcfg/ -run TestResolve -v`
Expected: FAIL (undefined: Resolve)

- [x] **Step 3: Implement**

`pkg/gonkcfg/resolve.go`:

```go
package gonkcfg

import "math"

// Effective is the fully-resolved configuration for one project: defaults
// applied, precedence folded, budgets tightened. This is the only type the
// rest of gonk consumes.
type Effective struct {
	Enabled    bool
	Actions    Actions
	Schedule   *Schedule
	Budget     EffectiveBudget
	Ladder     []string
	Continuity string
	Triage     EffectiveTriage
	Provenance EffectiveProvenance
}

type Actions struct{ Triage, Pipelines, Features bool }

type EffectiveBudget struct {
	MonthlyCostUSD float64       // +Inf when unlimited
	MonthlyTokens  TokenQuantity // MaxInt64 when unlimited
	PerTaskTokens  TokenQuantity // MaxInt64 when unlimited
}

type EffectiveTriage struct {
	LabelPrefix       string
	RespondToMentions bool
}

type EffectiveProvenance struct {
	CommitTrailers bool
	IncludeUsage   bool
}

// Resolve folds instance defaults, group overrides, and the project config
// into an Effective. Precedence: most specific wins, except (a) enabled and
// actions are vetoable by coarser layers, and (b) budget ceilings are the
// minimum across layers (spec 5.4: ceilings only tighten downward).
func Resolve(instance, group Policy, project ProjectConfig) Effective {
	layers := []Policy{instance, group, project.Policy} // coarse -> specific

	eff := Effective{
		Enabled:    project.Enabled != nil && *project.Enabled,
		Continuity: "resume",
		Triage:     EffectiveTriage{LabelPrefix: "gonk::", RespondToMentions: true},
		Provenance: EffectiveProvenance{CommitTrailers: true, IncludeUsage: false},
		Budget: EffectiveBudget{
			MonthlyCostUSD: math.Inf(1),
			MonthlyTokens:  TokenQuantity(math.MaxInt64),
			PerTaskTokens:  TokenQuantity(math.MaxInt64),
		},
	}

	// Vetoes: any coarser layer explicitly disabling wins.
	for _, l := range []Policy{instance, group} {
		if l.Enabled != nil && !*l.Enabled {
			eff.Enabled = false
		}
	}
	eff.Actions = Actions{
		Triage:    andAction(project.Actions.Triage, instance.Actions.Triage, group.Actions.Triage),
		Pipelines: andAction(project.Actions.Pipelines, instance.Actions.Pipelines, group.Actions.Pipelines),
		Features:  andAction(project.Actions.Features, instance.Actions.Features, group.Actions.Features),
	}

	// Tighten-only budgets.
	for _, l := range layers {
		if v := l.Budget.MonthlyCostUSD; v != nil && *v < eff.Budget.MonthlyCostUSD {
			eff.Budget.MonthlyCostUSD = *v
		}
		if v := l.Budget.MonthlyTokens; v != nil && *v < eff.Budget.MonthlyTokens {
			eff.Budget.MonthlyTokens = *v
		}
		if v := l.Budget.PerTaskTokens; v != nil && *v < eff.Budget.PerTaskTokens {
			eff.Budget.PerTaskTokens = *v
		}
	}

	// Ladder: most specific list is the base (ordering), coarser set lists
	// act as allow-lists.
	for i := len(layers) - 1; i >= 0; i-- {
		if layers[i].Ladder != nil {
			eff.Ladder = append([]string(nil), layers[i].Ladder...)
			for j := 0; j < i; j++ {
				if layers[j].Ladder != nil {
					eff.Ladder = intersect(eff.Ladder, layers[j].Ladder)
				}
			}
			break
		}
	}

	// Most-specific-wins scalars.
	for _, l := range layers {
		if l.Schedule != nil {
			sc := *l.Schedule
			eff.Schedule = &sc
		}
		if l.Continuity != nil {
			eff.Continuity = *l.Continuity
		}
		if l.Triage.LabelPrefix != nil {
			eff.Triage.LabelPrefix = *l.Triage.LabelPrefix
		}
		if l.Triage.RespondToMentions != nil {
			eff.Triage.RespondToMentions = *l.Triage.RespondToMentions
		}
		if l.Provenance.CommitTrailers != nil {
			eff.Provenance.CommitTrailers = *l.Provenance.CommitTrailers
		}
		if l.Provenance.IncludeUsage != nil {
			eff.Provenance.IncludeUsage = *l.Provenance.IncludeUsage
		}
	}
	return eff
}

// andAction: project must opt in (nil -> false); coarser layers veto
// (nil -> allow).
func andAction(project *bool, vetoes ...*bool) bool {
	if project == nil || !*project {
		return false
	}
	for _, v := range vetoes {
		if v != nil && !*v {
			return false
		}
	}
	return true
}

func intersect(base, allowed []string) []string {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[a] = struct{}{}
	}
	out := base[:0:0]
	for _, b := range base {
		if _, ok := set[b]; ok {
			out = append(out, b)
		}
	}
	return out
}
```

- [x] **Step 4: Run all package tests**

Run: `go test ./pkg/gonkcfg/ -race -v`
Expected: PASS

- [x] **Step 5: Record the resolver semantics as the published contract**

Create `docs/adr/ADR-002-config-precedence-semantics.md` containing the
semantics block from the top of this task verbatim (enabled kill switch,
action opt-in + veto, tighten-only budget min-fold, ladder base+allow-list
intersection, most-specific-wins scalars, defaults). Spec 5.4 states the
one-line rule; this ADR is the precise contract the resolver implements,
so published docs and code cannot drift apart in later plans.

- [x] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(gonkcfg): precedence resolver with vetoes and tighten-only budgets (ADR-002)"
```

---

### Task 6: Schema publication + drift gates

**Files:** Create: `docs/schemas/gonk-config.v1.schema.json` (copy of canonical), `pkg/gonkcfg/testdata/schema.v1.sha256`, `pkg/gonkcfg/drift_test.go`

- [x] **Step 1: Publish the copy and record the checksum**

```bash
mkdir -p docs/schemas pkg/gonkcfg/testdata
cp pkg/gonkcfg/gonk-config.v1.schema.json docs/schemas/
sha256sum pkg/gonkcfg/gonk-config.v1.schema.json | cut -d' ' -f1 > pkg/gonkcfg/testdata/schema.v1.sha256
```

- [x] **Step 2: Write the failing-if-drifted test**

`pkg/gonkcfg/drift_test.go`:

```go
package gonkcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The embedded schema is canonical. The docs/ copy is the published
// artifact, and the recorded checksum is the breaking-change gate
// (spec 10.1): changing the schema without consciously updating the
// checksum (and bumping the version on breaking changes) fails CI.

func TestPublishedSchemaMatchesCanonical(t *testing.T) {
	published, err := os.ReadFile("../../docs/schemas/gonk-config.v1.schema.json")
	if err != nil {
		t.Fatalf("published schema missing: %v", err)
	}
	if !bytes.Equal(published, schemaJSON) {
		t.Fatal("docs/schemas copy differs from embedded canonical schema; re-run the publish copy step")
	}
}

func TestSchemaChangeIsDeliberate(t *testing.T) {
	want, err := os.ReadFile("testdata/schema.v1.sha256")
	if err != nil {
		t.Fatalf("checksum file missing: %v", err)
	}
	sum := sha256.Sum256(schemaJSON)
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("schema content changed (sha256 %s). If this is intentional: additive change -> update testdata/schema.v1.sha256 and docs copy; breaking change -> new schema version file + version const. See spec 10.1.", got)
	}
}
```

- [x] **Step 3: Run tests**

Run: `go test ./pkg/gonkcfg/ -run 'Schema' -v`
Expected: PASS

- [x] **Step 4: Verify the gate actually gates**

Run: `printf ' ' >> pkg/gonkcfg/gonk-config.v1.schema.json && go test ./pkg/gonkcfg/ -run 'Schema' ; git checkout pkg/gonkcfg/gonk-config.v1.schema.json`
Expected: FAIL while modified (both drift tests), then restored.

- [x] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(gonkcfg): published schema copy with drift and deliberate-change gates"
```

---

### Task 7: Attribution tags contract (pkg/atags)

**Files:** Create: `pkg/atags/tags.go`, `pkg/atags/tags_test.go`

- [x] **Step 1: Write the failing test**

`pkg/atags/tags_test.go`:

```go
package atags

import (
	"reflect"
	"testing"
)

func valid() Tags {
	return Tags{
		Project: "group/repo", Rig: "repo", BeadID: "gk-1a2b",
		SessionKey: "sess-9", Rung: "qwen-local", Attempt: 1,
		Trigger: TriggerIssueTriage,
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid tags rejected: %v", err)
	}
	bad := []func(*Tags){
		func(x *Tags) { x.Project = "" },
		func(x *Tags) { x.Rig = "" },
		func(x *Tags) { x.BeadID = "" },
		func(x *Tags) { x.SessionKey = "" },
		func(x *Tags) { x.Rung = "" },
		func(x *Tags) { x.Attempt = 0 },
		func(x *Tags) { x.Trigger = "vibes" },
	}
	for i, mut := range bad {
		tags := valid()
		mut(&tags)
		if err := tags.Validate(); err == nil {
			t.Errorf("case %d: invalid tags accepted: %+v", i, tags)
		}
	}
}

// The literal key and trigger strings ARE the ledger contract (spec 10.1):
// a rename must fail CI even though the round-trip test, which uses the
// constants symmetrically, would still pass.
func TestContractLiterals(t *testing.T) {
	wantKeys := map[string]string{
		KeyProject: "gonk_project", KeyRig: "gonk_rig",
		KeyBeadID: "gonk_bead_id", KeySessionKey: "gonk_session_key",
		KeyRung: "gonk_rung", KeyAttempt: "gonk_attempt",
		KeyTrigger: "gonk_trigger",
	}
	for got, want := range wantKeys {
		if got != want {
			t.Errorf("metadata key changed: %q != %q (breaking ledger change; see spec 10.1)", got, want)
		}
	}
	wantTriggers := map[string]string{
		TriggerIssueTriage: "issue-triage", TriggerOnboarding: "onboarding",
		TriggerScaffold: "scaffold", TriggerMentionReply: "mention-reply",
	}
	for got, want := range wantTriggers {
		if got != want {
			t.Errorf("trigger changed: %q != %q (breaking ledger change)", got, want)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	in := valid()
	out, err := FromMetadata(in.Metadata())
	if err != nil {
		t.Fatalf("FromMetadata = %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: got %+v want %+v", out, in)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/atags/ -v`
Expected: FAIL (package missing)

- [x] **Step 3: Implement**

`pkg/atags/tags.go`:

```go
// Package atags is the attribution-tags contract: the metadata attached to
// every LiteLLM request so gonk-meter can join spend to beads, sessions,
// and GitLab artifacts (spec 6.1). Key names are part of the public
// contract; changing them is a breaking change to the ledger.
package atags

import (
	"fmt"
	"strconv"
)

const (
	TriggerIssueTriage  = "issue-triage"
	TriggerOnboarding   = "onboarding"
	TriggerScaffold     = "scaffold"
	TriggerMentionReply = "mention-reply"
)

var validTriggers = map[string]struct{}{
	TriggerIssueTriage: {}, TriggerOnboarding: {},
	TriggerScaffold: {}, TriggerMentionReply: {},
}

// Metadata key names as they appear in LiteLLM spend logs.
const (
	KeyProject    = "gonk_project"
	KeyRig        = "gonk_rig"
	KeyBeadID     = "gonk_bead_id"
	KeySessionKey = "gonk_session_key"
	KeyRung       = "gonk_rung"
	KeyAttempt    = "gonk_attempt"
	KeyTrigger    = "gonk_trigger"
)

type Tags struct {
	Project    string // GitLab path_with_namespace
	Rig        string
	BeadID     string
	SessionKey string
	Rung       string
	Attempt    int // 1-based
	Trigger    string
}

func (t Tags) Validate() error {
	for name, v := range map[string]string{
		"project": t.Project, "rig": t.Rig, "bead_id": t.BeadID,
		"session_key": t.SessionKey, "rung": t.Rung,
	} {
		if v == "" {
			return fmt.Errorf("atags: %s empty", name)
		}
	}
	if t.Attempt < 1 {
		return fmt.Errorf("atags: attempt %d < 1", t.Attempt)
	}
	if _, ok := validTriggers[t.Trigger]; !ok {
		return fmt.Errorf("atags: unknown trigger %q", t.Trigger)
	}
	return nil
}

func (t Tags) Metadata() map[string]string {
	return map[string]string{
		KeyProject: t.Project, KeyRig: t.Rig, KeyBeadID: t.BeadID,
		KeySessionKey: t.SessionKey, KeyRung: t.Rung,
		KeyAttempt: strconv.Itoa(t.Attempt), KeyTrigger: t.Trigger,
	}
}

func FromMetadata(m map[string]string) (Tags, error) {
	attempt, err := strconv.Atoi(m[KeyAttempt])
	if err != nil {
		return Tags{}, fmt.Errorf("atags: attempt: %w", err)
	}
	t := Tags{
		Project: m[KeyProject], Rig: m[KeyRig], BeadID: m[KeyBeadID],
		SessionKey: m[KeySessionKey], Rung: m[KeyRung],
		Attempt: attempt, Trigger: m[KeyTrigger],
	}
	return t, t.Validate()
}
```

- [x] **Step 4: Run tests**

Run: `go test ./pkg/atags/ -race -v`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(atags): attribution tags contract with metadata round trip"
```

---

### Task 8: Full-repo verification and plan index update

- [x] **Step 1: Run everything CI will run**

Run: `go vet ./... && go test ./... -race -count=1`
Expected: PASS. Then, if golangci-lint is available locally: `golangci-lint run ./...` (otherwise CI is the gate).

- [x] **Step 2: Update `PLAN.md`** — set plan 01 Status to `done`.

- [x] **Step 3: Commit**

```bash
git add -A && git commit -m "chore: complete plan 01 (foundation and config contract)"
```

**Definition of done for Plan 01:** local `go vet` + tests + lint green (Task 8 Step 1 runs exactly what CI runs); `gonkcfg.Load` + `Resolve` and `atags` usable by Plans 02/03; schema published and drift-gated; pushed to `https://gitlab.orac.local/agentic/gonk-project` with the `lint` and `test` CI jobs green. If no GitLab runner is available on the instance, CI-green is recorded as deferred with the local equivalents (`go vet`, `go test -race`, `golangci-lint run`) as the standing gate.
