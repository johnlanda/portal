# Portal

## Project Overview

Portal is a CLI tool that creates secure, multiplexed Envoy reverse tunnels between Kubernetes clusters using mTLS (TLS 1.3, RSA 4096). It is part of the [Synapse workspace](../CLAUDE.md) — the management plane for Envoy AI Gateway deployments. Synapse uses Portal tunnels for worker-to-backend connectivity across clusters.

- **Module:** `github.com/tetratelabs/portal`
- **Language:** Go 1.23
- **License:** MIT
- **Dependencies:** `cobra` (CLI), `gopkg.in/yaml.v3` (YAML rendering); no `k8s.io` client libraries

## Repository Structure

```
portal/
├── cmd/portal/
│   └── main.go                 # CLI entrypoint — assembles root cobra.Command
├── internal/
│   ├── certs/                  # mTLS certificate generation and rotation (RSA 4096)
│   ├── cli/                    # Command implementations (connect, generate, expose, etc.)
│   ├── envoy/                  # Envoy bootstrap config rendering
│   │   └── templates/          # Go-embedded YAML templates (initiator_tcp, responder_tcp)
│   ├── kube/                   # kubectl-based Kubernetes client abstraction
│   ├── manifest/               # K8s manifest rendering, disk writer, cert-manager CRDs
│   └── state/                  # Persistent tunnel state (~/.portal/tunnels.json)
├── test/e2e/                   # E2E tests (build tag: e2e) — KIND + MetalLB
├── docs/                       # PRD, demo guide, logo
├── go.mod
├── README.md
├── SECURITY.md
└── REQUIREMENTS.md
```

## Architecture

### Tunnel Topology

```
Source Cluster                          Destination Cluster
┌─────────────────────┐                ┌──────────────────────┐
│  App Pod             │                │  Target Service      │
│    ↓                 │                │    ↑                 │
│  portal-initiator    │  TLS 1.3      │  portal-responder    │
│  (Envoy + client     │───────────────→│  (Envoy + server     │
│   cert)              │  port 10443    │   cert)              │
└─────────────────────┘                └──────────────────────┘
```

### Certificate Hierarchy

```
Portal CA (self-signed, per-tunnel)
  ├── Initiator Client Cert  (CN: portal-initiator/<tunnel-name>)
  └── Responder Server Cert  (CN: portal-responder/<tunnel-name>)
```

Supports both built-in PKI (`internal/certs`) and cert-manager integration (`--cert-manager`).

### Package Responsibilities

| Package | Responsibility |
|---------|---------------|
| `certs` | Generate per-tunnel CA + leaf certificates; rotate leaves from persisted CA |
| `cli` | Cobra command implementations; orchestrate render → apply → state-update |
| `envoy` | Render Envoy bootstrap YAML from Go templates (initiator + responder) |
| `kube` | Shell out to `kubectl` for apply/delete/wait/port-forward; `CommandRunner` interface for testing |
| `manifest` | Render full K8s manifest bundles (Deployments, Services, Secrets, NetworkPolicies, Kustomization); cert-manager CRDs; disk writer |
| `state` | Thread-safe CRUD for `~/.portal/tunnels.json`; track deployed tunnels and exposed services |

## Development Conventions

- Follow Effective Go; exported symbols require godoc comments
- Unit tests live alongside source (`*_test.go`); E2E tests in `test/e2e/` behind `//go:build e2e`
- Testability via package-level `var` hooks (e.g., `newKubeClient`, `checkKubectlFn`) — tests swap and restore via `t.Cleanup`
- No `k8s.io` client imports — `kube.Client` wraps `kubectl` for zero-dependency auth compatibility
- Error wrapping with `fmt.Errorf("context: %w", err)` throughout

See [`AGENTS.md`](AGENTS.md) for full coding standards, testing conventions, command reference, and local development workflow.

## Module Map

| Task | See |
|------|-----|
| Add a CLI subcommand | [Change Patterns in AGENTS.md](AGENTS.md#change-patterns) |
| Modify generated K8s manifests | [Change Patterns in AGENTS.md](AGENTS.md#change-patterns) |
| Add a new Envoy feature (listener, filter) | [Change Patterns in AGENTS.md](AGENTS.md#change-patterns) |
| Extend `kube.Client` interface | [Change Patterns in AGENTS.md](AGENTS.md#change-patterns) |
| Add an E2E test | [Change Patterns in AGENTS.md](AGENTS.md#change-patterns) |
| Build, test, or lint | [Important Commands in AGENTS.md](AGENTS.md#important-commands) |
| Run E2E tests with KIND | [Local Development in AGENTS.md](AGENTS.md#local-development) |
| Understand data flow | [Data Flow in AGENTS.md](AGENTS.md#data-flow) |
| Look up a file or type | [File Index](AGENTS.md#file-index) / [Key Types](AGENTS.md#key-types) |
