#!/usr/bin/env bash
set -euo pipefail

# Portal Demo — Automated KIND Setup
#
# Creates two KIND clusters, installs MetalLB, generates tunnel manifests,
# deploys both sides, verifies mTLS connectivity, and sends data through
# the tunnel using a simple echo server.
#
# Usage: ./docs/demo/demo.sh
# Cleanup: ./docs/demo/cleanup.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

SOURCE_CLUSTER="portal-source"
DEST_CLUSTER="portal-destination"
METALLB_VERSION="v0.14.9"
OUTPUT_DIR="${ROOT_DIR}/demo-tunnel"
PORTAL_BIN="${ROOT_DIR}/portal"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

PF_PID=""

cleanup_on_exit() {
    if [[ -n "${PF_PID}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
    fi
}
trap cleanup_on_exit EXIT

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# ------------------------------------------------------------------
# Prerequisites
# ------------------------------------------------------------------
info "Checking prerequisites..."

for cmd in docker kind kubectl go; do
    if ! command -v "${cmd}" &>/dev/null; then
        error "${cmd} is required but not found in PATH"
    fi
done
ok "All prerequisites found"

# ------------------------------------------------------------------
# Step 1: Create KIND clusters
# ------------------------------------------------------------------
info "Creating KIND cluster: ${SOURCE_CLUSTER}"
if kind get clusters 2>/dev/null | grep -q "^${SOURCE_CLUSTER}$"; then
    warn "Cluster ${SOURCE_CLUSTER} already exists, skipping creation"
else
    kind create cluster --name "${SOURCE_CLUSTER}"
fi
ok "Cluster ${SOURCE_CLUSTER} ready"

info "Creating KIND cluster: ${DEST_CLUSTER}"
if kind get clusters 2>/dev/null | grep -q "^${DEST_CLUSTER}$"; then
    warn "Cluster ${DEST_CLUSTER} already exists, skipping creation"
else
    kind create cluster --name "${DEST_CLUSTER}"
fi
ok "Cluster ${DEST_CLUSTER} ready"

# ------------------------------------------------------------------
# Step 2: Install MetalLB on destination cluster
# ------------------------------------------------------------------
info "Installing MetalLB ${METALLB_VERSION} on ${DEST_CLUSTER}"
kubectl apply \
    -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml" \
    --context "kind-${DEST_CLUSTER}"

info "Waiting for MetalLB controller to be ready..."
kubectl wait deployment/controller -n metallb-system \
    --for=condition=Available --timeout=120s \
    --context "kind-${DEST_CLUSTER}"
ok "MetalLB is ready"

# ------------------------------------------------------------------
# Step 3: Derive a LoadBalancer IP from the KIND Docker subnet
# ------------------------------------------------------------------
info "Deriving LoadBalancer IP from KIND network..."
DEST_NODE_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
    "${DEST_CLUSTER}-control-plane")

IFS='.' read -r a b _ _ <<< "${DEST_NODE_IP}"
RESPONDER_IP="${a}.${b}.255.200"

info "Destination node IP: ${DEST_NODE_IP}"
ok "Responder LoadBalancer IP: ${RESPONDER_IP}"

# ------------------------------------------------------------------
# Step 4: Configure MetalLB with a single-IP pool
# ------------------------------------------------------------------
info "Configuring MetalLB IPAddressPool and L2Advertisement"
kubectl apply --context "kind-${DEST_CLUSTER}" -f - <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: portal-pool
  namespace: metallb-system
spec:
  addresses:
    - ${RESPONDER_IP}/32
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: portal-l2
  namespace: metallb-system
spec:
  ipAddressPools:
    - portal-pool
EOF
ok "MetalLB configured"

# ------------------------------------------------------------------
# Step 5: Build portal binary
# ------------------------------------------------------------------
info "Building portal..."
(cd "${ROOT_DIR}" && go build -o "${PORTAL_BIN}" ./cmd/portal)
ok "Built ${PORTAL_BIN}"

# ------------------------------------------------------------------
# Step 6: Generate tunnel manifests
# ------------------------------------------------------------------
info "Generating tunnel manifests..."
"${PORTAL_BIN}" generate "kind-${SOURCE_CLUSTER}" "kind-${DEST_CLUSTER}" \
    --responder-endpoint "${RESPONDER_IP}:10443" \
    --output-dir "${OUTPUT_DIR}"
ok "Manifests written to ${OUTPUT_DIR}"

# ------------------------------------------------------------------
# Step 7: Inject echo-server sidecar into the responder deployment
# ------------------------------------------------------------------
info "Injecting echo-server sidecar into responder deployment..."
cp "${SCRIPT_DIR}/echo-sidecar-patch.yaml" "${OUTPUT_DIR}/destination/"
printf '\npatches:\n  - path: echo-sidecar-patch.yaml\n' >> "${OUTPUT_DIR}/destination/kustomization.yaml"
ok "Echo-server sidecar patch applied"

# ------------------------------------------------------------------
# Step 8: Deploy responder, then initiator
# ------------------------------------------------------------------
info "Deploying responder to ${DEST_CLUSTER}..."
kubectl apply -k "${OUTPUT_DIR}/destination/" --context "kind-${DEST_CLUSTER}"

info "Waiting for responder to be ready..."
kubectl wait deployment/portal-responder -n portal-system \
    --for=condition=Available --timeout=120s \
    --context "kind-${DEST_CLUSTER}"
ok "Responder is running"

info "Deploying initiator to ${SOURCE_CLUSTER}..."
kubectl apply -k "${OUTPUT_DIR}/source/" --context "kind-${SOURCE_CLUSTER}"

info "Waiting for initiator to be ready..."
kubectl wait deployment/portal-initiator -n portal-system \
    --for=condition=Available --timeout=120s \
    --context "kind-${SOURCE_CLUSTER}"
ok "Initiator is running"

# ------------------------------------------------------------------
# Step 9a: Verify the tunnel via Envoy stats
# ------------------------------------------------------------------
info "Port-forwarding to initiator admin API (port 15000)..."
kubectl port-forward deployment/portal-initiator -n portal-system 15000:15000 \
    --context "kind-${SOURCE_CLUSTER}" &
PF_PID=$!
sleep 3

info "Checking Envoy tunnel stats..."
echo ""
STATS=$(curl -s http://127.0.0.1:15000/stats | grep tunnel_to_responder || true)

if [[ -z "${STATS}" ]]; then
    warn "No tunnel_to_responder stats found — tunnel may not have connected yet"
else
    echo "${STATS}"
fi

kill "${PF_PID}" 2>/dev/null || true
PF_PID=""

# ------------------------------------------------------------------
# Step 9b: Send data through the tunnel (port-forward)
# ------------------------------------------------------------------
echo ""
info "Sending data through the tunnel..."
info "Port-forwarding to initiator tunnel port (10443)..."
kubectl port-forward deployment/portal-initiator -n portal-system 10443:10443 \
    --context "kind-${SOURCE_CLUSTER}" &
PF_PID=$!
sleep 3

info "Sending HTTP request: localhost:10443 → initiator → [mTLS tunnel] → responder → echo-server"
echo ""
RESPONSE=$(curl -s --max-time 5 http://127.0.0.1:10443 2>&1) || true

if [[ "${RESPONSE}" == *"Hello from the destination"* ]]; then
    echo -e "${GREEN}${RESPONSE}${NC}"
    echo ""
    ok "Data successfully traversed the tunnel!"
else
    warn "Unexpected response (tunnel may need a moment to establish):"
    echo "${RESPONSE}"
fi

kill "${PF_PID}" 2>/dev/null || true
PF_PID=""

# ------------------------------------------------------------------
# Step 9c: Send data from a pod inside the source cluster
# ------------------------------------------------------------------
info "Creating a temporary Service for the initiator..."
kubectl expose deployment/portal-initiator -n portal-system \
    --port=10443 --target-port=10443 --name=portal-initiator \
    --context "kind-${SOURCE_CLUSTER}" 2>/dev/null || true

info "Sending request from a pod inside the source cluster..."
echo ""
POD_RESPONSE=$(kubectl run curl-test -n portal-system --rm -i --restart=Never \
    --image=curlimages/curl:8.5.0 \
    --context "kind-${SOURCE_CLUSTER}" \
    -- curl -s --max-time 10 http://portal-initiator:10443 2>&1) || true

if [[ "${POD_RESPONSE}" == *"Hello from the destination"* ]]; then
    echo -e "${GREEN}${POD_RESPONSE}${NC}"
    echo ""
    ok "Pod-to-pod communication across clusters verified!"
else
    warn "Unexpected pod response (the curl image may still be pulling):"
    echo "${POD_RESPONSE}"
fi

# ------------------------------------------------------------------
# Summary
# ------------------------------------------------------------------
echo ""
echo -e "${GREEN}=== Demo Summary ===${NC}"
echo ""

info "Pods:"
kubectl get pods -n portal-system --context "kind-${SOURCE_CLUSTER}" 2>/dev/null || true
kubectl get pods -n portal-system --context "kind-${DEST_CLUSTER}" 2>/dev/null || true

echo ""
info "Responder Service:"
kubectl get svc portal-responder -n portal-system --context "kind-${DEST_CLUSTER}" 2>/dev/null || true

echo ""
echo -e "${GREEN}Data flow:${NC}"
echo "  curl (source cluster) → portal-initiator:10443 → [mTLS tunnel] → portal-responder:10443 → echo-server:10001"
echo ""
ok "Demo is running. To clean up: ./docs/demo/cleanup.sh"
