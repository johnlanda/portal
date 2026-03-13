package baremetal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/johnlanda/portal/internal/manifest"
)

func TestWriteToDisk(t *testing.T) {
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "test-output")
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() failed: %v", err)
	}

	// Verify directory structure.
	expectedFiles := []string{
		"initiator/envoy.yaml",
		"initiator/certs/tls.crt",
		"initiator/certs/tls.key",
		"initiator/certs/ca.crt",
		"initiator/portal-initiator.service",
		"initiator/docker-compose.yaml",
		"responder/envoy.yaml",
		"responder/certs/tls.crt",
		"responder/certs/tls.key",
		"responder/certs/ca.crt",
		"responder/portal-responder.service",
		"responder/docker-compose.yaml",
		"ca/ca.crt",
		"ca/ca.key",
		"ca/.gitignore",
		"tunnel.yaml",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(outputDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s does not exist", f)
		}
	}

	// Verify tunnel.yaml is valid YAML with correct metadata.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta["tunnelName"] != "source-host--dest-host" {
		t.Errorf("tunnelName = %v, want %q", meta["tunnelName"], "source-host--dest-host")
	}
	if meta["deployTarget"] != "bare-metal" {
		t.Errorf("deployTarget = %v, want %q", meta["deployTarget"], "bare-metal")
	}

	// Verify envoy.yaml references the responder.
	envoyBytes, err := os.ReadFile(filepath.Join(outputDir, "initiator", "envoy.yaml"))
	if err != nil {
		t.Fatalf("failed to read initiator envoy.yaml: %v", err)
	}
	if !strings.Contains(string(envoyBytes), "10.0.1.50") {
		t.Error("initiator envoy.yaml should reference responder host")
	}

	// Verify cert file permissions.
	info, err := os.Stat(filepath.Join(outputDir, "initiator", "certs", "tls.key"))
	if err != nil {
		t.Fatalf("failed to stat tls.key: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("tls.key permissions = %o, want 0600", info.Mode().Perm())
	}

	info, err = os.Stat(filepath.Join(outputDir, "ca", "ca.key"))
	if err != nil {
		t.Fatalf("failed to stat ca.key: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("ca.key permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestWriteToDiskExternalCerts(t *testing.T) {
	// When using external certs, no ca/ directory should be created.
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		ExternalCerts: &manifest.ExternalCertificates{
			CACert:        []byte("ca-cert"),
			InitiatorCert: []byte("init-cert"),
			InitiatorKey:  []byte("init-key"),
			ResponderCert: []byte("resp-cert"),
			ResponderKey:  []byte("resp-key"),
		},
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "ext-output")
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() failed: %v", err)
	}

	// CA directory should not exist when using external certs.
	caDir := filepath.Join(outputDir, "ca")
	if _, err := os.Stat(caDir); !os.IsNotExist(err) {
		t.Error("ca/ directory should not exist when using external certs")
	}

	// Cert files should still be written.
	certBytes, err := os.ReadFile(filepath.Join(outputDir, "initiator", "certs", "tls.crt"))
	if err != nil {
		t.Fatalf("failed to read tls.crt: %v", err)
	}
	if string(certBytes) != "init-cert" {
		t.Errorf("tls.crt = %q, want %q", string(certBytes), "init-cert")
	}
}

func TestWriteToDiskMultiService(t *testing.T) {
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		Services: []manifest.ServiceConfig{
			{SNI: "backend", BackendHost: "10.0.1.100", BackendPort: 8443},
			{SNI: "otel", BackendHost: "10.0.1.101", BackendPort: 4317},
		},
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "multi-output")
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() failed: %v", err)
	}

	// Verify responder envoy config has multi-service routing.
	responderConfig, err := os.ReadFile(filepath.Join(outputDir, "responder", "envoy.yaml"))
	if err != nil {
		t.Fatalf("failed to read responder envoy.yaml: %v", err)
	}
	if !strings.Contains(string(responderConfig), "tls_inspector") {
		t.Error("responder envoy.yaml should contain tls_inspector for multi-service")
	}

	// Verify tunnel.yaml has services.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta BareMetalMetadata
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("failed to parse tunnel.yaml: %v", err)
	}
	if len(meta.Services) != 2 {
		t.Errorf("expected 2 services in metadata, got %d", len(meta.Services))
	}
}
