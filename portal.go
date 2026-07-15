// Package portal provides a Go library for creating secure, multiplexed Envoy
// reverse tunnels between Kubernetes clusters. It is the programmatic equivalent
// of the `portal` CLI tool.
//
// Use [RenderTunnel] or [RenderTunnelWithServices] to generate Kubernetes manifests
// for a tunnel. Use [AddService] to additively expose new services through an
// existing tunnel. Use [GenerateCertificates] to create the mTLS PKI.
//
// Example:
//
//	bundle, err := portal.RenderTunnelWithServices(portal.TunnelConfig{
//	    SourceContext:      "dp-cluster",
//	    DestinationContext: "mgmt-cluster",
//	    ResponderEndpoint:  "tunnel.example.com:10443",
//	}, []portal.ServiceConfig{
//	    {SNI: "backend", BackendHost: "backend-svc.ns.svc", BackendPort: 8443},
//	    {SNI: "otel", BackendHost: "otel-collector.ns.svc", BackendPort: 4317},
//	})
package portal

import (
	"time"

	"github.com/johnlanda/portal/internal/baremetal"
	"math/big"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/manifest"
)

// TunnelConfig contains all parameters needed to render a tunnel's manifests.
// This is a re-export of [manifest.TunnelConfig] for the public API.
type TunnelConfig = manifest.TunnelConfig

// ServiceConfig describes a service to be routed through the tunnel.
// This is a re-export of [manifest.ServiceConfig] for the public API.
type ServiceConfig = manifest.ServiceConfig

// ExternalCertificates holds PEM-encoded certificate material provided externally.
// This is a re-export of [manifest.ExternalCertificates] for the public API.
type ExternalCertificates = manifest.ExternalCertificates

// ManifestBundle contains all rendered Kubernetes resources for both sides of a tunnel.
// This is a re-export of [manifest.ManifestBundle] for the public API.
type ManifestBundle = manifest.ManifestBundle

// Resource represents a single Kubernetes resource manifest.
// This is a re-export of [manifest.Resource] for the public API.
type Resource = manifest.Resource

// TunnelMetadata stores information about the tunnel for the metadata file.
// This is a re-export of [manifest.TunnelMetadata] for the public API.
type TunnelMetadata = manifest.TunnelMetadata

// TunnelCertificates holds all PEM-encoded certificates and keys for a tunnel.
// This is a re-export of [certs.TunnelCertificates] for the public API.
type TunnelCertificates = certs.TunnelCertificates

// RenderTunnel generates a complete [ManifestBundle] for a single-service tunnel.
// This is the simplest entry point — it renders all Kubernetes resources needed
// for both the source (initiator) and destination (responder) clusters.
func RenderTunnel(cfg TunnelConfig) (*ManifestBundle, error) {
	return manifest.Render(cfg)
}

// RenderTunnelWithServices generates a [ManifestBundle] for a multi-service tunnel.
// Each service gets its own SNI-based route on the responder and a dedicated
// listener port on the initiator.
func RenderTunnelWithServices(cfg TunnelConfig, services []ServiceConfig) (*ManifestBundle, error) {
	cfg.Services = services
	return manifest.Render(cfg)
}

// AddService generates updated manifests that include a new service alongside
// any existing services. The existing services and new service are merged and
// rendered together.
func AddService(cfg TunnelConfig, existing []ServiceConfig, svc ServiceConfig) (*ManifestBundle, error) {
	cfg.Services = append(existing, svc)
	return manifest.Render(cfg)
}

// GenerateCertificates creates the mTLS PKI for a tunnel: a self-signed CA,
// an initiator client certificate, and a responder server certificate.
// responderSANs should include any DNS names or IPs the responder needs in its cert.
func GenerateCertificates(tunnelName string, responderSANs []string, validity time.Duration) (*TunnelCertificates, error) {
	return certs.GenerateTunnelCertificates(tunnelName, responderSANs, validity)
}

// BareMetalConfig contains all parameters needed to render bare metal tunnel artifacts.
// This is a re-export of [baremetal.BareMetalConfig] for the public API.
type BareMetalConfig = baremetal.BareMetalConfig

// BareMetalBundle contains all rendered artifacts for both sides of a bare metal tunnel.
// This is a re-export of [baremetal.BareMetalBundle] for the public API.
type BareMetalBundle = baremetal.BareMetalBundle

// RenderBareMetalTunnel generates a complete [BareMetalBundle] for a bare metal
// or VM tunnel. It produces raw Envoy configs, systemd units, and docker-compose
// files instead of Kubernetes manifests.
func RenderBareMetalTunnel(cfg BareMetalConfig) (*BareMetalBundle, error) {
	return baremetal.Render(cfg)
}

// --- v2 hub/member API (docs/v2-proposal.md) ---
//
// The hub/member model serves topologies where the member side has egress
// only: the member dials out and maintains persistent reverse connections;
// hub-originated requests reach published member services over them. These
// functions are stateless building blocks for embedding Portal in a hosted
// product: the caller persists CA material and member registries, renders
// Envoy bootstrap content (e.g. into ConfigMaps via Helm), and manages
// enrollment. Member private keys are generated with
// [GenerateMemberKeyAndCSR] where the member runs and never travel; the hub
// signs CSRs with [HubCA.SignCSR], binding identity to the certificate DNS
// SAN. Eviction is a re-rendered CRL ([HubCA.RenderCRL]) that Envoy
// hot-reloads.

// MemberIdentity identifies a member for certificate issuance.
// This is a re-export of [certs.MemberIdentity] for the public API.
type MemberIdentity = certs.MemberIdentity

// RevokedCert identifies a certificate to include in a CRL.
// This is a re-export of [certs.RevokedCert] for the public API.
type RevokedCert = certs.RevokedCert

// HubCA signs member client certificates and renders CRLs for eviction.
// This is a re-export of [certs.HubCA] for the public API.
type HubCA = certs.HubCA

// MemberConfig configures the member (egress-only cluster) Envoy bootstrap.
// This is a re-export of [envoy.MemberConfig] for the public API.
type MemberConfig = envoy.MemberConfig

// HubConfig configures the hub (ingress-capable cluster) Envoy bootstrap.
// This is a re-export of [envoy.HubConfig] for the public API.
type HubConfig = envoy.HubConfig

// PublishedService describes a member-local service reachable from the hub.
// This is a re-export of [envoy.PublishedService] for the public API.
type PublishedService = envoy.PublishedService

// ServiceListener describes a v1-style forward listener on the member.
// This is a re-export of [envoy.ServiceListener] for the public API.
type ServiceListener = envoy.ServiceListener

// ServiceRoute describes a hub-local backend reachable by members over the
// forward SNI path. This is a re-export of [envoy.ServiceRoute] for the
// public API.
type ServiceRoute = envoy.ServiceRoute

// NewHubCA generates a new self-signed hub certificate authority.
func NewHubCA(hubName string, validity time.Duration) (*HubCA, error) {
	return certs.NewHubCA(hubName, validity)
}

// LoadHubCA parses persisted hub CA material previously obtained from
// [HubCA.CertPEM] and [HubCA.KeyPEM].
func LoadHubCA(certPEM, keyPEM []byte) (*HubCA, error) {
	return certs.LoadHubCA(certPEM, keyPEM)
}

// GenerateMemberKeyAndCSR generates a member keypair and CSR for two-phase
// enrollment. Call it where the member runs so the private key never leaves
// the member's environment.
func GenerateMemberKeyAndCSR(id MemberIdentity) (keyPEM, csrPEM []byte, err error) {
	return certs.GenerateMemberKeyAndCSR(id)
}

// ParseCertificateSerial extracts a certificate's serial number, for building
// [RevokedCert] entries in eviction flows.
func ParseCertificateSerial(certPEM []byte) (*big.Int, error) {
	return certs.ParseCertificateSerial(certPEM)
}

// RenderMemberBootstrap renders the member Envoy bootstrap configuration
// (e.g. for a ConfigMap in a Helm chart).
func RenderMemberBootstrap(cfg MemberConfig) ([]byte, error) {
	return envoy.RenderMemberBootstrap(cfg)
}

// RenderHubBootstrap renders the hub Envoy bootstrap configuration.
func RenderHubBootstrap(cfg HubConfig) ([]byte, error) {
	return envoy.RenderHubBootstrap(cfg)
}
