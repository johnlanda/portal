# Portal — Agent Reference

## Project Context

Portal (`github.com/tetratelabs/portal`) is a Go CLI that creates secure Envoy reverse tunnels between Kubernetes clusters. This document covers all packages, commands, testing, and change workflows.

## Important Commands

| Command | Description |
|---------|-------------|
| `go build -o portal ./cmd/portal` | Build the CLI binary |
| `go build -ldflags="-X main.Version=v0.1.0" -o portal ./cmd/portal` | Build with version stamp |
| `go test ./...` | Run all unit tests |
| `go test -v ./internal/cli/` | Run CLI package tests (verbose) |
| `go test -v ./internal/manifest/` | Run manifest package tests |
| `go test -v ./internal/certs/` | Run certs package tests |
| `go test -v ./internal/kube/` | Run kube package tests |
| `go test -v ./internal/envoy/` | Run envoy package tests |
| `go test -v ./internal/state/` | Run state package tests |
| `go test -race ./...` | Run tests with race detector |
| `go test -cover ./...` | Run tests with coverage |
| `go test -tags e2e -v ./test/e2e/` | Run E2E tests (requires KIND clusters) |
| `go vet ./...` | Run static analysis |
| `gofmt -d .` | Check formatting |
| `golangci-lint run ./...` | Run all linters (errcheck, govet, godoclint, wrapcheck, thelper, gofmt) |

## Coding and Testing Standards

- **Style:** Effective Go; `gofmt` enforced
- **Doc comments:** All exported types, functions, and package declarations require godoc comments
- **Error handling:** Wrap errors with `fmt.Errorf("context: %w", err)`; never discard errors silently
- **Testing pattern:** Table-driven tests with `t.Run` subtests; test files alongside source as `*_test.go`
- **Testability hooks:** Package-level `var` functions that tests swap and restore via `t.Cleanup`:
  ```go
  var newKubeClient = func(ctx, ns string) kube.Client { return kube.NewClient(ctx, ns) }
  // In tests:
  orig := newKubeClient
  t.Cleanup(func() { newKubeClient = orig })
  newKubeClient = func(ctx, ns string) kube.Client { return &fakeClient{} }
  ```
- **Interfaces for testing:** `kube.Client` (K8s operations) and `kube.CommandRunner` (subprocess execution) enable full unit testing without a live cluster
- **No k8s.io imports:** The `kube` package wraps `kubectl` to leverage the user's kubeconfig, auth plugins, and context settings with zero additional dependencies
- **Build tags:** E2E tests use `//go:build e2e` to avoid running during `go test ./...`

## Local Development

### Prerequisites

- Go 1.23+
- `kubectl` on PATH
- [KIND](https://kind.sigs.k8s.io/) (for E2E tests)
- [MetalLB](https://metallb.universe.tf/) v0.14.9 (auto-installed by E2E suite)

### E2E Workflow

The E2E suite (`test/e2e/`) manages its own infrastructure:

1. `TestMain` checks prerequisites (`kind`, `kubectl`, `docker`)
2. Builds the portal binary from source
3. Creates two KIND clusters: `portal-e2e-source` and `portal-e2e-destination`
4. Installs MetalLB on the destination cluster for LoadBalancer support
5. Runs tests: connect, expose, status, security, stability, certs, tunnel
6. Tears down clusters on completion

```bash
# Run the full E2E suite
go test -tags e2e -v -timeout 30m ./test/e2e/

# Run a single E2E test
go test -tags e2e -v -run TestConnect ./test/e2e/
```

### Manual Testing with KIND

```bash
# Create clusters
kind create cluster --name portal-source
kind create cluster --name portal-dest

# Build and use
go build -o portal ./cmd/portal
./portal connect kind-portal-source kind-portal-dest

# Inspect
./portal status kind-portal-source kind-portal-dest
./portal list

# Clean up
./portal disconnect kind-portal-source kind-portal-dest
kind delete cluster --name portal-source
kind delete cluster --name portal-dest
```

## Scope

This document covers all packages in the repository:
- `cmd/portal` — CLI entrypoint
- `internal/certs` — Certificate generation and rotation
- `internal/cli` — Command implementations
- `internal/envoy` — Envoy bootstrap configuration
- `internal/kube` — Kubernetes client abstraction
- `internal/manifest` — Manifest rendering and disk writer
- `internal/state` — Tunnel state persistence
- `test/e2e` — End-to-end test suite

## File Index

### `cmd/portal/`

| File | Lines | Description |
|------|-------|-------------|
| `main.go` | 37 | Entrypoint; assembles root `cobra.Command`, registers subcommands, injects version via `-ldflags` |

### `internal/certs/`

| File | Lines | Description |
|------|-------|-------------|
| `certs.go` | 300 | Per-tunnel CA generation (RSA 4096), client/server leaf cert issuance, PEM encoding, cert validation |
| `certs_test.go` | 420 | Certificate chain validation, expiry, key usage, SAN, rotation scenarios |

### `internal/cli/`

| File | Lines | Description |
|------|-------|-------------|
| `connect.go` | 316 | `portal connect` — two-phase deploy: render manifests, apply responder, discover LB, re-render initiator, apply, save state |
| `connect_test.go` | 594 | Connect command tests: flag validation, dry-run, two-phase render, cert-dir, error paths |
| `disconnect.go` | 179 | `portal disconnect` — delete manifests from both clusters, remove state entry |
| `disconnect_test.go` | 353 | Disconnect tests: cleanup verification, missing tunnel, namespace override |
| `expose.go` | 296 | `portal expose` — add ClusterIP Service + Envoy route for a service through an existing tunnel |
| `expose_test.go` | 583 | Expose tests: destination/source direction, tunnel selection, config update |
| `format.go` | 36 | Shared output formatting helpers |
| `generate.go` | 256 | `portal generate` — render manifests to disk for GitOps (Kustomize/ArgoCD/Flux) |
| `generate_test.go` | 648 | Generate tests: output structure, cert-manager mode, custom flags, existing cert-dir |
| `list.go` | 64 | `portal list` — display all known tunnels from state file |
| `list_test.go` | 174 | List tests: JSON output, empty state, multiple tunnels |
| `rotate_certs.go` | 56 | `portal rotate-certs` — re-issue leaf certs from persisted CA |
| `rotate_certs_test.go` | 64 | Rotate-certs tests: validity override, missing tunnel dir |
| `status.go` | 444 | `portal status` — show tunnel health: pod status, Envoy stats, connectivity |
| `status_test.go` | 601 | Status tests: JSON output, pod phases, Envoy metrics parsing |

### `internal/envoy/`

| File | Lines | Description |
|------|-------|-------------|
| `config.go` | 116 | Render initiator/responder bootstrap YAML from embedded Go templates |
| `config_test.go` | 256 | Template rendering tests: port overrides, SNI, multi-backend |
| `templates/initiator_tcp.yaml` | — | Envoy bootstrap: TCP proxy with client TLS, upstream proxy protocol, admin interface |
| `templates/responder_tcp.yaml` | — | Envoy bootstrap: TCP proxy with server TLS, client cert validation, backend routing |

### `internal/kube/`

| File | Lines | Description |
|------|-------|-------------|
| `errors.go` | 40 | Sentinel errors and error type helpers for K8s operations |
| `kube.go` | 77 | `Client` interface, `NewClient` constructor, `CheckKubectl`, `CheckContext` |
| `kube_test.go` | 82 | Client construction tests, kubectl/context check tests |
| `kubectl.go` | 325 | `kubectlClient` implementation: Apply, Delete, WaitForDeployment, WaitForServiceAddress, PortForward, GetPods, GetService, RolloutRestart |
| `kubectl_test.go` | 687 | Full kubectl client tests with mock `CommandRunner` |
| `runner.go` | 51 | `CommandRunner` interface and `execRunner` (real subprocess) implementation |
| `types.go` | 85 | `PodInfo`, `ContainerInfo`, `ServiceInfo`, `ServicePort`, `LoadBalancerIngress`, `PortForwardSession` |

### `internal/manifest/`

| File | Lines | Description |
|------|-------|-------------|
| `certmanager.go` | 186 | Generate cert-manager CRDs: Issuer, Certificate for CA and leaf certs |
| `certmanager_test.go` | 112 | Cert-manager manifest validation tests |
| `render.go` | 734 | `Render()` — produces full `ManifestBundle` (Namespace, RBAC, ConfigMap, Secrets, Deployments, Services, NetworkPolicies, Kustomization) |
| `render_test.go` | 933 | Render tests: default values, custom flags, cert-manager, hostname endpoint, service types |
| `rotate.go` | 138 | `RotateCertificates` — re-issue leaves from persisted CA, update secrets and metadata |
| `rotate_test.go` | 199 | Rotation tests: leaf renewal, CA preservation, validity tracking |
| `writer.go` | 106 | `WriteToDisk` — write `ManifestBundle` to `source/` + `destination/` directories with Kustomization files |

### `internal/state/`

| File | Lines | Description |
|------|-------|-------------|
| `state.go` | 203 | `Store` — thread-safe CRUD for `~/.portal/tunnels.json`; `TunnelState` and `StateFile` types |
| `state_test.go` | 223 | State persistence tests: add, remove, list, concurrent access, exposed services |

### `test/e2e/`

| File | Lines | Description |
|------|-------|-------------|
| `e2e_test.go` | 208 | `TestMain` — builds portal, creates KIND clusters, installs MetalLB, orchestrates suite |
| `testutil.go` | 439 | Helpers: `runPortal`, `kubectlWithContext`, `waitForPods`, `waitForService`, `deployEchoServer` |
| `connect_test.go` | 244 | Tunnel creation and verification E2E tests |
| `expose_test.go` | 135 | Service exposure E2E tests |
| `status_test.go` | 111 | Status command E2E tests |
| `security_test.go` | 283 | Security validation: RBAC, NetworkPolicy, container hardening, TLS config |
| `stability_test.go` | 123 | Recovery and stability: pod restart, reconnection |
| `certs_test.go` | 164 | Certificate rotation E2E tests |
| `tunnel_test.go` | 88 | Tunnel topology and multi-tunnel E2E tests |

## Key Types

| Type | File | Purpose |
|------|------|---------|
| `certs.TunnelCertificates` | `internal/certs/certs.go` | PEM-encoded CA + initiator client + responder server certs and keys |
| `envoy.InitiatorConfig` | `internal/envoy/config.go` | Initiator Envoy bootstrap parameters (responder host/port, listen port, certs) |
| `envoy.ResponderConfig` | `internal/envoy/config.go` | Responder Envoy bootstrap parameters (listen port, backend host/port, certs) |
| `kube.Client` | `internal/kube/kube.go` | Interface: Apply, Delete, WaitForDeployment, WaitForServiceAddress, PortForward, GetPods, GetService, RolloutRestart |
| `kube.CommandRunner` | `internal/kube/runner.go` | Interface: subprocess execution abstraction for testability |
| `kube.PodInfo` | `internal/kube/types.go` | Pod summary: name, phase, ready, restarts, containers |
| `kube.ServiceInfo` | `internal/kube/types.go` | Service summary: type, cluster IP, external IPs, ports, LB ingress |
| `kube.PortForwardSession` | `internal/kube/types.go` | Active port-forward subprocess handle |
| `manifest.TunnelConfig` | `internal/manifest/render.go` | All parameters for rendering a tunnel's manifests |
| `manifest.ManifestBundle` | `internal/manifest/render.go` | Complete set of rendered K8s manifests for source + destination clusters |
| `manifest.Resource` | `internal/manifest/render.go` | Single K8s manifest (filename + YAML content) |
| `manifest.TunnelMetadata` | `internal/manifest/render.go` | Tunnel info for `tunnel.yaml` (name, contexts, endpoint, cert validity, rotation) |
| `manifest.RotateConfig` | `internal/manifest/rotate.go` | Parameters for certificate rotation |
| `state.Store` | `internal/state/state.go` | Thread-safe CRUD for tunnel state file |
| `state.TunnelState` | `internal/state/state.go` | Metadata for a single deployed tunnel |
| `state.StateFile` | `internal/state/state.go` | Root structure for `~/.portal/tunnels.json` |

## Data Flow

### `portal connect` (imperative deploy)

```
CLI args + flags
    │
    ▼
Validate prerequisites (kubectl, kube contexts)
    │
    ▼
certs.Generate() ──► TunnelCertificates (CA + leaves)
    │
    ▼
manifest.Render(TunnelConfig) ──► ManifestBundle
    │                               ├── Source resources (initiator)
    │                               └── Destination resources (responder)
    ▼
kube.Client.Apply() ── destination context ──► Deploy responder
    │
    ▼
kube.Client.WaitForServiceAddress() ──► Discover LB IP
    │
    ▼
manifest.Render() (re-render with real endpoint) ──► Updated initiator manifests
    │
    ▼
kube.Client.Apply() ── source context ──► Deploy initiator
    │
    ▼
state.Store.Add() ──► Persist tunnel to ~/.portal/tunnels.json
```

### `portal generate` (GitOps)

```
CLI args + flags
    │
    ▼
certs.Generate() ──► TunnelCertificates
    │
    ▼
manifest.Render(TunnelConfig) ──► ManifestBundle
    │
    ▼
manifest.WriteToDisk() ──► Output directory:
                            ├── source/
                            │   ├── *.yaml
                            │   └── kustomization.yaml
                            ├── destination/
                            │   ├── *.yaml
                            │   └── kustomization.yaml
                            ├── ca/
                            │   ├── ca.crt
                            │   ├── ca.key
                            │   └── .gitignore
                            └── tunnel.yaml
```

### `portal expose`

```
CLI args (context, service, --port)
    │
    ▼
state.Store.Find() ──► Look up tunnel by context
    │
    ▼
Parse existing responder ConfigMap ──► Current Envoy bootstrap
    │
    ▼
envoy.RenderResponder() ──► Updated bootstrap with new backend cluster
    │
    ▼
kube.Client.Apply() ── destination ──► Updated ConfigMap + new ClusterIP Service
    │
    ▼
kube.Client.RolloutRestart() ──► Restart responder to pick up config
    │
    ▼
kube.Client.Apply() ── source ──► ClusterIP Service pointing to initiator
    │
    ▼
state.Store.Update() ──► Record exposed service
```

## Change Patterns

### Add a CLI Subcommand

1. Create `internal/cli/<command>.go` with `NewXxxCmd() *cobra.Command`
2. Follow the existing pattern: validate prerequisites → render → apply/write → update state
3. Add testability hooks as package-level `var` functions if the command calls external services
4. Create `internal/cli/<command>_test.go` with table-driven tests
5. Register the command in `cmd/portal/main.go`: `rootCmd.AddCommand(cli.NewXxxCmd())`
6. Run `go test ./internal/cli/` and `go vet ./...`

### Modify Generated Kubernetes Manifests

1. Edit `internal/manifest/render.go` — modify the relevant `renderXxx()` helper or add a new one
2. If adding a new resource, add a `Resource` entry to `ManifestBundle.Source` or `.Destination`
3. Update `renderKustomization()` to include the new filename
4. Update `internal/manifest/render_test.go` — verify YAML structure, field values, and edge cases
5. If the resource needs new `TunnelConfig` fields, add them and update all callers in `internal/cli/`
6. Run `go test ./internal/manifest/ ./internal/cli/`

### Add a New Envoy Feature (Listener, Filter, Cluster)

1. Edit the relevant template in `internal/envoy/templates/` (`initiator_tcp.yaml` or `responder_tcp.yaml`)
2. If the feature requires new parameters, add fields to `InitiatorConfig` or `ResponderConfig` in `internal/envoy/config.go`
3. Update `internal/envoy/config_test.go` to verify the rendered output
4. If the feature is configurable via CLI flags, thread the flag through `internal/cli/` → `manifest.TunnelConfig` → `envoy.XxxConfig`
5. Run `go test ./internal/envoy/ ./internal/manifest/ ./internal/cli/`

### Extend `kube.Client` Interface

1. Add the method signature to `Client` in `internal/kube/kube.go`
2. Implement it on `kubectlClient` in `internal/kube/kubectl.go`
3. Add tests using a mock `CommandRunner` in `internal/kube/kubectl_test.go`
4. Update any test fakes in `internal/cli/` tests that implement `kube.Client`
5. Run `go test ./internal/kube/ ./internal/cli/`

### Add an E2E Test

1. Create or extend a test file in `test/e2e/` with the `//go:build e2e` tag
2. Use helpers from `testutil.go`: `runPortal`, `kubectlWithContext`, `waitForPods`, `waitForService`
3. Tests assume two KIND clusters (`portal-e2e-source`, `portal-e2e-destination`) and MetalLB — `TestMain` sets these up
4. Run: `go test -tags e2e -v -run TestYourTest ./test/e2e/`

## Entry Point

```
portal (cmd/portal/main.go)
├── generate <source_ctx> <dest_ctx>
│   ├── --output-dir (required)
│   ├── --responder-endpoint (required)
│   ├── --namespace (default: portal-system)
│   ├── --tunnel-port (default: 10443)
│   ├── --connection-count (default: 4)
│   ├── --cert-validity (default: 8760h)
│   ├── --cert-dir
│   ├── --cert-manager
│   ├── --envoy-image (default: envoyproxy/envoy:v1.37-latest@sha256:...)
│   ├── --envoy-log-level (default: info)
│   └── --service-type (default: LoadBalancer)
├── connect <source_ctx> <dest_ctx>
│   ├── (all generate flags)
│   ├── --deploy-timeout (default: 5m)
│   ├── --lb-timeout (default: 5m)
│   └── --dry-run
├── disconnect <source_ctx> <dest_ctx>
│   ├── --namespace
│   └── --delete-timeout (default: 2m)
├── expose <context> <service>
│   ├── --port (required)
│   ├── --service-namespace (default: default)
│   └── --tunnel
├── status [<source_ctx> <dest_ctx>]
│   └── --json
├── list
│   └── --json
└── rotate-certs <tunnel-dir>
    └── --cert-validity
```

## Test Locations

### Unit Tests

| Package | Test Files |
|---------|-----------|
| `internal/certs` | `certs_test.go` |
| `internal/cli` | `connect_test.go`, `disconnect_test.go`, `expose_test.go`, `generate_test.go`, `list_test.go`, `rotate_certs_test.go`, `status_test.go` |
| `internal/envoy` | `config_test.go` |
| `internal/kube` | `kube_test.go`, `kubectl_test.go` |
| `internal/manifest` | `certmanager_test.go`, `render_test.go`, `rotate_test.go` |
| `internal/state` | `state_test.go` |

### E2E Tests

| File | Focus |
|------|-------|
| `test/e2e/e2e_test.go` | Suite setup: build binary, create KIND clusters, install MetalLB |
| `test/e2e/connect_test.go` | Tunnel creation and verification |
| `test/e2e/expose_test.go` | Service exposure through tunnels |
| `test/e2e/status_test.go` | Status command output validation |
| `test/e2e/security_test.go` | RBAC, NetworkPolicy, container hardening, TLS |
| `test/e2e/stability_test.go` | Pod restart and reconnection recovery |
| `test/e2e/certs_test.go` | Certificate rotation |
| `test/e2e/tunnel_test.go` | Tunnel topology and multi-tunnel scenarios |
| `test/e2e/testutil.go` | Shared helpers: `runPortal`, `kubectlWithContext`, `waitForPods`, etc. |

## AGENTS.md Maintenance Rule

When making code changes, update this file if any of the following apply:

- A `.go` file is added, removed, or renamed → update **File Index**
- An exported type is added, removed, or renamed → update **Key Types**
- A CLI command or flag is added or changed → update **Entry Point**
- The data flow for a command changes materially → update **Data Flow**
- A new package is introduced → add to **Scope** and **File Index**
- Build/test commands change → update **Important Commands**
