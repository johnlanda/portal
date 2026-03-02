# Portal Security Model

This document describes the threat model, trust boundaries, and security
architecture of Portal. It is intended for operators deploying Portal tunnels
and security teams evaluating the tool for production use.

Portal creates mTLS-encrypted tunnels between Kubernetes clusters using Envoy
Proxy. Its security posture inherits from Envoy's own
[threat model](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/security/threat_model.html),
with additional constraints specific to the tunnel topology.

## Trust Boundaries

Portal operates across three trust boundaries:

```
 Operator Workstation          Source Cluster              Destination Cluster
+---------------------+    +--------------------+      +----------------------+
| portal CLI           |    | portal-initiator   |      | portal-responder     |
| ~/.portal/           |    | (Envoy)            |      | (Envoy)              |
| CA key material      |    | Client cert + key  |      | Server cert + key    |
+---------------------+    +-------|------------+      +---------|------------+
                                    |      mTLS / TLS 1.3       |
                                    +-------------------------->+
                                      Per-tunnel CA trust root
```

### Boundary 1: Operator workstation to clusters

The portal CLI uses the operator's kubeconfig and kubectl to apply manifests.
Trust is delegated to Kubernetes RBAC -- Portal requires permissions to create
Namespaces, Deployments, Services, ConfigMaps, Secrets, ServiceAccounts, and
NetworkPolicies in the tunnel namespace.

**Threat**: A compromised workstation with access to the CA private key can
issue arbitrary client certificates and impersonate the initiator.

**Mitigation**: CA material is stored under `~/.portal/certs/` with 0600
permissions. For GitOps workflows, the `ca/` directory includes a `.gitignore`
to prevent accidental commits. Operators should store CA keys in a secrets
manager for production deployments.

### Boundary 2: Source cluster (initiator)

The initiator Envoy presents a client certificate to prove its identity and
validates the responder's server certificate against the tunnel CA.

**Threat**: An attacker with access to the source cluster's `portal-tunnel-tls`
Secret can extract the initiator's client certificate and key.

**Mitigation**: The Secret is scoped to the `portal-system` namespace.
Kubernetes RBAC should restrict Secret read access. NetworkPolicies limit
egress to DNS and the responder port only.

### Boundary 3: Destination cluster (responder)

The responder Envoy terminates TLS, requires a valid client certificate signed
by the tunnel CA, and forwards traffic to a local backend service.

**Threat**: An attacker who compromises the responder pod gains access to the
server certificate and can potentially intercept tunnel traffic.

**Mitigation**: The responder pod runs with a hardened security context (see
[Container Hardening](#container-hardening)). NetworkPolicies restrict ingress
to the tunnel port and egress to DNS and in-cluster services.

## Certificate Architecture

### Per-Tunnel PKI

Each tunnel has its own independent certificate authority:

```
Portal CA (RSA 4096, self-signed)
|
+-- Initiator Client Certificate
|   CN: portal-initiator/<tunnel-name>
|   Extended Key Usage: Client Authentication
|
+-- Responder Server Certificate
    CN: portal-responder/<tunnel-name>
    Extended Key Usage: Server Authentication
    SANs: <responder IP or DNS hostname>
```

This isolation means:

- **No cross-tunnel trust**: Compromise of one tunnel's CA does not grant
  access to any other tunnel.
- **No shared root**: There is no global CA that, if compromised, would
  affect all tunnels.
- **Simple rotation**: Leaf certificates can be rotated independently using
  `portal rotate-certs` without regenerating the CA.

### TLS Configuration

Both the initiator and responder enforce strict TLS settings:

| Setting | Value |
|---------|-------|
| Minimum TLS version | TLSv1.3 |
| Key algorithm | RSA 4096 |
| Client certificate required | Yes (on responder) |
| ALPN | h2 (HTTP/2 for multiplexing) |
| SNI validation | Initiator validates responder hostname via SNI |
| Certificate chain validation | Both sides validate against the tunnel CA |

### Certificate Lifecycle

| Event | What Happens |
|-------|-------------|
| `portal connect` / `portal generate` | CA + leaf certs generated, stored in Secrets and locally |
| `portal rotate-certs` | New leaf certs issued from existing CA; Secrets overwritten |
| Leaf certificate expiry | Manual rotation required (or automatic via cert-manager) |
| CA certificate expiry | Full tunnel regeneration required |
| `portal disconnect` | Secrets deleted from both clusters; local CA material removed |

**Default validity**: 1 year (configurable via `--cert-validity`).

**cert-manager mode**: When `--cert-manager` is used, cert-manager handles
automatic renewal. The `renewBefore` is set to one-third of the certificate
validity period.

In cert-manager mode, the certificate lifecycle differs from built-in PKI in
several important ways:

- **CRDs replace raw Secrets**: Portal generates cert-manager Issuer and
  Certificate resources instead of Kubernetes Secrets with embedded PEM data.
  cert-manager provisions and rotates the Secrets automatically.
- **CA key in-cluster**: The CA private key lives in a Kubernetes Secret
  (`portal-tunnel-ca`) managed by cert-manager, rather than on the operator's
  workstation. Key protection shifts to Kubernetes RBAC -- restrict Secret read
  access in the tunnel namespace accordingly.
- **`portal rotate-certs` is blocked**: cert-manager owns the renewal lifecycle.
  Manual rotation via the CLI is not permitted.
- **Monitoring**: Operators should monitor the `Ready` condition on Certificate
  resources (`kubectl get certificate -n portal-system`) and alert on
  `Ready=False` states, which indicate renewal failures.

### Certificate Storage

| Location | Contents | Permissions |
|----------|----------|-------------|
| `~/.portal/certs/<tunnel>/ca.crt` | CA public certificate | 0600 |
| `~/.portal/certs/<tunnel>/ca.key` | CA private key | 0600 |
| `<output-dir>/ca/` | CA material (GitOps mode) | 0700 dir, git-ignored |
| `portal-tunnel-tls` K8s Secret | Leaf cert + key + CA cert | Namespace-scoped |

## Network Security

### NetworkPolicies

Portal generates NetworkPolicies for both the initiator and responder pods.

**Initiator NetworkPolicy**:
- Ingress: Allow TCP on tunnel port from any pod in the namespace
- Egress: Allow DNS (UDP 53) and TCP to the tunnel port (responder)
- All other traffic is denied

**Responder NetworkPolicy**:
- Ingress: Allow TCP on tunnel port from any source
- Egress: Allow DNS (UDP 53) and any in-cluster destination (for backend
  forwarding)
- All other traffic is denied

### Exposed Attack Surface

| Component | Listening Port | Exposed To |
|-----------|---------------|------------|
| Responder Envoy | 10443 (tunnel) | External (via LoadBalancer/NodePort) |
| Responder Envoy | 15001 (admin) | Pod-local only (127.0.0.1) |
| Initiator Envoy | 10443 (tunnel) | In-cluster only (ClusterIP) |
| Initiator Envoy | 15000 (admin) | Pod-local only (127.0.0.1) |

The only externally reachable port is the responder's tunnel listener, which
requires a valid mTLS client certificate to complete the TLS handshake.

The Envoy admin interface is bound to localhost and is not reachable from
outside the pod. Portal's `status` command accesses it via `kubectl
port-forward`, which requires Kubernetes RBAC authorization.

## Container Hardening

All Portal-managed Envoy pods run with the following security context:

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
```

Additionally:

- **No service account token**: `automountServiceAccountToken: false` prevents
  the pod from accessing the Kubernetes API.
- **Read-only filesystem**: The root filesystem is read-only. Envoy writes only
  to its pre-configured paths.
- **Resource limits**: CPU and memory limits are set to prevent resource
  exhaustion (100m/500m CPU, 128Mi/256Mi memory).
- **Pinned image**: The Envoy image is pinned by digest to prevent supply chain
  attacks from tag mutation.

## Threat Analysis

### Threats Mitigated

| Threat | Mitigation |
|--------|-----------|
| Eavesdropping on tunnel traffic | mTLS with TLS 1.3; all tunnel traffic is encrypted |
| Unauthorized tunnel access | Client certificate required; only certs signed by the tunnel CA are accepted |
| Cross-tunnel lateral movement | Per-tunnel CA isolation; compromise of one CA does not affect others |
| Man-in-the-middle | Initiator validates responder certificate against the tunnel CA; SNI verification |
| Privilege escalation in pod | Non-root, read-only filesystem, all capabilities dropped, seccomp enforced |
| Kubernetes API abuse from pod | Service account token not mounted |
| Network-level lateral movement | NetworkPolicies restrict ingress/egress to tunnel and DNS traffic |
| Supply chain (image tampering) | Envoy image pinned by digest |
| Accidental CA key commit | `ca/` directory contains `.gitignore`; CA files written with 0600 |

### Threats Not Mitigated (Operator Responsibility)

| Threat | Required Action |
|--------|----------------|
| Kubernetes RBAC misconfiguration | Restrict Secret read access in the tunnel namespace to authorized operators |
| CA key compromise on operator workstation | Store CA keys in a secrets manager; rotate tunnels if compromised |
| Expired certificates | Monitor certificate expiry; use cert-manager mode or schedule `rotate-certs` |
| Envoy CVEs | Keep the Envoy image updated; rebuild tunnel manifests with new `--envoy-image` |
| DoS on the responder LoadBalancer | Configure cloud provider DDoS protection and rate limiting on the LB |
| Compromised cluster control plane | Portal trusts the Kubernetes API; a compromised control plane can read Secrets |
| Admin API exposure | Envoy admin binds to 127.0.0.1 by default; do not change this to 0.0.0.0 |
| cert-manager unavailability | In cert-manager mode, certificates may fail to renew if cert-manager is down or misconfigured; monitor Certificate `Ready` condition |

### Alignment with Envoy's Threat Model

Portal uses Envoy components classified as
"[robust to untrusted downstream and upstream](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/security/threat_model.html)"
in Envoy's security posture:

- **TCP proxy filter** (`envoy.filters.network.tcp_proxy`): Hardened against
  adversarial traffic.
- **TLS transport socket**: Core TLS implementation, fully hardened.
- **Round-robin load balancer**: Used for upstream cluster routing.

Portal does not use extensions classified as requiring trusted downstream or
upstream (e.g., Lua, WASM, Redis proxy).

As noted in Envoy's threat model, default settings are not safe from an
availability perspective. Portal addresses this by generating NetworkPolicies
and resource limits, but operators should additionally configure:

- Connection limits on the responder listener
- Overload manager settings for high-traffic deployments
- Circuit breakers on upstream clusters

## Operational Recommendations

### Certificate Rotation

Rotate leaf certificates before they expire. The default validity is 1 year.

```bash
# Rotate and apply
portal rotate-certs ./tunnel-dir
kubectl apply -f ./tunnel-dir/destination/portal-tunnel-tls-secret.yaml --context dest
kubectl apply -f ./tunnel-dir/source/portal-tunnel-tls-secret.yaml --context source
kubectl rollout restart deployment/portal-responder -n portal-system --context dest
kubectl rollout restart deployment/portal-initiator -n portal-system --context source
```

For zero-touch rotation, use `--cert-manager` mode.

### Monitoring

Use `portal status` to check tunnel health. Key indicators:

- `upstream_cx_connect_fail > 0`: Initiator cannot reach responder
- `ssl.handshake > 0`: TLS handshakes are completing
- `upstream_cx_active > 0`: Active connections through the tunnel
- Pod restarts increasing: Check Envoy logs for certificate or config errors

### Namespace Isolation

Deploy each tunnel in its own namespace to limit the blast radius of a
compromised Secret. The default `portal-system` namespace is suitable for
single-tunnel deployments; use `--namespace` for multi-tunnel setups.

### CA Key Protection

For production deployments:

1. Generate the tunnel with `portal generate`
2. Move `ca/ca.key` to a secrets manager (Vault, AWS Secrets Manager, etc.)
3. Delete the local copy
4. For rotation, retrieve the key temporarily, run `portal rotate-certs`, and
   re-secure it

Alternatively, use `--cert-manager` to avoid managing CA keys entirely.

## Reporting Vulnerabilities

If you discover a security vulnerability in Portal, please report it
responsibly. Do not open a public issue. Instead, contact the maintainers
directly via the repository's security advisory process.
