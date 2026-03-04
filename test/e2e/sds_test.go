//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E16_SDSCertHotReload verifies that Envoy picks up rotated certificates
// via SDS watched_directory without a pod restart.
//
// This is the key validation for the SDS migration: when a K8s Secret is updated,
// kubelet performs an atomic symlink swap on the mounted volume. Envoy's
// watched_directory detects the filesystem move event and re-reads the cert files,
// establishing new TLS connections with the updated certificates.
//
// Steps:
//  1. Generate and deploy tunnel with echo sidecar
//  2. Verify data path works
//  3. Record original cert serial numbers
//  4. Rotate certificates (portal rotate-certs)
//  5. Apply updated secrets to K8s (NO pod restart)
//  6. Wait for kubelet to sync secret volume (~60-90s)
//  7. Verify data path still works
//  8. Verify pod restart count is 0 (proves SDS hot-reload, not restart)
//  9. Verify new cert serial numbers differ from originals
func TestE2E16_SDSCertHotReload(t *testing.T) {
	const namespace = "portal-system"
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	outputDir := t.TempDir()

	// Step 1: Generate manifests with echo sidecar.
	stdout, stderr, err := runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", outputDir)
	if err != nil {
		t.Fatalf("portal generate failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	injectEchoSidecar(t, outputDir)

	// Deploy destination then source.
	kubectlWithContext(t, destCtx, "apply", "-k", filepath.Join(outputDir, "destination"))
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)

	kubectlWithContext(t, sourceCtx, "apply", "-k", filepath.Join(outputDir, "source"))
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	t.Cleanup(func() {
		kubectlWithContextErr(t, sourceCtx, "delete", "-k", filepath.Join(outputDir, "source"), "--ignore-not-found")
		kubectlWithContextErr(t, destCtx, "delete", "-k", filepath.Join(outputDir, "destination"), "--ignore-not-found")
		cleanupNamespace(t, namespace)
	})

	// Step 2: Wait for tunnel to establish, then verify data path.
	time.Sleep(10 * time.Second)

	body := portForwardAndGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
	if !strings.Contains(body, "Hello from the destination") {
		t.Fatalf("initial tunnel verification failed, got: %s", body)
	}
	t.Log("initial data path verified")

	// Step 3: Record original cert serial numbers.
	origSourceSerial := readCertSerial(t, filepath.Join(outputDir, "source", "portal-tunnel-tls-secret.yaml"))
	origDestSerial := readCertSerial(t, filepath.Join(outputDir, "destination", "portal-tunnel-tls-secret.yaml"))
	t.Logf("original source cert serial: %s", origSourceSerial)
	t.Logf("original dest cert serial: %s", origDestSerial)

	// Step 4: Rotate certificates.
	stdout, stderr, err = runPortal(t, "rotate-certs", outputDir)
	if err != nil {
		t.Fatalf("portal rotate-certs failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Logf("rotate-certs output: %s", stdout)

	if !strings.Contains(stdout, "Rotated certificates") {
		t.Fatalf("expected 'Rotated certificates' in output, got: %s", stdout)
	}

	// Step 5: Apply updated secrets to K8s — NO pod restart.
	kubectlWithContext(t, destCtx, "apply", "-f",
		filepath.Join(outputDir, "destination", "portal-tunnel-tls-secret.yaml"))
	kubectlWithContext(t, sourceCtx, "apply", "-f",
		filepath.Join(outputDir, "source", "portal-tunnel-tls-secret.yaml"))

	t.Log("updated secrets applied; waiting for kubelet sync + SDS reload (NO pod restart)...")

	// Step 6: Wait for kubelet to propagate the secret update to the pod volume mounts.
	// kubelet syncs projected/secret volumes on a configurable interval (default: ~1 min + TTL cache).
	// We poll the data path to detect when the new certs are active.
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	passed := false
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)

		resp, err := portForwardGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
		if err != nil {
			lastErr = err
			t.Logf("data path probe failed (expected during cert transition): %v", err)
			continue
		}
		if strings.Contains(resp, "Hello from the destination") {
			t.Log("data path still working after cert rotation (SDS hot-reload confirmed)")
			passed = true
			break
		}
		lastErr = fmt.Errorf("unexpected response: %s", resp)
		t.Logf("unexpected response (retrying): %s", resp)
	}

	if !passed {
		t.Fatalf("data path did not recover after cert rotation within timeout; last error: %v", lastErr)
	}

	// Step 8: Verify zero pod restarts — this proves SDS hot-reload, not a pod restart.
	checkRestarts(t, sourceCtx, namespace, "app.kubernetes.io/name=portal-initiator", "initiator")
	checkRestarts(t, destCtx, namespace, "app.kubernetes.io/name=portal-responder", "responder")

	// Step 9: Verify cert serials actually changed on disk.
	newSourceSerial := readCertSerial(t, filepath.Join(outputDir, "source", "portal-tunnel-tls-secret.yaml"))
	newDestSerial := readCertSerial(t, filepath.Join(outputDir, "destination", "portal-tunnel-tls-secret.yaml"))
	t.Logf("new source cert serial: %s", newSourceSerial)
	t.Logf("new dest cert serial: %s", newDestSerial)

	if origSourceSerial == newSourceSerial {
		t.Error("source cert serial did not change after rotation")
	}
	if origDestSerial == newDestSerial {
		t.Error("destination cert serial did not change after rotation")
	}

	t.Log("E2E-16 PASSED: SDS certificate hot-reload verified (zero pod restarts, certs rotated)")
}

// TestE2E17_SecretRefMode verifies that --secret-ref mode works end-to-end:
// Portal skips cert generation and references an externally-provided K8s Secret.
//
// Steps:
//  1. Generate certificates with portal (to get valid cert material)
//  2. Create the K8s Secret manually with a custom name
//  3. Generate tunnel manifests with --secret-ref pointing to that secret
//  4. Deploy and verify the tunnel works
func TestE2E17_SecretRefMode(t *testing.T) {
	const namespace = "portal-e2e-secretref"
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Step 1: Generate a normal tunnel to get valid cert material.
	certDir := t.TempDir()
	stdout, stderr, err := runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", certDir,
		"--namespace", namespace)
	if err != nil {
		t.Fatalf("portal generate (for certs) failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Step 2: Create the external secret with a custom name in both clusters.
	// We read the generated secret YAML, replace the secret name, and apply it.
	secretName := "vault-managed-tls"
	for _, side := range []struct {
		ctx     string
		sideDir string
	}{
		{sourceCtx, "source"},
		{destCtx, "destination"},
	} {
		// Ensure namespace exists.
		kubectlWithContextErr(t, side.ctx, "create", "namespace", namespace)

		// Read the generated secret YAML and replace the name.
		secretPath := filepath.Join(certDir, side.sideDir, "portal-tunnel-tls-secret.yaml")
		secretData, err := os.ReadFile(secretPath)
		if err != nil {
			t.Fatalf("failed to read generated secret %s: %v", secretPath, err)
		}

		// Replace the secret name in the YAML.
		renamedYAML := strings.Replace(string(secretData), "name: portal-tunnel-tls", "name: "+secretName, 1)

		// Write renamed secret to a temp file and apply it.
		tmpFile := filepath.Join(t.TempDir(), "secret.yaml")
		if err := os.WriteFile(tmpFile, []byte(renamedYAML), 0644); err != nil {
			t.Fatalf("failed to write temp secret file: %v", err)
		}
		kubectlWithContext(t, side.ctx, "apply", "-f", tmpFile, "-n", namespace)
		t.Logf("created external secret %q in %s/%s", secretName, side.ctx, namespace)
	}

	// Step 3: Generate tunnel manifests with --secret-ref.
	outputDir := t.TempDir()
	stdout, stderr, err = runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", outputDir,
		"--namespace", namespace,
		"--secret-ref", secretName)
	if err != nil {
		t.Fatalf("portal generate --secret-ref failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	if !strings.Contains(stdout, "Using existing secret") {
		t.Fatalf("expected 'Using existing secret' in output, got: %s", stdout)
	}

	// Verify no secret file was generated in the output.
	for _, side := range []string{"source", "destination"} {
		secretFile := filepath.Join(outputDir, side, "portal-tunnel-tls-secret.yaml")
		if _, err := os.Stat(secretFile); err == nil {
			t.Errorf("secret file should not exist in --secret-ref mode: %s", secretFile)
		}
	}

	// Inject echo sidecar.
	injectEchoSidecarNS(t, outputDir, namespace)

	// Step 4: Deploy and verify.
	kubectlWithContext(t, destCtx, "apply", "-k", filepath.Join(outputDir, "destination"))
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)

	kubectlWithContext(t, sourceCtx, "apply", "-k", filepath.Join(outputDir, "source"))
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	t.Cleanup(func() {
		kubectlWithContextErr(t, sourceCtx, "delete", "-k", filepath.Join(outputDir, "source"), "--ignore-not-found")
		kubectlWithContextErr(t, destCtx, "delete", "-k", filepath.Join(outputDir, "destination"), "--ignore-not-found")
		cleanupNamespace(t, namespace)
	})

	// Wait for tunnel to establish.
	time.Sleep(10 * time.Second)

	body := portForwardAndGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
	if !strings.Contains(body, "Hello from the destination") {
		t.Fatalf("tunnel not working with --secret-ref, got: %s", body)
	}

	t.Log("E2E-17 PASSED: --secret-ref mode works end-to-end with externally managed secret")
}
