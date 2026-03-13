package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/kube"
	"github.com/johnlanda/portal/internal/manifest"
	"github.com/johnlanda/portal/internal/state"
	"github.com/johnlanda/portal/internal/validate"
)

// sentinelEndpoint is used during two-phase render when the real LB address is unknown.
const sentinelEndpoint = "pending-discovery.portal.local"

// Testability hooks — tests swap these and restore via t.Cleanup.
var (
	newKubeClient  = func(kubeContext, namespace string) kube.Client { return kube.NewClient(kubeContext, namespace) }
	checkKubectlFn = kube.CheckKubectl
	checkContextFn = kube.CheckContext
	newStateStore  = func() (*state.Store, error) {
		p, err := state.DefaultPath()
		if err != nil {
			return nil, err
		}
		return state.NewStore(p), nil
	}
)

// connectOpts holds all flags for the connect command.
type connectOpts struct {
	responderEndpoint string
	namespace         string
	tunnelPort        int
	connectionCount   int
	certValidity      time.Duration
	certDir           string
	initiatorCertDir  string
	responderCertDir  string
	certManager       bool
	secretRef         string
	envoyImage        string
	envoyLogLevel     string
	serviceType       string
	deployTimeout     time.Duration
	lbTimeout         time.Duration
	dryRun            bool
	serviceFlags      []string
	serviceLocalPorts []string
}

// NewConnectCmd creates the `portal connect` command.
func NewConnectCmd() *cobra.Command {
	var opts connectOpts

	cmd := &cobra.Command{
		Use:   "connect <source_context> <destination_context>",
		Short: "Create a tunnel by deploying to both clusters",
		Long: `Deploy a Portal tunnel to two Kubernetes clusters.

Creates and applies the complete set of manifests for both the source (initiator)
and destination (responder) clusters, waits for deployments to become ready,
and persists tunnel state to ~/.portal/tunnels.json.

If --responder-endpoint is omitted, the command will deploy the responder first,
discover the LoadBalancer address, then re-render manifests with the real endpoint
before deploying the initiator.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(cmd, args[0], args[1], opts)
		},
	}

	cmd.Flags().StringVar(&opts.responderEndpoint, "responder-endpoint", "", "Responder address (IP:port or hostname:port); discovered from LB if omitted")
	cmd.Flags().StringVar(&opts.namespace, "namespace", manifest.DefaultNamespace, "Namespace for portal components")
	cmd.Flags().IntVar(&opts.tunnelPort, "tunnel-port", manifest.DefaultTunnelPort, "Responder listen port")
	cmd.Flags().IntVar(&opts.connectionCount, "connection-count", manifest.DefaultConnectionCount, "Number of reverse connections to maintain")
	cmd.Flags().DurationVar(&opts.certValidity, "cert-validity", 8760*time.Hour, "Certificate validity duration")
	cmd.Flags().StringVar(&opts.certDir, "cert-dir", "", "Use existing certificates instead of generating")
	cmd.Flags().StringVar(&opts.initiatorCertDir, "initiator-cert-dir", "", "Directory with initiator certs (tls.crt, tls.key, ca.crt)")
	cmd.Flags().StringVar(&opts.responderCertDir, "responder-cert-dir", "", "Directory with responder certs (tls.crt, tls.key, ca.crt)")
	cmd.Flags().BoolVar(&opts.certManager, "cert-manager", false, "Use cert-manager CRDs for certificate management")
	cmd.Flags().StringVar(&opts.secretRef, "secret-ref", "", "Reference an existing K8s Secret for TLS certificates (skip cert generation)")
	cmd.Flags().StringVar(&opts.envoyImage, "envoy-image", manifest.DefaultEnvoyImage, "Envoy proxy image")
	cmd.Flags().StringVar(&opts.envoyLogLevel, "envoy-log-level", manifest.DefaultEnvoyLogLevel, "Envoy log level")
	cmd.Flags().StringVar(&opts.serviceType, "service-type", manifest.DefaultServiceType, "Responder Service type (LoadBalancer, NodePort, ClusterIP)")
	cmd.Flags().DurationVar(&opts.deployTimeout, "deploy-timeout", 5*time.Minute, "Timeout waiting for deployment readiness")
	cmd.Flags().DurationVar(&opts.lbTimeout, "lb-timeout", 5*time.Minute, "Timeout waiting for LoadBalancer address")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print rendered manifests to stdout without applying")
	cmd.Flags().StringArrayVar(&opts.serviceFlags, "service", nil, "Service to route through the tunnel (format: sni=host:port); can be repeated")
	cmd.Flags().StringArrayVar(&opts.serviceLocalPorts, "service-local-port", nil, "Override initiator listener port for a service (format: sni=port); can be repeated")

	return cmd
}

func runConnect(cmd *cobra.Command, sourceCtx, destCtx string, opts connectOpts) error {
	// 0. Validate input names.
	if err := validate.Name(sourceCtx); err != nil {
		return fmt.Errorf("invalid source context: %w", err)
	}
	if err := validate.Name(destCtx); err != nil {
		return fmt.Errorf("invalid destination context: %w", err)
	}

	// 1. Fail fast if kubectl is missing.
	if err := checkKubectlFn(); err != nil {
		return fmt.Errorf("prerequisite check failed: %w", err)
	}

	// 2. Validate kube contexts exist.
	if err := checkContextFn(sourceCtx); err != nil {
		return fmt.Errorf("source context validation failed: %w", err)
	}
	if err := checkContextFn(destCtx); err != nil {
		return fmt.Errorf("destination context validation failed: %w", err)
	}

	// Parse service flags.
	services, err := parseServiceFlags(opts.serviceFlags, opts.serviceLocalPorts)
	if err != nil {
		return fmt.Errorf("invalid service flags: %w", err)
	}

	// Derive tunnel name.
	tunnelName := sourceCtx + "--" + destCtx

	// 2. Check for duplicate tunnel in state.
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	existing, err := store.Get(tunnelName)
	if err != nil {
		return fmt.Errorf("failed to check tunnel state: %w", err)
	}
	if existing != nil && !opts.dryRun {
		return fmt.Errorf("tunnel %q already exists; use 'portal disconnect' first", tunnelName)
	}

	// Dry-run mode: render and print manifests without applying.
	if opts.dryRun {
		if opts.responderEndpoint == "" {
			return fmt.Errorf("--responder-endpoint is required for --dry-run (cannot discover LB address)")
		}
		bundle, err := renderBundle(sourceCtx, destCtx, opts.responderEndpoint, opts, services)
		if err != nil {
			return fmt.Errorf("dry-run render failed: %w", err)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "# Source (initiator) cluster resources")
		for _, r := range bundle.Source {
			fmt.Fprintf(out, "---\n%s", r.Content)
		}
		fmt.Fprintln(out, "# Destination (responder) cluster resources")
		for _, r := range bundle.Destination {
			fmt.Fprintf(out, "---\n%s", r.Content)
		}
		return nil
	}

	// 3. Create kube clients.
	destClient := newKubeClient(destCtx, opts.namespace)
	sourceClient := newKubeClient(sourceCtx, opts.namespace)

	ctx := context.Background()

	var bundle *manifest.ManifestBundle

	if opts.responderEndpoint != "" {
		// Single-phase: endpoint is known.
		bundle, err = renderBundle(sourceCtx, destCtx, opts.responderEndpoint, opts, services)
		if err != nil {
			return fmt.Errorf("failed to render manifests: %w", err)
		}

		// Apply destination resources.
		if err := destClient.Apply(ctx, extractContents(bundle.Destination)); err != nil {
			return fmt.Errorf("failed to apply destination resources: %w", err)
		}

		// Apply source resources.
		if err := sourceClient.Apply(ctx, extractContents(bundle.Source)); err != nil {
			return fmt.Errorf("failed to apply source resources: %w", err)
		}
	} else {
		// Two-phase: deploy responder first, discover LB, re-render with real address.
		sentinelAddr := fmt.Sprintf("%s:%d", sentinelEndpoint, opts.tunnelPort)
		phase1Bundle, err := renderBundle(sourceCtx, destCtx, sentinelAddr, opts, services)
		if err != nil {
			return fmt.Errorf("failed to render phase-1 manifests: %w", err)
		}

		// Phase 1: deploy destination (responder only listens).
		if err := destClient.Apply(ctx, extractContents(phase1Bundle.Destination)); err != nil {
			return fmt.Errorf("failed to apply destination resources (phase 1): %w", err)
		}

		// Discover LB address.
		address, err := destClient.WaitForServiceAddress(ctx, "portal-responder", opts.lbTimeout)
		if err != nil {
			return fmt.Errorf("failed to discover responder LoadBalancer address: %w", err)
		}

		// Phase 2: re-render with real endpoint.
		realEndpoint := fmt.Sprintf("%s:%d", address, opts.tunnelPort)
		bundle, err = renderBundle(sourceCtx, destCtx, realEndpoint, opts, services)
		if err != nil {
			return fmt.Errorf("failed to render phase-2 manifests: %w", err)
		}

		// Update destination with correct certs and service config.
		if err := destClient.Apply(ctx, extractContents(bundle.Destination)); err != nil {
			return fmt.Errorf("failed to apply destination resources (phase 2): %w", err)
		}

		// Deploy source (initiator).
		if err := sourceClient.Apply(ctx, extractContents(bundle.Source)); err != nil {
			return fmt.Errorf("failed to apply source resources: %w", err)
		}
	}

	// 10. Wait for both deployments.
	if err := destClient.WaitForDeployment(ctx, "portal-responder", opts.deployTimeout); err != nil {
		return fmt.Errorf("responder deployment not ready: %w", err)
	}
	if err := sourceClient.WaitForDeployment(ctx, "portal-initiator", opts.deployTimeout); err != nil {
		return fmt.Errorf("initiator deployment not ready: %w", err)
	}

	// 11. Print success output.
	out := cmd.OutOrStdout()
	if opts.secretRef != "" {
		fmt.Fprintf(out, "\u2713 Using existing secret %q for TLS certificates\n", opts.secretRef)
	} else if opts.certManager {
		fmt.Fprintln(out, "\u2713 Generated tunnel manifests with cert-manager CRDs")
	} else {
		fmt.Fprintln(out, "\u2713 Generated tunnel CA and certificates")
	}
	fmt.Fprintf(out, "\u2713 Deployed responder Envoy in %s (namespace: %s)\n", destCtx, opts.namespace)
	fmt.Fprintf(out, "\u2713 Deployed initiator Envoy in %s (namespace: %s)\n", sourceCtx, opts.namespace)
	fmt.Fprintf(out, "\u2713 Tunnel established: %s \u2192 %s\n", sourceCtx, destCtx)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Tunnel name:  %s\n", tunnelName)
	fmt.Fprintf(out, "Tunnel port:  portal-tunnel.%s.svc:%d\n", opts.namespace, opts.tunnelPort)
	fmt.Fprintln(out, "Status:       Connected")

	// 12. Save tunnel state (non-fatal on failure).
	if saveErr := saveConnectState(store, bundle, sourceCtx, destCtx, opts); saveErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to save tunnel state: %v\n", saveErr)
	}

	// 13. Save CA material (non-fatal on failure).
	if bundle.Certs != nil {
		if saveErr := saveCACerts(tunnelName, bundle); saveErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to save CA certificates: %v\n", saveErr)
		}
	}

	return nil
}

// renderBundle builds a TunnelConfig and calls manifest.Render.
func renderBundle(sourceCtx, destCtx, endpoint string, opts connectOpts, services []manifest.ServiceConfig) (*manifest.ManifestBundle, error) {
	cfg := manifest.TunnelConfig{
		SourceContext:      sourceCtx,
		DestinationContext: destCtx,
		Namespace:          opts.namespace,
		ResponderEndpoint:  endpoint,
		TunnelPort:         opts.tunnelPort,
		ConnectionCount:    opts.connectionCount,
		CertValidity:       opts.certValidity,
		EnvoyImage:         opts.envoyImage,
		EnvoyLogLevel:      opts.envoyLogLevel,
		ServiceType:        opts.serviceType,
		CertDir:            opts.certDir,
		InitiatorCertDir:   opts.initiatorCertDir,
		ResponderCertDir:   opts.responderCertDir,
		CertManager:        opts.certManager,
		SecretRef:          opts.secretRef,
		Services:           services,
	}

	bundle, err := manifest.Render(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to render manifests: %w", err)
	}
	return bundle, nil
}

// extractContents maps []manifest.Resource → [][]byte for kube.Apply.
func extractContents(resources []manifest.Resource) [][]byte {
	yamls := make([][]byte, len(resources))
	for i, r := range resources {
		yamls[i] = r.Content
	}
	return yamls
}

// saveConnectState persists tunnel metadata to the state store.
func saveConnectState(store *state.Store, bundle *manifest.ManifestBundle, sourceCtx, destCtx string, opts connectOpts) error {
	tunnelName := sourceCtx + "--" + destCtx

	var caCertPath string
	if bundle.Certs != nil {
		dir, err := state.DefaultDir()
		if err != nil {
			return fmt.Errorf("failed to determine state directory for CA cert path: %w", err)
		}
		caCertPath = filepath.Join(dir, "certs", tunnelName, "ca.crt")
	}

	ts := state.TunnelState{
		Name:               tunnelName,
		SourceContext:      sourceCtx,
		DestinationContext: destCtx,
		Namespace:          opts.namespace,
		TunnelPort:         opts.tunnelPort,
		CreatedAt:          time.Now().UTC(),
		CACertPath:         caCertPath,
		Mode:               "imperative",
	}

	// Store service entries if services were declared at connect time.
	if bundle.Metadata.Services != nil {
		for _, svc := range bundle.Metadata.Services {
			lp := svc.LocalPort
			if lp == 0 {
				lp = svc.BackendPort
			}
			ts.Services = append(ts.Services, fmt.Sprintf("%s:%d", svc.SNI, svc.BackendPort))
			ts.ServiceEntries = append(ts.ServiceEntries, state.ServiceEntry{
				Name:      svc.SNI,
				Namespace: "", // namespace is embedded in BackendHost for connect-time services
				Port:      svc.BackendPort,
				LocalPort: lp,
				SNI:       svc.SNI,
				Direction: "destination",
			})
		}
	}

	if err := store.Add(ts); err != nil {
		return fmt.Errorf("failed to save tunnel state: %w", err)
	}
	return nil
}

// saveCACerts writes CA certificate material to ~/.portal/certs/<tunnel-name>/.
func saveCACerts(tunnelName string, bundle *manifest.ManifestBundle) error {
	dir, err := state.DefaultDir()
	if err != nil {
		return fmt.Errorf("failed to determine state directory: %w", err)
	}
	certDir := filepath.Join(dir, "certs", tunnelName)
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return fmt.Errorf("failed to create cert directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), bundle.Certs.CACert, 0600); err != nil {
		return fmt.Errorf("failed to write CA certificate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.key"), bundle.Certs.CAKey, 0600); err != nil {
		return fmt.Errorf("failed to write CA key: %w", err)
	}
	return nil
}
