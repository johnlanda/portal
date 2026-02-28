package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tetratelabs/portal/internal/kube"
	"github.com/tetratelabs/portal/internal/manifest"
	"github.com/tetratelabs/portal/internal/state"
)

// placeholderEndpoint is used when re-rendering manifests for deletion.
// The actual value doesn't matter — only resource metadata (names, namespaces) is needed.
const placeholderEndpoint = "disconnect.placeholder.local"

type disconnectOpts struct {
	namespace     string
	deleteTimeout time.Duration
}

// NewDisconnectCmd creates the `portal disconnect` command.
func NewDisconnectCmd() *cobra.Command {
	var opts disconnectOpts

	cmd := &cobra.Command{
		Use:   "disconnect <source_context> <destination_context>",
		Short: "Tear down a tunnel and clean up resources",
		Long: `Remove a Portal tunnel by deleting resources from both clusters.

Deletes the initiator resources from the source cluster and the responder
resources from the destination cluster, then removes the tunnel from
~/.portal/tunnels.json and cleans up local CA certificates.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDisconnect(cmd, args[0], args[1], opts)
		},
	}

	cmd.Flags().StringVar(&opts.namespace, "namespace", "", "Override namespace (default: read from tunnel state)")
	cmd.Flags().DurationVar(&opts.deleteTimeout, "delete-timeout", 2*time.Minute, "Timeout waiting for resource deletion")

	return cmd
}

func runDisconnect(cmd *cobra.Command, sourceCtx, destCtx string, opts disconnectOpts) error {
	// 1. Fail fast if kubectl is missing.
	if err := checkKubectlFn(); err != nil {
		return fmt.Errorf("prerequisite check failed: %w", err)
	}

	// 2. Validate kube contexts exist.
	if err := checkContextFn(sourceCtx); err != nil {
		return err
	}
	if err := checkContextFn(destCtx); err != nil {
		return err
	}

	tunnelName := sourceCtx + "--" + destCtx

	// 2. Look up tunnel in state.
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	ts, err := store.Get(tunnelName)
	if err != nil {
		return fmt.Errorf("failed to read tunnel state: %w", err)
	}
	if ts == nil {
		return fmt.Errorf("tunnel %q not found in state; nothing to disconnect", tunnelName)
	}

	// Use stored namespace unless overridden.
	ns := ts.Namespace
	if opts.namespace != "" {
		ns = opts.namespace
	}
	port := ts.TunnelPort

	// 3. Re-render manifests to get resource definitions for deletion.
	endpoint := fmt.Sprintf("%s:%d", placeholderEndpoint, port)
	cfg := manifest.TunnelConfig{
		SourceContext:      sourceCtx,
		DestinationContext: destCtx,
		Namespace:          ns,
		ResponderEndpoint:  endpoint,
		TunnelPort:         port,
	}

	bundle, err := manifest.Render(cfg)
	if err != nil {
		return fmt.Errorf("failed to render manifests for deletion: %w", err)
	}

	// 4. Create kube clients.
	sourceClient := newKubeClient(sourceCtx, ns)
	destClient := newKubeClient(destCtx, ns)

	ctx := context.Background()
	out := cmd.OutOrStdout()

	// 5. Delete initiator (source) first — it depends on the responder.
	if err := sourceClient.Delete(ctx, extractContents(bundle.Source)); err != nil {
		return fmt.Errorf("failed to delete source resources in %s: %w", sourceCtx, err)
	}
	fmt.Fprintf(out, "\u2713 Deleted initiator resources from %s (namespace: %s)\n", sourceCtx, ns)

	// 6. Delete responder (destination).
	if err := destClient.Delete(ctx, extractContents(bundle.Destination)); err != nil {
		return fmt.Errorf("failed to delete destination resources in %s: %w", destCtx, err)
	}
	fmt.Fprintf(out, "\u2713 Deleted responder resources from %s (namespace: %s)\n", destCtx, ns)

	// 7. Delete exposed services (best-effort, non-fatal).
	if len(ts.Services) > 0 {
		deleteExposedServices(ctx, cmd, ts, sourceClient, destClient, sourceCtx, destCtx, ns)
	}

	// 8. Remove tunnel from state.
	if err := store.Remove(tunnelName); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to remove tunnel from state: %v\n", err)
	}

	// 9. Clean up local CA certs.
	if ts.CACertPath != "" {
		certDir := filepath.Dir(ts.CACertPath)
		if err := os.RemoveAll(certDir); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to remove CA certificates from %s: %v\n", certDir, err)
		}
	}

	fmt.Fprintf(out, "\u2713 Tunnel %s disconnected\n", tunnelName)
	return nil
}

// deleteExposedServices removes ClusterIP Services created by portal expose.
// Each service entry is "serviceName:port". The ClusterIP was created in the
// opposite cluster, so we try deleting from both sides using the naming
// convention portal-<context>-<serviceName>.
func deleteExposedServices(ctx context.Context, cmd *cobra.Command, ts *state.TunnelState, sourceClient, destClient kube.Client, sourceCtx, destCtx, ns string) {
	out := cmd.OutOrStdout()
	for _, entry := range ts.Services {
		svcName, _, _ := strings.Cut(entry, ":")

		// Service exposed from source → ClusterIP lives in destination.
		srcExposeName := fmt.Sprintf("portal-%s-%s", sourceCtx, svcName)
		srcYAML := buildDeleteServiceYAML(srcExposeName, ns)
		if err := destClient.Delete(ctx, [][]byte{srcYAML}); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to delete exposed service %s from %s: %v\n", srcExposeName, destCtx, err)
		}

		// Service exposed from destination → ClusterIP lives in source.
		dstExposeName := fmt.Sprintf("portal-%s-%s", destCtx, svcName)
		dstYAML := buildDeleteServiceYAML(dstExposeName, ns)
		if err := sourceClient.Delete(ctx, [][]byte{dstYAML}); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to delete exposed service %s from %s: %v\n", dstExposeName, sourceCtx, err)
		}
	}
	fmt.Fprintf(out, "\u2713 Cleaned up %d exposed service(s)\n", len(ts.Services))
}

// buildDeleteServiceYAML builds a minimal Service YAML sufficient for kubectl delete.
func buildDeleteServiceYAML(name, namespace string) []byte {
	// kubectl delete only needs apiVersion, kind, and metadata to identify the resource.
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
`, name, namespace))
}
