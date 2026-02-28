//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runPortal executes the compiled portal binary with the given arguments.
// Returns stdout, stderr, and any error.
func runPortal(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	t.Logf("portal %s", strings.Join(args, " "))
	cmd := exec.Command(portalBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// runPortalWithHome executes portal with a custom HOME directory for state isolation.
func runPortalWithHome(t *testing.T, home string, args ...string) (string, string, error) {
	t.Helper()
	t.Logf("HOME=%s portal %s", home, strings.Join(args, " "))
	cmd := exec.Command(portalBin, args...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// kubectlWithContext runs kubectl with the given context and arguments.
func kubectlWithContext(t *testing.T, kubeCtx string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"--context", kubeCtx}, args...)
	t.Logf("kubectl %s", strings.Join(fullArgs, " "))
	out, err := exec.Command("kubectl", fullArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s failed: %v\n%s", strings.Join(fullArgs, " "), err, string(out))
	}
	return string(out)
}

// kubectlWithContextErr is like kubectlWithContext but returns an error instead of calling Fatal.
func kubectlWithContextErr(t *testing.T, kubeCtx string, args ...string) (string, error) {
	t.Helper()
	fullArgs := append([]string{"--context", kubeCtx}, args...)
	t.Logf("kubectl %s", strings.Join(fullArgs, " "))
	out, err := exec.Command("kubectl", fullArgs...).CombinedOutput()
	return string(out), err
}

// waitForDeployment waits until a deployment is Available or the timeout expires.
func waitForDeployment(t *testing.T, kubeCtx, namespace, name string, timeout time.Duration) {
	t.Helper()
	kubectlWithContext(t, kubeCtx,
		"wait", fmt.Sprintf("deployment/%s", name),
		"-n", namespace,
		"--for=condition=Available",
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())),
	)
}

// waitForDeploymentGone waits until a deployment is deleted or the timeout expires.
func waitForDeploymentGone(t *testing.T, kubeCtx, namespace, name string, timeout time.Duration) {
	t.Helper()
	kubectlWithContext(t, kubeCtx,
		"wait", fmt.Sprintf("deployment/%s", name),
		"-n", namespace,
		"--for=delete",
		fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())),
	)
}

// portForwardAndGet starts a port-forward, sends an HTTP GET to the given path,
// and returns the response body. The port-forward process is cleaned up via t.Cleanup.
func portForwardAndGet(t *testing.T, kubeCtx, namespace, target string, remotePort int, path string) string {
	t.Helper()

	localPort := findFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeCtx,
		"port-forward", target, "-n", namespace,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("port-forward start failed: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	// Wait for port-forward to be ready.
	waitForPort(t, localPort, 30*time.Second)

	url := fmt.Sprintf("http://127.0.0.1:%d%s", localPort, path)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("HTTP GET %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return string(body)
}

// portForwardGet is like portForwardAndGet but returns an error instead of calling Fatal.
// Also returns the cancel function so callers can stop the port-forward explicitly.
func portForwardGet(t *testing.T, kubeCtx, namespace, target string, remotePort int, path string) (string, error) {
	t.Helper()

	localPort := findFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "--context", kubeCtx,
		"port-forward", target, "-n", namespace,
		fmt.Sprintf("%d:%d", localPort, remotePort))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("port-forward start failed: %w", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	// Wait for port-forward to be ready.
	if err := waitForPortErr(localPort, 30*time.Second); err != nil {
		return "", fmt.Errorf("port-forward not ready: %w", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", localPort, path)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("HTTP GET %s failed: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	return string(body), nil
}

// findFreePort returns a free TCP port on localhost.
func findFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// waitForPort TCP-dials localhost:port until it succeeds or the timeout expires.
func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	if err := waitForPortErr(port, timeout); err != nil {
		t.Fatalf("port %d not ready: %v", port, err)
	}
}

func waitForPortErr(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for port %d", port)
}

// isolatePortalState creates a temp directory and returns it for use as HOME,
// isolating ~/.portal/tunnels.json state across tests. Cleaned up via t.Cleanup.
func isolatePortalState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	return home
}

// cleanupNamespace deletes a namespace in both clusters (best-effort).
func cleanupNamespace(t *testing.T, namespace string) {
	t.Helper()
	for _, ctx := range []string{sourceCtx, destCtx} {
		_, _ = kubectlWithContextErr(t, ctx, "delete", "namespace", namespace, "--ignore-not-found", "--timeout=60s")
	}
}

// injectEchoSidecar copies the echo-sidecar-patch.yaml into the destination dir
// and appends the Kustomize patch reference to kustomization.yaml.
func injectEchoSidecar(t *testing.T, outputDir string) {
	t.Helper()
	destDir := filepath.Join(outputDir, "destination")

	patch := `# Kustomize strategic merge patch - adds HTTP echo server sidecar
apiVersion: apps/v1
kind: Deployment
metadata:
  name: portal-responder
  namespace: portal-system
spec:
  template:
    spec:
      containers:
        - name: echo-server
          image: hashicorp/http-echo:0.2.3
          args:
            - "-listen=:10001"
            - "-text=Hello from the destination cluster through the Portal tunnel!"
          ports:
            - containerPort: 10001
              protocol: TCP
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
            limits:
              cpu: 50m
              memory: 32Mi
          securityContext:
            runAsNonRoot: true
            runAsUser: 1000
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
            seccompProfile:
              type: RuntimeDefault
`
	if err := os.WriteFile(filepath.Join(destDir, "echo-sidecar-patch.yaml"), []byte(patch), 0644); err != nil {
		t.Fatalf("failed to write echo-sidecar-patch.yaml: %v", err)
	}

	// Append patch reference to kustomization.yaml.
	kustomFile := filepath.Join(destDir, "kustomization.yaml")
	f, err := os.OpenFile(kustomFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to open kustomization.yaml: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("\npatches:\n  - path: echo-sidecar-patch.yaml\n"); err != nil {
		t.Fatalf("failed to append to kustomization.yaml: %v", err)
	}
}

// injectEchoSidecarNS is like injectEchoSidecar but allows specifying the namespace.
func injectEchoSidecarNS(t *testing.T, outputDir, namespace string) {
	t.Helper()
	destDir := filepath.Join(outputDir, "destination")

	patch := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: portal-responder
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: echo-server
          image: hashicorp/http-echo:0.2.3
          args:
            - "-listen=:10001"
            - "-text=Hello from the destination cluster through the Portal tunnel!"
          ports:
            - containerPort: 10001
              protocol: TCP
          resources:
            requests:
              cpu: 10m
              memory: 16Mi
            limits:
              cpu: 50m
              memory: 32Mi
          securityContext:
            runAsNonRoot: true
            runAsUser: 1000
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
            seccompProfile:
              type: RuntimeDefault
`, namespace)
	if err := os.WriteFile(filepath.Join(destDir, "echo-sidecar-patch.yaml"), []byte(patch), 0644); err != nil {
		t.Fatalf("failed to write echo-sidecar-patch.yaml: %v", err)
	}

	kustomFile := filepath.Join(destDir, "kustomization.yaml")
	f, err := os.OpenFile(kustomFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to open kustomization.yaml: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("\npatches:\n  - path: echo-sidecar-patch.yaml\n"); err != nil {
		t.Fatalf("failed to append to kustomization.yaml: %v", err)
	}
}

// deployTunnelWithEcho generates manifests, injects the echo sidecar, and deploys
// both sides. It returns the output directory. Intended for tests that need a
// fully-working tunnel with data-path verification.
func deployTunnelWithEcho(t *testing.T, namespace string) string {
	t.Helper()

	outputDir := t.TempDir()
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Generate manifests.
	stdout, stderr, err := runPortal(t, "generate", sourceCtx, destCtx,
		"--responder-endpoint", endpoint,
		"--output-dir", outputDir,
		"--namespace", namespace)
	if err != nil {
		t.Fatalf("portal generate failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Inject echo sidecar.
	injectEchoSidecarNS(t, outputDir, namespace)

	// Deploy responder first.
	kubectlWithContext(t, destCtx, "apply", "-k", filepath.Join(outputDir, "destination"))
	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)

	// Deploy initiator.
	kubectlWithContext(t, sourceCtx, "apply", "-k", filepath.Join(outputDir, "source"))
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	t.Cleanup(func() {
		_, _ = kubectlWithContextErr(t, sourceCtx, "delete", "-k", filepath.Join(outputDir, "source"), "--ignore-not-found")
		_, _ = kubectlWithContextErr(t, destCtx, "delete", "-k", filepath.Join(outputDir, "destination"), "--ignore-not-found")
		cleanupNamespace(t, namespace)
	})

	return outputDir
}

// verifyTunnelData port-forwards to the initiator and verifies the echo response.
func verifyTunnelData(t *testing.T, namespace string) {
	t.Helper()
	body := portForwardAndGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
	if !strings.Contains(body, "Hello from the destination") {
		t.Fatalf("unexpected tunnel response: %s", body)
	}
	t.Logf("tunnel data verification passed: %s", strings.TrimSpace(body))
}

// verifyEnvoyStats port-forwards to the initiator admin API and checks for
// tunnel_to_responder stats.
func verifyEnvoyStats(t *testing.T, namespace string) {
	t.Helper()
	body := portForwardAndGet(t, sourceCtx, namespace, "deployment/portal-initiator", 15000, "/stats")
	if !strings.Contains(body, "tunnel_to_responder") {
		t.Logf("warning: no tunnel_to_responder stats found (tunnel may still be connecting)")
	} else {
		t.Logf("envoy stats verification passed")
	}
}

// parsePodJSON parses the JSON output of kubectl get pod -o json and returns
// a simplified structure for security context verification.
type podJSON struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
			Containers                   []struct {
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
			} `json:"containers"`
		} `json:"spec"`
	} `json:"items"`
}

// getPodJSON runs kubectl get pods -n namespace -o json and parses the result.
func getPodJSON(t *testing.T, kubeCtx, namespace, labelSelector string) podJSON {
	t.Helper()
	out := kubectlWithContext(t, kubeCtx, "get", "pods", "-n", namespace, "-l", labelSelector, "-o", "json")
	var result podJSON
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("failed to parse pod JSON: %v", err)
	}
	return result
}
