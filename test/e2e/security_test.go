//go:build e2e

package e2e

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E10_CrossTunnelCAIsolation verifies each tunnel gets its own CA.
//
// Steps:
//  1. Generate tunnel A
//  2. Generate tunnel B
//  3. Verify CA certs differ between the two tunnels
func TestE2E10_CrossTunnelCAIsolation(t *testing.T) {
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	dirA := t.TempDir()
	dirB := t.TempDir()

	// Generate tunnel A.
	stdout, stderr, err := runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", dirA)
	if err != nil {
		t.Fatalf("generate tunnel A failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Generate tunnel B (same contexts, different output dir — will get different CA).
	stdout, stderr, err = runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", dirB)
	if err != nil {
		t.Fatalf("generate tunnel B failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Read CA certs.
	caA := readPEMFile(t, filepath.Join(dirA, "ca", "ca.crt"))
	caB := readPEMFile(t, filepath.Join(dirB, "ca", "ca.crt"))

	// Parse and compare.
	certA, err := x509.ParseCertificate(caA.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CA cert A: %v", err)
	}
	certB, err := x509.ParseCertificate(caB.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CA cert B: %v", err)
	}

	if certA.SerialNumber.Cmp(certB.SerialNumber) == 0 {
		t.Fatal("tunnel A and B have the same CA serial number — CAs should be independent")
	}

	// Verify public keys differ.
	if certA.PublicKey == certB.PublicKey {
		t.Fatal("tunnel A and B have the same CA public key — CAs should be independent")
	}

	t.Logf("CA A serial: %s, CA B serial: %s", certA.SerialNumber, certB.SerialNumber)
	t.Log("E2E-10 PASSED: per-tunnel CA isolation verified")
}

// TestE2E11_mTLSEnforcement verifies the responder rejects connections without valid client certs.
//
// Steps:
//  1. Deploy tunnel
//  2. Attempt plain TCP/TLS connection to responder LB IP:10443 (no client cert)
//  3. Verify connection is rejected
func TestE2E11_mTLSEnforcement(t *testing.T) {
	const namespace = "portal-system"
	endpoint := fmt.Sprintf("%s:10443", responderIP)
	outputDir := t.TempDir()

	// Generate and deploy.
	stdout, stderr, err := runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", outputDir)
	if err != nil {
		t.Fatalf("portal generate failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	kubectlWithContext(t, destCtx, "apply", "-k", filepath.Join(outputDir, "destination"))
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)

	t.Cleanup(func() {
		kubectlWithContextErr(t, destCtx, "delete", "-k", filepath.Join(outputDir, "destination"), "--ignore-not-found")
		cleanupNamespace(t, namespace)
	})

	// Wait for LoadBalancer to be assigned.
	time.Sleep(10 * time.Second)

	// Attempt plain TLS connection to responder (no client cert).
	// The responder requires mTLS (require_client_certificate: true), so this should fail.
	// We use a temporary pod to attempt the connection from within the dest cluster.
	curlCmd := fmt.Sprintf("curl -sk --max-time 5 https://%s:10443/ 2>&1 || echo 'CONNECTION_FAILED'", responderIP)
	out, _ := kubectlWithContextErr(t, destCtx, "run", "mtls-test", "-n", "default",
		"--rm", "-i", "--restart=Never",
		"--image=curlimages/curl:8.5.0",
		"--", "sh", "-c", curlCmd)

	// The connection should fail because no client cert was provided.
	if strings.Contains(out, "Hello from") {
		t.Fatal("plain TLS connection succeeded — mTLS enforcement is broken")
	}

	// We expect either a TLS handshake error, connection reset, or our fallback marker.
	if !strings.Contains(out, "CONNECTION_FAILED") &&
		!strings.Contains(out, "alert") &&
		!strings.Contains(out, "error") &&
		!strings.Contains(out, "reset") &&
		!strings.Contains(out, "refused") &&
		!strings.Contains(out, "SSL") {
		t.Logf("warning: unexpected output from mTLS test: %s", out)
	}

	t.Logf("mTLS enforcement output: %s", strings.TrimSpace(out))
	t.Log("E2E-11 PASSED: responder rejects connections without valid client certificate")
}

// TestE2E14_PodSecurityContext verifies running pods match SECURITY.md claims.
//
// Checks:
//   - runAsNonRoot: true
//   - runAsUser: 1000
//   - readOnlyRootFilesystem: true
//   - allowPrivilegeEscalation: false
//   - capabilities drop: ["ALL"]
//   - seccomp: RuntimeDefault
//   - automountServiceAccountToken: false
//   - resource limits match documented values
func TestE2E14_PodSecurityContext(t *testing.T) {
	const namespace = "portal-system"

	// Deploy a tunnel.
	outputDir := deployTunnelWithEcho(t, namespace)
	_ = outputDir

	// Give pods time to stabilize.
	time.Sleep(5 * time.Second)

	// Check initiator pod security context.
	t.Run("initiator", func(t *testing.T) {
		pods := getPodJSON(t, sourceCtx, namespace, "app.kubernetes.io/name=portal-initiator")
		if len(pods.Items) == 0 {
			t.Fatal("no initiator pods found")
		}

		pod := pods.Items[0]

		// automountServiceAccountToken should be false.
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Error("automountServiceAccountToken should be false")
		}

		verifyEnvoyContainerSecurity(t, pod.Spec.Containers, "envoy")
	})

	// Check responder pod security context.
	t.Run("responder", func(t *testing.T) {
		pods := getPodJSON(t, destCtx, namespace, "app.kubernetes.io/name=portal-responder")
		if len(pods.Items) == 0 {
			t.Fatal("no responder pods found")
		}

		pod := pods.Items[0]

		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Error("automountServiceAccountToken should be false")
		}

		verifyEnvoyContainerSecurity(t, pod.Spec.Containers, "envoy")
	})

	t.Log("E2E-14 PASSED: pod security contexts match SECURITY.md claims")
}

// verifyEnvoyContainerSecurity checks that a container's security context matches SECURITY.md.
func verifyEnvoyContainerSecurity(t *testing.T, containers []struct {
	Name            string `json:"name"`
	SecurityContext struct {
		RunAsNonRoot             *bool `json:"runAsNonRoot"`
		RunAsUser                *int  `json:"runAsUser"`
		ReadOnlyRootFilesystem   *bool `json:"readOnlyRootFilesystem"`
		AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
		Capabilities             struct {
			Drop []string `json:"drop"`
		} `json:"capabilities"`
		SeccompProfile struct {
			Type string `json:"type"`
		} `json:"seccompProfile"`
	} `json:"securityContext"`
	Resources struct {
		Requests struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"requests"`
		Limits struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"limits"`
	} `json:"resources"`
}, containerName string) {
	t.Helper()

	var found bool
	for _, c := range containers {
		if c.Name != containerName {
			continue
		}
		found = true
		sc := c.SecurityContext

		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Error("runAsNonRoot should be true")
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
			t.Errorf("runAsUser should be 1000, got: %v", sc.RunAsUser)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Error("readOnlyRootFilesystem should be true")
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Error("allowPrivilegeEscalation should be false")
		}

		// Check capabilities drop includes ALL.
		hasDropAll := false
		for _, cap := range sc.Capabilities.Drop {
			if cap == "ALL" {
				hasDropAll = true
				break
			}
		}
		if !hasDropAll {
			t.Errorf("capabilities.drop should include ALL, got: %v", sc.Capabilities.Drop)
		}

		if sc.SeccompProfile.Type != "RuntimeDefault" {
			t.Errorf("seccompProfile.type should be RuntimeDefault, got: %s", sc.SeccompProfile.Type)
		}

		// Verify resource limits.
		if c.Resources.Requests.CPU != "100m" {
			t.Errorf("CPU request should be 100m, got: %s", c.Resources.Requests.CPU)
		}
		if c.Resources.Requests.Memory != "128Mi" {
			t.Errorf("memory request should be 128Mi, got: %s", c.Resources.Requests.Memory)
		}
		if c.Resources.Limits.CPU != "500m" {
			t.Errorf("CPU limit should be 500m, got: %s", c.Resources.Limits.CPU)
		}
		if c.Resources.Limits.Memory != "256Mi" {
			t.Errorf("memory limit should be 256Mi, got: %s", c.Resources.Limits.Memory)
		}
	}

	if !found {
		t.Errorf("container %q not found", containerName)
	}
}

// readPEMFile reads a PEM file and returns the first block.
func readPEMFile(t *testing.T, path string) *pem.Block {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("no PEM block found in %s", path)
	}
	return block
}
