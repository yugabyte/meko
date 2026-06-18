#!/usr/bin/env bash
#
# Re-deploy the amp controller (cAmp control plane + its managed storage
# controller) to Kubernetes, wired to deploy compute endpoints as internal
# LoadBalancers via Helm.
#
# Idempotent: it's all `kubectl apply`, so re-running updates in place.
#
# Assumes these already exist in the namespace (NOT created here):
#   - the namespace
#   - the backing YugabyteDB (the controller + endpoints connect to it; the amp
#     and storage_controller schemas live there and persist across re-deploys)
#   - the image pull secret (to pull the controller + endpoint images from ECR)
#
# The images must already be pushed to ECR. To rebuild first:
#   make amp-bundle && docker build -f deploy/amp/Dockerfile -t <repo>/amp/controller:<tag> . && docker push <repo>/amp/controller:<tag>
# then run this with CONTROLLER_TAG=<tag>.
#
# Usage:  deploy/amp/redeploy.sh
#   override any default via env, e.g.:  NS=shubin-amp CONTROLLER_TAG=latest deploy/amp/redeploy.sh
set -euo pipefail

# --- configuration (override via environment) ---
NS="${NS:-shubin-amp}"
PULL_SECRET="${PULL_SECRET:-pull-secret}"
ECR="${ECR:-454529406029.dkr.ecr.us-west-2.amazonaws.com}"
CONTROLLER_TAG="${CONTROLLER_TAG:-20260618-221855}"   # amp/controller image tag
ENDPOINT_TAG="${ENDPOINT_TAG:-20260618-184649}"       # amp/endpoint image tag endpoints run
# Backing YugabyteDB (reused; not created here).
YB_HOST_PORTS="${YB_HOST_PORTS:-yb-tserver.${NS}.svc.cluster.local:5433}"
DB_URL="${DB_URL:-postgres://yugabyte@yb-tserver.${NS}.svc.cluster.local:5433/yb_storage?sslmode=disable}"
# Internal-LB annotations applied to the controller + every endpoint Service
# (GKE: a private VPC IP rather than a public one).
LB_ANNOTATIONS="${LB_ANNOTATIONS:-networking.gke.io/load-balancer-type=Internal,networking.gke.io/internal-load-balancer-allow-global-access=true}"

CONTROLLER_IMAGE="${ECR}/amp/controller:${CONTROLLER_TAG}"
ENDPOINT_REPO="${ECR}/amp/endpoint"

echo ">> namespace=${NS}  controller=${CONTROLLER_IMAGE}  endpoint=${ENDPOINT_REPO}:${ENDPOINT_TAG}"

# Sanity: warn (don't fail) if expected prerequisites are missing.
kubectl get ns "${NS}" >/dev/null 2>&1 || echo "WARN: namespace ${NS} not found"
kubectl -n "${NS}" get secret "${PULL_SECRET}" >/dev/null 2>&1 || echo "WARN: pull secret ${PULL_SECRET} not found in ${NS}"

# Render the controller-LB Service annotations only when set — an empty
# LB_ANNOTATIONS must produce no `annotations:` block (a bare key would be
# invalid YAML and fail the whole apply).
LB_ANNOTATIONS_YAML=""
if [ -n "${LB_ANNOTATIONS}" ]; then
  LB_ANNOTATIONS_YAML="  annotations:
$(echo "${LB_ANNOTATIONS}" | tr ',' '\n' | sed 's/^/    /; s/=/: "/; s/$/"/')"
fi

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
# Helm's --create-namespace checks/creates the namespace.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: amp-controller-namespaces
rules:
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, list, create]
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
        - name: amp
          image: ${CONTROLLER_IMAGE}
          imagePullPolicy: IfNotPresent
          ports:
            - { name: api, containerPort: 8080 }
            - { name: sc, containerPort: 50051 }
          env:
            - { name: AMP_DATABASE_URL, value: "${DB_URL}" }
            - { name: AMP_DB_SCHEMA, value: "amp" }
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
            # Endpoints get an internal LoadBalancer (private VPC IP).
            - { name: AMP_DEPLOY_SERVICE_TYPE, value: "LoadBalancer" }
            - { name: AMP_DEPLOY_SERVICE_ANNOTATIONS, value: "${LB_ANNOTATIONS}" }
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
---
# Internal LoadBalancer for the UI/API on a private VPC IP (SC port stays internal).
apiVersion: v1
kind: Service
metadata:
  name: amp-controller-lb
  namespace: ${NS}
${LB_ANNOTATIONS_YAML}
spec:
  type: LoadBalancer
  selector: { app: amp-controller }
  ports:
    - { name: api, port: 8080, targetPort: 8080 }
YAML

echo ">> waiting for rollout..."
kubectl -n "${NS}" rollout status deploy/amp-controller --timeout=180s

echo ">> amp controller is up. Access:"
LBIP="$(kubectl -n "${NS}" get svc amp-controller-lb -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)"
if [ -n "${LBIP}" ]; then
  echo "   internal LB (UI/API):  http://${LBIP}:8080   (UI at /, API under /api/v1)"
else
  echo "   internal LB IP: still provisioning — check: kubectl -n ${NS} get svc amp-controller-lb -w"
fi
echo "   or port-forward:       kubectl -n ${NS} port-forward svc/amp-controller 8080:8080   then http://localhost:8080"
