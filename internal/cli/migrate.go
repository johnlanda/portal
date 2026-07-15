package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewMigrateCmd creates the `portal migrate` command.
//
// Migration from a v1 tunnel to the hub/member model is a re-key, not a
// translation: v1 uses a per-tunnel CA whose leaves cannot become hub-CA
// leaves, so the tunnel must be torn down and re-established, with a brief
// outage. This command is deliberately GUIDED — it inspects the tunnel
// record and prints the exact command sequence for the operator to review
// and run, rather than performing a multi-step, hard-to-unwind orchestration
// automatically.
func NewMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate <source_context> <destination_context>",
		Short: "Print the command sequence to migrate a v1 tunnel to the hub/member model",
		Long: `Inspects a v1 tunnel and prints the exact commands that recreate it as a
hub (destination side) and member (source side), including every exposed
service. Migration re-keys the tunnel — the v1 per-tunnel CA is replaced by
a hub CA — so connectivity is briefly interrupted between 'portal
disconnect' and the completed 'portal join'. Review the plan, schedule the
window, and run the commands in order.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd, args[0], args[1])
		},
	}
	return cmd
}

func runMigrate(cmd *cobra.Command, sourceCtx, destCtx string) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	tunnelName := sourceCtx + "--" + destCtx
	tunnel, err := store.Get(tunnelName)
	if err != nil {
		return fmt.Errorf("failed to read tunnel state: %w", err)
	}
	if tunnel == nil {
		return fmt.Errorf("tunnel %q not found", tunnelName)
	}
	if tunnel.DeployTarget == "bare-metal" {
		return fmt.Errorf("tunnel %q is a bare-metal deployment; hub/member migration currently covers Kubernetes tunnels only", tunnelName)
	}

	hubName := destCtx
	memberName := sourceCtx
	out := cmd.OutOrStdout()

	fmt.Fprintf(out, "Migration plan for tunnel %q (v1 → hub/member)\n", tunnelName)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "⚠ Migration RE-KEYS the tunnel: the v1 per-tunnel CA is replaced by a hub")
	fmt.Fprintln(out, "  CA, and connectivity is interrupted between steps 1 and 4. Schedule a")
	fmt.Fprintln(out, "  maintenance window. Review each command before running it.")
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "  # 1. Tear down the v1 tunnel (outage begins)\n")
	fmt.Fprintf(out, "  portal disconnect %s %s\n\n", sourceCtx, destCtx)
	fmt.Fprintf(out, "  # 2. Deploy the hub on the ingress-capable side\n")
	fmt.Fprintf(out, "  portal hub init %s --name %s --namespace %s --tunnel-port %d\n\n", destCtx, hubName, tunnel.Namespace, tunnel.TunnelPort)
	fmt.Fprintf(out, "  # 3. Enroll the egress-only side as a member (single-operator shortcut;\n")
	fmt.Fprintf(out, "  #    use the CSR flow instead if two parties operate the clusters)\n")
	fmt.Fprintf(out, "  portal hub invite %s -o %s.credential\n", memberName, memberName)
	fmt.Fprintf(out, "  portal join %s --credential %s.credential --namespace %s\n\n", sourceCtx, memberName, tunnel.Namespace)

	services := tunnel.AllServiceEntries()
	if len(services) > 0 {
		fmt.Fprintf(out, "  # 4. Recreate each exposed service on the forward path (outage ends\n")
		fmt.Fprintf(out, "  #    per service as its listener comes up)\n")
		for _, se := range services {
			sni := se.SNI
			if sni == "" {
				sni = se.Name
			}
			localPort := se.LocalPort
			if localPort == 0 {
				localPort = se.Port
			}
			ns := se.Namespace
			if ns == "" {
				ns = "default"
			}
			fmt.Fprintf(out, "  portal hub expose %s --port %d --service-namespace %s --sni %s\n", se.Name, se.Port, ns, sni)
			fmt.Fprintf(out, "  portal forward %s %s --local-port %d\n", sourceCtx, sni, localPort)
		}
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "  # Note: forward Service names change from the v1 shapes to portal-fwd-<sni>;")
		fmt.Fprintln(out, "  # update any consumers pinned to the old ClusterIP Service names.")
	} else {
		fmt.Fprintf(out, "  # 4. No exposed services recorded for this tunnel.\n")
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "  # 5. Verify, then the hub can also reach member services via 'portal publish'\n")
	fmt.Fprintf(out, "  portal status %s\n", memberName)
	return nil
}
