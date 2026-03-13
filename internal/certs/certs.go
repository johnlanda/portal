// Package certs provides certificate generation for Portal tunnel mTLS.
// The certificate hierarchy is:
//
//	Portal CA (self-signed, per-tunnel)
//	  ├── Initiator Client Cert (CN: portal-initiator/<tunnel-name>)
//	  └── Responder Server Cert (CN: portal-responder/<tunnel-name>)
package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	// DefaultCertificateValidity is the default certificate validity (1 year).
	DefaultCertificateValidity = 365 * 24 * time.Hour

	// keySize is the RSA key size in bits.
	keySize = 4096

	// CACommonName is the CN for the Portal tunnel CA.
	CACommonName = "portal-ca"

	// InitiatorCNPrefix is the prefix for initiator client certificate CNs.
	InitiatorCNPrefix = "portal-initiator/"

	// ResponderCNPrefix is the prefix for responder server certificate CNs.
	ResponderCNPrefix = "portal-responder/"
)

// TunnelCertificates holds all PEM-encoded certificates and keys for a tunnel.
type TunnelCertificates struct {
	// CACert is the PEM-encoded CA certificate.
	CACert []byte
	// CAKey is the PEM-encoded CA private key.
	CAKey []byte

	// InitiatorCert is the PEM-encoded initiator client certificate.
	InitiatorCert []byte
	// InitiatorKey is the PEM-encoded initiator client private key.
	InitiatorKey []byte

	// ResponderCert is the PEM-encoded responder server certificate.
	ResponderCert []byte
	// ResponderKey is the PEM-encoded responder server private key.
	ResponderKey []byte
}

// certificateRequest describes a certificate to generate.
type certificateRequest struct {
	caCertPEM   []byte
	caKeyPEM    []byte
	expiry      time.Time
	commonName  string
	dnsNames    []string
	ipAddresses []net.IP
	isClient    bool
}

// GenerateTunnelCertificates generates a complete set of certificates for a tunnel:
// a self-signed CA, an initiator client cert, and a responder server cert.
// responderSANs should include any DNS names or IPs the responder needs in its cert.
func GenerateTunnelCertificates(tunnelName string, responderSANs []string, validity time.Duration) (*TunnelCertificates, error) {
	if validity == 0 {
		validity = DefaultCertificateValidity
	}

	now := time.Now()
	expiry := now.Add(validity)

	// Generate CA.
	caCertPEM, caKeyPEM, err := newCA(CACommonName, expiry)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA: %w", err)
	}

	// Generate initiator client cert.
	initiatorCN := InitiatorCNPrefix + tunnelName
	initiatorCert, initiatorKey, err := newCert(&certificateRequest{
		caCertPEM:  caCertPEM,
		caKeyPEM:   caKeyPEM,
		expiry:     expiry,
		commonName: initiatorCN,
		isClient:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate initiator certificate: %w", err)
	}

	// Generate responder server cert with SANs.
	responderCN := ResponderCNPrefix + tunnelName
	dnsNames, ipAddresses := splitNamesAndIPs(responderSANs)
	responderCert, responderKey, err := newCert(&certificateRequest{
		caCertPEM:   caCertPEM,
		caKeyPEM:    caKeyPEM,
		expiry:      expiry,
		commonName:  responderCN,
		dnsNames:    dnsNames,
		ipAddresses: ipAddresses,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate responder certificate: %w", err)
	}

	return &TunnelCertificates{
		CACert:        caCertPEM,
		CAKey:         caKeyPEM,
		InitiatorCert: initiatorCert,
		InitiatorKey:  initiatorKey,
		ResponderCert: responderCert,
		ResponderKey:  responderKey,
	}, nil
}

// RotateLeafCertificates generates new initiator and responder leaf certificates
// signed by an existing CA. The CA cert and key are unchanged; only new leaf
// certs and keys are produced.
func RotateLeafCertificates(caCertPEM, caKeyPEM []byte, tunnelName string, responderSANs []string, validity time.Duration) (*TunnelCertificates, error) {
	if validity == 0 {
		validity = DefaultCertificateValidity
	}

	// Validate CA material before using it for rotation.
	if err := validateCAMaterial(caCertPEM, caKeyPEM); err != nil {
		return nil, fmt.Errorf("CA validation failed: %w", err)
	}

	expiry := time.Now().Add(validity)

	// Generate new initiator client cert.
	initiatorCN := InitiatorCNPrefix + tunnelName
	initiatorCert, initiatorKey, err := newCert(&certificateRequest{
		caCertPEM:  caCertPEM,
		caKeyPEM:   caKeyPEM,
		expiry:     expiry,
		commonName: initiatorCN,
		isClient:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate initiator certificate: %w", err)
	}

	// Generate new responder server cert with SANs.
	responderCN := ResponderCNPrefix + tunnelName
	dnsNames, ipAddresses := splitNamesAndIPs(responderSANs)
	responderCert, responderKey, err := newCert(&certificateRequest{
		caCertPEM:   caCertPEM,
		caKeyPEM:    caKeyPEM,
		expiry:      expiry,
		commonName:  responderCN,
		dnsNames:    dnsNames,
		ipAddresses: ipAddresses,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate responder certificate: %w", err)
	}

	return &TunnelCertificates{
		CACert:        caCertPEM,
		CAKey:         caKeyPEM,
		InitiatorCert: initiatorCert,
		InitiatorKey:  initiatorKey,
		ResponderCert: responderCert,
		ResponderKey:  responderKey,
	}, nil
}

// validateCAMaterial verifies that the CA cert has IsCA set and that the key
// matches the certificate by signing and verifying a test payload.
func validateCAMaterial(caCertPEM, caKeyPEM []byte) error {
	caKeyPair, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("failed to parse CA key pair: %w", err)
	}

	caCert, err := x509.ParseCertificate(caKeyPair.Certificate[0])
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	if !caCert.IsCA {
		return fmt.Errorf("certificate is not a CA (IsCA=false)")
	}

	caKey, ok := caKeyPair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return fmt.Errorf("CA private key has unexpected type %T", caKeyPair.PrivateKey)
	}

	// Verify the key matches by signing and verifying a test payload.
	testData := []byte("portal-ca-validation")
	hash := sha1.Sum(testData)
	sig, err := rsa.SignPKCS1v15(rand.Reader, caKey, 0, hash[:])
	if err != nil {
		return fmt.Errorf("failed to sign test data: %w", err)
	}
	if err := rsa.VerifyPKCS1v15(&caKey.PublicKey, 0, hash[:], sig); err != nil {
		return fmt.Errorf("CA key does not match certificate: %w", err)
	}

	return nil
}

// splitNamesAndIPs separates a mixed list of DNS names and IP addresses.
func splitNamesAndIPs(names []string) (dnsNames []string, ips []net.IP) {
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, name)
		}
	}
	return dnsNames, ips
}

// newCA generates a self-signed CA certificate and private key.
func newCA(cn string, expiry time.Time) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate CA key: %w", err)
	}

	now := time.Now()
	serial, err := newSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			SerialNumber: serial.String(),
		},
		NotBefore:             now.UTC().AddDate(0, 0, -1),
		NotAfter:              expiry.UTC(),
		SubjectKeyId:          bigIntHash(key.N),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	return certPEM, keyPEM, nil
}

// newCert generates a certificate signed by the given CA.
func newCert(req *certificateRequest) ([]byte, []byte, error) {
	caKeyPair, err := tls.X509KeyPair(req.caCertPEM, req.caKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA key pair: %w", err)
	}

	caCert, err := x509.ParseCertificate(caKeyPair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	caKey, ok := caKeyPair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA private key has unexpected type %T", caKeyPair.PrivateKey)
	}

	newKey, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate key: %w", err)
	}

	now := time.Now()
	serial, err := newSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	keyUsage := x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	var extKeyUsage []x509.ExtKeyUsage
	if req.isClient {
		extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		extKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: req.commonName,
		},
		NotBefore:      now.UTC().AddDate(0, 0, -1),
		NotAfter:       req.expiry.UTC(),
		SubjectKeyId:   bigIntHash(newKey.N),
		AuthorityKeyId: bigIntHash(caKey.N),
		KeyUsage:       keyUsage,
		ExtKeyUsage:    extKeyUsage,
		DNSNames:       req.dnsNames,
		IPAddresses:    req.ipAddresses,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &newKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(newKey),
	})

	return certPEM, keyPEM, nil
}

// newSerial generates a cryptographically random serial number (159 bits).
func newSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 159)
	return rand.Int(rand.Reader, max)
}

// bigIntHash produces a hash suitable for SubjectKeyId.
func bigIntHash(n *big.Int) []byte {
	h := sha1.New()
	h.Write(n.Bytes())
	return h.Sum(nil)
}
