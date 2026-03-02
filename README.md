# Portal

<p align="center">
  <img src="docs/portal.svg" alt="Portal logo" width="200">
</p>

Portal creates secure, multiplexed reverse tunnels between Kubernetes clusters
using [Envoy Proxy](https://www.envoyproxy.io/). It automates mTLS certificate
provisioning, Envoy deployment on both ends, and tunnel lifecycle management --
all without introducing new services into the data path.

## When Are Reverse Tunnels Useful?

Traditional service connectivity requires the target cluster to be reachable
from the caller. This breaks down in common real-world topologies:

- **Private clusters behind NAT/firewalls** -- Clusters in air-gapped
  environments, on-prem data centers, or edge locations cannot accept inbound
  connections from a management plane.
- **Multi-cloud and hybrid deployments** -- Connecting services across AWS,
  GCP, Azure, and on-prem without VPN peering or shared network fabric.
- **Zero-trust networking** -- Avoiding broad firewall rules by having the
  remote side initiate the connection outbound to a known endpoint.
- **Management plane connectivity** -- A centralized control plane (e.g.,
  Synapse) needs to configure and observe gateway deployments in clusters it
  cannot directly reach.

A reverse tunnel inverts the connection model: the remote (initiator) cluster
dials out to a publicly reachable endpoint on the destination (responder)
cluster. Once the mTLS tunnel is established, traffic flows bidirectionally
over the encrypted connection. No inbound firewall rules are required on the
initiator side.

## How It Works

```mermaid
flowchart LR
    subgraph source["Source Cluster"]
        app["App / Management Plane"]
        initiator["portal-initiator\n(Envoy, TCP proxy)"]
        app -- "port 10443" --> initiator
    end

    subgraph dest["Destination Cluster"]
        responder["portal-responder\n(Envoy, TCP proxy)\nrequire_client_cert: true"]
        backend["Backend Service"]
        responder -- "port 8080" --> backend
    end

    initiator -- "mTLS / HTTP/2" --> responder

    style source fill:#f0f4ff,stroke:#4a7fff,color:#000
    style dest fill:#fff4f0,stroke:#e17b31,color:#000
    style initiator fill:#2ba8d3,stroke:#1a7a9e,color:#fff
    style responder fill:#e17b31,stroke:#b35e1f,color:#fff
    style app fill:#fff,stroke:#999,color:#000
    style backend fill:#fff,stroke:#999,color:#000
```

- **Initiator** (source cluster): Envoy configured as a TCP proxy that dials
  the responder over mTLS with HTTP/2 multiplexing.
- **Responder** (destination cluster): Envoy listening behind a LoadBalancer
  (or NodePort/ClusterIP) that terminates mTLS, validates the client
  certificate, and forwards traffic to a local backend service.
- **Per-tunnel CA**: Each tunnel gets its own self-signed CA. Compromise of one
  tunnel's certificates does not affect any other tunnel.
- **TLS 1.3 enforced**: Both sides require TLS 1.3 as the minimum protocol
  version.

### Multi-Service Tunnels

A single tunnel can multiplex multiple services using SNI-based routing. Each
service gets its own listener port on the initiator and its own filter chain on
the responder:

```mermaid
flowchart LR
    subgraph source["Source Cluster"]
        app1["App (port 8443)"]
        app2["App (port 4317)"]
        initiator["portal-initiator\n(Envoy)\nlistener:8443 → SNI:backend\nlistener:4317 → SNI:otel"]
        app1 --> initiator
        app2 --> initiator
    end

    subgraph dest["Destination Cluster"]
        responder["portal-responder\n(Envoy, tls_inspector)\nSNI:backend → backend-svc:8443\nSNI:otel → otel-collector:4317"]
        svc1["backend-svc:8443"]
        svc2["otel-collector:4317"]
        responder --> svc1
        responder --> svc2
    end

    initiator -- "mTLS / HTTP/2\nSNI routing" --> responder

    style source fill:#f0f4ff,stroke:#4a7fff,color:#000
    style dest fill:#fff4f0,stroke:#e17b31,color:#000
    style initiator fill:#2ba8d3,stroke:#1a7a9e,color:#fff
    style responder fill:#e17b31,stroke:#b35e1f,color:#fff
    style app1 fill:#fff,stroke:#999,color:#000
    style app2 fill:#fff,stroke:#999,color:#000
    style svc1 fill:#fff,stroke:#999,color:#000
    style svc2 fill:#fff,stroke:#999,color:#000
```

The responder uses Envoy's `tls_inspector` listener filter to read the SNI from
the TLS ClientHello without terminating TLS, then routes to the correct backend
via `filter_chain_match.server_names`. Each service gets a dedicated Envoy
cluster on the responder side.

## Installation

### From Source

Requires Go 1.23+.

```bash
git clone https://github.com/tetratelabs/portal.git
cd portal
go build -o portal ./cmd/portal
```

Set version at build time:

```bash
go build -ldflags="-X main.Version=v0.1.0" -o portal ./cmd/portal
```

### Prerequisites

- **kubectl** on `PATH`, configured with contexts for both clusters
- **Docker** (for the KIND-based demo)

## Quick Start

### Imperative (one command)

Deploy a tunnel between two clusters. Portal discovers the LoadBalancer IP,
generates certificates, and applies manifests to both sides:

```bash
portal connect kind-source kind-destination
```

Deploy a multi-service tunnel that routes multiple backends over a single
connection:

```bash
portal connect source-ctx dest-ctx \
  --service backend=backend-svc.ns.svc:8443 \
  --service otel=otel-collector.ns.svc:4317 \
  --service-local-port backend=18443
```

When you are done:

```bash
portal disconnect kind-source kind-destination
```

### Declarative (GitOps)

Generate manifests to disk for use with Kustomize, Argo CD, or Flux:

```bash
portal generate source-ctx destination-ctx \
  --responder-endpoint "34.120.1.50:10443" \
  --output-dir ./tunnel-manifests
```

Multi-service works with `generate` too:

```bash
portal generate source-ctx dest-ctx \
  --responder-endpoint "34.120.1.50:10443" \
  --output-dir ./tunnel-manifests \
  --service backend=backend-svc.ns.svc:8443 \
  --service otel=otel-collector.ns.svc:4317
```

Apply with kubectl or your GitOps controller:

```bash
kubectl apply -k ./tunnel-manifests/destination/ --context destination-ctx
kubectl apply -k ./tunnel-manifests/source/ --context source-ctx
```

## Commands

| Command | Description |
|---------|-------------|
| `portal connect` | Deploy a tunnel to both clusters imperatively |
| `portal disconnect` | Tear down a tunnel and clean up resources |
| `portal generate` | Generate manifests to disk for GitOps workflows |
| `portal generate expose` | Generate expose manifests to disk |
| `portal expose` | Expose a service through an existing tunnel |
| `portal status` | Show tunnel status and Envoy connection stats |
| `portal list` | List all known tunnels |
| `portal rotate-certs` | Rotate leaf TLS certificates using the existing CA |

### portal connect

```
portal connect <source_context> <destination_context> [flags]

Flags:
  --responder-endpoint   Responder address (IP:port or hostname:port); LB-discovered if omitted
  --namespace            Namespace for portal components (default: portal-system)
  --tunnel-port          Responder listen port (default: 10443)
  --cert-validity        Certificate validity duration (default: 8760h)
  --cert-manager         Use cert-manager CRDs instead of raw Secrets
  --cert-dir             Use existing certs from a shared directory
  --initiator-cert-dir   Use existing certs for initiator from this directory
  --responder-cert-dir   Use existing certs for responder from this directory
  --envoy-image          Envoy proxy image (default: envoyproxy/envoy:v1.37-latest, pinned by digest)
  --service-type         Responder Service type: LoadBalancer, NodePort, ClusterIP (default: LoadBalancer)
  --service              Service to route: sni=host:port (repeatable)
  --service-local-port   Override initiator listener port: sni=port (repeatable)
  --deploy-timeout       Timeout waiting for deployment readiness (default: 5m)
  --lb-timeout           Timeout waiting for LoadBalancer address (default: 5m)
  --dry-run              Print rendered manifests without applying
```

When `--responder-endpoint` is omitted, Portal uses a two-phase deployment:
deploy the responder first, wait for the LoadBalancer IP, then re-render and
deploy the initiator with the real endpoint.

### portal generate

```
portal generate <source_context> <destination_context> [flags]

Flags:
  --output-dir           Directory to write manifests (required)
  --responder-endpoint   Responder address (required)
  --service              Service to route: sni=host:port (repeatable)
  --service-local-port   Override initiator listener port: sni=port (repeatable)
  --initiator-cert-dir   Use existing certs for initiator from this directory
  --responder-cert-dir   Use existing certs for responder from this directory
  (plus all shared flags from connect except deploy-timeout, lb-timeout, dry-run)
```

Output structure:

```
<output-dir>/
+-- source/            # Apply to source cluster
|   +-- kustomization.yaml
|   +-- namespace.yaml
|   +-- portal-initiator-*.yaml
|   +-- portal-tunnel-tls-secret.yaml
+-- destination/       # Apply to destination cluster
|   +-- kustomization.yaml
|   +-- namespace.yaml
|   +-- portal-responder-*.yaml
|   +-- portal-tunnel-tls-secret.yaml
+-- tunnel.yaml        # Tunnel metadata
+-- ca/                # CA material (keep private, git-ignored)
    +-- ca.crt
    +-- ca.key
    +-- .gitignore
```

### portal expose

```
portal expose <context> <service> --port <port> [flags]

Flags:
  --port                 Port the service listens on (required)
  --local-port           Initiator listener port (default: same as --port)
  --sni                  Custom SNI value (default: service name)
  --service-namespace    Namespace of the service (default: default)
  --tunnel               Tunnel name (required if context matches multiple tunnels)
```

Creates a ClusterIP Service in the opposite cluster and updates both the
responder and initiator Envoy configs with the new service route. Expose is
additive -- calling it multiple times adds services to the existing tunnel
without disrupting already-routed services.

### portal rotate-certs

```
portal rotate-certs <tunnel-dir> [flags]

Flags:
  --cert-validity   New certificate validity (default: reuse from tunnel.yaml)
```

Re-issues leaf certificates from the existing CA. After rotation, re-apply the
updated secrets and restart the deployments:

```bash
portal rotate-certs ./tunnel-manifests
kubectl apply -f ./tunnel-manifests/destination/portal-tunnel-tls-secret.yaml --context dest-ctx
kubectl apply -f ./tunnel-manifests/source/portal-tunnel-tls-secret.yaml --context source-ctx
kubectl rollout restart deployment/portal-responder -n portal-system --context dest-ctx
kubectl rollout restart deployment/portal-initiator -n portal-system --context source-ctx
```

### portal status

```
portal status [<source_context> <destination_context>] [--json]
```

With no arguments, shows a summary of all tunnels. With two arguments, shows
detailed status including pod health, Envoy connection metrics (active
connections, bytes sent/received, TLS handshakes), and per-service health
derived from Envoy cluster stats:

```
Tunnel: dp1--mgmt (Connected)
  Services:
    backend (SNI: backend)  -> backend-svc:8443  healthy
    otel    (SNI: otel)     -> otel-collector:4317  healthy
```

### portal list

```
portal list [--json]
```

Lists all tunnels from `~/.portal/tunnels.json` in a table format.

## Certificate Management

Portal supports two certificate management modes. Before choosing one, it
helps to understand what Portal's TLS actually covers.

### TLS Scope

Portal secures the cross-cluster transport hop only. Traffic within each
cluster travels over plaintext TCP:

```
  Source Cluster                           Destination Cluster
  ┌─────────────────────────┐              ┌──────────────────────────┐
  │ App                     │              │ Target Service           │
  │   │                     │              │   ▲                      │
  │   │ hop 1: plaintext    │              │   │ hop 3: plaintext     │
  │   ▼                     │              │   │                      │
  │ portal-initiator (Envoy)│              │ portal-responder (Envoy) │
  │   │                     │              │   ▲                      │
  └───┼─────────────────────┘              └───┼──────────────────────┘
      │        hop 2: mTLS (TLS 1.3)           │
      └────────────────────────────────────────┘
```

- **Hop 1** (App to initiator): Plaintext TCP inside the source cluster.
- **Hop 2** (Initiator to responder): mTLS with TLS 1.3 -- this is what
  Portal secures.
- **Hop 3** (Responder to target service): Plaintext TCP inside the
  destination cluster.

If your services need end-to-end encryption, they should use their own TLS.
The encrypted payload passes through the tunnel transparently as TCP data.
Using cert-manager to secure your application services is orthogonal to
Portal's `--cert-manager` flag, which only manages the tunnel transport
certificates.

### Built-in PKI (default)

Portal generates a per-tunnel self-signed CA and issues leaf certificates for
the initiator (client auth) and responder (server auth). The CA material is
persisted locally for certificate rotation.

- **Key size**: RSA 4096
- **Default validity**: 1 year
- **TLS minimum**: TLSv1.3
- **Rotation**: `portal rotate-certs` re-issues leaves from the existing CA

### cert-manager Integration

With `--cert-manager`, Portal generates cert-manager Issuer and Certificate
CRDs instead of raw Kubernetes Secrets. cert-manager handles automatic renewal.

**Prerequisites**: cert-manager v1.0+ must be installed in both clusters
before using this mode.

**Usage**:

```bash
# Imperative
portal connect source dest --cert-manager

# GitOps
portal generate source dest \
  --responder-endpoint "34.120.1.50:10443" \
  --output-dir ./tunnel-manifests \
  --cert-manager
```

**Generated CRD chain**:

```
SelfSignedIssuer
  └── CA Certificate (isCA: true, duration: 3x leaf validity)
        └── CA Issuer (secretName: portal-tunnel-ca)
              ├── Initiator Certificate (usages: client auth)
              └── Responder Certificate (usages: server auth, SANs from endpoint)
```

All five CRDs are rendered into both the source and destination manifests so
that each cluster has a complete, self-contained cert-manager chain.

**Automatic renewal**: Leaf certificates set `renewBefore` to one-third of
the validity period and `privateKey.rotationPolicy: Always`, so cert-manager
rotates keys and reissues certificates automatically.

**`portal rotate-certs` is blocked** in cert-manager mode because cert-manager
owns the certificate lifecycle. Use `kubectl get certificate -n portal-system`
to monitor renewal status.

**No `ca/` directory** is created in cert-manager mode -- the CA private key
lives in a Kubernetes Secret (`portal-tunnel-ca`) managed by cert-manager.

**`--cert-validity` interaction**: Controls the leaf certificate duration. The
CA certificate duration is automatically set to 3x the leaf value.

### External Certificates

Bring your own certificates using split cert directories. Each directory must
contain `tls.crt`, `tls.key`, and `ca.crt`:

```bash
portal connect source dest \
  --initiator-cert-dir ./certs/initiator \
  --responder-cert-dir ./certs/responder
```

Or use a shared directory when both sides use the same cert material:

```bash
portal connect source dest --cert-dir ./certs/shared
```

When using the Go library API, certificates can be provided as PEM bytes via
the `ExternalCertificates` struct (see Go Library section below).

## Generated Kubernetes Resources

For each side of the tunnel, Portal generates:

| Resource | Purpose |
|----------|---------|
| Namespace | Isolated namespace (default: `portal-system`) |
| ServiceAccount | Dedicated SA, no auto-mounted token |
| ConfigMap | Envoy bootstrap configuration |
| Secret or Certificate | TLS certificates (raw Secret or cert-manager CRD) |
| Deployment | Envoy proxy pod with hardened security context |
| Service | Responder-side LoadBalancer/NodePort/ClusterIP |
| NetworkPolicy | Restricts ingress/egress to tunnel traffic and DNS |

### Container Security

All Envoy pods run with a hardened security context:

- `runAsNonRoot: true` (UID 1000)
- `readOnlyRootFilesystem: true`
- `allowPrivilegeEscalation: false`
- All capabilities dropped
- Seccomp profile: `RuntimeDefault`

## Demo

A full end-to-end demo using KIND clusters is available in
[docs/demo/README.md](docs/demo/README.md). It covers:

1. Creating two KIND clusters with MetalLB
2. Generating and deploying tunnel manifests
3. Sending HTTP traffic through the mTLS tunnel
4. Verifying connectivity via Envoy admin stats
5. Rotating certificates

Run the automated demo:

```bash
./docs/demo/demo.sh
```

Or follow the step-by-step guide for a deeper understanding.

## Local State

Portal stores tunnel metadata in `~/.portal/tunnels.json` for imperatively
created tunnels. This file tracks tunnel names, contexts, namespaces, ports,
and exposed services. CA certificates are stored in `~/.portal/certs/<tunnel>/`.

For GitOps workflows (`portal generate`), all state lives in the output
directory and the file can be committed to version control (except the `ca/`
directory, which is git-ignored by default).

## Go Library API

Portal can be used as a Go library for programmatic tunnel management. This is
useful when you need to embed tunnel provisioning into a larger system (e.g., a
Helm chart renderer, a Kubernetes operator, or a CI/CD pipeline).

```
go get github.com/tetratelabs/portal
```

### Functions

| Function | Description |
|----------|-------------|
| `RenderTunnel(cfg)` | Render manifests for a single-service tunnel |
| `RenderTunnelWithServices(cfg, services)` | Render manifests for a multi-service (SNI-routed) tunnel |
| `AddService(cfg, existing, newSvc)` | Re-render manifests with an additional service added to the list |
| `GenerateCertificates(name, SANs, validity)` | Generate mTLS PKI (CA + leaf certs) without rendering manifests |

All render functions return a `*ManifestBundle` containing YAML-encoded
Kubernetes resources ready to be written to disk or applied to clusters.

### Types

#### TunnelConfig

```go
type TunnelConfig struct {
    // Required: kube context names identify the tunnel endpoints.
    SourceContext      string
    DestinationContext string

    // Required: the address initiators dial to reach the responder.
    // Format: "host:port" or "ip:port".
    ResponderEndpoint  string

    // Optional (all have sensible defaults):
    TunnelName      string        // default: "<source>--<destination>"
    Namespace       string        // default: "portal-system"
    TunnelPort      int           // default: 10443
    ConnectionCount int           // default: 4
    CertValidity    time.Duration // default: 8760h (1 year)
    EnvoyImage      string        // default: envoyproxy/envoy:v1.37-latest (pinned by digest)
    EnvoyLogLevel   string        // default: "info"
    ServiceType     string        // default: "LoadBalancer" (also: "NodePort", "ClusterIP")
    CertManager     bool          // use cert-manager CRDs instead of raw Secrets

    // Multi-service routing (set by RenderTunnelWithServices / AddService):
    Services []ServiceConfig

    // External certificates — provide your own PEM-encoded certs instead
    // of having Portal auto-generate them:
    ExternalCerts *ExternalCertificates
}
```

#### ServiceConfig

```go
type ServiceConfig struct {
    SNI         string // SNI value for routing (also used as the service name)
    BackendHost string // Backend FQDN in the destination cluster
    BackendPort int    // Backend port
    LocalPort   int    // Initiator listener port (0 = use BackendPort)
}
```

#### ExternalCertificates

```go
type ExternalCertificates struct {
    CACert        []byte // PEM-encoded CA certificate
    InitiatorCert []byte // PEM-encoded initiator (client) certificate
    InitiatorKey  []byte // PEM-encoded initiator private key
    ResponderCert []byte // PEM-encoded responder (server) certificate
    ResponderKey  []byte // PEM-encoded responder private key
}
```

#### ManifestBundle

```go
type ManifestBundle struct {
    Source      []Resource             // K8s resources for the source (initiator) cluster
    Destination []Resource            // K8s resources for the destination (responder) cluster
    Certs       *TunnelCertificates   // Generated certs (nil when using external certs or cert-manager)
    Metadata    TunnelMetadata        // Tunnel metadata (name, contexts, services, timestamps)
}
```

Each `Resource` has a `Filename` (e.g., `portal-initiator-deployment.yaml`) and
`Content` (raw YAML bytes). This makes it straightforward to write to disk,
embed in a Helm chart, or pipe to `kubectl apply`.

#### TunnelCertificates

Returned by `GenerateCertificates` and available on `ManifestBundle.Certs`
when Portal auto-generates the PKI:

```go
type TunnelCertificates struct {
    CACert        []byte // PEM-encoded CA certificate
    CAKey         []byte // PEM-encoded CA private key
    InitiatorCert []byte // PEM-encoded initiator client certificate
    InitiatorKey  []byte // PEM-encoded initiator private key
    ResponderCert []byte // PEM-encoded responder server certificate
    ResponderKey  []byte // PEM-encoded responder private key
}
```

### Examples

#### Render a single-service tunnel and write to disk

```go
package main

import (
    "log"
    "os"
    "path/filepath"

    "github.com/tetratelabs/portal"
)

func main() {
    bundle, err := portal.RenderTunnel(portal.TunnelConfig{
        SourceContext:      "dp-cluster",
        DestinationContext: "mgmt-cluster",
        ResponderEndpoint:  "tunnel.example.com:10443",
    })
    if err != nil {
        log.Fatal(err)
    }

    // Write source (initiator) manifests.
    for _, r := range bundle.Source {
        if err := os.WriteFile(filepath.Join("out/source", r.Filename), r.Content, 0o644); err != nil {
            log.Fatal(err)
        }
    }

    // Write destination (responder) manifests.
    for _, r := range bundle.Destination {
        if err := os.WriteFile(filepath.Join("out/destination", r.Filename), r.Content, 0o644); err != nil {
            log.Fatal(err)
        }
    }

    // Persist the CA material for future certificate rotation.
    if bundle.Certs != nil {
        os.WriteFile("out/ca/ca.crt", bundle.Certs.CACert, 0o644)
        os.WriteFile("out/ca/ca.key", bundle.Certs.CAKey, 0o600)
    }
}
```

#### Multi-service tunnel with SNI routing

```go
bundle, err := portal.RenderTunnelWithServices(portal.TunnelConfig{
    SourceContext:      "dp-cluster",
    DestinationContext: "mgmt-cluster",
    ResponderEndpoint:  "tunnel.example.com:10443",
    Namespace:          "tunnels",          // override default namespace
    CertValidity:       30 * 24 * time.Hour, // 30-day certs
}, []portal.ServiceConfig{
    {SNI: "backend", BackendHost: "backend-svc.ns.svc", BackendPort: 8443},
    {SNI: "otel", BackendHost: "otel-collector.ns.svc", BackendPort: 4317},
    {SNI: "metrics", BackendHost: "prometheus.monitoring.svc", BackendPort: 9090, LocalPort: 19090},
})
```

Each service gets its own listener port on the initiator side. If `LocalPort`
is not set, it defaults to `BackendPort`.

#### Incrementally add a service

```go
existing := []portal.ServiceConfig{
    {SNI: "backend", BackendHost: "backend-svc.ns.svc", BackendPort: 8443},
}

bundle, err := portal.AddService(portal.TunnelConfig{
    SourceContext:      "dp-cluster",
    DestinationContext: "mgmt-cluster",
    ResponderEndpoint:  "tunnel.example.com:10443",
}, existing, portal.ServiceConfig{
    SNI: "otel", BackendHost: "otel-collector.ns.svc", BackendPort: 4317,
})
```

The returned bundle includes manifests for **all** services (existing + new).
Re-apply the full bundle to update both Envoy configs.

#### Bring your own certificates

```go
bundle, err := portal.RenderTunnel(portal.TunnelConfig{
    SourceContext:      "dp-cluster",
    DestinationContext: "mgmt-cluster",
    ResponderEndpoint:  "tunnel.example.com:10443",
    ExternalCerts: &portal.ExternalCertificates{
        CACert:        caCertPEM,
        InitiatorCert: initCertPEM,
        InitiatorKey:  initKeyPEM,
        ResponderCert: respCertPEM,
        ResponderKey:  respKeyPEM,
    },
})
// bundle.Certs will be nil — no PKI was generated.
```

#### Generate certificates independently

Use `GenerateCertificates` when you need the PKI material but want to handle
manifest rendering yourself:

```go
certs, err := portal.GenerateCertificates(
    "my-tunnel",
    []string{"tunnel.example.com", "10.0.0.50"}, // responder SANs
    365 * 24 * time.Hour,
)
if err != nil {
    log.Fatal(err)
}

// certs.CACert, certs.CAKey — self-signed CA
// certs.InitiatorCert, certs.InitiatorKey — client auth leaf
// certs.ResponderCert, certs.ResponderKey — server auth leaf (with SANs)
```

#### Use with cert-manager

```go
bundle, err := portal.RenderTunnel(portal.TunnelConfig{
    SourceContext:      "dp-cluster",
    DestinationContext: "mgmt-cluster",
    ResponderEndpoint:  "tunnel.example.com:10443",
    CertManager:        true,
})
// bundle.Certs will be nil — cert-manager handles the PKI.
// The manifests include Issuer and Certificate CRDs instead of Secrets.
```

## Project Layout

```
portal.go              Go library API (RenderTunnel, RenderTunnelWithServices, AddService, GenerateCertificates)
cmd/portal/            CLI entrypoint
internal/
  cli/                 Command implementations (connect, disconnect, expose, etc.)
  manifest/            Kubernetes manifest rendering and disk I/O
  envoy/               Envoy bootstrap configuration templates
    templates/         Single-service + multi-service (SNI-routing) Envoy YAML templates
  certs/               mTLS certificate generation and rotation
  kube/                Kubernetes client abstraction (kubectl wrapper)
  state/               Local tunnel state persistence (~/.portal/tunnels.json)
docs/
  demo/                Demo walkthrough, scripts, and patches
  PRD.md               Product requirements document
```

## Security

See [SECURITY.md](SECURITY.md) for the threat model, trust boundaries,
certificate lifecycle details, and operational security recommendations.
