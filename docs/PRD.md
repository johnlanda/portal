# Portal — PRD & Design Document

## Executive Summary

Portal is a CLI tool that creates secure, multiplexed reverse tunnels between Kubernetes clusters using Envoy Proxy. It eliminates the complexity of configuring cross-cluster connectivity by automating Envoy deployment, mTLS certificate provisioning, and tunnel lifecycle management.

A single command — `portal connect <source> <destination>` — deploys Envoy proxies on both clusters, generates and distributes certificates, and establishes a reverse tunnel that applications can use to communicate securely across cluster boundaries.

```
portal connect gke-us-east eks-eu-west
```

For GitOps workflows, `portal generate` produces the same manifests as files without touching any cluster, so they can be committed and reconciled by Argo CD, Flux, or any other GitOps controller.

```
portal generate gke-us-east eks-eu-west --output-dir ./infra/tunnels/
```

---

## Problem Statement

Connecting services across Kubernetes clusters — especially across clouds or network boundaries — is painful:

1. **Network complexity** — Clusters behind NATs, firewalls, or private VPCs can't accept inbound connections. VPN setup is heavyweight and operationally expensive.
2. **Per-service configuration** — Each cross-cluster connection requires its own ingress, DNS, TLS cert, and firewall rule. This doesn't scale.
3. **Security** — Ad-hoc solutions often skip mutual TLS, leave ports open, or rely on network-level trust that doesn't hold across clouds.
4. **Multiplexing** — HTTP/2 multiplexing through a single tunnel is far more efficient than opening N independent connections, but setting it up manually with Envoy is non-trivial.

### Existing Art

Synapse (this repo's parent project) implements a reverse tunnel for its management-plane-to-data-plane connectivity. That implementation is tightly coupled to the Synapse domain (data plane IDs, agent sidecars, config sync). Portal extracts the general-purpose tunnel primitive and makes it a standalone tool.

Other tools in this space (Submariner, Skupper, Cilium ClusterMesh) are full-blown service mesh or multi-cluster networking solutions. Portal is deliberately narrower: it does one thing — create a secure tunnel between two clusters — and does it well.

---

## Goals

1. **One command to connect two clusters** — `portal connect <source> <destination>` handles everything.
2. **GitOps-native** — `portal generate` outputs manifests that can be committed, reviewed, and reconciled by any GitOps controller. No runtime CLI dependency required.
3. **Zero inbound firewall rules on source** — Source cluster initiates outbound connections only. Destination must be reachable from source on a single port.
4. **Mutual TLS by default** — All tunnel traffic is encrypted and authenticated. No plaintext mode.
5. **Connection multiplexing** — Multiple application-level connections share a single HTTP/2 tunnel. Applications register services to expose through the tunnel.
6. **Multi-cloud / multi-cluster** — Works across any combination of clouds (GKE, EKS, AKS, on-prem) as long as the source can reach the destination on one port.
7. **Minimal footprint** — Two lightweight Envoy pods (one per cluster), a Secret, and a ConfigMap. No CRDs, no operators, no control plane.

### Non-Goals (v1)

- Service discovery or DNS integration (applications must use explicit tunnel endpoints)
- Mesh-level features (load balancing across clusters, failover, circuit breaking)
- Multi-hop tunnels (A → B → C)
- Non-Kubernetes environments (bare metal, VMs)
- Data path proxying (Portal tunnels configuration/control traffic; it is not a service mesh)

---

## User Personas

### Platform Engineer
Manages multi-cluster Kubernetes infrastructure. Needs to connect clusters across clouds for control plane communication, observability pipelines, or internal tooling. Wants a lightweight, auditable solution — not another mesh to operate. Likely uses GitOps and wants manifests in a repo.

### Application Developer
Needs their service in Cluster A to call a service in Cluster B. Doesn't want to think about networking, certs, or firewall rules. Wants a stable endpoint they can point their client at.

---

## User Experience

### Core CLI Commands

```
# Direct mode — apply to clusters immediately
portal connect <source_context> <destination_context> [flags]
portal disconnect <source_context> <destination_context>
portal expose <context> <service> --port <port> [--tunnel <tunnel_name>]
portal status [<source_context> <destination_context>]
portal list

# GitOps mode — generate manifests to disk
portal generate <source_context> <destination_context> [flags]
portal generate expose <context> <service> --port <port> [--tunnel <tunnel_name>]
```

### Workflow A: Direct Mode (Imperative)

#### 1. Connect Two Clusters

```bash
# Source cluster (behind NAT) initiates tunnel to destination (reachable).
# Source = kube context that will run the initiator Envoy.
# Destination = kube context that will run the responder Envoy.
$ portal connect gke-us-east eks-eu-west

✓ Generated tunnel CA and certificates
✓ Deployed responder Envoy in eks-eu-west (namespace: portal-system)
✓ Deployed initiator Envoy in gke-us-east (namespace: portal-system)
✓ Tunnel established: gke-us-east → eks-eu-west

Tunnel name:  gke-us-east--eks-eu-west
Tunnel port:  portal-tunnel.portal-system.svc:10443
Status:       Connected
```

#### 2. Expose a Service Through the Tunnel

```bash
# Make a service in gke-us-east reachable from eks-eu-west
$ portal expose gke-us-east my-api --port 8080

✓ Service "my-api:8080" registered on tunnel gke-us-east--eks-eu-west
  Reachable from eks-eu-west at: portal-gke-us-east-my-api.portal-system.svc:8080

# Make a service in eks-eu-west reachable from gke-us-east
$ portal expose eks-eu-west their-db --port 5432

✓ Service "their-db:5432" registered on tunnel gke-us-east--eks-eu-west
  Reachable from gke-us-east at: portal-eks-eu-west-their-db.portal-system.svc:5432
```

#### 3. Check Status

```bash
$ portal status

TUNNEL                        STATUS      SERVICES    AGE
gke-us-east--eks-eu-west      Connected   2           4h
```

```bash
$ portal status gke-us-east eks-eu-west

Tunnel:     gke-us-east--eks-eu-west
Status:     Connected
Uptime:     4h12m
Initiator:  gke-us-east (portal-system/portal-initiator)
Responder:  eks-eu-west (portal-system/portal-responder)

SERVICES EXPOSED
  gke-us-east/my-api:8080    → eks-eu-west/portal-gke-us-east-my-api.portal-system:8080
  eks-eu-west/their-db:5432  → gke-us-east/portal-eks-eu-west-their-db.portal-system:5432

CONNECTIONS
  Active streams:  3
  Total streams:   1,247
  Bytes TX:        45.2 MB
  Bytes RX:        128.7 MB
```

#### 4. Disconnect

```bash
$ portal disconnect gke-us-east eks-eu-west

✓ Removed initiator from gke-us-east
✓ Removed responder from eks-eu-west
✓ Cleaned up certificates and configuration
```

### Workflow B: GitOps Mode (Declarative)

For teams that manage infrastructure through Git, `portal generate` produces the full set of Kubernetes manifests and certificates without applying anything. The output is structured for committing directly into a GitOps repo.

#### 1. Generate Tunnel Manifests

`portal generate` requires `--responder-endpoint` since the responder's address can't be discovered at generation time. Two common approaches:

**Option A: Pre-allocated static IP**
Reserve a static IP from your cloud provider (GCP: `gcloud compute addresses create`, AWS: Elastic IP, Azure: `az network public-ip create`), then pass it directly. Portal annotates the responder Service to claim that IP.

```bash
$ portal generate gke-us-east eks-eu-west \
    --responder-endpoint 34.120.1.50:10443 \
    --output-dir ./infra/tunnels/gke-us-east--eks-eu-west

✓ Generated tunnel CA and certificates
✓ Wrote source cluster manifests      → ./infra/tunnels/gke-us-east--eks-eu-west/source/
✓ Wrote destination cluster manifests → ./infra/tunnels/gke-us-east--eks-eu-west/destination/
  Responder Service loadBalancerIP: 34.120.1.50

Output structure:
  ./infra/tunnels/gke-us-east--eks-eu-west/
  ├── source/                          # Apply to gke-us-east
  │   ├── namespace.yaml
  │   ├── portal-initiator-deployment.yaml
  │   ├── portal-initiator-bootstrap-cm.yaml
  │   ├── portal-tunnel-tls-secret.yaml
  │   └── kustomization.yaml
  ├── destination/                     # Apply to eks-eu-west
  │   ├── namespace.yaml
  │   ├── portal-responder-deployment.yaml
  │   ├── portal-responder-bootstrap-cm.yaml
  │   ├── portal-responder-service.yaml
  │   ├── portal-tunnel-tls-secret.yaml
  │   └── kustomization.yaml
  └── tunnel.yaml                      # Portal metadata (contexts, ports, tunnel name)
```

**Option B: DNS name with external-dns**
If the destination cluster runs [external-dns](https://github.com/kubernetes-sigs/external-dns), pass a hostname instead. Portal adds the `external-dns.alpha.kubernetes.io/hostname` annotation to the responder Service. The initiator uses STRICT_DNS resolution — no IP needed.

```bash
$ portal generate gke-us-east eks-eu-west \
    --responder-endpoint tunnel.infra.example.com:10443 \
    --output-dir ./infra/tunnels/gke-us-east--eks-eu-west

✓ Generated tunnel CA and certificates
✓ Wrote source cluster manifests      → ./infra/tunnels/gke-us-east--eks-eu-west/source/
✓ Wrote destination cluster manifests → ./infra/tunnels/gke-us-east--eks-eu-west/destination/
  Responder Service annotation: external-dns.alpha.kubernetes.io/hostname=tunnel.infra.example.com

# Responder server cert SANs automatically include tunnel.infra.example.com
```

Both approaches produce fully deterministic manifests — no post-apply patching required.

For `portal connect` (imperative mode), `--responder-endpoint` is **optional**. If omitted, Portal deploys the responder, waits for the LoadBalancer IP to be assigned, then uses it to configure the initiator.

#### 2. Generate Exposed Service Manifests

```bash
$ portal generate expose gke-us-east my-api --port 8080 \
    --tunnel gke-us-east--eks-eu-west \
    --output-dir ./infra/tunnels/gke-us-east--eks-eu-west

✓ Wrote service manifests → ./infra/tunnels/gke-us-east--eks-eu-west/destination/services/
✓ Updated initiator bootstrap ConfigMap

Output:
  destination/services/portal-gke-us-east-my-api-svc.yaml
  source/portal-initiator-bootstrap-cm.yaml  (updated with new route)
```

#### 3. Sealed Secrets Integration

By default, `portal generate` writes plain Kubernetes Secrets (the certificates). For teams using Sealed Secrets, SOPS, or external secret stores:

```bash
# Output cert values to separate files for encryption
$ portal generate gke-us-east eks-eu-west \
    --output-dir ./infra/tunnels/ \
    --secret-format raw

# Certs written as raw PEM files instead of Secret manifests:
#   source/certs/tls.crt, tls.key, ca.crt
#   destination/certs/tls.crt, tls.key, ca.crt
# You can then encrypt these with your tool of choice and
# create Secrets/SealedSecrets/ExternalSecrets yourself.

# Or use Sealed Secrets directly
$ portal generate gke-us-east eks-eu-west \
    --output-dir ./infra/tunnels/ \
    --secret-format sealed-secret \
    --sealed-secret-cert ./pub-cert.pem
```

#### 4. Regenerate After Changes

`portal generate` is idempotent. Running it again with the same arguments regenerates all manifests. Certificates are only regenerated if `--regenerate-certs` is passed or the cert files don't exist yet in the output directory.

```bash
# Regenerate manifests (reuses existing certs)
$ portal generate gke-us-east eks-eu-west \
    --output-dir ./infra/tunnels/gke-us-east--eks-eu-west

# Force new certificates
$ portal generate gke-us-east eks-eu-west \
    --output-dir ./infra/tunnels/gke-us-east--eks-eu-west \
    --regenerate-certs
```

### Shared Flags

```
# Common to both connect and generate
  --responder-endpoint <addr>   Responder address (IP or hostname:port).
                                REQUIRED for generate. Optional for connect (auto-discovered).
                                Accepts IP (34.120.1.50:10443) or DNS (tunnel.example.com:10443).
                                IP: sets loadBalancerIP on responder Service.
                                DNS: adds external-dns annotation + includes hostname in cert SANs.
  --namespace <ns>              Namespace for portal components (default: portal-system)
  --tunnel-port <port>          Responder listen port (default: 10443)
  --connection-count <n>        Number of reverse connections to maintain (default: 4)
  --cert-validity <dur>         Certificate validity duration (default: 8760h / 1 year)
  --cert-dir <path>             Use existing certificates instead of generating
  --envoy-image <image>         Envoy image (default: envoyproxy/envoy:v1.37-latest)
  --envoy-log-level <lvl>       Envoy log level (default: info)
  --service-type <type>         Responder Service type: LoadBalancer (default), NodePort, ClusterIP

# connect only
  --dry-run                 Print manifests to stdout without applying (quick alternative to generate)

# generate only
  --output-dir <path>       REQUIRED. Directory to write manifests
  --output-format <fmt>     Manifest format: yaml (default), kustomize, helm
  --secret-format <fmt>     Secret format: k8s-secret (default), raw, sealed-secret
  --sealed-secret-cert      Public cert for Sealed Secrets encryption
  --regenerate-certs        Force regeneration of certificates
```

---

## Architecture

### High-Level Overview

```
┌─────────────────────────────────────────────────┐
│              Source Cluster                      │
│              (Initiator)                         │
│                                                  │
│  ┌─────────┐        ┌──────────────────────┐    │
│  │  App A  │──────▶ │  Portal Initiator    │    │
│  └─────────┘  svc   │  (Envoy)             │    │
│                      │                      │────┼──── outbound TCP ────┐
│  ┌─────────┐        │  • rc:// listener    │    │                      │
│  │  App B  │──────▶ │  • downstream socket │    │                      │
│  └─────────┘  svc   │    interface         │    │                      │
│                      └──────────────────────┘    │                      │
└─────────────────────────────────────────────────┘                      │
                                                                         │
                                                                    mTLS/H2
                                                                         │
┌─────────────────────────────────────────────────┐                      │
│              Destination Cluster                 │                      │
│              (Responder)                         │                      │
│                                                  │                      │
│  ┌─────────┐        ┌──────────────────────┐    │                      │
│  │  Svc X  │◀────── │  Portal Responder    │◀───┼──────────────────────┘
│  └─────────┘  route  │  (Envoy)            │    │
│                      │                      │    │
│  ┌─────────┐        │  • upstream socket   │    │
│  │  Svc Y  │◀────── │    interface         │    │
│  └─────────┘  route  │  • reverse conn     │    │
│                      │    cluster           │    │
│                      │  • tunnel filter     │    │
│                      └──────────────────────┘    │
└─────────────────────────────────────────────────┘
```

### Connection Modes

Portal supports two Envoy configuration modes:

#### Mode 1: TCP Proxy (Default, v1)

Simple and proven — the approach used in Synapse today.

- **Initiator**: Envoy TCP proxy on a local port, forwards to responder endpoint.
- **Responder**: Envoy TCP proxy, forwards to backend services.
- **Multiplexing**: Achieved via HTTP/2 upstream connections.
- **Pros**: Works with current stable Envoy. Simple to debug.
- **Cons**: Less granular routing. Each exposed service needs a separate listener/port pair.

```
App → localhost:PORT → [Initiator Envoy TCP Proxy] → [Responder Envoy TCP Proxy] → Backend Svc
```

#### Mode 2: Reverse Connection (Future, v2)

Uses Envoy's native `reverse_connection` extensions for true multiplexed reverse tunnels.

- **Initiator**: Uses `rc://` addressing with `downstream_socket_interface` bootstrap extension.
- **Responder**: Uses `upstream_socket_interface`, `reverse_tunnel` network filter, and `reverse_connection` cluster.
- **Multiplexing**: Native HTTP/2 stream multiplexing over cached reverse connections.
- **Pros**: Single port, true multiplexing, header-based routing to specific services.
- **Cons**: Requires Envoy builds with reverse_connection extensions (currently experimental).

```
App → [Initiator rc:// listener] ═══ reverse tunnel ═══▶ [Responder reverse_connection cluster] → Backend Svc
                                    (single TCP conn,
                                     multiplexed H2 streams)
```

### Component Details

#### Initiator Envoy (Source Cluster)

Deployed as a `Deployment` with 1 replica (configurable). Responsible for:

1. Establishing outbound TCP connection(s) to the responder
2. Maintaining connection health (keepalives, reconnection)
3. Proxying application traffic from local services through the tunnel

**Kubernetes Resources:**
- `Deployment`: portal-initiator
- `ConfigMap`: portal-initiator-bootstrap (Envoy config)
- `Secret`: portal-tunnel-tls (client cert, key, CA)
- `Service`: One per exposed remote service (ClusterIP, pointing to initiator)

#### Responder Envoy (Destination Cluster)

Deployed as a `Deployment` with 1 replica (configurable). Responsible for:

1. Listening for incoming tunnel connections from initiators
2. Routing traffic arriving through the tunnel to local services
3. TLS termination for tunnel connections (verifies initiator cert)

**Kubernetes Resources:**
- `Deployment`: portal-responder
- `ConfigMap`: portal-responder-bootstrap (Envoy config)
- `Secret`: portal-tunnel-tls (server cert, key, CA)
- `Service`: portal-responder (LoadBalancer or NodePort, external-facing)
- `Service`: One per exposed local service (ClusterIP, pointing to responder)

#### Certificate Authority

Portal generates a per-tunnel CA and issues:

| Certificate | Location | CN | Usage |
|---|---|---|---|
| Tunnel CA | Both clusters | portal-ca | Root of trust |
| Initiator client cert | Source cluster | portal-initiator/\<tunnel-name\> | mTLS client auth |
| Responder server cert | Destination cluster | portal-responder/\<tunnel-name\> | mTLS server auth |

All certs are stored as Kubernetes Secrets. The CA private key is stored only locally on the machine that ran `portal connect` (and optionally in a user-specified secret store).

### Data Flow

#### Exposing a Service (Source → Destination direction)

When `portal expose gke-us-east my-api --port 8080` is run:

1. **Destination cluster** (eks-eu-west): A new `ClusterIP Service` is created:
   - Name: `portal-gke-us-east-my-api`
   - Port: 8080
   - Target: portal-responder pod

2. **Responder Envoy config** updated: New route added — traffic arriving on the reverse connection cluster for `my-api` is forwarded to the responder's egress listener.

3. **Source cluster** (gke-us-east): Initiator Envoy config updated — new upstream route for `my-api:8080` through the tunnel.

4. **Traffic flow**:
   ```
   [eks-eu-west pod] → portal-gke-us-east-my-api.portal-system:8080
     → [Responder Envoy] → reverse tunnel → [Initiator Envoy]
     → my-api.default.svc:8080 [in gke-us-east]
   ```

#### Exposing a Service (Destination → Source direction)

When `portal expose eks-eu-west their-db --port 5432` is run:

1. **Source cluster** (gke-us-east): A new `ClusterIP Service` is created:
   - Name: `portal-eks-eu-west-their-db`
   - Port: 5432
   - Target: portal-initiator pod

2. **Initiator Envoy config** updated: New listener/route for `their-db:5432` proxied through the tunnel.

3. **Traffic flow**:
   ```
   [gke-us-east pod] → portal-eks-eu-west-their-db.portal-system:5432
     → [Initiator Envoy] → tunnel → [Responder Envoy]
     → their-db.default.svc:5432 [in eks-eu-west]
   ```

---

## Technical Design

### Project Structure

```
portal/
├── cmd/
│   └── portal/
│       └── main.go              # CLI entrypoint
├── internal/
│   ├── cli/
│   │   ├── connect.go           # portal connect
│   │   ├── disconnect.go        # portal disconnect
│   │   ├── expose.go            # portal expose
│   │   ├── generate.go          # portal generate
│   │   ├── status.go            # portal status
│   │   └── list.go              # portal list
│   ├── tunnel/
│   │   ├── tunnel.go            # Tunnel lifecycle (create, destroy, update)
│   │   ├── initiator.go         # Initiator deployment logic
│   │   └── responder.go         # Responder deployment logic
│   ├── manifest/
│   │   ├── render.go            # Render full manifest set for a tunnel
│   │   ├── render_test.go
│   │   ├── writer.go            # Write manifests to disk (generate) or apply to cluster (connect)
│   │   └── kustomize.go         # Kustomization.yaml generation
│   ├── envoy/
│   │   ├── config.go            # Bootstrap config rendering
│   │   ├── templates/
│   │   │   ├── initiator_tcp.yaml
│   │   │   ├── responder_tcp.yaml
│   │   │   ├── initiator_rc.yaml      # v2
│   │   │   └── responder_rc.yaml      # v2
│   │   └── config_test.go
│   ├── certs/
│   │   ├── ca.go                # CA generation
│   │   ├── certs.go             # Cert issuance (initiator, responder)
│   │   └── certs_test.go
│   ├── kube/
│   │   ├── client.go            # Multi-context kube client factory
│   │   ├── deploy.go            # Apply/delete Kubernetes resources
│   │   └── service.go           # Service creation for exposed services
│   └── state/
│       └── state.go             # Local tunnel state file (~/.portal/tunnels.json)
├── docs/
│   └── PRD.md                   # This document
├── go.mod
├── go.sum
└── CLAUDE.md
```

### Core Abstraction: Manifest Bundle

Both `connect` and `generate` share the same rendering pipeline. The only difference is the output sink.

```
                     ┌─────────────────┐
  TunnelConfig ────▶ │  manifest.Render │ ────▶ ManifestBundle
                     └─────────────────┘            │
                                                    ├──▶ manifest.Apply(kubeClient)   # connect
                                                    ├──▶ manifest.WriteToDisk(dir)    # generate
                                                    └──▶ manifest.PrintToStdout()     # --dry-run
```

```go
// ManifestBundle contains all Kubernetes resources for both sides of a tunnel.
type ManifestBundle struct {
    Source      []unstructured.Unstructured  // Resources for the source/initiator cluster
    Destination []unstructured.Unstructured  // Resources for the destination/responder cluster
    Metadata    TunnelMetadata               // Tunnel name, contexts, ports, etc.
}
```

This ensures `portal connect` and `portal generate` always produce identical resources — the only difference is whether they're applied or written to files.

### State Management

Portal stores tunnel metadata locally at `~/.portal/tunnels.json`:

```json
{
  "tunnels": [
    {
      "name": "gke-us-east--eks-eu-west",
      "source_context": "gke-us-east",
      "destination_context": "eks-eu-west",
      "namespace": "portal-system",
      "tunnel_port": 10443,
      "created_at": "2026-02-27T10:00:00Z",
      "ca_cert_path": "~/.portal/certs/gke-us-east--eks-eu-west/ca.crt",
      "mode": "imperative",
      "services": [
        {
          "cluster": "gke-us-east",
          "name": "my-api",
          "port": 8080,
          "remote_service": "portal-gke-us-east-my-api.portal-system.svc"
        }
      ]
    }
  ]
}
```

For GitOps tunnels, the local state records the output directory instead of tracking live cluster resources. The `tunnel.yaml` metadata file in the output directory is the durable source of truth.

### Certificate Lifecycle

```
portal connect / portal generate
  │
  ├─ Generate CA key pair (RSA 4096)
  │   ├─ connect: stored locally (~/.portal/certs/) + in both clusters as Secrets
  │   └─ generate: written to output-dir (raw PEM or wrapped in Secret manifests)
  │
  ├─ Issue responder server cert
  │   ├─ CN: portal-responder/<tunnel-name>
  │   ├─ SANs: responder K8s service DNS names +
  │   │        if --responder-endpoint is IP → IP SAN
  │   │        if --responder-endpoint is hostname → DNS SAN
  │   ├─ Validity: --cert-validity (default 1 year)
  │   └─ Output: Secret manifest or raw PEM
  │
  └─ Issue initiator client cert
      ├─ CN: portal-initiator/<tunnel-name>
      ├─ ExtKeyUsage: ClientAuth
      ├─ Validity: --cert-validity (default 1 year)
      └─ Output: Secret manifest or raw PEM

portal disconnect
  │
  └─ Delete Secrets in both clusters, remove local CA files
```

Future: `portal rotate-certs <tunnel>` for zero-downtime certificate rotation.

### Envoy Configuration (TCP Proxy Mode — v1)

#### Initiator Bootstrap

```yaml
static_resources:
  listeners:
  # One listener per exposed remote service (destination → source direction)
  - name: expose_their-db
    address:
      socket_address:
        address: 0.0.0.0
        port_value: 5432
    filter_chains:
    - filters:
      - name: envoy.filters.network.tcp_proxy
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
          stat_prefix: their-db
          cluster: tunnel_to_responder

  clusters:
  - name: tunnel_to_responder
    type: STRICT_DNS
    connect_timeout: 30s
    load_assignment:
      cluster_name: tunnel_to_responder
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: <responder-external-endpoint>
                port_value: 10443
    transport_socket:
      name: envoy.transport_sockets.tls
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
        common_tls_context:
          tls_certificates:
          - certificate_chain: { filename: /etc/portal/certs/tls.crt }
            private_key: { filename: /etc/portal/certs/tls.key }
          validation_context:
            trusted_ca: { filename: /etc/portal/certs/ca.crt }
          alpn_protocols: ["h2"]
```

#### Responder Bootstrap

```yaml
static_resources:
  listeners:
  - name: tunnel_listener
    address:
      socket_address:
        address: 0.0.0.0
        port_value: 10443
    filter_chains:
    - filters:
      - name: envoy.filters.network.tcp_proxy
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
          stat_prefix: tunnel
          cluster: local_backend
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          require_client_certificate: true
          common_tls_context:
            tls_certificates:
            - certificate_chain: { filename: /etc/portal/certs/tls.crt }
              private_key: { filename: /etc/portal/certs/tls.key }
            validation_context:
              trusted_ca: { filename: /etc/portal/certs/ca.crt }
            alpn_protocols: ["h2"]

  # One cluster per exposed local service (source → destination direction)
  clusters:
  - name: local_my-api
    type: STRICT_DNS
    connect_timeout: 5s
    load_assignment:
      cluster_name: local_my-api
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: my-api.default.svc.cluster.local
                port_value: 8080
```

### Envoy Configuration (Reverse Connection Mode — v2)

The v2 mode uses Envoy's native reverse connection extensions, enabling true multiplexed bidirectional communication over a single set of TCP connections. This is the target architecture; v1 TCP proxy mode exists to ship quickly with proven technology.

#### Initiator Bootstrap (v2)

```yaml
bootstrap_extensions:
- name: envoy.bootstrap.reverse_tunnel.downstream_socket_interface
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.bootstrap.reverse_tunnel.downstream_socket_interface.v3.DownstreamReverseConnectionSocketInterface
    stat_prefix: portal_initiator

static_resources:
  listeners:
  # Reverse tunnel listener — establishes N connections to responder
  - name: rc_tunnel
    address:
      rc://$(POD_NAME):<tunnel-name>:portal@responder-cluster:<connection-count>
    # rc:// address format: rc://<src_node_id>:<src_cluster_id>:<src_tenant_id>@<cluster>:<count>
    #   src_node_id   = $(POD_NAME) via Downward API — must be globally unique, enables targeting specific pods
    #   src_cluster_id = tunnel name — shared across replicas, enables load-balanced routing
    #   src_tenant_id  = "portal" — reserved for future multi-tenant isolation
    address_resolver:
      name: envoy.resolvers.reverse_connection

  # Local ingress — apps in this cluster send traffic here
  - name: local_ingress
    address:
      socket_address:
        address: 0.0.0.0
        port_value: 10080
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: local_ingress
          route_config:
            virtual_hosts:
            - name: services
              domains: ["*"]
              routes:
              - match: { prefix: "/" }
                route: { cluster: tunnel_to_responder }

  clusters:
  - name: responder-cluster
    type: STRICT_DNS
    connect_timeout: 30s
    load_assignment:
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: <responder-external-endpoint>
                port_value: 10443
    transport_socket:
      # mTLS config (same as v1)
```

> **Note:** The `additional_addresses` field on the rc:// listener enables connecting to multiple responder clusters simultaneously (mesh topologies). Not needed for v1 point-to-point tunnels but documented for future extension.

#### Responder Bootstrap (v2)

```yaml
bootstrap_extensions:
- name: envoy.bootstrap.reverse_tunnel.upstream_socket_interface
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.bootstrap.reverse_tunnel.upstream_socket_interface.v3.UpstreamReverseConnectionSocketInterface
    stat_prefix: portal_responder

static_resources:
  listeners:
  # Tunnel handshake listener — accepts initiator connections
  - name: tunnel_handshake
    address:
      socket_address:
        address: 0.0.0.0
        port_value: 10443
    filter_chains:
    - filters:
      - name: envoy.filters.network.reverse_tunnel
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.reverse_tunnel.v3.ReverseTunnel
          ping_interval: 2s
      transport_socket:
        # mTLS config (same as v1)

  # Egress — routes traffic through the tunnel to initiator-side services
  - name: egress
    address:
      socket_address:
        address: 0.0.0.0
        port_value: 10080
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: egress
          route_config:
            virtual_hosts:
            - name: remote_services
              domains: ["*"]
              routes:
              - match: { prefix: "/" }
                route: { cluster: reverse_connection_cluster }
          http_filters:
          - name: envoy.filters.http.lua
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
              inline_code: |
                function envoy_on_request(handle)
                  local node_id = handle:headers():get("x-node-id")
                  local cluster_id = handle:headers():get("x-cluster-id")
                  if node_id then
                    handle:headers():add("x-computed-host-id", node_id)
                  elseif cluster_id then
                    handle:headers():add("x-computed-host-id", cluster_id)
                  else
                    handle:logErr("Missing x-node-id or x-cluster-id routing header")
                    handle:respond({[":status"] = "400"}, "Missing routing header")
                  end
                end
          - name: envoy.filters.http.router

  clusters:
  - name: reverse_connection_cluster
    connect_timeout: 200s
    lb_policy: CLUSTER_PROVIDED
    cluster_type:
      name: envoy.clusters.reverse_connection
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.clusters.reverse_connection.v3.ReverseConnectionClusterConfig
        cleanup_interval: 60s
        host_id_format: "%REQ(x-computed-host-id)%"
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}
```

### Exposed Service Routing (v2)

In v2 mode, services are exposed via HTTP header-based routing rather than separate ports. Each exposed service gets a `ClusterIP Service` + `headless endpoint` that routes to the Envoy pod, with the service name injected as a header.

Applications call the exposed service by its Kubernetes service name. An init container or sidecar adds the `x-node-id` (for pod-specific targeting) or `x-cluster-id` (for load-balanced routing) header, or the application can set it directly.

---

## Implementation Plan

### Phase 1: TCP Proxy Mode (v1)

Minimum viable tunnel using the proven Synapse approach.

| Step | Description |
|------|-------------|
| 1.1 | Project scaffolding: Go module, CLI framework (cobra), kubeconfig context handling |
| 1.2 | Certificate generation: CA + initiator/responder certs (extract from Synapse `certgen.go`) |
| 1.3 | Envoy bootstrap templating: Initiator and responder TCP proxy configs |
| 1.4 | `ManifestBundle` rendering: Shared pipeline producing the full set of K8s resources |
| 1.5 | `portal generate`: Write ManifestBundle to disk with Kustomization files |
| 1.6 | `portal connect`: Apply ManifestBundle to clusters, wait for external IP, verify connectivity |
| 1.7 | `portal disconnect`: Tear down both sides, clean up secrets |
| 1.8 | `portal status`: Query Envoy admin API through kubectl port-forward for connection stats |
| 1.9 | `portal expose` / `portal generate expose`: Add listener/cluster pair, update ConfigMap |
| 1.10 | `portal list`: List tunnels from local state |
| 1.11 | E2E tests with KIND multi-cluster setup |

### Phase 2: Reverse Connection Mode (v2)

True multiplexed tunnels using Envoy reverse_connection extensions.

| Step | Description |
|------|-------------|
| 2.1 | Validate upstream Envoy version (v1.28+) supports reverse_connection extensions (experimental, already available) |
| 2.2 | Reverse connection bootstrap templates (initiator + responder) |
| 2.3 | Header-based service routing through the tunnel |
| 2.4 | `portal expose` updates for v2 (header injection, single-port model) |
| 2.5 | CDS/xDS integration for dynamic service exposure (avoids pod restart on `portal expose`) |
| 2.6 | Connection health monitoring and automatic reconnection |
| 2.7 | Migration path from v1 to v2 |

### Phase 3: Operational Maturity

| Step | Description |
|------|-------------|
| 3.1 | `portal rotate-certs`: Zero-downtime certificate rotation |
| 3.2 | `portal logs`: Stream Envoy access logs from both sides |
| 3.3 | Prometheus metrics endpoint on each Envoy (tunnel stats, connection counts, bytes transferred) |
| 3.4 | `portal doctor`: Diagnose connectivity issues (firewall, DNS, cert expiry) |
| 3.5 | Helm chart for declarative tunnel management (alternative to generate) |
| 3.6 | Sealed Secrets / SOPS / External Secrets integration for `portal generate` |

---

## Security Model

### Trust Boundary

Each tunnel has its own CA. A compromised CA only affects that single tunnel — not other tunnels or cluster-level trust.

### Certificate Pinning

Both sides verify the peer's certificate against the tunnel-specific CA. No system CA trust is used. This prevents MITM even if a cluster's default CA is compromised.

### Identity

- Initiator presents a client cert with `CN: portal-initiator/<tunnel-name>`
- Responder verifies the CN matches the expected tunnel
- Responder presents a server cert with `CN: portal-responder/<tunnel-name>`
- Initiator verifies the CN and SANs

### Least Privilege

Portal creates a dedicated namespace (`portal-system`) and a ServiceAccount with minimal RBAC:
- Read/write to its own namespace only
- No cluster-admin, no CRD creation
- Envoy runs as non-root with read-only root filesystem

### Secret Management

| Secret | Stored In | Rotation |
|--------|-----------|----------|
| CA private key | Local filesystem (`~/.portal/certs/`) or `--output-dir` | Manual (v1), automated (v3) |
| Initiator client cert + key | Source cluster Secret or generated manifest | `portal rotate-certs` (v3) |
| Responder server cert + key | Destination cluster Secret or generated manifest | `portal rotate-certs` (v3) |
| CA public cert | Both clusters (Secret or manifest) | Follows CA rotation |

For GitOps workflows, the `--secret-format` flag controls how secrets appear in generated output. Teams with existing PKI can use `--cert-dir` to provide their own certificates and skip Portal's CA entirely.

### TLS Protocol Security

All Envoy TLS contexts enforce TLS 1.3 minimum via `tls_minimum_protocol_version: TLSv1_3`. Rationale: TLS 1.2 cipher suites have known weaknesses; TLS 1.3 is the floor for new infrastructure.

### Server Name Indication (SNI)

Initiator sets SNI to the responder endpoint hostname in the upstream TLS context. This ensures the TLS handshake validates the responder's identity matches the expected endpoint, preventing misdirected connections.

### Network Isolation

Portal generates `NetworkPolicy` resources for both clusters:

- **Responder**: allows ingress only on the tunnel port, egress only to in-cluster services + DNS
- **Initiator**: allows ingress from namespace pods (for exposed services), egress to responder endpoint + in-cluster services + DNS
- Clusters without a NetworkPolicy controller silently ignore these resources (safe no-op)

### SPKI Pinning (deferred to Phase 3)

Subject Public Key Info pinning provides defense-in-depth against CA compromise. Since Portal uses per-tunnel self-signed CAs (the CA itself is the trust anchor), SPKI pinning is less critical than in public-CA environments. Deferred to Phase 3 alongside cert rotation.

---

## Comparison with Synapse Implementation

| Aspect | Synapse | Portal |
|--------|---------|--------|
| Purpose | Management plane ↔ data plane agent | General-purpose cluster-to-cluster |
| Identity model | Data plane ID in cert CN (`dp/<id>`) | Tunnel name in cert CN |
| Service exposure | Single gRPC service (config sync) | N arbitrary services |
| Tunnel direction | Always agent → backend | Bidirectional service exposure |
| Configuration | Helm chart values, CRD-driven | CLI-driven or GitOps manifests |
| Envoy mode | TCP proxy (rc:// in development) | TCP proxy (v1), rc:// (v2) |
| Dependencies | Full Synapse backend, controller, CRDs | None — standalone CLI + Envoy |
| Deployment model | Imperative (CLI + Helm) | Imperative or declarative (GitOps) |

### Code Reuse from Synapse

The following Synapse packages can be adapted for Portal:

1. `internal/crypto/certgen.go` → `internal/certs/` — CA and cert generation (generalize CN format)
2. `internal/connectivity/envoy/config.go` → `internal/envoy/config.go` — Bootstrap templating
3. `internal/connectivity/envoy/*.yaml` → `internal/envoy/templates/` — Bootstrap templates (adapt for multi-service routing)

---

## Success Metrics

1. **Time to connect**: < 2 minutes from `portal connect` to verified tunnel (excluding cloud LB provisioning)
2. **Time to generate**: < 5 seconds from `portal generate` to manifests on disk
3. **Time to expose**: < 10 seconds from `portal expose` to service reachable
4. **Overhead**: < 1ms added latency per hop, < 50MB memory per Envoy pod
5. **Reliability**: Tunnel auto-reconnects within 30s of transient network failure
6. **Adoption**: Usable without reading docs beyond `portal --help`

---

## Open Questions

### Resolved (v1)

1. ~~**Responder service type**~~: `LoadBalancer` default with `--service-type` flag for `NodePort`/`ClusterIP`.
2. ~~**xDS for dynamic config**~~: Not for v1. Pod rolling on `portal expose` is acceptable. xDS is a v2+ optimization.
3. ~~**Responder endpoint in GitOps**~~: `--responder-endpoint` is required for `portal generate`. Users provide either a pre-allocated static IP (sets `loadBalancerIP` on the responder Service) or a DNS hostname (adds `external-dns` annotation). Both produce fully deterministic manifests with no post-apply patching.
4. ~~**Multiple initiators**~~: YES. The upstream rc:// format natively supports this: unique `src_node_id` per pod (via Downward API), shared `src_cluster_id` (tunnel name). Requests targeting the cluster_id are load-balanced across all initiator pods. Phase 1 uses single replica; Phase 2 enables multiple.
5. ~~**WASM/Lua filters**~~: Lua. Upstream Envoy reverse tunnel docs use Lua for host_id extraction. Lua is built-in, proven, and sufficient for the two-tier routing pattern. WASM migration deferred unless Lua proves insufficient.

### Deferred (v2+)

6. ~~**Integration with cert-manager**~~: Implemented via `--cert-manager` flag on `portal generate`. Generates cert-manager CRDs (Issuer, Certificate) instead of raw Secrets.

---

## Task Tracker

Status legend: ✅ Done | 🔧 In Progress | ⬚ Not Started

### Phase 1: TCP Proxy Mode (v1)

| # | Task | Status | Notes |
|---|------|--------|-------|
| 1.1 | Project scaffolding (Go module, Cobra CLI, kubeconfig handling) | ✅ | `cmd/portal/main.go`, all commands registered |
| 1.2 | Certificate generation (CA + initiator/responder certs) | ✅ | `internal/certs/certs.go` — RSA 4096, per-tunnel CA, SAN support |
| 1.3 | Envoy bootstrap templating (TCP proxy mode) | ✅ | `internal/envoy/templates/initiator_tcp.yaml`, `responder_tcp.yaml` |
| 1.4 | `ManifestBundle` rendering pipeline | ✅ | `internal/manifest/render.go` — shared by generate and future connect |
| 1.5 | `portal generate` | ✅ | Fully implemented with all flags. 7 unit tests passing |
| 1.6 | `portal connect` — apply manifests to clusters, wait for LB IP, verify | ⬚ | Stub exists (`"not yet implemented"`). Requires `internal/kube/` client factory and deploy logic |
| 1.7 | `portal disconnect` — tear down both sides, clean up secrets | ⬚ | Stub exists. Depends on state management (1.6a) to know what to remove |
| 1.8 | `portal status` — query Envoy admin API via port-forward | ⬚ | Stub exists. Needs kube client + port-forward + stats parsing |
| 1.9 | `portal expose` / `portal generate expose` — add listener/cluster pair | ⬚ | Stub exists. Requires updating Envoy bootstrap ConfigMap and creating per-service Services |
| 1.10 | `portal list` — list tunnels from local state | ⬚ | Stub exists. Depends on state management (1.6a) |
| 1.11 | E2E tests with KIND multi-cluster setup | ⬚ | Demo scripts exist (`docs/demo/`) but no automated test harness |

#### Phase 1 sub-tasks (not in original plan, discovered during implementation)

| # | Task | Status | Notes |
|---|------|--------|-------|
| 1.5a | cert-manager integration for `portal generate` | ✅ | `--cert-manager` flag generates Issuer + Certificate CRDs instead of raw Secrets |
| 1.5b | Certificate rotation (`portal rotate-certs`) | ✅ | `internal/manifest/rotate.go` — re-issues leaf certs from existing CA. 2 unit tests passing |
| 1.5c | NetworkPolicy generation | ✅ | Generated for both initiator and responder |
| 1.6a | Local state management (`~/.portal/tunnels.json`) | ⬚ | `internal/state/` does not exist. Required by connect, disconnect, status, list |
| 1.6b | Kubernetes client factory (`internal/kube/`) | ⬚ | Multi-context kube client for apply/delete. Required by connect, disconnect, status |
| 1.6c | `--dry-run` flag for `portal connect` | ⬚ | Print manifests to stdout without applying |
| 1.9a | `portal generate expose` — generate service manifest to disk | ⬚ | Separate subcommand for GitOps service exposure |
| 1.11a | Demo documentation (`docs/demo/`) | ✅ | README, demo.sh, cleanup.sh, echo-sidecar-patch.yaml |

### Phase 2: Reverse Connection Mode (v2)

| # | Task | Status | Notes |
|---|------|--------|-------|
| 2.1 | Validate Envoy reverse_connection extension support (v1.28+) | ⬚ | |
| 2.2 | Reverse connection bootstrap templates (initiator + responder) | ⬚ | Template structure designed in PRD (`initiator_rc.yaml`, `responder_rc.yaml`) |
| 2.3 | Header-based service routing through the tunnel | ⬚ | Lua filter for `x-computed-host-id` extraction (designed in PRD) |
| 2.4 | `portal expose` updates for v2 (header injection, single-port model) | ⬚ | |
| 2.5 | CDS/xDS integration for dynamic service exposure | ⬚ | Avoids pod restart on `portal expose` |
| 2.6 | Connection health monitoring and automatic reconnection | ⬚ | |
| 2.7 | Migration path from v1 (TCP proxy) to v2 (reverse connection) | ⬚ | |

### Phase 3: Operational Maturity

| # | Task | Status | Notes |
|---|------|--------|-------|
| 3.1 | `portal rotate-certs` — zero-downtime certificate rotation | ✅ | Implemented. Currently requires manual pod restart; zero-downtime (SDS/hot-restart) is future work |
| 3.2 | `portal logs` — stream Envoy access logs from both sides | ⬚ | |
| 3.3 | Prometheus metrics endpoint on each Envoy | ⬚ | Envoy admin already exposes `/stats`; needs Prometheus scrape annotations |
| 3.4 | `portal doctor` — diagnose connectivity issues | ⬚ | Firewall, DNS, cert expiry checks |
| 3.5 | Helm chart for declarative tunnel management | ⬚ | |
| 3.6 | Sealed Secrets / SOPS / External Secrets integration | ⬚ | `--secret-format raw\|sealed-secret` flags (designed in PRD, not implemented) |

### Summary

| Phase | Total | Done | Remaining |
|-------|-------|------|-----------|
| Phase 1 (TCP Proxy) | 19 | 8 | 11 |
| Phase 2 (Reverse Connection) | 7 | 0 | 7 |
| Phase 3 (Operational Maturity) | 6 | 1 | 5 |
| **Total** | **32** | **9** | **23** |
