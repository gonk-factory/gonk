{{/*
  gonk.guards -- every fail-closed check the JSON Schema cannot express.

  Read the messages carefully before you change one: each says WHAT breaks, not
  just that something is wrong. A guard nobody can act on is a guard that gets
  disabled.
*/}}
{{- define "gonk.guards" -}}

{{- /* Build the set of rung names in the catalog. */ -}}
{{- $catalog := dict -}}
{{- range .Values.operatorConfig.rungs -}}
  {{- $_ := set $catalog .name . -}}
{{- end -}}

{{- /* G2: every rung named in ANY ladder must be priced by the catalog. */ -}}
{{- $ladders := dict "instance" .Values.operatorConfig.instance.ladder -}}
{{- range $g, $p := .Values.operatorConfig.groups -}}
  {{- if $p.ladder -}}{{- $_ := set $ladders (printf "group %s" $g) $p.ladder -}}{{- end -}}
{{- end -}}
{{- range $where, $ladder := $ladders -}}
  {{- range $ladder -}}
    {{- if not (hasKey $catalog .) -}}
      {{- fail (printf "operatorConfig: the %s ladder names rung %q, which is not in the rung catalog (operatorConfig.rungs). gonk-meter cannot price a rung it does not know, and will refuse to start." $where .) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- /* G3/G4 belt-and-braces: the schema enforces these, but a `--set-json` of a
       whole rung can smuggle a shape past a conditional if the schema is ever
       relaxed. These two invariants are the difference between a budget and a
       suggestion, so check them twice. */ -}}
{{- range .Values.operatorConfig.rungs -}}
  {{- if eq .kind "local" -}}
    {{- if not (gt (float64 (default 0 .synthetic_usd_per_1m_tokens)) 0.0) -}}
      {{- fail (printf "operatorConfig: local rung %q has no synthetic_usd_per_1m_tokens > 0. Local inference is free, so LiteLLM's USD virtual-key ceiling would never move, and monthly_tokens would have NO hard enforcement anywhere." .name) -}}
    {{- end -}}
    {{- if ne (float64 (default 0 .est_cost_usd)) 0.0 -}}
      {{- fail (printf "operatorConfig: local rung %q has est_cost_usd != 0. Local rungs cost no real money; a non-zero real cost would charge projects for inference they are not billed for." .name) -}}
    {{- end -}}
  {{- else if eq .kind "cloud" -}}
    {{- if not (gt (float64 (default 0 .est_cost_usd)) 0.0) -}}
      {{- fail (printf "operatorConfig: cloud rung %q has est_cost_usd <= 0. gonk-meter applies the cost gate ONLY to priced rungs, so an unpriced cloud rung is free money: it would sail past a $0 budget." .name) -}}
    {{- end -}}
    {{- if .synthetic_usd_per_1m_tokens -}}
      {{- fail (printf "operatorConfig: cloud rung %q has a synthetic_usd_per_1m_tokens. A cloud rung's price is REAL and lives in LiteLLM's own model config; declaring a synthetic price for it would be a lie." .name) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}

{{- /* G5: the onboarding MR's default .gonk.yml names exactly one rung. */ -}}
{{- if not (has .Values.onboarding.defaultRung .Values.operatorConfig.instance.ladder) -}}
  {{- fail (printf "onboarding.defaultRung is %q but the instance ladder is [%s]. The onboarding MR ships a .gonk.yml naming that rung, and a rung outside the instance allow-list resolves to an empty ladder -- so every newly-onboarded project would resolve to `disabled` and nothing would ever run (ADR-002)." .Values.onboarding.defaultRung (join ", " .Values.operatorConfig.instance.ladder)) -}}
{{- end -}}

{{- /* Former G6 is VOID (Task 0 reconciliation): Task 0b's spike ran, Dolt
       failed, the ledger moved to Postgres, and ADR-004 records that
       ReserveIfFits' atomicity is now enforced in the database (SELECT ...
       FOR UPDATE + a partial UNIQUE index), across replicas -- proven by
       TestReserveIfFitsRace / TestReserveIsIdempotentAcrossReplicas with no
       service-side mutex in the way. AD-10 (single-replica) is lifted. There
       is deliberately no fail() here and no unsafeAllowMultipleReplicas
       value: meter.replicaCount is a plain resource knob now. */ -}}

{{- /* G8: intake must have a meter to ask, or it would dispatch unmetered work. */ -}}
{{- if and .Values.intake.enabled (not .Values.meter.enabled) -}}
  {{- fail "intake.enabled is true but meter.enabled is false. gonk-intake asks gonk-meter whether a project may spend before it fires an order; with no meter it would dispatch unmetered work. Deploy meter, or disable intake." -}}
{{- end -}}

{{- /* G9/G17: orders that go nowhere, and a BYO controller with no address.
       When gascity.enabled=true the supervisor URL is DERIVED from the in-chart
       controller Service, so it need not be set. It is required ONLY when the
       controller is external (enabled=false) and dispatch is http. */ -}}
{{- if and .Values.intake.enabled (eq .Values.gascity.dispatch "http") (not .Values.gascity.enabled) (not .Values.gascity.supervisorURL) -}}
  {{- fail "gascity.dispatch is \"http\" and gascity.enabled is false (BYO controller), but gascity.supervisorURL is empty. Either set gascity.enabled=true to deploy the bundled controller, set gascity.supervisorURL to your external supervisor, or set gascity.dispatch=\"log\" DELIBERATELY (a dry run: orders are logged and never executed)." -}}
{{- end -}}

{{- /* G16: a disabled Dolt with no external server. Beads is Dolt-ONLY -- there is
       no Postgres path for it -- so the only alternative to the bundled server is
       an external Dolt (e.g. a self-hosted DoltLab). */ -}}
{{- if and (not .Values.dolt.enabled) (not .Values.dolt.external.host) -}}
  {{- fail "dolt.enabled is false but dolt.external.host is empty. The Gas City beads store is Dolt-ONLY (there is no beads-on-Postgres option), so a disabled bundled Dolt REQUIRES an external Dolt SQL server (e.g. self-hosted DoltLab). Set dolt.external.host/port, or re-enable dolt.enabled=true." -}}
{{- end -}}

{{- /* G19, RECONCILED (Task 0): the ledger is ALWAYS Postgres now (there is no
       "dolt" position on ledger.backend -- ADR-004 Decision 10), and UNLIKE the
       bundled Dolt (which derives its DSN from the in-chart Service + the
       shipped root/empty/--no-tls auth), NONE of the three Postgres modes
       (shared/cnpg/external) auto-derive a DSN the chart can wire up on its
       own -- `mode: cnpg` renders a Cluster CR, but its generated app Secret
       still has to be pointed at via secrets.ledger.existingSecret by hand
       (Task 6 Step 5's own NOTE), and `shared`/`external` render nothing at
       all. So every mode needs a real DSN Secret, and an empty
       secrets.ledger.existingSecret is fatal in all three, not just one. */ -}}
{{- if not .Values.secrets.ledger.existingSecret -}}
  {{- fail "secrets.ledger.existingSecret is empty. gonk-meter's ledger is Postgres/CNPG (ADR-004; there is no dolt-backed ledger any more), and none of the three ledger.postgres.mode values (shared, cnpg, external) derive a DSN automatically the way the bundled Dolt used to -- even mode=cnpg's generated Cluster Secret must be pointed at explicitly. Provision the ledger DSN Secret (see chart/gonk/README.md and Task 6 Step 5b's four-object gitops recipe for mode=shared)." -}}
{{- end -}}

{{- /* G10: a NetworkPolicy that selects nothing, or everything. */ -}}
{{- if .Values.networkPolicy.enabled -}}
  {{- if not .Values.networkPolicy.agentPodSelector -}}
    {{- fail "networkPolicy.enabled is true but networkPolicy.agentPodSelector is empty. An egress NetworkPolicy with an empty podSelector selects EVERY pod in the namespace. Worse, the agent-session policy is what makes spec 9's \"agent pods reach only GitLab and LiteLLM, so budgets cannot be bypassed\" true -- and gonk does not create those pods (Gas City's session provider does), so the chart cannot infer their labels. Set the selector to match them. NOTE: the chart cannot detect a WRONG selector, only an empty one. AND NOTE: NetworkPolicy is NOT ENFORCED on this cluster today (Flannel does not implement it; the Cilium HelmRelease is suspended), so this policy currently blocks NOTHING and local-model budgets are ADVISORY. Plan 06 carries the egress-denial test, SKIPPED until Cilium lands; un-skipping it is the gate." -}}
  {{- end -}}
  {{- if and (not .Values.networkPolicy.gitlab.cidrs) (not .Values.networkPolicy.gitlabInCluster.enabled) -}}
    {{- fail "networkPolicy.enabled is true but neither networkPolicy.gitlab.cidrs nor networkPolicy.gitlabInCluster is set. A NetworkPolicy egress rule cannot name a DNS host; it needs an ipBlock CIDR (or a selector, if GitLab is in-cluster). Without one, gonk-intake cannot reach GitLab at all." -}}
  {{- end -}}
{{- end -}}

{{- /* G12 */ -}}
{{- if .Values.ingress.enabled -}}
  {{- if not .Values.ingress.host -}}{{- fail "ingress.enabled is true but ingress.host is empty." -}}{{- end -}}
  {{- if not .Values.ingress.className -}}{{- fail "ingress.enabled is true but ingress.className is empty." -}}{{- end -}}
  {{- if ne .Values.ingress.path "/hook/gitlab" -}}
    {{- fail (printf "ingress.path is %q. The public listener serves POST /hook/gitlab and nothing else; widening the ingress path cannot expose anything useful, but it CAN expose the private listener if you also change the backend port. Leave it alone." .Values.ingress.path) -}}
  {{- end -}}
{{- end -}}

{{- /* The bundled Dolt must have a pinned image (it is tier-0 state -- a floating
       tag is not a reproducible deploy). RECONCILED, Task 0: it backs the beads
       store ONLY now (never the ledger -- ADR-004 Decision 10), but that changes
       nothing about why it must be pinned. */ -}}
{{- if and .Values.dolt.enabled (not .Values.dolt.image.tag) -}}
  {{- fail "dolt.enabled is true but dolt.image.tag is empty. The bundled Dolt is tier-0 state (the Gas City beads store); pin an exact image tag (no default, never `latest`)." -}}
{{- end -}}

{{- /* The bundled controller must have a pinned image when it is deployed. */ -}}
{{- if and .Values.gascity.enabled (not .Values.gascity.image.tag) -}}
  {{- fail "gascity.enabled is true but gascity.image.tag is empty. The chart deploys the Gas City controller (Plan 04's gonk-controller image); pin an exact tag (no default, never `latest`)." -}}
{{- end -}}

{{- /* G21: the bundled controller's supervisor port is NOT operator-tunable.
       Task 0.5's smoke proved the per-city [api] listener binds 0.0.0.0:9443
       hardcoded by `gc init --bootstrap-profile k8s-cell` (no env or flag moves
       it), while 8372 is the machine-wide supervisor API bound to 127.0.0.1 only.
       So the Service MUST target 9443: any other value renders a controller
       Service that connection-refuses (and 8372 specifically would advertise the
       loopback admin port). This value exists only so the doctrine is visible in
       values.yaml; the guard is what keeps it honest. See smoke/gc-controller-smoke.md. */ -}}
{{- if and .Values.gascity.enabled (ne (int .Values.gascity.supervisorPort) 9443) -}}
  {{- fail (printf "gascity.supervisorPort is %d but the bundled Gas City controller's per-city [api] listener is hardcoded to 0.0.0.0:9443 by `gc init --bootstrap-profile k8s-cell` -- it is not tunable. A Service on any other port connection-refuses against the pod (and 8372 is the 127.0.0.1-only supervisor admin API that must never be exposed). Leave gascity.supervisorPort at 9443. See chart/gonk/smoke/gc-controller-smoke.md." (int .Values.gascity.supervisorPort)) -}}
{{- end -}}
{{- end -}}
