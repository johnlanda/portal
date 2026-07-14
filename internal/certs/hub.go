package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/johnlanda/portal/internal/validate"
)

// Hub PKI for the v2 hub/member model (docs/v2-proposal.md). The hierarchy is:
//
//	Portal Hub CA (self-signed, per-hub)
//	  ├── Hub Server Cert    (CN: portal-hub/<hub-name>)
//	  └── Member Client Cert (CN: portal-member/<member-name>, DNS SAN: <member-name>)
//
// A member's DNS SAN is its identity: the hub's reverse_tunnel filter compares
// the handshake's claimed cluster-id against the peer certificate DNS SAN, so
// a member can only claim the identity its certificate proves. Member keys are
// generated where the member runs (two-phase CSR enrollment); the CA only ever
// sees public material.

const (
	// HubCACommonNamePrefix is the CN prefix for hub CA certificates.
	HubCACommonNamePrefix = "portal-hub-ca/"

	// HubCNPrefix is the CN prefix for hub server certificates.
	HubCNPrefix = "portal-hub/"

	// MemberCNPrefix is the CN prefix for member client certificates.
	MemberCNPrefix = "portal-member/"

	// minCSRKeyBits is the minimum RSA key size accepted in member CSRs.
	minCSRKeyBits = 2048

	// crlNextUpdateWindow is how long a rendered CRL is valid before
	// verifiers should expect a fresh one.
	crlNextUpdateWindow = 7 * 24 * time.Hour
)

// MemberIdentity identifies a member for certificate issuance. Member becomes
// the certificate's DNS SAN (and the reverse tunnel cluster-id); Tenant is
// recorded in the certificate subject for auditability. Neither may contain
// ':', which Envoy reserves as its tenant-scoping delimiter.
type MemberIdentity struct {
	// Member is the member name: DNS SAN and reverse tunnel cluster-id.
	Member string
	// Tenant is the tenant identifier (typically the hub name).
	Tenant string
}

func (id MemberIdentity) validate() error {
	if err := validate.DNSName(id.Member); err != nil {
		return fmt.Errorf("invalid member name: %w", err)
	}
	if id.Tenant != "" {
		if err := validate.Name(id.Tenant); err != nil {
			return fmt.Errorf("invalid tenant name: %w", err)
		}
	}
	return nil
}

// RevokedCert identifies a certificate to include in a CRL.
type RevokedCert struct {
	// Serial is the certificate serial number.
	Serial *big.Int
	// RevokedAt is the revocation time (defaults to now if zero).
	RevokedAt time.Time
}

// HubCA is a hub certificate authority that signs member client certificates
// and hub server certificates, and renders CRLs for member eviction. It is
// stateless: callers persist the PEM material (CertPEM/KeyPEM) and reload it
// with LoadHubCA.
type HubCA struct {
	certPEM []byte
	keyPEM  []byte
	cert    *x509.Certificate
	key     *rsa.PrivateKey
}

// NewHubCA generates a new self-signed hub CA.
func NewHubCA(hubName string, validity time.Duration) (*HubCA, error) {
	if err := validate.Name(hubName); err != nil {
		return nil, fmt.Errorf("invalid hub name: %w", err)
	}
	if validity == 0 {
		validity = DefaultCertificateValidity
	}
	certPEM, keyPEM, err := newCA(HubCACommonNamePrefix+hubName, time.Now().Add(validity))
	if err != nil {
		return nil, fmt.Errorf("failed to generate hub CA: %w", err)
	}
	return LoadHubCA(certPEM, keyPEM)
}

// LoadHubCA parses persisted hub CA material.
func LoadHubCA(certPEM, keyPEM []byte) (*HubCA, error) {
	if err := validateCAMaterial(certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("hub CA validation failed: %w", err)
	}
	keyPair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hub CA key pair: %w", err)
	}
	cert, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse hub CA certificate: %w", err)
	}
	key, ok := keyPair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("hub CA private key has unexpected type %T", keyPair.PrivateKey)
	}
	return &HubCA{certPEM: certPEM, keyPEM: keyPEM, cert: cert, key: key}, nil
}

// CertPEM returns the PEM-encoded hub CA certificate.
func (ca *HubCA) CertPEM() []byte { return ca.certPEM }

// KeyPEM returns the PEM-encoded hub CA private key.
func (ca *HubCA) KeyPEM() []byte { return ca.keyPEM }

// SignCSR issues a member client certificate from a CSR. Only the public key
// is taken from the CSR; the certificate's identity (CN, DNS SAN, tenant) is
// derived entirely from id, so a CSR cannot request an identity it was not
// granted. The CSR signature is verified to prove possession of the key.
func (ca *HubCA) SignCSR(csrPEM []byte, id MemberIdentity, validity time.Duration) ([]byte, error) {
	if err := id.validate(); err != nil {
		return nil, err
	}
	if validity == 0 {
		validity = DefaultCertificateValidity
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("failed to decode CSR: expected a CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature verification failed: %w", err)
	}
	pubKey, ok := csr.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("CSR public key has unsupported type %T (only RSA is supported)", csr.PublicKey)
	}
	if pubKey.N.BitLen() < minCSRKeyBits {
		return nil, fmt.Errorf("CSR public key is %d bits; minimum is %d", pubKey.N.BitLen(), minCSRKeyBits)
	}

	serial, err := newSerial()
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}
	now := time.Now()
	subject := pkix.Name{CommonName: MemberCNPrefix + id.Member}
	if id.Tenant != "" {
		subject.OrganizationalUnit = []string{id.Tenant}
	}
	template := &x509.Certificate{
		SerialNumber:   serial,
		Subject:        subject,
		NotBefore:      now.UTC().AddDate(0, 0, -1),
		NotAfter:       now.Add(validity).UTC(),
		SubjectKeyId:   bigIntHash(pubKey.N),
		AuthorityKeyId: bigIntHash(ca.key.N),
		KeyUsage:       x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:       []string{id.Member},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, pubKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("failed to create member certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), nil
}

// IssueHubServerCert issues the hub's server certificate for the shared
// tunnel listener. sans should include the hub's public hostname/IP and the
// reverse tunnel handshake SNI.
func (ca *HubCA) IssueHubServerCert(hubName string, sans []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	if err := validate.Name(hubName); err != nil {
		return nil, nil, fmt.Errorf("invalid hub name: %w", err)
	}
	if len(sans) == 0 {
		return nil, nil, fmt.Errorf("hub server certificate requires at least one SAN")
	}
	if validity == 0 {
		validity = DefaultCertificateValidity
	}
	dnsNames, ipAddresses := splitNamesAndIPs(sans)
	return newCert(&certificateRequest{
		caCertPEM:   ca.certPEM,
		caKeyPEM:    ca.keyPEM,
		expiry:      time.Now().Add(validity),
		commonName:  HubCNPrefix + hubName,
		dnsNames:    dnsNames,
		ipAddresses: ipAddresses,
	})
}

// RenderCRL renders a PEM-encoded CRL listing the given revoked certificates.
// The CRL number is derived from the current time in nanoseconds, so
// successive renderings are monotonically increasing without the CA having to
// persist a counter. NextUpdate is set 7 days out; re-render and re-publish
// whenever the revocation set changes and before NextUpdate passes.
func (ca *HubCA) RenderCRL(revoked []RevokedCert) ([]byte, error) {
	now := time.Now()
	entries := make([]x509.RevocationListEntry, 0, len(revoked))
	for i, r := range revoked {
		if r.Serial == nil {
			return nil, fmt.Errorf("revoked certificate at index %d has no serial number", i)
		}
		revokedAt := r.RevokedAt
		if revokedAt.IsZero() {
			revokedAt = now
		}
		entries = append(entries, x509.RevocationListEntry{
			SerialNumber:   r.Serial,
			RevocationTime: revokedAt.UTC(),
		})
	}
	template := &x509.RevocationList{
		Number:                    big.NewInt(now.UnixNano()),
		ThisUpdate:                now.UTC(),
		NextUpdate:                now.Add(crlNextUpdateWindow).UTC(),
		RevokedCertificateEntries: entries,
	}
	crlDER, err := x509.CreateRevocationList(rand.Reader, template, ca.cert, ca.key)
	if err != nil {
		return nil, fmt.Errorf("failed to create CRL: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crlDER}), nil
}

// GenerateMemberKeyAndCSR generates a member keypair and CSR for two-phase
// enrollment. Call it where the member runs so the private key never leaves
// the member's environment; send only the CSR to the hub owner for SignCSR.
func GenerateMemberKeyAndCSR(id MemberIdentity) (keyPEM, csrPEM []byte, err error) {
	if err := id.validate(); err != nil {
		return nil, nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate member key: %w", err)
	}
	subject := pkix.Name{CommonName: MemberCNPrefix + id.Member}
	if id.Tenant != "" {
		subject.OrganizationalUnit = []string{id.Tenant}
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  subject,
		DNSNames: []string{id.Member},
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CSR: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	csrPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	return keyPEM, csrPEM, nil
}

// ParseCertificateSerial extracts the serial number from a PEM-encoded
// certificate, for building RevokedCert entries in eviction flows.
func ParseCertificateSerial(certPEM []byte) (*big.Int, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("failed to decode certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}
	return cert.SerialNumber, nil
}
