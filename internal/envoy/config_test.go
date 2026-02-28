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
