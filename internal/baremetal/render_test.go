package baremetal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/portal/internal/manifest"
)

func TestRenderSingleService(t *testing.T) {
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	// Check initiator bootstrap config is non-empty and references the responder.
	if len(bundle.Initiator.EnvoyConfig) == 0 {
		t.Error("initiator EnvoyConfig is empty")
	}
	if !strings.Contains(string(bundle.Initiator.EnvoyConfig), "10.0.1.50") {
		t.Error("initiator EnvoyConfig should reference responder host")
	}

	// Check responder bootstrap config is non-empty.
	if len(bundle.Responder.EnvoyConfig) == 0 {
		t.Error("responder EnvoyConfig is empty")
	}

	// Check certs were generated.
	if bundle.Certs == nil {
		t.Fatal("expected generated certs, got nil")
	}
	if len(bundle.Initiator.CertFiles.Cert) == 0 {
		t.Error("initiator cert files are empty")
	}
	if len(bundle.Responder.CertFiles.Cert) == 0 {
		t.Error("responder cert files are empty")
	}

	// Check systemd units.
	if len(bundle.Initiator.SystemdUnit) == 0 {
		t.Error("initiator systemd unit is empty")
	}
	if !strings.Contains(string(bundle.Initiator.SystemdUnit), "Portal Initiator") {
		t.Error("initiator systemd unit should reference Portal Initiator")
	}
	if !strings.Contains(string(bundle.Responder.SystemdUnit), "Portal Responder") {
		t.Error("responder systemd unit should reference Portal Responder")
	}

	// Check docker-compose files.
	if len(bundle.Initiator.DockerCompose) == 0 {
		t.Error("initiator docker-compose is empty")
	}
	if !strings.Contains(string(bundle.Initiator.DockerCompose), "portal-initiator") {
		t.Error("initiator docker-compose should reference portal-initiator")
	}
	if !strings.Contains(string(bundle.Responder.DockerCompose), "portal-responder") {
		t.Error("responder docker-compose should reference portal-responder")
	}

	// Check metadata.
	if bundle.Metadata.DeployTarget != "bare-metal" {
		t.Errorf("metadata deploy target = %q, want %q", bundle.Metadata.DeployTarget, "bare-metal")
	}
	if bundle.Metadata.TunnelName != "source-host--dest-host" {
		t.Errorf("tunnel name = %q, want %q", bundle.Metadata.TunnelName, "source-host--dest-host")
	}
}

func TestRenderMultiService(t *testing.T) {
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

	// Check multi-service config in responder (should have tls_inspector).
	responder := string(bundle.Responder.EnvoyConfig)
	if !strings.Contains(responder, "tls_inspector") {
		t.Error("responder bootstrap should contain tls_inspector for multi-service")
	}
	if !strings.Contains(responder, "backend") {
		t.Error("responder bootstrap should reference 'backend' service")
	}
	if !strings.Contains(responder, "otel") {
		t.Error("responder bootstrap should reference 'otel' service")
	}
}

func TestRenderCustomEnvoyCommand(t *testing.T) {
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		EnvoyCommand:      "func-e run",
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	if !strings.Contains(string(bundle.Initiator.SystemdUnit), "func-e run") {
		t.Error("systemd unit should contain custom envoy command 'func-e run'")
	}
}

func TestRenderCustomPaths(t *testing.T) {
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		CertInstallPath:   "/opt/portal/certs",
		ConfigInstallPath: "/opt/portal",
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	if !strings.Contains(string(bundle.Initiator.SystemdUnit), "/opt/portal/envoy.yaml") {
		t.Error("systemd unit should use custom config path")
	}
	if !strings.Contains(string(bundle.Initiator.EnvoyConfig), "/opt/portal/certs") {
		t.Error("envoy config should use custom cert path")
	}
}

func TestRenderExternalCerts(t *testing.T) {
	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		ExternalCerts: &manifest.ExternalCertificates{
			CACert:        []byte("ca-cert-pem"),
			InitiatorCert: []byte("init-cert-pem"),
			InitiatorKey:  []byte("init-key-pem"),
			ResponderCert: []byte("resp-cert-pem"),
			ResponderKey:  []byte("resp-key-pem"),
		},
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	if bundle.Certs != nil {
		t.Error("expected Certs to be nil when using external certs")
	}
	if string(bundle.Initiator.CertFiles.Cert) != "init-cert-pem" {
		t.Errorf("initiator cert = %q, want %q", string(bundle.Initiator.CertFiles.Cert), "init-cert-pem")
	}
	if string(bundle.Responder.CertFiles.Cert) != "resp-cert-pem" {
		t.Errorf("responder cert = %q, want %q", string(bundle.Responder.CertFiles.Cert), "resp-cert-pem")
	}
}

func TestRenderCertDirs(t *testing.T) {
	// Create temp cert dirs.
	initDir := t.TempDir()
	respDir := t.TempDir()

	for _, dir := range []string{initDir, respDir} {
		os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("cert"), 0644)
		os.WriteFile(filepath.Join(dir, "tls.key"), []byte("key"), 0600)
		os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("ca"), 0644)
	}

	bundle, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		InitiatorCertDir:  initDir,
		ResponderCertDir:  respDir,
	})
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}

	if bundle.Certs != nil {
		t.Error("expected Certs to be nil when using cert dirs")
	}
	if string(bundle.Initiator.CertFiles.Cert) != "cert" {
		t.Errorf("initiator cert = %q, want %q", string(bundle.Initiator.CertFiles.Cert), "cert")
	}
}

func TestRenderInvalidEndpoint(t *testing.T) {
	_, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "not_valid:::",
	})
	if err == nil {
		t.Fatal("expected error for invalid endpoint")
	}
}

func TestRenderMissingCertDir(t *testing.T) {
	_, err := Render(BareMetalConfig{
		SourceHost:        "source-host",
		DestinationHost:   "dest-host",
		ResponderEndpoint: "10.0.1.50:10443",
		InitiatorCertDir:  "/nonexistent",
	})
	if err == nil {
		t.Fatal("expected error when only initiator-cert-dir is set")
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := BareMetalConfig{
		SourceHost:      "a",
		DestinationHost: "b",
	}
	applyDefaults(&cfg)

	if cfg.TunnelName != "a--b" {
		t.Errorf("TunnelName = %q, want %q", cfg.TunnelName, "a--b")
	}
	if cfg.TunnelPort != 10443 {
		t.Errorf("TunnelPort = %d, want %d", cfg.TunnelPort, 10443)
	}
	if cfg.EnvoyCommand != DefaultEnvoyCommand {
		t.Errorf("EnvoyCommand = %q, want %q", cfg.EnvoyCommand, DefaultEnvoyCommand)
	}
	if cfg.CertInstallPath != DefaultCertInstallPath {
		t.Errorf("CertInstallPath = %q, want %q", cfg.CertInstallPath, DefaultCertInstallPath)
	}
	if cfg.ConfigInstallPath != DefaultConfigInstallPath {
		t.Errorf("ConfigInstallPath = %q, want %q", cfg.ConfigInstallPath, DefaultConfigInstallPath)
	}
	if cfg.RunUser != DefaultRunUser {
		t.Errorf("RunUser = %q, want %q", cfg.RunUser, DefaultRunUser)
	}
}

func TestSystemdUnitContent(t *testing.T) {
	unit, err := renderSystemdUnit(systemdConfig{
		Description:   "Test Portal Initiator",
		UnitName:      "portal-initiator",
		EnvoyCommand:  "func-e run",
		ConfigPath:    "/etc/portal/envoy.yaml",
		EnvoyLogLevel: "debug",
		RunUser:       "testuser",
	})
	if err != nil {
		t.Fatalf("renderSystemdUnit() failed: %v", err)
	}

	content := string(unit)
	checks := []string{
		"Test Portal Initiator",
		"func-e run -c /etc/portal/envoy.yaml --log-level debug",
		"User=testuser",
		"Restart=always",
		"LimitNOFILE=65536",
		"WantedBy=multi-user.target",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("systemd unit should contain %q", check)
		}
	}
}

func TestDockerComposeContent(t *testing.T) {
	compose, err := renderDockerCompose(composeConfig{
		ServiceName:   "portal-responder",
		EnvoyImage:    "envoyproxy/envoy:v1.37-latest",
		EnvoyLogLevel: "info",
		ConfigPath:    "/etc/portal",
		CertPath:      "/etc/portal/certs",
		TunnelPort:    10443,
		IsInitiator:   false,
	})
	if err != nil {
		t.Fatalf("renderDockerCompose() failed: %v", err)
	}

	content := string(compose)
	checks := []string{
		"portal-responder",
		"envoyproxy/envoy:v1.37-latest",
		"10443:10443",
		"./envoy.yaml:/etc/envoy/envoy.yaml:ro",
		"./certs:/etc/portal/certs:ro",
		"unless-stopped",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("docker-compose should contain %q, got:\n%s", check, content)
		}
	}
}
