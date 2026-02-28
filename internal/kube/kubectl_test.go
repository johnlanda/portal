package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mockRunner records calls and returns canned responses.
type mockRunner struct {
	calls   []mockCall
	runFunc func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error)
}

type mockCall struct {
	Stdin []byte
	Name  string
	Args  []string
}

func (m *mockRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	m.calls = append(m.calls, mockCall{Stdin: stdin, Name: name, Args: args})
	if m.runFunc != nil {
		return m.runFunc(ctx, stdin, name, args...)
	}
	return nil, nil, nil
}

func (m *mockRunner) Start(ctx context.Context, name string, args ...string) (*Process, error) {
	m.calls = append(m.calls, mockCall{Name: name, Args: args})
	return &Process{}, nil
}

func TestApply(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("test-ctx", "test-ns", WithRunner(m))

	yamls := [][]byte{
		[]byte("apiVersion: v1\nkind: Pod\n"),
		[]byte("apiVersion: v1\nkind: Service\n"),
	}

	err := c.Apply(context.Background(), yamls)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if len(m.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(m.calls))
	}

	call := m.calls[0]
	if call.Name != "kubectl" {
		t.Errorf("command = %q, want kubectl", call.Name)
	}

	args := strings.Join(call.Args, " ")
	if !strings.Contains(args, "--context test-ctx") {
		t.Errorf("args missing --context: %s", args)
	}
	if !strings.Contains(args, "-n test-ns") {
		t.Errorf("args missing -n: %s", args)
	}
	if !strings.Contains(args, "apply -f -") {
		t.Errorf("args missing apply -f -: %s", args)
	}

	stdin := string(call.Stdin)
	if !strings.Contains(stdin, "---\n") {
		t.Errorf("stdin missing YAML separator: %q", stdin)
	}
}

func TestApplyEmpty(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.Apply(context.Background(), nil)
	if err != nil {
		t.Fatalf("Apply(nil) error = %v", err)
	}
	if len(m.calls) != 0 {
		t.Error("expected no subprocess calls for empty input")
	}
}

func TestApplyError(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return nil, []byte("error: connection refused"), fmt.Errorf("exit status 1")
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.Apply(context.Background(), [][]byte{[]byte("apiVersion: v1\n")})
	if err == nil {
		t.Fatal("expected error")
	}

	var ke *KubectlError
	if !errors.As(err, &ke) {
		t.Fatalf("expected KubectlError, got %T", err)
	}
	if ke.Command != "apply" {
		t.Errorf("command = %q, want apply", ke.Command)
	}
	if !strings.Contains(ke.Stderr, "connection refused") {
		t.Errorf("stderr = %q, want to contain 'connection refused'", ke.Stderr)
	}
}

func TestDelete(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.Delete(context.Background(), [][]byte{[]byte("apiVersion: v1\n")})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	args := strings.Join(m.calls[0].Args, " ")
	if !strings.Contains(args, "delete -f -") {
		t.Errorf("args missing delete -f -: %s", args)
	}
	if !strings.Contains(args, "--ignore-not-found") {
		t.Errorf("args missing --ignore-not-found: %s", args)
	}
}

func TestDeleteEmpty(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.Delete(context.Background(), nil)
	if err != nil {
		t.Fatalf("Delete(nil) error = %v", err)
	}
	if len(m.calls) != 0 {
		t.Error("expected no subprocess calls for empty input")
	}
}

func TestWaitForDeployment(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.WaitForDeployment(context.Background(), "my-deploy", 120*time.Second)
	if err != nil {
		t.Fatalf("WaitForDeployment() error = %v", err)
	}

	args := strings.Join(m.calls[0].Args, " ")
	if !strings.Contains(args, "deployment/my-deploy") {
		t.Errorf("args missing deployment name: %s", args)
	}
	if !strings.Contains(args, "--for=condition=Available") {
		t.Errorf("args missing --for=condition=Available: %s", args)
	}
	if !strings.Contains(args, "--timeout=120s") {
		t.Errorf("args missing --timeout: %s", args)
	}
}

func TestWaitForDeploymentTimeout(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return nil, []byte("error: timed out waiting for the condition"), fmt.Errorf("exit status 1")
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.WaitForDeployment(context.Background(), "my-deploy", 30*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}

	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError, got %T: %v", err, err)
	}
	if !strings.Contains(te.Operation, "deployment/my-deploy") {
		t.Errorf("operation = %q, want to contain deployment name", te.Operation)
	}
}

func TestGetService(t *testing.T) {
	svcJSON := `{
		"metadata": {"name": "my-svc", "namespace": "test-ns"},
		"spec": {
			"type": "LoadBalancer",
			"clusterIP": "10.0.0.1",
			"ports": [
				{"name": "http", "protocol": "TCP", "port": 80, "targetPort": 8080},
				{"name": "grpc", "protocol": "TCP", "port": 443, "targetPort": "https"}
			]
		},
		"status": {
			"loadBalancer": {
				"ingress": [{"ip": "203.0.113.1"}]
			}
		}
	}`

	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return []byte(svcJSON), nil, nil
		},
	}
	c := NewClient("ctx", "test-ns", WithRunner(m))

	svc, err := c.GetService(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("GetService() error = %v", err)
	}

	if svc.Name != "my-svc" {
		t.Errorf("Name = %q, want my-svc", svc.Name)
	}
	if svc.Type != "LoadBalancer" {
		t.Errorf("Type = %q, want LoadBalancer", svc.Type)
	}
	if svc.ClusterIP != "10.0.0.1" {
		t.Errorf("ClusterIP = %q, want 10.0.0.1", svc.ClusterIP)
	}
	if len(svc.Ports) != 2 {
		t.Fatalf("Ports count = %d, want 2", len(svc.Ports))
	}
	if svc.Ports[0].TargetPort != "8080" {
		t.Errorf("Ports[0].TargetPort = %q, want 8080", svc.Ports[0].TargetPort)
	}
	if svc.Ports[1].TargetPort != "https" {
		t.Errorf("Ports[1].TargetPort = %q, want https", svc.Ports[1].TargetPort)
	}
	if len(svc.LoadBalancerIngress) != 1 {
		t.Fatalf("LoadBalancerIngress count = %d, want 1", len(svc.LoadBalancerIngress))
	}
	if svc.LoadBalancerIngress[0].IP != "203.0.113.1" {
		t.Errorf("LB IP = %q, want 203.0.113.1", svc.LoadBalancerIngress[0].IP)
	}
	if svc.LoadBalancerIngress[0].Address() != "203.0.113.1" {
		t.Errorf("LB Address() = %q, want 203.0.113.1", svc.LoadBalancerIngress[0].Address())
	}
}

func TestGetServiceNotFound(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return nil, []byte(`Error from server (NotFound): services "my-svc" not found`), fmt.Errorf("exit status 1")
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	_, err := c.GetService(context.Background(), "my-svc")
	if err == nil {
		t.Fatal("expected error")
	}

	var nfe *NotFoundError
	if !errors.As(err, &nfe) {
		t.Fatalf("expected NotFoundError, got %T: %v", err, err)
	}
	if !strings.Contains(nfe.Resource, "my-svc") {
		t.Errorf("resource = %q, want to contain my-svc", nfe.Resource)
	}
}

func TestGetPods(t *testing.T) {
	podsJSON := fmt.Sprintf(`{
		"items": [{
			"metadata": {
				"name": "my-pod-abc123",
				"namespace": "test-ns",
				"creationTimestamp": %q
			},
			"spec": {"nodeName": "node-1"},
			"status": {
				"phase": "Running",
				"podIP": "10.0.0.5",
				"containerStatuses": [
					{"name": "envoy", "ready": true, "restartCount": 2, "image": "envoy:latest"},
					{"name": "sidecar", "ready": true, "restartCount": 1, "image": "sidecar:v1"}
				]
			}
		}]
	}`, time.Now().Add(-1*time.Hour).Format(time.RFC3339))

	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return []byte(podsJSON), nil, nil
		},
	}
	c := NewClient("ctx", "test-ns", WithRunner(m))

	pods, err := c.GetPods(context.Background(), "app=my-app")
	if err != nil {
		t.Fatalf("GetPods() error = %v", err)
	}

	if len(pods) != 1 {
		t.Fatalf("pods count = %d, want 1", len(pods))
	}

	pod := pods[0]
	if pod.Name != "my-pod-abc123" {
		t.Errorf("Name = %q, want my-pod-abc123", pod.Name)
	}
	if pod.Phase != PodRunning {
		t.Errorf("Phase = %q, want Running", pod.Phase)
	}
	if !pod.Ready {
		t.Error("expected pod to be Ready")
	}
	if pod.Restarts != 3 {
		t.Errorf("Restarts = %d, want 3", pod.Restarts)
	}
	if pod.IP != "10.0.0.5" {
		t.Errorf("IP = %q, want 10.0.0.5", pod.IP)
	}
	if pod.NodeName != "node-1" {
		t.Errorf("NodeName = %q, want node-1", pod.NodeName)
	}
	if len(pod.Containers) != 2 {
		t.Fatalf("Containers count = %d, want 2", len(pod.Containers))
	}
	if pod.Containers[0].Name != "envoy" {
		t.Errorf("Container[0].Name = %q, want envoy", pod.Containers[0].Name)
	}

	// Verify label selector is passed correctly.
	args := strings.Join(m.calls[0].Args, " ")
	if !strings.Contains(args, "-l app=my-app") {
		t.Errorf("args missing label selector: %s", args)
	}
}

func TestParsePodListJSON(t *testing.T) {
	raw := `{
		"items": [{
			"metadata": {"name": "p1", "namespace": "ns", "creationTimestamp": "2025-01-01T00:00:00Z"},
			"spec": {"nodeName": "n1"},
			"status": {
				"phase": "Running",
				"podIP": "10.0.0.1",
				"containerStatuses": [
					{"name": "c1", "ready": true, "restartCount": 5, "image": "img:v1"},
					{"name": "c2", "ready": false, "restartCount": 3, "image": "img:v2"}
				]
			}
		}]
	}`

	pods, err := parsePodListJSON([]byte(raw))
	if err != nil {
		t.Fatalf("parsePodListJSON() error = %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("pods count = %d, want 1", len(pods))
	}
	if pods[0].Restarts != 8 {
		t.Errorf("Restarts = %d, want 8 (5+3)", pods[0].Restarts)
	}
	if pods[0].Ready {
		t.Error("expected pod not to be Ready (c2 is not ready)")
	}
}

// ---------------------------------------------------------------------------
// WaitForServiceAddress tests
// ---------------------------------------------------------------------------

func TestWaitForServiceAddressIP(t *testing.T) {
	callCount := 0
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, []byte, error) {
			callCount++
			// First call: service exists but no LB ingress yet.
			if callCount == 1 {
				return []byte(`{
					"metadata": {"name": "portal-responder", "namespace": "ns"},
					"spec": {"type": "LoadBalancer", "ports": []},
					"status": {"loadBalancer": {"ingress": []}}
				}`), nil, nil
			}
			// Second call: LB has an IP.
			return []byte(`{
				"metadata": {"name": "portal-responder", "namespace": "ns"},
				"spec": {"type": "LoadBalancer", "ports": []},
				"status": {"loadBalancer": {"ingress": [{"ip": "34.120.1.50"}]}}
			}`), nil, nil
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	addr, err := c.WaitForServiceAddress(context.Background(), "portal-responder", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForServiceAddress() error = %v", err)
	}
	if addr != "34.120.1.50" {
		t.Errorf("address = %q, want 34.120.1.50", addr)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 GetService calls, got %d", callCount)
	}
}

func TestWaitForServiceAddressHostname(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return []byte(`{
				"metadata": {"name": "portal-responder", "namespace": "ns"},
				"spec": {"type": "LoadBalancer", "ports": []},
				"status": {"loadBalancer": {"ingress": [{"hostname": "lb.example.com"}]}}
			}`), nil, nil
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	addr, err := c.WaitForServiceAddress(context.Background(), "portal-responder", 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForServiceAddress() error = %v", err)
	}
	if addr != "lb.example.com" {
		t.Errorf("address = %q, want lb.example.com", addr)
	}
}

func TestWaitForServiceAddressTimeout(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			// Always return service with no ingress.
			return []byte(`{
				"metadata": {"name": "svc", "namespace": "ns"},
				"spec": {"type": "LoadBalancer", "ports": []},
				"status": {"loadBalancer": {"ingress": []}}
			}`), nil, nil
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	// Use a very short timeout so the test doesn't hang.
	_, err := c.WaitForServiceAddress(context.Background(), "svc", 3*time.Second)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError, got %T: %v", err, err)
	}
	if !strings.Contains(te.Operation, "service/svc") {
		t.Errorf("operation = %q, want to contain service/svc", te.Operation)
	}
}

func TestWaitForServiceAddressNotFoundThenFound(t *testing.T) {
	callCount := 0
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			callCount++
			if callCount <= 2 {
				// Service not found yet.
				return nil, []byte(`Error from server (NotFound): services "svc" not found`), fmt.Errorf("exit status 1")
			}
			// Service appears with address.
			return []byte(`{
				"metadata": {"name": "svc", "namespace": "ns"},
				"spec": {"type": "LoadBalancer", "ports": []},
				"status": {"loadBalancer": {"ingress": [{"ip": "10.0.0.1"}]}}
			}`), nil, nil
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	addr, err := c.WaitForServiceAddress(context.Background(), "svc", 15*time.Second)
	if err != nil {
		t.Fatalf("WaitForServiceAddress() error = %v", err)
	}
	if addr != "10.0.0.1" {
		t.Errorf("address = %q, want 10.0.0.1", addr)
	}
}

func TestWaitForServiceAddressFatalError(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return nil, []byte("error: connection refused"), fmt.Errorf("exit status 1")
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	_, err := c.WaitForServiceAddress(context.Background(), "svc", 10*time.Second)
	if err == nil {
		t.Fatal("expected error on fatal kubectl failure")
	}
	var ke *KubectlError
	if !errors.As(err, &ke) {
		t.Fatalf("expected KubectlError, got %T: %v", err, err)
	}
}

func TestWaitForServiceAddressContextCancelled(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return []byte(`{
				"metadata": {"name": "svc", "namespace": "ns"},
				"spec": {"type": "LoadBalancer", "ports": []},
				"status": {"loadBalancer": {"ingress": []}}
			}`), nil, nil
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately.
	cancel()

	_, err := c.WaitForServiceAddress(ctx, "svc", 30*time.Second)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// ---------------------------------------------------------------------------
// PortForward tests
// ---------------------------------------------------------------------------

func TestPortForwardCommandArgs(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("my-ctx", "my-ns", WithRunner(m))

	// PortForward calls Start then waitForPort. Since there's no real process
	// listening, waitForPort will fail — but we can still verify the command
	// was constructed correctly.
	_, _ = c.PortForward(context.Background(), "deployment/portal-initiator", 15000, 15000)

	if len(m.calls) == 0 {
		t.Fatal("expected at least 1 call to Start")
	}

	call := m.calls[0]
	if call.Name != "kubectl" {
		t.Errorf("command = %q, want kubectl", call.Name)
	}

	args := strings.Join(call.Args, " ")
	if !strings.Contains(args, "--context my-ctx") {
		t.Errorf("args missing --context: %s", args)
	}
	if !strings.Contains(args, "-n my-ns") {
		t.Errorf("args missing -n: %s", args)
	}
	if !strings.Contains(args, "port-forward") {
		t.Errorf("args missing port-forward: %s", args)
	}
	if !strings.Contains(args, "deployment/portal-initiator") {
		t.Errorf("args missing target: %s", args)
	}
	if !strings.Contains(args, "15000:15000") {
		t.Errorf("args missing port mapping: %s", args)
	}
}

func TestPortForwardStartError(t *testing.T) {
	m := &mockRunner{}
	origStart := m.Start
	_ = origStart
	// Override Start to return an error.
	startErr := fmt.Errorf("command not found")
	m2 := &startFailRunner{err: startErr}
	c := NewClient("ctx", "ns", WithRunner(m2))

	_, err := c.PortForward(context.Background(), "deployment/test", 8080, 8080)
	if err == nil {
		t.Fatal("expected error when Start fails")
	}
	var ke *KubectlError
	if !errors.As(err, &ke) {
		t.Fatalf("expected KubectlError, got %T: %v", err, err)
	}
	if ke.Command != "port-forward" {
		t.Errorf("command = %q, want port-forward", ke.Command)
	}
}

func TestPortForwardSessionClose(t *testing.T) {
	// A nil-process session should close cleanly.
	session := &PortForwardSession{
		LocalPort:  8080,
		RemotePort: 8080,
		Target:     "deployment/test",
		Namespace:  "ns",
		process:    nil,
	}
	if err := session.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil for nil process", err)
	}

	// A session with an empty Process (no cmd) should also close cleanly.
	session2 := &PortForwardSession{
		LocalPort:  8080,
		RemotePort: 8080,
		Target:     "deployment/test",
		Namespace:  "ns",
		process:    &Process{},
	}
	if err := session2.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil for empty process", err)
	}
}

// startFailRunner is a CommandRunner that fails on Start.
type startFailRunner struct {
	err error
}

func (r *startFailRunner) Run(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}

func (r *startFailRunner) Start(_ context.Context, _ string, _ ...string) (*Process, error) {
	return nil, r.err
}

// ---------------------------------------------------------------------------
// RolloutRestart tests
// ---------------------------------------------------------------------------

func TestRolloutRestart(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.RolloutRestart(context.Background(), "portal-responder")
	if err != nil {
		t.Fatalf("RolloutRestart() error = %v", err)
	}

	if len(m.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(m.calls))
	}
	args := strings.Join(m.calls[0].Args, " ")
	if !strings.Contains(args, "rollout restart deployment/portal-responder") {
		t.Errorf("args missing rollout restart command: %s", args)
	}
}

func TestRolloutRestartError(t *testing.T) {
	m := &mockRunner{
		runFunc: func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, []byte, error) {
			return nil, []byte("deployment not found"), fmt.Errorf("exit status 1")
		},
	}
	c := NewClient("ctx", "ns", WithRunner(m))

	err := c.RolloutRestart(context.Background(), "missing-deploy")
	if err == nil {
		t.Fatal("expected error")
	}
	var ke *KubectlError
	if !errors.As(err, &ke) {
		t.Fatalf("expected KubectlError, got %T: %v", err, err)
	}
	if ke.Command != "rollout restart" {
		t.Errorf("command = %q, want 'rollout restart'", ke.Command)
	}
}

func TestLoadBalancerIngressAddress(t *testing.T) {
	tests := []struct {
		name    string
		ingress LoadBalancerIngress
		want    string
	}{
		{name: "ip only", ingress: LoadBalancerIngress{IP: "1.2.3.4"}, want: "1.2.3.4"},
		{name: "hostname only", ingress: LoadBalancerIngress{Hostname: "lb.example.com"}, want: "lb.example.com"},
		{name: "both prefers ip", ingress: LoadBalancerIngress{IP: "1.2.3.4", Hostname: "lb.example.com"}, want: "1.2.3.4"},
		{name: "empty", ingress: LoadBalancerIngress{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ingress.Address(); got != tt.want {
				t.Errorf("Address() = %q, want %q", got, tt.want)
			}
		})
	}
}
