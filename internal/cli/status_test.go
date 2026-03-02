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

func setupStatusTestHooks(t *testing.T) (srcMock, dstMock *mockKubeClient, storePath string) {
	t.Helper()

	srcMock = &mockKubeClient{}
	dstMock = &mockKubeClient{}

	origNewKubeClient := newKubeClient
	origCheckKubectl := checkKubectlFn
	origCheckContext := checkContextFn
	origNewStateStore := newStateStore
	origFetchStats := fetchEnvoyStatsFn

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

	// Default: no stats (avoids port-forwarding in tests).
	fetchEnvoyStatsFn = func(ctx context.Context, client kube.Client, podName string, adminPort int) *envoyStats {
		return nil
	}

	t.Cleanup(func() {
		newKubeClient = origNewKubeClient
		checkKubectlFn = origCheckKubectl
		checkContextFn = origCheckContext
		newStateStore = origNewStateStore
		fetchEnvoyStatsFn = origFetchStats
	})

	return srcMock, dstMock, storePath
}

func addStatusTestTunnel(t *testing.T, storePath string) {
	t.Helper()
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "src-cluster--dst-cluster",
		SourceContext:      "src-cluster",
		DestinationContext: "dst-cluster",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		CreatedAt:          time.Now(),
		Mode:               "imperative",
	}); err != nil {
		t.Fatalf("failed to add test tunnel: %v", err)
	}
}

func TestStatusNoTunnels(t *testing.T) {
	setupStatusTestHooks(t)

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "No tunnels found") {
		t.Errorf("expected 'No tunnels found', got:\n%s", buf.String())
	}
}

func TestStatusSingleArg(t *testing.T) {
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"only-one"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error with 1 arg")
	}
	if !strings.Contains(err.Error(), "expected 0 or 2 arguments") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStatusTunnelNotFound(t *testing.T) {
	setupStatusTestHooks(t)

	cmd := NewStatusCmd()
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

func TestStatusConnected(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:     "portal-initiator-abc123",
			Phase:    kube.PodRunning,
			Ready:    true,
			Restarts: 0,
		}}, nil
	}

	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:     "portal-responder-def456",
			Phase:    kube.PodRunning,
			Ready:    true,
			Restarts: 0,
		}}, nil
	}

	dstMock.getServiceFn = func(ctx context.Context, name string) (*kube.ServiceInfo, error) {
		return &kube.ServiceInfo{
			Name: "portal-responder",
			Type: "LoadBalancer",
			LoadBalancerIngress: []kube.LoadBalancerIngress{
				{IP: "34.120.1.50"},
			},
		}, nil
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Connected") {
		t.Errorf("expected 'Connected' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "34.120.1.50:10443") {
		t.Errorf("expected endpoint in output, got:\n%s", output)
	}
	if !strings.Contains(output, "portal-initiator-abc123") {
		t.Errorf("expected initiator pod name in output, got:\n%s", output)
	}
	if !strings.Contains(output, "portal-responder-def456") {
		t.Errorf("expected responder pod name in output, got:\n%s", output)
	}
}

func TestStatusDegraded(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	// Initiator not ready.
	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:     "portal-initiator-abc123",
			Phase:    kube.PodRunning,
			Ready:    false,
			Restarts: 3,
		}}, nil
	}

	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:  "portal-responder-def456",
			Phase: kube.PodRunning,
			Ready: true,
		}}, nil
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Degraded") {
		t.Errorf("expected 'Degraded' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "3") {
		t.Errorf("expected restart count in output, got:\n%s", output)
	}
}

func TestStatusPending(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	// No initiator pods yet.
	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return nil, nil
	}
	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:  "portal-responder-def456",
			Phase: kube.PodRunning,
			Ready: true,
		}}, nil
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Pending") {
		t.Errorf("expected 'Pending' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "No pods found") {
		t.Errorf("expected 'No pods found' for initiator, got:\n%s", output)
	}
}

func TestStatusQueryError(t *testing.T) {
	srcMock, _, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return nil, fmt.Errorf("connection refused")
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	// Query errors are displayed, not returned as command errors.
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Error") {
		t.Errorf("expected 'Error' status in output, got:\n%s", output)
	}
	if !strings.Contains(output, "connection refused") {
		t.Errorf("expected error detail in output, got:\n%s", output)
	}
}

func TestStatusAllMultipleTunnels(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)

	store := state.NewStore(storePath)
	for _, name := range []string{"src-a--dst-a", "src-b--dst-b"} {
		parts := strings.SplitN(name, "--", 2)
		if err := store.Add(state.TunnelState{
			Name:               name,
			SourceContext:      parts[0],
			DestinationContext: parts[1],
			Namespace:          "portal-system",
			TunnelPort:         10443,
			CreatedAt:          time.Now(),
			Mode:               "imperative",
		}); err != nil {
			t.Fatalf("failed to add %s: %v", name, err)
		}
	}

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "pod", Phase: kube.PodRunning, Ready: true}}, nil
	}
	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "pod", Phase: kube.PodRunning, Ready: true}}, nil
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "src-a--dst-a") {
		t.Errorf("expected first tunnel in output, got:\n%s", output)
	}
	if !strings.Contains(output, "src-b--dst-b") {
		t.Errorf("expected second tunnel in output, got:\n%s", output)
	}
}

func TestStatusJSONOutput(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "init-pod", Phase: kube.PodRunning, Ready: true}}, nil
	}
	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "resp-pod", Phase: kube.PodRunning, Ready: true}}, nil
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"status": "Connected"`) {
		t.Errorf("expected JSON status, got:\n%s", output)
	}
	if !strings.Contains(output, `"pod_name": "init-pod"`) {
		t.Errorf("expected initiator pod in JSON, got:\n%s", output)
	}
}

func TestStatusWithEnvoyStats(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:  "portal-initiator-abc123",
			Phase: kube.PodRunning,
			Ready: true,
		}}, nil
	}
	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:  "portal-responder-def456",
			Phase: kube.PodRunning,
			Ready: true,
		}}, nil
	}

	// Mock stats to return canned data.
	origFetchStats := fetchEnvoyStatsFn
	t.Cleanup(func() { fetchEnvoyStatsFn = origFetchStats })
	fetchEnvoyStatsFn = func(ctx context.Context, client kube.Client, podName string, adminPort int) *envoyStats {
		if podName == "portal-initiator-abc123" {
			return &envoyStats{
				UptimeSeconds:     3661,
				ActiveConnections: 5,
				TotalConnections:  150,
				BytesSent:         1048576,  // 1 MiB
				BytesReceived:     10485760, // 10 MiB
			}
		}
		return &envoyStats{
			UptimeSeconds:     7200,
			ActiveConnections: 3,
			TotalConnections:  100,
			BytesSent:         2097152, // 2 MiB
			BytesReceived:     5242880, // 5 MiB
		}
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()

	// Verify initiator stats.
	if !strings.Contains(output, "1h1m") {
		t.Errorf("expected initiator uptime '1h1m' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "5 active, 150 total") {
		t.Errorf("expected initiator connection stats in output, got:\n%s", output)
	}
	if !strings.Contains(output, "1.0 MiB sent") {
		t.Errorf("expected initiator bytes sent in output, got:\n%s", output)
	}

	// Verify responder stats.
	if !strings.Contains(output, "2h0m") {
		t.Errorf("expected responder uptime '2h0m' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "3 active, 100 total") {
		t.Errorf("expected responder connection stats in output, got:\n%s", output)
	}
}

func TestStatusStatsNotShownWhenUnavailable(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:  "portal-initiator-abc123",
			Phase: kube.PodRunning,
			Ready: true,
		}}, nil
	}
	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{
			Name:  "portal-responder-def456",
			Phase: kube.PodRunning,
			Ready: true,
		}}, nil
	}

	// fetchEnvoyStatsFn returns nil (default from setupStatusTestHooks).
	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	// Stats lines should not appear.
	if strings.Contains(output, "Uptime:") {
		t.Errorf("should not show Uptime when stats unavailable, got:\n%s", output)
	}
	if strings.Contains(output, "Connections:") {
		t.Errorf("should not show Connections when stats unavailable, got:\n%s", output)
	}
	if strings.Contains(output, "Traffic:") {
		t.Errorf("should not show Traffic when stats unavailable, got:\n%s", output)
	}
}

func TestStatusJSONWithStats(t *testing.T) {
	srcMock, dstMock, storePath := setupStatusTestHooks(t)
	addStatusTestTunnel(t, storePath)

	srcMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "init-pod", Phase: kube.PodRunning, Ready: true}}, nil
	}
	dstMock.getPodsFn = func(ctx context.Context, labelSelector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "resp-pod", Phase: kube.PodRunning, Ready: true}}, nil
	}

	origFetchStats := fetchEnvoyStatsFn
	t.Cleanup(func() { fetchEnvoyStatsFn = origFetchStats })
	fetchEnvoyStatsFn = func(ctx context.Context, client kube.Client, podName string, adminPort int) *envoyStats {
		return &envoyStats{
			UptimeSeconds:     3600,
			ActiveConnections: 2,
			TotalConnections:  50,
			BytesSent:         1024,
			BytesReceived:     2048,
		}
	}

	var buf strings.Builder
	cmd := NewStatusCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"src-cluster", "dst-cluster", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"uptime_seconds": 3600`) {
		t.Errorf("expected uptime in JSON, got:\n%s", output)
	}
	if !strings.Contains(output, `"active_connections": 2`) {
		t.Errorf("expected active_connections in JSON, got:\n%s", output)
	}
	if !strings.Contains(output, `"bytes_sent": 1024`) {
		t.Errorf("expected bytes_sent in JSON, got:\n%s", output)
	}
}

func TestParseEnvoyStats(t *testing.T) {
	data := []byte(`{
		"stats": [
			{"name": "server.uptime", "value": 7200},
			{"name": "cluster.tunnel_to_responder.upstream_cx_active", "value": 5},
			{"name": "cluster.tunnel_to_responder.upstream_cx_total", "value": 100},
			{"name": "cluster.tunnel_to_responder.upstream_cx_tx_bytes_total", "value": 1048576},
			{"name": "cluster.tunnel_to_responder.upstream_cx_rx_bytes_total", "value": 2097152},
			{"name": "server.live", "value": 1}
		]
	}`)

	stats := parseEnvoyStats(data)
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.UptimeSeconds != 7200 {
		t.Errorf("UptimeSeconds = %d, want 7200", stats.UptimeSeconds)
	}
	if stats.ActiveConnections != 5 {
		t.Errorf("ActiveConnections = %d, want 5", stats.ActiveConnections)
	}
	if stats.TotalConnections != 100 {
		t.Errorf("TotalConnections = %d, want 100", stats.TotalConnections)
	}
	if stats.BytesSent != 1048576 {
		t.Errorf("BytesSent = %d, want 1048576", stats.BytesSent)
	}
	if stats.BytesReceived != 2097152 {
		t.Errorf("BytesReceived = %d, want 2097152", stats.BytesReceived)
	}
}

func TestParseEnvoyStatsInvalidJSON(t *testing.T) {
	stats := parseEnvoyStats([]byte("not json"))
	if stats != nil {
		t.Error("expected nil stats for invalid JSON")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{30, "30s"},
		{90, "1m30s"},
		{3661, "1h1m"},
		{90000, "1d1h"},
	}
	for _, tt := range tests {
		got := formatDuration(time.Duration(tt.seconds) * time.Second)
		if got != tt.want {
			t.Errorf("formatDuration(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.bytes)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}
