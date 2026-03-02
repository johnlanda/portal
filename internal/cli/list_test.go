package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/portal/internal/state"
)

func setupListTestHooks(t *testing.T) string {
	t.Helper()

	origNewStateStore := newStateStore
	storePath := filepath.Join(t.TempDir(), "tunnels.json")
	newStateStore = func() (*state.Store, error) {
		return state.NewStore(storePath), nil
	}
	t.Cleanup(func() { newStateStore = origNewStateStore })

	return storePath
}

func TestListEmpty(t *testing.T) {
	setupListTestHooks(t)

	var buf strings.Builder
	cmd := NewListCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "No tunnels found") {
		t.Errorf("expected 'No tunnels found', got:\n%s", buf.String())
	}
}

func TestListSingleTunnel(t *testing.T) {
	storePath := setupListTestHooks(t)

	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "gke--eks",
		SourceContext:      "gke-us-east",
		DestinationContext: "eks-eu-west",
		Namespace:          "portal-system",
		TunnelPort:         10443,
		CreatedAt:          time.Now().Add(-2 * time.Hour),
		Mode:               "imperative",
	}); err != nil {
		t.Fatalf("failed to add tunnel: %v", err)
	}

	var buf strings.Builder
	cmd := NewListCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	// Verify header.
	if !strings.Contains(output, "NAME") {
		t.Errorf("expected table header, got:\n%s", output)
	}
	// Verify tunnel row.
	if !strings.Contains(output, "gke--eks") {
		t.Errorf("expected tunnel name in output, got:\n%s", output)
	}
	if !strings.Contains(output, "gke-us-east") {
		t.Errorf("expected source context in output, got:\n%s", output)
	}
	if !strings.Contains(output, "eks-eu-west") {
		t.Errorf("expected destination context in output, got:\n%s", output)
	}
	if !strings.Contains(output, "10443") {
		t.Errorf("expected port in output, got:\n%s", output)
	}
}

func TestListMultipleTunnels(t *testing.T) {
	storePath := setupListTestHooks(t)

	store := state.NewStore(storePath)
	for _, name := range []string{"tunnel-a", "tunnel-b", "tunnel-c"} {
		if err := store.Add(state.TunnelState{
			Name:               name,
			SourceContext:      "src-" + name,
			DestinationContext: "dst-" + name,
			Namespace:          "portal-system",
			TunnelPort:         10443,
			CreatedAt:          time.Now(),
			Mode:               "imperative",
		}); err != nil {
			t.Fatalf("failed to add %s: %v", name, err)
		}
	}

	var buf strings.Builder
	cmd := NewListCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	for _, name := range []string{"tunnel-a", "tunnel-b", "tunnel-c"} {
		if !strings.Contains(output, name) {
			t.Errorf("expected %q in output, got:\n%s", name, output)
		}
	}
}

func TestListJSONOutput(t *testing.T) {
	storePath := setupListTestHooks(t)

	store := state.NewStore(storePath)
	if err := store.Add(state.TunnelState{
		Name:               "json-tunnel",
		SourceContext:      "src",
		DestinationContext: "dst",
		Namespace:          "ns",
		TunnelPort:         9443,
		CreatedAt:          time.Now(),
		Mode:               "imperative",
	}); err != nil {
		t.Fatalf("failed to add tunnel: %v", err)
	}

	var buf strings.Builder
	cmd := NewListCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"name": "json-tunnel"`) {
		t.Errorf("expected JSON with tunnel name, got:\n%s", output)
	}
	if !strings.Contains(output, `"tunnel_port": 9443`) {
		t.Errorf("expected JSON with tunnel port, got:\n%s", output)
	}
}

func TestListRejectsArgs(t *testing.T) {
	cmd := NewListCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"extra-arg"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when args provided")
	}
}
