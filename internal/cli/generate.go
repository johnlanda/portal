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

	"github.com/johnlanda/portal/internal/baremetal"
	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/manifest"
)

// k8sOnlyFlags lists flags that are only valid for Kubernetes targets.
var k8sOnlyFlags = []string{"namespace", "cert-manager", "secret-ref", "service-type", "connection-count", "envoy-image"}

// NewGenerateCmd creates the `portal generate` command.
func NewGenerateCmd() *cobra.Command {
	var (
		outputDir         string
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
		responderEndpoint string
		serviceType       string
		serviceFlags      []string
		serviceLocalPorts []string
		target            string

		// Bare-metal-specific flags.
		envoyCommand      string
		certInstallPath   string
		configInstallPath string
		runUser           string
	)

	cmd := &cobra.Command{
		Use:   "generate <source> <destination>",
		Short: "Generate tunnel manifests to disk for GitOps workflows",
		Long: `Generate deployment artifacts for a Portal tunnel without applying them.

For Kubernetes targets (default), produces K8s manifests structured for use
with Kustomize, Argo CD, Flux, or kubectl apply.

For bare-metal targets (--target bare-metal), produces raw Envoy configs,
systemd unit files, and docker-compose files for deployment to VMs or hosts.

The --responder-endpoint flag is required. Pass either:
  - An IP address (e.g., 34.120.1.50:10443) to set loadBalancerIP on the Service
  - A DNS hostname (e.g., tunnel.example.com:10443) to add an external-dns annotation`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if outputDir == "" {
				return fmt.Errorf("--output-dir is required")
			}
			if responderEndpoint == "" {
				return fmt.Errorf("--responder-endpoint is required for generate")
			}

			switch target {
			case "kubernetes":
				return runGenerateKubernetes(cmd, args, generateK8sOpts{
					outputDir:         outputDir,
					namespace:         namespace,
					tunnelPort:        tunnelPort,
					connectionCount:   connectionCount,
					certValidity:      certValidity,
					certDir:           certDir,
					initiatorCertDir:  initiatorCertDir,
					responderCertDir:  responderCertDir,
					certManager:       certManager,
					secretRef:         secretRef,
					envoyImage:        envoyImage,
					envoyLogLevel:     envoyLogLevel,
					responderEndpoint: responderEndpoint,
					serviceType:       serviceType,
					serviceFlags:      serviceFlags,
					serviceLocalPorts: serviceLocalPorts,
				})
			case "bare-metal":
				// Reject K8s-only flags when targeting bare metal.
				for _, name := range k8sOnlyFlags {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("--%s is not supported with --target bare-metal", name)
					}
				}
				return runGenerateBareMetal(cmd, args, generateBareMetalOpts{
					outputDir:         outputDir,
					tunnelPort:        tunnelPort,
					certValidity:      certValidity,
					certDir:           certDir,
					initiatorCertDir:  initiatorCertDir,
					responderCertDir:  responderCertDir,
					envoyLogLevel:     envoyLogLevel,
					responderEndpoint: responderEndpoint,
					serviceFlags:      serviceFlags,
					serviceLocalPorts: serviceLocalPorts,
					envoyCommand:      envoyCommand,
					certInstallPath:   certInstallPath,
					configInstallPath: configInstallPath,
					runUser:           runUser,
				})
			default:
				return fmt.Errorf("invalid --target %q: must be 'kubernetes' or 'bare-metal'", target)
			}
		},
	}

	// Common flags.
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory to write manifests (required)")
	cmd.Flags().StringVar(&responderEndpoint, "responder-endpoint", "", "Responder address (IP:port or hostname:port, required)")
	cmd.Flags().IntVar(&tunnelPort, "tunnel-port", manifest.DefaultTunnelPort, "Responder listen port")
	cmd.Flags().DurationVar(&certValidity, "cert-validity", 8760*time.Hour, "Certificate validity duration")
	cmd.Flags().StringVar(&certDir, "cert-dir", "", "Use existing certificates instead of generating")
	cmd.Flags().StringVar(&initiatorCertDir, "initiator-cert-dir", "", "Directory with initiator certs (tls.crt, tls.key, ca.crt)")
	cmd.Flags().StringVar(&responderCertDir, "responder-cert-dir", "", "Directory with responder certs (tls.crt, tls.key, ca.crt)")
	cmd.Flags().StringVar(&envoyLogLevel, "envoy-log-level", manifest.DefaultEnvoyLogLevel, "Envoy log level")
	cmd.Flags().StringArrayVar(&serviceFlags, "service", nil, "Service to route through the tunnel (format: sni=host:port); can be repeated")
	cmd.Flags().StringArrayVar(&serviceLocalPorts, "service-local-port", nil, "Override initiator listener port for a service (format: sni=port); can be repeated")
	cmd.Flags().StringVar(&target, "target", "kubernetes", "Deploy target: 'kubernetes' or 'bare-metal'")

	// Kubernetes-only flags.
	cmd.Flags().StringVar(&namespace, "namespace", manifest.DefaultNamespace, "Namespace for portal components")
	cmd.Flags().IntVar(&connectionCount, "connection-count", manifest.DefaultConnectionCount, "Number of reverse connections to maintain")
	cmd.Flags().BoolVar(&certManager, "cert-manager", false, "Use cert-manager CRDs for certificate management instead of raw secrets")
	cmd.Flags().StringVar(&secretRef, "secret-ref", "", "Reference an existing K8s Secret for TLS certificates (skip cert generation)")
	cmd.Flags().StringVar(&envoyImage, "envoy-image", manifest.DefaultEnvoyImage, "Envoy proxy image")
	cmd.Flags().StringVar(&serviceType, "service-type", manifest.DefaultServiceType, "Responder Service type (LoadBalancer, NodePort, ClusterIP)")

	// Bare-metal-only flags.
	cmd.Flags().StringVar(&envoyCommand, "envoy-command", baremetal.DefaultEnvoyCommand, "Command to run Envoy (bare-metal only)")
	cmd.Flags().StringVar(&certInstallPath, "cert-install-path", baremetal.DefaultCertInstallPath, "Path to install certs on host (bare-metal only)")
	cmd.Flags().StringVar(&configInstallPath, "config-install-path", baremetal.DefaultConfigInstallPath, "Path to install Envoy config on host (bare-metal only)")
	cmd.Flags().StringVar(&runUser, "run-user", baremetal.DefaultRunUser, "OS user for running Envoy (bare-metal only)")

	cmd.AddCommand(newGenerateExposeCmd())

	return cmd
}

// generateK8sOpts holds options for the Kubernetes generate path.
type generateK8sOpts struct {
	outputDir, namespace, certDir, initiatorCertDir, responderCertDir string
	envoyImage, envoyLogLevel, responderEndpoint, serviceType, secretRef string
	tunnelPort, connectionCount                                          int
	certValidity                                                         time.Duration
	certManager                                                          bool
	serviceFlags, serviceLocalPorts                                      []string
}

func runGenerateKubernetes(cmd *cobra.Command, args []string, opts generateK8sOpts) error {
	sourceContext := args[0]
	destinationContext := args[1]

	services, err := parseServiceFlags(opts.serviceFlags, opts.serviceLocalPorts)
	if err != nil {
		return fmt.Errorf("invalid service flags: %w", err)
	}

	cfg := manifest.TunnelConfig{
		SourceContext:      sourceContext,
		DestinationContext: destinationContext,
		Namespace:          opts.namespace,
		ResponderEndpoint:  opts.responderEndpoint,
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
		return fmt.Errorf("failed to render manifests: %w", err)
	}

	if err := manifest.WriteToDisk(bundle, opts.outputDir); err != nil {
		return fmt.Errorf("failed to write manifests: %w", err)
	}

	// Print summary.
	out := cmd.OutOrStdout()
	if opts.secretRef != "" {
		fmt.Fprintf(out, "Using existing secret %q for TLS certificates\n", opts.secretRef)
	} else if opts.certManager {
		fmt.Fprintf(out, "Generated tunnel manifests with cert-manager CRDs\n")
	} else {
		fmt.Fprintf(out, "Generated tunnel CA and certificates\n")
	}
	fmt.Fprintf(out, "Wrote source cluster manifests      → %s/source/\n", opts.outputDir)
	fmt.Fprintf(out, "Wrote destination cluster manifests → %s/destination/\n", opts.outputDir)
	fmt.Fprintf(out, "\nTunnel name: %s\n", bundle.Metadata.TunnelName)
	fmt.Fprintf(out, "Namespace:   %s\n", bundle.Metadata.Namespace)
	fmt.Fprintf(out, "\nNext steps:\n")
	fmt.Fprintf(out, "  kubectl apply -k %s/destination/ --context %s\n", opts.outputDir, destinationContext)
	fmt.Fprintf(out, "  kubectl apply -k %s/source/ --context %s\n", opts.outputDir, sourceContext)

	return nil
}

// generateBareMetalOpts holds options for the bare-metal generate path.
type generateBareMetalOpts struct {
	outputDir, certDir, initiatorCertDir, responderCertDir         string
	envoyLogLevel, responderEndpoint                               string
	envoyCommand, certInstallPath, configInstallPath, runUser      string
	tunnelPort                                                     int
	certValidity                                                   time.Duration
	serviceFlags, serviceLocalPorts                                []string
}

func runGenerateBareMetal(cmd *cobra.Command, args []string, opts generateBareMetalOpts) error {
	sourceHost := args[0]
	destinationHost := args[1]

	services, err := parseServiceFlags(opts.serviceFlags, opts.serviceLocalPorts)
	if err != nil {
		return fmt.Errorf("invalid service flags: %w", err)
	}

	cfg := baremetal.BareMetalConfig{
		SourceHost:        sourceHost,
		DestinationHost:   destinationHost,
		ResponderEndpoint: opts.responderEndpoint,
		TunnelPort:        opts.tunnelPort,
		CertValidity:      opts.certValidity,
		EnvoyLogLevel:     opts.envoyLogLevel,
		EnvoyCommand:      opts.envoyCommand,
		CertInstallPath:   opts.certInstallPath,
		ConfigInstallPath: opts.configInstallPath,
		RunUser:           opts.runUser,
		CertDir:           opts.certDir,
		InitiatorCertDir:  opts.initiatorCertDir,
		ResponderCertDir:  opts.responderCertDir,
		Services:          services,
	}

	bundle, err := baremetal.Render(cfg)
	if err != nil {
		return fmt.Errorf("failed to render bare-metal artifacts: %w", err)
	}

	if err := baremetal.WriteToDisk(bundle, opts.outputDir); err != nil {
		return fmt.Errorf("failed to write bare-metal artifacts: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Generated tunnel CA and certificates\n")
	fmt.Fprintf(out, "Wrote initiator artifacts → %s/initiator/\n", opts.outputDir)
	fmt.Fprintf(out, "Wrote responder artifacts → %s/responder/\n", opts.outputDir)
	fmt.Fprintf(out, "\nTunnel name: %s\n", bundle.Metadata.TunnelName)

	fmt.Fprintf(out, "\nNext steps (using func-e):\n")
	fmt.Fprintf(out, "  scp -r %s/responder/ user@%s:/tmp/portal/\n", opts.outputDir, destinationHost)
	fmt.Fprintf(out, "  ssh user@%s 'func-e run -c /tmp/portal/envoy.yaml'\n", destinationHost)
	fmt.Fprintf(out, "  scp -r %s/initiator/ user@%s:/tmp/portal/\n", opts.outputDir, sourceHost)
	fmt.Fprintf(out, "  ssh user@%s 'func-e run -c /tmp/portal/envoy.yaml'\n", sourceHost)

	fmt.Fprintf(out, "\nNext steps (using systemd):\n")
	fmt.Fprintf(out, "  scp -r %s/responder/ user@%s:/tmp/portal/\n", opts.outputDir, destinationHost)
	fmt.Fprintf(out, "  ssh user@%s 'sudo cp /tmp/portal/portal-responder.service /etc/systemd/system/ && sudo systemctl enable --now portal-responder'\n", destinationHost)
	fmt.Fprintf(out, "  scp -r %s/initiator/ user@%s:/tmp/portal/\n", opts.outputDir, sourceHost)
	fmt.Fprintf(out, "  ssh user@%s 'sudo cp /tmp/portal/portal-initiator.service /etc/systemd/system/ && sudo systemctl enable --now portal-initiator'\n", sourceHost)

	fmt.Fprintf(out, "\nNext steps (using Docker):\n")
	fmt.Fprintf(out, "  scp -r %s/responder/ user@%s:/opt/portal/\n", opts.outputDir, destinationHost)
	fmt.Fprintf(out, "  ssh user@%s 'cd /opt/portal && docker compose up -d'\n", destinationHost)
	fmt.Fprintf(out, "  scp -r %s/initiator/ user@%s:/opt/portal/\n", opts.outputDir, sourceHost)
	fmt.Fprintf(out, "  ssh user@%s 'cd /opt/portal && docker compose up -d'\n", sourceHost)

	return nil
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
		return fmt.Errorf("failed to find tunnel for context %q: %w", kubeContext, err)
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
