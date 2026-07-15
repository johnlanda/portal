package manifest

import (
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
	"gopkg.in/yaml.v3"
)

func testHubTLS(t *testing.T) (certPEM, keyPEM, caPEM, crlPEM []byte) {
	t.Helper()
	ca, err := certs.NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err = ca.IssueHubServerCert("synapse", []string{"tunnel.corp.example"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crlPEM, err = ca.RenderCRL(nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM, ca.CertPEM(), crlPEM
}

func resourceByName(t *testing.T, resources []Resource, filename string) []byte {
	t.Helper()
	for _, r := range resources {
		if r.Filename == filename {
			return r.Content
		}
	}
	t.Fatalf("resource %q not found; have %v", filename, resourceNames(resources))
	return nil
}

func resourceNames(resources []Resource) []string {
	names := make([]string, len(resources))
	for i, r := range resources {
		names[i] = r.Filename
	}
	return names
}

func TestRenderHubManifests(t *testing.T) {
	certPEM, keyPEM, caPEM, crlPEM := testHubTLS(t)
	resources, err := RenderHubManifests(HubDeployConfig{
		HubName:   "synapse",
		Members:   []string{"acme-prod"},
		Services:  []ServiceConfig{{SNI: "api.hub", BackendHost: "backend.portal.svc", BackendPort: 8443}},
		EnableCRL: true,
		CertPEM:   certPEM, KeyPEM: keyPEM, CAPEM: caPEM, CRLPEM: crlPEM,
	})
	if err != nil {
		t.Fatalf("RenderHubManifests() error = %v", err)
	}

	for _, r := range resources {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Errorf("%s is not valid YAML: %v", r.Filename, err)
		}
	}

	bootstrap := resourceByName(t, resources, "portal-hub-bootstrap-cm.yaml")
	if !strings.Contains(string(bootstrap), "reverse_tunnel") {
		t.Error("hub bootstrap ConfigMap does not contain reverse tunnel config")
	}
	if !strings.Contains(string(bootstrap), "crl.pem") {
		t.Error("hub bootstrap does not reference the CRL despite EnableCRL")
	}

	secret := resourceByName(t, resources, "portal-hub-tls-secret.yaml")
	if !strings.Contains(string(secret), "crl.pem") {
		t.Error("hub Secret does not contain the CRL")
	}

	svc := resourceByName(t, resources, "portal-hub-service.yaml")
	if !strings.Contains(string(svc), "type: LoadBalancer") {
		t.Error("tunnel Service did not default to LoadBalancer")
	}

	egress := resourceByName(t, resources, "portal-hub-egress-service.yaml")
	if !strings.Contains(string(egress), "type: ClusterIP") || !strings.Contains(string(egress), "port: 10080") {
		t.Error("egress Service is not ClusterIP on the default egress port")
	}

	dep := resourceByName(t, resources, "portal-hub-deployment.yaml")
	if !strings.Contains(string(dep), DefaultEnvoyImage) {
		t.Error("hub Deployment does not use the pinned Envoy image")
	}
	if !strings.Contains(string(dep), "readOnlyRootFilesystem: true") {
		t.Error("hub Deployment is not hardened")
	}
}

func TestRenderHubManifestsRequiresTLS(t *testing.T) {
	if _, err := RenderHubManifests(HubDeployConfig{HubName: "synapse"}); err == nil {
		t.Error("expected error without TLS material")
	}
	certPEM, keyPEM, caPEM, _ := testHubTLS(t)
	if _, err := RenderHubManifests(HubDeployConfig{
		HubName: "synapse", EnableCRL: true,
		CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caPEM,
	}); err == nil {
		t.Error("expected error with EnableCRL but no CRLPEM")
	}
}

func TestRenderMemberManifests(t *testing.T) {
	resources, err := RenderMemberManifests(MemberDeployConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubAddr:    "tunnel.corp.example:10443",
		Published:  []envoy.PublishedService{{Name: "inference", BackendHost: "inference.svc", BackendPort: 8080}},
		Forward:    []envoy.ServiceListener{{Name: "telemetry", ListenPort: 4317}},
		CertPEM:    []byte("CERT"), KeyPEM: []byte("KEY"), CAPEM: []byte("CA"),
	})
	if err != nil {
		t.Fatalf("RenderMemberManifests() error = %v", err)
	}

	bootstrap := resourceByName(t, resources, "portal-member-bootstrap-cm.yaml")
	if !strings.Contains(string(bootstrap), "rc://acme-prod:acme-prod:synapse@hub:4") {
		t.Error("member bootstrap does not contain the rc:// address")
	}

	resourceByName(t, resources, "portal-member-tls-secret.yaml")
	resourceByName(t, resources, "portal-member-deployment.yaml")

	fwd := resourceByName(t, resources, "portal-fwd-telemetry-service.yaml")
	if !strings.Contains(string(fwd), "port: 4317") {
		t.Error("forward Service does not expose the listener port")
	}
}

// TestRenderMemberManifestsWithoutSecret covers the two-phase join flow: the
// Secret is managed out of band so the key never rides a rendered manifest.
func TestRenderMemberManifestsWithoutSecret(t *testing.T) {
	resources, err := RenderMemberManifests(MemberDeployConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubAddr:    "tunnel.corp.example:10443",
	})
	if err != nil {
		t.Fatalf("RenderMemberManifests() error = %v", err)
	}
	for _, r := range resources {
		if r.Filename == "portal-member-tls-secret.yaml" {
			t.Error("Secret must not be rendered without key material")
		}
	}
}
