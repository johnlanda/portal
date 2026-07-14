package envoy

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mustYAML(t *testing.T, data []byte) {
	t.Helper()
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, data)
	}
}

func TestRenderMemberBootstrap(t *testing.T) {
	cfg := MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		HubPort:    10443,
		Published: []PublishedService{
			{Name: "inference", BackendHost: "inference.default.svc.cluster.local", BackendPort: 8080, Protocol: "grpc"},
			{Name: "admin", BackendHost: "admin.default.svc.cluster.local", BackendPort: 9000},
		},
	}

	data, err := RenderMemberBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderMemberBootstrap() error = %v", err)
	}
	mustYAML(t, data)

	s := string(data)
	if !strings.Contains(s, `address: "rc://acme-prod:acme-prod:synapse@hub:4"`) {
		t.Errorf("rendered config does not contain rc:// address with derived node ID and default connection count:\n%s", s)
	}
	if !strings.Contains(s, "resolver_name: envoy.resolvers.reverse_connection") {
		t.Error("rendered config does not use the reverse connection resolver")
	}
	if !strings.Contains(s, "downstream_socket_interface.v3.DownstreamReverseConnectionSocketInterface") {
		t.Error("rendered config does not register the downstream socket interface bootstrap extension")
	}
	if !strings.Contains(s, `"inference.acme-prod"`) {
		t.Error("rendered config does not contain canonical authority for published service")
	}
	if !strings.Contains(s, "cluster: local_inference") {
		t.Error("rendered config does not route published service to local cluster")
	}
	if !strings.Contains(s, "sni: reverse-tunnel.portal") {
		t.Error("rendered config does not use the default handshake SNI")
	}
	if !strings.Contains(s, "tls_minimum_protocol_version: TLSv1_3") {
		t.Error("rendered config does not enforce TLS 1.3 minimum")
	}
	if !strings.Contains(s, "watched_directory") {
		t.Error("rendered config does not use SDS watched_directory for cert hot-reload")
	}
	// grpc backend gets HTTP/2 protocol options; the plain http one must not.
	inferenceIdx := strings.Index(s, "name: local_inference")
	adminIdx := strings.Index(s, "name: local_admin")
	if inferenceIdx == -1 || adminIdx == -1 {
		t.Fatal("rendered config is missing local service clusters")
	}
	inferenceBlock := s[inferenceIdx:adminIdx]
	if !strings.Contains(inferenceBlock, "http2_protocol_options") {
		t.Error("grpc published service cluster does not enable HTTP/2")
	}
	adminBlock := s[adminIdx:]
	if strings.Contains(adminBlock[:strings.Index(adminBlock, "secrets:")], "http2_protocol_options") {
		t.Error("http published service cluster should not force HTTP/2 to the backend")
	}
}

func TestRenderMemberBootstrapDefaults(t *testing.T) {
	data, err := RenderMemberBootstrap(MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
	})
	if err != nil {
		t.Fatalf("RenderMemberBootstrap() error = %v", err)
	}
	mustYAML(t, data)

	s := string(data)
	if !strings.Contains(s, "port_value: 15000") {
		t.Error("admin port did not default to 15000")
	}
	if !strings.Contains(s, "port_value: 10443") {
		t.Error("hub port did not default to 10443")
	}
	if !strings.Contains(s, "@hub:4") {
		t.Error("connection count did not default to 4")
	}
	if !strings.Contains(s, "virtual_hosts: []") {
		t.Error("empty publish list should render an empty virtual_hosts list")
	}
	if !strings.Contains(s, "address: 127.0.0.1") {
		t.Error("admin listener must bind localhost")
	}
}

func TestRenderMemberBootstrapNodeIDOverride(t *testing.T) {
	data, err := RenderMemberBootstrap(MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		NodeID:     "acme-prod-a1x9",
	})
	if err != nil {
		t.Fatalf("RenderMemberBootstrap() error = %v", err)
	}
	if !strings.Contains(string(data), "rc://acme-prod-a1x9:acme-prod:synapse@hub:4") {
		t.Error("node ID override not reflected in rc:// address")
	}
}

func TestRenderMemberBootstrapRejectsTCP(t *testing.T) {
	_, err := RenderMemberBootstrap(MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Published:  []PublishedService{{Name: "postgres", BackendHost: "db", BackendPort: 5432, Protocol: "tcp"}},
	})
	if err == nil {
		t.Fatal("expected error for tcp protocol on reverse path")
	}
	if !strings.Contains(err.Error(), "HTTP/2-only") {
		t.Errorf("error should explain the HTTP/2 constraint, got: %v", err)
	}
}

func TestRenderMemberBootstrapRejectsUnknownProtocol(t *testing.T) {
	_, err := RenderMemberBootstrap(MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Published:  []PublishedService{{Name: "svc", BackendHost: "b", BackendPort: 1, Protocol: "udp"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol") {
		t.Errorf("expected unsupported protocol error, got: %v", err)
	}
}

func TestRenderMemberBootstrapForwardServices(t *testing.T) {
	cfg := MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Forward: []ServiceListener{
			{Name: "telemetry", ListenPort: 4317},
		},
	}
	data, err := RenderMemberBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderMemberBootstrap() error = %v", err)
	}
	mustYAML(t, data)

	s := string(data)
	if !strings.Contains(s, "name: fwd_telemetry") {
		t.Error("rendered config does not contain forward listener")
	}
	if !strings.Contains(s, "cluster: tunnel_to_telemetry") {
		t.Error("forward listener does not route to tunnel cluster")
	}
	if !strings.Contains(s, "sni: telemetry") {
		t.Error("forward cluster does not default SNI to service name")
	}
}

func TestRenderMemberBootstrapForwardSNICollision(t *testing.T) {
	_, err := RenderMemberBootstrap(MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Forward: []ServiceListener{
			{Name: "svc", ListenPort: 4317, SNI: DefaultHandshakeSNI},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("expected reserved-SNI error, got: %v", err)
	}
}

func TestRenderMemberBootstrapValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  MemberConfig
	}{
		{"empty member", MemberConfig{HubName: "h", HubHost: "hub"}},
		{"invalid member", MemberConfig{MemberName: "UPPER CASE", HubName: "h", HubHost: "hub"}},
		{"empty hub host", MemberConfig{MemberName: "m", HubName: "h"}},
		{"node id with colon", MemberConfig{MemberName: "m", HubName: "h", HubHost: "hub", NodeID: "a:b"}},
		{"missing backend port", MemberConfig{MemberName: "m", HubName: "h", HubHost: "hub",
			Published: []PublishedService{{Name: "svc", BackendHost: "b"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderMemberBootstrap(tc.cfg); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestRenderHubBootstrap(t *testing.T) {
	cfg := HubConfig{
		Members: []string{"acme-prod", "globex-dev"},
		Services: []ServiceRoute{
			{SNI: "api.hub", BackendHost: "backend.portal.svc", BackendPort: 8443},
		},
	}
	data, err := RenderHubBootstrap(cfg)
	if err != nil {
		t.Fatalf("RenderHubBootstrap() error = %v", err)
	}
	mustYAML(t, data)

	s := string(data)
	if !strings.Contains(s, "upstream_socket_interface.v3.UpstreamReverseConnectionSocketInterface") {
		t.Error("rendered config does not register the upstream socket interface bootstrap extension")
	}
	if !strings.Contains(s, "envoy.filters.network.reverse_tunnel") {
		t.Error("rendered config does not contain the reverse tunnel handshake filter")
	}
	if !strings.Contains(s, `cluster_id_format: "%DOWNSTREAM_PEER_DNS_SAN%"`) {
		t.Error("handshake validation is not bound to the peer certificate DNS SAN")
	}
	if !strings.Contains(s, "require_client_certificate: true") {
		t.Error("handshake chain does not require a client certificate")
	}
	if !strings.Contains(s, "tls_inspector") {
		t.Error("shared listener does not use tls_inspector")
	}
	if !strings.Contains(s, `- "reverse-tunnel.portal"`) {
		t.Error("handshake chain does not match the default handshake SNI")
	}
	if !strings.Contains(s, `- "*.acme-prod"`) {
		t.Error("egress listener does not contain wildcard authority for member")
	}
	if !strings.Contains(s, "key: x-portal-member") {
		t.Error("egress virtual host does not inject the member header")
	}
	if !strings.Contains(s, `host_id_format: "%REQ(x-portal-member)%"`) {
		t.Error("reverse connection cluster does not select host by member header")
	}
	if !strings.Contains(s, "lb_policy: CLUSTER_PROVIDED") {
		t.Error("reverse connection cluster does not delegate load balancing")
	}
	if !strings.Contains(s, "name: envoy.clusters.reverse_connection") {
		t.Error("rendered config does not contain the reverse connection cluster type")
	}
	if !strings.Contains(s, "http2_protocol_options") {
		t.Error("reverse connection cluster does not enable HTTP/2")
	}
	if !strings.Contains(s, "ping_interval: 2s") {
		t.Error("ping interval did not default to 2s")
	}
	if !strings.Contains(s, "cleanup_interval: 60s") {
		t.Error("cleanup interval did not default to 60s (must be protobuf duration format, not Go's 1m0s)")
	}
	// Forward chain (v1 mechanism) coexists on the shared listener.
	if !strings.Contains(s, `- "api.hub"`) {
		t.Error("shared listener does not contain forward service SNI chain")
	}
	if !strings.Contains(s, "cluster: api_hub") {
		t.Error("forward chain does not route to sanitized backend cluster")
	}
}

func TestRenderHubBootstrapDefaults(t *testing.T) {
	data, err := RenderHubBootstrap(HubConfig{})
	if err != nil {
		t.Fatalf("RenderHubBootstrap() error = %v", err)
	}
	mustYAML(t, data)

	s := string(data)
	if !strings.Contains(s, "port_value: 10443") {
		t.Error("tunnel listener port did not default to 10443")
	}
	if !strings.Contains(s, "port_value: 10080") {
		t.Error("egress port did not default to 10080")
	}
	if !strings.Contains(s, "port_value: 15001") {
		t.Error("admin port did not default to 15001")
	}
	if !strings.Contains(s, "virtual_hosts: []") {
		t.Error("empty member list should render an empty virtual_hosts list")
	}
	if !strings.Contains(s, "address: 127.0.0.1") {
		t.Error("admin listener must bind localhost")
	}
}

func TestRenderHubBootstrapCustomIntervals(t *testing.T) {
	data, err := RenderHubBootstrap(HubConfig{
		PingInterval:    5 * time.Second,
		CleanupInterval: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("RenderHubBootstrap() error = %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "ping_interval: 5s") {
		t.Error("custom ping interval not rendered")
	}
	if !strings.Contains(s, "cleanup_interval: 90s") {
		t.Error("custom cleanup interval not rendered in protobuf duration format")
	}
}

func TestRenderHubBootstrapHandshakeSNICollision(t *testing.T) {
	_, err := RenderHubBootstrap(HubConfig{
		Services: []ServiceRoute{{SNI: DefaultHandshakeSNI, BackendHost: "b", BackendPort: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("expected reserved-SNI error, got: %v", err)
	}
}

func TestRenderHubBootstrapMemberValidation(t *testing.T) {
	_, err := RenderHubBootstrap(HubConfig{Members: []string{"bad name"}})
	if err == nil {
		t.Error("expected validation error for invalid member name")
	}
}

func TestProtoDuration(t *testing.T) {
	cases := map[time.Duration]string{
		2 * time.Second:         "2s",
		60 * time.Second:        "60s",
		90 * time.Second:        "90s",
		1500 * time.Millisecond: "1.5s",
	}
	for d, want := range cases {
		if got := protoDuration(d); got != want {
			t.Errorf("protoDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
