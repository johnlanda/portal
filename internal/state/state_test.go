package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadNonExistentFile(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))
	sf, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sf.Tunnels) != 0 {
		t.Errorf("expected empty tunnels, got %d", len(sf.Tunnels))
	}
}

func TestSaveAndLoad(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	now := time.Now().Truncate(time.Second)
	sf := &StateFile{
		Tunnels: []TunnelState{
			{
				Name:               "test-tunnel",
				SourceContext:      "src",
				DestinationContext: "dst",
				Namespace:          "portal-system",
				TunnelPort:         10443,
				CreatedAt:          now,
				Mode:               "imperative",
				Services:           []string{"svc-a"},
			},
		},
	}

	if err := store.Save(sf); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(loaded.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(loaded.Tunnels))
	}
	got := loaded.Tunnels[0]
	if got.Name != "test-tunnel" {
		t.Errorf("Name = %q, want %q", got.Name, "test-tunnel")
	}
	if got.SourceContext != "src" {
		t.Errorf("SourceContext = %q, want %q", got.SourceContext, "src")
	}
	if got.DestinationContext != "dst" {
		t.Errorf("DestinationContext = %q, want %q", got.DestinationContext, "dst")
	}
	if got.TunnelPort != 10443 {
		t.Errorf("TunnelPort = %d, want %d", got.TunnelPort, 10443)
	}
	if got.Mode != "imperative" {
		t.Errorf("Mode = %q, want %q", got.Mode, "imperative")
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
}

func TestAddTunnel(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	ts := TunnelState{
		Name:               "my-tunnel",
		SourceContext:      "a",
		DestinationContext: "b",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		CreatedAt:          time.Now(),
		Mode:               "imperative",
	}
	if err := store.Add(ts); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	sf, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(sf.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(sf.Tunnels))
	}
	if sf.Tunnels[0].Name != "my-tunnel" {
		t.Errorf("Name = %q, want %q", sf.Tunnels[0].Name, "my-tunnel")
	}
}

func TestAddDuplicate(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	ts := TunnelState{Name: "dup", Mode: "imperative"}
	if err := store.Add(ts); err != nil {
		t.Fatalf("first Add failed: %v", err)
	}

	err := store.Add(ts)
	if err == nil {
		t.Fatal("expected error on duplicate Add")
	}
}

func TestRemoveTunnel(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	if err := store.Add(TunnelState{Name: "rm-me", Mode: "imperative"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := store.Remove("rm-me"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	sf, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(sf.Tunnels) != 0 {
		t.Errorf("expected 0 tunnels after remove, got %d", len(sf.Tunnels))
	}
}

func TestRemoveNonExistent(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	err := store.Remove("ghost")
	if err == nil {
		t.Fatal("expected error when removing non-existent tunnel")
	}
}

func TestGetTunnel(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	ts := TunnelState{
		Name:               "get-me",
		SourceContext:      "src",
		DestinationContext: "dst",
		Namespace:          "ns",
		TunnelPort:         9443,
		Mode:               "imperative",
		Services:           []string{"svc1", "svc2"},
	}
	if err := store.Add(ts); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	got, err := store.Get("get-me")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Name != "get-me" {
		t.Errorf("Name = %q, want %q", got.Name, "get-me")
	}
	if got.TunnelPort != 9443 {
		t.Errorf("TunnelPort = %d, want %d", got.TunnelPort, 9443)
	}
	if len(got.Services) != 2 {
		t.Errorf("Services length = %d, want 2", len(got.Services))
	}
}

func TestGetNonExistent(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	got, err := store.Get("nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestListTunnels(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	for _, name := range []string{"t1", "t2", "t3"} {
		if err := store.Add(TunnelState{Name: name, Mode: "imperative"}); err != nil {
			t.Fatalf("Add %s failed: %v", name, err)
		}
	}

	tunnels, err := store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(tunnels) != 3 {
		t.Errorf("expected 3 tunnels, got %d", len(tunnels))
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "deep", "dir")
	store := NewStore(filepath.Join(dir, "tunnels.json"))

	sf := &StateFile{
		Tunnels: []TunnelState{{Name: "nested", Mode: "imperative"}},
	}
	if err := store.Save(sf); err != nil {
		t.Fatalf("Save to nested path failed: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load from nested path failed: %v", err)
	}
	if len(loaded.Tunnels) != 1 {
		t.Errorf("expected 1 tunnel, got %d", len(loaded.Tunnels))
	}
}

func TestHubCRUD(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	hub := HubState{
		Name: "synapse", Context: "hub-ctx", Namespace: "portal",
		PublicAddr: "tunnel.corp.example:10443", TunnelPort: 10443,
		EgressPort: 10080, HandshakeSNI: "reverse-tunnel.portal",
		CADir: "/tmp/ca", CreatedAt: time.Now(),
	}
	if err := s.AddHub(hub); err != nil {
		t.Fatalf("AddHub() error = %v", err)
	}
	if err := s.AddHub(hub); err == nil {
		t.Error("expected error adding duplicate hub")
	}

	got, err := s.GetHub("synapse")
	if err != nil {
		t.Fatalf("GetHub() error = %v", err)
	}
	if got.PublicAddr != "tunnel.corp.example:10443" {
		t.Errorf("PublicAddr = %q", got.PublicAddr)
	}

	got.Members = append(got.Members, MemberRecord{Name: "acme-prod", CertSerial: "12345", JoinedAt: time.Now()})
	if err := s.UpdateHub(*got); err != nil {
		t.Fatalf("UpdateHub() error = %v", err)
	}
	got, err = s.GetHub("synapse")
	if err != nil {
		t.Fatal(err)
	}
	if m := got.Member("acme-prod"); m == nil || m.CertSerial != "12345" {
		t.Errorf("Member lookup failed: %+v", got.Members)
	}
	if got.Member("nope") != nil {
		t.Error("Member() should return nil for unknown member")
	}

	hubs, err := s.ListHubs()
	if err != nil || len(hubs) != 1 {
		t.Fatalf("ListHubs() = %v, %v", hubs, err)
	}
	if err := s.RemoveHub("synapse"); err != nil {
		t.Fatalf("RemoveHub() error = %v", err)
	}
	if _, err := s.GetHub("synapse"); err == nil {
		t.Error("expected error getting removed hub")
	}
	if err := s.RemoveHub("synapse"); err == nil {
		t.Error("expected error removing missing hub")
	}
}

func TestMembershipCRUD(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "tunnels.json"))

	m := MembershipState{
		Member: "acme-prod", Hub: "synapse", HubAddr: "tunnel.corp.example:10443",
		Context: "member-ctx", Namespace: "portal", Pending: true, JoinedAt: time.Now(),
	}
	if err := s.AddMembership(m); err != nil {
		t.Fatalf("AddMembership() error = %v", err)
	}
	if err := s.AddMembership(m); err == nil {
		t.Error("expected error adding duplicate membership")
	}

	got, err := s.GetMembership("acme-prod")
	if err != nil {
		t.Fatalf("GetMembership() error = %v", err)
	}
	if !got.Pending {
		t.Error("Pending flag not persisted")
	}

	got.Pending = false
	got.Published = append(got.Published, PublishedEntry{Name: "inference", Port: 8080, Protocol: "grpc"})
	if err := s.UpdateMembership(*got); err != nil {
		t.Fatalf("UpdateMembership() error = %v", err)
	}
	got, err = s.GetMembership("acme-prod")
	if err != nil {
		t.Fatal(err)
	}
	if p := got.PublishedService("inference"); p == nil || p.Protocol != "grpc" {
		t.Errorf("PublishedService lookup failed: %+v", got.Published)
	}

	list, err := s.ListMemberships()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListMemberships() = %v, %v", list, err)
	}
	if err := s.RemoveMembership("acme-prod"); err != nil {
		t.Fatalf("RemoveMembership() error = %v", err)
	}
	if _, err := s.GetMembership("acme-prod"); err == nil {
		t.Error("expected error getting removed membership")
	}
}

// TestV1StateFileLoadsWithV2Fields ensures a legacy tunnels.json (no hubs or
// memberships keys) loads cleanly and round-trips without corrupting v1 data.
func TestV1StateFileLoadsWithV2Fields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnels.json")
	legacy := `{"tunnels":[{"name":"t1","source_context":"a","destination_context":"b","namespace":"portal","tunnel_port":10443,"created_at":"2025-01-01T00:00:00Z","mode":"tcp","services":["svc:8080"]}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if err := s.AddHub(HubState{Name: "h", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("AddHub() on legacy file error = %v", err)
	}
	tunnels, err := s.List()
	if err != nil || len(tunnels) != 1 || tunnels[0].Name != "t1" {
		t.Fatalf("legacy tunnel lost after v2 write: %v, %v", tunnels, err)
	}
}
