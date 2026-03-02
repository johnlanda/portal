//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sourceCluster = "portal-e2e-source"
	destCluster   = "portal-e2e-destination"
	metallbVer    = "v0.14.9"
)

// portalBin is the path to the compiled portal binary. Set in TestMain.
var portalBin string

// responderIP is the MetalLB-assigned LoadBalancer IP. Set in TestMain.
var responderIP string

// sourceCtx and destCtx are the KIND kube context names.
var (
	sourceCtx = "kind-" + sourceCluster
	destCtx   = "kind-" + destCluster
)

func TestMain(m *testing.M) {
	if err := checkPrerequisites(); err != nil {
		fmt.Fprintf(os.Stderr, "prerequisite check failed: %v\n", err)
		os.Exit(1)
	}

	portalBinary, err := buildPortal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build portal: %v\n", err)
		os.Exit(1)
	}
	portalBin = portalBinary
	// Clean up the temp directory holding the compiled binary after tests.
	defer os.RemoveAll(filepath.Dir(portalBin))

	if err := ensureCluster(sourceCluster); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create source cluster: %v\n", err)
		os.Exit(1)
	}
	if err := ensureCluster(destCluster); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create destination cluster: %v\n", err)
		os.Exit(1)
	}

	if err := installMetalLB(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to install MetalLB: %v\n", err)
		os.Exit(1)
	}

	ip, err := configureMetalLB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure MetalLB: %v\n", err)
		os.Exit(1)
	}
	responderIP = ip

	code := m.Run()

	if os.Getenv("E2E_KEEP_CLUSTERS") != "1" {
		_ = deleteCluster(sourceCluster)
		_ = deleteCluster(destCluster)
	}

	os.Exit(code)
}

// checkPrerequisites verifies docker, kind, kubectl, and go are on PATH.
func checkPrerequisites() error {
	for _, cmd := range []string{"docker", "kind", "kubectl", "go"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("%s is required but not found in PATH", cmd)
		}
	}
	return nil
}

// buildPortal compiles the portal binary to a temp directory.
func buildPortal() (string, error) {
	tmpDir, err := os.MkdirTemp("", "portal-e2e-*")
	if err != nil {
		return "", err
	}
	binPath := filepath.Join(tmpDir, "portal")
	rootDir := findRootDir()
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/portal")
	cmd.Dir = rootDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build failed: %w", err)
	}
	return binPath, nil
}

// findRootDir walks up from the test directory to find the module root (where go.mod lives).
func findRootDir() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Fallback: assume two levels up from test/e2e.
			wd, _ := os.Getwd()
			return filepath.Join(wd, "..", "..")
		}
		dir = parent
	}
}

// ensureCluster creates a KIND cluster if it doesn't already exist.
func ensureCluster(name string) error {
	out, _ := exec.Command("kind", "get", "clusters").Output()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return nil // already exists
		}
	}
	cmd := exec.Command("kind", "create", "cluster", "--name", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// deleteCluster removes a KIND cluster.
func deleteCluster(name string) error {
	cmd := exec.Command("kind", "delete", "cluster", "--name", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// installMetalLB installs MetalLB on the destination cluster and waits for readiness.
func installMetalLB() error {
	url := fmt.Sprintf("https://raw.githubusercontent.com/metallb/metallb/%s/config/manifests/metallb-native.yaml", metallbVer)
	cmd := exec.Command("kubectl", "apply", "-f", url, "--context", destCtx)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply metallb failed: %w", err)
	}

	cmd = exec.Command("kubectl", "wait", "deployment/controller", "-n", "metallb-system",
		"--for=condition=Available", "--timeout=120s", "--context", destCtx)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("metallb controller not ready: %w", err)
	}
	return nil
}

// configureMetalLB derives a LoadBalancer IP from the KIND Docker subnet and
// configures an IPAddressPool + L2Advertisement.
func configureMetalLB() (string, error) {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		destCluster+"-control-plane").Output()
	if err != nil {
		return "", fmt.Errorf("failed to get node IP: %w", err)
	}

	nodeIP := strings.TrimSpace(string(out))
	parts := strings.Split(nodeIP, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("unexpected node IP format: %s", nodeIP)
	}
	lbIP := fmt.Sprintf("%s.%s.255.200", parts[0], parts[1])

	manifest := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: portal-pool
  namespace: metallb-system
spec:
  addresses:
    - %s/32
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: portal-l2
  namespace: metallb-system
spec:
  ipAddressPools:
    - portal-pool
`, lbIP)

	cmd := exec.Command("kubectl", "apply", "--context", destCtx, "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to configure MetalLB: %w", err)
	}

	return lbIP, nil
}
