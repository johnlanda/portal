package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/manifest"
)

// NewRenewCmd creates the `portal renew` command.
func NewRenewCmd() *cobra.Command {
	var (
		member   string
		certPath string
		csrOut   string
	)
	cmd := &cobra.Command{
		Use:   "renew <context>",
		Short: "Rotate a member's certificate (two-phase, key stays in-cluster)",
		Long: `Renews a member's certificate using the same two-phase flow as
enrollment — no new ceremony to learn:

  1. portal renew <ctx>
     Generates a fresh keypair into the in-cluster Secret's staging key and
     writes a new CSR. Send it to the hub owner ('portal hub sign' as usual;
     re-signing replaces the recorded serial).
  2. portal renew <ctx> --cert <bundle>
     Installs the new certificate and key atomically; SDS hot-reloads with
     zero downtime. The old certificate keeps working until this step.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRenew(cmd, args[0], member, certPath, csrOut)
		},
	}
	cmd.Flags().StringVar(&member, "member", "", "Member name (required when the context has multiple memberships)")
	cmd.Flags().StringVar(&certPath, "cert", "", "Signed certificate bundle — completes the renewal")
	cmd.Flags().StringVar(&csrOut, "csr-out", "", "CSR output path (default: <member>-renew.csr)")
	return cmd
}

func runRenew(cmd *cobra.Command, kubeContext, member, certPath, csrOut string) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	membership, err := membershipByContext(store, kubeContext, member)
	if err != nil {
		return err
	}
	if membership.Pending {
		return fmt.Errorf("membership %q has not completed initial enrollment", membership.Member)
	}

	ctx := context.Background()
	client := newKubeClient(membership.Context, membership.Namespace)
	out := cmd.OutOrStdout()

	if certPath == "" {
		// Phase 1: fresh keypair into a staging slot in the Secret; the live
		// key/cert pair keeps serving until phase 2 promotes the new pair.
		if csrOut == "" {
			csrOut = membership.Member + "-renew.csr"
		}
		keyPEM, csrPEM, err := certs.GenerateMemberKeyAndCSR(certs.MemberIdentity{
			Member: membership.Member,
			Tenant: membership.Hub,
		})
		if err != nil {
			return fmt.Errorf("failed to generate renewal key: %w", err)
		}
		if err := client.PatchSecret(ctx, manifest.MemberSecretName, map[string][]byte{
			"tls.key.next": keyPEM,
		}); err != nil {
			return fmt.Errorf("failed to stage renewal key: %w", err)
		}
		if err := os.WriteFile(csrOut, csrPEM, 0o644); err != nil {
			return fmt.Errorf("failed to write CSR: %w", err)
		}
		fmt.Fprintf(out, "✓ Renewal keypair staged in-cluster; CSR written to %s\n", csrOut)
		fmt.Fprintf(out, "\nHave the hub owner sign it:\n  portal hub sign %s --member %s\n", csrOut, membership.Member)
		fmt.Fprintf(out, "\nThen complete the renewal:\n  portal renew %s --cert %s-cert.pem\n", kubeContext, membership.Member)
		return nil
	}

	// Phase 2: promote the staged key together with the new certificate.
	// SDS watches the mounted directory, so the swap is hot and atomic from
	// Envoy's perspective (Secret updates propagate as one volume update).
	bundle, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("failed to read certificate bundle: %w", err)
	}
	leafPEM, caPEM, err := splitCertBundle(bundle)
	if err != nil {
		return err
	}
	stagedKey, err := client.GetSecretKey(ctx, manifest.MemberSecretName, "tls.key.next")
	if err != nil {
		return fmt.Errorf("no staged renewal key found; run 'portal renew %s' (without --cert) first: %w", kubeContext, err)
	}
	if err := client.PatchSecret(ctx, manifest.MemberSecretName, map[string][]byte{
		"tls.crt": leafPEM,
		"tls.key": stagedKey,
		"ca.crt":  caPEM,
	}); err != nil {
		return fmt.Errorf("failed to install renewed certificate: %w", err)
	}
	fmt.Fprintf(out, "✓ Certificate renewed for member %q; Envoy hot-reloads via SDS (no restart)\n", membership.Member)
	fmt.Fprintln(out, "  The superseded certificate stays valid until its own expiry; the hub tracks it")
	fmt.Fprintln(out, "  and 'portal hub evict' revokes every still-valid certificate this member holds.")
	return nil
}
