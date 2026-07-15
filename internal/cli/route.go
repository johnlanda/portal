package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/manifest"
	"github.com/johnlanda/portal/internal/state"
	"github.com/johnlanda/portal/internal/validate"
)

// NewRouteCmd creates the `portal route` command.
func NewRouteCmd() *cobra.Command {
	var hubName, alias string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "route <member>/<service>",
		Short: "Mint a friendly hub-side name for a published member service",
		Long: `Creates a ClusterIP alias Service on the hub cluster so hub-side apps
can call a member service by plain Kubernetes DNS, e.g.
http://inference-acme-prod.portal-system/. The egress listener rewrites the
authority to the canonical <service>.<member> form before forwarding.

Routing itself needs no per-service action — this mints a name. Apps can
always address the egress listener directly with a <service>.<member> Host.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRoute(cmd, args[0], hubName, alias, dryRun)
		},
	}
	cmd.Flags().StringVar(&hubName, "hub", "", "Hub name (required when multiple hubs exist)")
	cmd.Flags().StringVar(&alias, "as", "", "Alias Service name (default: <service>-<member>)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the re-rendered manifests without applying")
	return cmd
}

func runRoute(cmd *cobra.Command, target, hubName, alias string, dryRun bool) error {
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("target must be <member>/<service>, got %q", target)
	}
	member, service := parts[0], parts[1]
	if alias == "" {
		alias = fmt.Sprintf("%s-%s", service, member)
	}
	if err := validate.DNSName(alias); err != nil {
		return fmt.Errorf("invalid alias: %w", err)
	}

	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	hub, err := loadHub(store, hubName)
	if err != nil {
		return err
	}
	record := hub.Member(member)
	if record == nil {
		return fmt.Errorf("member %q not found on hub %q; sign it first with 'portal hub sign' or 'portal hub invite'", member, hub.Name)
	}
	if record.Evicted {
		return fmt.Errorf("member %q is evicted", member)
	}
	for _, r := range hub.Routes {
		if r.AliasService == alias {
			return fmt.Errorf("alias %q already exists (routes to %s/%s)", alias, r.Member, r.Service)
		}
	}

	hub.Routes = append(hub.Routes, state.RouteEntry{
		Member:       member,
		Service:      service,
		AliasService: alias,
	})
	if dryRun {
		cfg, err := hubDeployConfig(hub)
		if err != nil {
			return err
		}
		resources, err := manifest.RenderHubManifests(cfg)
		if err != nil {
			return err
		}
		printResources(cmd.OutOrStdout(), resources)
		return nil
	}
	if err := applyHub(context.Background(), hub); err != nil {
		return err
	}
	if err := store.UpdateHub(*hub); err != nil {
		return fmt.Errorf("failed to save hub state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Routed %s/%s as %s\n", member, service, alias)
	fmt.Fprintf(out, "  Hub-side apps call: http://%s.%s/\n", alias, hub.Namespace)
	fmt.Fprintf(out, "  Note: the member must have published %q for requests to succeed\n", service)
	return nil
}
