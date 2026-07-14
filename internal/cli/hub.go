package cli

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/manifest"
	"github.com/johnlanda/portal/internal/state"
	"github.com/johnlanda/portal/internal/validate"
)

// NewHubCmd creates the `portal hub` command group.
func NewHubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hub",
		Short: "Manage a Portal hub (the ingress-capable side of reverse tunnels)",
		Long: `A hub is the ingress-capable side of the hub/member model: it accepts
reverse tunnel connections from egress-only members and routes hub-originated
requests to their published services. See 'portal join' for the member side.`,
	}
	cmd.AddCommand(newHubInitCmd(), newHubSignCmd(), newHubInviteCmd(), newHubEvictCmd(), newHubExposeCmd())
	return cmd
}

// stateDirFn resolves the portal state directory; swapped in tests.
var stateDirFn = state.DefaultDir

// hubDir returns the directory holding a hub's PKI material.
func hubDir(hubName string) (string, error) {
	dir, err := stateDirFn()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hubs", hubName), nil
}

// loadHub returns the hub record, defaulting to the sole hub when name is empty.
func loadHub(store *state.Store, name string) (*state.HubState, error) {
	if name != "" {
		return store.GetHub(name)
	}
	hubs, err := store.ListHubs()
	if err != nil {
		return nil, err
	}
	switch len(hubs) {
	case 0:
		return nil, fmt.Errorf("no hubs found; run 'portal hub init' first")
	case 1:
		return &hubs[0], nil
	default:
		return nil, fmt.Errorf("multiple hubs exist; select one with --hub")
	}
}

// loadHubCA loads the hub CA from the hub's PKI directory.
func loadHubCA(hub *state.HubState) (*certs.HubCA, error) {
	caCert, err := os.ReadFile(filepath.Join(hub.CADir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("failed to read hub CA certificate: %w", err)
	}
	caKey, err := os.ReadFile(filepath.Join(hub.CADir, "ca.key"))
	if err != nil {
		return nil, fmt.Errorf("failed to read hub CA key: %w", err)
	}
	return certs.LoadHubCA(caCert, caKey)
}

// hubDeployConfig assembles the manifest config for a hub from its state and
// persisted PKI material, including the current CRL.
func hubDeployConfig(hub *state.HubState) (manifest.HubDeployConfig, error) {
	var cfg manifest.HubDeployConfig
	read := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(hub.CADir, name))
	}
	certPEM, err := read("tls.crt")
	if err != nil {
		return cfg, fmt.Errorf("failed to read hub server certificate: %w", err)
	}
	keyPEM, err := read("tls.key")
	if err != nil {
		return cfg, fmt.Errorf("failed to read hub server key: %w", err)
	}
	caPEM, err := read("ca.crt")
	if err != nil {
		return cfg, fmt.Errorf("failed to read hub CA certificate: %w", err)
	}
	crlPEM, err := read("crl.pem")
	if err != nil {
		return cfg, fmt.Errorf("failed to read hub CRL: %w", err)
	}

	var members []string
	for _, m := range hub.Members {
		if !m.Evicted {
			members = append(members, m.Name)
		}
	}
	var services []manifest.ServiceConfig
	for _, s := range hub.Services {
		services = append(services, manifest.ServiceConfig{
			SNI: s.SNI, BackendHost: s.Name, BackendPort: s.Port,
		})
	}
	var routes []envoy.HubRouteAlias
	for _, r := range hub.Routes {
		routes = append(routes, envoy.HubRouteAlias{
			Member: r.Member, Service: r.Service, Alias: r.AliasService,
		})
	}
	return manifest.HubDeployConfig{
		HubName:               hub.Name,
		EnvoyImage:            hub.EnvoyImage,
		AllowUnsupportedEnvoy: hub.AllowUnsupportedEnvoy,
		Namespace:             hub.Namespace,
		TunnelPort:            hub.TunnelPort,
		EgressPort:            hub.EgressPort,
		HandshakeSNI:          hub.HandshakeSNI,
		Members:               members,
		Routes:                routes,
		Services:              services,
		EnableCRL:             true,
		CertPEM:               certPEM,
		KeyPEM:                keyPEM,
		CAPEM:                 caPEM,
		CRLPEM:                crlPEM,
	}, nil
}

// applyHub re-renders and applies the hub's manifests, then restarts the hub
// deployment so Envoy picks up the new static bootstrap.
func applyHub(ctx context.Context, hub *state.HubState) error {
	cfg, err := hubDeployConfig(hub)
	if err != nil {
		return err
	}
	resources, err := manifest.RenderHubManifests(cfg)
	if err != nil {
		return fmt.Errorf("failed to render hub manifests: %w", err)
	}
	client := newKubeClient(hub.Context, hub.Namespace)
	if err := client.Apply(ctx, extractContents(resources)); err != nil {
		return fmt.Errorf("failed to apply hub resources: %w", err)
	}
	if err := client.RolloutRestart(ctx, "portal-hub"); err != nil {
		return fmt.Errorf("failed to restart hub deployment: %w", err)
	}
	return nil
}

// --- hub init ---

type hubInitOpts struct {
	name                  string
	publicAddr            string
	namespace             string
	tunnelPort            int
	egressPort            int
	handshakeSNI          string
	serviceType           string
	certValidity          time.Duration
	deployTimeout         time.Duration
	lbTimeout             time.Duration
	envoyImage            string
	allowUnsupportedEnvoy bool
}

func newHubInitCmd() *cobra.Command {
	var opts hubInitOpts
	cmd := &cobra.Command{
		Use:   "init <context>",
		Short: "Deploy a hub to a cluster and create its certificate authority",
		Long: `Deploys the hub Envoy (shared tunnel listener + egress listener) to the
given cluster and creates the hub CA that signs member certificates. The CA
key is stored under ~/.portal/hubs/<name>/ — protect it.

If --public-addr is omitted, the tunnel Service's LoadBalancer address is
discovered and used as the endpoint members dial.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubInit(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.name, "name", "", "Hub name (default: the context name)")
	cmd.Flags().StringVar(&opts.publicAddr, "public-addr", "", "Public tunnel endpoint (host:port); discovered from LB if omitted")
	cmd.Flags().StringVar(&opts.namespace, "namespace", manifest.DefaultNamespace, "Namespace for hub components")
	cmd.Flags().IntVar(&opts.tunnelPort, "tunnel-port", manifest.DefaultTunnelPort, "Shared tunnel listener port")
	cmd.Flags().IntVar(&opts.egressPort, "egress-port", 0, "Egress listener port (default: 10080)")
	cmd.Flags().StringVar(&opts.handshakeSNI, "handshake-sni", "", "Reserved SNI for reverse tunnel handshakes")
	cmd.Flags().StringVar(&opts.serviceType, "service-type", manifest.DefaultServiceType, "Tunnel Service type (LoadBalancer, NodePort, ClusterIP)")
	cmd.Flags().DurationVar(&opts.certValidity, "cert-validity", 8760*time.Hour, "CA and server certificate validity")
	cmd.Flags().DurationVar(&opts.deployTimeout, "deploy-timeout", 5*time.Minute, "Timeout waiting for deployment readiness")
	cmd.Flags().DurationVar(&opts.lbTimeout, "lb-timeout", 5*time.Minute, "Timeout waiting for LoadBalancer address")
	cmd.Flags().StringVar(&opts.envoyImage, "envoy-image", "", "Envoy proxy image (default: the pinned image)")
	cmd.Flags().BoolVar(&opts.allowUnsupportedEnvoy, "allow-unsupported-envoy", false, "Bypass the Envoy version gate (reverse tunnel APIs are experimental upstream)")
	return cmd
}

func runHubInit(cmd *cobra.Command, kubeContext string, opts hubInitOpts) error {
	if opts.name == "" {
		opts.name = kubeContext
	}
	if err := validate.Name(opts.name); err != nil {
		return fmt.Errorf("invalid hub name: %w", err)
	}
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
	if _, err := store.GetHub(opts.name); err == nil {
		return fmt.Errorf("hub %q already exists", opts.name)
	}

	// Create the hub CA and persist its material.
	dir, err := hubDir(opts.name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create hub directory: %w", err)
	}
	ca, err := certs.NewHubCA(opts.name, opts.certValidity)
	if err != nil {
		return fmt.Errorf("failed to create hub CA: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca.CertPEM(), 0o644); err != nil {
		return fmt.Errorf("failed to write CA certificate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), ca.KeyPEM(), 0o600); err != nil {
		return fmt.Errorf("failed to write CA key: %w", err)
	}
	crlPEM, err := ca.RenderCRL(nil)
	if err != nil {
		return fmt.Errorf("failed to render initial CRL: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "crl.pem"), crlPEM, 0o644); err != nil {
		return fmt.Errorf("failed to write CRL: %w", err)
	}

	handshakeSNI := opts.handshakeSNI
	if handshakeSNI == "" {
		handshakeSNI = envoyDefaultHandshakeSNI
	}

	// Issue the hub server certificate. When the public address is unknown it
	// is discovered from the LoadBalancer and the certificate is re-issued
	// with the address included.
	issueServerCert := func(sans []string) error {
		certPEM, keyPEM, err := ca.IssueHubServerCert(opts.name, sans, opts.certValidity)
		if err != nil {
			return fmt.Errorf("failed to issue hub server certificate: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o644); err != nil {
			return fmt.Errorf("failed to write hub server certificate: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
			return fmt.Errorf("failed to write hub server key: %w", err)
		}
		return nil
	}

	hub := &state.HubState{
		Name:                  opts.name,
		EnvoyImage:            opts.envoyImage,
		AllowUnsupportedEnvoy: opts.allowUnsupportedEnvoy,
		Context:               kubeContext,
		Namespace:             opts.namespace,
		PublicAddr:            opts.publicAddr,
		TunnelPort:            opts.tunnelPort,
		EgressPort:            opts.egressPort,
		HandshakeSNI:          handshakeSNI,
		CADir:                 dir,
		CreatedAt:             time.Now(),
	}

	ctx := context.Background()
	client := newKubeClient(kubeContext, opts.namespace)

	sans := []string{handshakeSNI}
	if opts.publicAddr != "" {
		host, _, err := splitHostPort(opts.publicAddr, opts.tunnelPort)
		if err != nil {
			return fmt.Errorf("invalid --public-addr: %w", err)
		}
		sans = append(sans, host)
	}
	if err := issueServerCert(sans); err != nil {
		return err
	}

	cfg, err := hubDeployConfig(hub)
	if err != nil {
		return err
	}
	cfg.ServiceType = opts.serviceType
	resources, err := manifest.RenderHubManifests(cfg)
	if err != nil {
		return fmt.Errorf("failed to render hub manifests: %w", err)
	}
	if err := client.Apply(ctx, extractContents(resources)); err != nil {
		return fmt.Errorf("failed to apply hub resources: %w", err)
	}

	if opts.publicAddr == "" {
		address, err := client.WaitForServiceAddress(ctx, "portal-hub", opts.lbTimeout)
		if err != nil {
			return fmt.Errorf("failed to discover hub LoadBalancer address: %w", err)
		}
		hub.PublicAddr = fmt.Sprintf("%s:%d", address, opts.tunnelPort)
		// Re-issue the server certificate including the discovered address
		// and patch it into the Secret; SDS hot-reloads it.
		if err := issueServerCert([]string{handshakeSNI, address}); err != nil {
			return err
		}
		certPEM, _ := os.ReadFile(filepath.Join(dir, "tls.crt"))
		keyPEM, _ := os.ReadFile(filepath.Join(dir, "tls.key"))
		if err := client.PatchSecret(ctx, manifest.HubSecretName, map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		}); err != nil {
			return fmt.Errorf("failed to update hub server certificate: %w", err)
		}
	}

	if err := client.WaitForDeployment(ctx, "portal-hub", opts.deployTimeout); err != nil {
		return fmt.Errorf("hub deployment not ready: %w", err)
	}
	if err := store.AddHub(*hub); err != nil {
		return fmt.Errorf("failed to save hub state: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Hub %q deployed to context %q\n", hub.Name, kubeContext)
	fmt.Fprintf(out, "✓ Tunnel endpoint: %s (handshake SNI %s)\n", hub.PublicAddr, handshakeSNI)
	fmt.Fprintf(out, "✓ Hub CA created under %s — protect ca.key\n", dir)
	fmt.Fprintf(out, "\nMembers can now enroll:\n")
	fmt.Fprintf(out, "  portal join <member-ctx> --member <name> --hub-addr %s --hub-name %s\n", hub.PublicAddr, hub.Name)
	return nil
}

// --- hub sign ---

type hubSignOpts struct {
	hub      string
	member   string
	tenant   string
	validity time.Duration
	output   string
	noApply  bool
}

func newHubSignCmd() *cobra.Command {
	var opts hubSignOpts
	cmd := &cobra.Command{
		Use:   "sign <csr-file>",
		Short: "Sign a member's CSR and emit its certificate bundle",
		Long: `Signs a member certificate signing request produced by phase 1 of
'portal join'. The output bundle contains the member's certificate followed
by the hub CA certificate; hand it back to the member owner, who completes
enrollment with 'portal join <ctx> --cert <bundle>'.

The identity granted (--member) is authoritative: any identity claimed inside
the CSR is ignored. By default the hub's egress routing is updated so the new
member is reachable as soon as it connects; --no-apply skips that.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubSign(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.hub, "hub", "", "Hub name (required when multiple hubs exist)")
	cmd.Flags().StringVar(&opts.member, "member", "", "Member name to grant (required)")
	_ = cmd.MarkFlagRequired("member")
	cmd.Flags().StringVar(&opts.tenant, "tenant", "", "Tenant identifier (default: the hub name)")
	cmd.Flags().DurationVar(&opts.validity, "validity", 8760*time.Hour, "Certificate validity")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "Output bundle path (default: <member>-cert.pem)")
	cmd.Flags().BoolVar(&opts.noApply, "no-apply", false, "Skip updating the hub's egress routing")
	return cmd
}

func runHubSign(cmd *cobra.Command, csrPath string, opts hubSignOpts) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	hub, err := loadHub(store, opts.hub)
	if err != nil {
		return err
	}
	if opts.tenant == "" {
		opts.tenant = hub.Name
	}
	if opts.output == "" {
		opts.output = opts.member + "-cert.pem"
	}

	csrPEM, err := os.ReadFile(csrPath)
	if err != nil {
		return fmt.Errorf("failed to read CSR: %w", err)
	}
	ca, err := loadHubCA(hub)
	if err != nil {
		return err
	}
	certPEM, err := ca.SignCSR(csrPEM, certs.MemberIdentity{Member: opts.member, Tenant: opts.tenant}, opts.validity)
	if err != nil {
		return fmt.Errorf("failed to sign CSR: %w", err)
	}
	if err := recordMember(store, hub, opts.member, opts.tenant, certPEM); err != nil {
		return err
	}

	bundle := append(append([]byte{}, certPEM...), ca.CertPEM()...)
	if err := os.WriteFile(opts.output, bundle, 0o644); err != nil {
		return fmt.Errorf("failed to write certificate bundle: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Signed certificate for member %q (bundle: %s)\n", opts.member, opts.output)
	if opts.noApply {
		fmt.Fprintln(out, "⚠ Hub routing not updated (--no-apply); run 'portal hub sign' without it or apply manually")
	} else {
		if err := applyHub(context.Background(), hub); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Hub egress routing updated for %q\n", opts.member)
	}
	fmt.Fprintf(out, "\nSend %s to the member owner; they complete enrollment with:\n", opts.output)
	fmt.Fprintf(out, "  portal join <member-ctx> --member %s --cert %s\n", opts.member, opts.output)
	return nil
}

// recordMember adds or refreshes a member record on the hub from a signed
// certificate, and persists the hub state.
func recordMember(store *state.Store, hub *state.HubState, member, tenant string, certPEM []byte) error {
	serial, err := certs.ParseCertificateSerial(certPEM)
	if err != nil {
		return fmt.Errorf("failed to parse issued certificate: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse issued certificate: %w", err)
	}
	record := state.MemberRecord{
		Name:       member,
		Tenant:     tenant,
		CertSerial: serial.String(),
		CertExpiry: cert.NotAfter,
		JoinedAt:   time.Now(),
	}
	if existing := hub.Member(member); existing != nil {
		record.JoinedAt = existing.JoinedAt
		*existing = record
	} else {
		hub.Members = append(hub.Members, record)
	}
	return store.UpdateHub(*hub)
}

// --- hub invite ---

type hubInviteOpts struct {
	hub      string
	tenant   string
	validity time.Duration
	output   string
	noApply  bool
}

func newHubInviteCmd() *cobra.Command {
	var opts hubInviteOpts
	cmd := &cobra.Command{
		Use:   "invite <member>",
		Short: "Mint a complete member credential (single-operator shortcut)",
		Long: `Generates a member keypair, signs it, and writes a credential file the
member joins with directly: 'portal join <ctx> --credential <file>'.

The credential contains the member's PRIVATE KEY, which this machine
generated — acceptable when one operator runs both sides, but for two-party
enrollment prefer the CSR flow ('portal join --hub-addr' then
'portal hub sign'), which never moves the key.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubInvite(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.hub, "hub", "", "Hub name (required when multiple hubs exist)")
	cmd.Flags().StringVar(&opts.tenant, "tenant", "", "Tenant identifier (default: the hub name)")
	cmd.Flags().DurationVar(&opts.validity, "validity", 8760*time.Hour, "Certificate validity")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "Output credential path (default: <member>.credential)")
	cmd.Flags().BoolVar(&opts.noApply, "no-apply", false, "Skip updating the hub's egress routing")
	return cmd
}

func runHubInvite(cmd *cobra.Command, member string, opts hubInviteOpts) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}
	hub, err := loadHub(store, opts.hub)
	if err != nil {
		return err
	}
	if opts.tenant == "" {
		opts.tenant = hub.Name
	}
	if opts.output == "" {
		opts.output = member + ".credential"
	}

	id := certs.MemberIdentity{Member: member, Tenant: opts.tenant}
	keyPEM, csrPEM, err := certs.GenerateMemberKeyAndCSR(id)
	if err != nil {
		return fmt.Errorf("failed to generate member key: %w", err)
	}
	ca, err := loadHubCA(hub)
	if err != nil {
		return err
	}
	certPEM, err := ca.SignCSR(csrPEM, id, opts.validity)
	if err != nil {
		return fmt.Errorf("failed to sign member certificate: %w", err)
	}
	if err := recordMember(store, hub, member, opts.tenant, certPEM); err != nil {
		return err
	}

	cred := credential{
		Member:       member,
		Hub:          hub.Name,
		HubAddr:      hub.PublicAddr,
		HandshakeSNI: hub.HandshakeSNI,
		Cert:         string(certPEM),
		Key:          string(keyPEM),
		CA:           string(ca.CertPEM()),
	}
	if err := writeCredential(opts.output, cred); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Credential for member %q written to %s (contains the private key — transport securely, delete after join)\n", member, opts.output)
	if !opts.noApply {
		if err := applyHub(context.Background(), hub); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Hub egress routing updated for %q\n", member)
	}
	fmt.Fprintf(out, "\nThe member joins with:\n  portal join <member-ctx> --credential %s\n", opts.output)
	return nil
}

// --- hub evict ---

func newHubEvictCmd() *cobra.Command {
	var hubName string
	cmd := &cobra.Command{
		Use:   "evict <member>",
		Short: "Revoke a member's certificate and remove its routing",
		Long: `Revokes the member's certificate by re-rendering the hub CRL (which
Envoy hot-reloads — no restart of other members' tunnels) and removes the
member from egress routing. Blast radius is the single member.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHubEvict(cmd, args[0], hubName)
		},
	}
	cmd.Flags().StringVar(&hubName, "hub", "", "Hub name (required when multiple hubs exist)")
	return cmd
}

func runHubEvict(cmd *cobra.Command, member, hubName string) error {
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
		return fmt.Errorf("member %q not found on hub %q", member, hub.Name)
	}
	if record.Evicted {
		return fmt.Errorf("member %q is already evicted", member)
	}
	record.Evicted = true

	// Re-render the CRL with every evicted member's serial.
	ca, err := loadHubCA(hub)
	if err != nil {
		return err
	}
	var revoked []certs.RevokedCert
	for _, m := range hub.Members {
		if !m.Evicted {
			continue
		}
		serial, ok := parseSerial(m.CertSerial)
		if !ok {
			return fmt.Errorf("member %q has an invalid stored serial %q", m.Name, m.CertSerial)
		}
		revoked = append(revoked, certs.RevokedCert{Serial: serial})
	}
	crlPEM, err := ca.RenderCRL(revoked)
	if err != nil {
		return fmt.Errorf("failed to render CRL: %w", err)
	}
	if err := os.WriteFile(filepath.Join(hub.CADir, "crl.pem"), crlPEM, 0o644); err != nil {
		return fmt.Errorf("failed to write CRL: %w", err)
	}

	if err := store.UpdateHub(*hub); err != nil {
		return fmt.Errorf("failed to save hub state: %w", err)
	}
	if err := applyHub(context.Background(), hub); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Member %q evicted: certificate revoked (serial %s), egress routing removed\n", member, record.CertSerial)
	fmt.Fprintln(out, "  Existing connections are rejected on the next TLS handshake; other members are unaffected")
	return nil
}
