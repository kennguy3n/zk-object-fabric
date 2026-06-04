{{/*
Expand the name of the chart.
*/}}
{{- define "zk-object-fabric.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "zk-object-fabric.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version label.
*/}}
{{- define "zk-object-fabric.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "zk-object-fabric.labels" -}}
helm.sh/chart: {{ include "zk-object-fabric.chart" . }}
{{ include "zk-object-fabric.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "zk-object-fabric.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zk-object-fabric.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "zk-object-fabric.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "zk-object-fabric.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding gateway credentials (created or existing).
*/}}
{{- define "zk-object-fabric.secretName" -}}
{{- if .Values.secret.existingSecret }}
{{- .Values.secret.existingSecret }}
{{- else }}
{{- printf "%s-secrets" (include "zk-object-fabric.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Whether a manifest-body encryption key is configured (inline value or an
existing Secret). When true the chart mounts the key file and sets
encryption.manifest_body_key_path. Emits "true" or "".
*/}}
{{- define "zk-object-fabric.manifestBodyKeyConfigured" -}}
{{- $mbk := .Values.config.encryption.manifestBodyKey -}}
{{- if or $mbk.value $mbk.existingSecret -}}true{{- end -}}
{{- end }}

{{/*
Name of the Secret that holds the manifest-body key: an operator-supplied
existingSecret if set, otherwise the chart-created gateway Secret.
*/}}
{{- define "zk-object-fabric.manifestBodyKeySecretName" -}}
{{- $mbk := .Values.config.encryption.manifestBodyKey -}}
{{- if $mbk.existingSecret -}}
{{- $mbk.existingSecret -}}
{{- else -}}
{{- include "zk-object-fabric.secretName" . -}}
{{- end -}}
{{- end }}

{{/*
Fail fast at template time when env=production is set with a persistent
metadata store (Postgres DSN or embedded SQLite) but no manifest-body key.
This mirrors cmd/gateway's enforceProductionManifestEncryption guard so the
misconfiguration surfaces at `helm install` instead of as a pod crash-loop.
*/}}
{{- define "zk-object-fabric.validateManifestBodyKey" -}}
{{- $cp := .Values.config.controlPlane -}}
{{- $persistent := or $cp.metadataDsnFromSecret (ne (toString $cp.embeddedDbPath) "") -}}
{{- if and (eq .Values.config.env "production") $persistent (not (include "zk-object-fabric.manifestBodyKeyConfigured" .)) -}}
{{- fail "config.env=production with a persistent metadata store (controlPlane.metadataDsnFromSecret or controlPlane.embeddedDbPath) requires config.encryption.manifestBodyKey.value or .existingSecret: the gateway refuses to boot without manifest_body_key_path (manifest JSON would be stored as plaintext). Set the key, or use config.env=development for a no-dependency trial." -}}
{{- end -}}
{{- end }}

{{/*
Resolved image reference (tag falls back to chart appVersion).
*/}}
{{- define "zk-object-fabric.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}
