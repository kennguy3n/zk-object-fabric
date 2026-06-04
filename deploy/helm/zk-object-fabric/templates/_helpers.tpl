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
Name of the dedicated Secret the chart creates to hold an inline
manifestBodyKey.value. It is kept separate from the gateway credentials Secret
so the credentials Secret can be consumed with envFrom (the init container
needs the creds as env vars) without Kubernetes trying — and failing — to
expose the dotted key name (manifest-body.key) as an env var and logging a
warning on every pod start. This Secret is only ever volume-mounted.
*/}}
{{- define "zk-object-fabric.manifestBodyKeySecretName" -}}
{{- $mbk := .Values.config.encryption.manifestBodyKey -}}
{{- if $mbk.existingSecret -}}
{{- $mbk.existingSecret -}}
{{- else -}}
{{- printf "%s-manifest-body-key" (include "zk-object-fabric.fullname" .) -}}
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
Fail fast at template time when the chart is told not to create the credentials
Secret (secret.create=false) but no existing Secret is named
(secret.existingSecret is empty). In that case the secretName helper falls back
to the chart's <fullname>-secrets name, which is never rendered, so the init
container's envFrom would reference a non-existent Secret and the pod would fail
with CreateContainerConfigError. Surface it at `helm install` instead.
*/}}
{{- define "zk-object-fabric.validateCredentialsSecret" -}}
{{- if and (not .Values.secret.create) (not .Values.secret.existingSecret) -}}
{{- fail "secret.create=false requires secret.existingSecret to name the Secret that provides the gateway credentials (WASABI_ACCESS_KEY, WASABI_SECRET_KEY, METADATA_DSN, VAULT_TOKEN, CONSOLE_ADMIN_TOKEN). Set secret.existingSecret, or set secret.create=true to let the chart render the Secret from secret.* values." -}}
{{- end -}}
{{- end }}

{{/*
Resolved image reference (tag falls back to chart appVersion).
*/}}
{{- define "zk-object-fabric.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}
