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
  gonk.supervisorURL -- where intake POSTs orders. DERIVED from the in-chart
  controller Service when the controller is bundled, else the operator-supplied
  external URL. The port is smoke-gated (gascity.supervisorPort, Task 0.5).
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
