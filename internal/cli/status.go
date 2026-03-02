package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/kube"
	"github.com/johnlanda/portal/internal/state"
)

type statusOpts struct {
	outputJSON bool
}

// NewStatusCmd creates the `portal status` command.
func NewStatusCmd() *cobra.Command {
	var opts statusOpts

	cmd := &cobra.Command{
		Use:   "status [<source_context> <destination_context>]",
		Short: "Show tunnel status and connection details",
		Long: `Show the live status of one or all Portal tunnels.

With no arguments, shows a summary of all tunnels.
With two arguments, shows detailed status for a specific tunnel including
pod health, restart counts, and service endpoints.`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return fmt.Errorf("expected 0 or 2 arguments, got 1")
			}
			if len(args) == 2 {
				return runStatusSingle(cmd, args[0], args[1], opts)
			}
			return runStatusAll(cmd, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.outputJSON, "json", false, "Output in JSON format")

	return cmd
}

// tunnelStatus holds the computed status for a single tunnel.
type tunnelStatus struct {
	Name               string          `json:"name"`
	SourceContext      string          `json:"source_context"`
	DestinationContext string          `json:"destination_context"`
	Namespace          string          `json:"namespace"`
	TunnelPort         int             `json:"tunnel_port"`
	Status             string          `json:"status"`
	Initiator          *podStatus      `json:"initiator,omitempty"`
	Responder          *podStatus      `json:"responder,omitempty"`
	ResponderEndpoint  string          `json:"responder_endpoint,omitempty"`
	Services           []serviceHealth `json:"services,omitempty"`
	Error              string          `json:"error,omitempty"`
}

// serviceHealth reports the health of a single service routed through the tunnel.
type serviceHealth struct {
	Name      string `json:"name"`
	SNI       string `json:"sni"`
	Port      int    `json:"port"`
	LocalPort int    `json:"local_port,omitempty"`
	Direction string `json:"direction,omitempty"`
	Healthy   *bool  `json:"healthy,omitempty"`
}

type podStatus struct {
	Ready    bool        `json:"ready"`
	Phase    string      `json:"phase"`
	Restarts int32       `json:"restarts"`
	PodName  string      `json:"pod_name"`
	Stats    *envoyStats `json:"stats,omitempty"`
}

// envoyStats holds key metrics from the Envoy admin /stats endpoint.
type envoyStats struct {
	UptimeSeconds     int64 `json:"uptime_seconds"`
	ActiveConnections int64 `json:"active_connections"`
	TotalConnections  int64 `json:"total_connections"`
	BytesSent         int64 `json:"bytes_sent"`
	BytesReceived     int64 `json:"bytes_received"`
}

func runStatusAll(cmd *cobra.Command, opts statusOpts) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	tunnels, err := store.List()
	if err != nil {
		return fmt.Errorf("failed to load tunnel state: %w", err)
	}

	out := cmd.OutOrStdout()

	if len(tunnels) == 0 {
		fmt.Fprintln(out, "No tunnels found.")
		return nil
	}

	// Query live status for each tunnel. Errors are captured per-tunnel, not fatal.
	statuses := make([]tunnelStatus, 0, len(tunnels))
	for _, t := range tunnels {
		ts := queryTunnelStatus(t)
		statuses = append(statuses, ts)
	}

	if opts.outputJSON {
		return printJSON(out, statuses)
	}

	for i, s := range statuses {
		if i > 0 {
			fmt.Fprintln(out)
		}
		printStatusSummary(cmd, s)
	}
	return nil
}

func runStatusSingle(cmd *cobra.Command, sourceCtx, destCtx string, opts statusOpts) error {
	tunnelName := sourceCtx + "--" + destCtx

	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	ts, err := store.Get(tunnelName)
	if err != nil {
		return fmt.Errorf("failed to read tunnel state: %w", err)
	}
	if ts == nil {
		return fmt.Errorf("tunnel %q not found in state", tunnelName)
	}

	status := queryTunnelStatus(*ts)

	// Enrich with Envoy admin stats (only for single-tunnel detail view).
	enrichWithEnvoyStats(&status, *ts)

	out := cmd.OutOrStdout()
	if opts.outputJSON {
		return printJSON(out, status)
	}

	printStatusDetail(cmd, status)
	return nil
}

// queryTunnelStatus probes both clusters for live pod and service info.
func queryTunnelStatus(ts state.TunnelState) tunnelStatus {
	s := tunnelStatus{
		Name:               ts.Name,
		SourceContext:      ts.SourceContext,
		DestinationContext: ts.DestinationContext,
		Namespace:          ts.Namespace,
		TunnelPort:         ts.TunnelPort,
		Status:             "Unknown",
	}

	ctx := context.Background()

	// Query initiator pods from source cluster.
	sourceClient := newKubeClient(ts.SourceContext, ts.Namespace)
	initiatorPods, err := sourceClient.GetPods(ctx, "app.kubernetes.io/name=portal-initiator")
	if err != nil {
		s.Error = fmt.Sprintf("failed to query initiator pods: %v", err)
		return s
	}
	if len(initiatorPods) > 0 {
		p := initiatorPods[0]
		s.Initiator = &podStatus{
			Ready:    p.Ready,
			Phase:    string(p.Phase),
			Restarts: p.Restarts,
			PodName:  p.Name,
		}
	}

	// Query responder pods from destination cluster.
	destClient := newKubeClient(ts.DestinationContext, ts.Namespace)
	responderPods, err := destClient.GetPods(ctx, "app.kubernetes.io/name=portal-responder")
	if err != nil {
		s.Error = fmt.Sprintf("failed to query responder pods: %v", err)
		return s
	}
	if len(responderPods) > 0 {
		p := responderPods[0]
		s.Responder = &podStatus{
			Ready:    p.Ready,
			Phase:    string(p.Phase),
			Restarts: p.Restarts,
			PodName:  p.Name,
		}
	}

	// Query responder service for external endpoint.
	svcInfo, err := destClient.GetService(ctx, "portal-responder")
	if err == nil && svcInfo != nil {
		if len(svcInfo.LoadBalancerIngress) > 0 {
			addr := svcInfo.LoadBalancerIngress[0].Address()
			if addr != "" {
				s.ResponderEndpoint = fmt.Sprintf("%s:%d", addr, ts.TunnelPort)
			}
		}
	}

	// Populate per-service health from state.
	for _, se := range ts.AllServiceEntries() {
		sh := serviceHealth{
			Name:      se.Name,
			SNI:       se.SNI,
			Port:      se.Port,
			LocalPort: se.LocalPort,
			Direction: se.Direction,
		}
		s.Services = append(s.Services, sh)
	}

	// Determine overall status.
	s.Status = deriveStatus(s)
	return s
}

func deriveStatus(s tunnelStatus) string {
	if s.Error != "" {
		return "Error"
	}
	if s.Initiator == nil || s.Responder == nil {
		return "Pending"
	}
	if s.Initiator.Ready && s.Responder.Ready {
		return "Connected"
	}
	if s.Initiator.Phase == string(kube.PodFailed) || s.Responder.Phase == string(kube.PodFailed) {
		return "Failed"
	}
	return "Degraded"
}

// fetchEnvoyStatsFn is a testability hook for fetching Envoy admin stats.
var fetchEnvoyStatsFn = fetchEnvoyStats

// fetchClusterHealthFn is a testability hook for fetching per-cluster health from Envoy admin.
var fetchClusterHealthFn = fetchClusterHealth

// enrichWithEnvoyStats fetches Envoy admin stats for both pods and attaches them.
// It also queries per-cluster health from the responder to determine per-service status.
func enrichWithEnvoyStats(s *tunnelStatus, ts state.TunnelState) {
	ctx := context.Background()

	if s.Initiator != nil && s.Initiator.Ready {
		sourceClient := newKubeClient(ts.SourceContext, ts.Namespace)
		stats := fetchEnvoyStatsFn(ctx, sourceClient, s.Initiator.PodName, envoy.DefaultInitiatorAdminPort)
		if stats != nil {
			s.Initiator.Stats = stats
		}
	}

	if s.Responder != nil && s.Responder.Ready {
		destClient := newKubeClient(ts.DestinationContext, ts.Namespace)
		stats := fetchEnvoyStatsFn(ctx, destClient, s.Responder.PodName, envoy.DefaultResponderAdminPort)
		if stats != nil {
			s.Responder.Stats = stats
		}

		// Fetch per-cluster health for service-level reporting.
		if len(s.Services) > 0 {
			clusterHealth := fetchClusterHealthFn(ctx, destClient, s.Responder.PodName, envoy.DefaultResponderAdminPort)
			if clusterHealth != nil {
				for i := range s.Services {
					sni := s.Services[i].SNI
					if sni == "" {
						sni = s.Services[i].Name
					}
					// Match cluster name pattern: "backend_to_<sni>" (from multi-service template).
					clusterName := "backend_to_" + sni
					if healthy, ok := clusterHealth[clusterName]; ok {
						h := healthy
						s.Services[i].Healthy = &h
					}
				}
			}
		}
	}
}

// fetchEnvoyStats port-forwards to the Envoy admin port and queries /stats.
// Returns nil with a logged warning on any failure — stats collection is best-effort.
func fetchEnvoyStats(ctx context.Context, client kube.Client, podName string, adminPort int) *envoyStats {
	localPort, err := findFreePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to find free port for %s stats: %v\n", podName, err)
		return nil
	}

	session, err := client.PortForward(ctx, "pod/"+podName, localPort, adminPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to port-forward to %s: %v\n", podName, err)
		return nil
	}
	if session == nil {
		return nil
	}
	defer func() { _ = session.Close() }()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/stats?usedonly&format=json", localPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to fetch stats from %s: %v\n", podName, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to read stats response from %s: %v\n", podName, err)
		return nil
	}

	return parseEnvoyStats(body)
}

// envoyStatsResponse mirrors the JSON structure from Envoy /stats?format=json.
type envoyStatsResponse struct {
	Stats []envoyStatEntry `json:"stats"`
}

type envoyStatEntry struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// parseEnvoyStats extracts key metrics from the Envoy stats JSON response.
func parseEnvoyStats(data []byte) *envoyStats {
	var resp envoyStatsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil
	}

	stats := &envoyStats{}
	for _, s := range resp.Stats {
		switch {
		case s.Name == "server.uptime":
			stats.UptimeSeconds = s.Value
		case strings.HasSuffix(s.Name, ".upstream_cx_active"):
			stats.ActiveConnections += s.Value
		case strings.HasSuffix(s.Name, ".upstream_cx_total"):
			stats.TotalConnections += s.Value
		case strings.HasSuffix(s.Name, ".upstream_cx_tx_bytes_total"):
			stats.BytesSent += s.Value
		case strings.HasSuffix(s.Name, ".upstream_cx_rx_bytes_total"):
			stats.BytesReceived += s.Value
		}
	}
	return stats
}

// fetchClusterHealth port-forwards to the Envoy admin port and queries /clusters?format=json.
// Returns a map of cluster name → healthy (true if any host is healthy).
func fetchClusterHealth(ctx context.Context, client kube.Client, podName string, adminPort int) map[string]bool {
	localPort, err := findFreePort()
	if err != nil {
		return nil
	}

	session, err := client.PortForward(ctx, "pod/"+podName, localPort, adminPort)
	if err != nil || session == nil {
		return nil
	}
	defer func() { _ = session.Close() }()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d/clusters?format=json", localPort))
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	return parseClusterHealth(body)
}

// envoyClustersResponse mirrors the JSON structure from /clusters?format=json.
type envoyClustersResponse struct {
	ClusterStatuses []envoyClusterStatus `json:"cluster_statuses"`
}

type envoyClusterStatus struct {
	Name         string            `json:"name"`
	HostStatuses []envoyHostStatus `json:"host_statuses"`
}

type envoyHostStatus struct {
	HealthStatus struct {
		EdsHealthStatus string `json:"eds_health_status"`
	} `json:"health_status"`
}

// parseClusterHealth extracts per-cluster health from the Envoy clusters JSON.
// Returns a map of cluster name → healthy.
func parseClusterHealth(data []byte) map[string]bool {
	var resp envoyClustersResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil
	}

	result := make(map[string]bool)
	for _, cs := range resp.ClusterStatuses {
		healthy := false
		for _, hs := range cs.HostStatuses {
			status := hs.HealthStatus.EdsHealthStatus
			if status == "HEALTHY" || status == "" {
				healthy = true
				break
			}
		}
		// If no hosts, mark as unhealthy (cluster exists but no endpoints).
		if len(cs.HostStatuses) == 0 {
			healthy = false
		}
		result[cs.Name] = healthy
	}
	return result
}

// findFreePort returns a free TCP port on localhost.
func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("failed to listen on ephemeral port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("failed to close ephemeral listener: %w", err)
	}
	return port, nil
}

func printStatusSummary(cmd *cobra.Command, s tunnelStatus) {
	out := cmd.OutOrStdout()
	var icon string
	switch s.Status {
	case "Connected":
		icon = "\u2713"
	case "Error", "Failed":
		icon = "\u2717"
	default:
		icon = "\u25cb"
	}
	fmt.Fprintf(out, "%s %s  %s \u2192 %s  [%s]\n", icon, s.Name, s.SourceContext, s.DestinationContext, s.Status)
}

func printStatusDetail(cmd *cobra.Command, s tunnelStatus) {
	out := cmd.OutOrStdout()

	fmt.Fprintf(out, "Tunnel:       %s\n", s.Name)
	fmt.Fprintf(out, "Source:       %s\n", s.SourceContext)
	fmt.Fprintf(out, "Destination:  %s\n", s.DestinationContext)
	fmt.Fprintf(out, "Namespace:    %s\n", s.Namespace)
	fmt.Fprintf(out, "Tunnel port:  %d\n", s.TunnelPort)
	fmt.Fprintf(out, "Status:       %s\n", s.Status)

	if s.ResponderEndpoint != "" {
		fmt.Fprintf(out, "Endpoint:     %s\n", s.ResponderEndpoint)
	}

	fmt.Fprintln(out)

	if s.Initiator != nil {
		fmt.Fprintln(out, "Initiator:")
		fmt.Fprintf(out, "  Pod:        %s\n", s.Initiator.PodName)
		fmt.Fprintf(out, "  Phase:      %s\n", s.Initiator.Phase)
		fmt.Fprintf(out, "  Ready:      %s\n", boolStr(s.Initiator.Ready))
		fmt.Fprintf(out, "  Restarts:   %d\n", s.Initiator.Restarts)
		printEnvoyStats(out, s.Initiator.Stats)
	} else {
		fmt.Fprintln(out, "Initiator:    No pods found")
	}

	fmt.Fprintln(out)

	if s.Responder != nil {
		fmt.Fprintln(out, "Responder:")
		fmt.Fprintf(out, "  Pod:        %s\n", s.Responder.PodName)
		fmt.Fprintf(out, "  Phase:      %s\n", s.Responder.Phase)
		fmt.Fprintf(out, "  Ready:      %s\n", boolStr(s.Responder.Ready))
		fmt.Fprintf(out, "  Restarts:   %d\n", s.Responder.Restarts)
		printEnvoyStats(out, s.Responder.Stats)
	} else {
		fmt.Fprintln(out, "Responder:    No pods found")
	}

	if len(s.Services) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Services:")
		for _, svc := range s.Services {
			healthStr := "unknown"
			if svc.Healthy != nil {
				if *svc.Healthy {
					healthStr = "healthy"
				} else {
					healthStr = "unhealthy"
				}
			}
			lp := svc.LocalPort
			if lp == 0 {
				lp = svc.Port
			}
			fmt.Fprintf(out, "  %s (SNI: %s)  port: %d  listener: %d  %s\n",
				svc.Name, svc.SNI, svc.Port, lp, healthStr)
		}
	}

	if s.Error != "" {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Error:        %s\n", s.Error)
	}
}

func printEnvoyStats(out io.Writer, stats *envoyStats) {
	if stats == nil {
		return
	}
	fmt.Fprintf(out, "  Uptime:     %s\n", formatDuration(time.Duration(stats.UptimeSeconds)*time.Second))
	fmt.Fprintf(out, "  Connections: %d active, %d total\n", stats.ActiveConnections, stats.TotalConnections)
	fmt.Fprintf(out, "  Traffic:    %s sent, %s received\n", formatBytes(stats.BytesSent), formatBytes(stats.BytesReceived))
}

// formatDuration formats a duration as a human-readable string.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours < 24 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func boolStr(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}
