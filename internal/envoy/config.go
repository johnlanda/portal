// Package envoy renders Envoy bootstrap configurations for Portal tunnel connectivity.
// It provides templates for both the initiator (source cluster) and
// responder (destination cluster) in TCP proxy mode.
package envoy

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
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
