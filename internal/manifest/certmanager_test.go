package manifest

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestBuildCertManagerResources(t *testing.T) {
	source, dest, shared, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, []string{"tunnel.example.com"})

	if len(shared) != 3 {
		t.Errorf("got %d shared resources, want 3", len(shared))
	}
	if len(source) != 1 {
		t.Errorf("got %d source resources, want 1", len(source))
	}
	if len(dest) != 1 {
		t.Errorf("got %d destination resources, want 1", len(dest))
	}

	// Verify filenames.
	expectedShared := []string{
		"cert-manager-selfsigned-issuer.yaml",
		"cert-manager-ca-certificate.yaml",
		"cert-manager-ca-issuer.yaml",
	}
	for i, want := range expectedShared {
		if shared[i].Filename != want {
			t.Errorf("shared[%d].Filename = %q, want %q", i, shared[i].Filename, want)
		}
	}
	if source[0].Filename != "cert-manager-initiator-certificate.yaml" {
		t.Errorf("source[0].Filename = %q, want %q", source[0].Filename, "cert-manager-initiator-certificate.yaml")
	}
	if dest[0].Filename != "cert-manager-responder-certificate.yaml" {
		t.Errorf("dest[0].Filename = %q, want %q", dest[0].Filename, "cert-manager-responder-certificate.yaml")
	}
}

func TestCertManagerSecretNameAlignment(t *testing.T) {
	source, dest, _, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, []string{"tunnel.example.com"})

	// Both leaf certificates should use secretName: portal-tunnel-tls.
	for _, tc := range []struct {
		name     string
		resource Resource
	}{
		{"initiator", source[0]},
		{"responder", dest[0]},
	} {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(tc.resource.Content, &parsed); err != nil {
			t.Fatalf("%s is not valid YAML: %v", tc.name, err)
		}
		spec := parsed["spec"].(map[string]interface{})
		if spec["secretName"] != "portal-tunnel-tls" {
			t.Errorf("%s secretName = %v, want %q", tc.name, spec["secretName"], "portal-tunnel-tls")
		}
	}
}

func TestCertManagerInitiatorUsages(t *testing.T) {
	source, _, _, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, nil)
	content := string(source[0].Content)
	if !strings.Contains(content, "client auth") {
		t.Error("initiator certificate should contain 'client auth' usage")
	}
}

func TestCertManagerResponderSANs(t *testing.T) {
	_, dest, _, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, []string{"tunnel.example.com", "10.0.0.1"})

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(dest[0].Content, &parsed); err != nil {
		t.Fatalf("responder is not valid YAML: %v", err)
	}
	spec := parsed["spec"].(map[string]interface{})

	dnsNames, ok := spec["dnsNames"].([]interface{})
	if !ok || len(dnsNames) != 1 {
		t.Errorf("expected 1 dnsName, got %v", spec["dnsNames"])
	} else if dnsNames[0] != "tunnel.example.com" {
		t.Errorf("dnsNames[0] = %v, want %q", dnsNames[0], "tunnel.example.com")
	}

	ipAddresses, ok := spec["ipAddresses"].([]interface{})
	if !ok || len(ipAddresses) != 1 {
		t.Errorf("expected 1 ipAddress, got %v", spec["ipAddresses"])
	} else if ipAddresses[0] != "10.0.0.1" {
		t.Errorf("ipAddresses[0] = %v, want %q", ipAddresses[0], "10.0.0.1")
	}
}

func TestCertManagerResponderUsages(t *testing.T) {
	_, dest, _, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, nil)
	content := string(dest[0].Content)
	if !strings.Contains(content, "server auth") {
		t.Error("responder certificate should contain 'server auth' usage")
	}
}

func TestCertManagerRenewBeforeCalculation(t *testing.T) {
	validity := 8760 * time.Hour // 1 year
	source, dest, _, _ := buildCertManagerResources("test-tunnel", "portal-system", validity, nil)
	wantRenewBefore := (validity / 3).String()

	for _, tc := range []struct {
		name     string
		resource Resource
	}{
		{"initiator", source[0]},
		{"responder", dest[0]},
	} {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(tc.resource.Content, &parsed); err != nil {
			t.Fatalf("%s is not valid YAML: %v", tc.name, err)
		}
		spec := parsed["spec"].(map[string]interface{})
		renewBefore, ok := spec["renewBefore"].(string)
		if !ok {
			t.Errorf("%s certificate missing renewBefore field", tc.name)
			continue
		}
		if renewBefore != wantRenewBefore {
			t.Errorf("%s renewBefore = %q, want %q", tc.name, renewBefore, wantRenewBefore)
		}
	}
}

func TestCertManagerIssuerRefChain(t *testing.T) {
	source, dest, shared, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, nil)

	// CA certificate should reference the selfsigned issuer.
	var caCert map[string]interface{}
	if err := yaml.Unmarshal(shared[1].Content, &caCert); err != nil {
		t.Fatalf("CA certificate is not valid YAML: %v", err)
	}
	caSpec := caCert["spec"].(map[string]interface{})
	caIssuerRef := caSpec["issuerRef"].(map[string]interface{})
	if caIssuerRef["name"] != "portal-selfsigned-issuer" {
		t.Errorf("CA cert issuerRef.name = %v, want %q", caIssuerRef["name"], "portal-selfsigned-issuer")
	}
	if caIssuerRef["kind"] != "Issuer" {
		t.Errorf("CA cert issuerRef.kind = %v, want %q", caIssuerRef["kind"], "Issuer")
	}
	if caIssuerRef["group"] != "cert-manager.io" {
		t.Errorf("CA cert issuerRef.group = %v, want %q", caIssuerRef["group"], "cert-manager.io")
	}

	// Leaf certificates should reference the CA issuer.
	for _, tc := range []struct {
		name     string
		resource Resource
	}{
		{"initiator", source[0]},
		{"responder", dest[0]},
	} {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(tc.resource.Content, &parsed); err != nil {
			t.Fatalf("%s is not valid YAML: %v", tc.name, err)
		}
		spec := parsed["spec"].(map[string]interface{})
		issuerRef := spec["issuerRef"].(map[string]interface{})
		if issuerRef["name"] != "portal-ca-issuer" {
			t.Errorf("%s issuerRef.name = %v, want %q", tc.name, issuerRef["name"], "portal-ca-issuer")
		}
		if issuerRef["kind"] != "Issuer" {
			t.Errorf("%s issuerRef.kind = %v, want %q", tc.name, issuerRef["kind"], "Issuer")
		}
		if issuerRef["group"] != "cert-manager.io" {
			t.Errorf("%s issuerRef.group = %v, want %q", tc.name, issuerRef["group"], "cert-manager.io")
		}
	}
}

func TestCertManagerNamespacePropagation(t *testing.T) {
	customNS := "custom-tunnel-ns"
	source, dest, shared, _ := buildCertManagerResources("test-tunnel", customNS, 8760*time.Hour, nil)

	allResources := append(shared, source...)
	allResources = append(allResources, dest...)

	for _, r := range allResources {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal(r.Content, &parsed); err != nil {
			t.Fatalf("%s is not valid YAML: %v", r.Filename, err)
		}
		meta := parsed["metadata"].(map[string]interface{})
		ns, ok := meta["namespace"].(string)
		if !ok {
			t.Errorf("%s: missing namespace in metadata", r.Filename)
			continue
		}
		if ns != customNS {
			t.Errorf("%s: namespace = %q, want %q", r.Filename, ns, customNS)
		}
	}
}

func TestCertManagerCAIssuerSecretName(t *testing.T) {
	_, _, shared, _ := buildCertManagerResources("test-tunnel", "portal-system", 8760*time.Hour, nil)

	// CA Issuer is the third shared resource (index 2).
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(shared[2].Content, &parsed); err != nil {
		t.Fatalf("CA Issuer is not valid YAML: %v", err)
	}
	spec := parsed["spec"].(map[string]interface{})
	ca := spec["ca"].(map[string]interface{})
	secretName, ok := ca["secretName"].(string)
	if !ok {
		t.Fatal("CA Issuer missing spec.ca.secretName")
	}
	if secretName != "portal-tunnel-ca" {
		t.Errorf("CA Issuer secretName = %q, want %q", secretName, "portal-tunnel-ca")
	}
}

func TestCertManagerCADuration(t *testing.T) {
	leafValidity := 8760 * time.Hour // 1 year
	_, _, shared, _ := buildCertManagerResources("test-tunnel", "portal-system", leafValidity, nil)

	// CA certificate is the second shared resource.
	var parsed map[string]interface{}
	if err := yaml.Unmarshal(shared[1].Content, &parsed); err != nil {
		t.Fatalf("CA certificate is not valid YAML: %v", err)
	}
	spec := parsed["spec"].(map[string]interface{})
	duration := spec["duration"].(string)
	wantDuration := (leafValidity * 3).String()
	if duration != wantDuration {
		t.Errorf("CA duration = %q, want %q", duration, wantDuration)
	}
}
