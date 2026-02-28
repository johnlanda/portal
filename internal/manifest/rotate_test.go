package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// generateTestTunnelCertManager creates a cert-manager-mode tunnel directory for testing.
func generateTestTunnelCertManager(t *testing.T) string {
	t.Helper()
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "tunnel.example.com:10443",
		CertValidity:       24 * time.Hour,
		CertManager:        true,
	}
	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	outputDir := t.TempDir()
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}
	return outputDir
}

// generateTestTunnel creates a tunnel directory with all manifests for testing.
func generateTestTunnel(t *testing.T) string {
	t.Helper()
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "tunnel.example.com:10443",
		CertValidity:       24 * time.Hour,
	}
	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	outputDir := t.TempDir()
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}
	return outputDir
}

func TestRotateCertificates(t *testing.T) {
	tunnelDir := generateTestTunnel(t)

	// Record original file contents for non-secret files.
	origDeployment, err := os.ReadFile(filepath.Join(tunnelDir, "source", "portal-initiator-deployment.yaml"))
	if err != nil {
		t.Fatalf("failed to read original deployment: %v", err)
	}
	origSourceSecret, err := os.ReadFile(filepath.Join(tunnelDir, "source", "portal-tunnel-tls-secret.yaml"))
	if err != nil {
		t.Fatalf("failed to read original source secret: %v", err)
	}
	origDestSecret, err := os.ReadFile(filepath.Join(tunnelDir, "destination", "portal-tunnel-tls-secret.yaml"))
	if err != nil {
		t.Fatalf("failed to read original destination secret: %v", err)
	}

	// Rotate.
	meta, err := RotateCertificates(RotateConfig{TunnelDir: tunnelDir})
	if err != nil {
		t.Fatalf("RotateCertificates() error = %v", err)
	}

	if meta.TunnelName != "src--dst" {
		t.Errorf("TunnelName = %q, want %q", meta.TunnelName, "src--dst")
	}

	// Secret files should have changed.
	newSourceSecret, _ := os.ReadFile(filepath.Join(tunnelDir, "source", "portal-tunnel-tls-secret.yaml"))
	newDestSecret, _ := os.ReadFile(filepath.Join(tunnelDir, "destination", "portal-tunnel-tls-secret.yaml"))

	if string(newSourceSecret) == string(origSourceSecret) {
		t.Error("source secret should have changed after rotation")
	}
	if string(newDestSecret) == string(origDestSecret) {
		t.Error("destination secret should have changed after rotation")
	}

	// Non-secret files should be unchanged.
	currentDeployment, _ := os.ReadFile(filepath.Join(tunnelDir, "source", "portal-initiator-deployment.yaml"))
	if string(currentDeployment) != string(origDeployment) {
		t.Error("deployment file should not change during rotation")
	}
}

func TestRotateCertificatesNoCaDir(t *testing.T) {
	tunnelDir := generateTestTunnel(t)

	// Remove the CA directory.
	os.RemoveAll(filepath.Join(tunnelDir, "ca"))

	_, err := RotateCertificates(RotateConfig{TunnelDir: tunnelDir})
	if err == nil {
		t.Fatal("expected error when CA directory is missing")
	}
	if !strings.Contains(err.Error(), "older version") {
		t.Errorf("error should mention older version, got: %v", err)
	}
}

func TestRotateCertificatesMetadataUpdated(t *testing.T) {
	tunnelDir := generateTestTunnel(t)

	// First rotation.
	meta1, err := RotateCertificates(RotateConfig{TunnelDir: tunnelDir})
	if err != nil {
		t.Fatalf("first rotation error = %v", err)
	}
	if meta1.RotationCount != 1 {
		t.Errorf("RotationCount after first rotation = %d, want 1", meta1.RotationCount)
	}
	if meta1.LastRotatedAt == nil {
		t.Error("LastRotatedAt should be set after rotation")
	}

	// Second rotation.
	meta2, err := RotateCertificates(RotateConfig{TunnelDir: tunnelDir})
	if err != nil {
		t.Fatalf("second rotation error = %v", err)
	}
	if meta2.RotationCount != 2 {
		t.Errorf("RotationCount after second rotation = %d, want 2", meta2.RotationCount)
	}

	// Verify metadata persisted to disk.
	metaBytes, err := os.ReadFile(filepath.Join(tunnelDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var diskMeta TunnelMetadata
	if err := yaml.Unmarshal(metaBytes, &diskMeta); err != nil {
		t.Fatalf("failed to parse tunnel.yaml: %v", err)
	}
	if diskMeta.RotationCount != 2 {
		t.Errorf("disk RotationCount = %d, want 2", diskMeta.RotationCount)
	}
	if diskMeta.LastRotatedAt == nil {
		t.Error("disk LastRotatedAt should be set")
	}
}

func TestRotateCertificatesBackwardCompat(t *testing.T) {
	tunnelDir := generateTestTunnel(t)

	// Simulate an old tunnel.yaml without new fields by stripping them.
	metaPath := filepath.Join(tunnelDir, "tunnel.yaml")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var rawMeta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &rawMeta); err != nil {
		t.Fatalf("failed to parse tunnel.yaml: %v", err)
	}
	delete(rawMeta, "certValidity")
	delete(rawMeta, "responderSANs")
	delete(rawMeta, "lastRotatedAt")
	delete(rawMeta, "rotationCount")
	strippedBytes, _ := yaml.Marshal(rawMeta)
	os.WriteFile(metaPath, strippedBytes, 0644)

	// Rotation should still work — fields default cleanly.
	meta, err := RotateCertificates(RotateConfig{TunnelDir: tunnelDir})
	if err != nil {
		t.Fatalf("RotateCertificates() error = %v", err)
	}
	if meta.RotationCount != 1 {
		t.Errorf("RotationCount = %d, want 1", meta.RotationCount)
	}
	// SANs should be derived from ResponderEndpoint.
	if len(meta.ResponderSANs) == 0 {
		t.Error("ResponderSANs should be derived from ResponderEndpoint")
	}
}

func TestRotateCertificatesCertManagerTunnel(t *testing.T) {
	tunnelDir := generateTestTunnelCertManager(t)

	_, err := RotateCertificates(RotateConfig{TunnelDir: tunnelDir})
	if err == nil {
		t.Fatal("expected error when rotating a cert-manager tunnel")
	}
	if !strings.Contains(err.Error(), "cert-manager") {
		t.Errorf("error should mention cert-manager, got: %v", err)
	}
}
