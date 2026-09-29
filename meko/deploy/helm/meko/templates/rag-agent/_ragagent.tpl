{{/*
Env every rag-agent worker sets before its own identity vars: the YugabyteDB
connection and optional AWS Bedrock creds. Split from the suffix below so
deployment-conversations.yaml can insert its own WORKER_TYPE/COLUMN_EMBED_*
vars (rather than WORKER_DOCUMENT_TYPE) between the two.
Usage: include "meko.ragAgent.envPrefix" (dict "root" $)
*/}}
{{- define "meko.ragAgent.envPrefix" -}}
{{- $secret := .root.Values.ragAgent.secrets.existing.rag -}}
- name: YUGABYTEDB_CONNECTION_STRING
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: YUGABYTEDB_CONNECTION_STRING
# OPENAI_API_KEY is environment-specific. Appended by ragAgent.envSuffix in
# environments whose indexes are initialized with r_ai_provider='OPENAI'.
# AWS_BEDROCK indexes use the AWS keys below.
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
{{- end -}}

{{/*
Env every rag-agent worker sets after its own identity vars: Langfuse tracing
plus (prod-with-openai) OPENAI_API_KEY.
Usage: include "meko.ragAgent.envSuffix" (dict "root" $ "langfuseKey" "TXT_LANGFUSE_HOST")
*/}}
{{- define "meko.ragAgent.envSuffix" -}}
{{- $c := .root.Values.ragAgent -}}
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
      name: {{ $c.secrets.existing.rag }}
      key: OPENAI_API_KEY
      optional: true
{{- end }}
{{- end -}}

{{/*
Text/PDF worker env: envPrefix + WORKER_DOCUMENT_TYPE + envSuffix.
Usage: include "meko.ragAgent.env" (dict "root" $ "docType" "TEXT" "langfuseKey" "TXT_LANGFUSE_HOST")
*/}}
{{- define "meko.ragAgent.env" -}}
{{- include "meko.ragAgent.envPrefix" . }}
- name: WORKER_DOCUMENT_TYPE
  value: {{ .docType | quote }}
{{ include "meko.ragAgent.envSuffix" . }}
{{- end -}}
