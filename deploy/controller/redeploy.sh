#!/usr/bin/env bash
#
# Re-deploy the amp controller (cAmp control plane + its managed storage
# controller) to Kubernetes, wired to deploy compute endpoints via Helm. The
# controller's own exposure (CONTROLLER_EXPOSE) and the endpoints' Service type
# (ENDPOINT_SERVICE_TYPE) are both configurable; defaults avoid cloud LBs.
#
# Idempotent: it's all `kubectl apply`, so re-running updates in place.
#
# Assumes these already exist in the namespace (NOT created here):
#   - the namespace
#   - the backing YugabyteDB (the controller + endpoints connect to it; the controller
#     and storage_controller schemas live there and persist across re-deploys)
#   - the image pull secret (to pull the controller + endpoint images from ECR)
#
# The images must already be pushed to ECR. To rebuild first:
#   make controller-bundle && docker build -f deploy/controller/Dockerfile -t <repo>/controller/controller:<tag> . && docker push <repo>/controller/controller:<tag>
# then run this with CONTROLLER_TAG=<tag>.
#
# Usage:  deploy/controller/redeploy.sh
#   override any default via env, e.g.:  NS=amp CONTROLLER_TAG=latest deploy/controller/redeploy.sh
#   namespace also reads AMP_NAMESPACE (NS takes precedence); default is amp.
#   image repo paths default to controller/controller + controller/endpoint;
#   for the amp/* repos:  CONTROLLER_IMAGE_REPO=amp/controller ENDPOINT_IMAGE_REPO=amp/endpoint ...
#
# Exposure (avoid slow cloud LBs):
#   CONTROLLER_EXPOSE   nodeport | internal-lb | external-lb | none   (default: internal-lb)
#   ENDPOINT_SERVICE_TYPE  ClusterIP | NodePort | LoadBalancer        (default: ClusterIP)
# The in-cluster ClusterIP Service (amp-controller, API + SC port) is always
# created. ClusterIP endpoints avoid a per-endpoint cloud LB entirely (reach via
# Service DNS in-cluster, or `kubectl port-forward` from outside).
#
# To stand up the prerequisites (namespace, pull secret, single-node YugabyteDB)
# from scratch and then deploy the controller, use deploy/up.sh instead.
set -euo pipefail

# --- configuration (override via environment) ---
# Namespace: NS wins, then AMP_NAMESPACE, then the default.
NS="${NS:-${AMP_NAMESPACE:-amp}}"
PULL_SECRET="${PULL_SECRET:-pull-secret}"
ECR="${ECR:-454529406029.dkr.ecr.us-west-2.amazonaws.com}"
CONTROLLER_TAG="${CONTROLLER_TAG:-20260618-221855}"   # controller/controller image tag
ENDPOINT_TAG="${ENDPOINT_TAG:-20260618-184649}"       # controller/endpoint image tag endpoints run
# Backing YugabyteDB (reused; not created here).
YB_HOST_PORTS="${YB_HOST_PORTS:-yb-tserver.${NS}.svc.cluster.local:5433}"
DB_URL="${DB_URL:-postgres://yugabyte@yb-tserver.${NS}.svc.cluster.local:5433/yb_storage?sslmode=disable}"
# Internal-LB annotations (GKE: a private VPC IP). Applied to the controller LB
# when CONTROLLER_EXPOSE=internal-lb, and to endpoints via ENDPOINT_SERVICE_ANNOTATIONS.
LB_ANNOTATIONS="${LB_ANNOTATIONS:-networking.gke.io/load-balancer-type=Internal,networking.gke.io/internal-load-balancer-allow-global-access=true}"

# How to expose the controller UI/API externally:
#   nodeport     — a NodePort Service (node-ip:3xxxx), no cloud LB, VPC-reachable
#   internal-lb  — internal LoadBalancer (private VPC IP), uses LB_ANNOTATIONS
#   external-lb  — public LoadBalancer (public IP)
#   none         — in-cluster ClusterIP only (reach via `kubectl port-forward`)
CONTROLLER_EXPOSE="${CONTROLLER_EXPOSE:-internal-lb}"
# Fixed nodePort for CONTROLLER_EXPOSE=nodeport (30000–32767; empty = auto-assign).
CONTROLLER_NODEPORT="${CONTROLLER_NODEPORT:-}"

# Endpoint Service exposure: ClusterIP (default) | NodePort | LoadBalancer.
# ClusterIP/NodePort avoid a per-endpoint cloud LB (CreateEndpoint returns at once);
# NodePort reports <nodeIP>:<nodePort> as the endpoint's client address.
ENDPOINT_SERVICE_TYPE="${ENDPOINT_SERVICE_TYPE:-ClusterIP}"
# Annotations for a LoadBalancer endpoint Service (e.g. internal-LB); empty for ClusterIP.
ENDPOINT_SERVICE_ANNOTATIONS="${ENDPOINT_SERVICE_ANNOTATIONS:-}"

# Image repo paths within the registry. Override for the amp/* ECR repos, e.g.
# CONTROLLER_IMAGE_REPO=amp/controller ENDPOINT_IMAGE_REPO=amp/endpoint.
CONTROLLER_IMAGE_REPO="${CONTROLLER_IMAGE_REPO:-controller/controller}"
ENDPOINT_IMAGE_REPO="${ENDPOINT_IMAGE_REPO:-controller/endpoint}"

CONTROLLER_IMAGE="${ECR}/${CONTROLLER_IMAGE_REPO}:${CONTROLLER_TAG}"
ENDPOINT_REPO="${ECR}/${ENDPOINT_IMAGE_REPO}"

echo ">> namespace=${NS}  controller=${CONTROLLER_IMAGE}  endpoint=${ENDPOINT_REPO}:${ENDPOINT_TAG}"

# Sanity: warn (don't fail) if expected prerequisites are missing.
kubectl get ns "${NS}" >/dev/null 2>&1 || echo "WARN: namespace ${NS} not found"
kubectl -n "${NS}" get secret "${PULL_SECRET}" >/dev/null 2>&1 || echo "WARN: pull secret ${PULL_SECRET} not found in ${NS}"

# Build the external-facing controller Service per CONTROLLER_EXPOSE. The
# in-cluster ClusterIP Service (amp-controller) is always created in the manifest
# below; this is the *extra* Service (NodePort or LoadBalancer), or empty.
CONTROLLER_SVC_YAML=""
case "${CONTROLLER_EXPOSE}" in
  none) ;;
  nodeport)
    NP_LINE=""
    [ -n "${CONTROLLER_NODEPORT}" ] && NP_LINE="
      nodePort: ${CONTROLLER_NODEPORT}"
    CONTROLLER_SVC_YAML="---
# NodePort for the UI/API — reachable at any node IP (VPC-internal), no cloud LB.
apiVersion: v1
kind: Service
metadata:
  name: amp-controller-np
  namespace: ${NS}
  labels: { app: amp-controller }
spec:
  type: NodePort
  selector: { app: amp-controller }
  ports:
    - name: api
      port: 8080
      targetPort: 8080${NP_LINE}"
    ;;
  internal-lb|external-lb)
    # internal-lb carries LB_ANNOTATIONS; external-lb gets a public IP (no annotations).
    ANN_YAML=""
    if [ "${CONTROLLER_EXPOSE}" = "internal-lb" ] && [ -n "${LB_ANNOTATIONS}" ]; then
      ANN_YAML="  annotations:
$(echo "${LB_ANNOTATIONS}" | tr ',' '\n' | sed 's/^/    /; s/=/: "/; s/$/"/')"
    fi
    CONTROLLER_SVC_YAML="---
# LoadBalancer for the UI/API (${CONTROLLER_EXPOSE}); the SC port stays ClusterIP-only.
apiVersion: v1
kind: Service
metadata:
  name: amp-controller-lb
  namespace: ${NS}
${ANN_YAML}
spec:
  type: LoadBalancer
  selector: { app: amp-controller }
  ports:
    - { name: api, port: 8080, targetPort: 8080 }"
    ;;
  *)
    echo "ERROR: invalid CONTROLLER_EXPOSE=${CONTROLLER_EXPOSE} (want nodeport|internal-lb|external-lb|none)" >&2
    exit 1
    ;;
esac

kubectl apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: amp-controller
  namespace: ${NS}
---
# The controller drives Helm to deploy endpoint workloads, and reads pods/services
# (pod-IP / LoadBalancer-IP lookups), so it needs broad access in its namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: amp-controller
  namespace: ${NS}
rules:
  - apiGroups: [""]
    resources: [pods, services, configmaps, secrets, persistentvolumeclaims, endpoints, events, serviceaccounts]
    verbs: ["*"]
  - apiGroups: ["apps"]
    resources: [deployments, replicasets, statefulsets]
    verbs: ["*"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: amp-controller
  namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: amp-controller
subjects:
  - kind: ServiceAccount
    name: amp-controller
    namespace: ${NS}
---
# Cluster-scoped reads: Helm's --create-namespace probes namespaces, and the
# NodePort endpoint address resolver reads node IPs.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: amp-controller-namespaces
rules:
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, list, create]
  - apiGroups: [""]
    resources: [nodes]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: amp-controller-namespaces
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: amp-controller-namespaces
subjects:
  - kind: ServiceAccount
    name: amp-controller
    namespace: ${NS}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: amp-controller
  namespace: ${NS}
  labels: { app: amp-controller }
spec:
  replicas: 1
  selector:
    matchLabels: { app: amp-controller }
  template:
    metadata:
      labels: { app: amp-controller }
    spec:
      serviceAccountName: amp-controller
      imagePullSecrets:
        - name: ${PULL_SECRET}
      containers:
        - name: controller
          image: ${CONTROLLER_IMAGE}
          imagePullPolicy: IfNotPresent
          ports:
            - { name: api, containerPort: 8080 }
            - { name: sc, containerPort: 50051 }
          env:
            - { name: AMP_DATABASE_URL, value: "${DB_URL}" }
            - { name: AMP_DB_SCHEMA, value: "controller" }
            - { name: AMP_LOG_FORMAT, value: "text" }
            # Managed storage controller (in-pod subprocess), dev mode (no auth).
            - { name: AMP_STORAGE_CONTROLLER_DEV, value: "true" }
            - { name: AMP_STORAGE_CONTROLLER_LISTEN, value: "0.0.0.0:50051" }
            # Endpoint deployer: Kubernetes/Helm driver.
            - { name: AMP_DEPLOY, value: "true" }
            - { name: AMP_DEPLOY_DRIVER, value: "kubernetes" }
            - { name: AMP_DEPLOY_NAMESPACE, value: "${NS}" }
            - { name: AMP_ENDPOINT_IMAGE_REPO, value: "${ENDPOINT_REPO}" }
            - { name: AMP_ENDPOINT_IMAGE_TAG, value: "${ENDPOINT_TAG}" }
            - { name: AMP_DEPLOY_PULL_SECRET, value: "${PULL_SECRET}" }
            - { name: AMP_DEPLOY_YB_HOST_PORTS, value: "${YB_HOST_PORTS}" }
            - { name: AMP_DEPLOY_YB_USER, value: "yugabyte" }
            - { name: AMP_DEPLOY_YB_DBNAME, value: "yb_storage" }
            # The in-cluster URL endpoints use to reach this controller's SC.
            - { name: AMP_DEPLOY_STORAGE_CONTROLLER_URL, value: "http://amp-controller.${NS}.svc.cluster.local:50051" }
            - { name: AMP_DEPLOY_BOOTSTRAP, value: "true" }
            # Endpoint Service exposure. ClusterIP (default) avoids a per-endpoint
            # cloud LB; LoadBalancer uses ENDPOINT_SERVICE_ANNOTATIONS (e.g. internal-LB).
            - { name: AMP_DEPLOY_SERVICE_TYPE, value: "${ENDPOINT_SERVICE_TYPE}" }
            - { name: AMP_DEPLOY_SERVICE_ANNOTATIONS, value: "${ENDPOINT_SERVICE_ANNOTATIONS}" }
          readinessProbe:
            httpGet: { path: /readyz, port: 8080 }
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
            initialDelaySeconds: 15
            periodSeconds: 10
---
# In-cluster Service: the API + the SC port endpoints register against.
apiVersion: v1
kind: Service
metadata:
  name: amp-controller
  namespace: ${NS}
  labels: { app: amp-controller }
spec:
  type: ClusterIP
  selector: { app: amp-controller }
  ports:
    - { name: api, port: 8080, targetPort: 8080 }
    - { name: sc, port: 50051, targetPort: 50051 }
${CONTROLLER_SVC_YAML}
YAML

# Drop a controller external Service left over from a different exposure mode.
case "${CONTROLLER_EXPOSE}" in
  nodeport|none)       kubectl -n "${NS}" delete svc amp-controller-lb --ignore-not-found >/dev/null 2>&1 || true ;;
esac
case "${CONTROLLER_EXPOSE}" in
  internal-lb|external-lb|none) kubectl -n "${NS}" delete svc amp-controller-np --ignore-not-found >/dev/null 2>&1 || true ;;
esac

echo ">> waiting for rollout..."
kubectl -n "${NS}" rollout status deploy/amp-controller --timeout=180s

echo ">> amp controller is up. Access (UI at /, API under /api/v1):"
case "${CONTROLLER_EXPOSE}" in
  internal-lb|external-lb)
    LBIP="$(kubectl -n "${NS}" get svc amp-controller-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
    if [ -n "${LBIP}" ]; then
      echo "   ${CONTROLLER_EXPOSE} (UI/API):  http://${LBIP}:8080"
    else
      echo "   ${CONTROLLER_EXPOSE} IP: still provisioning — check: kubectl -n ${NS} get svc amp-controller-lb -w"
    fi
    ;;
  nodeport)
    NP="$(kubectl -n "${NS}" get svc amp-controller-np -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
    # Prefer an ExternalIP (matches the controller's own node-IP resolver), else InternalIP.
    NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    [ -n "${NODEIP}" ] || NODEIP="$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | awk '{print $1}' || true)"
    echo "   NodePort (UI/API):  http://<any-node-ip>:${NP:-<pending>}   (reachable at any node IP)"
    [ -n "${NODEIP}" ] && [ -n "${NP}" ] && echo "      e.g.  http://${NODEIP}:${NP}"
    ;;
esac
echo "   or port-forward:       kubectl -n ${NS} port-forward svc/amp-controller 8080:8080   then http://localhost:8080"
