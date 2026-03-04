package envoy

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderInitiatorBootstrap(t *testing.T) {
	cfg := InitiatorConfig{
		ResponderHost: "34.120.1.50",
		ResponderPort: 10443,
		CertPath:      "/etc/portal/certs",
	}

	data, err := RenderInitiatorBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorBootstrap() error = %v", err)
	}

	// Verify it's valid YAML.
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	s := string(data)
	// Verify key content.
	if !strings.Contains(s, "34.120.1.50") {
		t.Error("rendered config does not contain responder host")
	}
	if !strings.Contains(s, "port_value: 10443") {
		t.Error("rendered config does not contain responder port")
	}
	if !strings.Contains(s, "/etc/portal/certs/tls.crt") {
		t.Error("rendered config does not contain cert path")
	}
	if !strings.Contains(s, "tunnel_to_responder") {
		t.Error("rendered config does not contain tunnel cluster name")
	}
	if !strings.Contains(s, "alpn_protocols") {
		t.Error("rendered config does not contain ALPN config")
	}
	if !strings.Contains(s, "tls_minimum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not enforce TLS 1.3 minimum")
	}
	if !strings.Contains(s, "tls_maximum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not set TLS 1.3 maximum (required for UpstreamTlsContext where default max is TLSv1_2)")
	}
	if !strings.Contains(s, "sni: 34.120.1.50") {
		t.Error("rendered config does not contain SNI (should default to ResponderHost)")
	}
}

func TestRenderInitiatorBootstrapDNS(t *testing.T) {
	cfg := InitiatorConfig{
		ResponderHost: "tunnel.infra.example.com",
		ResponderPort: 10443,
	}

	data, err := RenderInitiatorBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorBootstrap() error = %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "tunnel.infra.example.com") {
		t.Error("rendered config does not contain DNS hostname")
	}
	if !strings.Contains(s, "sni: tunnel.infra.example.com") {
		t.Error("rendered config does not contain SNI for DNS hostname")
	}
}

func TestRenderResponderBootstrap(t *testing.T) {
	cfg := ResponderConfig{
		ListenPort: 10443,
		CertPath:   "/etc/portal/certs",
	}

	data, err := RenderResponderBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderResponderBootstrap() error = %v", err)
	}

	// Verify it's valid YAML.
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	s := string(data)
	if !strings.Contains(s, "require_client_certificate: true") {
		t.Error("rendered config does not require client certificate")
	}
	if !strings.Contains(s, "/etc/portal/certs/tls.crt") {
		t.Error("rendered config does not contain cert path")
	}
	if !strings.Contains(s, "local_backend") {
		t.Error("rendered config does not contain local backend cluster")
	}
	if !strings.Contains(s, "tls_minimum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not enforce TLS 1.3 minimum")
	}
}

func TestRenderInitiatorBootstrapCustomSNI(t *testing.T) {
	cfg := InitiatorConfig{
		ResponderHost: "10.0.0.1",
		ResponderPort: 10443,
		SNI:           "custom.example.com",
	}

	data, err := RenderInitiatorBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorBootstrap() error = %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "sni: custom.example.com") {
		t.Error("rendered config does not use custom SNI")
	}
}

func TestRenderInitiatorBootstrapDefaults(t *testing.T) {
	cfg := InitiatorConfig{
		ResponderHost: "10.0.0.1",
	}

	data, err := RenderInitiatorBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorBootstrap() error = %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "port_value: 15000") {
		t.Error("rendered config does not use default admin port 15000")
	}
	if !strings.Contains(s, "/etc/portal/certs/") {
		t.Error("rendered config does not use default cert path")
	}
}

func TestRenderResponderBootstrapDefaults(t *testing.T) {
	// Pass zero-value config — all defaults should be applied.
	cfg := ResponderConfig{}

	data, err := RenderResponderBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderResponderBootstrap() error = %v", err)
	}

	s := string(data)

	// Verify default admin port (15001).
	if !strings.Contains(s, "port_value: 15001") {
		t.Error("rendered config does not use default admin port 15001")
	}
	// Verify default cert path.
	if !strings.Contains(s, "/etc/portal/certs/tls.crt") {
		t.Error("rendered config does not use default cert path")
	}
	// Verify default listen port.
	if !strings.Contains(s, "port_value: 10443") {
		t.Error("rendered config does not use default listen port 10443")
	}
	// Verify it's valid YAML.
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v", err)
	}
}

func TestResponderAdminBindsLocalhost(t *testing.T) {
	cfg := ResponderConfig{
		ListenPort: 10443,
		CertPath:   "/etc/portal/certs",
	}

	data, err := RenderResponderBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderResponderBootstrap() error = %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	// Navigate: admin -> address -> socket_address -> address
	admin, ok := parsed["admin"].(map[string]interface{})
	if !ok {
		t.Fatal("admin section not found in rendered config")
	}
	addr, ok := admin["address"].(map[string]interface{})
	if !ok {
		t.Fatal("admin.address not found")
	}
	socketAddr, ok := addr["socket_address"].(map[string]interface{})
	if !ok {
		t.Fatal("admin.address.socket_address not found")
	}
	if socketAddr["address"] != "127.0.0.1" {
		t.Errorf("admin address = %v, want 127.0.0.1", socketAddr["address"])
	}
}

func TestBootstrapALPNH2(t *testing.T) {
	initiatorData, err := RenderInitiatorBootstrap(InitiatorConfig{
		ResponderHost: "10.0.0.1",
		ResponderPort: 10443,
		CertPath:      "/etc/portal/certs",
	})
	if err != nil {
		t.Fatalf("RenderInitiatorBootstrap() error = %v", err)
	}

	responderData, err := RenderResponderBootstrap(ResponderConfig{
		ListenPort: 10443,
		CertPath:   "/etc/portal/certs",
	})
	if err != nil {
		t.Fatalf("RenderResponderBootstrap() error = %v", err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"initiator", initiatorData},
		{"responder", responderData},
	} {
		if !strings.Contains(string(tc.data), "- h2") {
			t.Errorf("%s bootstrap does not contain ALPN protocol '- h2'", tc.name)
		}
	}
}

func TestRenderInitiatorBootstrapSNIDefaultsToHost(t *testing.T) {
	cfg := InitiatorConfig{
		ResponderHost: "my-responder.example.com",
		ResponderPort: 10443,
		// SNI intentionally left empty — should default to ResponderHost.
	}

	data, err := RenderInitiatorBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorBootstrap() error = %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "sni: my-responder.example.com") {
		t.Error("SNI should default to ResponderHost when not set")
	}
}

// --- Multi-service template tests ---

func TestRenderResponderMultiBootstrapSingleService(t *testing.T) {
	cfg := ResponderMultiServiceConfig{
		ListenPort: 10443,
		CertPath:   "/etc/portal/certs",
		Services: []ServiceRoute{
			{SNI: "backend", BackendHost: "backend-svc.ns.svc", BackendPort: 8443},
		},
	}

	data, err := RenderResponderMultiBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderResponderMultiBootstrap() error = %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	s := string(data)
	if !strings.Contains(s, "tls_inspector") {
		t.Error("rendered config does not contain tls_inspector listener filter")
	}
	if !strings.Contains(s, `"backend"`) {
		t.Error("rendered config does not contain SNI value 'backend'")
	}
	if !strings.Contains(s, "backend-svc.ns.svc") {
		t.Error("rendered config does not contain backend host")
	}
	if !strings.Contains(s, "port_value: 8443") {
		t.Error("rendered config does not contain backend port")
	}
	if !strings.Contains(s, "require_client_certificate: true") {
		t.Error("rendered config does not require client certificate")
	}
	if !strings.Contains(s, "tls_minimum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not enforce TLS 1.3")
	}
}

func TestRenderResponderMultiBootstrapTwoServices(t *testing.T) {
	cfg := ResponderMultiServiceConfig{
		ListenPort: 10443,
		CertPath:   "/etc/portal/certs",
		Services: []ServiceRoute{
			{SNI: "backend", BackendHost: "backend-svc.ns.svc", BackendPort: 8443},
			{SNI: "otel", BackendHost: "otel-collector.ns.svc", BackendPort: 4317},
		},
	}

	data, err := RenderResponderMultiBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderResponderMultiBootstrap() error = %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	s := string(data)
	// Both SNI values should be present.
	if !strings.Contains(s, `"backend"`) {
		t.Error("rendered config does not contain SNI 'backend'")
	}
	if !strings.Contains(s, `"otel"`) {
		t.Error("rendered config does not contain SNI 'otel'")
	}
	// Both backend hosts should be present.
	if !strings.Contains(s, "backend-svc.ns.svc") {
		t.Error("rendered config does not contain backend host")
	}
	if !strings.Contains(s, "otel-collector.ns.svc") {
		t.Error("rendered config does not contain otel host")
	}
	// Both cluster names should be present.
	if !strings.Contains(s, "cluster: backend") {
		t.Error("rendered config does not contain cluster name 'backend'")
	}
	if !strings.Contains(s, "cluster: otel") {
		t.Error("rendered config does not contain cluster name 'otel'")
	}
	// tls_inspector must be present.
	if !strings.Contains(s, "tls_inspector") {
		t.Error("rendered config does not contain tls_inspector")
	}
}

func TestRenderResponderMultiBootstrapDefaults(t *testing.T) {
	cfg := ResponderMultiServiceConfig{
		Services: []ServiceRoute{
			{SNI: "backend", BackendHost: "backend-svc.svc", BackendPort: 8443},
		},
	}

	data, err := RenderResponderMultiBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderResponderMultiBootstrap() error = %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "port_value: 10443") {
		t.Error("should use default listen port")
	}
	if !strings.Contains(s, "port_value: 15001") {
		t.Error("should use default admin port")
	}
	if !strings.Contains(s, "/etc/portal/certs/") {
		t.Error("should use default cert path")
	}
}

func TestRenderInitiatorMultiBootstrapSingleService(t *testing.T) {
	cfg := InitiatorMultiServiceConfig{
		ResponderHost: "10.0.0.1",
		ResponderPort: 10443,
		CertPath:      "/etc/portal/certs",
		Services: []ServiceListener{
			{Name: "backend", ListenPort: 18443, SNI: "backend"},
		},
	}

	data, err := RenderInitiatorMultiBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorMultiBootstrap() error = %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	s := string(data)
	if !strings.Contains(s, "svc_backend") {
		t.Error("rendered config does not contain listener name")
	}
	if !strings.Contains(s, "port_value: 18443") {
		t.Error("rendered config does not contain listen port")
	}
	if !strings.Contains(s, "tunnel_to_backend") {
		t.Error("rendered config does not contain cluster name")
	}
	if !strings.Contains(s, "sni: backend") {
		t.Error("rendered config does not contain SNI")
	}
	if !strings.Contains(s, "10.0.0.1") {
		t.Error("rendered config does not contain responder host")
	}
	if !strings.Contains(s, "tls_minimum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not enforce TLS 1.3")
	}
	if !strings.Contains(s, "tls_maximum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not set TLS 1.3 maximum")
	}
	if !strings.Contains(s, "- h2") {
		t.Error("rendered config does not contain ALPN h2")
	}
}

func TestRenderInitiatorMultiBootstrapTwoServices(t *testing.T) {
	cfg := InitiatorMultiServiceConfig{
		ResponderHost: "tunnel.example.com",
		ResponderPort: 10443,
		CertPath:      "/etc/portal/certs",
		Services: []ServiceListener{
			{Name: "backend", ListenPort: 18443, SNI: "backend"},
			{Name: "otel", ListenPort: 14317, SNI: "otel"},
		},
	}

	data, err := RenderInitiatorMultiBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorMultiBootstrap() error = %v", err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}

	s := string(data)
	// Both listeners should be present.
	if !strings.Contains(s, "svc_backend") {
		t.Error("rendered config does not contain backend listener")
	}
	if !strings.Contains(s, "svc_otel") {
		t.Error("rendered config does not contain otel listener")
	}
	// Both ports should be present.
	if !strings.Contains(s, "port_value: 18443") {
		t.Error("rendered config does not contain backend listen port")
	}
	if !strings.Contains(s, "port_value: 14317") {
		t.Error("rendered config does not contain otel listen port")
	}
	// Both clusters should be present.
	if !strings.Contains(s, "tunnel_to_backend") {
		t.Error("rendered config does not contain backend cluster")
	}
	if !strings.Contains(s, "tunnel_to_otel") {
		t.Error("rendered config does not contain otel cluster")
	}
	// Both SNI values should be present.
	if !strings.Contains(s, "sni: backend") {
		t.Error("rendered config does not contain backend SNI")
	}
	if !strings.Contains(s, "sni: otel") {
		t.Error("rendered config does not contain otel SNI")
	}
	// All clusters point to same responder.
	if strings.Count(s, "tunnel.example.com") < 2 {
		t.Error("both clusters should reference the responder host")
	}
}

func TestRenderInitiatorMultiBootstrapDefaults(t *testing.T) {
	cfg := InitiatorMultiServiceConfig{
		ResponderHost: "10.0.0.1",
		Services: []ServiceListener{
			{Name: "backend", ListenPort: 18443},
		},
	}

	data, err := RenderInitiatorMultiBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderInitiatorMultiBootstrap() error = %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "port_value: 15000") {
		t.Error("should use default admin port")
	}
	if !strings.Contains(s, "/etc/portal/certs/") {
		t.Error("should use default cert path")
	}
	// SNI should default to service name.
	if !strings.Contains(s, "sni: backend") {
		t.Error("SNI should default to service name")
	}
	// ListenAddress should default to 0.0.0.0.
	if !strings.Contains(s, "address: 0.0.0.0") {
		t.Error("ListenAddress should default to 0.0.0.0")
	}
}

func TestSDSWatchedDirectoryPresent(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			"initiator-single",
			mustRender(t, func() ([]byte, error) {
				return RenderInitiatorBootstrap(InitiatorConfig{
					ResponderHost: "10.0.0.1",
					ResponderPort: 10443,
					CertPath:      "/etc/portal/certs",
				})
			}),
		},
		{
			"responder-single",
			mustRender(t, func() ([]byte, error) {
				return RenderResponderBootstrap(ResponderConfig{
					ListenPort: 10443,
					CertPath:   "/etc/portal/certs",
				})
			}),
		},
		{
			"initiator-multi",
			mustRender(t, func() ([]byte, error) {
				return RenderInitiatorMultiBootstrap(InitiatorMultiServiceConfig{
					ResponderHost: "10.0.0.1",
					ResponderPort: 10443,
					CertPath:      "/etc/portal/certs",
					Services: []ServiceListener{
						{Name: "backend", ListenPort: 18443, SNI: "backend"},
					},
				})
			}),
		},
		{
			"responder-multi",
			mustRender(t, func() ([]byte, error) {
				return RenderResponderMultiBootstrap(ResponderMultiServiceConfig{
					ListenPort: 10443,
					CertPath:   "/etc/portal/certs",
					Services: []ServiceRoute{
						{SNI: "backend", BackendHost: "backend-svc.ns.svc", BackendPort: 8443},
					},
				})
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := string(tc.data)

			// Verify SDS secret references in TLS context.
			if !strings.Contains(s, "tls_certificate_sds_secret_configs") {
				t.Error("should contain tls_certificate_sds_secret_configs")
			}
			if !strings.Contains(s, "validation_context_sds_secret_config") {
				t.Error("should contain validation_context_sds_secret_config")
			}
			if !strings.Contains(s, "name: portal_tls") {
				t.Error("should reference portal_tls SDS secret")
			}
			if !strings.Contains(s, "name: portal_ca") {
				t.Error("should reference portal_ca SDS secret")
			}

			// Verify static secrets section with watched_directory.
			if !strings.Contains(s, "secrets:") {
				t.Error("should contain secrets section")
			}
			if !strings.Contains(s, "watched_directory:") {
				t.Error("should contain watched_directory for cert hot-reload")
			}

			// Verify inline tls_certificates is NOT present in TLS contexts.
			// The cert paths should only appear inside the secrets section.
			lines := strings.Split(s, "\n")
			for i, line := range lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "tls_certificates:" {
					// This key should NOT appear — SDS replaces it.
					t.Errorf("line %d: found inline tls_certificates (should use SDS instead)", i+1)
				}
			}

			// Verify it's valid YAML.
			var parsed map[string]interface{}
			if err := yaml.Unmarshal(tc.data, &parsed); err != nil {
				t.Fatalf("rendered config is not valid YAML: %v", err)
			}
		})
	}
}

func mustRender(t *testing.T, fn func() ([]byte, error)) []byte {
	t.Helper()
	data, err := fn()
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	return data
}

func TestSanitizeStatPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"backend", "backend"},
		{"otel-collector", "otel_collector"},
		{"my.service.name", "my_service_name"},
		{"svc_123", "svc_123"},
	}
	for _, tt := range tests {
		got := sanitizeStatPrefix(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeStatPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
