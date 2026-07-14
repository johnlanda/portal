package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/state"
)

// setupHubTestHooks extends setupTestHooks with an isolated hub PKI directory.
func setupHubTestHooks(t *testing.T) (srcMock, dstMock *mockKubeClient, storePath string) {
	t.Helper()
	srcMock, dstMock, storePath = setupTestHooks(t)
	origStateDir := stateDirFn
	dir := t.TempDir()
	stateDirFn = func() (string, error) { return dir, nil }
	t.Cleanup(func() { stateDirFn = origStateDir })
	return srcMock, dstMock, storePath
}

func runCommand(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func initTestHub(t *testing.T) (dstMock *mockKubeClient, storePath string, srcMock *mockKubeClient) {
	t.Helper()
	srcMock, dstMock, storePath = setupHubTestHooks(t)
	out, err := runCommand(t, NewHubCmd(), "init", "dst-hub", "--name", "synapse", "--public-addr", "tunnel.corp.example:10443")
	if err != nil {
		t.Fatalf("hub init failed: %v\n%s", err, out)
	}
	return dstMock, storePath, srcMock
}

func TestHubInit(t *testing.T) {
	dstMock, storePath, _ := initTestHub(t)

	if dstMock.applyCalls == 0 {
		t.Error("hub init did not apply resources")
	}
	store := state.NewStore(storePath)
	hub, err := store.GetHub("synapse")
	if err != nil {
		t.Fatalf("hub state not saved: %v", err)
	}
	if hub.PublicAddr != "tunnel.corp.example:10443" {
		t.Errorf("PublicAddr = %q", hub.PublicAddr)
	}
	for _, f := range []string{"ca.crt", "ca.key", "tls.crt", "tls.key", "crl.pem"} {
		if _, err := os.Stat(filepath.Join(hub.CADir, f)); err != nil {
			t.Errorf("missing PKI file %s: %v", f, err)
		}
	}
	info, err := os.Stat(filepath.Join(hub.CADir, "ca.key"))
	if err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("ca.key permissions = %v, want 0600", info.Mode().Perm())
	}
}

func TestHubInitDiscoversLB(t *testing.T) {
	_, dstMock, storePath := setupHubTestHooks(t)
	dstMock.waitForServiceAddrFn = func(_ context.Context, name string, _ time.Duration) (string, error) {
		return "203.0.113.7", nil
	}
	out, err := runCommand(t, NewHubCmd(), "init", "dst-hub", "--name", "synapse")
	if err != nil {
		t.Fatalf("hub init failed: %v\n%s", err, out)
	}
	store := state.NewStore(storePath)
	hub, err := store.GetHub("synapse")
	if err != nil {
		t.Fatal(err)
	}
	if hub.PublicAddr != "203.0.113.7:10443" {
		t.Errorf("PublicAddr = %q, want discovered LB address", hub.PublicAddr)
	}
	if dstMock.patchSecretCalls == 0 {
		t.Error("server certificate was not re-issued with the discovered address")
	}
}

func TestHubInitDuplicate(t *testing.T) {
	initTestHub(t)
	if _, err := runCommand(t, NewHubCmd(), "init", "dst-hub", "--name", "synapse", "--public-addr", "x:1"); err == nil {
		t.Error("expected error for duplicate hub")
	}
}

func TestHubSignAndEvictLifecycle(t *testing.T) {
	dstMock, storePath, srcMock := initTestHub(t)
	tmp := t.TempDir()

	// Member side, phase 1: keygen in-cluster + CSR out.
	csrPath := filepath.Join(tmp, "acme-prod.csr")
	out, err := runCommand(t, NewJoinCmd(), "src-member",
		"--member", "acme-prod", "--hub-addr", "tunnel.corp.example:10443",
		"--hub-name", "synapse", "--csr-out", csrPath)
	if err != nil {
		t.Fatalf("join phase 1 failed: %v\n%s", err, out)
	}
	if srcMock.applyCalls == 0 {
		t.Error("phase 1 did not create the in-cluster key Secret")
	}
	store := state.NewStore(storePath)
	membership, err := store.GetMembership("acme-prod")
	if err != nil || !membership.Pending {
		t.Fatalf("membership not pending after phase 1: %+v, %v", membership, err)
	}

	// Hub side: sign the CSR.
	bundlePath := filepath.Join(tmp, "acme-prod-cert.pem")
	out, err = runCommand(t, NewHubCmd(), "sign", csrPath, "--member", "acme-prod", "-o", bundlePath)
	if err != nil {
		t.Fatalf("hub sign failed: %v\n%s", err, out)
	}
	hub, err := store.GetHub("synapse")
	if err != nil {
		t.Fatal(err)
	}
	record := hub.Member("acme-prod")
	if record == nil || record.CertSerial == "" {
		t.Fatalf("member not recorded after sign: %+v", hub.Members)
	}
	if record.CertExpiry.IsZero() {
		t.Error("member cert expiry not recorded")
	}
	if dstMock.rolloutRestartCalls == 0 {
		t.Error("hub routing was not updated after sign")
	}

	// Member side, phase 2: install cert + deploy.
	out, err = runCommand(t, NewJoinCmd(), "src-member", "--member", "acme-prod", "--cert", bundlePath)
	if err != nil {
		t.Fatalf("join phase 2 failed: %v\n%s", err, out)
	}
	if srcMock.patchSecretCalls == 0 {
		t.Error("phase 2 did not patch the certificate into the Secret")
	}
	membership, err = store.GetMembership("acme-prod")
	if err != nil || membership.Pending {
		t.Fatalf("membership still pending after phase 2: %+v, %v", membership, err)
	}

	// Publish a service.
	out, err = runCommand(t, NewPublishCmd(), "src-member", "inference", "--port", "8080", "--protocol", "grpc")
	if err != nil {
		t.Fatalf("publish failed: %v\n%s", err, out)
	}
	membership, _ = store.GetMembership("acme-prod")
	if membership.PublishedService("inference") == nil {
		t.Error("published service not recorded")
	}

	// Route a friendly alias on the hub.
	out, err = runCommand(t, NewRouteCmd(), "acme-prod/inference")
	if err != nil {
		t.Fatalf("route failed: %v\n%s", err, out)
	}
	hub, _ = store.GetHub("synapse")
	if len(hub.Routes) != 1 || hub.Routes[0].AliasService != "inference-acme-prod" {
		t.Errorf("route not recorded: %+v", hub.Routes)
	}

	// Evict: CRL grows, member removed from routing.
	crlBefore, _ := os.ReadFile(filepath.Join(hub.CADir, "crl.pem"))
	out, err = runCommand(t, NewHubCmd(), "evict", "acme-prod")
	if err != nil {
		t.Fatalf("hub evict failed: %v\n%s", err, out)
	}
	hub, _ = store.GetHub("synapse")
	if !hub.Member("acme-prod").Evicted {
		t.Error("member not marked evicted")
	}
	crlAfter, _ := os.ReadFile(filepath.Join(hub.CADir, "crl.pem"))
	if bytes.Equal(crlBefore, crlAfter) {
		t.Error("CRL was not re-rendered on evict")
	}
	if _, err = runCommand(t, NewHubCmd(), "evict", "acme-prod"); err == nil {
		t.Error("expected error evicting an already-evicted member")
	}
}

func TestHubSignIgnoresCSRIdentityViaCLI(t *testing.T) {
	_, _, _ = initTestHub(t)
	tmp := t.TempDir()

	csrPath := filepath.Join(tmp, "evil.csr")
	out, err := runCommand(t, NewJoinCmd(), "src-member",
		"--member", "globex-dev", "--hub-addr", "tunnel.corp.example:10443", "--csr-out", csrPath)
	if err != nil {
		t.Fatalf("join phase 1 failed: %v\n%s", err, out)
	}
	// The hub owner grants a DIFFERENT identity than the CSR requested.
	bundlePath := filepath.Join(tmp, "granted.pem")
	if _, err := runCommand(t, NewHubCmd(), "sign", csrPath, "--member", "acme-prod", "-o", bundlePath); err != nil {
		t.Fatalf("hub sign failed: %v", err)
	}
	bundle, _ := os.ReadFile(bundlePath)
	leaf, _, err := splitCertBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(leaf), "CERTIFICATE") {
		t.Fatal("no leaf in bundle")
	}
}

func TestJoinRequiresMode(t *testing.T) {
	setupHubTestHooks(t)
	if _, err := runCommand(t, NewJoinCmd(), "src-member"); err == nil {
		t.Error("expected error when no join mode flag given")
	}
}

func TestPublishRejectsTCP(t *testing.T) {
	setupHubTestHooks(t)
	_, err := runCommand(t, NewPublishCmd(), "src-member", "postgres", "--port", "5432", "--protocol", "tcp")
	if err == nil || !strings.Contains(err.Error(), "HTTP/2-only") {
		t.Errorf("expected HTTP/2-only error, got: %v", err)
	}
}

func TestInviteCredentialJoin(t *testing.T) {
	_, storePath, srcMock := initTestHub(t)
	tmp := t.TempDir()

	credPath := filepath.Join(tmp, "acme.credential")
	out, err := runCommand(t, NewHubCmd(), "invite", "acme-prod", "-o", credPath)
	if err != nil {
		t.Fatalf("hub invite failed: %v\n%s", err, out)
	}
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("credential permissions = %v, want 0600 (contains a private key)", info.Mode().Perm())
	}

	out, err = runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath)
	if err != nil {
		t.Fatalf("credential join failed: %v\n%s", err, out)
	}
	if srcMock.applyCalls == 0 {
		t.Error("credential join did not apply member resources")
	}
	store := state.NewStore(storePath)
	membership, err := store.GetMembership("acme-prod")
	if err != nil || membership.Pending {
		t.Fatalf("membership not enrolled after credential join: %+v, %v", membership, err)
	}
	if membership.HubAddr != "tunnel.corp.example:10443" {
		t.Errorf("HubAddr from credential = %q", membership.HubAddr)
	}
}

func TestRouteUnknownMember(t *testing.T) {
	initTestHub(t)
	if _, err := runCommand(t, NewRouteCmd(), "ghost/svc"); err == nil {
		t.Error("expected error routing to unknown member")
	}
}
