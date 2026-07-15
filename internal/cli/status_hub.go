package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/kube"
	"github.com/johnlanda/portal/internal/manifest"
	"github.com/johnlanda/portal/internal/state"
)

// v2 hub/member status. The caller may be the hub owner, the member owner,
// or both (single-operator): output is computed from whichever records and
// contexts this machine has, and degrades honestly for the rest.

// memberStatus is the hub-side view of one member.
type memberStatus struct {
	Member       string        `json:"member"`
	Hub          string        `json:"hub"`
	Tenant       string        `json:"tenant,omitempty"`
	CertSerial   string        `json:"cert_serial,omitempty"`
	CertExpiry   time.Time     `json:"cert_expiry,omitempty"`
	Evicted      bool          `json:"evicted"`
	HubPod       *podStatus    `json:"hub_pod,omitempty"`
	Handshakes   *tunnelCounts `json:"handshakes,omitempty"`
	Routes       []routeProbe  `json:"routes,omitempty"`
	Membership   *localMember  `json:"membership,omitempty"`
	ProbeWarning string        `json:"probe_warning,omitempty"`
}

// tunnelCounts are the hub's reverse tunnel handshake counters (aggregate
// across members — per-member counters require detailed stats upstream).
type tunnelCounts struct {
	Accepted         int64 `json:"accepted"`
	Rejected         int64 `json:"rejected"`
	ValidationFailed int64 `json:"validation_failed"`
	ParseError       int64 `json:"parse_error"`
}

// routeProbe is the result of probing one routed alias through the tunnel.
type routeProbe struct {
	Service string `json:"service"`
	Alias   string `json:"alias"`
	State   string `json:"state"` // reachable | not-published | unreachable | unknown
	Detail  string `json:"detail,omitempty"`
}

// localMember is the member-owner's view (present in single-operator setups
// or on the member owner's machine).
type localMember struct {
	Context   string     `json:"context"`
	Namespace string     `json:"namespace"`
	HubAddr   string     `json:"hub_addr"`
	Pending   bool       `json:"pending"`
	Published []string   `json:"published,omitempty"`
	Pod       *podStatus `json:"pod,omitempty"`
}

// Testability hooks for live probes.
var (
	fetchHandshakeStatsFn = fetchHandshakeStats
	probeRouteFn          = probeRoute
)

// statusMemberArg reports whether the argument names a v2 member or
// membership known to this machine.
func statusMemberArg(store *state.Store, arg string) bool {
	if _, err := store.GetMembership(arg); err == nil {
		return true
	}
	hubs, err := store.ListHubs()
	if err != nil {
		return false
	}
	for i := range hubs {
		if hubs[i].Member(arg) != nil {
			return true
		}
	}
	return false
}

func runStatusMember(cmd *cobra.Command, member string, opts statusOpts) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	s := memberStatus{Member: member}

	// Hub-side view.
	hubs, err := store.ListHubs()
	if err != nil {
		return err
	}
	var hub *state.HubState
	for i := range hubs {
		if hubs[i].Member(member) != nil {
			hub = &hubs[i]
			break
		}
	}
	if hub != nil {
		record := hub.Member(member)
		s.Hub = hub.Name
		s.Tenant = record.Tenant
		s.CertSerial = record.CertSerial
		s.CertExpiry = record.CertExpiry
		s.Evicted = record.Evicted

		ctx := context.Background()
		client := newKubeClient(hub.Context, hub.Namespace)
		pods, err := client.GetPods(ctx, "app.kubernetes.io/name=portal-hub")
		if err != nil {
			s.ProbeWarning = fmt.Sprintf("failed to query hub pods: %v", err)
		} else if len(pods) > 0 {
			p := pods[0]
			s.HubPod = &podStatus{Ready: p.Ready, Phase: string(p.Phase), Restarts: p.Restarts, PodName: p.Name}
			if p.Ready {
				if warn := checkEnvoyServerVersion(ctx, client, p.Name); warn != "" {
					s.ProbeWarning = warn
				}
				s.Handshakes = fetchHandshakeStatsFn(ctx, client, p.Name)
				for _, r := range hub.Routes {
					if r.Member != member {
						continue
					}
					probe := probeRouteFn(ctx, client, p.Name, hub.EgressPort, r.Service, member)
					probe.Alias = r.AliasService
					s.Routes = append(s.Routes, probe)
				}
			}
		}
	}

	// Member-side view.
	if membership, err := store.GetMembership(member); err == nil {
		lm := &localMember{
			Context:   membership.Context,
			Namespace: membership.Namespace,
			HubAddr:   membership.HubAddr,
			Pending:   membership.Pending,
		}
		for _, p := range membership.Published {
			lm.Published = append(lm.Published, fmt.Sprintf("%s :%d %s", p.Name, p.Port, p.Protocol))
		}
		if s.Hub == "" {
			s.Hub = membership.Hub
		}
		if !membership.Pending {
			pods, err := newKubeClient(membership.Context, membership.Namespace).
				GetPods(context.Background(), "app.kubernetes.io/name=portal-member")
			if err == nil && len(pods) > 0 {
				p := pods[0]
				lm.Pod = &podStatus{Ready: p.Ready, Phase: string(p.Phase), Restarts: p.Restarts, PodName: p.Name}
			}
		}
		s.Membership = lm
	}

	if hub == nil && s.Membership == nil {
		return fmt.Errorf("no hub record or membership found for %q", member)
	}

	out := cmd.OutOrStdout()
	if opts.outputJSON {
		return printJSON(out, s)
	}
	printMemberStatus(out, s)
	return nil
}

func printMemberStatus(out io.Writer, s memberStatus) {
	fmt.Fprintf(out, "MEMBER %s (hub %s)\n", s.Member, s.Hub)
	if s.CertSerial != "" {
		state := "active"
		if s.Evicted {
			state = "EVICTED"
		}
		fmt.Fprintf(out, "  identity    SAN %s, serial %s, %s", s.Member, s.CertSerial, state)
		if !s.CertExpiry.IsZero() {
			fmt.Fprintf(out, ", cert expires %s (%s)", s.CertExpiry.Format("2006-01-02"), formatDuration(time.Until(s.CertExpiry)))
		}
		fmt.Fprintln(out)
	}
	if s.HubPod != nil {
		fmt.Fprintf(out, "  hub pod     %s  phase=%s ready=%s restarts=%d\n", s.HubPod.PodName, s.HubPod.Phase, boolStr(s.HubPod.Ready), s.HubPod.Restarts)
	}
	if s.Handshakes != nil {
		fmt.Fprintf(out, "  handshakes  %d accepted / %d rejected / %d validation_failed (hub totals)\n",
			s.Handshakes.Accepted, s.Handshakes.Rejected, s.Handshakes.ValidationFailed)
		if s.Handshakes.ValidationFailed > 0 {
			fmt.Fprintln(out, "              ⚠ validation failures indicate identity mismatches — investigate")
		}
	}
	for _, r := range s.Routes {
		fmt.Fprintf(out, "  route       %s → %s.%s  [%s]%s\n", r.Alias, r.Service, s.Member, r.State, suffixIf(r.Detail))
	}
	if m := s.Membership; m != nil {
		fmt.Fprintf(out, "  membership  context=%s hub=%s", m.Context, m.HubAddr)
		if m.Pending {
			fmt.Fprint(out, "  [PENDING — complete enrollment with 'portal join --cert']")
		}
		fmt.Fprintln(out)
		if m.Pod != nil {
			fmt.Fprintf(out, "  member pod  %s  phase=%s ready=%s restarts=%d\n", m.Pod.PodName, m.Pod.Phase, boolStr(m.Pod.Ready), m.Pod.Restarts)
		}
		for _, p := range m.Published {
			fmt.Fprintf(out, "  published   %s\n", p)
		}
	}
	if s.ProbeWarning != "" {
		fmt.Fprintf(out, "  ⚠ %s\n", s.ProbeWarning)
	}
}

func suffixIf(detail string) string {
	if detail == "" {
		return ""
	}
	return " — " + detail
}

// printHubMemberSummary appends hub and membership summaries to `portal
// status` output.
func printHubMemberSummary(out io.Writer, store *state.Store) {
	hubs, _ := store.ListHubs()
	for _, h := range hubs {
		active, evicted := 0, 0
		for _, m := range h.Members {
			if m.Evicted {
				evicted++
			} else {
				active++
			}
		}
		fmt.Fprintf(out, "◆ hub %s  %s  [%d members", h.Name, h.PublicAddr, active)
		if evicted > 0 {
			fmt.Fprintf(out, ", %d evicted", evicted)
		}
		fmt.Fprintf(out, ", %d routes]\n", len(h.Routes))
		for _, m := range h.Members {
			marker := "✓"
			note := ""
			if m.Evicted {
				marker = "✗"
				note = " (evicted)"
			} else if !m.CertExpiry.IsZero() && time.Until(m.CertExpiry) < 30*24*time.Hour {
				note = fmt.Sprintf(" (cert expires in %s — rotate soon)", formatDuration(time.Until(m.CertExpiry)))
			}
			fmt.Fprintf(out, "  %s %s%s\n", marker, m.Name, note)
		}
	}
	memberships, _ := store.ListMemberships()
	for _, m := range memberships {
		stateStr := "enrolled"
		if m.Pending {
			stateStr = "PENDING enrollment"
		}
		fmt.Fprintf(out, "◇ membership %s → %s  [%s, %d published]\n", m.Member, m.HubAddr, stateStr, len(m.Published))
	}
}

// fetchHandshakeStats reads the reverse tunnel handshake counters from the
// hub Envoy admin endpoint. Best-effort: returns nil on any failure.
func fetchHandshakeStats(ctx context.Context, client kube.Client, podName string) *tunnelCounts {
	body := fetchAdminPath(ctx, client, podName, envoy.DefaultResponderAdminPort, "/stats?filter=reverse_tunnel&format=json")
	if body == nil {
		return nil
	}
	var resp envoyStatsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	counts := &tunnelCounts{}
	for _, s := range resp.Stats {
		switch {
		case strings.HasSuffix(s.Name, ".accepted"):
			counts.Accepted += s.Value
		case strings.HasSuffix(s.Name, ".rejected"):
			counts.Rejected += s.Value
		case strings.HasSuffix(s.Name, ".validation_failed"):
			counts.ValidationFailed += s.Value
		case strings.HasSuffix(s.Name, ".parse_error"):
			counts.ParseError += s.Value
		}
	}
	return counts
}

// probeRoute sends a request through the hub egress listener addressed to a
// member service and classifies the outcome, distinguishing the publish/route
// half-states from a down tunnel.
func probeRoute(ctx context.Context, client kube.Client, podName string, egressPort int, service, member string) routeProbe {
	probe := routeProbe{Service: service, State: "unknown"}
	if egressPort == 0 {
		egressPort = envoy.DefaultHubEgressPort
	}

	localPort, err := findFreePort()
	if err != nil {
		probe.Detail = err.Error()
		return probe
	}
	session, err := client.PortForward(ctx, "pod/"+podName, localPort, egressPort)
	if err != nil || session == nil {
		probe.Detail = "port-forward failed"
		return probe
	}
	defer func() { _ = session.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", localPort), nil)
	if err != nil {
		probe.Detail = err.Error()
		return probe
	}
	req.Host = fmt.Sprintf("%s.%s", service, member)
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		probe.State = "tunnel-down"
		probe.Detail = err.Error()
		return probe
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode < 400:
		probe.State = "reachable"
		probe.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		// Some Envoy builds surface an allowlist miss as a clean 404 from the
		// member's route table.
		probe.State = "not-published"
		probe.Detail = "member returned 404: service not in publish allowlist"
	default:
		// 5xx over the reverse connection is ambiguous: the member may be
		// disconnected (no cached socket) OR connected but without a matching
		// published route (the member resets the unmatched stream, which the
		// egress surfaces as 5xx rather than 404). The egress alone can't tell
		// these apart — correlate with the handshake counters above: nonzero
		// accepted with a live connection points to not-published.
		probe.State = "unreachable"
		probe.Detail = fmt.Sprintf("egress returned %d: member disconnected or service not published", resp.StatusCode)
	}
	return probe
}

// checkEnvoyServerVersion compares the hub Envoy's live version against the
// supported minors and returns a warning string on mismatch. Best-effort.
func checkEnvoyServerVersion(ctx context.Context, client kube.Client, podName string) string {
	body := fetchAdminPath(ctx, client, podName, envoy.DefaultResponderAdminPort, "/server_info")
	if body == nil {
		return ""
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &info); err != nil || info.Version == "" {
		return ""
	}
	for _, minor := range manifest.SupportedEnvoyMinors {
		if strings.Contains(info.Version, minor+".") {
			return ""
		}
	}
	return fmt.Sprintf("hub is running Envoy %q, which this Portal release does not support (supported: %s) — reverse tunnel behavior is not guaranteed",
		info.Version, strings.Join(manifest.SupportedEnvoyMinors, ", "))
}

// fetchAdminPath port-forwards to an Envoy admin port and GETs a path.
// Best-effort: returns nil on any failure.
func fetchAdminPath(ctx context.Context, client kube.Client, podName string, adminPort int, path string) []byte {
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
	resp, err := httpClient.Get(fmt.Sprintf("http://127.0.0.1:%d%s", localPort, path))
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	return body
}
