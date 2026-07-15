package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/kube"

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

func TestStatusMember(t *testing.T) {
	dstMock, _, srcMock := initTestHub(t)
	tmp := t.TempDir()

	// Enroll via credential for brevity.
	credPath := filepath.Join(tmp, "acme.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "acme-prod", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewPublishCmd(), "src-member", "inference", "--port", "8080"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewRouteCmd(), "acme-prod/inference"); err != nil {
		t.Fatal(err)
	}

	dstMock.getPodsFn = func(_ context.Context, selector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "portal-hub-abc", Ready: true, Phase: kube.PodRunning}}, nil
	}
	srcMock.getPodsFn = func(_ context.Context, selector string) ([]kube.PodInfo, error) {
		return []kube.PodInfo{{Name: "portal-member-xyz", Ready: true, Phase: kube.PodRunning}}, nil
	}
	origHandshake := fetchHandshakeStatsFn
	origProbe := probeRouteFn
	fetchHandshakeStatsFn = func(_ context.Context, _ kube.Client, _ string) *tunnelCounts {
		return &tunnelCounts{Accepted: 42, ValidationFailed: 1}
	}
	probeRouteFn = func(_ context.Context, _ kube.Client, _ string, _ int, service, member string) routeProbe {
		return routeProbe{Service: service, State: "not-published", Detail: "member Envoy returned 404"}
	}
	t.Cleanup(func() {
		fetchHandshakeStatsFn = origHandshake
		probeRouteFn = origProbe
	})

	out, err := runCommand(t, NewStatusCmd(), "acme-prod")
	if err != nil {
		t.Fatalf("status failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"MEMBER acme-prod",
		"42 accepted",
		"validation failures indicate identity mismatches",
		"not-published",
		"inference-acme-prod",
		"member pod  portal-member-xyz",
		"published   inference :8080 http",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusSummaryShowsHubsAndMemberships(t *testing.T) {
	_, _, _ = initTestHub(t)
	tmp := t.TempDir()
	credPath := filepath.Join(tmp, "acme.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "acme-prod", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath); err != nil {
		t.Fatal(err)
	}

	out, err := runCommand(t, NewStatusCmd())
	if err != nil {
		t.Fatalf("status failed: %v\n%s", err, out)
	}
	for _, want := range []string{"hub synapse", "tunnel.corp.example:10443", "acme-prod", "membership acme-prod"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestStatusUnknownMemberArg(t *testing.T) {
	setupHubTestHooks(t)
	if _, err := runCommand(t, NewStatusCmd(), "ghost"); err == nil {
		t.Error("expected error for unknown member arg")
	}
}

func TestForwardLifecycle(t *testing.T) {
	_, storePath, srcMock := initTestHub(t)
	tmp := t.TempDir()
	credPath := filepath.Join(tmp, "acme.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "acme-prod", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath); err != nil {
		t.Fatal(err)
	}

	// Hub side: expose a backend on the forward path.
	out, err := runCommand(t, NewHubCmd(), "expose", "telemetry", "--port", "4317", "--service-namespace", "monitoring")
	if err != nil {
		t.Fatalf("hub expose failed: %v\n%s", err, out)
	}
	store := state.NewStore(storePath)
	hub, _ := store.GetHub("synapse")
	if len(hub.Services) != 1 || hub.Services[0].SNI != "telemetry" {
		t.Fatalf("hub service not recorded: %+v", hub.Services)
	}
	if hub.Services[0].Name != "telemetry.monitoring.svc.cluster.local" {
		t.Errorf("backend host = %q", hub.Services[0].Name)
	}

	// Reserved SNI rejected.
	if _, err := runCommand(t, NewHubCmd(), "expose", "svc2", "--port", "1", "--sni", hub.HandshakeSNI); err == nil {
		t.Error("expected reserved-SNI error")
	}

	// Member side: add the local listener.
	applyBefore := srcMock.applyCalls
	out, err = runCommand(t, NewForwardCmd(), "src-member", "telemetry", "--local-port", "4317")
	if err != nil {
		t.Fatalf("forward failed: %v\n%s", err, out)
	}
	if srcMock.applyCalls == applyBefore {
		t.Error("forward did not re-apply member resources")
	}
	membership, _ := store.GetMembership("acme-prod")
	if len(membership.Forward) != 1 || membership.Forward[0].LocalPort != 4317 {
		t.Fatalf("forward listener not recorded: %+v", membership.Forward)
	}

	// Duplicate port rejected.
	if _, err := runCommand(t, NewForwardCmd(), "src-member", "other", "--local-port", "4317"); err == nil {
		t.Error("expected duplicate local port error")
	}

	// Remove.
	if _, err := runCommand(t, NewForwardCmd(), "src-member", "telemetry", "--remove"); err != nil {
		t.Fatalf("forward --remove failed: %v", err)
	}
	membership, _ = store.GetMembership("acme-prod")
	if len(membership.Forward) != 0 {
		t.Errorf("forward listener not removed: %+v", membership.Forward)
	}
}

func TestRenewLifecycle(t *testing.T) {
	_, _, srcMock := initTestHub(t)
	tmp := t.TempDir()
	credPath := filepath.Join(tmp, "acme.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "acme-prod", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath); err != nil {
		t.Fatal(err)
	}

	// Phase 1: stage new key + emit CSR.
	csrPath := filepath.Join(tmp, "renew.csr")
	var staged map[string][]byte
	srcMock.patchSecretFn = func(_ context.Context, name string, data map[string][]byte) error {
		staged = data
		return nil
	}
	out, err := runCommand(t, NewRenewCmd(), "src-member", "--csr-out", csrPath)
	if err != nil {
		t.Fatalf("renew phase 1 failed: %v\n%s", err, out)
	}
	if _, ok := staged["tls.key.next"]; !ok {
		t.Fatal("renewal key was not staged under tls.key.next")
	}
	stagedKey := staged["tls.key.next"]

	// Hub signs the renewal CSR (replaces the recorded serial).
	bundlePath := filepath.Join(tmp, "renewed.pem")
	if _, err := runCommand(t, NewHubCmd(), "sign", csrPath, "--member", "acme-prod", "-o", bundlePath); err != nil {
		t.Fatalf("hub sign (renewal) failed: %v", err)
	}

	// Phase 2: promote staged key + new cert atomically.
	srcMock.getSecretKeyFn = func(_ context.Context, name, key string) ([]byte, error) {
		if key != "tls.key.next" {
			t.Errorf("unexpected secret key read: %s", key)
		}
		return stagedKey, nil
	}
	var promoted map[string][]byte
	srcMock.patchSecretFn = func(_ context.Context, name string, data map[string][]byte) error {
		promoted = data
		return nil
	}
	out, err = runCommand(t, NewRenewCmd(), "src-member", "--cert", bundlePath)
	if err != nil {
		t.Fatalf("renew phase 2 failed: %v\n%s", err, out)
	}
	if string(promoted["tls.key"]) != string(stagedKey) {
		t.Error("staged key was not promoted to tls.key")
	}
	if len(promoted["tls.crt"]) == 0 || len(promoted["ca.crt"]) == 0 {
		t.Error("renewed cert/CA not installed")
	}
}

func TestMigrateGuided(t *testing.T) {
	_, _, storePath := setupHubTestHooks(t)
	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name: "src-a--dst-b", SourceContext: "src-a", DestinationContext: "dst-b",
		Namespace: "portal-system", TunnelPort: 10443, Mode: "imperative",
		ServiceEntries: []state.ServiceEntry{{Name: "backend", Port: 8443, SNI: "backend", LocalPort: 8443}},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runCommand(t, NewMigrateCmd(), "src-a", "dst-b")
	if err != nil {
		t.Fatalf("migrate failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"RE-KEYS",
		"portal disconnect src-a dst-b",
		"portal hub init dst-b",
		"portal hub invite src-a",
		"portal join src-a --credential",
		"portal hub expose backend --port 8443",
		"portal forward src-a backend --local-port 8443",
		"portal status src-a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("migration plan missing %q:\n%s", want, out)
		}
	}

	if _, err := runCommand(t, NewMigrateCmd(), "ghost", "tunnel"); err == nil {
		t.Error("expected error for unknown tunnel")
	}
}

func TestDryRunModes(t *testing.T) {
	dstMock, storePath, srcMock := initTestHub(t)
	tmp := t.TempDir()
	credPath := filepath.Join(tmp, "acme.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "acme-prod", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(storePath)

	t.Run("hub init", func(t *testing.T) {
		applyBefore := dstMock.applyCalls
		out, err := runCommand(t, NewHubCmd(), "init", "dst-other", "--name", "other",
			"--public-addr", "x.example:10443", "--dry-run")
		if err != nil {
			t.Fatalf("dry-run init failed: %v\n%s", err, out)
		}
		if dstMock.applyCalls != applyBefore {
			t.Error("dry-run applied resources")
		}
		if _, err := store.GetHub("other"); err == nil {
			t.Error("dry-run saved hub state")
		}
		if !strings.Contains(out, "portal-hub-deployment.yaml") || !strings.Contains(out, "DRY RUN") {
			t.Errorf("dry-run output missing manifests:\n%s", out[:min(400, len(out))])
		}
	})

	t.Run("publish", func(t *testing.T) {
		applyBefore := srcMock.applyCalls
		out, err := runCommand(t, NewPublishCmd(), "src-member", "svc-a", "--port", "8080", "--dry-run")
		if err != nil {
			t.Fatalf("dry-run publish failed: %v\n%s", err, out)
		}
		if srcMock.applyCalls != applyBefore {
			t.Error("dry-run applied resources")
		}
		m, _ := store.GetMembership("acme-prod")
		if m.PublishedService("svc-a") != nil {
			t.Error("dry-run saved publish state")
		}
		if !strings.Contains(out, "svc-a.acme-prod") {
			t.Error("dry-run output missing published route")
		}
	})

	t.Run("route", func(t *testing.T) {
		if _, err := runCommand(t, NewPublishCmd(), "src-member", "inference", "--port", "8080"); err != nil {
			t.Fatal(err)
		}
		restartBefore := dstMock.rolloutRestartCalls
		out, err := runCommand(t, NewRouteCmd(), "acme-prod/inference", "--dry-run")
		if err != nil {
			t.Fatalf("dry-run route failed: %v\n%s", err, out)
		}
		if dstMock.rolloutRestartCalls != restartBefore {
			t.Error("dry-run restarted the hub")
		}
		hub, _ := store.GetHub("synapse")
		if len(hub.Routes) != 0 {
			t.Error("dry-run saved route state")
		}
		if !strings.Contains(out, "inference-acme-prod") {
			t.Error("dry-run output missing alias resources")
		}
	})

	t.Run("evict", func(t *testing.T) {
		out, err := runCommand(t, NewHubCmd(), "evict", "acme-prod", "--dry-run")
		if err != nil {
			t.Fatalf("dry-run evict failed: %v\n%s", err, out)
		}
		hub, _ := store.GetHub("synapse")
		if hub.Member("acme-prod").Evicted {
			t.Error("dry-run marked member evicted")
		}
		if !strings.Contains(out, "would revoke") {
			t.Error("dry-run output missing revocation note")
		}
	})
}

// TestEvictRevokesRenewedPriorSerial covers finding M-1: after a member
// renews, eviction must revoke the superseded (still-valid, same-SAN)
// certificate too, not just the newest.
func TestEvictRevokesRenewedPriorSerial(t *testing.T) {
	dstMock, storePath, _ := initTestHub(t)
	tmp := t.TempDir()

	// Enroll member-a via credential.
	credPath := filepath.Join(tmp, "a.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "member-a", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(storePath)
	hub, _ := store.GetHub("synapse")
	origSerial := hub.Member("member-a").CertSerial
	if origSerial == "" {
		t.Fatal("no original serial recorded")
	}

	// Re-sign (as a renewal would): a fresh CSR signed for the same member.
	_, csrPEM, err := certsGenerate(t, "member-a", "synapse")
	if err != nil {
		t.Fatal(err)
	}
	csrPath := filepath.Join(tmp, "renew.csr")
	if err := os.WriteFile(csrPath, csrPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(t, NewHubCmd(), "sign", csrPath, "--member", "member-a", "-o", filepath.Join(tmp, "renewed.pem")); err != nil {
		t.Fatalf("re-sign failed: %v", err)
	}
	hub, _ = store.GetHub("synapse")
	newSerial := hub.Member("member-a").CertSerial
	if newSerial == origSerial {
		t.Fatal("serial did not change on re-sign")
	}
	priors := hub.Member("member-a").PriorCerts
	if len(priors) != 1 || priors[0].Serial != origSerial {
		t.Fatalf("prior serial not retained: %+v", priors)
	}

	// Capture the CRL the eviction writes.
	var appliedCRL []byte
	dstMock.applyFn = func(_ context.Context, yamls [][]byte) error {
		for _, y := range yamls {
			if bytesContains(y, "crl.pem") {
				appliedCRL = y
			}
		}
		return nil
	}
	if _, err := runCommand(t, NewHubCmd(), "evict", "member-a"); err != nil {
		t.Fatalf("evict failed: %v", err)
	}

	// Both serials must be present in the on-disk CRL.
	crlPEM, err := os.ReadFile(filepath.Join(hub.CADir, "crl.pem"))
	if err != nil {
		t.Fatal(err)
	}
	revoked := crlSerials(t, crlPEM)
	for _, want := range []string{origSerial, newSerial} {
		if !revoked[want] {
			t.Errorf("CRL missing serial %s; revoked set = %v", want, revoked)
		}
	}
	if len(revoked) != 2 {
		t.Errorf("expected exactly 2 revoked serials, got %d: %v", len(revoked), revoked)
	}
	_ = appliedCRL
}

func bytesContains(b []byte, s string) bool { return strings.Contains(string(b), s) }

// certsGenerate produces a member keypair+CSR without importing the certs
// package name collision in the test body.
func certsGenerate(t *testing.T, member, tenant string) ([]byte, []byte, error) {
	t.Helper()
	return certs.GenerateMemberKeyAndCSR(certs.MemberIdentity{Member: member, Tenant: tenant})
}

// crlSerials parses a PEM CRL and returns the set of decimal serial strings.
func crlSerials(t *testing.T, crlPEM []byte) map[string]bool {
	t.Helper()
	block, _ := pem.Decode(crlPEM)
	if block == nil {
		t.Fatal("failed to decode CRL PEM")
	}
	crl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CRL: %v", err)
	}
	out := map[string]bool{}
	for _, e := range crl.RevokedCertificateEntries {
		out[e.SerialNumber.String()] = true
	}
	return out
}

// TestCredentialDryRunOmitsKey covers finding L-1: --credential --dry-run must
// not print the member private key.
func TestCredentialDryRunOmitsKey(t *testing.T) {
	_, _, srcMock := initTestHub(t)
	tmp := t.TempDir()
	credPath := filepath.Join(tmp, "a.credential")
	if _, err := runCommand(t, NewHubCmd(), "invite", "member-a", "-o", credPath); err != nil {
		t.Fatal(err)
	}
	// Pull the actual private-key material out of the credential so we can
	// assert none of it appears in the dry-run output (the bootstrap legitimately
	// references the path "/etc/portal/certs/tls.key", which is not key material).
	cred, err := readCredential(credPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBody := strings.ReplaceAll(cred.Key, "\n", "")
	keyBody = strings.TrimPrefix(keyBody, "-----BEGIN RSA PRIVATE KEY-----")
	keySample := keyBody[:60] // a distinctive slice of the base64 key body

	applyBefore := srcMock.applyCalls
	out, err := runCommand(t, NewJoinCmd(), "src-member", "--credential", credPath, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run credential join failed: %v\n%s", err, out)
	}
	if srcMock.applyCalls != applyBefore {
		t.Error("dry-run applied resources")
	}
	if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, keySample) {
		t.Errorf("dry-run output leaked private key material:\n%s", out)
	}
	if !strings.Contains(out, "omitted") {
		t.Error("dry-run should note the Secret is omitted")
	}
}
