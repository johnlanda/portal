package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func mustParseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}
	return cert
}

func testIdentity() MemberIdentity {
	return MemberIdentity{Member: "acme-prod", Tenant: "synapse"}
}

func TestNewHubCARoundTrip(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}
	reloaded, err := LoadHubCA(ca.CertPEM(), ca.KeyPEM())
	if err != nil {
		t.Fatalf("LoadHubCA() error = %v", err)
	}
	cert := mustParseCert(t, reloaded.CertPEM())
	if !cert.IsCA {
		t.Error("hub CA certificate does not have IsCA set")
	}
	if cert.Subject.CommonName != "portal-hub-ca/synapse" {
		t.Errorf("hub CA CN = %q, want portal-hub-ca/synapse", cert.Subject.CommonName)
	}
	if cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Error("hub CA certificate cannot sign CRLs")
	}
}

func TestNewHubCAInvalidName(t *testing.T) {
	if _, err := NewHubCA("bad name", time.Hour); err == nil {
		t.Error("expected error for invalid hub name")
	}
}

func TestSignCSRIssuesSANBoundIdentity(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}
	keyPEM, csrPEM, err := GenerateMemberKeyAndCSR(testIdentity())
	if err != nil {
		t.Fatalf("GenerateMemberKeyAndCSR() error = %v", err)
	}
	certPEM, err := ca.SignCSR(csrPEM, testIdentity(), time.Hour)
	if err != nil {
		t.Fatalf("SignCSR() error = %v", err)
	}
	cert := mustParseCert(t, certPEM)

	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "acme-prod" {
		t.Errorf("DNS SANs = %v, want exactly [acme-prod]", cert.DNSNames)
	}
	if cert.Subject.CommonName != MemberCNPrefix+"acme-prod" {
		t.Errorf("CN = %q, want %q", cert.Subject.CommonName, MemberCNPrefix+"acme-prod")
	}
	if len(cert.Subject.OrganizationalUnit) != 1 || cert.Subject.OrganizationalUnit[0] != "synapse" {
		t.Errorf("OU = %v, want [synapse]", cert.Subject.OrganizationalUnit)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Error("member certificate is not a client certificate")
	}

	// Chain verifies against the hub CA.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("failed to add hub CA to pool")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("member certificate does not verify against hub CA: %v", err)
	}

	// Issued cert carries the key from the CSR (proves the member's key was used).
	block, _ := pem.Decode(keyPEM)
	memberKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse member key: %v", err)
	}
	certPub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || certPub.N.Cmp(memberKey.N) != 0 {
		t.Error("issued certificate public key does not match the CSR key")
	}
}

// TestSignCSRIgnoresCSRClaimedIdentity is security-critical: a CSR requesting
// a different identity (CN/SANs) must receive only the identity granted by
// the signer, otherwise a member could enroll as another member.
func TestSignCSRIgnoresCSRClaimedIdentity(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "portal-member/globex-dev"},
		DNSNames: []string{"globex-dev", "*.portal"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	certPEM, err := ca.SignCSR(csrPEM, testIdentity(), time.Hour)
	if err != nil {
		t.Fatalf("SignCSR() error = %v", err)
	}
	cert := mustParseCert(t, certPEM)
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "acme-prod" {
		t.Errorf("DNS SANs = %v; CSR-claimed identity must be ignored, want exactly [acme-prod]", cert.DNSNames)
	}
	if cert.Subject.CommonName != MemberCNPrefix+"acme-prod" {
		t.Errorf("CN = %q; CSR-claimed CN must be ignored", cert.Subject.CommonName)
	}
}

func TestSignCSRRejections(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}
	_, validCSR, err := GenerateMemberKeyAndCSR(testIdentity())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("bad PEM", func(t *testing.T) {
		if _, err := ca.SignCSR([]byte("not a csr"), testIdentity(), time.Hour); err == nil {
			t.Error("expected error for invalid PEM")
		}
	})

	t.Run("wrong PEM type", func(t *testing.T) {
		if _, err := ca.SignCSR(ca.CertPEM(), testIdentity(), time.Hour); err == nil {
			t.Error("expected error for certificate PEM passed as CSR")
		}
	})

	t.Run("weak key", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatal(err)
		}
		csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: pkix.Name{CommonName: "weak"},
		}, key)
		if err != nil {
			t.Fatal(err)
		}
		csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
		_, err = ca.SignCSR(csrPEM, testIdentity(), time.Hour)
		if err == nil || !strings.Contains(err.Error(), "1024 bits") {
			t.Errorf("expected weak-key error, got: %v", err)
		}
	})

	t.Run("invalid member identity", func(t *testing.T) {
		if _, err := ca.SignCSR(validCSR, MemberIdentity{Member: "has:colon"}, time.Hour); err == nil {
			t.Error("expected error for member name containing colon")
		}
	})
}

func TestGenerateMemberKeyAndCSRValidation(t *testing.T) {
	cases := []MemberIdentity{
		{Member: ""},
		{Member: "has:colon"},
		{Member: "UPPER CASE"},
		{Member: "ok", Tenant: "bad:tenant"},
	}
	for _, id := range cases {
		if _, _, err := GenerateMemberKeyAndCSR(id); err == nil {
			t.Errorf("expected validation error for %+v", id)
		}
	}
}

func TestIssueHubServerCert(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}
	certPEM, keyPEM, err := ca.IssueHubServerCert("synapse", []string{"tunnel.corp.example", "reverse-tunnel.portal", "203.0.113.7"}, time.Hour)
	if err != nil {
		t.Fatalf("IssueHubServerCert() error = %v", err)
	}
	if keyPEM == nil {
		t.Fatal("no key returned")
	}
	cert := mustParseCert(t, certPEM)
	if cert.Subject.CommonName != HubCNPrefix+"synapse" {
		t.Errorf("CN = %q, want %q", cert.Subject.CommonName, HubCNPrefix+"synapse")
	}
	wantDNS := map[string]bool{"tunnel.corp.example": true, "reverse-tunnel.portal": true}
	for _, d := range cert.DNSNames {
		delete(wantDNS, d)
	}
	if len(wantDNS) != 0 {
		t.Errorf("missing DNS SANs: %v (got %v)", wantDNS, cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "203.0.113.7" {
		t.Errorf("IP SANs = %v, want [203.0.113.7]", cert.IPAddresses)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Error("hub certificate is not a server certificate")
	}

	if _, _, err := ca.IssueHubServerCert("synapse", nil, time.Hour); err == nil {
		t.Error("expected error for empty SAN list")
	}
}

func TestRenderCRL(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatalf("NewHubCA() error = %v", err)
	}
	_, csrPEM, err := GenerateMemberKeyAndCSR(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, testIdentity(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := ParseCertificateSerial(certPEM)
	if err != nil {
		t.Fatalf("ParseCertificateSerial() error = %v", err)
	}

	crlPEM, err := ca.RenderCRL([]RevokedCert{{Serial: serial}})
	if err != nil {
		t.Fatalf("RenderCRL() error = %v", err)
	}
	block, _ := pem.Decode(crlPEM)
	if block == nil || block.Type != "X509 CRL" {
		t.Fatalf("CRL PEM block missing or wrong type")
	}
	crl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CRL: %v", err)
	}
	caCert := mustParseCert(t, ca.CertPEM())
	if err := crl.CheckSignatureFrom(caCert); err != nil {
		t.Errorf("CRL signature does not verify against hub CA: %v", err)
	}
	if len(crl.RevokedCertificateEntries) != 1 || crl.RevokedCertificateEntries[0].SerialNumber.Cmp(serial) != 0 {
		t.Errorf("CRL does not contain revoked serial %v", serial)
	}

	// CRL numbers must increase across renderings so verifiers accept updates.
	crlPEM2, err := ca.RenderCRL(nil)
	if err != nil {
		t.Fatal(err)
	}
	block2, _ := pem.Decode(crlPEM2)
	crl2, err := x509.ParseRevocationList(block2.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if crl2.Number.Cmp(crl.Number) <= 0 {
		t.Errorf("CRL number did not increase: %v then %v", crl.Number, crl2.Number)
	}
	if len(crl2.RevokedCertificateEntries) != 0 {
		t.Error("empty revocation set should render an empty CRL")
	}
}

func TestRenderCRLNilSerial(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.RenderCRL([]RevokedCert{{}}); err == nil {
		t.Error("expected error for revoked cert without serial")
	}
}

func TestLoadHubCARejectsNonCA(t *testing.T) {
	ca, err := NewHubCA("synapse", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueHubServerCert("synapse", []string{"tunnel.example"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHubCA(certPEM, keyPEM); err == nil {
		t.Error("expected error loading a leaf certificate as a CA")
	}
}
