package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnlanda/portal/internal/manifest"
)

func TestRotateCertsCmdRequiresTunnelDir(t *testing.T) {
	cmd := NewRotateCertsCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{}) // no args

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no tunnel-dir provided")
	}
}

func TestRotateCertsCmdSuccess(t *testing.T) {
	// Generate a tunnel to rotate.
	cfg := manifest.TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}
	bundle, err := manifest.Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	tunnelDir := filepath.Join(t.TempDir(), "tunnel")
	if err := os.MkdirAll(tunnelDir, 0755); err != nil {
		t.Fatalf("failed to create tunnel dir: %v", err)
	}
	if err := manifest.WriteToDisk(bundle, tunnelDir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	// Run the rotate-certs command.
	var out strings.Builder
	cmd := NewRotateCertsCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&out)
	cmd.SetArgs([]string{tunnelDir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Rotated certificates") {
		t.Errorf("output should mention rotation, got: %s", output)
	}
	if !strings.Contains(output, "Rotation count: 1") {
		t.Errorf("output should show rotation count 1, got: %s", output)
	}
}
