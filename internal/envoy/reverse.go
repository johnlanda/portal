package envoy

import (
	_ "embed"
	"fmt"
	"time"

	"github.com/johnlanda/portal/internal/validate"
)

const (
	// DefaultHandshakeSNI is the reserved SNI that selects the reverse tunnel
	// handshake filter chain on the hub's shared listener. Published forward
	// services must not use this value.
	DefaultHandshakeSNI = "reverse-tunnel.portal"

	// DefaultReverseConnectionCount is the default number of persistent
	// reverse connections a member maintains to the hub.
	DefaultReverseConnectionCount = 4

	// DefaultHubEgressPort is the default port for the hub's egress listener,
	// where hub-side services address members through the reverse tunnel.
	DefaultHubEgressPort = 10080

	// DefaultPingInterval is the default health-check ping interval for
	// established reverse tunnel connections.
	DefaultPingInterval = 2 * time.Second

	// DefaultCleanupInterval is the default interval after which unused
	// dynamic reverse-connection hosts are removed on the hub.
	DefaultCleanupInterval = 60 * time.Second
)

// PublishedService describes a member-local service that hub-originated
// requests may reach over the reverse tunnel, addressed by the canonical
// authority <Name>.<MemberName>.
type PublishedService struct {
	// Name is the service name (DNS label); forms the canonical authority.
	Name string
	// BackendHost is the address of the local backend service.
	BackendHost string
	// BackendPort is the port of the local backend service.
	BackendPort int
	// Protocol is "http" (default) or "grpc". The reverse path is HTTP/2-only;
	// "tcp" is rejected.
	Protocol string
}

// MemberConfig configures the member (egress-only cluster) Envoy bootstrap.
type MemberConfig struct {
	// MemberName is the member's identity: the reverse tunnel cluster-id and
	// the DNS SAN its certificate must carry.
	MemberName string
	// HubName is the hub's name, used as the reverse tunnel tenant-id.
	HubName string
	// NodeID uniquely identifies this Envoy instance (default: MemberName).
	NodeID string
	// HubHost is the hub tunnel endpoint hostname or IP.
	HubHost string
	// HubPort is the hub tunnel endpoint port (default: 10443).
	HubPort int
	// ConnectionCount is the number of reverse connections to maintain (default: 4).
	ConnectionCount int
	// AdminPort is the Envoy admin port (default: 15000).
	AdminPort int
	// CertPath is the path to the certificate directory (default: /etc/portal/certs).
	CertPath string
	// HandshakeSNI is the SNI used for reverse tunnel establishment
	// (default: DefaultHandshakeSNI). Must match the hub's configuration.
	HandshakeSNI string
	// Published lists local services the hub may reach over the reverse tunnel.
	Published []PublishedService
	// Forward lists optional v1-style forward listeners for local apps calling
	// hub-side services over the same tunnel endpoint.
	Forward []ServiceListener
}

// HubConfig configures the hub (ingress-capable cluster) Envoy bootstrap.
type HubConfig struct {
	// ListenPort is the shared public tunnel listener port (default: 10443).
	ListenPort int
	// EgressPort is the egress listener port for hub-originated requests
	// (default: 10080).
	EgressPort int
	// AdminPort is the Envoy admin port (default: 15001).
	AdminPort int
	// CertPath is the path to the certificate directory (default: /etc/portal/certs).
	CertPath string
	// HandshakeSNI is the reserved SNI selecting the reverse tunnel handshake
	// chain (default: DefaultHandshakeSNI).
	HandshakeSNI string
	// PingInterval is the reverse connection health-check interval (default: 2s).
	PingInterval time.Duration
	// CleanupInterval is the unused-host cleanup interval for the reverse
	// connection cluster (default: 60s).
	CleanupInterval time.Duration
	// EnableCRL adds a CRL to certificate validation for member eviction.
	// When set, a CRL file must exist at <CertPath>/crl.pem or Envoy will
	// refuse the configuration; render one with the certs package (an empty
	// revocation set is fine) before enabling.
	EnableCRL bool
	// Members lists member names routable via the egress listener.
	Members []string
	// Routes lists alias authorities minted for member services.
	Routes []HubRouteAlias
	// Services lists hub-local backends reachable by members over the v1
	// forward SNI path, sharing the tunnel listener.
	Services []ServiceRoute
}

// HubRouteAlias maps a friendly hub-side authority (an alias Service name)
// to a member service. The egress listener rewrites the authority to the
// canonical <Service>.<Member> form before forwarding, so member route
// configs never depend on hub-side alias naming.
type HubRouteAlias struct {
	// Member is the member name.
	Member string
	// Service is the published service name on the member.
	Service string
	// Alias is the alias authority (e.g. inference-acme-prod); requests to
	// <Alias> or <Alias>.* are routed to the member.
	Alias string
}

//go:embed templates/member_reverse.yaml
var memberReverseTemplate string

//go:embed templates/hub.yaml
var hubTemplate string

// RenderMemberBootstrap renders the reverse tunnel member bootstrap config.
func RenderMemberBootstrap(cfg MemberConfig) ([]byte, error) {
	if err := validate.DNSName(cfg.MemberName); err != nil {
		return nil, fmt.Errorf("invalid member name: %w", err)
	}
	if err := validate.Name(cfg.HubName); err != nil {
		return nil, fmt.Errorf("invalid hub name: %w", err)
	}
	if cfg.HubHost == "" {
		return nil, fmt.Errorf("hub host must not be empty")
	}
	if cfg.NodeID == "" {
		cfg.NodeID = cfg.MemberName
	}
	if err := validate.Name(cfg.NodeID); err != nil {
		return nil, fmt.Errorf("invalid node ID: %w", err)
	}
	if cfg.HubPort == 0 {
		cfg.HubPort = DefaultTunnelPort
	}
	if cfg.ConnectionCount == 0 {
		cfg.ConnectionCount = DefaultReverseConnectionCount
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = DefaultInitiatorAdminPort
	}
	if cfg.CertPath == "" {
		cfg.CertPath = DefaultCertPath
	}
	if cfg.HandshakeSNI == "" {
		cfg.HandshakeSNI = DefaultHandshakeSNI
	}
	if err := validate.DNSName(cfg.HandshakeSNI); err != nil {
		return nil, fmt.Errorf("invalid handshake SNI: %w", err)
	}
	for i := range cfg.Published {
		svc := &cfg.Published[i]
		if err := validate.DNSName(svc.Name); err != nil {
			return nil, fmt.Errorf("invalid published service name %q: %w", svc.Name, err)
		}
		if svc.BackendHost == "" {
			return nil, fmt.Errorf("published service %q: backend host must not be empty", svc.Name)
		}
		if svc.BackendPort == 0 {
			return nil, fmt.Errorf("published service %q: backend port must not be zero", svc.Name)
		}
		switch svc.Protocol {
		case "":
			svc.Protocol = "http"
		case "http", "grpc":
		case "tcp":
			return nil, fmt.Errorf("published service %q: the reverse tunnel path is HTTP/2-only and cannot carry raw TCP; use the forward path or a gRPC/HTTP interface", svc.Name)
		default:
			return nil, fmt.Errorf("published service %q: unsupported protocol %q (supported: http, grpc)", svc.Name, svc.Protocol)
		}
	}
	for i := range cfg.Forward {
		if cfg.Forward[i].ListenAddress == "" {
			cfg.Forward[i].ListenAddress = "0.0.0.0"
		}
		if cfg.Forward[i].SNI == "" {
			cfg.Forward[i].SNI = cfg.Forward[i].Name
		}
		if err := validate.DNSName(cfg.Forward[i].SNI); err != nil {
			return nil, fmt.Errorf("invalid SNI for forward service %q: %w", cfg.Forward[i].Name, err)
		}
		if cfg.Forward[i].SNI == cfg.HandshakeSNI {
			return nil, fmt.Errorf("forward service %q: SNI %q is reserved for the reverse tunnel handshake", cfg.Forward[i].Name, cfg.HandshakeSNI)
		}
	}
	return renderTemplate("member-reverse", memberReverseTemplate, cfg)
}

// RenderHubBootstrap renders the reverse tunnel hub bootstrap config.
func RenderHubBootstrap(cfg HubConfig) ([]byte, error) {
	if cfg.ListenPort == 0 {
		cfg.ListenPort = DefaultTunnelPort
	}
	if cfg.EgressPort == 0 {
		cfg.EgressPort = DefaultHubEgressPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = DefaultResponderAdminPort
	}
	if cfg.CertPath == "" {
		cfg.CertPath = DefaultCertPath
	}
	if cfg.HandshakeSNI == "" {
		cfg.HandshakeSNI = DefaultHandshakeSNI
	}
	if err := validate.DNSName(cfg.HandshakeSNI); err != nil {
		return nil, fmt.Errorf("invalid handshake SNI: %w", err)
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = DefaultPingInterval
	}
	if cfg.CleanupInterval == 0 {
		cfg.CleanupInterval = DefaultCleanupInterval
	}
	for _, m := range cfg.Members {
		if err := validate.DNSName(m); err != nil {
			return nil, fmt.Errorf("invalid member name %q: %w", m, err)
		}
	}
	for _, r := range cfg.Routes {
		if err := validate.DNSName(r.Member); err != nil {
			return nil, fmt.Errorf("invalid route member %q: %w", r.Member, err)
		}
		if err := validate.DNSName(r.Service); err != nil {
			return nil, fmt.Errorf("invalid route service %q: %w", r.Service, err)
		}
		if err := validate.DNSName(r.Alias); err != nil {
			return nil, fmt.Errorf("invalid route alias %q: %w", r.Alias, err)
		}
	}
	for i := range cfg.Services {
		if err := validate.DNSName(cfg.Services[i].SNI); err != nil {
			return nil, fmt.Errorf("invalid SNI for service route: %w", err)
		}
		if cfg.Services[i].SNI == cfg.HandshakeSNI {
			return nil, fmt.Errorf("service route SNI %q is reserved for the reverse tunnel handshake", cfg.HandshakeSNI)
		}
		if cfg.Services[i].StatPrefix == "" {
			cfg.Services[i].StatPrefix = sanitizeStatPrefix(cfg.Services[i].SNI)
		}
		if cfg.Services[i].ClusterName == "" {
			cfg.Services[i].ClusterName = sanitizeStatPrefix(cfg.Services[i].SNI)
		}
	}
	data := hubTemplateData{
		HubConfig:       cfg,
		PingInterval:    protoDuration(cfg.PingInterval),
		CleanupInterval: protoDuration(cfg.CleanupInterval),
	}
	return renderTemplate("hub", hubTemplate, data)
}

// hubTemplateData wraps HubConfig with durations pre-formatted as protobuf
// duration strings; the string fields shadow the embedded time.Duration
// fields, whose native formatting (e.g. "1m0s") Envoy would reject.
type hubTemplateData struct {
	HubConfig
	PingInterval    string
	CleanupInterval string
}

// protoDuration formats d as a protobuf JSON duration string ("60s", "1.5s").
func protoDuration(d time.Duration) string {
	return fmt.Sprintf("%gs", d.Seconds())
}
