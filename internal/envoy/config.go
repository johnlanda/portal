// Package envoy renders Envoy bootstrap configurations for Portal tunnel connectivity.
// It provides templates for both the initiator (source cluster) and
// responder (destination cluster) in TCP proxy mode.
package envoy

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/johnlanda/portal/internal/validate"
)

const (
	// DefaultTunnelPort is the default port for the responder listener.
	DefaultTunnelPort = 10443

	// DefaultInitiatorListenPort is the default port for the initiator internal listener.
	DefaultInitiatorListenPort = 10443

	// DefaultInitiatorAdminPort is the default admin port for the initiator Envoy.
	DefaultInitiatorAdminPort = 15000

	// DefaultResponderAdminPort is the default admin port for the responder Envoy.
	DefaultResponderAdminPort = 15001

	// DefaultCertPath is the default path where certs are mounted in the container.
	DefaultCertPath = "/etc/portal/certs"
)

// InitiatorConfig configures the initiator (source cluster) Envoy bootstrap.
type InitiatorConfig struct {
	// ResponderHost is the remote responder hostname or IP.
	ResponderHost string
	// ResponderPort is the remote responder port (default: 10443).
	ResponderPort int
	// ListenPort is the local listener port for apps to connect to (default: 10443).
	ListenPort int
	// AdminPort is the Envoy admin port (default: 15000).
	AdminPort int
	// CertPath is the path to the certificate directory (default: /etc/portal/certs).
	CertPath string
	// SNI is the Server Name Indication sent during the TLS handshake (default: ResponderHost).
	SNI string
}

// ResponderConfig configures the responder (destination cluster) Envoy bootstrap.
type ResponderConfig struct {
	// ListenPort is the tunnel listener port (default: 10443).
	ListenPort int
	// AdminPort is the Envoy admin port (default: 15001).
	AdminPort int
	// CertPath is the path to the certificate directory (default: /etc/portal/certs).
	CertPath string
	// BackendHost is the address of the backend service (default: 127.0.0.1).
	BackendHost string
	// BackendPort is the port of the backend service (default: 10001).
	BackendPort int
}

//go:embed templates/initiator_tcp.yaml
var initiatorTCPTemplate string

//go:embed templates/responder_tcp.yaml
var responderTCPTemplate string

//go:embed templates/initiator_multi_tcp.yaml
var initiatorMultiTCPTemplate string

//go:embed templates/responder_multi_tcp.yaml
var responderMultiTCPTemplate string

// ServiceRoute describes a backend routed by SNI on the responder.
type ServiceRoute struct {
	// SNI is the Server Name Indication value used to match this service.
	SNI string
	// BackendHost is the address of the backend service.
	BackendHost string
	// BackendPort is the port of the backend service.
	BackendPort int
	// StatPrefix is the Envoy stats prefix for this service (derived from SNI if empty).
	StatPrefix string
	// ClusterName is the Envoy cluster name (derived from SNI if empty).
	ClusterName string
}

// ResponderMultiServiceConfig configures SNI-based routing to N backends.
type ResponderMultiServiceConfig struct {
	// ListenPort is the tunnel listener port (default: 10443).
	ListenPort int
	// AdminPort is the Envoy admin port (default: 15001).
	AdminPort int
	// CertPath is the path to the certificate directory (default: /etc/portal/certs).
	CertPath string
	// Services is the list of backend services to route to by SNI.
	Services []ServiceRoute
}

// ServiceListener describes a local listener on the initiator.
type ServiceListener struct {
	// Name is the service name (used for listener name and cluster name).
	Name string
	// ListenAddress is the local bind address (default: 0.0.0.0).
	ListenAddress string
	// ListenPort is the local listener port.
	ListenPort int
	// SNI is the Server Name Indication sent during the TLS handshake.
	SNI string
}

// InitiatorMultiServiceConfig configures N local listeners on the initiator.
type InitiatorMultiServiceConfig struct {
	// ResponderHost is the remote responder hostname or IP.
	ResponderHost string
	// ResponderPort is the remote responder port (default: 10443).
	ResponderPort int
	// AdminPort is the Envoy admin port (default: 15000).
	AdminPort int
	// CertPath is the path to the certificate directory (default: /etc/portal/certs).
	CertPath string
	// Services is the list of local listeners, one per service.
	Services []ServiceListener
}

// RenderInitiatorBootstrap renders the TCP proxy initiator bootstrap config.
func RenderInitiatorBootstrap(cfg InitiatorConfig) ([]byte, error) {
	if cfg.ResponderPort == 0 {
		cfg.ResponderPort = DefaultTunnelPort
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = DefaultInitiatorListenPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = DefaultInitiatorAdminPort
	}
	if cfg.CertPath == "" {
		cfg.CertPath = DefaultCertPath
	}
	if cfg.SNI == "" {
		cfg.SNI = cfg.ResponderHost
	}
	return renderTemplate("initiator-tcp", initiatorTCPTemplate, cfg)
}

// RenderResponderBootstrap renders the TCP proxy responder bootstrap config.
func RenderResponderBootstrap(cfg ResponderConfig) ([]byte, error) {
	if cfg.ListenPort == 0 {
		cfg.ListenPort = DefaultTunnelPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = DefaultResponderAdminPort
	}
	if cfg.CertPath == "" {
		cfg.CertPath = DefaultCertPath
	}
	if cfg.BackendHost == "" {
		cfg.BackendHost = "127.0.0.1"
	}
	if cfg.BackendPort == 0 {
		cfg.BackendPort = 10001
	}
	return renderTemplate("responder-tcp", responderTCPTemplate, cfg)
}

// RenderInitiatorMultiBootstrap renders the multi-service TCP proxy initiator bootstrap config.
func RenderInitiatorMultiBootstrap(cfg InitiatorMultiServiceConfig) ([]byte, error) {
	if cfg.ResponderPort == 0 {
		cfg.ResponderPort = DefaultTunnelPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = DefaultInitiatorAdminPort
	}
	if cfg.CertPath == "" {
		cfg.CertPath = DefaultCertPath
	}
	for i := range cfg.Services {
		if cfg.Services[i].ListenAddress == "" {
			cfg.Services[i].ListenAddress = "0.0.0.0"
		}
		if cfg.Services[i].SNI == "" {
			cfg.Services[i].SNI = cfg.Services[i].Name
		}
		if err := validate.DNSName(cfg.Services[i].SNI); err != nil {
			return nil, fmt.Errorf("invalid SNI for service %q: %w", cfg.Services[i].Name, err)
		}
	}
	return renderTemplate("initiator-multi-tcp", initiatorMultiTCPTemplate, cfg)
}

// RenderResponderMultiBootstrap renders the multi-service TCP proxy responder bootstrap config.
func RenderResponderMultiBootstrap(cfg ResponderMultiServiceConfig) ([]byte, error) {
	if cfg.ListenPort == 0 {
		cfg.ListenPort = DefaultTunnelPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = DefaultResponderAdminPort
	}
	if cfg.CertPath == "" {
		cfg.CertPath = DefaultCertPath
	}
	for i := range cfg.Services {
		if err := validate.DNSName(cfg.Services[i].SNI); err != nil {
			return nil, fmt.Errorf("invalid SNI for service route: %w", err)
		}
		if cfg.Services[i].StatPrefix == "" {
			cfg.Services[i].StatPrefix = sanitizeStatPrefix(cfg.Services[i].SNI)
		}
		if cfg.Services[i].ClusterName == "" {
			cfg.Services[i].ClusterName = sanitizeStatPrefix(cfg.Services[i].SNI)
		}
	}
	return renderTemplate("responder-multi-tcp", responderMultiTCPTemplate, cfg)
}

// sanitizeStatPrefix converts a string into a valid Envoy stat prefix
// by replacing non-alphanumeric characters with underscores.
func sanitizeStatPrefix(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func renderTemplate(name, tmplStr string, data interface{}) ([]byte, error) {
	tmpl, err := template.New(name).Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s template: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to render %s template: %w", name, err)
	}
	return buf.Bytes(), nil
}
