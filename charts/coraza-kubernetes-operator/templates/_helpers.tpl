{{/*
Chart name, truncated to 63 chars.
*/}}
{{- define "coraza-operator.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to 63 chars.
*/}}
{{- define "coraza-operator.fullname" -}}
{{- if eq .Release.Name .Chart.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end -}}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "coraza-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "coraza-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "coraza-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "coraza-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: coraza-controller-manager
{{- end }}

{{/*
Service account name.
*/}}
{{- define "coraza-operator.serviceAccountName" -}}
{{- include "coraza-operator.fullname" . }}
{{- end }}

{{/*
Name of the Secret holding the xDS serving certificate. One definition so the
Deployment volume, the generated Secret, the bootstrap Role and
--xds-cert-secret cannot drift.
*/}}
{{- define "coraza-operator.servingCertSecretName" -}}
{{- .Values.tls.existingSecret | default (printf "%s-serving-cert" (include "coraza-operator.fullname" .)) }}
{{- end }}

{{/*
Manager service FQDN (used in Istio resources and envoy cluster name).
*/}}
{{- define "coraza-operator.serviceFQDN" -}}
{{- printf "%s.%s.svc.cluster.local" (include "coraza-operator.fullname" .) .Release.Namespace }}
{{- end }}

{{/*
Renders "true" when the value is an integer: an int from --set, or a whole
number from a values file.
*/}}
{{- define "coraza-operator.isInt" -}}
{{- if or (kindIs "int64" .) (kindIs "int" .) (and (kindIs "float64" .) (eq . (float64 (int64 .)))) }}true{{ end }}
{{- end }}
