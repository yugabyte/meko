{{/*
The shard's resource name. Derived from shardName rather than the release name so
the Kubernetes objects, the ingress_shards row and the `shard` on every endpoint
route all carry one identifier — the controller looks the shard's load-balancer
address up by exactly this name.
*/}}
{{- define "yb-amp-ingress.fullname" -}}
{{- .Values.shardName | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "yb-amp-ingress.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: yb-amp-ingress
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
amp.yugabyte.com/ingress-shard: {{ .Values.shardName }}
amp.yugabyte.com/ingress-family: {{ .Values.family }}
{{- end -}}

{{- define "yb-amp-ingress.selectorLabels" -}}
app.kubernetes.io/name: yb-amp-ingress
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The shard's ports, as a list. One definition so the container ports, the Service
ports and the --entryPoints arguments cannot drift from each other.
*/}}
{{- define "yb-amp-ingress.ports" -}}
{{- untilStep (int .Values.ports.lo) (int (add1 .Values.ports.hi)) 1 | toJson -}}
{{- end -}}
