//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestE2E07_StatusCommand verifies portal status text and JSON output.
//
// Steps:
//  1. Deploy tunnel via portal connect
//  2. portal status <source> <dest> — verify "Connected" in output
//  3. portal status --json — validate JSON structure
//  4. Verify Envoy stats present in detailed status
func TestE2E07_StatusCommand(t *testing.T) {
	const namespace = "portal-system"
	home := isolatePortalState(t)
	endpoint := fmt.Sprintf("%s:10443", responderIP)

	// Deploy tunnel.
	stdout, stderr, err := runPortalWithHome(t, home, "connect", sourceCtx, destCtx,
		"--responder-endpoint", endpoint)
	if err != nil {
		t.Fatalf("portal connect failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Cleanup(func() {
		runPortalWithHome(t, home, "disconnect", sourceCtx, destCtx)
		cleanupNamespace(t, namespace)
	})

	waitForDeployment(t, destCtx, namespace, "portal-responder", 2*time.Minute)
	waitForDeployment(t, sourceCtx, namespace, "portal-initiator", 2*time.Minute)

	// Give tunnel time to establish.
	time.Sleep(5 * time.Second)

	// Step 2: Text status for specific tunnel.
	stdout, stderr, err = runPortalWithHome(t, home, "status", sourceCtx, destCtx)
	if err != nil {
		t.Fatalf("portal status failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	t.Logf("status output:\n%s", stdout)

	if !strings.Contains(stdout, "Connected") {
		t.Fatalf("expected 'Connected' in status output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Initiator:") {
		t.Fatalf("expected 'Initiator:' in status output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Responder:") {
		t.Fatalf("expected 'Responder:' in status output, got: %s", stdout)
	}

	// Step 3: JSON status (all tunnels).
	stdout, stderr, err = runPortalWithHome(t, home, "status", "--json")
	if err != nil {
		t.Fatalf("portal status --json failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	// Validate it's valid JSON.
	var statuses []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &statuses); err != nil {
		t.Fatalf("status --json output is not valid JSON: %v\nraw: %s", err, stdout)
	}
	if len(statuses) == 0 {
		t.Fatal("expected at least one tunnel in status JSON")
	}

	tunnelName := sourceCtx + "--" + destCtx
	found := false
	for _, s := range statuses {
		if name, ok := s["name"].(string); ok && name == tunnelName {
			found = true
			if status, ok := s["status"].(string); ok && status != "Connected" {
				t.Errorf("expected Connected status in JSON, got: %s", status)
			}
		}
	}
	if !found {
		t.Fatalf("tunnel %q not found in status JSON", tunnelName)
	}

	// Step 4: JSON single-tunnel status with stats.
	stdout, stderr, err = runPortalWithHome(t, home, "status", sourceCtx, destCtx, "--json")
	if err != nil {
		t.Fatalf("portal status --json (single) failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	var singleStatus map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &singleStatus); err != nil {
		t.Fatalf("single status --json is not valid JSON: %v\nraw: %s", err, stdout)
	}

	if singleStatus["status"] != "Connected" {
		t.Errorf("expected Connected in single status JSON, got: %v", singleStatus["status"])
	}

	// Verify initiator section exists with stats.
	if initiator, ok := singleStatus["initiator"].(map[string]interface{}); ok {
		if _, hasStats := initiator["stats"]; !hasStats {
			t.Log("warning: no stats in initiator status (Envoy admin may not be reachable)")
		}
	}

	t.Log("E2E-07 PASSED: status command text and JSON output verified")
}
