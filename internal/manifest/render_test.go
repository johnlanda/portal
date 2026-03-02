package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRenderWithIP(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "gke-us-east",
		DestinationContext: "eks-eu-west",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Verify tunnel name was auto-generated.
	if bundle.Metadata.TunnelName != "gke-us-east--eks-eu-west" {
		t.Errorf("TunnelName = %q, want %q", bundle.Metadata.TunnelName, "gke-us-east--eks-eu-west")
	}

	// Verify source resources.
	if len(bundle.Source) != 6 {
		t.Errorf("got %d source resources, want 6", len(bundle.Source))
	}
	assertResourceExists(t, bundle.Source, "namespace.yaml")
	assertResourceExists(t, bundle.Source, "portal-initiator-sa.yaml")
	assertResourceExists(t, bundle.Source, "portal-initiator-bootstrap-cm.yaml")
	assertResourceExists(t, bundle.Source, "portal-tunnel-tls-secret.yaml")
	assertResourceExists(t, bundle.Source, "portal-initiator-deployment.yaml")
	assertResourceExists(t, bundle.Source, "portal-initiator-networkpolicy.yaml")

	// Verify destination resources.
	if len(bundle.Destination) != 7 {
		t.Errorf("got %d destination resources, want 7", len(bundle.Destination))
	}
	assertResourceExists(t, bundle.Destination, "namespace.yaml")
	assertResourceExists(t, bundle.Destination, "portal-responder-sa.yaml")
	assertResourceExists(t, bundle.Destination, "portal-responder-bootstrap-cm.yaml")
	assertResourceExists(t, bundle.Destination, "portal-tunnel-tls-secret.yaml")
	assertResourceExists(t, bundle.Destination, "portal-responder-deployment.yaml")
	assertResourceExists(t, bundle.Destination, "portal-responder-service.yaml")
	assertResourceExists(t, bundle.Destination, "portal-responder-networkpolicy.yaml")

	// Verify responder service has loadBalancerIP.
	svc := findResource(bundle.Destination, "portal-responder-service.yaml")
	if svc == nil {
		t.Fatal("portal-responder-service.yaml not found")
	}
	if !strings.Contains(string(svc.Content), "loadBalancerIP") {
		t.Error("responder service should have loadBalancerIP for IP endpoint")
	}
	if !strings.Contains(string(svc.Content), "10.0.0.1") {
		t.Error("responder service should contain the IP address")
	}

	// Verify all resources are valid YAML.
	for _, r := range append(bundle.Source, bundle.Destination...) {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Errorf("%s is not valid YAML: %v", r.Filename, err)
		}
	}

	// Verify certs are populated.
	if bundle.Certs == nil {
		t.Fatal("Certs is nil")
	}
	if len(bundle.Certs.CACert) == 0 {
		t.Error("CACert is empty")
	}
}

func TestRenderWithDNS(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "gke-us-east",
		DestinationContext: "eks-eu-west",
		ResponderEndpoint:  "tunnel.infra.example.com:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Verify responder service has external-dns annotation instead of loadBalancerIP.
	svc := findResource(bundle.Destination, "portal-responder-service.yaml")
	if svc == nil {
		t.Fatal("portal-responder-service.yaml not found")
	}
	if strings.Contains(string(svc.Content), "loadBalancerIP") {
		t.Error("responder service should NOT have loadBalancerIP for DNS endpoint")
	}
	if !strings.Contains(string(svc.Content), "external-dns.alpha.kubernetes.io/hostname") {
		t.Error("responder service should have external-dns annotation for DNS endpoint")
	}
	if !strings.Contains(string(svc.Content), "tunnel.infra.example.com") {
		t.Error("responder service should contain the DNS hostname")
	}
}

func TestWriteToDisk(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	outputDir := t.TempDir()
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	// Verify directory structure.
	expectedFiles := []string{
		"source/namespace.yaml",
		"source/portal-initiator-sa.yaml",
		"source/portal-initiator-bootstrap-cm.yaml",
		"source/portal-tunnel-tls-secret.yaml",
		"source/portal-initiator-deployment.yaml",
		"source/portal-initiator-networkpolicy.yaml",
		"source/kustomization.yaml",
		"destination/namespace.yaml",
		"destination/portal-responder-sa.yaml",
		"destination/portal-responder-bootstrap-cm.yaml",
		"destination/portal-tunnel-tls-secret.yaml",
		"destination/portal-responder-deployment.yaml",
		"destination/portal-responder-service.yaml",
		"destination/portal-responder-networkpolicy.yaml",
		"destination/kustomization.yaml",
		"tunnel.yaml",
		"ca/ca.crt",
		"ca/ca.key",
		"ca/.gitignore",
	}

	for _, f := range expectedFiles {
		path := filepath.Join(outputDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s does not exist", f)
		}
	}

	// Verify CA directory permissions.
	caInfo, err := os.Stat(filepath.Join(outputDir, "ca"))
	if err != nil {
		t.Fatalf("failed to stat ca/: %v", err)
	}
	if caInfo.Mode().Perm() != 0700 {
		t.Errorf("ca/ permissions = %o, want 0700", caInfo.Mode().Perm())
	}

	// Verify ca.key permissions.
	keyInfo, err := os.Stat(filepath.Join(outputDir, "ca", "ca.key"))
	if err != nil {
		t.Fatalf("failed to stat ca/ca.key: %v", err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Errorf("ca/ca.key permissions = %o, want 0600", keyInfo.Mode().Perm())
	}

	// Verify kustomization.yaml lists all resources.
	kustBytes, err := os.ReadFile(filepath.Join(outputDir, "source", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("failed to read source kustomization.yaml: %v", err)
	}
	var kust map[string]interface{}
	if err := yaml.Unmarshal(kustBytes, &kust); err != nil {
		t.Fatalf("source kustomization.yaml is not valid YAML: %v", err)
	}
	resources, ok := kust["resources"].([]interface{})
	if !ok {
		t.Fatal("source kustomization.yaml has no resources field")
	}
	if len(resources) != 6 {
		t.Errorf("source kustomization.yaml has %d resources, want 6", len(resources))
	}

	// Verify tunnel.yaml metadata.
	metaBytes, err := os.ReadFile(filepath.Join(outputDir, "tunnel.yaml"))
	if err != nil {
		t.Fatalf("failed to read tunnel.yaml: %v", err)
	}
	var meta TunnelMetadata
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("tunnel.yaml is not valid YAML: %v", err)
	}
	if meta.TunnelName != "src--dst" {
		t.Errorf("tunnel.yaml TunnelName = %q, want %q", meta.TunnelName, "src--dst")
	}
}

func TestRenderDeploymentSecurity(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Check that both deployments have security context, resource limits, and SA refs.
	for _, side := range []struct {
		name      string
		resources []Resource
		filename  string
		saName    string
	}{
		{"initiator", bundle.Source, "portal-initiator-deployment.yaml", "portal-initiator"},
		{"responder", bundle.Destination, "portal-responder-deployment.yaml", "portal-responder"},
	} {
		r := findResource(side.resources, side.filename)
		if r == nil {
			t.Fatalf("%s not found", side.filename)
		}
		content := string(r.Content)
		if !strings.Contains(content, "runAsNonRoot") {
			t.Errorf("%s deployment should have runAsNonRoot", side.name)
		}
		if !strings.Contains(content, "readOnlyRootFilesystem") {
			t.Errorf("%s deployment should have readOnlyRootFilesystem", side.name)
		}
		// Resource limits.
		for _, expected := range []string{"cpu", "memory", "100m", "128Mi", "500m", "256Mi"} {
			if !strings.Contains(content, expected) {
				t.Errorf("%s deployment should contain resource value %q", side.name, expected)
			}
		}
		// ServiceAccount reference.
		if !strings.Contains(content, "serviceAccountName") {
			t.Errorf("%s deployment should have serviceAccountName", side.name)
		}
		if !strings.Contains(content, side.saName) {
			t.Errorf("%s deployment should reference SA %q", side.name, side.saName)
		}
		if !strings.Contains(content, "automountServiceAccountToken") {
			t.Errorf("%s deployment should have automountServiceAccountToken", side.name)
		}
		// Capabilities drop ALL.
		if !strings.Contains(content, "drop") {
			t.Errorf("%s deployment should drop capabilities", side.name)
		}
		if !strings.Contains(content, "ALL") {
			t.Errorf("%s deployment should drop ALL capabilities", side.name)
		}
		// Seccomp profile.
		if !strings.Contains(content, "seccompProfile") {
			t.Errorf("%s deployment should have seccompProfile", side.name)
		}
		if !strings.Contains(content, "RuntimeDefault") {
			t.Errorf("%s deployment should use RuntimeDefault seccomp profile", side.name)
		}
	}
}

func TestRenderNetworkPolicies(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Verify initiator NetworkPolicy.
	initNP := findResource(bundle.Source, "portal-initiator-networkpolicy.yaml")
	if initNP == nil {
		t.Fatal("portal-initiator-networkpolicy.yaml not found in source resources")
	}
	initContent := string(initNP.Content)
	var initParsed map[string]interface{}
	if err := yaml.Unmarshal(initNP.Content, &initParsed); err != nil {
		t.Fatalf("initiator NetworkPolicy is not valid YAML: %v", err)
	}
	if initParsed["kind"] != "NetworkPolicy" {
		t.Errorf("initiator NetworkPolicy kind = %v, want NetworkPolicy", initParsed["kind"])
	}
	if !strings.Contains(initContent, "portal-initiator") {
		t.Error("initiator NetworkPolicy should reference portal-initiator")
	}

	// Verify responder NetworkPolicy.
	respNP := findResource(bundle.Destination, "portal-responder-networkpolicy.yaml")
	if respNP == nil {
		t.Fatal("portal-responder-networkpolicy.yaml not found in destination resources")
	}
	var respParsed map[string]interface{}
	if err := yaml.Unmarshal(respNP.Content, &respParsed); err != nil {
		t.Fatalf("responder NetworkPolicy is not valid YAML: %v", err)
	}
	if respParsed["kind"] != "NetworkPolicy" {
		t.Errorf("responder NetworkPolicy kind = %v, want NetworkPolicy", respParsed["kind"])
	}
	respContent := string(respNP.Content)
	if !strings.Contains(respContent, "portal-responder") {
		t.Error("responder NetworkPolicy should reference portal-responder")
	}
}

func TestRenderWithBareIP(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1", // no port
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Should use default port.
	if bundle.Metadata.TunnelPort != DefaultTunnelPort {
		t.Errorf("TunnelPort = %d, want %d", bundle.Metadata.TunnelPort, DefaultTunnelPort)
	}
}

func TestRenderWithInvalidEndpoint(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "not:valid:endpoint",
		CertValidity:       24 * time.Hour,
	}

	_, err := Render(cfg)
	if err == nil {
		t.Fatal("expected error for invalid endpoint")
	}
	if !strings.Contains(err.Error(), "invalid responder endpoint") {
		t.Errorf("error should mention invalid endpoint, got: %v", err)
	}
}

func TestRenderCustomNamespace(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		Namespace:          "my-custom-ns",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if bundle.Metadata.Namespace != "my-custom-ns" {
		t.Errorf("Namespace = %q, want %q", bundle.Metadata.Namespace, "my-custom-ns")
	}

	// Verify namespace propagates to all resources.
	allResources := append(bundle.Source, bundle.Destination...)
	for _, r := range allResources {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Fatalf("%s is not valid YAML: %v", r.Filename, err)
		}
		meta, ok := parsed["metadata"].(map[string]interface{})
		if !ok {
			continue
		}
		// Namespace resource has name=namespace, not namespace field.
		if parsed["kind"] == "Namespace" {
			if meta["name"] != "my-custom-ns" {
				t.Errorf("%s: Namespace name = %v, want %q", r.Filename, meta["name"], "my-custom-ns")
			}
		} else if ns, ok := meta["namespace"]; ok {
			if ns != "my-custom-ns" {
				t.Errorf("%s: namespace = %v, want %q", r.Filename, ns, "my-custom-ns")
			}
		}
	}
}

func TestRenderDefaultsApplied(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if bundle.Metadata.TunnelName != "src--dst" {
		t.Errorf("TunnelName = %q, want %q", bundle.Metadata.TunnelName, "src--dst")
	}
	if bundle.Metadata.Namespace != DefaultNamespace {
		t.Errorf("Namespace = %q, want %q", bundle.Metadata.Namespace, DefaultNamespace)
	}
	if bundle.Metadata.TunnelPort != DefaultTunnelPort {
		t.Errorf("TunnelPort = %d, want %d", bundle.Metadata.TunnelPort, DefaultTunnelPort)
	}
	if bundle.Metadata.EnvoyImage != DefaultEnvoyImage {
		t.Errorf("EnvoyImage = %q, want %q", bundle.Metadata.EnvoyImage, DefaultEnvoyImage)
	}
	if bundle.Metadata.ServiceType != DefaultServiceType {
		t.Errorf("ServiceType = %q, want %q", bundle.Metadata.ServiceType, DefaultServiceType)
	}
}

func TestDefaultEnvoyImagePinnedByDigest(t *testing.T) {
	if !strings.Contains(DefaultEnvoyImage, "@sha256:") {
		t.Errorf("DefaultEnvoyImage should be pinned by digest, got %q", DefaultEnvoyImage)
	}
}

func TestRenderServiceAccount(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	for _, tc := range []struct {
		name      string
		resources []Resource
		saFile    string
		saName    string
	}{
		{"initiator", bundle.Source, "portal-initiator-sa.yaml", "portal-initiator"},
		{"responder", bundle.Destination, "portal-responder-sa.yaml", "portal-responder"},
	} {
		r := findResource(tc.resources, tc.saFile)
		if r == nil {
			t.Fatalf("%s ServiceAccount file %q not found", tc.name, tc.saFile)
		}

		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Fatalf("%s SA is not valid YAML: %v", tc.name, err)
		}
		if parsed["kind"] != "ServiceAccount" {
			t.Errorf("%s SA kind = %v, want ServiceAccount", tc.name, parsed["kind"])
		}
		meta := parsed["metadata"].(map[string]interface{})
		if meta["name"] != tc.saName {
			t.Errorf("%s SA name = %v, want %q", tc.name, meta["name"], tc.saName)
		}
		if parsed["automountServiceAccountToken"] != false {
			t.Errorf("%s SA automountServiceAccountToken = %v, want false", tc.name, parsed["automountServiceAccountToken"])
		}
	}
}

func TestParseEndpointPortRange(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		wantPort  int
		wantError bool
	}{
		{"port 1", "10.0.0.1:1", 1, false},
		{"port 443", "10.0.0.1:443", 443, false},
		{"port 65535", "10.0.0.1:65535", 65535, false},
		{"port 0", "10.0.0.1:0", 0, true},
		{"port 65536", "10.0.0.1:65536", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := parseEndpoint(tt.endpoint, 10443)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error for endpoint %q, got host=%q port=%d", tt.endpoint, host, port)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for endpoint %q: %v", tt.endpoint, err)
			}
			if port != tt.wantPort {
				t.Errorf("port = %d, want %d", port, tt.wantPort)
			}
		})
	}
}

func TestRenderRejectsLoopbackEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{"IPv4 loopback", "127.0.0.1:10443"},
		{"IPv6 loopback", "[::1]:10443"},
		{"localhost", "localhost:10443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TunnelConfig{
				SourceContext:      "src",
				DestinationContext: "dst",
				ResponderEndpoint:  tt.endpoint,
				CertValidity:       24 * time.Hour,
			}
			_, err := Render(cfg)
			if err == nil {
				t.Fatalf("expected error for loopback endpoint %q", tt.endpoint)
			}
			if !strings.Contains(err.Error(), "loopback") {
				t.Errorf("error should mention loopback, got: %v", err)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", true},
		{"::1", true},
		{"localhost", true},
		{"LOCALHOST", true},
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"example.com", false},
		{"0.0.0.0", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			got := isLoopback(tt.host)
			if got != tt.want {
				t.Errorf("isLoopback(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestRenderWithCertManager(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "tunnel.example.com:10443",
		CertValidity:       24 * time.Hour,
		CertManager:        true,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Source: namespace + SA + configmap + 3 shared cert-manager + 1 initiator cert + deployment + networkpolicy = 9
	if len(bundle.Source) != 9 {
		t.Errorf("got %d source resources, want 9", len(bundle.Source))
		for _, r := range bundle.Source {
			t.Logf("  source: %s", r.Filename)
		}
	}

	// Destination: namespace + SA + configmap + 3 shared cert-manager + 1 responder cert + deployment + service + networkpolicy = 10
	if len(bundle.Destination) != 10 {
		t.Errorf("got %d destination resources, want 10", len(bundle.Destination))
		for _, r := range bundle.Destination {
			t.Logf("  destination: %s", r.Filename)
		}
	}

	// Verify cert-manager resources exist.
	assertResourceExists(t, bundle.Source, "cert-manager-selfsigned-issuer.yaml")
	assertResourceExists(t, bundle.Source, "cert-manager-ca-certificate.yaml")
	assertResourceExists(t, bundle.Source, "cert-manager-ca-issuer.yaml")
	assertResourceExists(t, bundle.Source, "cert-manager-initiator-certificate.yaml")

	assertResourceExists(t, bundle.Destination, "cert-manager-selfsigned-issuer.yaml")
	assertResourceExists(t, bundle.Destination, "cert-manager-ca-certificate.yaml")
	assertResourceExists(t, bundle.Destination, "cert-manager-ca-issuer.yaml")
	assertResourceExists(t, bundle.Destination, "cert-manager-responder-certificate.yaml")

	// Verify no raw secret.
	if findResource(bundle.Source, "portal-tunnel-tls-secret.yaml") != nil {
		t.Error("source should not have raw TLS secret in cert-manager mode")
	}
	if findResource(bundle.Destination, "portal-tunnel-tls-secret.yaml") != nil {
		t.Error("destination should not have raw TLS secret in cert-manager mode")
	}
}

func TestRenderWithCertManagerNoCerts(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		CertManager:        true,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if bundle.Certs != nil {
		t.Error("Certs should be nil in cert-manager mode")
	}
}

func TestDeploymentSecurityStructural(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	for _, side := range []struct {
		name      string
		resources []Resource
		filename  string
	}{
		{"initiator", bundle.Source, "portal-initiator-deployment.yaml"},
		{"responder", bundle.Destination, "portal-responder-deployment.yaml"},
	} {
		r := findResource(side.resources, side.filename)
		if r == nil {
			t.Fatalf("%s deployment not found", side.name)
		}

		var dep map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &dep); err != nil {
			t.Fatalf("%s: failed to parse deployment YAML: %v", side.name, err)
		}

		spec := dep["spec"].(map[string]interface{})
		template := spec["template"].(map[string]interface{})
		podSpec := template["spec"].(map[string]interface{})
		containers := podSpec["containers"].([]interface{})
		container := containers[0].(map[string]interface{})
		secCtx := container["securityContext"].(map[string]interface{})

		// SEC-07: capabilities.drop == ["ALL"]
		caps := secCtx["capabilities"].(map[string]interface{})
		dropList := caps["drop"].([]interface{})
		if len(dropList) != 1 || dropList[0] != "ALL" {
			t.Errorf("%s: capabilities.drop = %v, want [ALL]", side.name, dropList)
		}

		// SEC-08: seccompProfile.type == "RuntimeDefault"
		seccomp := secCtx["seccompProfile"].(map[string]interface{})
		if seccomp["type"] != "RuntimeDefault" {
			t.Errorf("%s: seccompProfile.type = %v, want RuntimeDefault", side.name, seccomp["type"])
		}

		// SEC-09: resource requests and limits
		resources := container["resources"].(map[string]interface{})
		requests := resources["requests"].(map[string]interface{})
		limits := resources["limits"].(map[string]interface{})
		if requests["cpu"] != "100m" {
			t.Errorf("%s: requests.cpu = %v, want 100m", side.name, requests["cpu"])
		}
		if requests["memory"] != "128Mi" {
			t.Errorf("%s: requests.memory = %v, want 128Mi", side.name, requests["memory"])
		}
		if limits["cpu"] != "500m" {
			t.Errorf("%s: limits.cpu = %v, want 500m", side.name, limits["cpu"])
		}
		if limits["memory"] != "256Mi" {
			t.Errorf("%s: limits.memory = %v, want 256Mi", side.name, limits["memory"])
		}
	}
}

func TestInitiatorNetworkPolicyRules(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	r := findResource(bundle.Source, "portal-initiator-networkpolicy.yaml")
	if r == nil {
		t.Fatal("portal-initiator-networkpolicy.yaml not found")
	}

	var np map[string]interface{}
	if err := yaml.Unmarshal(r.Content, &np); err != nil {
		t.Fatalf("failed to parse initiator NetworkPolicy YAML: %v", err)
	}

	spec := np["spec"].(map[string]interface{})

	// SEC-13: policyTypes includes Ingress
	policyTypes := spec["policyTypes"].([]interface{})
	hasIngress := false
	hasEgress := false
	for _, pt := range policyTypes {
		if pt == "Ingress" {
			hasIngress = true
		}
		if pt == "Egress" {
			hasEgress = true
		}
	}
	if !hasIngress {
		t.Error("initiator NetworkPolicy policyTypes missing Ingress")
	}
	if !hasEgress {
		t.Error("initiator NetworkPolicy policyTypes missing Egress")
	}

	// SEC-13: Ingress rule allows TCP on tunnel port from any namespace.
	ingressRules := spec["ingress"].([]interface{})
	if len(ingressRules) < 1 {
		t.Fatal("initiator NetworkPolicy has no ingress rules")
	}
	ingressRule := ingressRules[0].(map[string]interface{})
	ingressPorts := ingressRule["ports"].([]interface{})
	ingressPort := ingressPorts[0].(map[string]interface{})
	if ingressPort["protocol"] != "TCP" {
		t.Errorf("initiator ingress port protocol = %v, want TCP", ingressPort["protocol"])
	}
	if ingressPort["port"] != 10443 {
		t.Errorf("initiator ingress port = %v, want 10443", ingressPort["port"])
	}
	// Verify from uses namespaceSelector: {} (any namespace).
	from := ingressRule["from"].([]interface{})
	fromEntry := from[0].(map[string]interface{})
	nsSelector := fromEntry["namespaceSelector"].(map[string]interface{})
	if len(nsSelector) != 0 {
		t.Errorf("initiator ingress namespaceSelector should be empty (any namespace), got %v", nsSelector)
	}

	// SEC-14: Exactly 2 egress rules — DNS (UDP 53) and tunnel (TCP on tunnel port).
	egressRules := spec["egress"].([]interface{})
	if len(egressRules) != 2 {
		t.Fatalf("initiator NetworkPolicy has %d egress rules, want 2", len(egressRules))
	}
	// DNS rule.
	dnsRule := egressRules[0].(map[string]interface{})
	dnsPorts := dnsRule["ports"].([]interface{})
	dnsPort := dnsPorts[0].(map[string]interface{})
	if dnsPort["protocol"] != "UDP" {
		t.Errorf("initiator egress DNS port protocol = %v, want UDP", dnsPort["protocol"])
	}
	if dnsPort["port"] != 53 {
		t.Errorf("initiator egress DNS port = %v, want 53", dnsPort["port"])
	}
	// Tunnel rule.
	tunnelRule := egressRules[1].(map[string]interface{})
	tunnelPorts := tunnelRule["ports"].([]interface{})
	tunnelPort := tunnelPorts[0].(map[string]interface{})
	if tunnelPort["protocol"] != "TCP" {
		t.Errorf("initiator egress tunnel port protocol = %v, want TCP", tunnelPort["protocol"])
	}
	if tunnelPort["port"] != 10443 {
		t.Errorf("initiator egress tunnel port = %v, want 10443", tunnelPort["port"])
	}
}

func TestResponderNetworkPolicyRules(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	r := findResource(bundle.Destination, "portal-responder-networkpolicy.yaml")
	if r == nil {
		t.Fatal("portal-responder-networkpolicy.yaml not found")
	}

	var np map[string]interface{}
	if err := yaml.Unmarshal(r.Content, &np); err != nil {
		t.Fatalf("failed to parse responder NetworkPolicy YAML: %v", err)
	}

	spec := np["spec"].(map[string]interface{})

	// SEC-15: Ingress allows TCP on tunnel port with NO from restriction.
	ingressRules := spec["ingress"].([]interface{})
	if len(ingressRules) < 1 {
		t.Fatal("responder NetworkPolicy has no ingress rules")
	}
	ingressRule := ingressRules[0].(map[string]interface{})
	ingressPorts := ingressRule["ports"].([]interface{})
	ingressPort := ingressPorts[0].(map[string]interface{})
	if ingressPort["protocol"] != "TCP" {
		t.Errorf("responder ingress port protocol = %v, want TCP", ingressPort["protocol"])
	}
	if ingressPort["port"] != 10443 {
		t.Errorf("responder ingress port = %v, want 10443", ingressPort["port"])
	}
	// No "from" field means accept from any source (including external).
	if _, hasFrom := ingressRule["from"]; hasFrom {
		t.Error("responder ingress rule should have no 'from' restriction (accepts external traffic)")
	}

	// SEC-16: Egress includes DNS (UDP 53) + open egress rule (empty map for backend forwarding).
	egressRules := spec["egress"].([]interface{})
	if len(egressRules) != 2 {
		t.Fatalf("responder NetworkPolicy has %d egress rules, want 2", len(egressRules))
	}
	// DNS rule.
	dnsRule := egressRules[0].(map[string]interface{})
	dnsPorts := dnsRule["ports"].([]interface{})
	dnsPort := dnsPorts[0].(map[string]interface{})
	if dnsPort["protocol"] != "UDP" {
		t.Errorf("responder egress DNS port protocol = %v, want UDP", dnsPort["protocol"])
	}
	if dnsPort["port"] != 53 {
		t.Errorf("responder egress DNS port = %v, want 53", dnsPort["port"])
	}
	// Open egress rule (empty map — allows any in-cluster destination).
	openRule := egressRules[1].(map[string]interface{})
	if len(openRule) != 0 {
		t.Errorf("responder egress open rule should be empty map {}, got %v", openRule)
	}
}

func TestInitiatorNoService(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	for _, r := range bundle.Source {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Fatalf("%s: failed to parse YAML: %v", r.Filename, err)
		}
		if parsed["kind"] == "Service" {
			t.Errorf("source resources contain a Service (%s); initiator should have no Service", r.Filename)
		}
	}
}

func TestGitignoreContent(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	outputDir := t.TempDir()
	if err := WriteToDisk(bundle, outputDir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	gitignoreBytes, err := os.ReadFile(filepath.Join(outputDir, "ca", ".gitignore"))
	if err != nil {
		t.Fatalf("failed to read ca/.gitignore: %v", err)
	}
	if string(gitignoreBytes) != "*\n" {
		t.Errorf("ca/.gitignore content = %q, want %q", string(gitignoreBytes), "*\n")
	}
}

func TestRenderMultiService(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		Services: []ServiceConfig{
			{SNI: "backend", BackendHost: "backend-svc.synapse.svc", BackendPort: 8443, LocalPort: 18443},
			{SNI: "otel", BackendHost: "otel-collector.synapse.svc", BackendPort: 4317},
		},
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Verify multi-service bootstrap (responder should have tls_inspector).
	responderCM := findResource(bundle.Destination, "portal-responder-bootstrap-cm.yaml")
	if responderCM == nil {
		t.Fatal("portal-responder-bootstrap-cm.yaml not found")
	}
	responderContent := string(responderCM.Content)
	if !strings.Contains(responderContent, "tls_inspector") {
		t.Error("responder bootstrap should contain tls_inspector for multi-service")
	}
	if !strings.Contains(responderContent, "backend") {
		t.Error("responder bootstrap should reference 'backend' service")
	}
	if !strings.Contains(responderContent, "otel") {
		t.Error("responder bootstrap should reference 'otel' service")
	}

	// Verify initiator bootstrap has per-service listeners.
	initiatorCM := findResource(bundle.Source, "portal-initiator-bootstrap-cm.yaml")
	if initiatorCM == nil {
		t.Fatal("portal-initiator-bootstrap-cm.yaml not found")
	}
	initiatorContent := string(initiatorCM.Content)
	if !strings.Contains(initiatorContent, "18443") {
		t.Error("initiator bootstrap should contain custom local port 18443 for backend")
	}
	if !strings.Contains(initiatorContent, "4317") {
		t.Error("initiator bootstrap should contain port 4317 for otel")
	}

	// Verify initiator deployment has per-service container ports.
	initDep := findResource(bundle.Source, "portal-initiator-deployment.yaml")
	if initDep == nil {
		t.Fatal("portal-initiator-deployment.yaml not found")
	}
	depContent := string(initDep.Content)
	if !strings.Contains(depContent, "svc-backend") {
		t.Error("initiator deployment should have container port named svc-backend")
	}
	if !strings.Contains(depContent, "svc-otel") {
		t.Error("initiator deployment should have container port named svc-otel")
	}

	// Verify initiator NetworkPolicy allows per-service ports.
	initNP := findResource(bundle.Source, "portal-initiator-networkpolicy.yaml")
	if initNP == nil {
		t.Fatal("portal-initiator-networkpolicy.yaml not found")
	}
	npContent := string(initNP.Content)
	if !strings.Contains(npContent, "18443") {
		t.Error("initiator NetworkPolicy should allow port 18443")
	}
	if !strings.Contains(npContent, "4317") {
		t.Error("initiator NetworkPolicy should allow port 4317")
	}

	// Verify metadata includes services.
	if len(bundle.Metadata.Services) != 2 {
		t.Errorf("Metadata.Services has %d entries, want 2", len(bundle.Metadata.Services))
	}

	// Verify all resources are valid YAML.
	for _, r := range append(bundle.Source, bundle.Destination...) {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Errorf("%s is not valid YAML: %v", r.Filename, err)
		}
	}
}

func TestRenderWithExternalCerts(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		ExternalCerts: &ExternalCertificates{
			CACert:        []byte("fake-ca-cert"),
			InitiatorCert: []byte("fake-init-cert"),
			InitiatorKey:  []byte("fake-init-key"),
			ResponderCert: []byte("fake-resp-cert"),
			ResponderKey:  []byte("fake-resp-key"),
		},
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Certs should be nil (not generated).
	if bundle.Certs != nil {
		t.Error("Certs should be nil when using external certificates")
	}

	// Source should have a secret with the external initiator cert.
	srcSecret := findResource(bundle.Source, "portal-tunnel-tls-secret.yaml")
	if srcSecret == nil {
		t.Fatal("source secret not found")
	}

	// Destination should have a secret with the external responder cert.
	dstSecret := findResource(bundle.Destination, "portal-tunnel-tls-secret.yaml")
	if dstSecret == nil {
		t.Fatal("destination secret not found")
	}
}

func TestRenderWithSplitCertDirs(t *testing.T) {
	// Create temp cert directories with test files.
	initDir := filepath.Join(t.TempDir(), "initiator")
	respDir := filepath.Join(t.TempDir(), "responder")
	if err := os.MkdirAll(initDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(respDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Write test cert files.
	for _, dir := range []string{initDir, respDir} {
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("cert-data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("key-data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("ca-data"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		InitiatorCertDir:   initDir,
		ResponderCertDir:   respDir,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Should not generate certs.
	if bundle.Certs != nil {
		t.Error("Certs should be nil when using cert directories")
	}

	// Both sides should have secrets.
	assertResourceExists(t, bundle.Source, "portal-tunnel-tls-secret.yaml")
	assertResourceExists(t, bundle.Destination, "portal-tunnel-tls-secret.yaml")
}

func TestRenderWithSplitCertDirsMissingOne(t *testing.T) {
	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		InitiatorCertDir:   "/some/path",
		// ResponderCertDir intentionally missing
	}

	_, err := Render(cfg)
	if err == nil {
		t.Fatal("expected error when only one cert dir is specified")
	}
	if !strings.Contains(err.Error(), "both") {
		t.Errorf("error should mention 'both', got: %v", err)
	}
}

func TestRenderWithSharedCertDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("cert"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("ca"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := TunnelConfig{
		SourceContext:      "src",
		DestinationContext: "dst",
		ResponderEndpoint:  "10.0.0.1:10443",
		CertValidity:       24 * time.Hour,
		CertDir:            dir,
	}

	bundle, err := Render(cfg)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if bundle.Certs != nil {
		t.Error("Certs should be nil when using CertDir")
	}
	assertResourceExists(t, bundle.Source, "portal-tunnel-tls-secret.yaml")
	assertResourceExists(t, bundle.Destination, "portal-tunnel-tls-secret.yaml")
}

func assertResourceExists(t *testing.T, resources []Resource, filename string) {
	t.Helper()
	if findResource(resources, filename) == nil {
		t.Errorf("resource %q not found", filename)
	}
}

func findResource(resources []Resource, filename string) *Resource {
	for i := range resources {
		if resources[i].Filename == filename {
			return &resources[i]
		}
	}
	return nil
}
