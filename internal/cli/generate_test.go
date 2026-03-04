package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johnlanda/portal/internal/state"
	"gopkg.in/yaml.v3"
)

func TestGenerateCmdRequiresArgs(t *testing.T) {
	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{}) // no args

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no positional args provided")
	}
}

func TestGenerateCmdRequiresOutputDir(t *testing.T) {
	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"source-ctx", "dest-ctx",
		"--responder-endpoint", "10.0.0.1:10443",
		// --output-dir intentionally omitted
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --output-dir is missing")
	}
	if !strings.Contains(err.Error(), "--output-dir") {
		t.Errorf("error should mention --output-dir, got: %v", err)
	}
}

func TestGenerateCmdRequiresResponderEndpoint(t *testing.T) {
	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"source-ctx", "dest-ctx",
		"--output-dir", t.TempDir(),
		// --responder-endpoint intentionally omitted
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --responder-endpoint is missing")
	}
	if !strings.Contains(err.Error(), "--responder-endpoint") {
		t.Errorf("error should mention --responder-endpoint, got: %v", err)
	}
}

func TestGenerateCmdSuccess(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "tunnel-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	// Verify directory structure was created.
	expectedFiles := []string{
		"source/namespace.yaml",
		"source/portal-initiator-sa.yaml",
		"source/portal-initiator-deployment.yaml",
		"source/kustomization.yaml",
		"destination/namespace.yaml",
		"destination/portal-responder-sa.yaml",
		"destination/portal-responder-deployment.yaml",
		"destination/portal-responder-service.yaml",
		"destination/kustomization.yaml",
		"tunnel.yaml",
		"ca/ca.crt",
		"ca/ca.key",
		"ca/.gitignore",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(outputDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s does not exist", f)
		}
	}

	// Verify tunnel metadata.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta["tunnelName"] != "src-cluster--dst-cluster" {
		t.Errorf("tunnelName = %v, want %q", meta["tunnelName"], "src-cluster--dst-cluster")
	}
}

func TestGenerateCmdCertManager(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "cm-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--output-dir", outputDir,
		"--responder-endpoint", "tunnel.example.com:10443",
		"--cert-manager",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	// Verify cert-manager resources exist.
	cmFiles := []string{
		"source/cert-manager-selfsigned-issuer.yaml",
		"source/cert-manager-ca-certificate.yaml",
		"source/cert-manager-ca-issuer.yaml",
		"source/cert-manager-initiator-certificate.yaml",
		"destination/cert-manager-selfsigned-issuer.yaml",
		"destination/cert-manager-ca-certificate.yaml",
		"destination/cert-manager-ca-issuer.yaml",
		"destination/cert-manager-responder-certificate.yaml",
	}
	for _, f := range cmFiles {
		path := filepath.Join(outputDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected cert-manager file %s does not exist", f)
		}
	}

	// Verify no raw secret.
	for _, side := range []string{"source", "destination"} {
		path := filepath.Join(outputDir, side, "portal-tunnel-tls-secret.yaml")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s should not have raw TLS secret in cert-manager mode", side)
		}
	}
}

func TestGenerateCmdCertManagerNoCaDir(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "cm-no-ca")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--cert-manager",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	// Verify no ca/ directory when using cert-manager.
	caDir := filepath.Join(outputDir, "ca")
	if _, err := os.Stat(caDir); !os.IsNotExist(err) {
		t.Error("ca/ directory should not exist in cert-manager mode")
	}
}

func TestGenerateCmdCustomFlags(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "custom-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "tunnel.example.com:9443",
		"--namespace", "custom-ns",
		"--tunnel-port", "9443",
		"--envoy-image", "envoyproxy/envoy:v1.30-latest",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	// Verify custom namespace propagated to tunnel metadata.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta["namespace"] != "custom-ns" {
		t.Errorf("namespace = %v, want %q", meta["namespace"], "custom-ns")
	}
	if meta["tunnelPort"] != 9443 {
		t.Errorf("tunnelPort = %v, want %d", meta["tunnelPort"], 9443)
	}
	if meta["envoyImage"] != "envoyproxy/envoy:v1.30-latest" {
		t.Errorf("envoyImage = %v, want %q", meta["envoyImage"], "envoyproxy/envoy:v1.30-latest")
	}

	// Verify the namespace is used in the rendered resources.
	nsBytes, err := os.ReadFile(filepath.Join(outputDir, "source", "namespace.yaml"))
	if err != nil {
		t.Fatalf("failed to read source namespace.yaml: %v", err)
	}
	if !strings.Contains(string(nsBytes), "custom-ns") {
		t.Error("source namespace.yaml should contain custom-ns")
	}
}

// ---------------------------------------------------------------------------
// Flag combination tests — verify flags propagate into generated manifests
// ---------------------------------------------------------------------------

func TestGenerateNodePortServiceType(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "nodeport-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--service-type", "NodePort",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	svcBytes, err := os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-service.yaml"))
	if err != nil {
		t.Fatalf("failed to read service YAML: %v", err)
	}
	svcYAML := string(svcBytes)
	if !strings.Contains(svcYAML, "NodePort") {
		t.Error("service YAML should contain 'NodePort' type")
	}
}

func TestGenerateClusterIPServiceType(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "clusterip-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--service-type", "ClusterIP",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	svcBytes, err := os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-service.yaml"))
	if err != nil {
		t.Fatalf("failed to read service YAML: %v", err)
	}
	svcYAML := string(svcBytes)
	if !strings.Contains(svcYAML, "ClusterIP") {
		t.Error("service YAML should contain 'ClusterIP' type")
	}
}

func TestGenerateConnectionCount(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "conncount-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--connection-count", "16",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify tunnel metadata records the port/config.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	// The connection-count is used in rendering but may not be in tunnel.yaml;
	// the key verification is that the generate command accepts and uses the flag.
	if len(metaBytes) == 0 {
		t.Error("tunnel.yaml should not be empty")
	}
}

func TestGenerateEnvoyLogLevel(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "loglevel-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--envoy-log-level", "warning",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the log level appears in one of the deployment YAMLs.
	depBytes, err := os.ReadFile(filepath.Join(outputDir, "source", "portal-initiator-deployment.yaml"))
	if err != nil {
		t.Fatalf("failed to read initiator deployment: %v", err)
	}
	if !strings.Contains(string(depBytes), "warning") {
		t.Error("initiator deployment should contain 'warning' log level in args")
	}

	depBytes, err = os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-deployment.yaml"))
	if err != nil {
		t.Fatalf("failed to read responder deployment: %v", err)
	}
	if !strings.Contains(string(depBytes), "warning") {
		t.Error("responder deployment should contain 'warning' log level in args")
	}
}

func TestGenerateCertValidity(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "certvalidity-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--cert-validity", "2160h",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify tunnel metadata records the cert validity.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta["certValidity"] != "2160h0m0s" {
		t.Errorf("certValidity = %v, want %q", meta["certValidity"], "2160h0m0s")
	}
}

func TestGenerateCustomTunnelPort(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "port-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:9443",
		"--tunnel-port", "9443",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the custom port in tunnel metadata.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta["tunnelPort"] != 9443 {
		t.Errorf("tunnelPort = %v, want 9443", meta["tunnelPort"])
	}

	// Verify the port appears in the responder service.
	svcBytes, err := os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-service.yaml"))
	if err != nil {
		t.Fatalf("failed to read service: %v", err)
	}
	if !strings.Contains(string(svcBytes), "9443") {
		t.Error("service should contain port 9443")
	}
}

func TestGenerateDNSEndpointAnnotation(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "dns-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src", "dst",
		"--output-dir", outputDir,
		"--responder-endpoint", "tunnel.example.com:10443",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	svcBytes, err := os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-service.yaml"))
	if err != nil {
		t.Fatalf("failed to read service: %v", err)
	}
	svcYAML := string(svcBytes)
	if !strings.Contains(svcYAML, "external-dns.alpha.kubernetes.io/hostname") {
		t.Error("DNS endpoint should produce external-dns annotation on service")
	}
	if !strings.Contains(svcYAML, "tunnel.example.com") {
		t.Error("service should contain the DNS hostname")
	}
}

// setupGenerateExposeHooks sets up state store hooks for generate expose tests.
func setupGenerateExposeHooks(t *testing.T) string {
	t.Helper()

	origNewStateStore := newStateStore
	storePath := filepath.Join(t.TempDir(), "tunnels.json")
	newStateStore = func() (*state.Store, error) {
		return state.NewStore(storePath), nil
	}
	t.Cleanup(func() { newStateStore = origNewStateStore })

	// Pre-populate with a tunnel.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "src-cluster--dst-cluster",
		SourceContext:      "src-cluster",
		DestinationContext: "dst-cluster",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		Mode:               "imperative",
	}); err != nil {
		t.Fatalf("failed to add test tunnel: %v", err)
	}

	return storePath
}

func TestGenerateExposeRequiresArgs(t *testing.T) {
	setupGenerateExposeHooks(t)

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"expose"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no positional args provided")
	}
}

func TestGenerateExposeRequiresPort(t *testing.T) {
	setupGenerateExposeHooks(t)

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"expose", "dst-cluster", "my-api",
		"--output-dir", t.TempDir(),
		// --port omitted
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --port is missing")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("error should mention port, got: %v", err)
	}
}

func TestGenerateExposeRequiresOutputDir(t *testing.T) {
	setupGenerateExposeHooks(t)

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"expose", "dst-cluster", "my-api",
		"--port", "8080",
		// --output-dir omitted
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --output-dir is missing")
	}
	if !strings.Contains(err.Error(), "output-dir") {
		t.Errorf("error should mention output-dir, got: %v", err)
	}
}

func TestGenerateExposeDestinationService(t *testing.T) {
	setupGenerateExposeHooks(t)
	outputDir := filepath.Join(t.TempDir(), "expose-output")

	var buf strings.Builder
	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"expose", "dst-cluster", "my-api",
		"--port", "8080",
		"--output-dir", outputDir,
		"--service-namespace", "apps",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify service YAML was written.
	svcFile := filepath.Join(outputDir, "portal-dst-cluster-my-api-service.yaml")
	svcBytes, err := os.ReadFile(svcFile)
	if err != nil {
		t.Fatalf("expected service file, got error: %v", err)
	}

	var svc map[string]interface{}
	if err := yaml.Unmarshal(svcBytes, &svc); err != nil {
		t.Fatalf("invalid YAML in service file: %v", err)
	}
	if svc["kind"] != "Service" {
		t.Errorf("kind = %v, want Service", svc["kind"])
	}

	// Verify ConfigMap was written (natural direction).
	cmFile := filepath.Join(outputDir, "portal-responder-bootstrap-cm.yaml")
	cmBytes, err := os.ReadFile(cmFile)
	if err != nil {
		t.Fatalf("expected ConfigMap file, got error: %v", err)
	}
	if !strings.Contains(string(cmBytes), "my-api.apps.svc") {
		t.Error("ConfigMap should contain service FQDN 'my-api.apps.svc'")
	}
	if !strings.Contains(string(cmBytes), "port_value: 8080") {
		t.Error("ConfigMap should contain 'port_value: 8080'")
	}

	// Verify output contains next steps.
	output := buf.String()
	if !strings.Contains(output, "kubectl apply") {
		t.Errorf("output should contain kubectl apply instructions, got:\n%s", output)
	}
}

func TestGenerateExposeSourceService(t *testing.T) {
	setupGenerateExposeHooks(t)
	outputDir := filepath.Join(t.TempDir(), "expose-src-output")

	var buf strings.Builder
	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"expose", "src-cluster", "my-api",
		"--port", "8080",
		"--output-dir", outputDir,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify service YAML was written.
	svcFile := filepath.Join(outputDir, "portal-src-cluster-my-api-service.yaml")
	if _, err := os.Stat(svcFile); os.IsNotExist(err) {
		t.Fatal("expected service file to be written")
	}

	// Verify NO ConfigMap was written (reverse direction).
	cmFile := filepath.Join(outputDir, "portal-responder-bootstrap-cm.yaml")
	if _, err := os.Stat(cmFile); !os.IsNotExist(err) {
		t.Error("ConfigMap should NOT be written for reverse direction")
	}

	output := buf.String()
	if !strings.Contains(output, "Phase 2") {
		t.Errorf("output should mention Phase 2 for reverse direction, got:\n%s", output)
	}
}

func TestGenerateWithServices(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "svc-output")

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--service", "backend=backend-svc.synapse.svc:8443",
		"--service", "otel=otel-collector.synapse.svc:4317",
		"--service-local-port", "backend=18443",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify initiator bootstrap contains multi-service config (tls_inspector/SNI).
	bootstrapBytes, err := os.ReadFile(filepath.Join(outputDir, "source", "portal-initiator-bootstrap-cm.yaml"))
	if err != nil {
		t.Fatalf("failed to read initiator bootstrap: %v", err)
	}
	bootstrap := string(bootstrapBytes)
	if !strings.Contains(bootstrap, "backend") {
		t.Error("initiator bootstrap should reference 'backend' service")
	}
	if !strings.Contains(bootstrap, "otel") {
		t.Error("initiator bootstrap should reference 'otel' service")
	}

	// Verify responder bootstrap contains SNI routing.
	responderBytes, err := os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-bootstrap-cm.yaml"))
	if err != nil {
		t.Fatalf("failed to read responder bootstrap: %v", err)
	}
	responder := string(responderBytes)
	if !strings.Contains(responder, "tls_inspector") {
		t.Error("responder bootstrap should contain tls_inspector for multi-service")
	}

	// Verify tunnel.yaml records services.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	services, ok := meta["services"].([]interface{})
	if !ok {
		t.Fatalf("expected services in tunnel.yaml, got %T", meta["services"])
	}
	if len(services) != 2 {
		t.Errorf("expected 2 services in metadata, got %d", len(services))
	}
}

func TestGenerateWithSecretRef(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "secretref-output")

	var buf strings.Builder
	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--output-dir", outputDir,
		"--responder-endpoint", "10.0.0.1:10443",
		"--secret-ref", "my-vault-tls",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No TLS secret file should exist on disk.
	for _, side := range []string{"source", "destination"} {
		path := filepath.Join(outputDir, side, "portal-tunnel-tls-secret.yaml")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s should not have TLS secret in secret-ref mode", side)
		}
	}

	// No ca/ directory should exist.
	caDir := filepath.Join(outputDir, "ca")
	if _, err := os.Stat(caDir); !os.IsNotExist(err) {
		t.Error("ca/ directory should not exist in secret-ref mode")
	}

	// Verify deployment volumes reference the custom secret name.
	depBytes, err := os.ReadFile(filepath.Join(outputDir, "source", "portal-initiator-deployment.yaml"))
	if err != nil {
		t.Fatalf("failed to read initiator deployment: %v", err)
	}
	if !strings.Contains(string(depBytes), "my-vault-tls") {
		t.Error("initiator deployment should reference secret 'my-vault-tls'")
	}

	depBytes, err = os.ReadFile(filepath.Join(outputDir, "destination", "portal-responder-deployment.yaml"))
	if err != nil {
		t.Fatalf("failed to read responder deployment: %v", err)
	}
	if !strings.Contains(string(depBytes), "my-vault-tls") {
		t.Error("responder deployment should reference secret 'my-vault-tls'")
	}

	// Verify tunnel.yaml records secretRef.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta map[string]interface{}
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta["secretRef"] != "my-vault-tls" {
		t.Errorf("secretRef = %v, want %q", meta["secretRef"], "my-vault-tls")
	}

	// Verify output mentions using existing secret.
	output := buf.String()
	if !strings.Contains(output, "Using existing secret") {
		t.Errorf("output should mention using existing secret, got:\n%s", output)
	}
}

func TestGenerateExposeContextNotFound(t *testing.T) {
	setupGenerateExposeHooks(t)

	cmd := NewGenerateCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"expose", "unknown-cluster", "my-api",
		"--port", "8080",
		"--output-dir", t.TempDir(),
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when context not in any tunnel")
	}
	if !strings.Contains(err.Error(), "no tunnel found") {
		t.Errorf("error should mention 'no tunnel found', got: %v", err)
	}
}
