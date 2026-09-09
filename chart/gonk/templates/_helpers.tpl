{{/* Standard names. */}}
{{- define "gonk.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gonk.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "gonk.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "gonk.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: gonk
{{- end -}}

{{/* selectorLabels COMPONENT -- must be stable across upgrades. */}}
{{- define "gonk.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gonk.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
  gonk.networkPolicyBanner -- the "NOT ENFORCED" disclaimer, as a real
  ANNOTATION, not just a spec-block comment.

  Helm comments never reach the API server (they are stripped at template
  render time, long before `kubectl apply`), so `kubectl get networkpolicy -o
  yaml` on the real cluster would show NONE of the surrounding spec comments
  -- an operator inspecting the live object would see nothing warning them
  that it blocks nothing. An annotation is real object data: it survives all
  the way to etcd and back out through `kubectl get/describe`. Every
  NetworkPolicy template carries this AND keeps its own spec-block prose
  comment for anyone reading the chart source or a `helm template` dry run.
*/}}
{{- define "gonk.networkPolicyBanner" -}}
gonk.orac.local/network-policy-enforcement: "NOT ENFORCED on this cluster: Flannel does not implement NetworkPolicy, and the Cilium HelmRelease that would (gitops:clusters/orac/foundation/kustomization.yaml) is SUSPENDED. This object blocks nothing today; see chart/gonk/README.md."
{{- end -}}

{{/*
  image DICT{ctx,image} -> registry-qualified reference.

  RECONCILED, Task 0: `.image.registry`, if set on the SPECIFIC image dict,
  overrides the global `.Values.image.registry` default. This is required for
  `dolt.image` (docker.io/dolthub/dolt-sql-server) — Dolt is a third-party
  upstream image, never rebuilt into registry.orac.local/agentic/gonk-project,
  so it must NOT inherit the four first-party images' in-cluster registry.
  Without this override the helper previously produced
  `registry.orac.local/agentic/gonk-project/dolthub/dolt-sql-server:<tag>`,
  which the in-cluster registry can never serve. gonk's own four images
  (gonk-agent, gonk-intake, gonk-meter, gonk-controller) leave `.image.registry`
  unset and fall through to the global default, exactly as before.
*/}}
{{- define "gonk.image" -}}
{{- $reg := .ctx.Values.image.registry -}}
{{- if .image.registry -}}{{ $reg = .image.registry }}{{- end -}}
{{- if $reg -}}{{ printf "%s/%s:%s" (trimSuffix "/" $reg) .image.repository .image.tag }}
{{- else -}}{{ printf "%s:%s" .image.repository .image.tag }}
{{- end -}}
{{- end -}}

{{/*
  gonk.secretVolume DICT{name, secret, keys}
  A projected volume, mode 0400. optional is FALSE everywhere on purpose: a
  missing Secret or a missing key must make the pod fail to start LOUDLY, not
  start with an empty credential file.
*/}}
{{- define "gonk.secretVolume" -}}
- name: secret-{{ .name }}
  projected:
    defaultMode: 0400
    sources:
      - secret:
          name: {{ .secret }}
          items:
{{- range .keys }}
            - key: {{ . }}
              path: {{ . }}
{{- end }}
{{- end -}}

{{- define "gonk.secretMount" -}}
- name: secret-{{ .name }}
  mountPath: {{ .root }}/{{ .name }}
  readOnly: true
{{- end -}}

{{/* Where a given secret key lands on disk. NEVER an env VALUE -- only a PATH. */}}
{{- define "gonk.secretPath" -}}
{{ .root }}/{{ .name }}/{{ .key }}
{{- end -}}

{{/* ---- umbrella component wiring helpers (Tasks 5, 6, 6.5) ---- */}}

{{/*
  gonk.gcWriteKeyID CTX -- the signing kid (GONK_GC_WRITE_KEY_ID), DERIVED from
  the first entry of gascity.writeAuth.verifyKey ("kid:base64[,kid2:base64]").
  Deriving it here GUARANTEES the private-key kid the client sends matches the
  public key the controller verifies with -- a hand-set second value could drift
  and every grant would 403. Standard base64 never contains ':', so splitting the
  first entry on ':' yields [kid, base64] and its first element is the kid.
*/}}
{{- define "gonk.gcWriteKeyID" -}}
{{- $first := index (splitList "," .Values.gascity.writeAuth.verifyKey) 0 -}}
{{- index (splitList ":" $first) 0 -}}
{{- end -}}

{{/*
  gonk.gcWriteEnv CTX -- the write-auth SIGNING env shared by gonk-intake and the
  in-controller gonk-gate. Points GONK_GC_WRITE_KEY_FILE at the 0400 file mount
  (NEVER an env value), sets the kid derived from the public verifyKey, and passes
  the optional tenancy cid. The key material itself is mounted via
  gonk.secretVolume/gonk.secretMount with name "gc-write-key".
*/}}
{{- define "gonk.gcWriteEnv" -}}
- name: GONK_GC_WRITE_KEY_FILE
  value: {{ include "gonk.secretPath" (dict "root" .Values.secrets.mountRoot "name" "gc-write-key" "key" .Values.secrets.gcWriteKey.key) | quote }}
- name: GONK_GC_WRITE_KEY_ID
  value: {{ include "gonk.gcWriteKeyID" . | quote }}
{{- if .Values.gascity.writeAuth.cid }}
- name: GONK_GC_WRITE_CID
  value: {{ .Values.gascity.writeAuth.cid | quote }}
{{- end }}
{{- end -}}

{{/*
  gonk.supervisorURL -- where intake POSTs orders. DERIVED from the in-chart
  controller Service when the controller is bundled, else the operator-supplied
  external URL. The port is SETTLED at 9443 (gascity.supervisorPort, smoke U1).

  IMAGE-GATED (bead gonk-fsl): the http-vs-https scheme on 9443 was NOT
  socket-confirmed -- the image cannot finish city init yet, so the 9443 listener
  never bound during the Task 0.5 smoke. `http` is the smoke doc's default; re-check
  the scheme once the image can boot a city.
*/}}
{{- define "gonk.supervisorURL" -}}
{{- if .Values.gascity.enabled -}}
http://gonk-controller.{{ .Release.Namespace }}.svc:{{ .Values.gascity.supervisorPort }}
{{- else -}}
{{ .Values.gascity.supervisorURL }}
{{- end -}}
{{- end -}}

{{/* gonk.doltHost / gonk.doltPort -- the bundled Dolt Service, or the external one.
     Consumed by the controller (beads) and, via the DSN, by meter (ledger). */}}
{{- define "gonk.doltHost" -}}
{{- if .Values.dolt.enabled -}}
gonk-dolt.{{ .Release.Namespace }}.svc
{{- else -}}
{{ .Values.dolt.external.host }}
{{- end -}}
{{- end -}}

{{- define "gonk.doltPort" -}}
{{- if .Values.dolt.enabled -}}{{ .Values.dolt.port }}{{- else -}}{{ .Values.dolt.external.port }}{{- end -}}
{{- end -}}

{{/*
  gonk.controllerEnv CTX -- the env SHARED by the controller's bootstrap
  initContainer and its `gc supervisor run` main container.

  SETTLED by Task 0.5 (smoke/gc-controller-smoke.md), NOT the plan draft:
  - HOME=/home/gonk on a writable volume -- the image's passwd home for uid 65532
    is `/` (read-only), so gc cannot write ~/.gc without this (smoke U2).
  - GC_DOLT_HOST/PORT point beads at the bundled/external Dolt.
  - The session provider is NOT an env var. GC_SESSION_PROVIDER DOES NOT EXIST in
    Gas City -- the string appears nowhere in its source at GASCITY_REF. Setting it
    did nothing, the runtime silently defaulted to tmux, and sessions "started"
    successfully while logging `tmux server unreachable: no tmux server running`
    and never creating a pod. The real selector is city.toml's `[session] provider`
    (config.SessionConfig, `toml:"session"`), written by the bootstrap
    initContainer. Only the GC_K8S_* DETAILS (image/namespace/SA/prebaked) are env.
  - The draft's GC_DAEMON_SUPERVISOR_BIND / _ALLOW_MUTATIONS are OMITTED on
    purpose: smoke U1 proved no env moves the bind. The 0.0.0.0:9443 [api] bind and
    allow_mutations=true come from `gc init --bootstrap-profile k8s-cell`, never env.
*/}}
{{- define "gonk.controllerEnv" -}}
- name: HOME
  value: /home/gonk
- name: GC_DOLT_HOST
  value: {{ include "gonk.doltHost" . | quote }}
- name: GC_DOLT_PORT
  value: {{ include "gonk.doltPort" . | quote }}
# R-48/T-26: the controller connects to Dolt as the `gc` user (dolt.gcUser.
# username), never root -- GC_DOLT_USER is not a credential (just a name), so
# unlike GC_DOLT_PASSWORD it can be a plain value here. `gc init` picks up
# GC_DOLT_USER as its --dolt-user fallback and `gc start`'s bd bridge mirrors
# it into BEADS_DOLT_SERVER_USER (cmd/gc/bd_env.go's mirrorBeadsDoltServerEnv
# at GASCITY_REF) -- no --dolt-user flag is needed at either call site.
# GC_DOLT_PASSWORD is set separately by each caller's own command wrapper,
# `cat`ing the FILE-mounted secrets.dolt.keys.gcPassword Secret key -- it
# cannot live here because this helper is plain `env:` entries, and the
# password can never be a chart-visible value (see the same helper's callers).
- name: GC_DOLT_USER
  value: {{ .Values.dolt.gcUser.username | quote }}
# GC_SESSION -- the REAL session-provider env override. cmd/gc's
# effectiveProviderName(cfg.Session.Provider) returns $GC_SESSION when set, so
# this is the documented way to force a provider from the environment. (The
# chart previously set GC_SESSION_PROVIDER, which EXISTS NOWHERE in Gas City;
# the runtime silently fell back to tmux and never created a pod.) Belt and
# braces with city.toml's [session] provider, written by the bootstrap: the
# TOML is the durable declaration, this is the override the supervisor reads.
- name: GC_SESSION
  value: k8s
{{- if .Values.gitlab.caCert.existingConfigMap }}
- name: SSL_CERT_FILE
  value: {{ .Values.gitlab.caCert.mountPath | quote }}
{{- end }}
{{- end -}}

{{/*
  gonk.controllerMounts CTX -- the volumeMounts SHARED by the bootstrap
  initContainer and the supervisor. /city and /home/gonk are WRITABLE emptyDirs
  (smoke U2: gc init writes the city into /city, gc writes ~/.gc under HOME).
*/}}
{{- define "gonk.controllerMounts" -}}
- name: city
  mountPath: /city
- name: home
  mountPath: /home/gonk
- name: tmp
  mountPath: /tmp
{{- if .Values.gitlab.caCert.existingConfigMap }}
- name: orac-ca
  mountPath: {{ .Values.gitlab.caCert.mountPath }}
  subPath: {{ .Values.gitlab.caCert.key }}
  readOnly: true
{{- end }}
{{- end -}}

{{/*
  gonk.defaultRungModel -- the model of onboarding.defaultRung in the operator's
  rung catalog, or "" when the rung is not found.

  This is the STATIC per-install model a resident agent session renders its
  opencode overlay with at startup. It exists because Gas City launches agent
  pods as POOL sessions with no prompt attached, so there is no per-session model
  at the moment the harness needs one to write its config. A prompt marker still
  overrides it per session once a bead is assigned (pack/formulas/gonk-triage.toml).

  THE PACK STILL NAMES NO MODEL. This reads the OPERATOR's catalog
  (operatorConfig.rungs, the same values gonk-meter serves from), so the model
  keeps coming from the rung catalog and never from the pack.
*/}}
{{- define "gonk.defaultRungModel" -}}
{{- $want := .Values.onboarding.defaultRung -}}
{{- range .Values.operatorConfig.rungs -}}
{{- if eq .name $want }}{{ .model }}{{ end -}}
{{- end -}}
{{- end -}}

{{/*
gonk.rigBaseURL -- where an agent pod fetches its per-session CHECKOUT.

gonk-intake's PRIVATE listener, by Service DNS. This is the per-INSTALL half of
the URL; the pod appends its own GC_ALIAS (which Gas City already puts in every
agent pod's env) to get the per-session half. That split is what lets a pooled,
generic pod fetch exactly its own tree without any per-session channel.

Empty when intake is disabled -- the entrypoint then simply grants no checkout
and the agent falls back to controller-side repository context (gonk-msz).
*/}}
{{- define "gonk.rigBaseURL" -}}
{{- if .Values.intake.enabled -}}
http://gonk-intake-internal.{{ .Release.Namespace }}.svc:{{ .Values.intake.ports.private }}
{{- end -}}
{{- end -}}

{{/*
gonk.promptBaseURL -- where an agent pod fetches its own PROMPT (gonk-mzd).

The pod pulls instead of having its prompt typed into a TUI. Delivery by
keystroke was unreliable and, worse, silent: the pod carried
GC_STARTUP_PROMPT_DELIVERED=1 even when the composer was empty.

Not a credential, and deliberately so -- there is no token to distribute here.
The capability is the 128-bit nonce in GC_ALIAS, which Gas City already places
in every agent pod's environment, so the pod composes the URL itself.

Injected explicitly rather than relying on GONK_METER_SERVICE_HOST/_PORT: the
kubelet only injects service env vars when the Service predates the pod, which
is a startup-ordering dependency nobody should have to reason about.
*/}}
{{- define "gonk.promptBaseURL" -}}
{{- if .Values.meter.enabled -}}
http://gonk-meter.{{ .Release.Namespace }}.svc:{{ .Values.meter.port }}
{{- end -}}
{{- end -}}
