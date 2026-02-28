//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestE2E15_LongRunningStability verifies the tunnel remains stable over time.
//
// Steps:
//  1. Deploy tunnel with echo server
//  2. Send traffic every 10 seconds for 5 minutes
//  3. Verify no connection drops or errors
//  4. Check pod restart count stays at 0
//  5. Verify Envoy stats show no upstream_cx_connect_fail
func TestE2E15_LongRunningStability(t *testing.T) {
	const namespace = "portal-system"

	// Deploy tunnel with echo sidecar.
	_ = deployTunnelWithEcho(t, namespace)

	// Give tunnel time to establish.
	time.Sleep(10 * time.Second)

	// Verify initial connectivity.
	verifyTunnelData(t, namespace)

	// Run stability loop: 5 minutes, request every 10 seconds = 30 iterations.
	const (
		duration = 5 * time.Minute
		interval = 10 * time.Second
	)

	iterations := int(duration / interval)
	failures := 0
	start := time.Now()

	for i := 0; i < iterations; i++ {
		body, err := portForwardGet(t, sourceCtx, namespace, "deployment/portal-initiator", 10443, "/")
		if err != nil {
			t.Logf("iteration %d/%d: request failed: %v", i+1, iterations, err)
			failures++
		} else if !strings.Contains(body, "Hello from the destination") {
			t.Logf("iteration %d/%d: unexpected response: %s", i+1, iterations, body)
			failures++
		} else {
			if (i+1)%6 == 0 { // Log progress every minute.
				t.Logf("iteration %d/%d: OK (elapsed: %s)", i+1, iterations, time.Since(start).Round(time.Second))
			}
		}

		if i < iterations-1 {
			time.Sleep(interval)
		}
	}

	elapsed := time.Since(start)
	t.Logf("stability test completed: %d/%d successful, %d failures (elapsed: %s)",
		iterations-failures, iterations, failures, elapsed.Round(time.Second))

	if failures > 0 {
		t.Fatalf("stability test failed: %d/%d requests failed", failures, iterations)
	}

	// Check pod restart counts.
	checkRestarts(t, sourceCtx, namespace, "app.kubernetes.io/name=portal-initiator", "initiator")
	checkRestarts(t, destCtx, namespace, "app.kubernetes.io/name=portal-responder", "responder")

	// Check Envoy stats for connection failures.
	checkNoConnectFail(t, namespace)

	t.Log("E2E-15 PASSED: 5-minute stability test, zero errors, zero restarts")
}

// checkRestarts verifies that all pods matching the label selector have zero restarts.
func checkRestarts(t *testing.T, kubeCtx, namespace, labelSelector, name string) {
	t.Helper()

	out := kubectlWithContext(t, kubeCtx, "get", "pods", "-n", namespace,
		"-l", labelSelector,
		"-o", "jsonpath={.items[*].status.containerStatuses[*].restartCount}")

	counts := strings.Fields(strings.TrimSpace(out))
	for _, c := range counts {
		if c != "0" {
			t.Errorf("%s pod has non-zero restart count: %s", name, out)
			return
		}
	}
	t.Logf("%s restart count: 0", name)
}

// checkNoConnectFail verifies the initiator Envoy has no upstream_cx_connect_fail.
func checkNoConnectFail(t *testing.T, namespace string) {
	t.Helper()

	body, err := portForwardGet(t, sourceCtx, namespace, "deployment/portal-initiator", 15000, "/stats?usedonly&format=json")
	if err != nil {
		t.Logf("warning: could not fetch Envoy stats for connect_fail check: %v", err)
		return
	}

	var statsResp struct {
		Stats []struct {
			Name  string `json:"name"`
			Value int64  `json:"value"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(body), &statsResp); err != nil {
		t.Logf("warning: failed to parse Envoy stats JSON: %v", err)
		return
	}

	for _, s := range statsResp.Stats {
		if strings.HasSuffix(s.Name, "upstream_cx_connect_fail") && s.Value > 0 {
			t.Errorf("Envoy has upstream_cx_connect_fail > 0: %s = %d", s.Name, s.Value)
		}
	}
}
