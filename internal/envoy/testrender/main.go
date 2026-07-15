// Command testrender renders sample reverse tunnel bootstraps and matching
// PKI material for validation against a real Envoy binary. Not shipped; used
// by developers and CI only.
//
// Usage: testrender <output-dir>
//
// Writes member.yaml, hub.yaml, and certs/{ca.crt,tls.crt,tls.key,crl.pem}
// under the output dir. Validate with:
//
//	docker run --rm -v <dir>:/cfg:ro -v <dir>/certs:/etc/portal/certs:ro \
//	  <pinned-envoy-image> --mode validate -c /cfg/hub.yaml
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: testrender <output-dir>")
		os.Exit(1)
	}
	out := os.Args[1]
	certDir := filepath.Join(out, "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		panic(err)
	}

	// PKI: hub CA, hub server cert, one member enrolled via CSR, one CRL
	// revoking a second member.
	ca, err := certs.NewHubCA("synapse", 24*time.Hour)
	if err != nil {
		panic(err)
	}
	hubCert, hubKey, err := ca.IssueHubServerCert("synapse", []string{"tunnel.corp.example", envoy.DefaultHandshakeSNI}, 24*time.Hour)
	if err != nil {
		panic(err)
	}
	_, csrPEM, err := certs.GenerateMemberKeyAndCSR(certs.MemberIdentity{Member: "acme-prod", Tenant: "synapse"})
	if err != nil {
		panic(err)
	}
	if _, err := ca.SignCSR(csrPEM, certs.MemberIdentity{Member: "acme-prod", Tenant: "synapse"}, 24*time.Hour); err != nil {
		panic(err)
	}
	_, evictedCSR, err := certs.GenerateMemberKeyAndCSR(certs.MemberIdentity{Member: "globex-dev", Tenant: "synapse"})
	if err != nil {
		panic(err)
	}
	evictedCert, err := ca.SignCSR(evictedCSR, certs.MemberIdentity{Member: "globex-dev", Tenant: "synapse"}, 24*time.Hour)
	if err != nil {
		panic(err)
	}
	serial, err := certs.ParseCertificateSerial(evictedCert)
	if err != nil {
		panic(err)
	}
	crlPEM, err := ca.RenderCRL([]certs.RevokedCert{{Serial: serial}}, 2)
	if err != nil {
		panic(err)
	}
	writeFile(filepath.Join(certDir, "ca.crt"), ca.CertPEM())
	writeFile(filepath.Join(certDir, "tls.crt"), hubCert)
	writeFile(filepath.Join(certDir, "tls.key"), hubKey)
	writeFile(filepath.Join(certDir, "crl.pem"), crlPEM)

	// Bootstraps.
	member, err := envoy.RenderMemberBootstrap(envoy.MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Published: []envoy.PublishedService{
			{Name: "inference", BackendHost: "inference.default.svc", BackendPort: 8080, Protocol: "grpc"},
			{Name: "admin", BackendHost: "admin.default.svc", BackendPort: 9000},
		},
		Forward: []envoy.ServiceListener{{Name: "telemetry", ListenPort: 4317}},
	})
	if err != nil {
		panic(err)
	}
	hub, err := envoy.RenderHubBootstrap(envoy.HubConfig{
		Members:   []string{"acme-prod", "globex-dev"},
		Services:  []envoy.ServiceRoute{{SNI: "api.hub", BackendHost: "backend.portal.svc", BackendPort: 8443}},
		EnableCRL: true,
	})
	if err != nil {
		panic(err)
	}
	writeFile(filepath.Join(out, "member.yaml"), member)
	writeFile(filepath.Join(out, "hub.yaml"), hub)
	fmt.Println("rendered")
}

func writeFile(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}
