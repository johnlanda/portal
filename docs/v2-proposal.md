# Portal v2 Proposal: True Reverse Tunnels

Status: **Draft for review**
Author: Portal maintainers
Date: 2026-07-14

## Summary

Portal v1 is a forward mTLS tunnel with SNI-based multiplexing: the initiator
dials the responder, and traffic flows only in the direction the connection was
dialed. Whichever side hosts the services must accept ingress. This does not
serve Portal's core use case — a data plane cluster with **egress only**, whose
services must be reachable from a management plane that allows ingress and
egress.

v2 adopts Envoy's native reverse tunnel machinery (available in the
`envoyproxy/envoy:v1.37` image Portal already pins) to add genuine connection
reversal: the egress-only cluster dials out and holds persistent connections;
the management plane sends requests *into* it over those connections. v2 also
replaces the two-context `connect` model with a two-party **hub/member** model,
binds identity to certificates instead of headers, and reshapes the Go library
API for embedding in hosted products.

## Background

### What v1 actually is

- `portal connect <source_ctx> <dest_ctx>` deploys an initiator Envoy
  (source) and a responder Envoy (destination, behind a LoadBalancer).
- The initiator dials the responder over mTLS (TLS 1.3). Multiple services are
  multiplexed by SNI; the responder routes via `tls_inspector` passthrough
  without terminating TLS.
- Streams flow only initiator → responder. The `--connection-count` flag
  ("reverse connections") is parsed but unused. `expose` in the reverse
  direction is explicitly stubbed as "Phase 2" (`internal/cli/expose.go`).

### The topology v2 must serve

```
Hosted management plane            Customer environment
(ingress + egress allowed)         (egress ONLY)
┌──────────────────────┐           ┌──────────────────────┐
│  mgmt services   ────┼──────►────┼──►  data plane svcs   │   ← the new capability
│  tunnel endpoint ◄───┼───dials───┼───  agent / worker    │   ← connection direction
└──────────────────────┘           └──────────────────────┘
```

The customer side must never require an inbound port, yet the management plane
must be able to originate requests to specific data planes.

### Envoy primitives used

All are in the default extension build of the pinned image (experimental,
under active upstream development):

| Component | Side | Purpose |
|---|---|---|
| `envoy.bootstrap.reverse_tunnel.downstream_socket_interface` | member | Dials out, maintains N persistent connections |
| `rc://node:cluster:tenant@remote-cluster:N` listener address | member | Declares identity + connection count; routes declare which local services the hub may reach |
| `envoy.bootstrap.reverse_tunnel.upstream_socket_interface` | hub | Accepts and caches reverse connections |
| `envoy.filters.network.reverse_tunnel` | hub | Validates handshakes; emits `accepted` / `rejected` / `validation_failed` counters |
| `REVERSE_CONNECTION` cluster (`lb: CLUSTER_PROVIDED`, `host_id_format`) | hub | Routes hub-originated requests over cached tunnel sockets |

Key constraints, verified against upstream source:

- **Streams over the tunnel flow only hub → member.** The member side can only
  serve requests arriving over cached sockets, never originate on them
  (`reverse_connection_io_handle.h`). Member-originated traffic (polling,
  telemetry export) uses ordinary outbound connections — which the topology
  permits by definition.
- **HTTP/2 only** on the reverse path. Raw TCP cannot ride it.
- Handshake identity headers (`x-envoy-reverse-tunnel-node-id` etc.) are
  self-asserted unless validated; see [Identity and PKI](#identity-and-pki).

## Goals

1. Management-plane-originated requests into egress-only data planes, zero
   inbound ports on the member side.
2. A two-party CLI model: hub owner and member owner hold different
   credentials and never need each other's kubeconfig.
3. Cryptographic member identity — a member can only claim the identity its
   certificate proves.
4. A stateless library API suitable for embedding in a hosted product that
   distributes the member side via Helm.
5. Preserve v1's forward SNI tunneling as an optional capability of the same
   member deployment (one Envoy, both directions, one public hub port).

## Non-goals

- Raw TCP over the reverse path (HTTP/2-only upstream constraint; rejected at
  command time, not discovered at runtime).
- Any new service in the data path. The hub and member remain Envoy + certs.
- Vanity DNS domains for routed services (K8s Service names cannot contain
  dots; aliases are real Services: `<svc>-<member>.portal.svc.cluster.local`).
- A Portal-hosted control plane. Enrollment automation is the embedding
  product's job (see [Embedded / hosted product mapping](#embedded--hosted-product-mapping)).

## Design

### Mental model: hub, members, published services

- A **hub** is the ingress-capable side (one public listener, one CA).
- A **member** is an egress-only cluster that joins a hub by dialing out.
- A member **publishes** local services, allowlisting what the hub may reach.
- Direction is always *inferred* from where a service lives — never stated.

`tunnels.json` v2 stores hub/member records. Internal Envoy identifiers are
derived, never surfaced: `cluster-id` = member name, `node-id` = member name +
pod suffix (HA via multiple pods sharing a cluster-id), `tenant-id` = hub name.

### CLI

Hub owner:

```
portal hub init <hub-ctx> --public-addr tunnel.corp.example:10443
portal hub sign acme-prod.csr -o acme-prod.crt          # enrollment (see PKI)
portal hub invite acme-prod -o acme-prod.credential     # single-operator shortcut
portal route <hub-ctx> acme-prod/inference              # optional friendly alias Service
portal status [acme-prod]                               # live health, both directions
portal hub evict acme-prod                              # CRL re-render + hot reload
```

Member owner:

```
portal join <member-ctx> --hub tunnel.corp.example:10443   # phase 1: keygen in-cluster, emits CSR
portal join <member-ctx> --cert acme-prod.crt              # phase 2: complete
portal publish <member-ctx> svc/inference --port 8080 --protocol grpc
portal unpublish <member-ctx> svc/inference
portal leave <member-ctx>
```

`--protocol` (default `http`) is required knowledge at publish time: `tcp` on
the reverse path is rejected with an error explaining the HTTP/2 constraint
and pointing at the forward path or gRPC.

### Identity and PKI

- Per-member leaf certificates signed by the hub CA, **SAN = member name**.
- The hub's `reverse_tunnel` filter validates the handshake's cluster-id
  against the peer certificate SAN via format string (e.g.
  `%DOWNSTREAM_PEER_URI_SAN%`), so identity is proven, not asserted.
  Cross-member impersonation is closed; intra-member node-id spoofing is
  harmless because node-ids are scoped under the SAN-bound cluster-id.
- **Enrollment default: two-phase CSR, key born in-cluster.** Phase 1 of
  `join` creates the keypair as an in-cluster Secret and writes a CSR file;
  the hub owner signs it (`hub sign`); phase 2 installs the cert. The private
  key never exists outside the member cluster. `hub invite` (vendor-minted
  bundle) remains only as a documented single-operator shortcut.
- **Revocation:** `hub evict` re-renders a CRL and updates the mounted Secret.
  Envoy's `CertificateValidationContext.crl` installs a filesystem watch, so
  revocation hot-reloads with no responder restart and no fleet disconnect.
  Blast radius of both eviction and rotation is a single member.
- **Rotation** reuses the identical two-phase CSR flow (no second ceremony to
  learn). `status` surfaces cert-expiry countdowns hub-side. cert-manager and
  BYO-PKI (`--cert-manager`, `--secret-ref`) remain first-class and are the
  recommended path beyond a handful of members.

### Routing

- **The member's publish allowlist is the routing truth.** Publishing renders
  the member Envoy's reverse-tunnel listener routes; nothing on the hub needs
  per-service config to *route*.
- The hub egress listener routes wildcard authority `*.<member>` →
  `host_id = <member>`, forwarding the request with authority normalized to
  the canonical `<svc>.<member>` form. Members match routes against the
  canonical form only — member route configs must not depend on hub-side
  alias naming.
- `portal route` is **name minting, not routing**: it creates a ClusterIP
  Service `<svc>-<member>` in the `portal` namespace pointing at the egress
  listener, so consuming apps use plain K8s DNS and never learn member IDs.
  Without it, apps may address the egress listener directly with a
  `<svc>.<member>` authority.
- Half-states are first-class in `status`: a hub-side probe through the tunnel
  distinguishes `published, not routed`, `routed, not published` (member Envoy
  404), and `tunnel down` (connect failure).

### The shared hub listener

One public port carries both directions, split by `tls_inspector` filter
chain match: the reverse-tunnel handshake SNI terminates TLS into an HCM chain
running the `reverse_tunnel` filter; published forward-service SNIs remain
`tcp_proxy` passthrough chains (v1 mechanism, unchanged). Consequences owned:

- SNI namespace is partitioned (handshake SNI is reserved).
- Every rendered responder bootstrap is validated (`envoy --mode validate`)
  before apply; all hub-mutating commands support `--dry-run` diffs.
- The `tls_inspector` no-match counter and handshake reject counters appear in
  default `status` output — misdirected clients must be visible, not
  "connection reset on 10443".

### Status

```
MEMBER acme-prod                      [hub-side view]
  identity      SAN acme-prod, cert expires 2027-01-10 (hub CA ok)
  reverse       3/3 connections (pods: -a1x9, -b2k4, -c8m2), last handshake 4s ago
                handshake: 1,204 accepted / 0 rejected / 0 validation_failed (24h)
  published →   inference :8080 grpc   ROUTED       probe: 200 OK 12ms
                admin     :9000 http   NOT ROUTED
  forward       SNI chains: telemetry.acme-prod, api.hub
  listener      :10443 chain matches (24h): reverse 1,204 / passthrough 88,120 / no-match 3
```

All health is computed live from Envoy admin stats (port-forward);
`tunnels.json` is a registry of names only, never a source of health. Output
degrades honestly by which contexts the caller can reach (hub-side view vs
member-side view).

## Library API v2

The embedded scenario (a hosted product distributing the member side via
Helm) reshapes the library more than the CLI. Principles: **stateless**, **no
kubectl**, split cert operations from config rendering from manifest shapes.

```go
// package portal — v2 additions (v1 API retained, see Compatibility)

// Hub PKI. The caller (e.g. a management plane backend) owns CA storage.
func NewHubCA(hubName string, validity time.Duration) (*HubCA, error)
func (ca *HubCA) SignCSR(csrPEM []byte, member MemberIdentity, validity time.Duration) (certPEM []byte, err error)
func (ca *HubCA) RenderCRL(revoked []RevokedMember) (crlPEM []byte, err error)

type MemberIdentity struct {
    Member string // SAN + cluster-id (no ':' — reserved by Envoy tenant scoping)
    Tenant string // tenant-id for multi-tenant hubs
}

// Envoy bootstrap rendering — returns config content only (e.g. for a
// ConfigMap in a Helm chart). No Deployments, Services, or Secrets.
func RenderMemberBootstrap(cfg MemberConfig) ([]byte, error)   // rc:// listener, publish routes, forward listeners
func RenderHubBootstrap(cfg HubConfig) ([]byte, error)         // shared listener, reverse_tunnel filter, egress listener

// Member-side keygen for two-phase enrollment (key stays with the caller).
func GenerateMemberKeyAndCSR(id MemberIdentity) (keyPEM, csrPEM []byte, err error)
```

Package movement:

| Package | v2 change |
|---|---|
| `certs` | Grows hub CA, CSR signing, CRL rendering. Becomes the library core. |
| `envoy` | Two new template families (member reverse bootstrap, hub bootstrap). Exposed via `RenderMemberBootstrap` / `RenderHubBootstrap`. |
| `manifest` | Used by the standalone CLI only. Embedders bring their own manifest shapes (Helm). |
| `kube` | CLI-only. Never imported by the library surface. |
| `state` | CLI-only. The library is stateless; embedders persist member registries themselves. |

## Embedded / hosted product mapping

How the flow maps when a vendor hosts the hub and ships the member side as a
Helm chart (the CSR ceremony automates away because the vendor's backend is a
legitimate control plane — Portal itself still ships no server):

| CLI verb | Hosted product equivalent |
|---|---|
| `hub init` | Hub Envoy deployed with the management plane; CA in backend-managed storage (`NewHubCA`) |
| `hub invite` | "Add data plane" in the vendor UI → one-time enrollment token |
| `join` phase 1 | Helm init container: `GenerateMemberKeyAndCSR`, key as in-cluster Secret |
| `hub sign` | Backend enrollment endpoint: authenticate token, `SignCSR`, return cert |
| `join` phase 2 | Init container writes cert; SDS `watched_directory` hot-loads; tunnel dials |
| `publish` | Baked into the chart — the publish set is product-defined |
| `route` | Unnecessary — the consuming app is the vendor's own backend, which sets `<svc>.<member>` authority directly |
| `status` | Backend reads co-located hub Envoy admin stats; surfaces per-member health in UI |
| `hub evict` | Deactivate in UI → `RenderCRL` → Secret update → hot reload |
| rotation | Agent re-enrolls via the same endpoint before expiry; zero humans |

Customer footprint:

```
helm install vendor-dataplane vendor/dataplane \
  --set tunnel.hub=tunnel.vendor.example:10443 \
  --set tunnel.enrollmentToken=<from UI>
```

## Compatibility and migration

**Keep the entry points, kill the model.** There is exactly one data model
(hub/member) with compatibility front doors:

- `portal connect A B` becomes a wrapper executing `hub init B` + `invite` +
  `join A` for the single-operator case, storing hub/member state.
- `portal expose` becomes a deprecation-warning alias mapping to
  `publish`/`route` with direction inferred from which side the context is.
- `portal migrate` converts existing v1 `tunnels.json` records; one
  deprecation cycle before alias removal.
- The v1 library functions (`RenderTunnel`, `RenderTunnelWithServices`,
  `AddService`, `GenerateCertificates`) are retained and reimplemented over
  the v2 internals.
- `--connection-count` finally gains real semantics (the `:N` in the `rc://`
  address). Because it previously did nothing, this is called out loudly in
  release notes.

**Envoy version gate.** The reverse tunnel APIs are experimental upstream: the
`rc://` grammar, handshake headers, filter protos, and stat names carry no
stability promise. Each Portal release pins an exact Envoy image digest;
`render` and `status` **refuse** (not warn) on an unrecognized Envoy minor; a
supported-version matrix ships in the README.

## Risks

| Risk | Standing | Mitigation shipping in v2.0 |
|---|---|---|
| Experimental Envoy API churn breaks rendered manifests in customer GitOps repos | Accepted | Digest pinning + hard version gate + version matrix (above) |
| Serverless cert rotation is a recurring two-party ceremony (standalone CLI use) | Accepted | Expiry countdowns in `status`; rotation reuses the enrollment flow verbatim; cert-manager/BYO-PKI positioned as the recommended path beyond a handful of members |
| Shared hub listener is fleet-wide blast radius; failures collapse to "connection reset" | Accepted | Bootstrap validation before apply; `--dry-run` on hub-mutating commands; no-match + handshake reject counters in default `status` |
| HTTP/2-only reverse path surprises TCP users | Solved at UX layer | `--protocol` required at publish; `tcp` rejected at command time with explanation |
| Header-asserted identity | Solved | SAN-bound validation; per-member CA blast radius; CRL hot-reload eviction |

## Implementation phases

1. **Templates + rendering.** Member/hub bootstrap template families in
   `internal/envoy`; golden-file tests against the upstream example configs
   (`envoy/docs/root/_configs/reverse_connection/`).
2. **PKI.** Hub CA, SAN-bound leaves, CSR signing, CRL rendering + eviction in
   `internal/certs`; library exports.
3. **CLI.** `hub init|sign|invite|evict`, `join`, `publish`/`unpublish`,
   `route`, `leave`; `status` v2; state schema v2 + `migrate`.
4. **Compatibility.** `connect`/`expose` shims over the new model; deprecation
   messaging; version gate.
5. **E2E.** KIND + MetalLB: hub + two members, SAN spoofing rejected
   (`validation_failed` observed), eviction disconnects one member only,
   publish/route half-states distinguished by `status`, forward + reverse
   coexisting on one listener.
