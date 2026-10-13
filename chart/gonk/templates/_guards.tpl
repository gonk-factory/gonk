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
  {{- fail "secrets.ledger.existingSecret is empty. gonk-meter's ledger is Postgres/CNPG (ADR-004; there is no dolt-backed ledger any more), and none of the three ledger.postgres.mode values (shared, cnpg, external) derive a DSN automatically the way the bundled Dolt used to -- even mode=cnpg's generated Cluster Secret must be pointed at explicitly. Provision the ledger DSN Secret (see chart/gonk/README.md, section 7, The ledger)." -}}
{{- end -}}

{{- /* R-48/T-26 (interim until T-56 deletes Dolt): Dolt is not root-open.
       G16 above already guarantees SOME Dolt endpoint always exists (bundled
       or dolt.external.host), so secrets.dolt.existingSecret is required
       unconditionally, not just when dolt.enabled -- an external Dolt still
       needs the gc-password key for the controller to authenticate as
       anything other than a credential this chart would otherwise have to
       generate or default, which is the exact thing this task closes. */ -}}
{{- if not .Values.secrets.dolt.existingSecret -}}
  {{- fail "secrets.dolt.existingSecret is empty. The bundled Dolt's root password (root-password) and the `gc` user's password (gc-password) both live in this Secret -- the chart creates and defaults NEITHER, on purpose (R-48: Dolt used to be root/no-password/reachable-from-any-pod). An external Dolt (dolt.enabled=false) still needs the gc-password key: it is what the controller authenticates with. Provision the Secret (see chart/gonk/README.md's secret table) and set secrets.dolt.existingSecret." -}}
{{- end -}}

{{- /* G10: a NetworkPolicy that selects nothing, or everything. */ -}}
{{- if .Values.networkPolicy.enabled -}}
  {{- if not .Values.networkPolicy.agentPodSelector -}}
    {{- fail "networkPolicy.enabled is true but networkPolicy.agentPodSelector is empty. An egress NetworkPolicy with an empty podSelector selects EVERY pod in the namespace. Worse, the agent-session policy is what makes spec 9's \"agent pods reach only GitLab and LiteLLM, so budgets cannot be bypassed\" true -- and the chart does not create those pods, so it cannot infer their labels. Set the selector to match them. NOTE: the chart cannot detect a WRONG selector, only an empty one. AND NOTE: NetworkPolicy is enforced only where the cluster's network plugin implements it; where it does not, this policy blocks NOTHING and local-model budgets are ADVISORY. `helm test` reports which you have." -}}
  {{- end -}}
  {{- if and (not .Values.networkPolicy.gitlab.cidrs) (not .Values.networkPolicy.gitlabInCluster.enabled) -}}
    {{- fail "networkPolicy.enabled is true but neither networkPolicy.gitlab.cidrs nor networkPolicy.gitlabInCluster is set. A NetworkPolicy egress rule cannot name a DNS host; it needs an ipBlock CIDR (or a selector, if GitLab is in-cluster). Without one, gonk-intake cannot reach GitLab at all." -}}
  {{- end -}}
{{- end -}}

{{- /* G10b (gonk-vxel): the API server allowance must stay an allowance.
       networkPolicy.kubeAPI.cidrs becomes an ipBlock egress rule on the two
       pods that hold the forge token and the LiteLLM admin key. A /0 there is
       egress to every address on the API port, which is the default-deny
       undone; and a rule with no port is every port on those addresses.
       So is 0.0.0.0/1 beside 128.0.0.0/1, which a check for a literal /0
       let through until chart 0.2.4: the floor is a prefix length, /8 for
       IPv4 and /16 for IPv6. A floor, not a proof -- 256 entries of /8 are
       everything too -- but no control plane is wider than that and nobody
       writes those by accident. */ -}}
{{- if and .Values.networkPolicy.enabled .Values.networkPolicy.kubeAPI.cidrs -}}
  {{- range .Values.networkPolicy.kubeAPI.cidrs -}}
    {{- $parts := splitList "/" . -}}
    {{- $floor := ternary 16 8 (contains ":" .) -}}
    {{- if and (eq (len $parts) 2) (lt (int (last $parts)) $floor) -}}
      {{- fail (printf "networkPolicy.kubeAPI.cidrs contains %q, a prefix shorter than /%d. An entry that wide (a /0, or one half of the address space) allows the controller and the meter to reach nearly EVERY address on the API port, which undoes the default-deny policy for the two pods that hold the most credentials. Name the Kubernetes API server's own addresses (the endpoints of the `kubernetes` Service in namespace default), each as a /32 or /128, or the control-plane subnet. See values.yaml." . $floor) -}}
    {{- end -}}
  {{- end -}}
  {{- if not .Values.networkPolicy.kubeAPI.ports -}}
    {{- fail "networkPolicy.kubeAPI.cidrs is set but networkPolicy.kubeAPI.ports is empty. An ipBlock rule with no port allows EVERY port on those addresses. Set the port the Kubernetes API server's endpoints listen on (6443 on most self-managed clusters, 443 on most managed ones)." -}}
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

{{- /* G25: THE PRIVATE CA IS BOTH VALUES OR NEITHER. gitlab.caCert.existingConfigMap
       and .key ship EMPTY (no CA mounted; the system trust store is used). The
       templates mount the CA only when existingConfigMap is set, and mount
       exactly `key` out of it. So each half alone is a silent fault:
         - a ConfigMap with no key mounts with an empty subPath -- a directory
           where the trust directory expects a file -- and every TLS call fails at run
           time with a message about certificates, not about this value;
         - a key with no ConfigMap mounts NOTHING, and a deployment that meant
           to trust a private CA starts without it and fails the same way.
       The second is the one an operator hits by overriding half of what used
       to be a default. Refuse both here, where the message can name the value. */ -}}
{{- $ca := .Values.gitlab.caCert | default dict -}}
{{- if and $ca.existingConfigMap (not $ca.key) -}}
  {{- fail (printf "gitlab.caCert.existingConfigMap is %q but gitlab.caCert.key is empty. Name the key inside that ConfigMap that holds the CA bundle (for example ca.crt); the chart mounts exactly that key at gitlab.caCert.mountPath. Set both, or neither." $ca.existingConfigMap) -}}
{{- end -}}
{{- if and $ca.key (not $ca.existingConfigMap) -}}
  {{- fail (printf "gitlab.caCert.key is %q but gitlab.caCert.existingConfigMap is empty, so NO CA would be mounted and every workload would start without the private CA it was configured to trust. Name the ConfigMap in the release namespace that holds the bundle. Set both, or neither." $ca.key) -}}
{{- end -}}
{{- if and $ca.existingConfigMap (not $ca.mountPath) -}}
  {{- fail "gitlab.caCert.existingConfigMap is set but gitlab.caCert.mountPath is empty. The bundle is mounted at mountPath and its directory is added to SSL_CERT_DIR; an empty one mounts nothing a workload can read." -}}
{{- end -}}

{{- /* The bundled Dolt must have a pinned image (it is tier-0 state -- a floating
       tag is not a reproducible deploy). RECONCILED, Task 0: it backs the beads
       store ONLY now (never the ledger -- ADR-004 Decision 10), but that changes
       nothing about why it must be pinned. */ -}}
{{- if and .Values.dolt.enabled (not .Values.dolt.image.tag) -}}
  {{- fail "dolt.enabled is true but dolt.image.tag is empty. The bundled Dolt is tier-0 state (the Gas City beads store); pin an exact image tag (no default, never `latest`)." -}}
{{- end -}}

{{- /* G24 (release-unit, 2026-09-26): every gonk image the release will run
       resolves to a NON-EMPTY tag -- its own .image.tag, else releaseTag.
       gonk.releaseTag fails with the value to set. Checked here, for every
       enabled component, so the refusal does not depend on which template
       helm happens to render first. The agent image is checked whenever the
       controller is bundled: the controller is what hands it to session pods
       (GC_K8S_IMAGE), and an empty tag there is InvalidImageName on the FIRST
       SESSION, not on this release. Supersedes the old "gascity.image.tag is
       empty" guard, which could not see releaseTag. */ -}}
{{- $releaseImages := list
      (dict "enabled" .Values.gascity.enabled "image" .Values.gascity.image "value" "gascity.image.tag")
      (dict "enabled" .Values.intake.enabled "image" .Values.intake.image "value" "intake.image.tag")
      (dict "enabled" .Values.meter.enabled "image" .Values.meter.image "value" "meter.image.tag")
      (dict "enabled" .Values.gascity.enabled "image" .Values.componentImages.agent "value" "componentImages.agent.tag") -}}
{{- range $releaseImages -}}
  {{- if .enabled -}}
    {{- $_ := include "gonk.releaseTag" (dict "ctx" $ "image" .image "value" .value) -}}
  {{- end -}}
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

{{- /* G22: the bundled controller runs `gc start --foreground` -- a per-city [api]
       plane bound 0.0.0.0 with allow_mutations=true and any-host (no allowed_hosts).
       That is an UNAUTHENTICATED mutation endpoint unless write-auth is on, and the
       NetworkPolicy that would restrict it is enforced only where the CNI
       implements NetworkPolicy. So both halves of ed25519 grant-gating are REQUIRED when
       the controller is bundled: the PUBLIC verify key (the controller checks
       grants) AND the PRIVATE signing key Secret (intake + the in-pod gonk-gate mint
       grants). Missing either => any pod that can route to gonk-controller:9443 could
       POST an order and trigger budgeted work. There is deliberately no
       allowUnauthenticated escape hatch. Re-smoke proof: chart/gonk/smoke/gc-controller-smoke.md. */ -}}
{{- if .Values.gascity.enabled -}}
  {{- if not .Values.gascity.writeAuth.verifyKey -}}
    {{- fail "gascity.enabled is true but gascity.writeAuth.verifyKey is empty. The bundled controller runs `gc start --foreground` with allow_mutations=true on an 0.0.0.0 any-host [api] listener -- with no verify key that is an UNAUTHENTICATED mutation plane, and any pod that can reach gonk-controller:9443 could POST an order and trigger budgeted work (the NetworkPolicy that would block it is NOT ENFORCED on this cluster). Set gascity.writeAuth.verifyKey to the ed25519 PUBLIC key (\"kid:base64\", standard-base64 raw-32 pubkey) whose PRIVATE half intake+gate sign with. There is deliberately no allowUnauthenticated escape hatch. See chart/gonk/smoke/gc-controller-smoke.md." -}}
  {{- end -}}
  {{- if not .Values.secrets.gcWriteKey.existingSecret -}}
    {{- fail "gascity.enabled is true but secrets.gcWriteKey.existingSecret is empty. gonk-intake and the in-pod gonk-gate must sign every order-run POST with the ed25519 PRIVATE key whose public half is gascity.writeAuth.verifyKey; with no signing key the grant-gated controller rejects every dispatch (401/403) and nothing runs. Provision the key out of band (by hand or from your secret store; PEM PKCS#8 or a raw 32-byte seed) and set secrets.gcWriteKey.existingSecret/key. The chart never creates or reads this Secret -- it is a 0400 file mount only. See chart/gonk/smoke/gc-controller-smoke.md." -}}
  {{- end -}}
{{- end -}}

{{- /* G23: NO CHART RENDERS AGAINST AN IMAGE OLDER THAN IT NEEDS (gonk-xsk3).

       The chart and the images reach the cluster SEPARATELY: the chart the
       moment Chart.yaml's version changes, each image whenever its pipeline
       finishes and Flux image automation notices. On 2026-09-12 chart 0.1.10's
       readinessProbe ran `gonk-gate sweep-health` against an image built before
       sweep-health existed -- `unknown subcommand`, exit 2, an unready
       controller, a failed upgrade and a rollback loop for 13 minutes. Each
       artifact was correct for its own commit. The COMBINATION was not, and
       this render is the only place both halves are visible at once.

       Chart.yaml carries a floor per image: gonk-factory.github.io/image-floor.<name>,
       a full default-branch tag. The floor's build number is compared with the
       build number of the tag being deployed, using the ImagePolicy's OWN
       filterTags pattern (internal/buildgate pins the two together:
       TestTheImageFloorGuardRanksWithTheImagePolicyPattern). A main
       pipeline is created for main's HEAD and main never rewinds, so a higher
       build number is always a descendant commit.

       FAILING HERE IS THE SAFE OUTCOME, NOT A SECOND OUTAGE. A template error
       happens before helm writes a release, so the running release stays
       deployed and no pod is touched; the HelmRelease retries, and renders as
       soon as image automation moves the tag up to the floor.

       A tag with no build number cannot be ranked and is REFUSED unless
       imageFloor.acceptUnrankedTags says the images were built from the same
       commit as this chart. Where image automation selects only rankable tags,
       reaching that branch means a hand pin, and a hand pin should have to
       say so. chart/IMAGE-CONTRACT lists what the chart invokes
       on each image, and internal/buildgate fails when that changes without
       the image's floor going up. It does NOT prove the floor's build has the
       feature; raising the floor to a real tag that does is on the author. */ -}}
{{- $rankable := `^v(?P<ver>\d+\.\d+\.\d+)-(?P<build>\d+)\.[0-9a-f]{12}$` -}}
{{- /* The tag compared is the RESOLVED one (release-unit): a component's own
       tag, else releaseTag. When it came from releaseTag the message says so,
       because that is the value to change. */ -}}
{{- $floorImages := list
      (dict "name" "controller" "enabled" .Values.gascity.enabled "image" .Values.gascity.image "key" "gascity.image.tag")
      (dict "name" "intake" "enabled" .Values.intake.enabled "image" .Values.intake.image "key" "intake.image.tag")
      (dict "name" "meter" "enabled" .Values.meter.enabled "image" .Values.meter.image "key" "meter.image.tag")
      (dict "name" "agent" "enabled" .Values.gascity.enabled "image" .Values.componentImages.agent "key" "componentImages.agent.tag") -}}
{{- range $floorImages -}}
  {{- $_ := set . "tag" (.image.tag | default $.Values.releaseTag | default "") -}}
  {{- $_ := set . "value" (ternary .key (printf "releaseTag (%s unset)" .key) (not (empty .image.tag))) -}}
  {{- $floor := index ($.Chart.Annotations | default dict) (printf "gonk-factory.github.io/image-floor.%s" .name) | default "" -}}
  {{- if and .enabled $floor -}}
    {{- if not (regexMatch $rankable $floor) -}}
      {{- fail (printf "Chart.yaml annotation gonk-factory.github.io/image-floor.%s is %q, which is not a default-branch image tag (v<ver>-<build>.<sha12>). A floor that cannot be ranked protects nothing. Fix the chart." .name $floor) -}}
    {{- end -}}
    {{- $tag := toString .tag -}}
    {{- if regexMatch $rankable $tag -}}
      {{- $have := regexReplaceAll $rankable $tag "${build}" | atoi -}}
      {{- $need := regexReplaceAll $rankable $floor "${build}" | atoi -}}
      {{- if lt $have $need -}}
        {{- fail (printf "IMAGE FLOOR (gonk-xsk3): %s is %s (build %d), but chart %s needs the %s image from build %d or later (floor %s) -- it invokes something that older image does not have. Refused before anything is applied: the running release is untouched. If a default-branch pipeline at build %d or later has published this image, Flux image automation will raise the tag and this release renders on the next reconcile. Do NOT lower the floor to get past this; that is the 2026-09-12 outage." .value $tag $have $.Chart.Version .name $need $floor $need) -}}
      {{- end -}}
    {{- else if not (($.Values.imageFloor).acceptUnrankedTags) -}}
      {{- fail (printf "IMAGE FLOOR (gonk-xsk3): %s is %q, which has no build number, so nothing can tell whether it is at least the build chart %s needs for the %s image (floor %s). If this image was built from the SAME commit as this chart (an e2e run, a release cut from one git tag), set imageFloor.acceptUnrankedTags=true to say so. Otherwise deploy a default-branch tag (v<ver>-<build>.<sha12>)." .value $tag $.Chart.Version .name $floor) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
