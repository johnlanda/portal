package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/portal/internal/kube"
	"github.com/tetratelabs/portal/internal/state"
)

// mockKubeClient implements kube.Client for testing.
type mockKubeClient struct {
	applyFn              func(ctx context.Context, yamls [][]byte) error
	deleteFn             func(ctx context.Context, yamls [][]byte) error
	waitForDeploymentFn  func(ctx context.Context, name string, timeout time.Duration) error
	waitForServiceAddrFn func(ctx context.Context, name string, timeout time.Duration) (string, error)
	portForwardFn        func(ctx context.Context, target string, localPort, remotePort int) (*kube.PortForwardSession, error)
	getPodsFn            func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error)
	getServiceFn         func(ctx context.Context, name string) (*kube.ServiceInfo, error)
	rolloutRestartFn     func(ctx context.Context, deployment string) error

	applyCalls          int
	rolloutRestartCalls int
}

func (m *mockKubeClient) Apply(ctx context.Context, yamls [][]byte) error {
	m.applyCalls++
	if m.applyFn != nil {
		return m.applyFn(ctx, yamls)
	}
	return nil
}

func (m *mockKubeClient) Delete(ctx context.Context, yamls [][]byte) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, yamls)
	}
	return nil
}

func (m *mockKubeClient) WaitForDeployment(ctx context.Context, name string, timeout time.Duration) error {
	if m.waitForDeploymentFn != nil {
		return m.waitForDeploymentFn(ctx, name, timeout)
	}
	return nil
}

func (m *mockKubeClient) WaitForServiceAddress(ctx context.Context, name string, timeout time.Duration) (string, error) {
	if m.waitForServiceAddrFn != nil {
		return m.waitForServiceAddrFn(ctx, name, timeout)
	}
	return "10.0.0.1", nil
}

func (m *mockKubeClient) PortForward(ctx context.Context, target string, localPort, remotePort int) (*kube.PortForwardSession, error) {
	if m.portForwardFn != nil {
		return m.portForwardFn(ctx, target, localPort, remotePort)
	}
	return nil, nil
}

func (m *mockKubeClient) GetPods(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
	if m.getPodsFn != nil {
		return m.getPodsFn(ctx, labelSelector)
	}
	return nil, nil
}

func (m *mockKubeClient) GetService(ctx context.Context, name string) (*kube.ServiceInfo, error) {
	if m.getServiceFn != nil {
		return m.getServiceFn(ctx, name)
	}
	return nil, nil
}

func (m *mockKubeClient) RolloutRestart(ctx context.Context, deployment string) error {
	m.rolloutRestartCalls++
	if m.rolloutRestartFn != nil {
		return m.rolloutRestartFn(ctx, deployment)
	}
	return nil
}

// setupTestHooks swaps the package-level hooks for testing and restores them on cleanup.
// Returns the source and destination mock clients and the state store path.
func setupTestHooks(t *testing.T) (srcMock, dstMock *mockKubeClient, storePath string) {
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

func TestConnectRequiresArgs(t *testing.T) {
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no positional args provided")
	}
}

func TestConnectWithEndpoint(t *testing.T) {
	srcMock, dstMock, _ := setupTestHooks(t)

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Destination should have Apply called once (single-phase).
	if dstMock.applyCalls != 1 {
		t.Errorf("destination Apply calls = %d, want 1", dstMock.applyCalls)
	}
	// Source should have Apply called once.
	if srcMock.applyCalls != 1 {
		t.Errorf("source Apply calls = %d, want 1", srcMock.applyCalls)
	}

	output := buf.String()
	if !strings.Contains(output, "Tunnel established") {
		t.Errorf("output should contain 'Tunnel established', got:\n%s", output)
	}
	if !strings.Contains(output, "Connected") {
		t.Errorf("output should contain 'Connected', got:\n%s", output)
	}
}

func TestConnectWithoutEndpoint(t *testing.T) {
	srcMock, dstMock, _ := setupTestHooks(t)

	dstMock.waitForServiceAddrFn = func(ctx context.Context, name string, timeout time.Duration) (string, error) {
		return "34.120.1.50", nil
	}

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Two-phase: destination Apply called twice (phase 1 + phase 2).
	if dstMock.applyCalls != 2 {
		t.Errorf("destination Apply calls = %d, want 2", dstMock.applyCalls)
	}
	// Source Apply called once (after phase 2).
	if srcMock.applyCalls != 1 {
		t.Errorf("source Apply calls = %d, want 1", srcMock.applyCalls)
	}
}

func TestConnectApplyError(t *testing.T) {
	_, dstMock, _ := setupTestHooks(t)

	dstMock.applyFn = func(ctx context.Context, yamls [][]byte) error {
		return fmt.Errorf("connection refused")
	}

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on Apply failure")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should contain 'connection refused', got: %v", err)
	}
}

func TestConnectLBTimeout(t *testing.T) {
	_, dstMock, _ := setupTestHooks(t)

	dstMock.waitForServiceAddrFn = func(ctx context.Context, name string, timeout time.Duration) (string, error) {
		return "", fmt.Errorf("timed out waiting for LoadBalancer")
	}

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		// No --responder-endpoint triggers two-phase.
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on LB timeout")
	}
	if !strings.Contains(err.Error(), "LoadBalancer") {
		t.Errorf("error should mention LoadBalancer, got: %v", err)
	}
}

func TestConnectDeploymentWaitError(t *testing.T) {
	_, dstMock, _ := setupTestHooks(t)

	dstMock.waitForDeploymentFn = func(ctx context.Context, name string, timeout time.Duration) error {
		return fmt.Errorf("deployment timed out")
	}

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on deployment wait failure")
	}
	if !strings.Contains(err.Error(), "deployment timed out") {
		t.Errorf("error should contain 'deployment timed out', got: %v", err)
	}
}

func TestConnectDuplicateTunnel(t *testing.T) {
	_, _, storePath := setupTestHooks(t)

	// Pre-populate state with an existing tunnel.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name: "src-cluster--dst-cluster",
		Mode: "imperative",
	}); err != nil {
		t.Fatalf("failed to pre-populate state: %v", err)
	}

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for duplicate tunnel")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
}

func TestConnectSavesState(t *testing.T) {
	_, _, storePath := setupTestHooks(t)

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--namespace", "custom-ns",
		"--tunnel-port", "9443",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify state was saved.
	store := state.NewStore(storePath)
	tunnels, err := store.List()
	if err != nil {
		t.Fatalf("failed to load state: %v", err)
	}
	if len(tunnels) != 1 {
		t.Fatalf("expected 1 tunnel in state, got %d", len(tunnels))
	}
	ts := tunnels[0]
	if ts.Name != "src-cluster--dst-cluster" {
		t.Errorf("Name = %q, want %q", ts.Name, "src-cluster--dst-cluster")
	}
	if ts.SourceContext != "src-cluster" {
		t.Errorf("SourceContext = %q, want %q", ts.SourceContext, "src-cluster")
	}
	if ts.DestinationContext != "dst-cluster" {
		t.Errorf("DestinationContext = %q, want %q", ts.DestinationContext, "dst-cluster")
	}
	if ts.Namespace != "custom-ns" {
		t.Errorf("Namespace = %q, want %q", ts.Namespace, "custom-ns")
	}
	if ts.TunnelPort != 9443 {
		t.Errorf("TunnelPort = %d, want %d", ts.TunnelPort, 9443)
	}
	if ts.Mode != "imperative" {
		t.Errorf("Mode = %q, want %q", ts.Mode, "imperative")
	}
}

func TestConnectKubectlNotFound(t *testing.T) {
	origCheckKubectl := checkKubectlFn
	checkKubectlFn = func() error {
		return fmt.Errorf("kubectl not found in PATH")
	}
	t.Cleanup(func() { checkKubectlFn = origCheckKubectl })

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when kubectl not found")
	}
	if !strings.Contains(err.Error(), "kubectl not found") {
		t.Errorf("error should mention kubectl, got: %v", err)
	}
}

func TestConnectDryRun(t *testing.T) {
	srcMock, dstMock, _ := setupTestHooks(t)

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--dry-run",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No Apply calls should happen in dry-run mode.
	if srcMock.applyCalls != 0 {
		t.Errorf("source Apply calls = %d, want 0 in dry-run", srcMock.applyCalls)
	}
	if dstMock.applyCalls != 0 {
		t.Errorf("destination Apply calls = %d, want 0 in dry-run", dstMock.applyCalls)
	}

	output := buf.String()
	if !strings.Contains(output, "# Source (initiator) cluster resources") {
		t.Error("dry-run output should contain source header")
	}
	if !strings.Contains(output, "# Destination (responder) cluster resources") {
		t.Error("dry-run output should contain destination header")
	}
	if !strings.Contains(output, "kind: Namespace") {
		t.Error("dry-run output should contain rendered manifests")
	}
}

func TestConnectDryRunRequiresEndpoint(t *testing.T) {
	setupTestHooks(t)

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--dry-run",
		// No --responder-endpoint
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for dry-run without endpoint")
	}
	if !strings.Contains(err.Error(), "responder-endpoint") {
		t.Errorf("error should mention responder-endpoint, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Flag combination tests — verify flags propagate to rendered manifests
// ---------------------------------------------------------------------------

func TestConnectCustomConnectionCount(t *testing.T) {
	_, _, _ = setupTestHooks(t)

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--connection-count", "8",
		"--dry-run",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Dry-run output should contain rendered manifests — the connection count
	// is used by the Envoy bootstrap template, so verify the YAML is valid.
	output := buf.String()
	if !strings.Contains(output, "kind: Deployment") {
		t.Error("dry-run output should contain deployment manifests")
	}
}

func TestConnectEnvoyLogLevel(t *testing.T) {
	_, _, _ = setupTestHooks(t)

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--envoy-log-level", "debug",
		"--dry-run",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "debug") {
		t.Error("dry-run output should contain 'debug' log level in Envoy args")
	}
}

func TestConnectCustomEnvoyImage(t *testing.T) {
	_, _, _ = setupTestHooks(t)

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--envoy-image", "envoyproxy/envoy:v1.30-latest",
		"--dry-run",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "envoyproxy/envoy:v1.30-latest") {
		t.Error("dry-run output should contain the custom Envoy image")
	}
}

func TestConnectCertManagerDryRun(t *testing.T) {
	_, _, _ = setupTestHooks(t)

	var buf strings.Builder
	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--cert-manager",
		"--dry-run",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	// cert-manager mode should produce Issuer/Certificate CRDs, not raw Secrets.
	if !strings.Contains(output, "kind: Issuer") && !strings.Contains(output, "kind: Certificate") {
		t.Error("cert-manager dry-run should contain cert-manager CRDs")
	}
	// Should NOT contain raw TLS secret.
	if strings.Contains(output, "portal-tunnel-tls") && strings.Contains(output, "kind: Secret") {
		t.Error("cert-manager mode should not render raw TLS Secrets")
	}
}

func TestConnectCustomCertValidity(t *testing.T) {
	_, _, _ = setupTestHooks(t)

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&strings.Builder{})
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--responder-endpoint", "10.0.0.1:10443",
		"--cert-validity", "720h",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Success means the flag parsed correctly and certs were generated
	// with the custom validity. The cert expiry is validated in certs_test.go.
}

func TestConnectInvalidContext(t *testing.T) {
	setupTestHooks(t)

	// Override checkContextFn to reject unknown contexts.
	origCheckContext := checkContextFn
	t.Cleanup(func() { checkContextFn = origCheckContext })
	checkContextFn = func(ctx string) error {
		if ctx == "bad-context" {
			return fmt.Errorf("kube context %q not found in kubeconfig", ctx)
		}
		return nil
	}

	cmd := NewConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{
		"src-cluster", "bad-context",
		"--responder-endpoint", "10.0.0.1:10443",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid context")
	}
	if !strings.Contains(err.Error(), "bad-context") {
		t.Errorf("error should mention 'bad-context', got: %v", err)
	}
	if !strings.Contains(err.Error(), "not found in kubeconfig") {
		t.Errorf("error should mention 'not found in kubeconfig', got: %v", err)
	}
}
