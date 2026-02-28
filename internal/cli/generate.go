// Package cli implements the portal CLI commands.
//
// Each public NewXxxCmd function returns a [cobra.Command] ready to be
// attached to the root command. Commands follow a consistent pattern:
//
//  1. Validate prerequisites (kubectl on PATH, kube contexts exist).
//  2. Render Kubernetes manifests via [manifest.Render].
//  3. Apply/delete manifests via [kube.Client], or write to disk.
//  4. Update local tunnel state in ~/.portal/tunnels.json.
//
// Testability hooks (newKubeClient, checkKubectlFn, etc.) allow tests to
// substitute stubs for external dependencies without a live cluster.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/tetratelabs/portal/internal/envoy"
	"github.com/tetratelabs/portal/internal/manifest"
)

// NewGenerateCmd creates the `portal generate` command.
func NewGenerateCmd() *cobra.Command {
	var (
		outputDir         string
		namespace         string
		tunnelPort        int
		connectionCount   int
		certValidity      time.Duration
		certDir           string
		certManager       bool
		envoyImage        string
		envoyLogLevel     string
		responderEndpoint string
		serviceType       string
	)

	cmd := &cobra.Command{
		Use:   "generate <source_context> <destination_context>",
		Short: "Generate tunnel manifests to disk for GitOps workflows",
		Long: `Generate Kubernetes manifests for a Portal tunnel without applying them.

Produces a complete set of manifests for both the source (initiator) and
destination (responder) clusters, structured for use with Kustomize, Argo CD,
Flux, or any other GitOps controller.

The --responder-endpoint flag is required. Pass either:
  - An IP address (e.g., 34.120.1.50:10443) to set loadBalancerIP on the Service
  - A DNS hostname (e.g., tunnel.example.com:10443) to add an external-dns annotation`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sourceContext := args[0]
			destinationContext := args[1]

			if outputDir == "" {
				return fmt.Errorf("--output-dir is required")
			}
			if responderEndpoint == "" {
				return fmt.Errorf("--responder-endpoint is required for generate")
			}

			cfg := manifest.TunnelConfig{
				SourceContext:      sourceContext,
				DestinationContext: destinationContext,
				Namespace:          namespace,
				ResponderEndpoint:  responderEndpoint,
				TunnelPort:         tunnelPort,
				ConnectionCount:    connectionCount,
				CertValidity:       certValidity,
				EnvoyImage:         envoyImage,
				EnvoyLogLevel:      envoyLogLevel,
				ServiceType:        serviceType,
				CertDir:            certDir,
				CertManager:        certManager,
			}

			bundle, err := manifest.Render(cfg)
			if err != nil {
				return fmt.Errorf("failed to render manifests: %w", err)
			}

			if err := manifest.WriteToDisk(bundle, outputDir); err != nil {
				return fmt.Errorf("failed to write manifests: %w", err)
			}

			// Print summary.
			if certManager {
				fmt.Fprintf(cmd.OutOrStdout(), "Generated tunnel manifests with cert-manager CRDs\n")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Generated tunnel CA and certificates\n")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote source cluster manifests      → %s/source/\n", outputDir)
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote destination cluster manifests → %s/destination/\n", outputDir)
			fmt.Fprintf(cmd.OutOrStdout(), "\nTunnel name: %s\n", bundle.Metadata.TunnelName)
			fmt.Fprintf(cmd.OutOrStdout(), "Namespace:   %s\n", bundle.Metadata.Namespace)
			fmt.Fprintf(cmd.OutOrStdout(), "\nNext steps:\n")
			fmt.Fprintf(cmd.OutOrStdout(), "  kubectl apply -k %s/destination/ --context %s\n", outputDir, destinationContext)
			fmt.Fprintf(cmd.OutOrStdout(), "  kubectl apply -k %s/source/ --context %s\n", outputDir, sourceContext)

			return nil
		},
	}

	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory to write manifests (required)")
	cmd.Flags().StringVar(&responderEndpoint, "responder-endpoint", "", "Responder address (IP:port or hostname:port, required)")
	cmd.Flags().StringVar(&namespace, "namespace", manifest.DefaultNamespace, "Namespace for portal components")
	cmd.Flags().IntVar(&tunnelPort, "tunnel-port", manifest.DefaultTunnelPort, "Responder listen port")
	cmd.Flags().IntVar(&connectionCount, "connection-count", manifest.DefaultConnectionCount, "Number of reverse connections to maintain")
	cmd.Flags().DurationVar(&certValidity, "cert-validity", 8760*time.Hour, "Certificate validity duration")
	cmd.Flags().StringVar(&certDir, "cert-dir", "", "Use existing certificates instead of generating")
	cmd.Flags().BoolVar(&certManager, "cert-manager", false, "Use cert-manager CRDs for certificate management instead of raw secrets")
	cmd.Flags().StringVar(&envoyImage, "envoy-image", manifest.DefaultEnvoyImage, "Envoy proxy image")
	cmd.Flags().StringVar(&envoyLogLevel, "envoy-log-level", manifest.DefaultEnvoyLogLevel, "Envoy log level")
	cmd.Flags().StringVar(&serviceType, "service-type", manifest.DefaultServiceType, "Responder Service type (LoadBalancer, NodePort, ClusterIP)")

	cmd.AddCommand(newGenerateExposeCmd())

	return cmd
}

// newGenerateExposeCmd creates the `portal generate expose` subcommand.
func newGenerateExposeCmd() *cobra.Command {
	var (
		outputDir        string
		serviceNamespace string
		tunnelPort       int
		namespace        string
	)

	cmd := &cobra.Command{
		Use:   "expose <context> <service> --port <port> --output-dir <dir>",
		Short: "Generate expose manifests to disk for GitOps workflows",
		Long: `Generate Kubernetes manifests for exposing a service through an existing tunnel.

Writes the ClusterIP Service and updated Envoy ConfigMap to the specified output
directory, ready for use with Kustomize, Argo CD, Flux, or kubectl apply.

The <context> must match the source or destination of a tunnel tracked in
~/.portal/tunnels.json.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kubeContext := args[0]
			serviceName := args[1]

			port, err := cmd.Flags().GetInt("port")
			if err != nil || port == 0 {
				return fmt.Errorf("--port is required")
			}
			if outputDir == "" {
				return fmt.Errorf("--output-dir is required")
			}

			return runGenerateExpose(cmd, kubeContext, serviceName, port, outputDir, serviceNamespace, tunnelPort, namespace)
		},
	}

	cmd.Flags().Int("port", 0, "Port the service listens on (required)")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory to write manifests (required)")
	cmd.Flags().StringVar(&serviceNamespace, "service-namespace", "default", "Namespace of the service being exposed")
	cmd.Flags().IntVar(&tunnelPort, "tunnel-port", 0, "Override tunnel port (default: read from tunnel state)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Override namespace (default: read from tunnel state)")

	return cmd
}

func runGenerateExpose(cmd *cobra.Command, kubeContext, serviceName string, port int, outputDir, serviceNamespace string, tunnelPortOverride int, nsOverride string) error {
	// Load state and find the tunnel.
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	tunnel, role, err := findTunnelForContext(store, kubeContext, "")
	if err != nil {
		return err
	}

	ns := tunnel.Namespace
	if nsOverride != "" {
		ns = nsOverride
	}
	tPort := tunnel.TunnelPort
	if tunnelPortOverride != 0 {
		tPort = tunnelPortOverride
	}

	// Determine direction.
	var targetContext, component string
	if role == "source" {
		targetContext = tunnel.DestinationContext
		component = "portal-responder"
	} else {
		targetContext = tunnel.SourceContext
		component = "portal-initiator"
	}

	// Build ClusterIP Service YAML.
	svcYAML, err := buildExposeService(kubeContext, ns, component, serviceName, port, tPort)
	if err != nil {
		return fmt.Errorf("failed to build service manifest: %w", err)
	}

	// Create output directory.
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Write ClusterIP Service.
	svcFile := filepath.Join(outputDir, fmt.Sprintf("portal-%s-%s-service.yaml", kubeContext, serviceName))
	if err := os.WriteFile(svcFile, svcYAML, 0644); err != nil {
		return fmt.Errorf("failed to write service manifest: %w", err)
	}

	out := cmd.OutOrStdout()

	// Build ConfigMap for natural direction.
	if role == "destination" {
		backendHost := fmt.Sprintf("%s.%s.svc", serviceName, serviceNamespace)
		bootstrap, err := envoy.RenderResponderBootstrap(envoy.ResponderConfig{
			ListenPort:  tPort,
			BackendHost: backendHost,
			BackendPort: port,
		})
		if err != nil {
			return fmt.Errorf("failed to render responder bootstrap: %w", err)
		}

		cmYAML, err := buildBootstrapConfigMap("portal-responder-bootstrap", ns, bootstrap)
		if err != nil {
			return fmt.Errorf("failed to build ConfigMap: %w", err)
		}

		cmFile := filepath.Join(outputDir, "portal-responder-bootstrap-cm.yaml")
		if err := os.WriteFile(cmFile, cmYAML, 0644); err != nil {
			return fmt.Errorf("failed to write ConfigMap manifest: %w", err)
		}
		fmt.Fprintf(out, "Wrote ConfigMap manifest → %s\n", cmFile)
	} else {
		fmt.Fprintln(out, "Note: routing from destination to source requires reverse tunneling (Phase 2)")
	}

	fmt.Fprintf(out, "Wrote service manifest  → %s\n", svcFile)
	fmt.Fprintf(out, "\nNext steps:\n")
	if role == "destination" {
		fmt.Fprintf(out, "  kubectl apply -f %s --context %s\n", filepath.Join(outputDir, "portal-responder-bootstrap-cm.yaml"), tunnel.DestinationContext)
		fmt.Fprintf(out, "  kubectl rollout restart deployment/portal-responder --context %s -n %s\n", tunnel.DestinationContext, ns)
	}
	fmt.Fprintf(out, "  kubectl apply -f %s --context %s\n", svcFile, targetContext)

	return nil
}
