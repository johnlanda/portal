//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E01_GenerateDeployVerify exercises the full generate + deploy workflow.
//
// Steps:
//  1. portal generate with --responder-endpoint and --output-dir
//  2. Verify directory structure: source/, destination/, ca/, tunnel.yaml
//  3. Inject echo-server sidecar into responder deployment
//  4. kubectl apply -k destination/, then source/
//  5. Wait for both deployments to be Available
//  6. Port-forward to initiator admin (15000), verify tunnel_to_responder stats
//  7. Port-forward to initiator tunnel port (10443), verify echo response
func TestE2E01_GenerateDeployVerify(t *testing.T) {
	const namespace = "portal-system"
	outputDir := t.TempDir()
	endpoint := responderIP + ":10443"

	// Step 1: Generate manifests.
	stdout, stderr, err := runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", outputDir)
	if err != nil {
		t.Fatalf("portal generate failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Logf("generate output: %s", stdout)

	// Step 2: Verify directory structure.
	for _, path := range []string{
		filepath.Join(outputDir, "source"),
		filepath.Join(outputDir, "destination"),
		filepath.Join(outputDir, "ca"),
		filepath.Join(outputDir, "tunnel.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected path %s to exist: %v", path, err)
		}
	}

	// Verify kustomization.yaml files exist.
	for _, dir := range []string{"source", "destination"} {
		kustPath := filepath.Join(outputDir, dir, "kustomization.yaml")
		if _, err := os.Stat(kustPath); err != nil {
			t.Fatalf("expected %s to exist: %v", kustPath, err)
		}
	}

	// Step 3: Inject echo-server sidecar.
	injectEchoSidecar(t, outputDir)

	// Step 4: Deploy responder first, then initiator.
	kubectlWithContext(t, destCtx, "apply", "-k", filepath.Join(outputDir, "destination"))
	t.Cleanup(func() {
		kubectlWithContextErr(t, destCtx, "delete", "-k", filepath.Join(outputDir, "destination"), "--ignore-not-found")
	})

	// Step 5: Wait for responder.
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)

	kubectlWithContext(t, sourceCtx, "apply", "-k", filepath.Join(outputDir, "source"))
	t.Cleanup(func() {
		kubectlWithContextErr(t, sourceCtx, "delete", "-k", filepath.Join(outputDir, "source"), "--ignore-not-found")
	})

	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	// Give the tunnel a moment to establish.
	time.Sleep(5 * time.Second)

	// Step 6: Verify Envoy stats.
	verifyEnvoyStats(t, namespace)

	// Step 7: Verify data through tunnel.
	body := portForwardAndGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
	if !strings.Contains(body, "Hello from the destination cluster through the Portal tunnel!") {
		t.Fatalf("unexpected tunnel response: %q", body)
	}
	t.Logf("E2E-01 PASSED: echo response received through tunnel: %s", strings.TrimSpace(body))
}
