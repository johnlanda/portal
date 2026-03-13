//go:build e2e_baremetal

package e2e_baremetal

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
)

// portalBin is the path to the compiled portal binary. Set in TestMain.
var portalBin string

func TestMain(m *testing.M) {
	// Check func-e is on PATH.
	if _, err := exec.LookPath("func-e"); err != nil {
		fmt.Fprintln(os.Stderr, "func-e not found on PATH, skipping bare metal E2E tests")
		os.Exit(0)
	}

	// Pre-download Envoy so tests don't timeout waiting for it.
	fmt.Fprintln(os.Stderr, "pre-downloading Envoy via func-e...")
	cmd := exec.Command("func-e", "run", "--version")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "func-e run --version failed: %v\n", err)
		os.Exit(1)
	}

	// Build portal binary for CLI test.
	bin, err := buildPortal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build portal: %v\n", err)
		os.Exit(1)
	}
	portalBin = bin
	defer os.RemoveAll(filepath.Dir(portalBin))

	os.Exit(m.Run())
}

// --- Helpers ---

// startTCPEchoServer starts a TCP server that echoes back data with an optional
// prefix. Returns the address and port. The listener is closed via t.Cleanup.
func startTCPEchoServer(t *testing.T, prefix string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start TCP echo server: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if prefix == "" {
					io.Copy(c, c)
				} else {
					buf := make([]byte, 4096)
					for {
						n, err := c.Read(buf)
						if err != nil {
							return
						}
						_, werr := c.Write([]byte(prefix + string(buf[:n])))
						if werr != nil {
							return
						}
					}
				}
			}(conn)
		}
	}()

	return fmt.Sprintf("127.0.0.1:%d", port), port
}

// startEnvoy runs func-e with the given Envoy config. The process is killed
// via SIGTERM on t.Cleanup. Stdout/stderr are written to the test log.
func startEnvoy(t *testing.T, configPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("func-e", "run", "-c", configPath, "--log-level", "info")
	cmd.Dir = filepath.Dir(configPath)

	// Capture output for debugging on failure.
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start Envoy with config %s: %v", configPath, err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("Envoy output (%s):\n%s", configPath, stderr.String())
		}
	})

	return cmd
}

// waitForEnvoyReady polls the Envoy admin /ready endpoint until it returns 200.
func waitForEnvoyReady(t *testing.T, adminPort int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/ready", adminPort)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Envoy admin port %d not ready after %v", adminPort, timeout)
}

// sendTCPData dials the given TCP address, writes data, reads the response,
// and returns it. Applies a 10-second timeout.
func sendTCPData(t *testing.T, addr string, data []byte) []byte {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to dial %s: %v", addr, err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write(data); err != nil {
		t.Fatalf("failed to write to %s: %v", addr, err)
	}

	// Read response. For echo servers, the response should be at least as
	// long as the sent data.
	buf := make([]byte, len(data)*2+256)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("failed to read from %s: %v", addr, err)
	}
	return buf[:n]
}

// findFreePort returns a free TCP port on 127.0.0.1.
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

// writeCertFiles writes certificate PEM files to the given directory.
func writeCertFiles(t *testing.T, dir string, cert, key, ca []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("failed to create cert dir %s: %v", dir, err)
	}
	for name, data := range map[string][]byte{
		"tls.crt": cert,
		"tls.key": key,
		"ca.crt":  ca,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}
}

// setupSingleServiceTunnel creates a single-service tunnel with two Envoy
// processes and returns the ports and output dir for further testing.
type tunnelSetup struct {
	outputDir          string
	initiatorListenPort int
	initiatorAdminPort  int
	responderAdminPort  int
	tunnelCerts        *certs.TunnelCertificates
}

func setupSingleServiceTunnel(t *testing.T, backendPort int) *tunnelSetup {
	t.Helper()

	tunnelPort := findFreePort(t)
	initiatorListenPort := findFreePort(t)
	responderAdminPort := findFreePort(t)
	initiatorAdminPort := findFreePort(t)

	outputDir := t.TempDir()

	// Generate certificates with 127.0.0.1 as SAN for localhost testing.
	tunnelCerts, err := certs.GenerateTunnelCertificates("e2e-test", []string{"127.0.0.1"}, certs.DefaultCertificateValidity)
	if err != nil {
		t.Fatalf("failed to generate certificates: %v", err)
	}

	// Write certs for each side.
	initiatorCertDir := filepath.Join(outputDir, "initiator", "certs")
	responderCertDir := filepath.Join(outputDir, "responder", "certs")
	writeCertFiles(t, initiatorCertDir, tunnelCerts.InitiatorCert, tunnelCerts.InitiatorKey, tunnelCerts.CACert)
	writeCertFiles(t, responderCertDir, tunnelCerts.ResponderCert, tunnelCerts.ResponderKey, tunnelCerts.CACert)

	// Render Envoy configs.
	initiatorBootstrap, err := envoy.RenderInitiatorBootstrap(envoy.InitiatorConfig{
		ResponderHost: "127.0.0.1",
		ResponderPort: tunnelPort,
		ListenPort:    initiatorListenPort,
		AdminPort:     initiatorAdminPort,
		CertPath:      initiatorCertDir,
		SNI:           "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("failed to render initiator bootstrap: %v", err)
	}

	responderBootstrap, err := envoy.RenderResponderBootstrap(envoy.ResponderConfig{
		ListenPort:  tunnelPort,
		AdminPort:   responderAdminPort,
		CertPath:    responderCertDir,
		BackendHost: "127.0.0.1",
		BackendPort: backendPort,
	})
	if err != nil {
		t.Fatalf("failed to render responder bootstrap: %v", err)
	}

	// Write Envoy configs.
	initiatorConfigPath := filepath.Join(outputDir, "initiator", "envoy.yaml")
	responderConfigPath := filepath.Join(outputDir, "responder", "envoy.yaml")
	if err := os.WriteFile(initiatorConfigPath, initiatorBootstrap, 0644); err != nil {
		t.Fatalf("failed to write initiator config: %v", err)
	}
	if err := os.WriteFile(responderConfigPath, responderBootstrap, 0644); err != nil {
		t.Fatalf("failed to write responder config: %v", err)
	}

	// Start responder first, then initiator.
	startEnvoy(t, responderConfigPath)
	waitForEnvoyReady(t, responderAdminPort, 30*time.Second)

	startEnvoy(t, initiatorConfigPath)
	waitForEnvoyReady(t, initiatorAdminPort, 30*time.Second)

	return &tunnelSetup{
		outputDir:           outputDir,
		initiatorListenPort: initiatorListenPort,
		initiatorAdminPort:  initiatorAdminPort,
		responderAdminPort:  responderAdminPort,
		tunnelCerts:         tunnelCerts,
	}
}

// buildPortal compiles the portal binary to a temp directory.
func buildPortal() (string, error) {
	tmpDir, err := os.MkdirTemp("", "portal-e2e-bm-*")
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

// findRootDir walks up from the test directory to find the module root.
func findRootDir() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			wd, _ := os.Getwd()
			return filepath.Join(wd, "..", "..")
		}
		dir = parent
	}
}

// --- Tests ---

func TestBareMetalE2E01_SingleServiceTunnel(t *testing.T) {
	// Start TCP echo server as the backend.
	_, backendPort := startTCPEchoServer(t, "")

	// Set up and start the tunnel.
	ts := setupSingleServiceTunnel(t, backendPort)

	// Send data through the tunnel.
	payload := []byte("hello from bare metal e2e test\n")
	resp := sendTCPData(t, fmt.Sprintf("127.0.0.1:%d", ts.initiatorListenPort), payload)

	if !bytes.Equal(resp, payload) {
		t.Fatalf("echo mismatch:\n  sent: %q\n  got:  %q", payload, resp)
	}
	t.Logf("single-service tunnel data verification passed")

	// Verify Envoy stats contain the tunnel cluster name.
	statsURL := fmt.Sprintf("http://127.0.0.1:%d/stats", ts.initiatorAdminPort)
	statsResp, err := http.Get(statsURL)
	if err != nil {
		t.Fatalf("failed to get initiator stats: %v", err)
	}
	defer statsResp.Body.Close()
	body, _ := io.ReadAll(statsResp.Body)
	if !strings.Contains(string(body), "tunnel_to_responder") {
		t.Errorf("initiator stats missing tunnel_to_responder cluster")
	} else {
		t.Logf("envoy stats verification passed")
	}
}

func TestBareMetalE2E02_MultiServiceTunnel(t *testing.T) {
	// Start 2 TCP echo servers, each with a unique prefix.
	_, backendPort1 := startTCPEchoServer(t, "[svc-a] ")
	_, backendPort2 := startTCPEchoServer(t, "[svc-b] ")

	tunnelPort := findFreePort(t)
	initiatorListenPort1 := findFreePort(t)
	initiatorListenPort2 := findFreePort(t)
	responderAdminPort := findFreePort(t)
	initiatorAdminPort := findFreePort(t)

	outputDir := t.TempDir()

	// Generate certificates.
	tunnelCerts, err := certs.GenerateTunnelCertificates("e2e-multi", []string{"127.0.0.1"}, certs.DefaultCertificateValidity)
	if err != nil {
		t.Fatalf("failed to generate certificates: %v", err)
	}

	// Write certs.
	initiatorCertDir := filepath.Join(outputDir, "initiator", "certs")
	responderCertDir := filepath.Join(outputDir, "responder", "certs")
	writeCertFiles(t, initiatorCertDir, tunnelCerts.InitiatorCert, tunnelCerts.InitiatorKey, tunnelCerts.CACert)
	writeCertFiles(t, responderCertDir, tunnelCerts.ResponderCert, tunnelCerts.ResponderKey, tunnelCerts.CACert)

	// Render multi-service initiator config.
	initiatorBootstrap, err := envoy.RenderInitiatorMultiBootstrap(envoy.InitiatorMultiServiceConfig{
		ResponderHost: "127.0.0.1",
		ResponderPort: tunnelPort,
		AdminPort:     initiatorAdminPort,
		CertPath:      initiatorCertDir,
		Services: []envoy.ServiceListener{
			{Name: "svc-a", ListenAddress: "127.0.0.1", ListenPort: initiatorListenPort1, SNI: "svc-a.portal.local"},
			{Name: "svc-b", ListenAddress: "127.0.0.1", ListenPort: initiatorListenPort2, SNI: "svc-b.portal.local"},
		},
	})
	if err != nil {
		t.Fatalf("failed to render initiator multi bootstrap: %v", err)
	}

	// Render multi-service responder config.
	responderBootstrap, err := envoy.RenderResponderMultiBootstrap(envoy.ResponderMultiServiceConfig{
		ListenPort: tunnelPort,
		AdminPort:  responderAdminPort,
		CertPath:   responderCertDir,
		Services: []envoy.ServiceRoute{
			{SNI: "svc-a.portal.local", BackendHost: "127.0.0.1", BackendPort: backendPort1},
			{SNI: "svc-b.portal.local", BackendHost: "127.0.0.1", BackendPort: backendPort2},
		},
	})
	if err != nil {
		t.Fatalf("failed to render responder multi bootstrap: %v", err)
	}

	// Write configs.
	initiatorConfigPath := filepath.Join(outputDir, "initiator", "envoy.yaml")
	responderConfigPath := filepath.Join(outputDir, "responder", "envoy.yaml")
	if err := os.MkdirAll(filepath.Dir(initiatorConfigPath), 0700); err != nil {
		t.Fatalf("failed to create initiator dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(responderConfigPath), 0700); err != nil {
		t.Fatalf("failed to create responder dir: %v", err)
	}
	if err := os.WriteFile(initiatorConfigPath, initiatorBootstrap, 0644); err != nil {
		t.Fatalf("failed to write initiator config: %v", err)
	}
	if err := os.WriteFile(responderConfigPath, responderBootstrap, 0644); err != nil {
		t.Fatalf("failed to write responder config: %v", err)
	}

	// Start responder, then initiator.
	startEnvoy(t, responderConfigPath)
	waitForEnvoyReady(t, responderAdminPort, 30*time.Second)

	startEnvoy(t, initiatorConfigPath)
	waitForEnvoyReady(t, initiatorAdminPort, 30*time.Second)

	// Verify service A routing.
	payload := []byte("hello from multi test\n")
	resp1 := sendTCPData(t, fmt.Sprintf("127.0.0.1:%d", initiatorListenPort1), payload)
	expected1 := []byte("[svc-a] hello from multi test\n")
	if !bytes.Equal(resp1, expected1) {
		t.Fatalf("service A routing failed:\n  expected: %q\n  got:      %q", expected1, resp1)
	}
	t.Logf("service A routing verified")

	// Verify service B routing.
	resp2 := sendTCPData(t, fmt.Sprintf("127.0.0.1:%d", initiatorListenPort2), payload)
	expected2 := []byte("[svc-b] hello from multi test\n")
	if !bytes.Equal(resp2, expected2) {
		t.Fatalf("service B routing failed:\n  expected: %q\n  got:      %q", expected2, resp2)
	}
	t.Logf("service B routing verified")
}

func TestBareMetalE2E03_CertRotation(t *testing.T) {
	// Start TCP echo server as the backend.
	_, backendPort := startTCPEchoServer(t, "")

	// Set up and start the tunnel.
	ts := setupSingleServiceTunnel(t, backendPort)

	// Verify tunnel works before rotation.
	payload := []byte("before rotation\n")
	resp := sendTCPData(t, fmt.Sprintf("127.0.0.1:%d", ts.initiatorListenPort), payload)
	if !bytes.Equal(resp, payload) {
		t.Fatalf("pre-rotation echo mismatch:\n  sent: %q\n  got:  %q", payload, resp)
	}
	t.Logf("pre-rotation tunnel verification passed")

	// Rotate leaf certificates (CA stays the same).
	rotated, err := certs.RotateLeafCertificates(
		ts.tunnelCerts.CACert,
		ts.tunnelCerts.CAKey,
		"e2e-test",
		[]string{"127.0.0.1"},
		certs.DefaultCertificateValidity,
	)
	if err != nil {
		t.Fatalf("failed to rotate certificates: %v", err)
	}

	// Overwrite cert files on disk. Envoy's SDS watched_directory will detect
	// the filesystem change and reload the certs.
	initiatorCertDir := filepath.Join(ts.outputDir, "initiator", "certs")
	responderCertDir := filepath.Join(ts.outputDir, "responder", "certs")
	writeCertFiles(t, initiatorCertDir, rotated.InitiatorCert, rotated.InitiatorKey, rotated.CACert)
	writeCertFiles(t, responderCertDir, rotated.ResponderCert, rotated.ResponderKey, rotated.CACert)
	t.Logf("wrote rotated certificates to disk")

	// Wait for Envoy SDS to detect the change. The watched_directory uses
	// inotify and should pick up changes within a few seconds.
	time.Sleep(5 * time.Second)

	// Verify tunnel still works with the new certs.
	payload2 := []byte("after rotation\n")
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		resp2 := sendTCPData(t, fmt.Sprintf("127.0.0.1:%d", ts.initiatorListenPort), payload2)
		if bytes.Equal(resp2, payload2) {
			t.Logf("post-rotation tunnel verification passed (attempt %d)", attempt+1)
			return
		}
		lastErr = fmt.Errorf("echo mismatch: sent %q, got %q", payload2, resp2)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("post-rotation tunnel verification failed after retries: %v", lastErr)
}

func TestBareMetalE2E04_CLIGenerate(t *testing.T) {
	outputDir := t.TempDir()
	tunnelPort := findFreePort(t)

	// Run portal generate with --target bare-metal.
	cmd := exec.Command(portalBin, "generate",
		"e2e-source", "e2e-dest",
		"--target", "bare-metal",
		"--responder-endpoint", fmt.Sprintf("127.0.0.1:%d", tunnelPort),
		"--cert-install-path", filepath.Join(outputDir, "responder", "certs"),
		"--config-install-path", outputDir,
		"--output-dir", outputDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("portal generate failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	t.Logf("portal generate output:\n%s", stdout.String())

	// Verify output directory structure.
	expectedFiles := []string{
		"initiator/envoy.yaml",
		"initiator/certs/tls.crt",
		"initiator/certs/tls.key",
		"initiator/certs/ca.crt",
		"initiator/portal-initiator.service",
		"initiator/docker-compose.yaml",
		"responder/envoy.yaml",
		"responder/certs/tls.crt",
		"responder/certs/tls.key",
		"responder/certs/ca.crt",
		"responder/portal-responder.service",
		"responder/docker-compose.yaml",
		"ca/ca.crt",
		"ca/ca.key",
		"tunnel.yaml",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(outputDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file missing: %s", f)
		}
	}

	// Start a backend echo server to verify the generated configs actually work.
	_, backendPort := startTCPEchoServer(t, "")

	// The generated configs use --cert-install-path for the responder cert path,
	// but both sides need their own cert paths pointed at the generated certs.
	// Re-render with correct cert paths and port assignments for localhost testing.
	initiatorCertDir := filepath.Join(outputDir, "initiator", "certs")
	responderCertDir := filepath.Join(outputDir, "responder", "certs")
	initiatorListenPort := findFreePort(t)
	responderAdminPort := findFreePort(t)
	initiatorAdminPort := findFreePort(t)

	initiatorBootstrap, err := envoy.RenderInitiatorBootstrap(envoy.InitiatorConfig{
		ResponderHost: "127.0.0.1",
		ResponderPort: tunnelPort,
		ListenPort:    initiatorListenPort,
		AdminPort:     initiatorAdminPort,
		CertPath:      initiatorCertDir,
		SNI:           "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("failed to render initiator bootstrap: %v", err)
	}

	responderBootstrap, err := envoy.RenderResponderBootstrap(envoy.ResponderConfig{
		ListenPort:  tunnelPort,
		AdminPort:   responderAdminPort,
		CertPath:    responderCertDir,
		BackendHost: "127.0.0.1",
		BackendPort: backendPort,
	})
	if err != nil {
		t.Fatalf("failed to render responder bootstrap: %v", err)
	}

	// Overwrite the generated configs with our localhost-adapted versions.
	if err := os.WriteFile(filepath.Join(outputDir, "initiator", "envoy.yaml"), initiatorBootstrap, 0644); err != nil {
		t.Fatalf("failed to write initiator config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "responder", "envoy.yaml"), responderBootstrap, 0644); err != nil {
		t.Fatalf("failed to write responder config: %v", err)
	}

	// Start Envoy processes using the CLI-generated certs.
	startEnvoy(t, filepath.Join(outputDir, "responder", "envoy.yaml"))
	waitForEnvoyReady(t, responderAdminPort, 30*time.Second)

	startEnvoy(t, filepath.Join(outputDir, "initiator", "envoy.yaml"))
	waitForEnvoyReady(t, initiatorAdminPort, 30*time.Second)

	// Verify tunnel connectivity.
	payload := []byte("hello from CLI-generated tunnel\n")
	resp := sendTCPData(t, fmt.Sprintf("127.0.0.1:%d", initiatorListenPort), payload)
	if !bytes.Equal(resp, payload) {
		t.Fatalf("CLI-generated tunnel echo mismatch:\n  sent: %q\n  got:  %q", payload, resp)
	}
	t.Logf("CLI-generated tunnel verification passed")
}
