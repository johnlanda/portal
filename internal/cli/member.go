package cli

import (
	"context"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/manifest"
	"github.com/johnlanda/portal/internal/state"
	"github.com/johnlanda/portal/internal/validate"
)

// memberDeployConfig assembles the manifest config for a member from its
// membership record. TLS material is never included: the member Secret is
// managed directly so private keys stay in the member cluster.
func memberDeployConfig(m *state.MembershipState) manifest.MemberDeployConfig {
	var published []envoy.PublishedService
	for _, p := range m.Published {
		ns := p.Namespace
		if ns == "" {
			ns = "default"
		}
		published = append(published, envoy.PublishedService{
			Name:        p.Name,
			BackendHost: fmt.Sprintf("%s.%s.svc.cluster.local", p.Name, ns),
			BackendPort: p.Port,
			Protocol:    p.Protocol,
		})
	}
	var forward []envoy.ServiceListener
	for _, f := range m.Forward {
		forward = append(forward, envoy.ServiceListener{
			Name:       f.Name,
			ListenPort: f.LocalPort,
			SNI:        f.SNI,
		})
	}
	return manifest.MemberDeployConfig{
		MemberName:            m.Member,
		EnvoyImage:            m.EnvoyImage,
		AllowUnsupportedEnvoy: m.AllowUnsupportedEnvoy,
		HubName:               m.Hub,
		Namespace:             m.Namespace,
		HubAddr:               m.HubAddr,
		HandshakeSNI:          m.HandshakeSNI,
		ConnectionCount:       m.ConnectionCount,
		Published:             published,
		Forward:               forward,
	}
}

// applyMember re-renders and applies the member's manifests, then restarts
// the member deployment so Envoy picks up the new static bootstrap.
func applyMember(ctx context.Context, m *state.MembershipState) error {
	resources, err := manifest.RenderMemberManifests(memberDeployConfig(m))
	if err != nil {
		return fmt.Errorf("failed to render member manifests: %w", err)
	}
	client := newKubeClient(m.Context, m.Namespace)
	if err := client.Apply(ctx, extractContents(resources)); err != nil {
		return fmt.Errorf("failed to apply member resources: %w", err)
	}
	if err := client.RolloutRestart(ctx, "portal-member"); err != nil {
		return fmt.Errorf("failed to restart member deployment: %w", err)
	}
	return nil
}

// membershipByContext finds the membership for a kube context, optionally
// disambiguated by member name.
func membershipByContext(store *state.Store, kubeContext, member string) (*state.MembershipState, error) {
	if member != "" {
		m, err := store.GetMembership(member)
		if err != nil {
			return nil, err
		}
		return m, nil
	}
	memberships, err := store.ListMemberships()
	if err != nil {
		return nil, err
	}
	var matches []state.MembershipState
	for _, m := range memberships {
		if m.Context == kubeContext {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no membership found for context %q; run 'portal join' first", kubeContext)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("multiple memberships use context %q; select one with --member", kubeContext)
	}
}

// --- join ---

type joinOpts struct {
	member                string
	hubAddr               string
	hubName               string
	credentialPath        string
	certPath              string
	csrOut                string
	namespace             string
	handshakeSNI          string
	connectionCount       int
	deployTimeout         time.Duration
	envoyImage            string
	allowUnsupportedEnvoy bool
	dryRun                bool
}

// NewJoinCmd creates the `portal join` command.
func NewJoinCmd() *cobra.Command {
	var opts joinOpts
	cmd := &cobra.Command{
		Use:   "join <context>",
		Short: "Join this cluster to a hub as a member (egress only)",
		Long: `Joins a cluster to a hub. The member dials out and maintains persistent
reverse connections; no inbound ports are required on the member side.

Two-phase CSR enrollment (recommended — the private key never leaves this
cluster):

  1. portal join <ctx> --member acme-prod --hub-addr tunnel.example:10443
     Generates the keypair as an in-cluster Secret and writes acme-prod.csr.
     Send the CSR to the hub owner, who runs 'portal hub sign'.
  2. portal join <ctx> --member acme-prod --cert acme-prod-cert.pem
     Installs the signed certificate and deploys the member.

Credential mode (single-operator shortcut): portal join <ctx> --credential
<file> completes in one step using a bundle from 'portal hub invite'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJoin(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.member, "member", "", "Member name (this cluster's identity)")
	cmd.Flags().StringVar(&opts.hubAddr, "hub-addr", "", "Hub tunnel endpoint (host:port) — starts phase 1 of CSR enrollment")
	cmd.Flags().StringVar(&opts.hubName, "hub-name", "portal", "Hub name (used as the tenant identifier)")
	cmd.Flags().StringVar(&opts.credentialPath, "credential", "", "Credential file from 'portal hub invite' (single-step join)")
	cmd.Flags().StringVar(&opts.certPath, "cert", "", "Signed certificate bundle from 'portal hub sign' — completes phase 2")
	cmd.Flags().StringVar(&opts.csrOut, "csr-out", "", "CSR output path for phase 1 (default: <member>.csr)")
	cmd.Flags().StringVar(&opts.namespace, "namespace", manifest.DefaultNamespace, "Namespace for member components")
	cmd.Flags().StringVar(&opts.handshakeSNI, "handshake-sni", envoyDefaultHandshakeSNI, "Reverse tunnel handshake SNI (must match the hub)")
	cmd.Flags().IntVar(&opts.connectionCount, "connection-count", manifest.DefaultConnectionCount, "Number of reverse connections to maintain")
	cmd.Flags().DurationVar(&opts.deployTimeout, "deploy-timeout", 5*time.Minute, "Timeout waiting for deployment readiness")
	cmd.Flags().StringVar(&opts.envoyImage, "envoy-image", "", "Envoy proxy image (default: the pinned image)")
	cmd.Flags().BoolVar(&opts.allowUnsupportedEnvoy, "allow-unsupported-envoy", false, "Bypass the Envoy version gate (reverse tunnel APIs are experimental upstream)")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print rendered manifests without applying (credential mode only)")
	return cmd
}

func runJoin(cmd *cobra.Command, kubeContext string, opts joinOpts) error {
	if err := checkKubectlFn(); err != nil {
		return fmt.Errorf("prerequisite check failed: %w", err)
	}
	if err := checkContextFn(kubeContext); err != nil {
		return fmt.Errorf("context validation failed: %w", err)
	}
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	switch {
	case opts.credentialPath != "":
		return joinWithCredential(cmd, store, kubeContext, opts)
	case opts.certPath != "":
		return joinPhase2(cmd, store, kubeContext, opts)
	case opts.hubAddr != "":
		return joinPhase1(cmd, store, kubeContext, opts)
	default:
		return fmt.Errorf("one of --hub-addr (phase 1), --cert (phase 2), or --credential is required")
	}
}

func joinPhase1(cmd *cobra.Command, store *state.Store, kubeContext string, opts joinOpts) error {
	if opts.member == "" {
		return fmt.Errorf("--member is required with --hub-addr")
	}
	if err := validate.DNSName(opts.member); err != nil {
		return fmt.Errorf("invalid member name: %w", err)
	}
	if _, err := store.GetMembership(opts.member); err == nil {
		return fmt.Errorf("membership %q already exists; use 'portal leave' first", opts.member)
	}
	if opts.csrOut == "" {
		opts.csrOut = opts.member + ".csr"
	}

	keyPEM, csrPEM, err := certs.GenerateMemberKeyAndCSR(certs.MemberIdentity{
		Member: opts.member,
		Tenant: opts.hubName,
	})
	if err != nil {
		return fmt.Errorf("failed to generate member key: %w", err)
	}

	// The key goes straight into an in-cluster Secret and nowhere else.
	resources, err := manifest.RenderMemberEnrollmentResources(opts.namespace, keyPEM)
	if err != nil {
		return fmt.Errorf("failed to render enrollment resources: %w", err)
	}
	client := newKubeClient(kubeContext, opts.namespace)
	if err := client.Apply(context.Background(), extractContents(resources)); err != nil {
		return fmt.Errorf("failed to create in-cluster key Secret: %w", err)
	}
	if err := os.WriteFile(opts.csrOut, csrPEM, 0o644); err != nil {
		return fmt.Errorf("failed to write CSR: %w", err)
	}

	membership := state.MembershipState{
		Member:                opts.member,
		Hub:                   opts.hubName,
		HubAddr:               opts.hubAddr,
		Context:               kubeContext,
		Namespace:             opts.namespace,
		HandshakeSNI:          opts.handshakeSNI,
		ConnectionCount:       opts.connectionCount,
		EnvoyImage:            opts.envoyImage,
		AllowUnsupportedEnvoy: opts.allowUnsupportedEnvoy,
		Pending:               true,
		JoinedAt:              time.Now(),
	}
	if err := store.AddMembership(membership); err != nil {
		return fmt.Errorf("failed to save membership state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Keypair generated in-cluster (Secret %s/%s); the private key never leaves this cluster\n", opts.namespace, manifest.MemberSecretName)
	fmt.Fprintf(out, "✓ CSR written to %s\n", opts.csrOut)
	fmt.Fprintf(out, "\nSend %s to the hub owner, who signs it with:\n", opts.csrOut)
	fmt.Fprintf(out, "  portal hub sign %s --member %s\n", opts.csrOut, opts.member)
	fmt.Fprintf(out, "\nThen complete enrollment with the returned bundle:\n")
	fmt.Fprintf(out, "  portal join %s --member %s --cert %s-cert.pem\n", kubeContext, opts.member, opts.member)
	return nil
}

func joinPhase2(cmd *cobra.Command, store *state.Store, kubeContext string, opts joinOpts) error {
	if opts.member == "" {
		return fmt.Errorf("--member is required with --cert")
	}
	membership, err := store.GetMembership(opts.member)
	if err != nil {
		return fmt.Errorf("no pending membership for %q; run phase 1 first (--hub-addr): %w", opts.member, err)
	}
	if !membership.Pending {
		return fmt.Errorf("membership %q is already enrolled", opts.member)
	}

	bundle, err := os.ReadFile(opts.certPath)
	if err != nil {
		return fmt.Errorf("failed to read certificate bundle: %w", err)
	}
	leafPEM, caPEM, err := splitCertBundle(bundle)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := newKubeClient(membership.Context, membership.Namespace)
	if err := client.PatchSecret(ctx, manifest.MemberSecretName, map[string][]byte{
		"tls.crt": leafPEM,
		"ca.crt":  caPEM,
	}); err != nil {
		return fmt.Errorf("failed to install certificate into member Secret: %w", err)
	}

	resources, err := manifest.RenderMemberManifests(memberDeployConfig(membership))
	if err != nil {
		return fmt.Errorf("failed to render member manifests: %w", err)
	}
	if err := client.Apply(ctx, extractContents(resources)); err != nil {
		return fmt.Errorf("failed to apply member resources: %w", err)
	}
	if err := client.WaitForDeployment(ctx, "portal-member", opts.deployTimeout); err != nil {
		return fmt.Errorf("member deployment not ready: %w", err)
	}

	membership.Pending = false
	if err := store.UpdateMembership(*membership); err != nil {
		return fmt.Errorf("failed to save membership state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Member %q enrolled and connected to %s\n", membership.Member, membership.HubAddr)
	fmt.Fprintf(out, "\nPublish services the hub may reach:\n")
	fmt.Fprintf(out, "  portal publish %s <service> --port <port>\n", kubeContext)
	return nil
}

func joinWithCredential(cmd *cobra.Command, store *state.Store, kubeContext string, opts joinOpts) error {
	cred, err := readCredential(opts.credentialPath)
	if err != nil {
		return err
	}
	if _, err := store.GetMembership(cred.Member); err == nil {
		return fmt.Errorf("membership %q already exists; use 'portal leave' first", cred.Member)
	}
	handshakeSNI := cred.HandshakeSNI
	if handshakeSNI == "" {
		handshakeSNI = opts.handshakeSNI
	}

	membership := state.MembershipState{
		Member:                cred.Member,
		Hub:                   cred.Hub,
		HubAddr:               cred.HubAddr,
		Context:               kubeContext,
		Namespace:             opts.namespace,
		HandshakeSNI:          handshakeSNI,
		ConnectionCount:       opts.connectionCount,
		EnvoyImage:            opts.envoyImage,
		AllowUnsupportedEnvoy: opts.allowUnsupportedEnvoy,
		JoinedAt:              time.Now(),
	}

	cfg := memberDeployConfig(&membership)
	cfg.CertPEM = []byte(cred.Cert)
	cfg.KeyPEM = []byte(cred.Key)
	cfg.CAPEM = []byte(cred.CA)
	resources, err := manifest.RenderMemberManifests(cfg)
	if err != nil {
		return fmt.Errorf("failed to render member manifests: %w", err)
	}
	if opts.dryRun {
		printResources(cmd.OutOrStdout(), resources)
		return nil
	}

	ctx := context.Background()
	client := newKubeClient(kubeContext, opts.namespace)
	if err := client.Apply(ctx, extractContents(resources)); err != nil {
		return fmt.Errorf("failed to apply member resources: %w", err)
	}
	if err := client.WaitForDeployment(ctx, "portal-member", opts.deployTimeout); err != nil {
		return fmt.Errorf("member deployment not ready: %w", err)
	}
	if err := store.AddMembership(membership); err != nil {
		return fmt.Errorf("failed to save membership state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Member %q joined hub %q at %s\n", cred.Member, cred.Hub, cred.HubAddr)
	fmt.Fprintf(out, "  Delete %s now that the key is installed\n", opts.credentialPath)
	return nil
}

// splitCertBundle splits a PEM bundle into the leaf certificate (first block)
// and the CA chain (remaining blocks).
func splitCertBundle(bundle []byte) (leaf, ca []byte, err error) {
	block, rest := pem.Decode(bundle)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("certificate bundle does not start with a CERTIFICATE block")
	}
	leaf = pem.EncodeToMemory(block)
	if len(rest) == 0 {
		return nil, nil, fmt.Errorf("certificate bundle is missing the CA certificate (expected leaf followed by CA)")
	}
	return leaf, rest, nil
}

// --- publish / unpublish ---

type publishOpts struct {
	member           string
	port             int
	protocol         string
	serviceNamespace string
	dryRun           bool
}

// NewPublishCmd creates the `portal publish` command.
func NewPublishCmd() *cobra.Command {
	var opts publishOpts
	cmd := &cobra.Command{
		Use:   "publish <context> <service> --port <port>",
		Short: "Publish a member-local service so the hub can reach it",
		Long: `Adds a local service to the member's publish allowlist. The hub reaches
it over the reverse tunnel at the canonical authority <service>.<member>.

The reverse path is HTTP/2-only: --protocol accepts http (default) and grpc.
Raw TCP services cannot be published over the reverse path.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublish(cmd, args[0], args[1], opts)
		},
	}
	cmd.Flags().StringVar(&opts.member, "member", "", "Member name (required when the context has multiple memberships)")
	cmd.Flags().IntVar(&opts.port, "port", 0, "Port the service listens on (required)")
	_ = cmd.MarkFlagRequired("port")
	cmd.Flags().StringVar(&opts.protocol, "protocol", "http", "Service protocol: http or grpc")
	cmd.Flags().StringVar(&opts.serviceNamespace, "service-namespace", "default", "Namespace of the service being published")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Print the re-rendered manifests without applying")
	return cmd
}

func runPublish(cmd *cobra.Command, kubeContext, service string, opts publishOpts) error {
	if opts.protocol == "tcp" {
		return fmt.Errorf("the reverse tunnel path is HTTP/2-only and cannot carry raw TCP; use a gRPC/HTTP interface, or the forward path for hub-side TCP services")
	}
	if opts.protocol != "http" && opts.protocol != "grpc" {
		return fmt.Errorf("unsupported protocol %q (supported: http, grpc)", opts.protocol)
	}
	if err := validate.DNSName(service); err != nil {
		return fmt.Errorf("invalid service name: %w", err)
	}
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	membership, err := membershipByContext(store, kubeContext, opts.member)
	if err != nil {
		return err
	}
	if membership.Pending {
		return fmt.Errorf("membership %q is still pending; complete enrollment first", membership.Member)
	}
	if membership.PublishedService(service) != nil {
		return fmt.Errorf("service %q is already published; use 'portal unpublish' first", service)
	}

	membership.Published = append(membership.Published, state.PublishedEntry{
		Name:      service,
		Namespace: opts.serviceNamespace,
		Port:      opts.port,
		Protocol:  opts.protocol,
	})
	if opts.dryRun {
		resources, err := manifest.RenderMemberManifests(memberDeployConfig(membership))
		if err != nil {
			return err
		}
		printResources(cmd.OutOrStdout(), resources)
		return nil
	}
	if err := applyMember(context.Background(), membership); err != nil {
		return err
	}
	if err := store.UpdateMembership(*membership); err != nil {
		return fmt.Errorf("failed to save membership state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Published %s (%s) — reachable from the hub as %s.%s\n", service, opts.protocol, service, membership.Member)
	fmt.Fprintf(out, "  Hub-side apps can be given a friendly name with: portal route %s/%s\n", membership.Member, service)
	return nil
}

// NewUnpublishCmd creates the `portal unpublish` command.
func NewUnpublishCmd() *cobra.Command {
	var member string
	cmd := &cobra.Command{
		Use:   "unpublish <context> <service>",
		Short: "Remove a service from the member's publish allowlist",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnpublish(cmd, args[0], args[1], member)
		},
	}
	cmd.Flags().StringVar(&member, "member", "", "Member name (required when the context has multiple memberships)")
	return cmd
}

func runUnpublish(cmd *cobra.Command, kubeContext, service, member string) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	membership, err := membershipByContext(store, kubeContext, member)
	if err != nil {
		return err
	}
	found := false
	for i, p := range membership.Published {
		if p.Name == service {
			membership.Published = append(membership.Published[:i], membership.Published[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("service %q is not published", service)
	}
	if err := applyMember(context.Background(), membership); err != nil {
		return err
	}
	if err := store.UpdateMembership(*membership); err != nil {
		return fmt.Errorf("failed to save membership state: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Unpublished %s from member %s\n", service, membership.Member)
	return nil
}

// --- leave ---

// NewLeaveCmd creates the `portal leave` command.
func NewLeaveCmd() *cobra.Command {
	var member string
	cmd := &cobra.Command{
		Use:   "leave <context>",
		Short: "Leave a hub: tear down member components and forget the membership",
		Long: `Removes the member deployment, configuration, and TLS Secret from the
cluster and deletes the local membership record. The hub owner should also
run 'portal hub evict' to revoke the member's certificate.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLeave(cmd, args[0], member)
		},
	}
	cmd.Flags().StringVar(&member, "member", "", "Member name (required when the context has multiple memberships)")
	return cmd
}

func runLeave(cmd *cobra.Command, kubeContext, member string) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	membership, err := membershipByContext(store, kubeContext, member)
	if err != nil {
		return err
	}

	resources, err := manifest.RenderMemberManifests(memberDeployConfig(membership))
	if err != nil {
		return fmt.Errorf("failed to render member manifests: %w", err)
	}
	// The TLS Secret is managed out of band by the enrollment flow; include
	// a minimal rendering so deletion covers it.
	if secretResources, err := manifest.RenderMemberEnrollmentResources(membership.Namespace, []byte("placeholder")); err == nil {
		resources = append(resources, secretResources[1:]...)
	}

	client := newKubeClient(membership.Context, membership.Namespace)
	if err := client.Delete(context.Background(), extractContents(resources)); err != nil {
		return fmt.Errorf("failed to delete member resources: %w", err)
	}
	if err := store.RemoveMembership(membership.Member); err != nil {
		return fmt.Errorf("failed to remove membership state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Member %q left hub %q; components removed from context %q\n", membership.Member, membership.Hub, membership.Context)
	fmt.Fprintf(out, "  Ask the hub owner to run: portal hub evict %s\n", membership.Member)
	return nil
}
