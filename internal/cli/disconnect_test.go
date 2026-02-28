package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/portal/internal/kube"
	"github.com/tetratelabs/portal/internal/state"
)

func setupDisconnectTestHooks(t *testing.T) (srcMock, dstMock *mockKubeClient, storePath string) {
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

// addTestTunnel pre-populates the state store with a tunnel.
func addTestTunnel(t *testing.T, storePath, name, src, dst, ns string, port int) {
	t.Helper()
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               name,
		SourceContext:      src,
		DestinationContext: dst,
		Namespace:          ns,
		TunnelPort:         port,
		CreatedAt:          time.Now(),
		Mode:               "imperative",
	}); err != nil {
		t.Fatalf("failed to add test tunnel: %v", err)
	}
}

func TestDisconnectRequiresArgs(t *testing.T) {
	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no args provided")
	}
}

func TestDisconnectNotFound(t *testing.T) {
	setupDisconnectTestHooks(t)

	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent tunnel")
	}
	if !strings.Contains(err.Error(), "not found in state") {
		t.Errorf("error should mention 'not found in state', got: %v", err)
	}
}

func TestDisconnectSuccess(t *testing.T) {
	srcMock, dstMock, storePath := setupDisconnectTestHooks(t)
	addTestTunnel(t, storePath, "src-cluster--dst-cluster", "src-cluster", "dst-cluster", "portal-system", 10443)

	var deletedSrc, deletedDst bool
	srcMock.deleteFn = func(ctx context.Context, yamls [][]byte) error {
		deletedSrc = true
		return nil
	}
	dstMock.deleteFn = func(ctx context.Context, yamls [][]byte) error {
		deletedDst = true
		return nil
	}

	var buf strings.Builder
	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !deletedSrc {
		t.Error("expected source Delete to be called")
	}
	if !deletedDst {
		t.Error("expected destination Delete to be called")
	}

	output := buf.String()
	if !strings.Contains(output, "Deleted initiator") {
		t.Errorf("expected 'Deleted initiator' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Deleted responder") {
		t.Errorf("expected 'Deleted responder' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "disconnected") {
		t.Errorf("expected 'disconnected' in output, got:\n%s", output)
	}

	// Verify tunnel removed from state.
	store := state.NewStore(storePath)
	tunnels, err := store.List()
	if err != nil {
		t.Fatalf("failed to list tunnels: %v", err)
	}
	if len(tunnels) != 0 {
		t.Errorf("expected 0 tunnels after disconnect, got %d", len(tunnels))
	}
}

func TestDisconnectSourceDeleteError(t *testing.T) {
	srcMock, _, storePath := setupDisconnectTestHooks(t)
	addTestTunnel(t, storePath, "src-cluster--dst-cluster", "src-cluster", "dst-cluster", "portal-system", 10443)

	srcMock.deleteFn = func(ctx context.Context, yamls [][]byte) error {
		return fmt.Errorf("connection refused")
	}

	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on source Delete failure")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should contain 'connection refused', got: %v", err)
	}
}

func TestDisconnectDestDeleteError(t *testing.T) {
	_, dstMock, storePath := setupDisconnectTestHooks(t)
	addTestTunnel(t, storePath, "src-cluster--dst-cluster", "src-cluster", "dst-cluster", "portal-system", 10443)

	dstMock.deleteFn = func(ctx context.Context, yamls [][]byte) error {
		return fmt.Errorf("forbidden")
	}

	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on destination Delete failure")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error should contain 'forbidden', got: %v", err)
	}
}

func TestDisconnectKubectlNotFound(t *testing.T) {
	origCheckKubectl := checkKubectlFn
	checkKubectlFn = func() error {
		return fmt.Errorf("kubectl not found in PATH")
	}
	t.Cleanup(func() { checkKubectlFn = origCheckKubectl })

	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when kubectl not found")
	}
	if !strings.Contains(err.Error(), "kubectl not found") {
		t.Errorf("error should mention kubectl, got: %v", err)
	}
}

func TestDisconnectCleansCACerts(t *testing.T) {
	_, _, storePath := setupDisconnectTestHooks(t)

	// Create a fake CA cert directory.
	certDir := filepath.Join(t.TempDir(), "certs", "src-cluster--dst-cluster")
	if err := os.MkdirAll(certDir, 0700); err != nil {
		t.Fatalf("failed to create cert dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), []byte("fake-cert"), 0600); err != nil {
		t.Fatalf("failed to write fake cert: %v", err)
	}

	// Add tunnel with CACertPath pointing to the fake cert.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "src-cluster--dst-cluster",
		SourceContext:      "src-cluster",
		DestinationContext: "dst-cluster",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		CreatedAt:          time.Now(),
		Mode:               "imperative",
		CACertPath:         filepath.Join(certDir, "ca.crt"),
	}); err != nil {
		t.Fatalf("failed to add tunnel: %v", err)
	}

	var buf strings.Builder
	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify cert directory was cleaned up.
	if _, err := os.Stat(certDir); !os.IsNotExist(err) {
		t.Errorf("expected cert directory %s to be removed", certDir)
	}
}

func TestDisconnectNamespaceOverride(t *testing.T) {
	srcMock, dstMock, storePath := setupDisconnectTestHooks(t)
	addTestTunnel(t, storePath, "src-cluster--dst-cluster", "src-cluster", "dst-cluster", "portal-system", 10443)

	// Track which namespace the kube clients were created with.
	var srcNs, dstNs string
	origNewKubeClient := newKubeClient
	newKubeClient = func(kubeContext, namespace string) kube.Client {
		if strings.HasPrefix(kubeContext, "src") {
			srcNs = namespace
			return srcMock
		}
		dstNs = namespace
		return dstMock
	}
	t.Cleanup(func() { newKubeClient = origNewKubeClient })

	var buf strings.Builder
	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{
		"src-cluster", "dst-cluster",
		"--namespace", "custom-ns",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if srcNs != "custom-ns" {
		t.Errorf("source client namespace = %q, want %q", srcNs, "custom-ns")
	}
	if dstNs != "custom-ns" {
		t.Errorf("destination client namespace = %q, want %q", dstNs, "custom-ns")
	}
}

func TestDisconnectCleansUpExposedServices(t *testing.T) {
	srcMock, dstMock, storePath := setupDisconnectTestHooks(t)

	// Pre-populate state with a tunnel that has exposed services.
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "src-cluster--dst-cluster",
		SourceContext:      "src-cluster",
		DestinationContext: "dst-cluster",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		CreatedAt:          time.Now(),
		Mode:               "imperative",
		Services:           []string{"my-api:8080", "backend:9090"},
	}); err != nil {
		t.Fatalf("failed to add tunnel: %v", err)
	}

	// Track delete calls on both sides.
	var srcDeleteCalls, dstDeleteCalls int
	srcMock.deleteFn = func(ctx context.Context, yamls [][]byte) error {
		srcDeleteCalls++
		return nil
	}
	dstMock.deleteFn = func(ctx context.Context, yamls [][]byte) error {
		dstDeleteCalls++
		return nil
	}

	var buf strings.Builder
	cmd := NewDisconnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Core resources: 1 Delete per cluster.
	// Exposed services: 2 services × 2 sides (both source and dest for each) = 2 per cluster.
	// Total: src = 1 (core) + 2 (exposed cleanup) = 3, dst = 1 (core) + 2 (exposed cleanup) = 3.
	if srcDeleteCalls != 3 {
		t.Errorf("source Delete calls = %d, want 3 (1 core + 2 exposed cleanup)", srcDeleteCalls)
	}
	if dstDeleteCalls != 3 {
		t.Errorf("destination Delete calls = %d, want 3 (1 core + 2 exposed cleanup)", dstDeleteCalls)
	}

	output := buf.String()
	if !strings.Contains(output, "Cleaned up 2 exposed service(s)") {
		t.Errorf("expected 'Cleaned up 2 exposed service(s)' in output, got:\n%s", output)
	}
}
