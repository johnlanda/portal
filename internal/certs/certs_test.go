package certs

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

func TestGenerateTunnelCertificates(t *testing.T) {
	tunnelName := "gke-us-east--eks-eu-west"
	sans := []string{"tunnel.example.com", "10.0.0.1"}
	validity := 24 * time.Hour

	certs, err := GenerateTunnelCertificates(tunnelName, sans, validity)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	// Verify all fields are non-empty.
	if len(certs.CACert) == 0 {
		t.Error("CACert is empty")
	}
	if len(certs.CAKey) == 0 {
		t.Error("CAKey is empty")
	}
	if len(certs.InitiatorCert) == 0 {
		t.Error("InitiatorCert is empty")
	}
	if len(certs.InitiatorKey) == 0 {
		t.Error("InitiatorKey is empty")
	}
	if len(certs.ResponderCert) == 0 {
		t.Error("ResponderCert is empty")
	}
	if len(certs.ResponderKey) == 0 {
		t.Error("ResponderKey is empty")
	}

	// Parse and verify the CA certificate.
	caCert := parseCert(t, certs.CACert)
	if caCert.Subject.CommonName != CACommonName {
		t.Errorf("CA CN = %q, want %q", caCert.Subject.CommonName, CACommonName)
	}
	if !caCert.IsCA {
		t.Error("CA cert IsCA = false, want true")
	}

	// Parse and verify the initiator certificate.
	initiatorCert := parseCert(t, certs.InitiatorCert)
	wantInitiatorCN := InitiatorCNPrefix + tunnelName
	if initiatorCert.Subject.CommonName != wantInitiatorCN {
		t.Errorf("Initiator CN = %q, want %q", initiatorCert.Subject.CommonName, wantInitiatorCN)
	}
	if len(initiatorCert.ExtKeyUsage) == 0 || initiatorCert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Error("Initiator cert should have ClientAuth extended key usage")
	}

	// Verify initiator cert is signed by the CA.
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	if _, err := initiatorCert.Verify(x509.VerifyOptions{
		Roots:     caPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("Initiator cert failed CA verification: %v", err)
	}

	// Parse and verify the responder certificate.
	responderCert := parseCert(t, certs.ResponderCert)
	wantResponderCN := ResponderCNPrefix + tunnelName
	if responderCert.Subject.CommonName != wantResponderCN {
		t.Errorf("Responder CN = %q, want %q", responderCert.Subject.CommonName, wantResponderCN)
	}
	if len(responderCert.ExtKeyUsage) == 0 || responderCert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Error("Responder cert should have ServerAuth extended key usage")
	}

	// Verify responder SANs.
	foundDNS := false
	for _, dns := range responderCert.DNSNames {
		if dns == "tunnel.example.com" {
			foundDNS = true
			break
		}
	}
	if !foundDNS {
		t.Errorf("Responder cert DNS SANs = %v, want to contain %q", responderCert.DNSNames, "tunnel.example.com")
	}

	foundIP := false
	wantIP := net.ParseIP("10.0.0.1")
	for _, ip := range responderCert.IPAddresses {
		if ip.Equal(wantIP) {
			foundIP = true
			break
		}
	}
	if !foundIP {
		t.Errorf("Responder cert IP SANs = %v, want to contain %v", responderCert.IPAddresses, wantIP)
	}

	// Verify responder cert is signed by the CA.
	if _, err := responderCert.Verify(x509.VerifyOptions{
		Roots:     caPool,
		DNSName:   "tunnel.example.com",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("Responder cert failed CA verification: %v", err)
	}
}

func TestGenerateTunnelCertificatesDefaultValidity(t *testing.T) {
	certs, err := GenerateTunnelCertificates("test-tunnel", nil, 0)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	caCert := parseCert(t, certs.CACert)
	// Default validity should be ~1 year from now.
	expectedExpiry := time.Now().Add(DefaultCertificateValidity)
	diff := caCert.NotAfter.Sub(expectedExpiry)
	if diff < -time.Minute || diff > time.Minute {
		t.Errorf("CA cert expiry = %v, want ~%v (diff: %v)", caCert.NotAfter, expectedExpiry, diff)
	}
}

func TestSplitNamesAndIPs(t *testing.T) {
	names := []string{"example.com", "10.0.0.1", "dns.local", "192.168.1.1", "::1"}
	dns, ips := splitNamesAndIPs(names)

	if len(dns) != 2 {
		t.Errorf("got %d DNS names, want 2: %v", len(dns), dns)
	}
	if len(ips) != 3 {
		t.Errorf("got %d IPs, want 3: %v", len(ips), ips)
	}
}

func TestGenerateTunnelCertificatesIPOnly(t *testing.T) {
	// SANs with only IP addresses (no DNS names).
	sans := []string{"10.0.0.1", "192.168.1.1"}
	certs, err := GenerateTunnelCertificates("ip-tunnel", sans, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	responderCert := parseCert(t, certs.ResponderCert)

	// Should have IP SANs but no DNS SANs.
	if len(responderCert.DNSNames) != 0 {
		t.Errorf("expected no DNS SANs, got %v", responderCert.DNSNames)
	}
	if len(responderCert.IPAddresses) != 2 {
		t.Errorf("expected 2 IP SANs, got %d: %v", len(responderCert.IPAddresses), responderCert.IPAddresses)
	}

	wantIPs := []string{"10.0.0.1", "192.168.1.1"}
	for _, wantStr := range wantIPs {
		wantIP := net.ParseIP(wantStr)
		found := false
		for _, ip := range responderCert.IPAddresses {
			if ip.Equal(wantIP) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("responder cert IP SANs = %v, want to contain %v", responderCert.IPAddresses, wantIP)
		}
	}
}

func TestGenerateTunnelCertificatesEmptySANs(t *testing.T) {
	// Empty SANs list — responder cert should still have the CN.
	certs, err := GenerateTunnelCertificates("empty-sans-tunnel", nil, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	responderCert := parseCert(t, certs.ResponderCert)

	wantCN := ResponderCNPrefix + "empty-sans-tunnel"
	if responderCert.Subject.CommonName != wantCN {
		t.Errorf("Responder CN = %q, want %q", responderCert.Subject.CommonName, wantCN)
	}
	if len(responderCert.DNSNames) != 0 {
		t.Errorf("expected no DNS SANs, got %v", responderCert.DNSNames)
	}
	if len(responderCert.IPAddresses) != 0 {
		t.Errorf("expected no IP SANs, got %v", responderCert.IPAddresses)
	}
}

func TestCertificateExpiry(t *testing.T) {
	validity := 48 * time.Hour
	certs, err := GenerateTunnelCertificates("expiry-test", []string{"example.com"}, validity)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	for _, tc := range []struct {
		name    string
		pemData []byte
	}{
		{"CA", certs.CACert},
		{"Initiator", certs.InitiatorCert},
		{"Responder", certs.ResponderCert},
	} {
		cert := parseCert(t, tc.pemData)
		expectedExpiry := time.Now().Add(validity)
		diff := cert.NotAfter.Sub(expectedExpiry)
		if diff < -time.Minute || diff > time.Minute {
			t.Errorf("%s cert NotAfter = %v, want ~%v (diff: %v)", tc.name, cert.NotAfter, expectedExpiry, diff)
		}
	}
}

func TestRotateLeafCertificates(t *testing.T) {
	tunnelName := "rotate-test"
	sans := []string{"tunnel.example.com", "10.0.0.1"}
	validity := 24 * time.Hour

	// First generate the original certs to get a CA.
	original, err := GenerateTunnelCertificates(tunnelName, sans, validity)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	// Rotate using the same CA.
	rotated, err := RotateLeafCertificates(original.CACert, original.CAKey, tunnelName, sans, validity)
	if err != nil {
		t.Fatalf("RotateLeafCertificates() error = %v", err)
	}

	// CA should be unchanged.
	if string(rotated.CACert) != string(original.CACert) {
		t.Error("rotated CACert should be identical to original")
	}
	if string(rotated.CAKey) != string(original.CAKey) {
		t.Error("rotated CAKey should be identical to original")
	}

	// Leaf certs should be different.
	if string(rotated.InitiatorCert) == string(original.InitiatorCert) {
		t.Error("rotated InitiatorCert should differ from original")
	}
	if string(rotated.ResponderCert) == string(original.ResponderCert) {
		t.Error("rotated ResponderCert should differ from original")
	}

	// Parse and verify the rotated initiator cert.
	initCert := parseCert(t, rotated.InitiatorCert)
	wantCN := InitiatorCNPrefix + tunnelName
	if initCert.Subject.CommonName != wantCN {
		t.Errorf("rotated initiator CN = %q, want %q", initCert.Subject.CommonName, wantCN)
	}
	if len(initCert.ExtKeyUsage) == 0 || initCert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Error("rotated initiator cert should have ClientAuth key usage")
	}

	// Parse and verify the rotated responder cert.
	respCert := parseCert(t, rotated.ResponderCert)
	wantRespCN := ResponderCNPrefix + tunnelName
	if respCert.Subject.CommonName != wantRespCN {
		t.Errorf("rotated responder CN = %q, want %q", respCert.Subject.CommonName, wantRespCN)
	}
	if len(respCert.ExtKeyUsage) == 0 || respCert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Error("rotated responder cert should have ServerAuth key usage")
	}

	// Verify SANs on rotated responder.
	foundDNS := false
	for _, dns := range respCert.DNSNames {
		if dns == "tunnel.example.com" {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Errorf("rotated responder DNS SANs = %v, want to contain %q", respCert.DNSNames, "tunnel.example.com")
	}
	foundIP := false
	wantIP := net.ParseIP("10.0.0.1")
	for _, ip := range respCert.IPAddresses {
		if ip.Equal(wantIP) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("rotated responder IP SANs = %v, want to contain %v", respCert.IPAddresses, wantIP)
	}
}

func TestRotateLeafCertificatesChainValid(t *testing.T) {
	tunnelName := "chain-test"
	sans := []string{"tunnel.example.com"}
	validity := 24 * time.Hour

	original, err := GenerateTunnelCertificates(tunnelName, sans, validity)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	rotated, err := RotateLeafCertificates(original.CACert, original.CAKey, tunnelName, sans, validity)
	if err != nil {
		t.Fatalf("RotateLeafCertificates() error = %v", err)
	}

	// Both original and rotated certs should validate against the same CA.
	caCert := parseCert(t, original.CACert)
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	for _, tc := range []struct {
		name    string
		pemData []byte
		usage   x509.ExtKeyUsage
		dns     string
	}{
		{"original-initiator", original.InitiatorCert, x509.ExtKeyUsageClientAuth, ""},
		{"original-responder", original.ResponderCert, x509.ExtKeyUsageServerAuth, "tunnel.example.com"},
		{"rotated-initiator", rotated.InitiatorCert, x509.ExtKeyUsageClientAuth, ""},
		{"rotated-responder", rotated.ResponderCert, x509.ExtKeyUsageServerAuth, "tunnel.example.com"},
	} {
		cert := parseCert(t, tc.pemData)
		opts := x509.VerifyOptions{
			Roots:     caPool,
			KeyUsages: []x509.ExtKeyUsage{tc.usage},
		}
		if tc.dns != "" {
			opts.DNSName = tc.dns
		}
		if _, err := cert.Verify(opts); err != nil {
			t.Errorf("%s failed CA verification: %v", tc.name, err)
		}
	}
}

func TestRotateLeafCertificatesInvalidCA(t *testing.T) {
	_, err := RotateLeafCertificates([]byte("not-a-cert"), []byte("not-a-key"), "test", nil, 24*time.Hour)
	if err == nil {
		t.Fatal("expected error with invalid CA material")
	}
}

func TestRSA4096KeySize(t *testing.T) {
	tc, err := GenerateTunnelCertificates("key-size-test", []string{"example.com"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates() error = %v", err)
	}

	for _, kc := range []struct {
		name   string
		keyPEM []byte
	}{
		{"CA", tc.CAKey},
		{"Initiator", tc.InitiatorKey},
		{"Responder", tc.ResponderKey},
	} {
		block, _ := pem.Decode(kc.keyPEM)
		if block == nil {
			t.Fatalf("%s: failed to decode PEM block", kc.name)
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("%s: failed to parse PKCS1 private key: %v", kc.name, err)
		}
		if key.N.BitLen() != 4096 {
			t.Errorf("%s: key size = %d bits, want 4096", kc.name, key.N.BitLen())
		}
	}
}

func TestPerTunnelCAIsolation(t *testing.T) {
	certsA, err := GenerateTunnelCertificates("tunnel-a", []string{"a.example.com"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates(tunnel-a) error = %v", err)
	}
	certsB, err := GenerateTunnelCertificates("tunnel-b", []string{"b.example.com"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateTunnelCertificates(tunnel-b) error = %v", err)
	}

	caA := parseCert(t, certsA.CACert)
	caB := parseCert(t, certsB.CACert)

	// CA public keys must differ between tunnels.
	pubA := caA.PublicKey.(*rsa.PublicKey)
	pubB := caB.PublicKey.(*rsa.PublicKey)
	if pubA.N.Cmp(pubB.N) == 0 {
		t.Error("tunnel-a and tunnel-b CA public keys are identical; expected unique per-tunnel CAs")
	}

	// Cross-verification must fail: tunnel-A initiator cert must NOT verify against tunnel-B's CA.
	poolB := x509.NewCertPool()
	poolB.AddCert(caB)
	initA := parseCert(t, certsA.InitiatorCert)
	_, err = initA.Verify(x509.VerifyOptions{
		Roots:     poolB,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err == nil {
		t.Error("tunnel-A initiator cert should NOT verify against tunnel-B CA, but did")
	}
}

func parseCert(t *testing.T, pemData []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemData)
	if block == nil {
		t.Fatal("failed to decode PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}
	return cert
}
