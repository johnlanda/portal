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

	"github.com/tetratelabs/portal/internal/certs"
	"github.com/tetratelabs/portal/internal/manifest"
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
