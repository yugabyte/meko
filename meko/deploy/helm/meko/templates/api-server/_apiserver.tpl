{{/*
Shared container spec for the api-server Deployment and the migration Job.
The kustomize base kept the Job's args/env in lockstep with the Deployment by
hand ("KEEP THEM IN SYNC"); here they are one source of truth.

The Deployment appends  --deploy-env=<env>.
The Job appends         --deploy-env=$(DEPLOY_ENV) --migrate-only [scope flag].
*/}}

{{/*
Common CLI args (everything except --deploy-env / --migrate-only).
In `ycp` (control-plane) mode the data-plane args (S3, OpenAI, langfuse, SES)
are dropped — the control plane needs only its DBs, OIDC, the internal secret,
and --control-plane-mode. In `mcp` (data-plane) mode the full arg set renders
byte-identically to the pre-mode chart.
*/}}
{{- define "meko.apiServer.args" -}}
{{- $c := .Values.apiServer -}}
{{- $mode := include "meko.mode" . -}}
- "--db-host=$(DB_HOST)"
- "--db-port=$(DB_PORT)"
- "--db-user-name=$(DB_USER_NAME)"
- "--db-password=$(DB_PASSWORD)"
- "--db-name=$(DB_NAME)"
- "--db-sslmode=$(DB_SSL_MODE)"
- "--system-db-host=$(SYSTEM_DB_HOST)"
- "--system-db-port=$(SYSTEM_DB_PORT)"
- "--system-db-user-name=$(SYSTEM_DB_USER_NAME)"
- "--system-db-password=$(SYSTEM_DB_PASSWORD)"
- "--system-db-name=$(SYSTEM_DB_NAME)"
- "--system-db-sslmode=$(SYSTEM_DB_SSL_MODE)"
{{- if and $c.cognito.enabled $c.oidc.enabled }}
{{- fail "apiServer.cognito.enabled and apiServer.oidc.enabled are both on: pick one identity provider. Two would mean the flag order decides who can sign in." }}
{{- end }}
{{- if $c.cognito.enabled }}
- "--cognito-region=$(COGNITO_REGION)"
- "--cognito-pool-id=$(COGNITO_POOL_ID)"
- "--cognito-client-id=$(COGNITO_CLIENT_ID)"
- "--cognito-client-secret=$(COGNITO_CLIENT_SECRET)"
- "--cognito-domain=$(COGNITO_DOMAIN)"
{{- end }}
{{- if $c.oidc.enabled }}
{{- if not $c.oidc.issuerUrl }}
{{- fail "apiServer.oidc.enabled needs apiServer.oidc.issuerUrl: every other endpoint is discovered from it." }}
{{- end }}
{{- if not $c.oidc.clientId }}
{{- fail "apiServer.oidc.enabled needs apiServer.oidc.clientId: it is the audience every ID token is verified against." }}
{{- end }}
{{- if not $c.oidc.redirectUrl }}
{{- fail "apiServer.oidc.enabled needs apiServer.oidc.redirectUrl: it must match the URI registered with the issuer byte-for-byte and cannot be derived behind a proxy." }}
{{- end }}
- "--auth-provider=oidc"
- "--oidc-issuer-url=$(OIDC_ISSUER_URL)"
- "--oidc-client-id=$(OIDC_CLIENT_ID)"
- "--oidc-redirect-url=$(OIDC_REDIRECT_URL)"
- "--oidc-scopes=$(OIDC_SCOPES)"
{{- /* The client secret is deliberately NOT a flag. Its env var is an optional
       secretKeyRef, and for a public client the Secret does not exist — an
       undefined name leaves Kubernetes substituting nothing, so the process
       would receive the literal string "$(OIDC_CLIENT_SECRET)" and present it
       to the token endpoint as client authentication, which Entra rejects
       (AADSTS700025) and other issuers answer with invalid_client. The flag
       already defaults from OIDC_CLIENT_SECRET, which is empty when unset —
       the public-client-with-PKCE flow this chart documents. */}}
{{- end }}
- "--crd-namespace={{ $c.staticArgs.crdNamespace }}"
{{- if $c.basePath }}
{{- /* Passed as a flag as well as an env var (the api-server would read
       MEKO_YCP_BASE_PATH on its own) so `kubectl describe pod` shows the mount
       point without anyone having to know the env fallback exists. */}}
- "--base-path=$(MEKO_YCP_BASE_PATH)"
{{- end }}
{{- if $c.enableByoc }}
{{- /* Rendered only when true, like --require-account-header below: the flag
       defaults false, and an api-server image predating it exits on an unknown
       flag rather than ignoring it. Pairs with mekoUi.env.enableByoc — both
       sides read MEKO_ENABLE_BYOC, so they must be set together. */}}
- "--enable-byoc"
{{- end }}
{{- if eq $mode "ycp" }}
- "--mcp-internal-secret=$(MCP_INTERNAL_SECRET)"
- "--control-plane-mode"
{{- /* Without this every gated route -- fleet, console, and the agent surface,
       which sits behind the same admin secret plus the api-key-id header --
       returns 503: RequireAdminSecret disables itself when the secret is empty. */}}
- "--admin-api-key=$(ADMIN_API_KEY)"
{{- if $c.requireAccountHeader }}
{{- /* Rendered only when true: the flag's own default is false, so passing it
       explicitly would be noise, and an older image that does not know the flag
       exits on an unknown one rather than ignoring it. */}}
- "--require-account-header"
{{- end }}
{{- /* Deprecated no-ops. These configured the stack registration used to seed;
       registration seeds nothing now and a data plane's workloads come from this
       chart. The api-server still accepts them and warns at boot, so they are
       rendered only when explicitly set — an operator upgrading past the removal
       then has nothing to unset. Drop them, and this block, once no control plane
       in the fleet predates the deprecation warning. */}}
{{- with $c.dataplane }}
{{- if .mcpImage }}
- "--dataplane-mcp-image={{ .mcpImage }}"
{{- end }}
{{- if .imagePullSecret }}
- "--dataplane-image-pull-secret={{ .imagePullSecret }}"
{{- end }}
{{- if .secretRef }}
- "--dataplane-secret-ref={{ .secretRef }}"
{{- end }}
{{- if .noBootstrapDefaults }}
- "--no-bootstrap-defaults"
{{- end }}
{{- end }}
{{- else }}
- "--langfuse-host=$(LANGFUSE_HOST)"
- "--langfuse-admin-api-key={{ $c.staticArgs.langfuseAdminApiKey }}"
- "--s3-auth-mode={{ $c.staticArgs.s3AuthMode }}"
- "--s3-access-key=$(S3_ACCESS_KEY)"
- "--s3-secret-key=$(S3_SECRET_KEY)"
- "--s3-bucket=$(S3_BUCKET)"
- "--s3-region=$(S3_REGION)"
- "--openai-api-key=$(OPENAI_API_KEY)"
- "--openai-llm-model={{ $c.staticArgs.openaiLlmModel }}"
- "--openai-embedding-model={{ $c.staticArgs.openaiEmbeddingModel }}"
- "--langfuse-db-host=$(LANGFUSE_DB_HOST)"
- "--langfuse-db-port={{ $c.staticArgs.langfuseDbPort }}"
- "--langfuse-db-user={{ $c.staticArgs.langfuseDbUser }}"
- "--langfuse-db-password=$(DB_PASSWORD)"
- "--langfuse-db-name={{ $c.staticArgs.langfuseDbName }}"
- "--langfuse-db-sslmode=$(LANGFUSE_DB_SSL_MODE)"
- "--mcp-internal-secret=$(MCP_INTERNAL_SECRET)"
- "--ses-from-address=$(SES_FROM_ADDRESS)"
- "--ses-region=$(SES_REGION)"
{{- end }}
{{- end -}}

{{/*
Common env block, sourced from the ConfigMap and the existing Secrets.
In `ycp` mode only the control-plane env is emitted (DBs, cognito, internal
secret) so the deploy needs no S3/OpenAI/langfuse/SES Secrets.
*/}}
{{- define "meko.apiServer.env" -}}
{{- $c := .Values.apiServer -}}
{{- $s := $c.secrets.existing -}}
{{- $cm := "meko-api-server-config" -}}
{{- $mode := include "meko.mode" . -}}
- name: DB_HOST
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: DB_HOST } }
- name: DB_PORT
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: DB_PORT } }
- name: DB_USER_NAME
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: DB_USER_NAME } }
- name: DB_PASSWORD
  valueFrom: { secretKeyRef: { name: {{ $s.db }}, key: db-password } }
- name: DB_NAME
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: DB_NAME } }
- name: DB_SSL_MODE
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: DB_SSL_MODE } }
- name: SYSTEM_DB_HOST
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: SYSTEM_DB_HOST } }
- name: SYSTEM_DB_PORT
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: SYSTEM_DB_PORT } }
- name: SYSTEM_DB_USER_NAME
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: SYSTEM_DB_USER_NAME } }
- name: SYSTEM_DB_PASSWORD
  valueFrom: { secretKeyRef: { name: {{ $s.systemDb }}, key: db-password } }
- name: SYSTEM_DB_NAME
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: SYSTEM_DB_NAME } }
- name: SYSTEM_DB_SSL_MODE
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: SYSTEM_DB_SSL_MODE } }
{{- if $c.basePath }}
- name: MEKO_YCP_BASE_PATH
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: MEKO_YCP_BASE_PATH } }
{{- end }}
{{- /* Metrics query API for the environment metrics route (--metrics-url). Emitted
       only when apiServer.config carries the key, for the same reason as the base
       path above: a configMapKeyRef to a key the ConfigMap does not hold blocks the
       pod from starting. Unset simply leaves the route answering 503. */}}
{{- if $c.config.MEKO_METRICS_URL }}
- name: MEKO_METRICS_URL
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: MEKO_METRICS_URL } }
{{- end }}
{{- /* The write side of that address: the BYOC OTLP door this control plane issues,
       printed in every amp install hint and enforced at enroll. Emitted on the same
       terms as MEKO_METRICS_URL above. Unlike it, the binary DEFAULTS this (to the
       devcloud door), so leaving it out is a real choice rather than a disabled
       route — a prod or srecloud control plane that omits it hands out the devcloud
       door and marks its own agents DEGRADED. */}}
{{- if $c.config.MEKO_BYOC_METRICS_URL }}
- name: MEKO_BYOC_METRICS_URL
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: MEKO_BYOC_METRICS_URL } }
{{- end }}
{{- if $c.cognito.enabled }}
- name: COGNITO_REGION
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: COGNITO_REGION } }
- name: COGNITO_POOL_ID
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: COGNITO_POOL_ID } }
- name: COGNITO_CLIENT_ID
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: COGNITO_CLIENT_ID } }
- name: COGNITO_CLIENT_SECRET
  valueFrom: { secretKeyRef: { name: {{ $s.cognito }}, key: cognito-client-secret, optional: true } }
- name: COGNITO_DOMAIN
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: COGNITO_DOMAIN } }
{{- end }}
{{- if $c.oidc.enabled }}
- name: OIDC_ISSUER_URL
  value: {{ $c.oidc.issuerUrl | quote }}
- name: OIDC_CLIENT_ID
  value: {{ $c.oidc.clientId | quote }}
- name: OIDC_REDIRECT_URL
  value: {{ $c.oidc.redirectUrl | quote }}
- name: OIDC_SCOPES
  value: {{ $c.oidc.scopes | quote }}
{{- /* Optional, and read as an env var rather than passed as a flag: a public
       client has no Secret to reference, and relies on the PKCE the server
       sends unconditionally. See the args block above. */}}
- name: OIDC_CLIENT_SECRET
  valueFrom: { secretKeyRef: { name: {{ $s.oidc }}, key: oidc-client-secret, optional: true } }
{{- end }}
{{- if eq $mode "ycp" }}
- name: MCP_INTERNAL_SECRET
  valueFrom: { secretKeyRef: { name: {{ $s.mcpInternal }}, key: secret-key } }
- name: ADMIN_API_KEY
  valueFrom: { secretKeyRef: { name: {{ $s.adminApiKey }}, key: admin-api-key } }
{{- else }}
{{- /*
  optional on the feature credentials (S3, LLM), not on the database ones.
  A missing Secret with optional=false is a pod stuck in
  CreateContainerConfigError — no logs, no probes, nothing to read — so a data
  plane that does not use S3 could not start for want of a credential it never
  calls. optional=true leaves the variable unset instead, which api_server
  already handles: the feature fails at the point of use, where the error names
  what is missing. The DB refs stay required, because an api-server that starts
  without a database password is a slower version of the same failure.
*/}}
- name: S3_ACCESS_KEY
  valueFrom: { secretKeyRef: { name: {{ $s.s3 }}, key: s3-access-key, optional: true } }
- name: S3_SECRET_KEY
  valueFrom: { secretKeyRef: { name: {{ $s.s3 }}, key: s3-secret-key, optional: true } }
- name: S3_BUCKET
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: S3_BUCKET } }
- name: S3_REGION
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: S3_REGION } }
- name: OPENAI_API_KEY
  valueFrom: { secretKeyRef: { name: {{ $s.llm }}, key: openai-api-key, optional: true } }
- name: MCP_INTERNAL_SECRET
  valueFrom: { secretKeyRef: { name: {{ $s.mcpInternal }}, key: secret-key } }
# api-server reuses the s3 secret's keys for the AWS SDK creds (§5 — dual use).
- name: AWS_ACCESS_KEY_ID
  valueFrom: { secretKeyRef: { name: {{ $s.s3 }}, key: s3-access-key, optional: true } }
- name: AWS_SECRET_ACCESS_KEY
  valueFrom: { secretKeyRef: { name: {{ $s.s3 }}, key: s3-secret-key, optional: true } }
- name: AWS_REGION
  value: {{ $c.config.S3_REGION | default $.Values.global.region | quote }}
- name: LANGFUSE_HOST
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: LANGFUSE_HOST } }
- name: LANGFUSE_DB_HOST
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: LANGFUSE_DB_HOST } }
- name: LANGFUSE_DB_SSL_MODE
  valueFrom: { configMapKeyRef: { name: {{ $cm }}, key: LANGFUSE_DB_SSL_MODE } }
- name: SES_FROM_ADDRESS
  value: {{ $c.ses.fromAddress | quote }}
- name: SES_REGION
  value: {{ $c.ses.region | quote }}
{{- end }}
{{- end -}}
