package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/state"
	"github.com/johnlanda/portal/internal/validate"
)

// exposeOpts holds all flags for the expose command.
type exposeOpts struct {
	port             int
	serviceNamespace string
	tunnel           string
	localPort        int
	sni              string
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
The initiator config is also updated to add a new listener for the service.

Each call is additive — new services are added alongside existing ones.

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
	cmd.Flags().IntVar(&opts.localPort, "local-port", 0, "Initiator listener port (default: same as --port)")
	cmd.Flags().StringVar(&opts.sni, "sni", "", "Custom SNI value for routing (default: service name)")

	return cmd
}

func runExpose(cmd *cobra.Command, kubeContext, serviceName string, opts exposeOpts) error {
	// 0. Validate input names.
	if err := validate.Name(kubeContext); err != nil {
		return fmt.Errorf("invalid kube context: %w", err)
	}
	if err := validate.Name(serviceName); err != nil {
		return fmt.Errorf("invalid service name: %w", err)
	}
	if opts.sni != "" {
		if err := validate.DNSName(opts.sni); err != nil {
			return fmt.Errorf("invalid SNI: %w", err)
		}
	}

	// 1. Fail fast if kubectl is missing.
	if err := checkKubectlFn(); err != nil {
		return fmt.Errorf("prerequisite check failed: %w", err)
	}

	// 2. Validate kube context exists.
	if err := checkContextFn(kubeContext); err != nil {
		return fmt.Errorf("context validation failed: %w", err)
	}

	// 3. Load state, find tunnel where kubeContext is source or destination.
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	tunnel, role, err := findTunnelForContext(store, kubeContext, opts.tunnel)
	if err != nil {
		return fmt.Errorf("failed to find tunnel for context %q: %w", kubeContext, err)
	}

	// 3. Check if service:port already exposed (check both legacy and new entries).
	entry := fmt.Sprintf("%s:%d", serviceName, opts.port)
	for _, svc := range tunnel.Services {
		if svc == entry {
			return fmt.Errorf("service %q is already exposed on tunnel %s", entry, tunnel.Name)
		}
	}
	for _, se := range tunnel.ServiceEntries {
		if se.Name == serviceName && se.Port == opts.port {
			return fmt.Errorf("service %q is already exposed on tunnel %s", entry, tunnel.Name)
		}
	}

	ctx := context.Background()

	// 4. Verify the service exists in the originating cluster.
	originClient := newKubeClient(kubeContext, opts.serviceNamespace)
	if _, err := originClient.GetService(ctx, serviceName); err != nil {
		return fmt.Errorf("service %q not found in context %q namespace %q: %w", serviceName, kubeContext, opts.serviceNamespace, err)
	}

	// Resolve defaults for the new service entry.
	sni := opts.sni
	if sni == "" {
		sni = serviceName
	}
	localPort := opts.localPort
	if localPort == 0 {
		localPort = opts.port
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

	// 6. Build ClusterIP Service YAML. For multi-service, targetPort is the localPort.
	svcYAML, err := buildExposeService(kubeContext, tunnel.Namespace, component, serviceName, opts.port, localPort)
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
		// Build the new service entry.
		newEntry := state.ServiceEntry{
			Name:      serviceName,
			Namespace: opts.serviceNamespace,
			Port:      opts.port,
			LocalPort: localPort,
			SNI:       sni,
			Direction: "destination",
		}

		// Gather all existing service entries and append the new one.
		allEntries := tunnel.AllServiceEntries()
		allEntries = append(allEntries, newEntry)

		// Update both responder and initiator configs additively.
		backendHost := fmt.Sprintf("%s.%s.svc", serviceName, opts.serviceNamespace)
		if err := updateTunnelConfigs(ctx, tunnel, allEntries, backendHost, opts.port); err != nil {
			return fmt.Errorf("failed to update tunnel config: %w", err)
		}
		fmt.Fprintf(out, "\u2713 Updated responder Envoy config to route to %s:%d\n", backendHost, opts.port)
	} else {
		// Reverse direction: source service accessible from destination.
		// ClusterIP created, but routing requires reverse tunneling (Phase 2).
		fmt.Fprintln(out, "Note: routing from destination to source requires reverse tunneling (Phase 2)")
		fmt.Fprintln(out, "The ClusterIP Service was created for service discovery, but traffic")
		fmt.Fprintln(out, "will not be routed until reverse tunneling is configured.")
	}

	// 8. Update state with the new service entry (both legacy and new fields).
	sf, err := store.Load()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to load tunnel state for update: %v\n", err)
	} else {
		for i := range sf.Tunnels {
			if sf.Tunnels[i].Name == tunnel.Name {
				sf.Tunnels[i].Services = append(sf.Tunnels[i].Services, entry)
				sf.Tunnels[i].ServiceEntries = append(sf.Tunnels[i].ServiceEntries, state.ServiceEntry{
					Name:      serviceName,
					Namespace: opts.serviceNamespace,
					Port:      opts.port,
					LocalPort: localPort,
					SNI:       sni,
					Direction: role,
				})
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

// updateTunnelConfigs re-renders both the responder and initiator bootstrap configs
// with all service entries, applies the updated ConfigMaps, and restarts both deployments.
func updateTunnelConfigs(ctx context.Context, tunnel *state.TunnelState, entries []state.ServiceEntry, _ string, _ int) error {
	// Build responder multi-service config.
	var routes []envoy.ServiceRoute
	for _, e := range entries {
		backendHost := fmt.Sprintf("%s.%s.svc", e.Name, e.Namespace)
		if e.Namespace == "" {
			backendHost = fmt.Sprintf("%s.default.svc", e.Name)
		}
		routes = append(routes, envoy.ServiceRoute{
			SNI:         e.SNI,
			BackendHost: backendHost,
			BackendPort: e.Port,
		})
	}

	responderBootstrap, err := envoy.RenderResponderMultiBootstrap(envoy.ResponderMultiServiceConfig{
		ListenPort: tunnel.TunnelPort,
		Services:   routes,
	})
	if err != nil {
		return fmt.Errorf("failed to render responder bootstrap: %w", err)
	}

	responderCM, err := buildBootstrapConfigMap("portal-responder-bootstrap", tunnel.Namespace, responderBootstrap)
	if err != nil {
		return fmt.Errorf("failed to build responder ConfigMap: %w", err)
	}

	// Apply responder ConfigMap and restart.
	destClient := newKubeClient(tunnel.DestinationContext, tunnel.Namespace)
	if err := destClient.Apply(ctx, [][]byte{responderCM}); err != nil {
		return fmt.Errorf("failed to apply updated responder ConfigMap: %w", err)
	}
	if err := destClient.RolloutRestart(ctx, "portal-responder"); err != nil {
		return fmt.Errorf("failed to restart responder: %w", err)
	}

	// Build initiator multi-service config.
	var listeners []envoy.ServiceListener
	for _, e := range entries {
		lp := e.LocalPort
		if lp == 0 {
			lp = e.Port
		}
		listeners = append(listeners, envoy.ServiceListener{
			Name:       e.Name,
			ListenPort: lp,
			SNI:        e.SNI,
		})
	}

	initiatorBootstrap, err := envoy.RenderInitiatorMultiBootstrap(envoy.InitiatorMultiServiceConfig{
		ResponderHost: "portal-responder." + tunnel.Namespace + ".svc",
		ResponderPort: tunnel.TunnelPort,
		Services:      listeners,
	})
	if err != nil {
		return fmt.Errorf("failed to render initiator bootstrap: %w", err)
	}

	initiatorCM, err := buildBootstrapConfigMap("portal-initiator-bootstrap", tunnel.Namespace, initiatorBootstrap)
	if err != nil {
		return fmt.Errorf("failed to build initiator ConfigMap: %w", err)
	}

	// Apply initiator ConfigMap and restart.
	sourceClient := newKubeClient(tunnel.SourceContext, tunnel.Namespace)
	if err := sourceClient.Apply(ctx, [][]byte{initiatorCM}); err != nil {
		return fmt.Errorf("failed to apply updated initiator ConfigMap: %w", err)
	}
	if err := sourceClient.RolloutRestart(ctx, "portal-initiator"); err != nil {
		return fmt.Errorf("failed to restart initiator: %w", err)
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
func buildExposeService(ctxName, namespace, component, serviceName string, servicePort, targetPort int) ([]byte, error) {
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      fmt.Sprintf("portal-%s-%s", ctxName, serviceName),
			"namespace": namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by":  "portal",
				"app.kubernetes.io/component":   "exposed-service",
				"portal.johnlanda.io/service": serviceName,
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
					"targetPort": targetPort,
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
