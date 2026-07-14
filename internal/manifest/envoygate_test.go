package manifest

import (
	"strings"
	"testing"
)

func TestCheckEnvoyImage(t *testing.T) {
	cases := []struct {
		image   string
		allow   bool
		wantErr string
	}{
		{DefaultEnvoyImage, false, ""},
		{"envoyproxy/envoy:v1.37.2", false, ""},
		{"envoyproxy/envoy:v1.38-latest", false, "not supported"},
		{"envoyproxy/envoy:v1.36.0", false, "not supported"},
		{"envoyproxy/envoy:latest", false, "cannot determine"},
		{"envoyproxy/envoy:v1.38-latest", true, ""},
		{"custom.registry/envoy:v1.37.1-distroless@sha256:abc", false, ""},
	}
	for _, tc := range cases {
		err := CheckEnvoyImage(tc.image, tc.allow)
		if tc.wantErr == "" && err != nil {
			t.Errorf("CheckEnvoyImage(%q, %v) = %v, want nil", tc.image, tc.allow, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("CheckEnvoyImage(%q, %v) = %v, want error containing %q", tc.image, tc.allow, err, tc.wantErr)
		}
	}
}

func TestRenderHubManifestsGatesEnvoyVersion(t *testing.T) {
	certPEM, keyPEM, caPEM, _ := testHubTLS(t)
	cfg := HubDeployConfig{
		HubName: "synapse", EnvoyImage: "envoyproxy/envoy:v1.99",
		CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caPEM,
	}
	if _, err := RenderHubManifests(cfg); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected version gate error, got: %v", err)
	}
	cfg.AllowUnsupportedEnvoy = true
	if _, err := RenderHubManifests(cfg); err != nil {
		t.Errorf("escape hatch did not bypass gate: %v", err)
	}
}

func TestRenderMemberManifestsGatesEnvoyVersion(t *testing.T) {
	cfg := MemberDeployConfig{
		MemberName: "acme-prod", HubName: "synapse",
		HubAddr: "tunnel.example:10443", EnvoyImage: "envoyproxy/envoy:v1.99",
	}
	if _, err := RenderMemberManifests(cfg); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("expected version gate error, got: %v", err)
	}
}
