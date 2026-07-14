//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E18_ReverseTunnelLifecycle exercises the v2 hub/member model end to
// end against real Envoys: hub deployment, two-phase CSR enrollment,
// hub-originated traffic over the reverse tunnel, publish/route half-states,
// SAN-spoofing rejection, single-member eviction blast radius, and forward +
// reverse coexisting on one shared listener.
//
// Topology: hub on the destination cluster (MetalLB LB), two members on the
// source cluster (namespaces portal-system and portal-b) — the source
// cluster only ever dials out.
func TestE2E18_ReverseTunnelLifecycle(t *testing.T) {
	home := isolatePortalState(t)
	tmp := t.TempDir()
	hubAddr := responderIP + ":10443"

	cleanupNamespace(t, "portal-system")
	defer cleanupNamespace(t, "portal-system")
	defer func() {
		_, _ = kubectlWithContextErr(t, sourceCtx, "delete", "namespace", "portal-b", "--ignore-not-found", "--timeout=60s")
	}()

	// --- Hub up ---
	stdout, stderr, err := runPortalWithHome(t, home, "hub", "init", destCtx,
		"--name", "e2e-hub", "--public-addr", hubAddr)
	if err != nil {
		t.Fatalf("hub init failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	waitForDeployment(t, destCtx, "portal-system", "portal-hub", 3*time.Minute)

	// Backend the members will publish: an echo server on the source cluster.
	deployEchoApp(t, sourceCtx, "default", "echo", "member echo says hi")

	// --- Member A: two-phase CSR enrollment (key never leaves the cluster) ---
	csrPath := filepath.Join(tmp, "member-a.csr")
	stdout, stderr, err = runPortalWithHome(t, home, "join", sourceCtx,
		"--member", "member-a", "--hub-addr", hubAddr, "--hub-name", "e2e-hub", "--csr-out", csrPath)
	if err != nil {
		t.Fatalf("join phase 1 failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	bundlePath := filepath.Join(tmp, "member-a-cert.pem")
	stdout, stderr, err = runPortalWithHome(t, home, "hub", "sign", csrPath,
		"--member", "member-a", "-o", bundlePath)
	if err != nil {
		t.Fatalf("hub sign failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	stdout, stderr, err = runPortalWithHome(t, home, "join", sourceCtx,
		"--member", "member-a", "--cert", bundlePath)
	if err != nil {
		t.Fatalf("join phase 2 failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	waitForDeployment(t, sourceCtx, "portal-system", "portal-member", 3*time.Minute)

	// Reverse connections should establish and be accepted.
	eventually(t, 2*time.Minute, "reverse tunnel handshake accepted", func() error {
		stats := hubAdminGet(t, "/stats?filter=reverse_tunnel")
		if !statNonZero(stats, "accepted") {
			return fmt.Errorf("no accepted handshakes yet:\n%s", stats)
		}
		return nil
	})

	// --- Publish + reverse traffic ---
	if _, stderr, err = runPortalWithHome(t, home, "publish", sourceCtx, "echo", "--port", "5678"); err != nil {
		t.Fatalf("publish failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, sourceCtx, "portal-system", "portal-member", 2*time.Minute)

	eventually(t, 2*time.Minute, "hub reaches member-a echo over the reverse tunnel", func() error {
		code, body := egressProbe(t, "echo.member-a")
		if code != 200 || !strings.Contains(body, "member echo says hi") {
			return fmt.Errorf("got %d %q", code, body)
		}
		return nil
	})

	// Half-state: an unpublished service answers 404 from the member Envoy —
	// tunnel up, allowlist miss.
	code, _ := egressProbe(t, "ghost.member-a")
	if code != 404 {
		t.Errorf("unpublished probe = %d, want 404 (member allowlist miss)", code)
	}

	// --- Route: friendly alias, authority rewritten to canonical form ---
	if _, stderr, err = runPortalWithHome(t, home, "route", "member-a/echo"); err != nil {
		t.Fatalf("route failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, destCtx, "portal-system", "portal-hub", 2*time.Minute)
	out := kubectlWithContext(t, destCtx, "get", "service", "echo-member-a", "-n", "portal-system", "-o", "name")
	if !strings.Contains(out, "echo-member-a") {
		t.Fatalf("alias Service not created: %s", out)
	}
	eventually(t, 2*time.Minute, "alias authority reaches member-a", func() error {
		code, body := egressProbe(t, "echo-member-a")
		if code != 200 || !strings.Contains(body, "member echo says hi") {
			return fmt.Errorf("got %d %q", code, body)
		}
		return nil
	})

	// --- Member B (credential mode), then SAN spoofing attempt ---
	credPath := filepath.Join(tmp, "member-b.credential")
	if _, stderr, err = runPortalWithHome(t, home, "hub", "invite", "member-b", "-o", credPath); err != nil {
		t.Fatalf("hub invite failed: %v\nstderr: %s", err, stderr)
	}
	if _, stderr, err = runPortalWithHome(t, home, "join", sourceCtx,
		"--credential", credPath, "--namespace", "portal-b"); err != nil {
		t.Fatalf("credential join failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, sourceCtx, "portal-b", "portal-member", 3*time.Minute)

	// Spoof: rewrite member-b's bootstrap to claim member-a's cluster-id while
	// presenting member-b's certificate. The hub must reject the handshake
	// because the claimed identity does not match the peer cert DNS SAN.
	spoofMemberBootstrap(t, "portal-b", "member-b", "member-a")
	eventually(t, 2*time.Minute, "spoofed identity rejected (validation_failed)", func() error {
		stats := hubAdminGet(t, "/stats?filter=reverse_tunnel")
		if !statNonZero(stats, "validation_failed") {
			return fmt.Errorf("no validation failures recorded yet:\n%s", stats)
		}
		return nil
	})

	// Restore member-b's real config by publishing (re-renders + restarts).
	if _, stderr, err = runPortalWithHome(t, home, "publish", sourceCtx, "echo",
		"--port", "5678", "--member", "member-b"); err != nil {
		t.Fatalf("publish member-b failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, sourceCtx, "portal-b", "portal-member", 2*time.Minute)
	eventually(t, 2*time.Minute, "hub reaches member-b after restore", func() error {
		code, body := egressProbe(t, "echo.member-b")
		if code != 200 || !strings.Contains(body, "member echo says hi") {
			return fmt.Errorf("got %d %q", code, body)
		}
		return nil
	})

	// --- Eviction: blast radius is exactly one member ---
	if _, stderr, err = runPortalWithHome(t, home, "hub", "evict", "member-b"); err != nil {
		t.Fatalf("hub evict failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, destCtx, "portal-system", "portal-hub", 2*time.Minute)
	// Force member-b to reconnect; its revoked cert must now fail the TLS
	// handshake (CRL) and its routing is gone.
	kubectlWithContext(t, sourceCtx, "rollout", "restart", "deployment/portal-member", "-n", "portal-b")

	eventually(t, 3*time.Minute, "member-a unaffected by member-b eviction", func() error {
		code, body := egressProbe(t, "echo.member-a")
		if code != 200 || !strings.Contains(body, "member echo says hi") {
			return fmt.Errorf("got %d %q", code, body)
		}
		return nil
	})
	eventually(t, 2*time.Minute, "member-b unreachable after eviction", func() error {
		code, _ := egressProbe(t, "echo.member-b")
		if code == 200 {
			return fmt.Errorf("evicted member still reachable")
		}
		return nil
	})

	// --- Forward path on the same listener ---
	deployEchoApp(t, destCtx, "default", "hub-echo", "hub echo says hi")
	if _, stderr, err = runPortalWithHome(t, home, "hub", "expose", "hub-echo",
		"--port", "5678", "--sni", "hub-echo"); err != nil {
		t.Fatalf("hub expose failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, destCtx, "portal-system", "portal-hub", 2*time.Minute)
	if _, stderr, err = runPortalWithHome(t, home, "forward", sourceCtx, "hub-echo",
		"--local-port", "15678", "--member", "member-a"); err != nil {
		t.Fatalf("forward failed: %v\nstderr: %s", err, stderr)
	}
	waitForDeployment(t, sourceCtx, "portal-system", "portal-member", 2*time.Minute)

	eventually(t, 3*time.Minute, "member reaches hub service over the forward path", func() error {
		out, err := kubectlWithContextErr(t, sourceCtx, "run", "curl-fwd-"+fmt.Sprint(time.Now().UnixNano()),
			"--rm", "-i", "--restart=Never", "--image=curlimages/curl:8.5.0", "--",
			"-s", "--max-time", "10", "http://portal-fwd-hub-echo.portal-system:15678/")
		if err != nil {
			return fmt.Errorf("curl pod failed: %v: %s", err, out)
		}
		if !strings.Contains(out, "hub echo says hi") {
			return fmt.Errorf("unexpected forward response: %s", out)
		}
		return nil
	})

	// --- Status reflects reality ---
	stdout, _, err = runPortalWithHome(t, home, "status", "member-a")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	for _, want := range []string{"MEMBER member-a", "accepted", "reachable"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q:\n%s", want, stdout)
		}
	}
}

// eventually polls fn until it succeeds or the timeout expires.
func eventually(t *testing.T, timeout time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = fn(); lastErr == nil {
			t.Logf("✓ %s", what)
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("timed out waiting for %s: %v", what, lastErr)
}

// deployEchoApp deploys an http-echo Deployment + Service.
func deployEchoApp(t *testing.T, kubeCtx, namespace, name, text string) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: echo
          image: hashicorp/http-echo:0.2.3
          args: ["-listen=:5678", "-text=%[3]s"]
          ports: [{containerPort: 5678}]
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector: {app: %[1]s}
  ports: [{port: 5678, targetPort: 5678}]
`, name, namespace, text)
	cmd := exec.Command("kubectl", "apply", "--context", kubeCtx, "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to deploy echo app: %v\n%s", err, out)
	}
	waitForDeployment(t, kubeCtx, namespace, name, 2*time.Minute)
}

// egressProbe sends a request through the hub egress listener with the given
// authority, returning the HTTP status (0 on transport failure) and body.
func egressProbe(t *testing.T, authority string) (int, string) {
	t.Helper()
	localPort := findFreePort(t)
	pf := exec.Command("kubectl", "--context", destCtx, "-n", "portal-system",
		"port-forward", "deploy/portal-hub", fmt.Sprintf("%d:10080", localPort))
	pf.Stdout = os.Stdout
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward failed to start: %v", err)
	}
	defer func() { _ = pf.Process.Kill(); _, _ = pf.Process.Wait() }()
	if err := waitForPortErr(localPort, 10*time.Second); err != nil {
		return 0, "port-forward not ready"
	}

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", localPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = authority
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// hubAdminGet fetches a path from the hub Envoy admin endpoint.
func hubAdminGet(t *testing.T, path string) string {
	t.Helper()
	return portForwardAndGet(t, destCtx, "portal-system", "deploy/portal-hub", 15001, path)
}

// statNonZero reports whether any stat line whose name contains the suffix
// has a value greater than zero. Stats text format: "name: value".
func statNonZero(stats, name string) bool {
	for _, line := range strings.Split(stats, "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			continue
		}
		if v := strings.TrimSpace(parts[1]); v != "0" && v != "" {
			return true
		}
	}
	return false
}

// spoofMemberBootstrap rewrites a member's bootstrap ConfigMap to claim a
// different cluster-id than its certificate proves, then restarts the member.
func spoofMemberBootstrap(t *testing.T, namespace, realMember, claimedMember string) {
	t.Helper()
	cm, err := kubectlWithContextErr(t, sourceCtx, "get", "configmap", "portal-member-bootstrap",
		"-n", namespace, "-o", "jsonpath={.data.envoy\\.yaml}")
	if err != nil {
		t.Fatalf("failed to read member bootstrap: %v", err)
	}
	spoofed := strings.Replace(cm,
		fmt.Sprintf("rc://%s:%s:", realMember, realMember),
		fmt.Sprintf("rc://%s:%s:", realMember, claimedMember), 1)
	if spoofed == cm {
		t.Fatalf("spoof substitution did not match bootstrap content:\n%s", cm[:min(400, len(cm))])
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "envoy.yaml")
	if err := os.WriteFile(path, []byte(spoofed), 0o644); err != nil {
		t.Fatal(err)
	}
	create := exec.Command("kubectl", "--context", sourceCtx, "-n", namespace,
		"create", "configmap", "portal-member-bootstrap",
		"--from-file=envoy.yaml="+path, "--dry-run=client", "-o", "yaml")
	createOut, err := create.Output()
	if err != nil {
		t.Fatalf("failed to build spoofed configmap: %v", err)
	}
	apply := exec.Command("kubectl", "--context", sourceCtx, "-n", namespace, "apply", "-f", "-")
	apply.Stdin = strings.NewReader(string(createOut))
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("failed to apply spoofed configmap: %v\n%s", err, out)
	}
	kubectlWithContext(t, sourceCtx, "rollout", "restart", "deployment/portal-member", "-n", namespace)
}
