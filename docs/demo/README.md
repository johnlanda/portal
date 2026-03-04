# Portal Demo

This guide walks through Portal's tunnel generation and deployment workflow.
There are two sections:

1. **Quick Look** — run `portal generate` locally to inspect the output (no clusters needed)
2. **Full Demo** — spin up two KIND clusters, deploy the tunnel, send data through it, and verify end-to-end mTLS connectivity

## Prerequisites

### Quick Look

- Go 1.23+

### Full Demo

- Go 1.23+
- Docker
- [kind](https://kind.sigs.k8s.io/)
- kubectl

## Quick Look (No Clusters Required)

Build the CLI and generate tunnel manifests against two fake contexts. The
command writes Kubernetes manifests to disk — it does not contact any cluster.

```bash
# Build the portal binary
go build -o ./portal ./cmd/portal

# Generate manifests (contexts don't need to exist for generation)
./portal generate my-source my-destination \
  --responder-endpoint "10.0.0.1:10443" \
  --output-dir ./quicklook-tunnel
```

Inspect the output:

```
quicklook-tunnel/
├── ca/
│   ├── ca.crt
│   ├── ca.key
│   └── .gitignore
├── source/
│   ├── kustomization.yaml
│   ├── namespace.yaml
│   ├── portal-initiator-bootstrap-cm.yaml
│   ├── portal-initiator-deployment.yaml
│   ├── portal-initiator-networkpolicy.yaml
│   ├── portal-initiator-sa.yaml
│   └── portal-tunnel-tls-secret.yaml
├── destination/
│   ├── kustomization.yaml
│   ├── namespace.yaml
│   ├── portal-responder-bootstrap-cm.yaml
│   ├── portal-responder-deployment.yaml
│   ├── portal-responder-networkpolicy.yaml
│   ├── portal-responder-sa.yaml
│   ├── portal-responder-service.yaml
│   └── portal-tunnel-tls-secret.yaml
└── tunnel.yaml
```

Key files to look at:

- **`tunnel.yaml`** — metadata (contexts, endpoint, cert validity, rotation count)
- **`source/portal-initiator-deployment.yaml`** — Envoy initiator that dials the responder
- **`destination/portal-responder-service.yaml`** — LoadBalancer Service exposing the responder
- **`ca/`** — self-signed CA used to issue mTLS leaf certificates (keep private)

Clean up:

```bash
rm -rf ./quicklook-tunnel ./portal
```

## Full Demo (KIND Clusters)

### Automated

Run everything in one shot:

```bash
./docs/demo/demo.sh
```

When finished, tear down:

```bash
./docs/demo/cleanup.sh
```

### Manual Step-by-Step

#### Step 1: Create KIND Clusters

```bash
kind create cluster --name portal-source
kind create cluster --name portal-destination
```

Verify both are reachable:

```bash
kubectl cluster-info --context kind-portal-source
kubectl cluster-info --context kind-portal-destination
```

#### Step 2: Install MetalLB on Destination

MetalLB gives the responder Service a real LoadBalancer IP routable from the
source cluster (both KIND clusters share the same Docker network).

```bash
kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/v0.14.9/config/manifests/metallb-native.yaml \
  --context kind-portal-destination

# Wait for the controller to be ready
kubectl wait deployment/controller -n metallb-system \
  --for=condition=Available --timeout=120s \
  --context kind-portal-destination
```

#### Step 3: Derive a LoadBalancer IP from the KIND Docker Subnet

KIND clusters run inside Docker. We pick an unused IP on the same bridge
network so the source cluster can reach it.

```bash
DEST_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
  portal-destination-control-plane)
IFS='.' read -r a b _ _ <<< "$DEST_IP"
RESPONDER_IP="${a}.${b}.255.200"
echo "Responder IP will be: ${RESPONDER_IP}"
```

#### Step 4: Configure MetalLB with a Single-IP Pool

```bash
kubectl apply --context kind-portal-destination -f - <<EOF
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
```

#### Step 5: Build Portal

```bash
go build -o ./portal ./cmd/portal
```

#### Step 6: Generate Tunnel Manifests

```bash
./portal generate kind-portal-source kind-portal-destination \
  --responder-endpoint "${RESPONDER_IP}:10443" \
  --output-dir ./demo-tunnel
```

#### Step 7: Add an Echo Server to the Responder

The responder Envoy forwards incoming tunnel traffic to `127.0.0.1:10001`
(the `local_backend` cluster). To demonstrate real data flowing through the
tunnel, we inject an HTTP echo server as a sidecar in the responder pod using
a Kustomize patch.

```bash
# Copy the patch into the generated destination directory
cp ./docs/demo/echo-sidecar-patch.yaml ./demo-tunnel/destination/

# Register it in kustomization.yaml
printf '\npatches:\n  - path: echo-sidecar-patch.yaml\n' >> ./demo-tunnel/destination/kustomization.yaml
```

The patch adds a lightweight [hashicorp/http-echo](https://hub.docker.com/r/hashicorp/http-echo)
container that responds on port 10001. After this step the data flow is:

```
curl (source) → initiator:10443 → [mTLS tunnel] → responder:10443 → echo-server:10001
                                                                      ↓
                                                   "Hello from the destination cluster
                                                    through the Portal tunnel!"
```

#### Step 8: Deploy to Both Clusters

Deploy the responder first (destination), then the initiator (source):

```bash
# Responder
kubectl apply -k ./demo-tunnel/destination/ --context kind-portal-destination
kubectl wait deployment/portal-responder -n portal-system \
  --for=condition=Available --timeout=120s \
  --context kind-portal-destination

# Initiator
kubectl apply -k ./demo-tunnel/source/ --context kind-portal-source
kubectl wait deployment/portal-initiator -n portal-system \
  --for=condition=Available --timeout=120s \
  --context kind-portal-source
```

#### Step 9: Send Data Through the Tunnel

Now send an actual HTTP request through the tunnel and get a response from the
echo server running in the destination cluster.

**Option A — port-forward from your machine:**

```bash
kubectl port-forward deployment/portal-initiator -n portal-system 10443:10443 \
  --context kind-portal-source &
PF_PID=$!
sleep 3

curl -s http://127.0.0.1:10443
# → Hello from the destination cluster through the Portal tunnel!

kill $PF_PID 2>/dev/null
```

**Option B — pod-to-pod inside the source cluster:**

Create a temporary ClusterIP Service for the initiator, then run a curl pod:

```bash
# Expose the initiator inside the source cluster
kubectl expose deployment/portal-initiator -n portal-system \
  --port=10443 --target-port=10443 --name=portal-initiator \
  --context kind-portal-source

# Send a request from a pod in the source cluster
kubectl run curl-test -n portal-system --rm -i --restart=Never \
  --image=curlimages/curl:8.5.0 \
  --context kind-portal-source \
  -- curl -s --max-time 10 http://portal-initiator:10443
# → Hello from the destination cluster through the Portal tunnel!
```

Both options prove that data originating in the source cluster travels through
the mTLS tunnel to the destination cluster and back.

You can also verify the tunnel via Envoy stats:

```bash
kubectl port-forward deployment/portal-initiator -n portal-system 15000:15000 \
  --context kind-portal-source &
PF_PID=$!
sleep 3

curl -s http://127.0.0.1:15000/stats | grep tunnel_to_responder

kill $PF_PID 2>/dev/null
```

After sending traffic, a healthy tunnel shows:

| Stat | Expected |
|------|----------|
| `cluster.tunnel_to_responder.upstream_cx_total` | > 0 |
| `cluster.tunnel_to_responder.ssl.handshake` | > 0 |
| `cluster.tunnel_to_responder.upstream_cx_connect_fail` | 0 |

#### Step 10: Rotate Certificates (Optional)

After verifying the tunnel, test certificate rotation. Envoy picks up the new
certificates automatically via SDS `watched_directory` -- no pod restart is
required:

```bash
./portal rotate-certs ./demo-tunnel

# Apply the new secrets — Envoy reloads certs automatically via SDS
kubectl apply -f ./demo-tunnel/destination/portal-tunnel-tls-secret.yaml \
  --context kind-portal-destination
kubectl apply -f ./demo-tunnel/source/portal-tunnel-tls-secret.yaml \
  --context kind-portal-source

# No restart needed — wait ~60-90s for kubelet to sync the Secret volumes,
# then Envoy detects the filesystem change and reloads the certificates.
sleep 90
```

Re-run Step 9 to confirm the tunnel continues working with the new certificates
(zero downtime).

#### Step 11: Clean Up

```bash
kind delete cluster --name portal-source
kind delete cluster --name portal-destination
rm -rf ./demo-tunnel ./portal
```

Or use the cleanup script:

```bash
./docs/demo/cleanup.sh
```

## Troubleshooting

### MetalLB controller not ready

If the MetalLB controller pod stays in Pending, the KIND cluster may not have
pulled the images. Check:

```bash
kubectl get pods -n metallb-system --context kind-portal-destination
kubectl describe pod -n metallb-system -l app=metallb,component=controller \
  --context kind-portal-destination
```

### Responder Service stuck in `<pending>` external IP

MetalLB must be fully ready and the IPAddressPool must be applied before the
Service is created. If the IP stays pending:

```bash
# Verify MetalLB resources
kubectl get ipaddresspool,l2advertisement -n metallb-system \
  --context kind-portal-destination

# Delete and re-apply the responder manifests
kubectl delete -k ./demo-tunnel/destination/ --context kind-portal-destination
kubectl apply -k ./demo-tunnel/destination/ --context kind-portal-destination
```

### Pods in CrashLoopBackOff

Check Envoy logs:

```bash
# Initiator
kubectl logs deployment/portal-initiator -n portal-system \
  --context kind-portal-source

# Responder
kubectl logs deployment/portal-responder -n portal-system \
  --context kind-portal-destination
```

Common causes:
- Certificate files not mounted (check the Secret exists in the namespace)
- Bootstrap ConfigMap missing or malformed

### curl to the tunnel returns "connection refused" or empty response

The echo-server sidecar may not be running. Check:

```bash
# Verify the responder pod has two containers (envoy + echo-server)
kubectl get pods -n portal-system --context kind-portal-destination -o wide
kubectl describe deployment/portal-responder -n portal-system \
  --context kind-portal-destination

# Check echo-server container logs
kubectl logs deployment/portal-responder -n portal-system \
  -c echo-server --context kind-portal-destination
```

If the patch was not applied, re-run the kustomize steps from Step 7 and
re-deploy the destination manifests.

### Tunnel stats show `upstream_cx_connect_fail` > 0

The initiator cannot reach the responder. Check:

1. Responder Service has an external IP:
   ```bash
   kubectl get svc portal-responder -n portal-system --context kind-portal-destination
   ```
2. The external IP matches `--responder-endpoint` (minus the port)
3. Responder pod is running and healthy
4. Both clusters are on the same Docker network:
   ```bash
   docker network inspect kind
   ```
