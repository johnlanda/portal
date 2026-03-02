//go:build e2e

package e2e

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TODO: Add E2E test for cert-manager mode. This requires installing cert-manager
// in the KIND clusters (e.g., via kubectl apply of the cert-manager release manifest)
// and verifying that:
//   - portal connect --cert-manager creates Certificate and Issuer CRDs
//   - cert-manager provisions the portal-tunnel-tls Secret from the Certificate CR
//   - the tunnel establishes successfully using cert-manager-provisioned certs
//   - portal rotate-certs is correctly blocked in cert-manager mode
//
// This is deferred because it adds significant infrastructure complexity to the
// E2E test setup (cert-manager installation, CRD readiness checks, webhook availability).

// TestE2E06_CertificateRotation verifies portal rotate-certs produces working certificates.
//
// Steps:
//  1. Generate and deploy tunnel
//  2. Record original cert serial numbers
//  3. portal rotate-certs <tunnel-dir>
//  4. Verify new cert serial numbers differ
//  5. Apply updated secrets and restart deployments
//  6. Verify tunnel still functions after rotation
func TestE2E06_CertificateRotation(t *testing.T) {
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

	// Deploy.
	kubectlWithContext(t, destCtx, "apply", "-k", filepath.Join(outputDir, "destination"))
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)

	kubectlWithContext(t, sourceCtx, "apply", "-k", filepath.Join(outputDir, "source"))
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	t.Cleanup(func() {
		kubectlWithContextErr(t, sourceCtx, "delete", "-k", filepath.Join(outputDir, "source"), "--ignore-not-found")
		kubectlWithContextErr(t, destCtx, "delete", "-k", filepath.Join(outputDir, "destination"), "--ignore-not-found")
		cleanupNamespace(t, namespace)
	})

	// Give tunnel time to establish.
	time.Sleep(5 * time.Second)

	// Step 2: Record original cert serial numbers.
	origSourceSerial := readCertSerial(t, filepath.Join(outputDir, "source", "portal-tunnel-tls-secret.yaml"))
	origDestSerial := readCertSerial(t, filepath.Join(outputDir, "destination", "portal-tunnel-tls-secret.yaml"))
	t.Logf("original source cert serial: %s", origSourceSerial)
	t.Logf("original dest cert serial: %s", origDestSerial)

	// Step 3: Rotate certificates.
	stdout, stderr, err = runPortal(t, "rotate-certs", outputDir)
	if err != nil {
		t.Fatalf("portal rotate-certs failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Logf("rotate-certs output: %s", stdout)

	if !strings.Contains(stdout, "Rotated certificates") {
		t.Fatalf("expected 'Rotated certificates' in output, got: %s", stdout)
	}

	// Step 4: Verify new serial numbers differ.
	newSourceSerial := readCertSerial(t, filepath.Join(outputDir, "source", "portal-tunnel-tls-secret.yaml"))
	newDestSerial := readCertSerial(t, filepath.Join(outputDir, "destination", "portal-tunnel-tls-secret.yaml"))
	t.Logf("new source cert serial: %s", newSourceSerial)
	t.Logf("new dest cert serial: %s", newDestSerial)

	if origSourceSerial == newSourceSerial {
		t.Fatal("source cert serial did not change after rotation")
	}
	if origDestSerial == newDestSerial {
		t.Fatal("destination cert serial did not change after rotation")
	}

	// Step 5: Apply updated secrets and restart.
	kubectlWithContext(t, destCtx, "apply", "-f",
		filepath.Join(outputDir, "destination", "portal-tunnel-tls-secret.yaml"))
	kubectlWithContext(t, sourceCtx, "apply", "-f",
		filepath.Join(outputDir, "source", "portal-tunnel-tls-secret.yaml"))

	// Restart both deployments.
	kubectlWithContext(t, destCtx, "rollout", "restart", "deployment/portal-responder", "-n", namespace)
	kubectlWithContext(t, sourceCtx, "rollout", "restart", "deployment/portal-initiator", "-n", namespace)

	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	// Step 6: Give tunnel time to re-establish, then verify data path.
	time.Sleep(10 * time.Second)

	body := portForwardAndGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
	if !strings.Contains(body, "Hello from the destination") {
		t.Fatalf("tunnel not working after cert rotation, got: %s", body)
	}

	t.Log("E2E-06 PASSED: certificate rotation and tunnel recovery verified")
}

// readCertSerial extracts the first certificate from a Secret YAML file's tls.crt
// field and returns its serial number as a string.
func readCertSerial(t *testing.T, secretPath string) string {
	t.Helper()

	data, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("failed to read secret file %s: %v", secretPath, err)
	}

	content := string(data)

	// Find tls.crt value in the YAML.
	tlsCrtKey := "tls.crt:"
	idx := strings.Index(content, tlsCrtKey)
	if idx < 0 {
		t.Fatalf("tls.crt not found in %s", secretPath)
	}

	// Extract the base64 value after "tls.crt:".
	rest := content[idx+len(tlsCrtKey):]
	rest = strings.TrimSpace(rest)
	// Take everything until we hit a line that contains ":" (next YAML key) or is empty.
	lines := strings.Split(rest, "\n")
	var b64Parts []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.Contains(trimmed, ":") {
			break
		}
		b64Parts = append(b64Parts, trimmed)
	}
	b64 := strings.Join(b64Parts, "")

	// Decode base64.
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("failed to decode base64 tls.crt: %v", err)
	}

	// Parse PEM.
	block, _ := pem.Decode(decoded)
	if block == nil {
		t.Fatalf("failed to decode PEM from tls.crt in %s", secretPath)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	return cert.SerialNumber.String()
}
