package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/state"
	"github.com/johnlanda/portal/internal/validate"
)

// The forward path is the v1 mechanism carried over: member-local apps dial a
// local listener, which proxies over mTLS to a hub-side backend, multiplexed
// by SNI on the shared tunnel listener. It complements the reverse path (hub
// → member) and is the only path that carries raw TCP. Each service has two
// halves owned by different parties: the hub owner exposes the backend
// ('portal hub expose'), the member owner adds a local listener
// ('portal forward').

// newHubExposeCmd creates the `portal hub expose` command.
func newHubExposeCmd() *cobra.Command {
	var (
		hubName          string
		port             int
		serviceNamespace string
		sni              string
	)
	cmd := &cobra.Command{
		Use:   "expose <service> --port <port>",
		Short: "Expose a hub-local service to members over the forward path",
		Long: `Adds an SNI filter chain to the hub's shared tunnel listener that routes
to a hub-local backend. Members reach it by adding a local listener with
'portal forward <ctx> <sni> --local-port <port>'.

The forward path is protocol-agnostic (plain TCP proxying), unlike the
HTTP/2-only reverse path.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubExpose(cmd, args[0], hubName, port, serviceNamespace, sni)
		},
	}
	cmd.Flags().StringVar(&hubName, "hub", "", "Hub name (required when multiple hubs exist)")
	cmd.Flags().IntVar(&port, "port", 0, "Port the backend service listens on (required)")
	_ = cmd.MarkFlagRequired("port")
	cmd.Flags().StringVar(&serviceNamespace, "service-namespace", "default", "Namespace of the backend service")
	cmd.Flags().StringVar(&sni, "sni", "", "SNI value for routing (default: the service name)")
	return cmd
}

func runHubExpose(cmd *cobra.Command, service, hubName string, port int, serviceNamespace, sni string) error {
	if err := validate.DNSName(service); err != nil {
		return fmt.Errorf("invalid service name: %w", err)
	}
	if sni == "" {
		sni = service
	}
	if err := validate.DNSName(sni); err != nil {
		return fmt.Errorf("invalid SNI: %w", err)
	}

	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	hub, err := loadHub(store, hubName)
	if err != nil {
		return err
	}
	if sni == hub.HandshakeSNI {
		return fmt.Errorf("SNI %q is reserved for the reverse tunnel handshake", sni)
	}
	for _, s := range hub.Services {
		if s.SNI == sni {
			return fmt.Errorf("SNI %q is already exposed (service %s)", sni, s.Name)
		}
	}

	hub.Services = append(hub.Services, state.ServiceEntry{
		Name:      fmt.Sprintf("%s.%s.svc.cluster.local", service, serviceNamespace),
		Namespace: serviceNamespace,
		Port:      port,
		SNI:       sni,
	})
	if err := applyHub(context.Background(), hub); err != nil {
		return err
	}
	if err := store.UpdateHub(*hub); err != nil {
		return fmt.Errorf("failed to save hub state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Exposed %s (SNI %s) on the forward path\n", service, sni)
	fmt.Fprintf(out, "\nMembers reach it by adding a local listener:\n")
	fmt.Fprintf(out, "  portal forward <member-ctx> %s --local-port %d\n", sni, port)
	return nil
}

// NewForwardCmd creates the `portal forward` command.
func NewForwardCmd() *cobra.Command {
	var (
		member    string
		localPort int
		remove    bool
	)
	cmd := &cobra.Command{
		Use:   "forward <context> <sni>",
		Short: "Add a member-local listener for a hub-exposed forward service",
		Long: `Adds a local listener on the member Envoy (plus a ClusterIP Service) that
proxies to a hub-side service exposed with 'portal hub expose'. Local apps
dial the listener; traffic rides the same mTLS endpoint as the reverse
tunnel, routed by SNI. Carries any TCP protocol.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runForward(cmd, args[0], args[1], member, localPort, remove)
		},
	}
	cmd.Flags().StringVar(&member, "member", "", "Member name (required when the context has multiple memberships)")
	cmd.Flags().IntVar(&localPort, "local-port", 0, "Local listener port (required unless --remove)")
	cmd.Flags().BoolVar(&remove, "remove", false, "Remove the forward listener instead of adding it")
	return cmd
}

func runForward(cmd *cobra.Command, kubeContext, sni, member string, localPort int, remove bool) error {
	if err := validate.DNSName(sni); err != nil {
		return fmt.Errorf("invalid SNI: %w", err)
	}
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	membership, err := membershipByContext(store, kubeContext, member)
	if err != nil {
		return err
	}
	if membership.Pending {
		return fmt.Errorf("membership %q is still pending; complete enrollment first", membership.Member)
	}

	out := cmd.OutOrStdout()
	if remove {
		found := false
		for i, f := range membership.Forward {
			if f.SNI == sni || f.Name == sni {
				membership.Forward = append(membership.Forward[:i], membership.Forward[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no forward listener for %q", sni)
		}
	} else {
		if localPort == 0 {
			return fmt.Errorf("--local-port is required")
		}
		for _, f := range membership.Forward {
			if f.SNI == sni {
				return fmt.Errorf("forward listener for %q already exists; use --remove first", sni)
			}
			if f.LocalPort == localPort {
				return fmt.Errorf("local port %d is already used by forward listener %q", localPort, f.SNI)
			}
		}
		if sni == membership.HandshakeSNI {
			return fmt.Errorf("SNI %q is reserved for the reverse tunnel handshake", sni)
		}
		membership.Forward = append(membership.Forward, state.ServiceEntry{
			Name:      sanitizeName(sni),
			SNI:       sni,
			LocalPort: localPort,
		})
	}

	if err := applyMember(context.Background(), membership); err != nil {
		return err
	}
	if err := store.UpdateMembership(*membership); err != nil {
		return fmt.Errorf("failed to save membership state: %w", err)
	}

	if remove {
		fmt.Fprintf(out, "✓ Removed forward listener %s from member %s\n", sni, membership.Member)
	} else {
		fmt.Fprintf(out, "✓ Forward listener for %s on port %d\n", sni, localPort)
		fmt.Fprintf(out, "  Local apps dial: portal-fwd-%s.%s:%d\n", sanitizeName(sni), membership.Namespace, localPort)
	}
	return nil
}

// sanitizeName converts an SNI into a valid resource name fragment.
func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
