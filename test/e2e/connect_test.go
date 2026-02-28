//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestE2E02_ConnectDisconnectLifecycle verifies the imperative connect/disconnect lifecycle.
//
// Steps:
//  1. portal connect with --responder-endpoint
//  2. Verify deployments are running in both clusters
//  3. Verify portal list shows the tunnel
//  4. portal disconnect
//  5. Verify namespace/resources deleted from both clusters
//  6. Verify portal list shows no tunnels
func TestE2E02_ConnectDisconnectLifecycle(t *testing.T) {
	const namespace = "portal-system"
	home := isolatePortalState(t)
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Step 1: Connect.
	stdout, stderr, err := runPortalWithHome(t, home, "connect", sourceCtx, destCtx,
		"--responder-endpoint", endpoint)
	if err != nil {
		t.Fatalf("portal connect failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Tunnel established") {
		t.Fatalf("expected 'Tunnel established' in output, got: %s", stdout)
	}
	t.Logf("connect output: %s", stdout)

	// Cleanup: always disconnect.
	t.Cleanup(func() {
		runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
		cleanupNamespace(t, namespace)
	})

	// Step 2: Verify deployments.
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	// Step 3: Verify portal list.
	listOut, _, err := runPortalWithHome(t, home, "list")
	if err != nil {
		t.Fatalf("portal list failed: %v", err)
	}
	tunnelName := sourceCtx + "--" + destCtx
	if !strings.Contains(listOut, tunnelName) {
		t.Fatalf("expected tunnel %q in list output: %s", tunnelName, listOut)
	}

	// Step 4: Disconnect.
	stdout, stderr, err = runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
	if err != nil {
		t.Fatalf("portal disconnect failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "disconnected") {
		t.Fatalf("expected 'disconnected' in output, got: %s", stdout)
	}
	t.Logf("disconnect output: %s", stdout)

	// Step 5: Verify resources deleted — deployments should no longer exist.
	_, errSrc := kubectlWithContextErr(t, sourceCtx, "get", "deployment/portal-initiator", "-n", namespace)
	if errSrc == nil {
		t.Fatal("expected initiator deployment to be deleted, but it still exists")
	}
	_, errDst := kubectlWithContextErr(t, destCtx, "get", "deployment/portal-responder", "-n", namespace)
	if errDst == nil {
		t.Fatal("expected responder deployment to be deleted, but it still exists")
	}

	// Step 6: Verify list empty.
	listOut, _, err = runPortalWithHome(t, home, "list")
	if err != nil {
		t.Fatalf("portal list failed: %v", err)
	}
	if !strings.Contains(listOut, "No tunnels found") {
		t.Fatalf("expected 'No tunnels found' after disconnect, got: %s", listOut)
	}

	t.Log("E2E-02 PASSED: connect/disconnect lifecycle verified")
}

// TestE2E03_ConnectWithEndpoint verifies single-phase deploy with explicit endpoint.
func TestE2E03_ConnectWithEndpoint(t *testing.T) {
	const namespace = "portal-system"
	home := isolatePortalState(t)
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Connect with explicit endpoint (single-phase).
	stdout, stderr, err := runPortalWithHome(t, home, "connect", sourceCtx, destCtx,
		"--responder-endpoint", endpoint)
	if err != nil {
		t.Fatalf("portal connect failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Cleanup(func() {
		runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
		cleanupNamespace(t, namespace)
	})

	// Verify both deployments are ready.
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	// Verify the output mentions both sides deployed.
	if !strings.Contains(stdout, "Deployed responder") {
		t.Fatalf("expected 'Deployed responder' in output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Deployed initiator") {
		t.Fatalf("expected 'Deployed initiator' in output, got: %s", stdout)
	}

	t.Log("E2E-03 PASSED: single-phase connect with explicit endpoint")
}

// TestE2E04_ConnectTwoPhase verifies two-phase deploy with LoadBalancer auto-discovery.
func TestE2E04_ConnectTwoPhase(t *testing.T) {
	const namespace = "portal-system"
	home := isolatePortalState(t)

	// Connect WITHOUT --responder-endpoint (two-phase: deploy responder, discover LB, deploy initiator).
	stdout, stderr, err := runPortalWithHome(t, home, "connect", sourceCtx, destCtx)
	if err != nil {
		t.Fatalf("portal connect (two-phase) failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Cleanup(func() {
		runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
		cleanupNamespace(t, namespace)
	})

	// Verify both deployments ready.
	waitForDeployment(t, destCtx, namespace, "portal-responder", 3*time.Minute)
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	// Verify tunnel established output.
	if !strings.Contains(stdout, "Tunnel established") {
		t.Fatalf("expected 'Tunnel established' in output, got: %s", stdout)
	}

	// Verify state recorded.
	listOut, _, err := runPortalWithHome(t, home, "list")
	if err != nil {
		t.Fatalf("portal list failed: %v", err)
	}
	tunnelName := sourceCtx + "--" + destCtx
	if !strings.Contains(listOut, tunnelName) {
		t.Fatalf("expected tunnel in list: %s", listOut)
	}

	t.Log("E2E-04 PASSED: two-phase LB discovery connect")
}

// TestE2E08_DryRun verifies --dry-run prints manifests without deploying.
func TestE2E08_DryRun(t *testing.T) {
	home := isolatePortalState(t)
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Run connect with --dry-run.
	stdout, stderr, err := runPortalWithHome(t, home, "connect", sourceCtx, destCtx,
		"--responder-endpoint", endpoint, "--dry-run")
	if err != nil {
		t.Fatalf("portal connect --dry-run failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Verify manifests were printed.
	if !strings.Contains(stdout, "Source (initiator)") {
		t.Fatalf("expected source manifests in dry-run output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Destination (responder)") {
		t.Fatalf("expected destination manifests in dry-run output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "kind: Deployment") {
		t.Fatalf("expected Deployment manifest in dry-run output, got: %s", stdout)
	}

	// Verify no resources created.
	_, errSrc := kubectlWithContextErr(t, sourceCtx, "get", "deployment/portal-initiator", "-n", "portal-system")
	if errSrc == nil {
		t.Fatal("dry-run should not create resources, but initiator deployment exists")
	}

	// Verify no state file entry.
	listOut, _, _ := runPortalWithHome(t, home, "list")
	if !strings.Contains(listOut, "No tunnels found") {
		t.Fatalf("dry-run should not save state, but list shows: %s", listOut)
	}

	t.Log("E2E-08 PASSED: dry-run prints manifests without side effects")
}

// TestE2E09_CustomNamespace verifies --namespace deploys to a custom namespace.
func TestE2E09_CustomNamespace(t *testing.T) {
	const customNS = "custom-tunnel-ns"
	home := isolatePortalState(t)
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Connect with custom namespace.
	stdout, stderr, err := runPortalWithHome(t, home, "connect", sourceCtx, destCtx,
		"--responder-endpoint", endpoint, "--namespace", customNS)
	if err != nil {
		t.Fatalf("portal connect --namespace failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Cleanup(func() {
		runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
		cleanupNamespace(t, customNS)
	})

	// Verify deployments in custom namespace.
	waitForDeployment(t, destCtx, customNS, "portal-responder", 2*time.Minute)
	waitForDeployment(t, sourceCtx, customNS, "portal-initiator", 2*time.Minute)

	// Verify output references custom namespace.
	if !strings.Contains(stdout, customNS) {
		t.Fatalf("expected custom namespace %q in output, got: %s", customNS, stdout)
	}

	// Verify state records custom namespace.
	listOut, _, err := runPortalWithHome(t, home, "list", "--json")
	if err != nil {
		t.Fatalf("portal list --json failed: %v", err)
	}
	if !strings.Contains(listOut, customNS) {
		t.Fatalf("expected custom namespace in state JSON, got: %s", listOut)
	}

	// Disconnect.
	stdout, stderr, err = runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
	if err != nil {
		t.Fatalf("portal disconnect failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Verify custom namespace resources cleaned up.
	_, errSrc := kubectlWithContextErr(t, sourceCtx, "get", "deployment/portal-initiator", "-n", customNS)
	if errSrc == nil {
		t.Fatal("expected initiator to be deleted from custom namespace")
	}

	t.Log("E2E-09 PASSED: custom namespace connect and cleanup")
}
