package manifest

import (
	"encoding/base64"
	"fmt"

	"github.com/johnlanda/portal/internal/envoy"
	"github.com/johnlanda/portal/internal/validate"
)

// v2 hub/member manifest rendering (docs/v2-proposal.md). Unlike v1 tunnel
// rendering, hub and member bundles are rendered independently — the two
// sides are typically operated by different parties.

const (
	// HubSecretName is the Secret holding the hub's TLS material.
	HubSecretName = "portal-hub-tls"

	// MemberSecretName is the Secret holding a member's TLS material. In the
	// two-phase enrollment flow the private key is written here at phase 1
	// and the certificate is added at phase 2.
	MemberSecretName = "portal-member-tls"
)

// HubDeployConfig describes a hub deployment.
type HubDeployConfig struct {
	// HubName is the hub name.
	HubName string
	// Namespace for hub components (default: portal-system).
	Namespace string
	// TunnelPort is the shared public listener port (default: 10443).
	TunnelPort int
	// EgressPort is the egress listener port (default: 10080).
	EgressPort int
	// HandshakeSNI selects the reverse tunnel handshake chain.
	HandshakeSNI string
	// ServiceType for the public tunnel Service (default: LoadBalancer).
	ServiceType string
	// EnvoyImage and EnvoyLogLevel configure the proxy container.
	EnvoyImage    string
	EnvoyLogLevel string
	// Members lists member names routable via the egress listener.
	Members []string
	// Routes lists alias authorities for member services; each also gets a
	// ClusterIP alias Service pointing at the egress listener.
	Routes []envoy.HubRouteAlias
	// Services are hub-local backends published to members (forward path).
	Services []ServiceConfig
	// EnableCRL wires the CRL into certificate validation; CRLPEM must be set.
	EnableCRL bool
	// AllowUnsupportedEnvoy bypasses the Envoy version gate.
	AllowUnsupportedEnvoy bool
	// TLS material for the hub Secret.
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
	CRLPEM  []byte
}

// RenderHubManifests renders the complete resource set for a hub.
func RenderHubManifests(cfg HubDeployConfig) ([]Resource, error) {
	if err := validate.Name(cfg.HubName); err != nil {
		return nil, fmt.Errorf("invalid hub name: %w", err)
	}
	applyHubDefaults(&cfg)
	if err := CheckEnvoyImage(cfg.EnvoyImage, cfg.AllowUnsupportedEnvoy); err != nil {
		return nil, err
	}
	if cfg.EnableCRL && len(cfg.CRLPEM) == 0 {
		return nil, fmt.Errorf("EnableCRL requires CRLPEM (render one with an empty revocation set)")
	}
	if len(cfg.CertPEM) == 0 || len(cfg.KeyPEM) == 0 || len(cfg.CAPEM) == 0 {
		return nil, fmt.Errorf("hub TLS material (CertPEM, KeyPEM, CAPEM) is required")
	}

	var serviceRoutes []envoy.ServiceRoute
	for _, svc := range cfg.Services {
		serviceRoutes = append(serviceRoutes, envoy.ServiceRoute{
			SNI:         svc.SNI,
			BackendHost: svc.BackendHost,
			BackendPort: svc.BackendPort,
		})
	}
	bootstrap, err := envoy.RenderHubBootstrap(envoy.HubConfig{
		ListenPort:   cfg.TunnelPort,
		EgressPort:   cfg.EgressPort,
		HandshakeSNI: cfg.HandshakeSNI,
		EnableCRL:    cfg.EnableCRL,
		Members:      cfg.Members,
		Routes:       cfg.Routes,
		Services:     serviceRoutes,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render hub bootstrap: %w", err)
	}

	c := &resourceCollector{}
	c.add(buildNamespace(cfg.Namespace))
	c.add(buildServiceAccount("portal-hub", cfg.Namespace))
	c.add(buildConfigMap("portal-hub-bootstrap", cfg.Namespace, "envoy.yaml", bootstrap))
	secretData := map[string][]byte{
		"tls.crt": cfg.CertPEM,
		"tls.key": cfg.KeyPEM,
		"ca.crt":  cfg.CAPEM,
	}
	if cfg.EnableCRL {
		secretData["crl.pem"] = cfg.CRLPEM
	}
	c.add(buildDataSecret(HubSecretName, cfg.Namespace, secretData))
	c.add(buildHubDeployment(cfg))
	c.add(buildHubTunnelService(cfg))
	c.add(buildHubEgressService(cfg))
	for _, r := range cfg.Routes {
		c.add(buildAliasService(cfg, r))
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.resources, nil
}

// RenderMemberEnrollmentResources renders the minimal resources for phase 1
// of the two-phase join: the namespace and a Secret holding only the private
// key. The certificate is patched in at phase 2, so the key never leaves the
// member cluster.
func RenderMemberEnrollmentResources(namespace string, keyPEM []byte) ([]Resource, error) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if len(keyPEM) == 0 {
		return nil, fmt.Errorf("key material is required")
	}
	c := &resourceCollector{}
	c.add(buildNamespace(namespace))
	c.add(buildDataSecret(MemberSecretName, namespace, map[string][]byte{
		"tls.key": keyPEM,
	}))
	if c.err != nil {
		return nil, c.err
	}
	return c.resources, nil
}

// buildAliasService builds the ClusterIP alias Service for a routed member
// service. Apps call http://<alias>.<namespace>/ and the egress listener
// rewrites the authority to the canonical <service>.<member> form.
func buildAliasService(cfg HubDeployConfig, r envoy.HubRouteAlias) (Resource, error) {
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      r.Alias,
			"namespace": cfg.Namespace,
			"labels":    hubLabels(cfg),
		},
		"spec": map[string]interface{}{
			"type": "ClusterIP",
			"selector": map[string]interface{}{
				"app.kubernetes.io/name": "portal-hub",
			},
			"ports": []interface{}{
				map[string]interface{}{
					"name":       "http",
					"port":       80,
					"targetPort": cfg.EgressPort,
					"protocol":   "TCP",
				},
			},
		},
	}
	return marshalResource(r.Alias+"-alias-service.yaml", svc)
}

// MemberDeployConfig describes a member deployment.
type MemberDeployConfig struct {
	// MemberName is the member's identity.
	MemberName string
	// HubName is the hub name (tenant-id).
	HubName string
	// Namespace for member components (default: portal-system).
	Namespace string
	// HubAddr is the hub tunnel endpoint (host:port).
	HubAddr string
	// HandshakeSNI must match the hub's configuration.
	HandshakeSNI string
	// ConnectionCount is the number of reverse connections (default: 4).
	ConnectionCount int
	// EnvoyImage and EnvoyLogLevel configure the proxy container.
	EnvoyImage    string
	EnvoyLogLevel string
	// AllowUnsupportedEnvoy bypasses the Envoy version gate.
	AllowUnsupportedEnvoy bool
	// Published lists local services the hub may reach.
	Published []envoy.PublishedService
	// Forward lists v1-style forward listeners.
	Forward []envoy.ServiceListener
	// TLS material for the member Secret. Omit to skip rendering the Secret
	// (the two-phase join flow manages the Secret directly so the private
	// key never leaves the member cluster).
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

// RenderMemberManifests renders the complete resource set for a member.
func RenderMemberManifests(cfg MemberDeployConfig) ([]Resource, error) {
	if err := validate.DNSName(cfg.MemberName); err != nil {
		return nil, fmt.Errorf("invalid member name: %w", err)
	}
	applyMemberDefaults(&cfg)
	if err := CheckEnvoyImage(cfg.EnvoyImage, cfg.AllowUnsupportedEnvoy); err != nil {
		return nil, err
	}
	hubHost, hubPort, err := parseEndpoint(cfg.HubAddr, envoy.DefaultTunnelPort)
	if err != nil {
		return nil, fmt.Errorf("invalid hub address: %w", err)
	}

	bootstrap, err := envoy.RenderMemberBootstrap(envoy.MemberConfig{
		MemberName:      cfg.MemberName,
		HubName:         cfg.HubName,
		HubHost:         hubHost,
		HubPort:         hubPort,
		ConnectionCount: cfg.ConnectionCount,
		HandshakeSNI:    cfg.HandshakeSNI,
		Published:       cfg.Published,
		Forward:         cfg.Forward,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render member bootstrap: %w", err)
	}

	c := &resourceCollector{}
	c.add(buildNamespace(cfg.Namespace))
	c.add(buildServiceAccount("portal-member", cfg.Namespace))
	c.add(buildConfigMap("portal-member-bootstrap", cfg.Namespace, "envoy.yaml", bootstrap))
	if len(cfg.KeyPEM) > 0 {
		c.add(buildDataSecret(MemberSecretName, cfg.Namespace, map[string][]byte{
			"tls.crt": cfg.CertPEM,
			"tls.key": cfg.KeyPEM,
			"ca.crt":  cfg.CAPEM,
		}))
	}
	c.add(buildMemberDeployment(cfg))
	for _, fwd := range cfg.Forward {
		c.add(buildMemberForwardService(cfg, fwd))
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.resources, nil
}

func applyHubDefaults(cfg *HubDeployConfig) {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.TunnelPort == 0 {
		cfg.TunnelPort = DefaultTunnelPort
	}
	if cfg.EgressPort == 0 {
		cfg.EgressPort = envoy.DefaultHubEgressPort
	}
	if cfg.HandshakeSNI == "" {
		cfg.HandshakeSNI = envoy.DefaultHandshakeSNI
	}
	if cfg.ServiceType == "" {
		cfg.ServiceType = DefaultServiceType
	}
	if cfg.EnvoyImage == "" {
		cfg.EnvoyImage = DefaultEnvoyImage
	}
	if cfg.EnvoyLogLevel == "" {
		cfg.EnvoyLogLevel = DefaultEnvoyLogLevel
	}
}

func applyMemberDefaults(cfg *MemberDeployConfig) {
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.ConnectionCount == 0 {
		cfg.ConnectionCount = DefaultConnectionCount
	}
	if cfg.HandshakeSNI == "" {
		cfg.HandshakeSNI = envoy.DefaultHandshakeSNI
	}
	if cfg.EnvoyImage == "" {
		cfg.EnvoyImage = DefaultEnvoyImage
	}
	if cfg.EnvoyLogLevel == "" {
		cfg.EnvoyLogLevel = DefaultEnvoyLogLevel
	}
}

// buildDataSecret builds an Opaque Secret with arbitrary keys.
func buildDataSecret(name, namespace string, data map[string][]byte) (Resource, error) {
	encoded := map[string]interface{}{}
	for k, v := range data {
		encoded[k] = base64.StdEncoding.EncodeToString(v)
	}
	sec := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/part-of": "portal",
			},
		},
		"type": "Opaque",
		"data": encoded,
	}
	return marshalResource(name+"-secret.yaml", sec)
}

func hubLabels(cfg HubDeployConfig) map[string]interface{} {
	return map[string]interface{}{
		"app.kubernetes.io/name":      "portal-hub",
		"app.kubernetes.io/component": "hub",
		"app.kubernetes.io/part-of":   "portal",
		"portal.johnlanda.io/hub":     cfg.HubName,
	}
}

func buildHubDeployment(cfg HubDeployConfig) (Resource, error) {
	dep := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "portal-hub",
			"namespace": cfg.Namespace,
			"labels":    hubLabels(cfg),
		},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/name": "portal-hub",
				},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": hubLabels(cfg),
				},
				"spec": map[string]interface{}{
					"serviceAccountName":           "portal-hub",
					"automountServiceAccountToken": false,
					"containers": []interface{}{
						envoyContainer(cfg.EnvoyImage, cfg.EnvoyLogLevel, []interface{}{
							map[string]interface{}{
								"name":          "tunnel",
								"containerPort": cfg.TunnelPort,
								"protocol":      "TCP",
							},
							map[string]interface{}{
								"name":          "egress",
								"containerPort": cfg.EgressPort,
								"protocol":      "TCP",
							},
							map[string]interface{}{
								"name":          "admin",
								"containerPort": envoy.DefaultResponderAdminPort,
								"protocol":      "TCP",
							},
						}),
					},
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "bootstrap",
							"configMap": map[string]interface{}{
								"name": "portal-hub-bootstrap",
							},
						},
						map[string]interface{}{
							"name": "certs",
							"secret": map[string]interface{}{
								"secretName": HubSecretName,
							},
						},
					},
				},
			},
		},
	}
	return marshalResource("portal-hub-deployment.yaml", dep)
}

func buildHubTunnelService(cfg HubDeployConfig) (Resource, error) {
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      "portal-hub",
			"namespace": cfg.Namespace,
			"labels":    hubLabels(cfg),
		},
		"spec": map[string]interface{}{
			"type": cfg.ServiceType,
			"selector": map[string]interface{}{
				"app.kubernetes.io/name": "portal-hub",
			},
			"ports": []interface{}{
				map[string]interface{}{
					"name":       "tunnel",
					"port":       cfg.TunnelPort,
					"targetPort": cfg.TunnelPort,
					"protocol":   "TCP",
				},
			},
		},
	}
	return marshalResource("portal-hub-service.yaml", svc)
}

func buildHubEgressService(cfg HubDeployConfig) (Resource, error) {
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      "portal-hub-egress",
			"namespace": cfg.Namespace,
			"labels":    hubLabels(cfg),
		},
		"spec": map[string]interface{}{
			"type": "ClusterIP",
			"selector": map[string]interface{}{
				"app.kubernetes.io/name": "portal-hub",
			},
			"ports": []interface{}{
				map[string]interface{}{
					"name":       "egress",
					"port":       cfg.EgressPort,
					"targetPort": cfg.EgressPort,
					"protocol":   "TCP",
				},
			},
		},
	}
	return marshalResource("portal-hub-egress-service.yaml", svc)
}

func memberLabels(cfg MemberDeployConfig) map[string]interface{} {
	return map[string]interface{}{
		"app.kubernetes.io/name":      "portal-member",
		"app.kubernetes.io/component": "member",
		"app.kubernetes.io/part-of":   "portal",
		"portal.johnlanda.io/member":  cfg.MemberName,
	}
}

func buildMemberDeployment(cfg MemberDeployConfig) (Resource, error) {
	ports := []interface{}{
		map[string]interface{}{
			"name":          "admin",
			"containerPort": envoy.DefaultInitiatorAdminPort,
			"protocol":      "TCP",
		},
	}
	for _, fwd := range cfg.Forward {
		ports = append(ports, map[string]interface{}{
			"name":          fmt.Sprintf("fwd-%s", fwd.Name),
			"containerPort": fwd.ListenPort,
			"protocol":      "TCP",
		})
	}
	dep := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "portal-member",
			"namespace": cfg.Namespace,
			"labels":    memberLabels(cfg),
		},
		"spec": map[string]interface{}{
			"replicas": 1,
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{
					"app.kubernetes.io/name": "portal-member",
				},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": memberLabels(cfg),
				},
				"spec": map[string]interface{}{
					"serviceAccountName":           "portal-member",
					"automountServiceAccountToken": false,
					"containers": []interface{}{
						envoyContainer(cfg.EnvoyImage, cfg.EnvoyLogLevel, ports),
					},
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "bootstrap",
							"configMap": map[string]interface{}{
								"name": "portal-member-bootstrap",
							},
						},
						map[string]interface{}{
							"name": "certs",
							"secret": map[string]interface{}{
								"secretName": MemberSecretName,
							},
						},
					},
				},
			},
		},
	}
	return marshalResource("portal-member-deployment.yaml", dep)
}

func buildMemberForwardService(cfg MemberDeployConfig, fwd envoy.ServiceListener) (Resource, error) {
	name := fmt.Sprintf("portal-fwd-%s", fwd.Name)
	svc := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": cfg.Namespace,
			"labels":    memberLabels(cfg),
		},
		"spec": map[string]interface{}{
			"type": "ClusterIP",
			"selector": map[string]interface{}{
				"app.kubernetes.io/name": "portal-member",
			},
			"ports": []interface{}{
				map[string]interface{}{
					"name":       "tcp",
					"port":       fwd.ListenPort,
					"targetPort": fwd.ListenPort,
					"protocol":   "TCP",
				},
			},
		},
	}
	return marshalResource(name+"-service.yaml", svc)
}

// envoyContainer builds the standard hardened Envoy container spec.
func envoyContainer(image, logLevel string, ports []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"name":  "envoy",
		"image": image,
		"args": []interface{}{
			"-c", "/etc/envoy/envoy.yaml",
			"--log-level", logLevel,
		},
		"ports": ports,
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
	}
}
