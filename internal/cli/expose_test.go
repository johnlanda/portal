package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tetratelabs/portal/internal/kube"
	"github.com/tetratelabs/portal/internal/state"
)

const (
	exposeSrc  = "src-cluster"
	exposeDst  = "dst-cluster"
	exposeNS   = "portal-system"
	exposePort = 10443
)

// setupExposeTestHooks swaps package-level hooks for expose testing and restores them on cleanup.
// Returns source and destination mock clients and the state store path.
// Tunnel contexts use "src-cluster" and "dst-cluster" so the mock router in
// newKubeClient (prefix "src" → srcMock, else → dstMock) works correctly.
func setupExposeTestHooks(t *testing.T) (srcMock, dstMock *mockKubeClient, storePath string) {
	t.Helper()

	srcMock = &mockKubeClient{}
	dstMock = &mockKubeClient{}

	origNewKubeClient := newKubeClient
	origCheckKubectl := checkKubectlFn
	origCheckContext := checkContextFn
	origNewStateStore := newStateStore

	newKubeClient = func(kubeContext, namespace string) kube.Client {
		if strings.HasPrefix(kubeContext, "src") {
			return srcMock
		}
		return dstMock
	}
	checkKubectlFn = func() error { return nil }
	checkContextFn = func(string) error { return nil }

	storePath = filepath.Join(t.TempDir(), "tunnels.json")
	newStateStore = func() (*state.Store, error) {
		return state.NewStore(storePath), nil
	}

	t.Cleanup(func() {
		newKubeClient = origNewKubeClient
		checkKubectlFn = origCheckKubectl
		checkContextFn = origCheckContext
		newStateStore = origNewStateStore
	})

	return srcMock, dstMock, storePath
}

// addExposeTunnel is a shorthand for adding the standard test tunnel.
func addExposeTunnel(t *testing.T, storePath string) {
	t.Helper()
	addTestTunnel(t, storePath, exposeSrc+"--"+exposeDst, exposeSrc, exposeDst, exposeNS, exposePort)
}

func TestExposeRequiresArgs(t *testing.T) {
	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	// Zero args.
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no positional args provided")
	}

	// One arg.
	cmd.SetArgs([]string{"src-cluster"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when only one positional arg provided")
	}
}

func TestExposeRequiresPort(t *testing.T) {
	setupExposeTestHooks(t)

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "my-api"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --port is not provided")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("error should mention 'port', got: %v", err)
	}
}

func TestExposeContextNotFound(t *testing.T) {
	_, _, storePath := setupExposeTestHooks(t)
	// No tunnel added — context won't match anything.
	_ = storePath

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"unknown-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when context is not in any tunnel")
	}
	if !strings.Contains(err.Error(), "no tunnel found") {
		t.Errorf("error should mention 'no tunnel found', got: %v", err)
	}
}

func TestExposeFromSourceContext(t *testing.T) {
	srcMock, dstMock, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	var buf strings.Builder
	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Service should be created in destination (dst-cluster).
	if dstMock.applyCalls != 1 {
		t.Errorf("destination Apply calls = %d, want 1", dstMock.applyCalls)
	}
	if srcMock.applyCalls != 0 {
		t.Errorf("source Apply calls = %d, want 0", srcMock.applyCalls)
	}

	// Source-to-destination is reverse direction: no config update or restart.
	if dstMock.rolloutRestartCalls != 0 {
		t.Errorf("destination rolloutRestartCalls = %d, want 0", dstMock.rolloutRestartCalls)
	}

	output := buf.String()
	if !strings.Contains(output, "Exposed my-api") {
		t.Errorf("output should contain 'Exposed my-api', got:\n%s", output)
	}
	if !strings.Contains(output, "Phase 2") {
		t.Errorf("output should mention Phase 2 for reverse direction, got:\n%s", output)
	}

	// Verify state was updated.
	store := state.NewStore(storePath)
	tunnel, err := store.Get("src-cluster--dst-cluster")
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	if len(tunnel.Services) != 1 || tunnel.Services[0] != "my-api:8080" {
		t.Errorf("Services = %v, want [my-api:8080]", tunnel.Services)
	}
}

func TestExposeFromDestinationContext(t *testing.T) {
	srcMock, dstMock, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	var buf strings.Builder
	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"dst-cluster", "backend-svc", "--port", "9090"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ClusterIP Service should be created in source (src-cluster): 1 Apply call.
	if srcMock.applyCalls != 1 {
		t.Errorf("source Apply calls = %d, want 1", srcMock.applyCalls)
	}

	// Destination-to-source is the natural direction: config update happens in destination.
	// updateResponderConfig creates its own client for dst-cluster → dstMock.
	// It calls Apply (ConfigMap) + RolloutRestart.
	if dstMock.applyCalls != 1 {
		t.Errorf("destination Apply calls = %d, want 1 (ConfigMap update)", dstMock.applyCalls)
	}
	if dstMock.rolloutRestartCalls != 1 {
		t.Errorf("destination rolloutRestartCalls = %d, want 1", dstMock.rolloutRestartCalls)
	}

	output := buf.String()
	if !strings.Contains(output, "Exposed backend-svc") {
		t.Errorf("output should contain 'Exposed backend-svc', got:\n%s", output)
	}
	if !strings.Contains(output, "Updated responder Envoy config") {
		t.Errorf("output should mention Envoy config update, got:\n%s", output)
	}
	if !strings.Contains(output, "src-cluster") {
		t.Errorf("output should mention source cluster, got:\n%s", output)
	}
}

func TestExposeDuplicateService(t *testing.T) {
	_, _, storePath := setupExposeTestHooks(t)

	// Pre-populate with a tunnel that already has the service.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "src-cluster--dst-cluster",
		SourceContext:      "src-cluster",
		DestinationContext: "dst-cluster",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		Mode:               "imperative",
		Services:           []string{"my-api:8080"},
	}); err != nil {
		t.Fatalf("failed to pre-populate state: %v", err)
	}

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for duplicate service")
	}
	if !strings.Contains(err.Error(), "already exposed") {
		t.Errorf("error should mention 'already exposed', got: %v", err)
	}
}

func TestExposeApplyError(t *testing.T) {
	_, dstMock, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	dstMock.applyFn = func(ctx context.Context, yamls [][]byte) error {
		return fmt.Errorf("connection refused")
	}

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on Apply failure")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should contain 'connection refused', got: %v", err)
	}
}

func TestExposeKubectlNotFound(t *testing.T) {
	origCheckKubectl := checkKubectlFn
	checkKubectlFn = func() error {
		return fmt.Errorf("kubectl not found in PATH")
	}
	t.Cleanup(func() { checkKubectlFn = origCheckKubectl })

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when kubectl not found")
	}
	if !strings.Contains(err.Error(), "kubectl not found") {
		t.Errorf("error should mention kubectl, got: %v", err)
	}
}

func TestExposeAppliedYAMLContent(t *testing.T) {
	_, dstMock, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	var capturedYAML []byte
	dstMock.applyFn = func(ctx context.Context, yamls [][]byte) error {
		if len(yamls) > 0 {
			capturedYAML = yamls[0]
		}
		return nil
	}

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&strings.Builder{})
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedYAML == nil {
		t.Fatal("no YAML was captured from Apply call")
	}

	// Parse the YAML and verify structure.
	var svc map[string]interface{}
	if err := yaml.Unmarshal(capturedYAML, &svc); err != nil {
		t.Fatalf("failed to parse captured YAML: %v", err)
	}

	// Verify kind.
	if kind, ok := svc["kind"].(string); !ok || kind != "Service" {
		t.Errorf("kind = %v, want Service", svc["kind"])
	}

	// Verify metadata.
	meta, ok := svc["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("metadata is not a map")
	}
	if name := meta["name"]; name != "portal-src-cluster-my-api" {
		t.Errorf("metadata.name = %v, want portal-src-cluster-my-api", name)
	}
	if ns := meta["namespace"]; ns != "portal-system" {
		t.Errorf("metadata.namespace = %v, want portal-system", ns)
	}

	// Verify spec.
	spec, ok := svc["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec is not a map")
	}
	if svcType := spec["type"]; svcType != "ClusterIP" {
		t.Errorf("spec.type = %v, want ClusterIP", svcType)
	}

	// Verify selector targets responder (since we're exposing from source).
	selector, ok := spec["selector"].(map[string]interface{})
	if !ok {
		t.Fatal("spec.selector is not a map")
	}
	if sel := selector["app.kubernetes.io/name"]; sel != "portal-responder" {
		t.Errorf("selector = %v, want portal-responder", sel)
	}

	// Verify ports.
	ports, ok := spec["ports"].([]interface{})
	if !ok || len(ports) != 1 {
		t.Fatalf("spec.ports length = %v, want 1", len(ports))
	}
	port, ok := ports[0].(map[string]interface{})
	if !ok {
		t.Fatal("port entry is not a map")
	}
	if port["name"] != "my-api" {
		t.Errorf("port.name = %v, want my-api", port["name"])
	}
	// yaml.v3 unmarshals integers as int.
	if portVal, ok := port["port"].(int); !ok || portVal != 8080 {
		t.Errorf("port.port = %v, want 8080", port["port"])
	}
	if targetPort, ok := port["targetPort"].(int); !ok || targetPort != 10443 {
		t.Errorf("port.targetPort = %v, want 10443", port["targetPort"])
	}
}

func TestExposeConfigMapUpdate(t *testing.T) {
	_, dstMock, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	// Capture ConfigMap YAML applied to destination during config update.
	var configMapYAML []byte
	dstMock.applyFn = func(ctx context.Context, yamls [][]byte) error {
		if len(yamls) > 0 {
			configMapYAML = yamls[0]
		}
		return nil
	}

	var buf strings.Builder
	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"dst-cluster", "my-api", "--port", "8080", "--service-namespace", "apps"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if configMapYAML == nil {
		t.Fatal("no ConfigMap YAML was captured")
	}

	// Parse the ConfigMap.
	var cm map[string]interface{}
	if err := yaml.Unmarshal(configMapYAML, &cm); err != nil {
		t.Fatalf("failed to parse ConfigMap YAML: %v", err)
	}

	if cm["kind"] != "ConfigMap" {
		t.Errorf("kind = %v, want ConfigMap", cm["kind"])
	}

	meta, ok := cm["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("metadata is not a map")
	}
	if meta["name"] != "portal-responder-bootstrap" {
		t.Errorf("name = %v, want portal-responder-bootstrap", meta["name"])
	}

	data, ok := cm["data"].(map[string]interface{})
	if !ok {
		t.Fatal("data is not a map")
	}
	envoyYAML, ok := data["envoy.yaml"].(string)
	if !ok {
		t.Fatal("data.envoy.yaml is not a string")
	}

	// Verify the bootstrap references the service backend.
	if !strings.Contains(envoyYAML, "my-api.apps.svc") {
		t.Errorf("bootstrap should contain service FQDN 'my-api.apps.svc', got:\n%s", envoyYAML)
	}
	if !strings.Contains(envoyYAML, "port_value: 8080") {
		t.Errorf("bootstrap should contain 'port_value: 8080', got:\n%s", envoyYAML)
	}
}

func TestExposeRolloutRestartError(t *testing.T) {
	_, dstMock, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	dstMock.rolloutRestartFn = func(ctx context.Context, deployment string) error {
		return fmt.Errorf("rollout restart failed")
	}

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"dst-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on rollout restart failure")
	}
	if !strings.Contains(err.Error(), "rollout restart failed") {
		t.Errorf("error should contain 'rollout restart failed', got: %v", err)
	}
}

func TestExposeMultipleTunnelsAmbiguity(t *testing.T) {
	_, _, storePath := setupExposeTestHooks(t)

	// Create two tunnels that both reference src-cluster.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name: "src-cluster--dst-cluster", SourceContext: "src-cluster", DestinationContext: "dst-cluster",
		Namespace: "portal-system", TunnelPort: 10443, Mode: "imperative",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(state.TunnelState{
		Name: "src-cluster--dst-cluster-2", SourceContext: "src-cluster", DestinationContext: "dst-cluster-2",
		Namespace: "portal-system", TunnelPort: 10443, Mode: "imperative",
	}); err != nil {
		t.Fatal(err)
	}

	// Without --tunnel flag, should error on ambiguity.
	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for ambiguous context")
	}
	if !strings.Contains(err.Error(), "multiple tunnels") {
		t.Errorf("error should mention 'multiple tunnels', got: %v", err)
	}
}

func TestExposeTunnelFlagDisambiguates(t *testing.T) {
	_, dstMock, storePath := setupExposeTestHooks(t)

	// Create two tunnels that both reference src-cluster.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name: "src-cluster--dst-cluster", SourceContext: "src-cluster", DestinationContext: "dst-cluster",
		Namespace: "portal-system", TunnelPort: 10443, Mode: "imperative",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(state.TunnelState{
		Name: "src-cluster--dst-cluster-2", SourceContext: "src-cluster", DestinationContext: "dst-cluster-2",
		Namespace: "portal-system", TunnelPort: 10443, Mode: "imperative",
	}); err != nil {
		t.Fatal(err)
	}

	// With --tunnel flag, should succeed.
	var buf strings.Builder
	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080", "--tunnel", "src-cluster--dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should apply to dst-cluster (since we selected the first tunnel).
	if dstMock.applyCalls != 1 {
		t.Errorf("destination Apply calls = %d, want 1", dstMock.applyCalls)
	}

	output := buf.String()
	if !strings.Contains(output, "Exposed my-api") {
		t.Errorf("output should contain 'Exposed my-api', got:\n%s", output)
	}
}

func TestExposeInvalidContext(t *testing.T) {
	setupExposeTestHooks(t)

	origCheckContext := checkContextFn
	t.Cleanup(func() { checkContextFn = origCheckContext })
	checkContextFn = func(ctx string) error {
		return fmt.Errorf("kube context %q not found in kubeconfig", ctx)
	}

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"bad-context", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid context")
	}
	if !strings.Contains(err.Error(), "not found in kubeconfig") {
		t.Errorf("error should mention 'not found in kubeconfig', got: %v", err)
	}
}

func TestExposeServiceNotFound(t *testing.T) {
	srcMock, _, storePath := setupExposeTestHooks(t)
	addExposeTunnel(t, storePath)

	// Mock GetService to return not-found error.
	srcMock.getServiceFn = func(ctx context.Context, name string) (*kube.ServiceInfo, error) {
		return nil, fmt.Errorf("service %q not found", name)
	}

	cmd := NewExposeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "my-api", "--port", "8080"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent service")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "my-api") {
		t.Errorf("error should mention service name, got: %v", err)
	}

	// No Apply should have been called.
	if srcMock.applyCalls != 0 {
		t.Errorf("source Apply calls = %d, want 0", srcMock.applyCalls)
	}
}
