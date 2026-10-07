{{- define "meko-foundation.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "meko-foundation.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains (include "meko-foundation.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "meko-foundation.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "meko-foundation.serviceAccountName" -}}
{{- default (include "meko-foundation.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/*
Cluster-scoped objects are prefixed with the namespace, because they are not
namespaced and two foundations in two namespaces of one cluster would otherwise
install the same ClusterRole over each other — the second release adopting the
first's object, and either uninstall taking it from both.
*/}}
{{- define "meko-foundation.clusterName" -}}
{{- printf "%s-%s" .Release.Namespace (include "meko-foundation.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "meko-foundation.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "meko-foundation.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "meko-foundation.selectorLabels" -}}
app.kubernetes.io/name: {{ include "meko-foundation.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
