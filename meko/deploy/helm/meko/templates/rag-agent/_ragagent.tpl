{{/*
Shared env for the rag-agent text and pdf workers. The two differ only in
WORKER_DOCUMENT_TYPE and which ConfigMap key feeds LANGFUSE_HOST, plus the
optional OPENAI_API_KEY when ragAgent.openai.enabled.
Usage: include "meko.ragAgent.env" (dict "root" $ "docType" "TEXT" "langfuseKey" "TXT_LANGFUSE_HOST")
*/}}
{{- define "meko.ragAgent.env" -}}
{{- $c := .root.Values.ragAgent -}}
{{- $secret := $c.secrets.existing.rag -}}
- name: YUGABYTEDB_CONNECTION_STRING
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: YUGABYTEDB_CONNECTION_STRING
# OPENAI_API_KEY is environment-specific. Inject it via an overlay
# patch in environments whose indexes are initialized with
# r_ai_provider='OPENAI'. AWS_BEDROCK indexes use the AWS keys below.
- name: AWS_ACCESS_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: AWS_ACCESS_KEY_ID
      optional: true
- name: AWS_SECRET_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: AWS_SECRET_ACCESS_KEY
      optional: true
- name: WORKER_DOCUMENT_TYPE
  value: {{ .docType | quote }}
- name: ENABLE_LANGFUSE_TRACING
  value: "true"
- name: LANGFUSE_HOST
  valueFrom:
    configMapKeyRef:
      name: rag-agent-config
      key: {{ .langfuseKey }}
{{- if $c.openai.enabled }}
# prod-with-openai: appended so indexes with r_ai_provider='OPENAI' work
# alongside AWS_BEDROCK indexes.
# Optional, like every other OpenAI ref in this chart: dataPlaneSecrets renders the
# key only when one was supplied, and a worker with no key should reach the log line
# that says so rather than never starting.
- name: OPENAI_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: OPENAI_API_KEY
      optional: true
{{- end }}
{{- end -}}
