// Package manifest renders and manages Kubernetes resource manifests for
// Portal tunnels.
//
// The primary entry point is [Render], which takes a [TunnelConfig] and
// produces a [ManifestBundle] containing all resources for both the source
// (initiator) and destination (responder) clusters. The bundle can be applied
// directly to clusters via [kube.Client] or written to disk with [WriteToDisk]
// for GitOps workflows.
//
// Certificate rotation is handled by [RotateCertificates], which re-issues leaf
// certificates from the persisted CA without regenerating the full PKI.
package manifest

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tetratelabs/portal/internal/certs"
	"github.com/tetratelabs/portal/internal/envoy"
)

const (
	// DefaultNamespace is the default namespace for portal components.
	DefaultNamespace = "portal-system"

	// DefaultTunnelPort is the default responder listen port.
	DefaultTunnelPort = 10443

	// DefaultConnectionCount is the default number of reverse connections.
	DefaultConnectionCount = 4

	// DefaultEnvoyImage is the default Envoy proxy image, pinned by digest.
	DefaultEnvoyImage = "envoyproxy/envoy:v1.37-latest@sha256:d9b4a70739d92b3e28cd407f106b0e90d55df453d7d87773efd22b4429777fe8"

	// DefaultEnvoyLogLevel is the default Envoy log level.
	DefaultEnvoyLogLevel = "info"

	// DefaultServiceType is the default responder Service type.
	DefaultServiceType = "LoadBalancer"

	// certMountPath is where certs are mounted inside the Envoy container.
	certMountPath = "/etc/portal/certs"
)

// ServiceConfig describes a service to be routed through the tunnel.
type ServiceConfig struct {
	// SNI is the Server Name Indication value used for routing (also used as service name).
	SNI string
	// BackendHost is the backend service address (FQDN).
	BackendHost string
	// BackendPort is the backend service port.
	BackendPort int
	// LocalPort is the initiator listener port (0 = use BackendPort).
	LocalPort int
}

// ExternalCertificates holds PEM-encoded certificate material provided externally.
type ExternalCertificates struct {
	// CACert is the PEM-encoded CA certificate.
	CACert []byte
	// InitiatorCert is the PEM-encoded initiator client certificate.
	InitiatorCert []byte
	// InitiatorKey is the PEM-encoded initiator client private key.
	InitiatorKey []byte
	// ResponderCert is the PEM-encoded responder server certificate.
	ResponderCert []byte
	// ResponderKey is the PEM-encoded responder server private key.
	ResponderKey []byte
}

// TunnelConfig contains all parameters needed to render a tunnel's manifests.
type TunnelConfig struct {
	TunnelName         string
	SourceContext      string
	DestinationContext string
	Namespace          string
	ResponderEndpoint  string // IP:port or hostname:port
	TunnelPort         int
	ConnectionCount    int
	CertValidity       time.Duration
	EnvoyImage         string
	EnvoyLogLevel      string
	ServiceType        string // LoadBalancer, NodePort, ClusterIP
	CertDir            string // Use existing certs instead of generating
	CertManager        bool   // Use cert-manager CRDs instead of raw secrets
	Services           []ServiceConfig
	InitiatorCertDir   string                // Separate cert path for initiator
	ResponderCertDir   string                // Separate cert path for responder
	ExternalCerts      *ExternalCertificates // PEM bytes for library API
}

// ManifestBundle contains all rendered Kubernetes resources for both sides of a tunnel.
type ManifestBundle struct {
	Source      []Resource
	Destination []Resource
	Certs       *certs.TunnelCertificates
	Metadata    TunnelMetadata
}

// Resource represents a single Kubernetes resource manifest.
type Resource struct {
	Filename string
	Content  []byte // YAML-encoded
}

// TunnelMetadata stores information about the tunnel for the metadata file.
type TunnelMetadata struct {
	TunnelName         string          `yaml:"tunnelName"`
	SourceContext      string          `yaml:"sourceContext"`
	DestinationContext string          `yaml:"destinationContext"`
	Namespace          string          `yaml:"namespace"`
	TunnelPort         int             `yaml:"tunnelPort"`
	ResponderEndpoint  string          `yaml:"responderEndpoint"`
	EnvoyImage         string          `yaml:"envoyImage"`
	ServiceType        string          `yaml:"serviceType"`
	CreatedAt          time.Time       `yaml:"createdAt"`
	CertValidity       string          `yaml:"certValidity,omitempty"`
	ResponderSANs      []string        `yaml:"responderSANs,omitempty"`
	LastRotatedAt      *time.Time      `yaml:"lastRotatedAt,omitempty"`
	RotationCount      int             `yaml:"rotationCount,omitempty"`
	Services           []ServiceConfig `yaml:"services,omitempty"`
}

// Render generates a complete ManifestBundle for a tunnel.
func Render(cfg TunnelConfig) (*ManifestBundle, error) {
	applyDefaults(&cfg)

	// Parse the responder endpoint.
	host, port, err := parseEndpoint(cfg.ResponderEndpoint, cfg.TunnelPort)
	if err != nil {
		return nil, fmt.Errorf("invalid responder endpoint: %w", err)
	}

	if isLoopback(host) {
		return nil, fmt.Errorf("invalid responder endpoint: loopback address %q cannot be reached from another cluster", host)
	}

	isIP := net.ParseIP(host) != nil

	// Build responder SANs.
	responderSANs := []string{host}

	var initiatorBootstrap, responderBootstrap []byte

	if len(cfg.Services) > 0 {
		// Multi-service mode: use SNI-based routing.
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
			CertPath:      certMountPath,
			Services:      listeners,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render initiator bootstrap: %w", err)
		}

		responderBootstrap, err = envoy.RenderResponderMultiBootstrap(envoy.ResponderMultiServiceConfig{
			ListenPort: cfg.TunnelPort,
			CertPath:   certMountPath,
			Services:   routes,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render responder bootstrap: %w", err)
		}
	} else {
		// Single-service mode: backward-compatible path.
		initiatorBootstrap, err = envoy.RenderInitiatorBootstrap(envoy.InitiatorConfig{
			ResponderHost: host,
			ResponderPort: port,
			ListenPort:    cfg.TunnelPort,
			CertPath:      certMountPath,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render initiator bootstrap: %w", err)
		}

		responderBootstrap, err = envoy.RenderResponderBootstrap(envoy.ResponderConfig{
			ListenPort: cfg.TunnelPort,
			CertPath:   certMountPath,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to render responder bootstrap: %w", err)
		}
	}

	// Build source (initiator) cluster resources.
	var src resourceCollector
	src.add(buildNamespace(cfg.Namespace))
	src.add(buildServiceAccount("portal-initiator", cfg.Namespace))
	src.add(buildConfigMap("portal-initiator-bootstrap", cfg.Namespace, "envoy.yaml", initiatorBootstrap))

	// Build destination (responder) cluster resources.
	var dst resourceCollector
	dst.add(buildNamespace(cfg.Namespace))
	dst.add(buildServiceAccount("portal-responder", cfg.Namespace))
	dst.add(buildConfigMap("portal-responder-bootstrap", cfg.Namespace, "envoy.yaml", responderBootstrap))

	if src.err != nil {
		return nil, fmt.Errorf("failed to build source resources: %w", src.err)
	}
	if dst.err != nil {
		return nil, fmt.Errorf("failed to build destination resources: %w", dst.err)
	}

	var tunnelCerts *certs.TunnelCertificates
	if cfg.CertManager {
		// cert-manager mode: append CRDs instead of raw secrets.
		cmSource, cmDest, cmShared, cmErr := buildCertManagerResources(cfg.TunnelName, cfg.Namespace, cfg.CertValidity, responderSANs)
		if cmErr != nil {
			return nil, fmt.Errorf("failed to build cert-manager resources: %w", cmErr)
		}
		src.resources = append(src.resources, cmShared...)
		src.resources = append(src.resources, cmSource...)
		dst.resources = append(dst.resources, cmShared...)
		dst.resources = append(dst.resources, cmDest...)
	} else if cfg.ExternalCerts != nil {
		// External certificates provided as PEM bytes (library API).
		// Build secrets from provided material; skip CA key storage.
		src.add(buildSecret("portal-tunnel-tls", cfg.Namespace,
			cfg.ExternalCerts.InitiatorCert, cfg.ExternalCerts.InitiatorKey, cfg.ExternalCerts.CACert))
		dst.add(buildSecret("portal-tunnel-tls", cfg.Namespace,
			cfg.ExternalCerts.ResponderCert, cfg.ExternalCerts.ResponderKey, cfg.ExternalCerts.CACert))
	} else if cfg.InitiatorCertDir != "" || cfg.ResponderCertDir != "" {
		// Split cert directories: initiator and responder certs from separate paths.
		initiatorDir := cfg.InitiatorCertDir
		responderDir := cfg.ResponderCertDir
		if initiatorDir == "" || responderDir == "" {
			return nil, fmt.Errorf("both --initiator-cert-dir and --responder-cert-dir must be specified together")
		}
		initCerts, err := loadCertsFromDir(initiatorDir)
		if err != nil {
			return nil, fmt.Errorf("failed to load initiator certificates from %s: %w", initiatorDir, err)
		}
		respCerts, err := loadCertsFromDir(responderDir)
		if err != nil {
			return nil, fmt.Errorf("failed to load responder certificates from %s: %w", responderDir, err)
		}
		src.add(buildSecret("portal-tunnel-tls", cfg.Namespace, initCerts.cert, initCerts.key, initCerts.ca))
		dst.add(buildSecret("portal-tunnel-tls", cfg.Namespace, respCerts.cert, respCerts.key, respCerts.ca))
	} else if cfg.CertDir != "" {
		// Shared cert directory: both sides use certs from one path.
		certFiles, err := loadCertsFromDir(cfg.CertDir)
		if err != nil {
			return nil, fmt.Errorf("failed to load certificates from %s: %w", cfg.CertDir, err)
		}
		// When using a shared cert dir, the same cert/key is used for both sides.
		// This is typically used when certs are pre-generated externally.
		src.add(buildSecret("portal-tunnel-tls", cfg.Namespace, certFiles.cert, certFiles.key, certFiles.ca))
		dst.add(buildSecret("portal-tunnel-tls", cfg.Namespace, certFiles.cert, certFiles.key, certFiles.ca))
	} else {
		// Standard mode: generate certs and build raw secrets.
		tunnelCerts, err = certs.GenerateTunnelCertificates(cfg.TunnelName, responderSANs, cfg.CertValidity)
		if err != nil {
			return nil, fmt.Errorf("failed to generate certificates: %w", err)
		}
		src.add(buildSecret("portal-tunnel-tls", cfg.Namespace, tunnelCerts.InitiatorCert, tunnelCerts.InitiatorKey, tunnelCerts.CACert))
		dst.add(buildSecret("portal-tunnel-tls", cfg.Namespace, tunnelCerts.ResponderCert, tunnelCerts.ResponderKey, tunnelCerts.CACert))
	}

	src.add(buildInitiatorDeployment(cfg))
	src.add(buildInitiatorNetworkPolicy(cfg))
	dst.add(buildResponderDeployment(cfg))
	dst.add(buildResponderService(cfg, host, isIP))
	dst.add(buildResponderNetworkPolicy(cfg))

	if src.err != nil {
		return nil, fmt.Errorf("failed to build source resources: %w", src.err)
	}
	if dst.err != nil {
		return nil, fmt.Errorf("failed to build destination resources: %w", dst.err)
	}

	sourceResources := src.resources
	destResources := dst.resources

	metadata := TunnelMetadata{
		TunnelName:         cfg.TunnelName,
		SourceContext:      cfg.SourceContext,
		DestinationContext: cfg.DestinationContext,
		Namespace:          cfg.Namespace,
		TunnelPort:         cfg.TunnelPort,
		ResponderEndpoint:  cfg.ResponderEndpoint,
		EnvoyImage:         cfg.EnvoyImage,
		ServiceType:        cfg.ServiceType,
		CreatedAt:          time.Now().UTC(),
		CertValidity:       cfg.CertValidity.String(),
		ResponderSANs:      responderSANs,
		Services:           cfg.Services,
	}

	return &ManifestBundle{
		Source:      sourceResources,
		Destination: destResources,
		Certs:       tunnelCerts,
		Metadata:    metadata,
	}, nil
}

func applyDefaults(cfg *TunnelConfig) {
	if cfg.TunnelName == "" {
		cfg.TunnelName = cfg.SourceContext + "--" + cfg.DestinationContext
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.TunnelPort == 0 {
		cfg.TunnelPort = DefaultTunnelPort
	}
	if cfg.ConnectionCount == 0 {
		cfg.ConnectionCount = DefaultConnectionCount
	}
	if cfg.EnvoyImage == "" {
		cfg.EnvoyImage = DefaultEnvoyImage
	}
	if cfg.EnvoyLogLevel == "" {
		cfg.EnvoyLogLevel = DefaultEnvoyLogLevel
	}
	if cfg.ServiceType == "" {
		cfg.ServiceType = DefaultServiceType
	}
	if cfg.CertValidity == 0 {
		cfg.CertValidity = certs.DefaultCertificateValidity
	}
}

func parseEndpoint(endpoint string, defaultPort int) (string, int, error) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		// Try treating the whole thing as just a host (no port).
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

// isLoopback returns true if the given host is a loopback address or "localhost".
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func buildNamespace(name string) (Resource, error) {
	ns := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": name,
		},
	}
	return marshalResource("namespace.yaml", ns)
}

func buildServiceAccount(name, namespace string) (Resource, error) {
	sa := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"automountServiceAccountToken": false,
	}
	return marshalResource(name+"-sa.yaml", sa)
}

func buildConfigMap(name, namespace, key string, data []byte) (Resource, error) {
	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"data": map[string]interface{}{
			key: string(data),
		},
	}
	return marshalResource(name+"-cm.yaml", cm)
}

func buildSecret(name, namespace string, cert, key, ca []byte) (Resource, error) {
	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"type": "Opaque",
		"data": map[string]interface{}{
			"tls.crt": base64.StdEncoding.EncodeToString(cert),
			"tls.key": base64.StdEncoding.EncodeToString(key),
			"ca.crt":  base64.StdEncoding.EncodeToString(ca),
		},
	}
	return marshalResource(name+"-secret.yaml", secret)
}

func buildInitiatorDeployment(cfg TunnelConfig) (Resource, error) {
	// Build container ports.
	var containerPorts []interface{}
	if len(cfg.Services) > 0 {
		for _, svc := range cfg.Services {
			lp := svc.LocalPort
			if lp == 0 {
				lp = svc.BackendPort
			}
			containerPorts = append(containerPorts, map[string]interface{}{
				"name":          fmt.Sprintf("svc-%s", svc.SNI),
				"containerPort": lp,
				"protocol":      "TCP",
			})
		}
	} else {
		containerPorts = append(containerPorts, map[string]interface{}{
			"name":          "tunnel",
			"containerPort": cfg.TunnelPort,
			"protocol":      "TCP",
		})
	}
	containerPorts = append(containerPorts, map[string]interface{}{
		"name":          "admin",
		"containerPort": envoy.DefaultInitiatorAdminPort,
		"protocol":      "TCP",
	})

	dep := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "portal-initiator",
			"namespace": cfg.Namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/name":       "portal-initiator",
				"app.kubernetes.io/component":  "initiator",
				"app.kubernetes.io/part-of":    "portal",
				"portal.tetratelabs.io/tunnel": cfg.TunnelName,
			},
		},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/name": "portal-initiator",
				},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{
						"app.kubernetes.io/name":       "portal-initiator",
						"app.kubernetes.io/component":  "initiator",
						"app.kubernetes.io/part-of":    "portal",
						"portal.tetratelabs.io/tunnel": cfg.TunnelName,
					},
				},
				"spec": map[string]interface{}{
					"serviceAccountName":           "portal-initiator",
					"automountServiceAccountToken": false,
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "envoy",
							"image": cfg.EnvoyImage,
							"args": []interface{}{
								"-c", "/etc/envoy/envoy.yaml",
								"--log-level", cfg.EnvoyLogLevel,
							},
							"ports": containerPorts,
							"volumeMounts": []interface{}{
								map[string]interface{}{
									"name":      "bootstrap",
									"mountPath": "/etc/envoy",
									"readOnly":  true,
								},
								map[string]interface{}{
									"name":      "certs",
									"mountPath": certMountPath,
									"readOnly":  true,
								},
							},
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"cpu":    "100m",
									"memory": "128Mi",
								},
								"limits": map[string]interface{}{
									"cpu":    "500m",
									"memory": "256Mi",
								},
							},
							"securityContext": map[string]interface{}{
								"runAsNonRoot":             true,
								"runAsUser":                1000,
								"readOnlyRootFilesystem":   true,
								"allowPrivilegeEscalation": false,
								"capabilities": map[string]interface{}{
									"drop": []interface{}{"ALL"},
								},
								"seccompProfile": map[string]interface{}{
									"type": "RuntimeDefault",
								},
							},
						},
					},
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "bootstrap",
							"configMap": map[string]interface{}{
								"name": "portal-initiator-bootstrap",
							},
						},
						map[string]interface{}{
							"name": "certs",
							"secret": map[string]interface{}{
								"secretName": "portal-tunnel-tls",
							},
						},
					},
				},
			},
		},
	}
	return marshalResource("portal-initiator-deployment.yaml", dep)
}

func buildResponderDeployment(cfg TunnelConfig) (Resource, error) {
	dep := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "portal-responder",
			"namespace": cfg.Namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/name":       "portal-responder",
				"app.kubernetes.io/component":  "responder",
				"app.kubernetes.io/part-of":    "portal",
				"portal.tetratelabs.io/tunnel": cfg.TunnelName,
			},
		},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/name": "portal-responder",
				},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{
						"app.kubernetes.io/name":       "portal-responder",
						"app.kubernetes.io/component":  "responder",
						"app.kubernetes.io/part-of":    "portal",
						"portal.tetratelabs.io/tunnel": cfg.TunnelName,
					},
				},
				"spec": map[string]interface{}{
					"serviceAccountName":           "portal-responder",
					"automountServiceAccountToken": false,
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "envoy",
							"image": cfg.EnvoyImage,
							"args": []interface{}{
								"-c", "/etc/envoy/envoy.yaml",
								"--log-level", cfg.EnvoyLogLevel,
							},
							"ports": []interface{}{
								map[string]interface{}{
									"name":          "tunnel",
									"containerPort": cfg.TunnelPort,
									"protocol":      "TCP",
								},
								map[string]interface{}{
									"name":          "admin",
									"containerPort": envoy.DefaultResponderAdminPort,
									"protocol":      "TCP",
								},
							},
							"volumeMounts": []interface{}{
								map[string]interface{}{
									"name":      "bootstrap",
									"mountPath": "/etc/envoy",
									"readOnly":  true,
								},
								map[string]interface{}{
									"name":      "certs",
									"mountPath": certMountPath,
									"readOnly":  true,
								},
							},
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"cpu":    "100m",
									"memory": "128Mi",
								},
								"limits": map[string]interface{}{
									"cpu":    "500m",
									"memory": "256Mi",
								},
							},
							"securityContext": map[string]interface{}{
								"runAsNonRoot":             true,
								"runAsUser":                1000,
								"readOnlyRootFilesystem":   true,
								"allowPrivilegeEscalation": false,
								"capabilities": map[string]interface{}{
									"drop": []interface{}{"ALL"},
								},
								"seccompProfile": map[string]interface{}{
									"type": "RuntimeDefault",
								},
							},
						},
					},
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "bootstrap",
							"configMap": map[string]interface{}{
								"name": "portal-responder-bootstrap",
							},
						},
						map[string]interface{}{
							"name": "certs",
							"secret": map[string]interface{}{
								"secretName": "portal-tunnel-tls",
							},
						},
					},
				},
			},
		},
	}
	return marshalResource("portal-responder-deployment.yaml", dep)
}

func buildResponderService(cfg TunnelConfig, host string, isIP bool) (Resource, error) {
	metadata := map[string]interface{}{
		"name":      "portal-responder",
		"namespace": cfg.Namespace,
		"labels": map[string]interface{}{
			"app.kubernetes.io/name":      "portal-responder",
			"app.kubernetes.io/component": "responder",
			"app.kubernetes.io/part-of":   "portal",
		},
	}

	// Add external-dns annotation for hostname endpoints.
	if !isIP && host != "" {
		metadata["annotations"] = map[string]interface{}{
			"external-dns.alpha.kubernetes.io/hostname": host,
		}
	}

	spec := map[string]interface{}{
		"type": cfg.ServiceType,
		"selector": map[string]interface{}{
			"app.kubernetes.io/name": "portal-responder",
		},
		"ports": []interface{}{
			map[string]interface{}{
				"name":       "tunnel",
				"port":       cfg.TunnelPort,
				"targetPort": cfg.TunnelPort,
				"protocol":   "TCP",
			},
		},
	}

	// Set loadBalancerIP for IP endpoints.
	if isIP && host != "" {
		spec["loadBalancerIP"] = host
	}

	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   metadata,
		"spec":       spec,
	}
	return marshalResource("portal-responder-service.yaml", svc)
}

func buildInitiatorNetworkPolicy(cfg TunnelConfig) (Resource, error) {
	// Build ingress ports: one per service listener, or the tunnel port for single-service.
	var ingressPorts []interface{}
	if len(cfg.Services) > 0 {
		for _, svc := range cfg.Services {
			lp := svc.LocalPort
			if lp == 0 {
				lp = svc.BackendPort
			}
			ingressPorts = append(ingressPorts, map[string]interface{}{
				"protocol": "TCP",
				"port":     lp,
			})
		}
	} else {
		ingressPorts = append(ingressPorts, map[string]interface{}{
			"protocol": "TCP",
			"port":     cfg.TunnelPort,
		})
	}

	np := map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]interface{}{
			"name":      "portal-initiator",
			"namespace": cfg.Namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/name":      "portal-initiator",
				"app.kubernetes.io/component": "initiator",
				"app.kubernetes.io/part-of":   "portal",
			},
		},
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/name": "portal-initiator",
				},
			},
			"policyTypes": []interface{}{"Ingress", "Egress"},
			"ingress": []interface{}{
				map[string]interface{}{
					"from": []interface{}{
						map[string]interface{}{
							"namespaceSelector": map[string]interface{}{},
						},
					},
					"ports": ingressPorts,
				},
			},
			"egress": []interface{}{
				// Allow DNS.
				map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"protocol": "UDP",
							"port":     53,
						},
					},
				},
				// Allow tunnel connection to responder.
				map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"protocol": "TCP",
							"port":     cfg.TunnelPort,
						},
					},
				},
			},
		},
	}
	return marshalResource("portal-initiator-networkpolicy.yaml", np)
}

func buildResponderNetworkPolicy(cfg TunnelConfig) (Resource, error) {
	np := map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]interface{}{
			"name":      "portal-responder",
			"namespace": cfg.Namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/name":      "portal-responder",
				"app.kubernetes.io/component": "responder",
				"app.kubernetes.io/part-of":   "portal",
			},
		},
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/name": "portal-responder",
				},
			},
			"policyTypes": []interface{}{"Ingress", "Egress"},
			"ingress": []interface{}{
				map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"protocol": "TCP",
							"port":     cfg.TunnelPort,
						},
					},
				},
			},
			"egress": []interface{}{
				// Allow DNS.
				map[string]interface{}{
					"ports": []interface{}{
						map[string]interface{}{
							"protocol": "UDP",
							"port":     53,
						},
					},
				},
				// Allow to any in-cluster service (local backend forwarding).
				map[string]interface{}{},
			},
		},
	}
	return marshalResource("portal-responder-networkpolicy.yaml", np)
}

func marshalResource(filename string, obj map[string]interface{}) (Resource, error) {
	data, err := yaml.Marshal(obj)
	if err != nil {
		return Resource{}, fmt.Errorf("failed to marshal %s: %w", filename, err)
	}
	return Resource{
		Filename: filename,
		Content:  data,
	}, nil
}

// resourceCollector accumulates resources and captures the first error.
type resourceCollector struct {
	resources []Resource
	err       error
}

func (c *resourceCollector) add(r Resource, err error) {
	if c.err != nil {
		return
	}
	if err != nil {
		c.err = err
		return
	}
	c.resources = append(c.resources, r)
}

// certBundle holds PEM-encoded cert material loaded from a directory.
type certBundle struct {
	cert []byte
	key  []byte
	ca   []byte
}

// loadCertsFromDir reads tls.crt, tls.key, and ca.crt from the given directory.
func loadCertsFromDir(dir string) (*certBundle, error) {
	cert, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		return nil, fmt.Errorf("failed to read tls.crt: %w", err)
	}
	key, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, fmt.Errorf("failed to read tls.key: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("failed to read ca.crt: %w", err)
	}
	return &certBundle{cert: cert, key: key, ca: ca}, nil
}
