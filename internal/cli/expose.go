package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tetratelabs/portal/internal/envoy"
	"github.com/tetratelabs/portal/internal/state"
)

// exposeOpts holds all flags for the expose command.
type exposeOpts struct {
	port             int
	serviceNamespace string
	tunnel           string
}

// NewExposeCmd creates the `portal expose` command.
func NewExposeCmd() *cobra.Command {
	var opts exposeOpts

	cmd := &cobra.Command{
		Use:   "expose <context> <service> --port <port>",
		Short: "Expose a service through a tunnel",
		Long: `Make a service in one cluster reachable from the other cluster through an existing tunnel.

Creates a ClusterIP Service in the opposite cluster that targets the local Envoy
proxy pod and updates the Envoy configuration to route traffic to the service.

When exposing a destination service (natural tunnel direction), the responder's
Envoy config is updated and the pod is restarted to apply the new route.

When exposing a source service (reverse direction), only the ClusterIP Service is
created. Actual traffic routing for this direction requires reverse tunneling (Phase 2).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExpose(cmd, args[0], args[1], opts)
		},
	}

	cmd.Flags().IntVar(&opts.port, "port", 0, "Port the service listens on (required)")
	_ = cmd.MarkFlagRequired("port")
	cmd.Flags().StringVar(&opts.serviceNamespace, "service-namespace", "default", "Namespace of the service being exposed")
	cmd.Flags().StringVar(&opts.tunnel, "tunnel", "", "Tunnel name to use (required when context matches multiple tunnels)")

	return cmd
}

func runExpose(cmd *cobra.Command, kubeContext, serviceName string, opts exposeOpts) error {
	// 1. Fail fast if kubectl is missing.
	if err := checkKubectlFn(); err != nil {
		return fmt.Errorf("prerequisite check failed: %w", err)
	}

	// 2. Validate kube context exists.
	if err := checkContextFn(kubeContext); err != nil {
		return err
	}

	// 3. Load state, find tunnel where kubeContext is source or destination.
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	tunnel, role, err := findTunnelForContext(store, kubeContext, opts.tunnel)
	if err != nil {
		return err
	}

	// 3. Check if service:port already exposed.
	entry := fmt.Sprintf("%s:%d", serviceName, opts.port)
	for _, svc := range tunnel.Services {
		if svc == entry {
			return fmt.Errorf("service %q is already exposed on tunnel %s", entry, tunnel.Name)
		}
	}

	ctx := context.Background()

	// 4. Verify the service exists in the originating cluster.
	originClient := newKubeClient(kubeContext, opts.serviceNamespace)
	if _, err := originClient.GetService(ctx, serviceName); err != nil {
		return fmt.Errorf("service %q not found in context %q namespace %q: %w", serviceName, kubeContext, opts.serviceNamespace, err)
	}

	// 5. Determine direction: expose in the opposite cluster.
	var targetContext, component string
	if role == "source" {
		// Service lives in source → create service in destination targeting responder.
		targetContext = tunnel.DestinationContext
		component = "portal-responder"
	} else {
		// Service lives in destination → create service in source targeting initiator.
		targetContext = tunnel.SourceContext
		component = "portal-initiator"
	}

	// 6. Build ClusterIP Service YAML.
	svcYAML, err := buildExposeService(kubeContext, tunnel.Namespace, component, serviceName, opts.port, tunnel.TunnelPort)
	if err != nil {
		return fmt.Errorf("failed to build service manifest: %w", err)
	}

	// 7. Apply ClusterIP in the target cluster.
	targetClient := newKubeClient(targetContext, tunnel.Namespace)
	if err := targetClient.Apply(ctx, [][]byte{svcYAML}); err != nil {
		return fmt.Errorf("failed to apply exposed service in %s: %w", targetContext, err)
	}

	out := cmd.OutOrStdout()

	// 7. Update Envoy config if this is the natural tunnel direction.
	if role == "destination" {
		// Natural direction: destination service accessible from source.
		// Update the responder's backend to route to the actual service.
		backendHost := fmt.Sprintf("%s.%s.svc", serviceName, opts.serviceNamespace)
		if err := updateResponderConfig(ctx, tunnel, backendHost, opts.port); err != nil {
			return fmt.Errorf("failed to update responder config: %w", err)
		}
		fmt.Fprintf(out, "\u2713 Updated responder Envoy config to route to %s:%d\n", backendHost, opts.port)
	} else {
		// Reverse direction: source service accessible from destination.
		// ClusterIP created, but routing requires reverse tunneling (Phase 2).
		fmt.Fprintln(out, "Note: routing from destination to source requires reverse tunneling (Phase 2)")
		fmt.Fprintln(out, "The ClusterIP Service was created for service discovery, but traffic")
		fmt.Fprintln(out, "will not be routed until reverse tunneling is configured.")
	}

	// 8. Update state with the new service entry.
	sf, err := store.Load()
	if err == nil {
		for i := range sf.Tunnels {
			if sf.Tunnels[i].Name == tunnel.Name {
				sf.Tunnels[i].Services = append(sf.Tunnels[i].Services, entry)
				break
			}
		}
		if saveErr := store.Save(sf); saveErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to save tunnel state: %v\n", saveErr)
		}
	}

	// 9. Print success.
	exposedName := fmt.Sprintf("portal-%s-%s", kubeContext, serviceName)
	fmt.Fprintf(out, "\u2713 Exposed %s through tunnel %s\n", serviceName, tunnel.Name)
	fmt.Fprintf(out, "  Service created: %s in %s\n", exposedName, targetContext)
	fmt.Fprintf(out, "  Reachable at:    %s.%s.svc:%d\n", exposedName, tunnel.Namespace, opts.port)

	return nil
}

// updateResponderConfig re-renders the responder bootstrap with the new backend
// service, applies the updated ConfigMap, and restarts the responder deployment.
func updateResponderConfig(ctx context.Context, tunnel *state.TunnelState, backendHost string, backendPort int) error {
	// Re-render responder bootstrap with actual backend.
	bootstrap, err := envoy.RenderResponderBootstrap(envoy.ResponderConfig{
		ListenPort:  tunnel.TunnelPort,
		BackendHost: backendHost,
		BackendPort: backendPort,
	})
	if err != nil {
		return fmt.Errorf("failed to render responder bootstrap: %w", err)
	}

	// Build ConfigMap YAML.
	cmYAML, err := buildBootstrapConfigMap("portal-responder-bootstrap", tunnel.Namespace, bootstrap)
	if err != nil {
		return fmt.Errorf("failed to build ConfigMap: %w", err)
	}

	// Apply to destination cluster.
	destClient := newKubeClient(tunnel.DestinationContext, tunnel.Namespace)
	if err := destClient.Apply(ctx, [][]byte{cmYAML}); err != nil {
		return fmt.Errorf("failed to apply updated ConfigMap: %w", err)
	}

	// Rollout restart the responder to pick up the new config.
	if err := destClient.RolloutRestart(ctx, "portal-responder"); err != nil {
		return fmt.Errorf("failed to restart responder: %w", err)
	}

	return nil
}

// findTunnelForContext scans all tunnels and returns one where kubeContext
// matches either the source or destination. If tunnelName is non-empty, only
// that specific tunnel is considered. If the context matches multiple tunnels
// and no tunnelName filter is given, an error is returned.
func findTunnelForContext(store *state.Store, kubeContext, tunnelName string) (*state.TunnelState, string, error) {
	tunnels, err := store.List()
	if err != nil {
		return nil, "", fmt.Errorf("failed to list tunnels: %w", err)
	}

	type match struct {
		tunnel *state.TunnelState
		role   string
	}
	var matches []match

	for i := range tunnels {
		if tunnelName != "" && tunnels[i].Name != tunnelName {
			continue
		}
		if tunnels[i].SourceContext == kubeContext {
			matches = append(matches, match{&tunnels[i], "source"})
		} else if tunnels[i].DestinationContext == kubeContext {
			matches = append(matches, match{&tunnels[i], "destination"})
		}
	}

	switch len(matches) {
	case 0:
		return nil, "", fmt.Errorf("no tunnel found for context %q; create one with 'portal connect' first", kubeContext)
	case 1:
		return matches[0].tunnel, matches[0].role, nil
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.tunnel.Name
		}
		return nil, "", fmt.Errorf("context %q matches multiple tunnels (%s); use --tunnel to specify which one", kubeContext, joinNames(names))
	}
}

func joinNames(names []string) string {
	result := ""
	for i, n := range names {
		if i > 0 {
			result += ", "
		}
		result += n
	}
	return result
}

// buildExposeService builds a ClusterIP Service YAML that routes to the Envoy proxy pod.
func buildExposeService(ctxName, namespace, component, serviceName string, servicePort, tunnelPort int) ([]byte, error) {
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      fmt.Sprintf("portal-%s-%s", ctxName, serviceName),
			"namespace": namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by":  "portal",
				"app.kubernetes.io/component":   "exposed-service",
				"portal.tetratelabs.io/service": serviceName,
			},
		},
		"spec": map[string]interface{}{
			"type": "ClusterIP",
			"selector": map[string]interface{}{
				"app.kubernetes.io/name": component,
			},
			"ports": []interface{}{
				map[string]interface{}{
					"name":       serviceName,
					"port":       servicePort,
					"targetPort": tunnelPort,
					"protocol":   "TCP",
				},
			},
		},
	}

	data, err := yaml.Marshal(svc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal service YAML: %w", err)
	}
	return data, nil
}

// buildBootstrapConfigMap builds a ConfigMap YAML containing the Envoy bootstrap config.
func buildBootstrapConfigMap(name, namespace string, bootstrap []byte) ([]byte, error) {
	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"data": map[string]interface{}{
			"envoy.yaml": string(bootstrap),
		},
	}
	data, err := yaml.Marshal(cm)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ConfigMap YAML: %w", err)
	}
	return data, nil
}
