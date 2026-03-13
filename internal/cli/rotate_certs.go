package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/manifest"
)

// NewRotateCertsCmd creates the `portal rotate-certs` command.
func NewRotateCertsCmd() *cobra.Command {
	var certValidity time.Duration

	cmd := &cobra.Command{
		Use:   "rotate-certs <tunnel-dir>",
		Short: "Rotate leaf certificates for an existing tunnel",
		Long: `Rotate the initiator and responder TLS certificates using the existing tunnel CA.

Only the portal-tunnel-tls-secret.yaml files in source/ and destination/ are
regenerated. All other manifests (deployments, configmaps, services) remain
unchanged. After rotation, re-apply the updated secrets to both clusters.

The tunnel must have been generated with a version of portal that persists the
CA material in the ca/ directory.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tunnelDir := args[0]

			cfg := manifest.RotateConfig{
				TunnelDir:    tunnelDir,
				CertValidity: certValidity,
			}

			meta, err := manifest.RotateCertificates(cfg)
			if err != nil {
				return fmt.Errorf("failed to rotate certificates: %w", err)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Rotated certificates for tunnel %q\n", meta.TunnelName)
			fmt.Fprintf(out, "Rotation count: %d\n", meta.RotationCount)

			if meta.DeployTarget == "bare-metal" {
				fmt.Fprintf(out, "Updated %s/initiator/certs/\n", tunnelDir)
				fmt.Fprintf(out, "Updated %s/responder/certs/\n", tunnelDir)
				fmt.Fprintf(out, "\nNext steps:\n")
				fmt.Fprintf(out, "  scp -r %s/responder/certs/ user@%s:/etc/portal/certs/\n", tunnelDir, meta.DestinationContext)
				fmt.Fprintf(out, "  scp -r %s/initiator/certs/ user@%s:/etc/portal/certs/\n", tunnelDir, meta.SourceContext)
				fmt.Fprintf(out, "  (Envoy will detect the updated certs via SDS watched_directory — no restart needed)\n")
			} else {
				fmt.Fprintf(out, "Updated %s/source/portal-tunnel-tls-secret.yaml\n", tunnelDir)
				fmt.Fprintf(out, "Updated %s/destination/portal-tunnel-tls-secret.yaml\n", tunnelDir)
				fmt.Fprintf(out, "\nNext steps:\n")
				fmt.Fprintf(out, "  kubectl apply -f %s/destination/portal-tunnel-tls-secret.yaml --context %s\n", tunnelDir, meta.DestinationContext)
				fmt.Fprintf(out, "  kubectl apply -f %s/source/portal-tunnel-tls-secret.yaml --context %s\n", tunnelDir, meta.SourceContext)
			}

			return nil
		},
	}

	cmd.Flags().DurationVar(&certValidity, "cert-validity", 0, "Certificate validity duration (default: reuse from tunnel.yaml)")

	return cmd
}
