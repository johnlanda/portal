package portal

import (
	"strings"
	"testing"
	"time"
)

func TestRenderTunnel(t *testing.T) {
	bundle, err := RenderTunnel(TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("RenderTunnel() error = %v", err)
	}

	if len(bundle.Source) == 0 {
		t.Error("expected source resources")
	}
	if len(bundle.Destination) == 0 {
		t.Error("expected destination resources")
	}
	if bundle.Certs == nil {
		t.Error("expected generated certs")
	}
	if bundle.Metadata.TunnelName != "src--dst" {
		t.Errorf("TunnelName = %q, want %q", bundle.Metadata.TunnelName, "src--dst")
	}
}

func TestRenderTunnelWithServices(t *testing.T) {
	bundle, err := RenderTunnelWithServices(TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}, []ServiceConfig{
		{SNI: "backend", BackendHost: "backend.ns.svc", BackendPort: 8443, LocalPort: 18443},
		{SNI: "otel", BackendHost: "otel.ns.svc", BackendPort: 4317},
	})
	if err != nil {
		t.Fatalf("RenderTunnelWithServices() error = %v", err)
	}

	if len(bundle.Metadata.Services) != 2 {
		t.Errorf("expected 2 services in metadata, got %d", len(bundle.Metadata.Services))
	}

	// Verify multi-service bootstrap is used.
	var hasInspector bool
	for _, r := range bundle.Destination {
		if strings.Contains(string(r.Content), "tls_inspector") {
			hasInspector = true
		}
	}
	if !hasInspector {
		t.Error("expected tls_inspector in multi-service responder config")
	}
}

func TestAddService(t *testing.T) {
	existing := []ServiceConfig{
		{SNI: "backend", BackendHost: "backend.ns.svc", BackendPort: 8443},
	}
	newSvc := ServiceConfig{
		SNI: "otel", BackendHost: "otel.ns.svc", BackendPort: 4317,
	}

	bundle, err := AddService(TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}, existing, newSvc)
	if err != nil {
		t.Fatalf("AddService() error = %v", err)
	}

	if len(bundle.Metadata.Services) != 2 {
		t.Errorf("expected 2 services after add, got %d", len(bundle.Metadata.Services))
	}
}

func TestGenerateCertificates(t *testing.T) {
	tc, err := GenerateCertificates("test-tunnel", []string{"10.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateCertificates() error = %v", err)
	}

	if len(tc.CACert) == 0 {
		t.Error("CACert is empty")
	}
	if len(tc.CAKey) == 0 {
		t.Error("CAKey is empty")
	}
	if len(tc.InitiatorCert) == 0 {
		t.Error("InitiatorCert is empty")
	}
	if len(tc.ResponderCert) == 0 {
		t.Error("ResponderCert is empty")
	}
}

func TestRenderTunnelWithExternalCerts(t *testing.T) {
	bundle, err := RenderTunnel(TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		ExternalCerts: &ExternalCertificates{
			CACert:        []byte("ca"),
			InitiatorCert: []byte("init-cert"),
			InitiatorKey:  []byte("init-key"),
			ResponderCert: []byte("resp-cert"),
			ResponderKey:  []byte("resp-key"),
		},
	})
	if err != nil {
		t.Fatalf("RenderTunnel() error = %v", err)
	}

	// Should not generate certs when external are provided.
	if bundle.Certs != nil {
		t.Error("Certs should be nil when using external certificates")
	}
}

// TestHubMemberEnrollmentLifecycle exercises the v2 public API end to end:
// CA creation, two-phase CSR enrollment, bootstrap rendering, and eviction.
func TestHubMemberEnrollmentLifecycle(t *testing.T) {
	ca, err := NewHubCA("synapse", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}

	// Member side: key born where the member runs.
	id := MemberIdentity{Member: "acme-prod", Tenant: "synapse"}
	keyPEM, csrPEM, err := GenerateMemberKeyAndCSR(id)
	if err != nil {
		t.Fatalf("GenerateMemberKeyAndCSR() error = %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("no member key generated")
	}

	// Hub side: sign, then later evict.
	certPEM, err := ca.SignCSR(csrPEM, id, 24*time.Hour)
	if err != nil {
		t.Fatalf("SignCSR() error = %v", err)
	}
	serial, err := ParseCertificateSerial(certPEM)
	if err != nil {
		t.Fatalf("ParseCertificateSerial() error = %v", err)
	}
	crlPEM, err := ca.RenderCRL([]RevokedCert{{Serial: serial}})
	if err != nil {
		t.Fatalf("RenderCRL() error = %v", err)
	}
	if len(crlPEM) == 0 {
		t.Fatal("no CRL rendered")
	}

	// Bootstrap rendering for both sides.
	member, err := RenderMemberBootstrap(MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Published:  []PublishedService{{Name: "inference", BackendHost: "inference.svc", BackendPort: 8080}},
	})
	if err != nil {
		t.Fatalf("RenderMemberBootstrap() error = %v", err)
	}
	hub, err := RenderHubBootstrap(HubConfig{Members: []string{"acme-prod"}, EnableCRL: true})
	if err != nil {
		t.Fatalf("RenderHubBootstrap() error = %v", err)
	}
	if len(member) == 0 || len(hub) == 0 {
		t.Fatal("empty bootstrap rendered")
	}
}
