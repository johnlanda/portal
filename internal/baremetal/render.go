// Package baremetal renders deployment artifacts for bare metal / VM
// tunnel deployments. It reuses the same Envoy bootstrap and certificate
// generation as the Kubernetes path but produces raw config files, systemd
// units, and docker-compose files instead of K8s manifests.
package baremetal

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johnlanda/portal/internal/certs"
	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/manifest"
)

const (
	// DefaultCertInstallPath is where certs are placed on the bare metal host.
	DefaultCertInstallPath = "/etc/portal/certs"

	// DefaultConfigInstallPath is where Envoy configs are placed.
	DefaultConfigInstallPath = "/etc/portal"

	// DefaultRunUser is the OS user for running Envoy.
	DefaultRunUser = "portal"

	// DefaultEnvoyCommand is the default command to run Envoy.
	DefaultEnvoyCommand = "envoy"

	// DefaultEnvoyImage is the default Envoy Docker image for compose mode.
	DefaultEnvoyImage = "envoyproxy/envoy:v1.37-latest"
)

// BareMetalConfig contains all parameters needed to render bare metal tunnel artifacts.
type BareMetalConfig struct {
	// TunnelName is an explicit name for the tunnel (auto-derived if empty).
	TunnelName string

	// SourceHost is the identifier for the source (initiator) host.
	SourceHost string
	// DestinationHost is the identifier for the destination (responder) host.
	DestinationHost string

	// ResponderEndpoint is the IP:port or hostname:port where the responder listens.
	ResponderEndpoint string
	// TunnelPort is the responder listen port (default: 10443).
	TunnelPort int

	// CertValidity is the certificate validity duration.
	CertValidity time.Duration

	// Services describes multi-service SNI-based routing.
	Services []manifest.ServiceConfig

	// EnvoyCommand is the command used to run Envoy (default: "envoy").
	EnvoyCommand string
	// CertInstallPath is where certs will be installed on the host (default: /etc/portal/certs).
	CertInstallPath string
	// ConfigInstallPath is where Envoy config will be installed (default: /etc/portal).
	ConfigInstallPath string
	// RunUser is the OS user for running Envoy (default: "portal").
	RunUser string

	// EnvoyImage is the Docker image for docker-compose mode.
	EnvoyImage string
	// EnvoyLogLevel is the Envoy log level (default: "info").
	EnvoyLogLevel string

	// Certificate source fields — mutually exclusive with auto-generation.
	CertDir          string // Shared cert directory for both sides.
	InitiatorCertDir string // Separate cert path for the initiator.
	ResponderCertDir string // Separate cert path for the responder.
	ExternalCerts    *manifest.ExternalCertificates
}

// BareMetalBundle contains all rendered artifacts for both sides of a bare metal tunnel.
type BareMetalBundle struct {
	Initiator BareMetalSide
	Responder BareMetalSide
	Certs     *certs.TunnelCertificates // nil when using external certs
	Metadata  BareMetalMetadata
}

// BareMetalSide holds the rendered artifacts for one side of the tunnel.
type BareMetalSide struct {
	EnvoyConfig    []byte
	SystemdUnit    []byte
	DockerCompose  []byte
	CertFiles      CertFileSet
}

// CertFileSet holds the PEM-encoded cert files for one side.
type CertFileSet struct {
	Cert []byte // tls.crt
	Key  []byte // tls.key
	CA   []byte // ca.crt
}

// BareMetalMetadata stores information about the tunnel for the metadata file.
type BareMetalMetadata struct {
	TunnelName        string                 `yaml:"tunnelName"`
	DeployTarget      string                 `yaml:"deployTarget"`
	SourceHost        string                 `yaml:"sourceHost"`
	DestinationHost   string                 `yaml:"destinationHost"`
	TunnelPort        int                    `yaml:"tunnelPort"`
	ResponderEndpoint string                 `yaml:"responderEndpoint"`
	EnvoyCommand      string                 `yaml:"envoyCommand"`
	CertInstallPath   string                 `yaml:"certInstallPath"`
	ConfigInstallPath string                 `yaml:"configInstallPath"`
	EnvoyLogLevel     string                 `yaml:"envoyLogLevel"`
	CreatedAt         time.Time              `yaml:"createdAt"`
	CertValidity      string                 `yaml:"certValidity,omitempty"`
	ResponderSANs     []string               `yaml:"responderSANs,omitempty"`
	Services          []manifest.ServiceConfig `yaml:"services,omitempty"`
}

// Render generates a complete BareMetalBundle for a tunnel.
func Render(cfg BareMetalConfig) (*BareMetalBundle, error) {
	applyDefaults(&cfg)

	host, port, err := parseEndpoint(cfg.ResponderEndpoint, cfg.TunnelPort)
	if err != nil {
		return nil, fmt.Errorf("invalid responder endpoint: %w", err)
	}

	responderSANs := []string{host}

	certPath := cfg.CertInstallPath

	var initiatorBootstrap, responderBootstrap []byte

	if len(cfg.Services) > 0 {
		var routes []envoy.ServiceRoute
		var listeners []envoy.ServiceListener
		for _, svc := range cfg.Services {
			lp := svc.LocalPort
			if lp == 0 {
				lp = svc.BackendPort
			}
			routes = append(routes, envoy.ServiceRoute{
				SNI:         svc.SNI,
				BackendHost: svc.BackendHost,
				BackendPort: svc.BackendPort,
			})
			listeners = append(listeners, envoy.ServiceListener{
				Name:       svc.SNI,
				ListenPort: lp,
				SNI:        svc.SNI,
			})
		}

		initiatorBootstrap, err = envoy.RenderInitiatorMultiBootstrap(envoy.InitiatorMultiServiceConfig{
			ResponderHost: host,
			ResponderPort: port,
			CertPath:      certPath,
			Services:      listeners,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render initiator bootstrap: %w", err)
		}

		responderBootstrap, err = envoy.RenderResponderMultiBootstrap(envoy.ResponderMultiServiceConfig{
			ListenPort: cfg.TunnelPort,
			CertPath:   certPath,
			Services:   routes,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render responder bootstrap: %w", err)
		}
	} else {
		initiatorBootstrap, err = envoy.RenderInitiatorBootstrap(envoy.InitiatorConfig{
			ResponderHost: host,
			ResponderPort: port,
			ListenPort:    cfg.TunnelPort,
			CertPath:      certPath,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render initiator bootstrap: %w", err)
		}

		responderBootstrap, err = envoy.RenderResponderBootstrap(envoy.ResponderConfig{
			ListenPort: cfg.TunnelPort,
			CertPath:   certPath,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render responder bootstrap: %w", err)
		}
	}

	// Generate or load certificates.
	var tunnelCerts *certs.TunnelCertificates
	var initiatorCertFiles, responderCertFiles CertFileSet

	if cfg.ExternalCerts != nil {
		initiatorCertFiles = CertFileSet{
			Cert: cfg.ExternalCerts.InitiatorCert,
			Key:  cfg.ExternalCerts.InitiatorKey,
			CA:   cfg.ExternalCerts.CACert,
		}
		responderCertFiles = CertFileSet{
			Cert: cfg.ExternalCerts.ResponderCert,
			Key:  cfg.ExternalCerts.ResponderKey,
			CA:   cfg.ExternalCerts.CACert,
		}
	} else if cfg.InitiatorCertDir != "" || cfg.ResponderCertDir != "" {
		if cfg.InitiatorCertDir == "" || cfg.ResponderCertDir == "" {
			return nil, fmt.Errorf("both initiator-cert-dir and responder-cert-dir must be specified together")
		}
		initCerts, err := loadCertsFromDir(cfg.InitiatorCertDir)
		if err != nil {
			return nil, fmt.Errorf("failed to load initiator certificates from %s: %w", cfg.InitiatorCertDir, err)
		}
		respCerts, err := loadCertsFromDir(cfg.ResponderCertDir)
		if err != nil {
			return nil, fmt.Errorf("failed to load responder certificates from %s: %w", cfg.ResponderCertDir, err)
		}
		initiatorCertFiles = *initCerts
		responderCertFiles = *respCerts
	} else if cfg.CertDir != "" {
		certFiles, err := loadCertsFromDir(cfg.CertDir)
		if err != nil {
			return nil, fmt.Errorf("failed to load certificates from %s: %w", cfg.CertDir, err)
		}
		initiatorCertFiles = *certFiles
		responderCertFiles = *certFiles
	} else {
		tunnelCerts, err = certs.GenerateTunnelCertificates(cfg.TunnelName, responderSANs, cfg.CertValidity)
		if err != nil {
			return nil, fmt.Errorf("failed to generate certificates: %w", err)
		}
		initiatorCertFiles = CertFileSet{
			Cert: tunnelCerts.InitiatorCert,
			Key:  tunnelCerts.InitiatorKey,
			CA:   tunnelCerts.CACert,
		}
		responderCertFiles = CertFileSet{
			Cert: tunnelCerts.ResponderCert,
			Key:  tunnelCerts.ResponderKey,
			CA:   tunnelCerts.CACert,
		}
	}

	// Render systemd units.
	initiatorSystemd, err := renderSystemdUnit(systemdConfig{
		Description:   fmt.Sprintf("Portal Initiator - %s", cfg.TunnelName),
		UnitName:      "portal-initiator",
		EnvoyCommand:  cfg.EnvoyCommand,
		ConfigPath:    cfg.ConfigInstallPath + "/envoy.yaml",
		EnvoyLogLevel: cfg.EnvoyLogLevel,
		RunUser:       cfg.RunUser,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render initiator systemd unit: %w", err)
	}

	responderSystemd, err := renderSystemdUnit(systemdConfig{
		Description:   fmt.Sprintf("Portal Responder - %s", cfg.TunnelName),
		UnitName:      "portal-responder",
		EnvoyCommand:  cfg.EnvoyCommand,
		ConfigPath:    cfg.ConfigInstallPath + "/envoy.yaml",
		EnvoyLogLevel: cfg.EnvoyLogLevel,
		RunUser:       cfg.RunUser,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render responder systemd unit: %w", err)
	}

	// Render docker-compose files.
	initiatorCompose, err := renderDockerCompose(composeConfig{
		ServiceName:    "portal-initiator",
		EnvoyImage:     cfg.EnvoyImage,
		EnvoyLogLevel:  cfg.EnvoyLogLevel,
		ConfigPath:     cfg.ConfigInstallPath,
		CertPath:       cfg.CertInstallPath,
		TunnelPort:     cfg.TunnelPort,
		Services:       cfg.Services,
		IsInitiator:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render initiator docker-compose: %w", err)
	}

	responderCompose, err := renderDockerCompose(composeConfig{
		ServiceName:    "portal-responder",
		EnvoyImage:     cfg.EnvoyImage,
		EnvoyLogLevel:  cfg.EnvoyLogLevel,
		ConfigPath:     cfg.ConfigInstallPath,
		CertPath:       cfg.CertInstallPath,
		TunnelPort:     cfg.TunnelPort,
		Services:       cfg.Services,
		IsInitiator:    false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render responder docker-compose: %w", err)
	}

	metadata := BareMetalMetadata{
		TunnelName:        cfg.TunnelName,
		DeployTarget:      "bare-metal",
		SourceHost:        cfg.SourceHost,
		DestinationHost:   cfg.DestinationHost,
		TunnelPort:        cfg.TunnelPort,
		ResponderEndpoint: cfg.ResponderEndpoint,
		EnvoyCommand:      cfg.EnvoyCommand,
		CertInstallPath:   cfg.CertInstallPath,
		ConfigInstallPath: cfg.ConfigInstallPath,
		EnvoyLogLevel:     cfg.EnvoyLogLevel,
		CreatedAt:         time.Now().UTC(),
		CertValidity:      cfg.CertValidity.String(),
		ResponderSANs:     responderSANs,
		Services:          cfg.Services,
	}

	return &BareMetalBundle{
		Initiator: BareMetalSide{
			EnvoyConfig:   initiatorBootstrap,
			SystemdUnit:   initiatorSystemd,
			DockerCompose: initiatorCompose,
			CertFiles:     initiatorCertFiles,
		},
		Responder: BareMetalSide{
			EnvoyConfig:   responderBootstrap,
			SystemdUnit:   responderSystemd,
			DockerCompose: responderCompose,
			CertFiles:     responderCertFiles,
		},
		Certs:    tunnelCerts,
		Metadata: metadata,
	}, nil
}

// MarshalMetadata serializes the metadata to YAML.
func MarshalMetadata(meta BareMetalMetadata) ([]byte, error) {
	return yaml.Marshal(meta)
}

func applyDefaults(cfg *BareMetalConfig) {
	if cfg.TunnelName == "" {
		cfg.TunnelName = cfg.SourceHost + "--" + cfg.DestinationHost
	}
	if cfg.TunnelPort == 0 {
		cfg.TunnelPort = envoy.DefaultTunnelPort
	}
	if cfg.CertValidity == 0 {
		cfg.CertValidity = certs.DefaultCertificateValidity
	}
	if cfg.EnvoyCommand == "" {
		cfg.EnvoyCommand = DefaultEnvoyCommand
	}
	if cfg.CertInstallPath == "" {
		cfg.CertInstallPath = DefaultCertInstallPath
	}
	if cfg.ConfigInstallPath == "" {
		cfg.ConfigInstallPath = DefaultConfigInstallPath
	}
	if cfg.RunUser == "" {
		cfg.RunUser = DefaultRunUser
	}
	if cfg.EnvoyImage == "" {
		cfg.EnvoyImage = DefaultEnvoyImage
	}
	if cfg.EnvoyLogLevel == "" {
		cfg.EnvoyLogLevel = "info"
	}
}

func parseEndpoint(endpoint string, defaultPort int) (string, int, error) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		if net.ParseIP(endpoint) != nil {
			return endpoint, defaultPort, nil
		}
		return "", 0, fmt.Errorf("cannot parse %q: %w", endpoint, err)
	}

	port := defaultPort
	if portStr != "" {
		_, err := fmt.Sscanf(portStr, "%d", &port)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
		}
	}

	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port %d out of valid range 1-65535", port)
	}

	return host, port, nil
}

func loadCertsFromDir(dir string) (*CertFileSet, error) {
	cert, err := readFile(dir, "tls.crt")
	if err != nil {
		return nil, err
	}
	key, err := readFile(dir, "tls.key")
	if err != nil {
		return nil, err
	}
	ca, err := readFile(dir, "ca.crt")
	if err != nil {
		return nil, err
	}
	return &CertFileSet{Cert: cert, Key: key, CA: ca}, nil
}

func readFile(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", name, err)
	}
	return data, nil
}
