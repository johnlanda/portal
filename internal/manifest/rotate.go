package manifest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/johnlanda/portal/internal/certs"
)

// RotateConfig contains parameters for certificate rotation.
type RotateConfig struct {
	TunnelDir    string
	CertValidity time.Duration
}

// RotateCertificates regenerates leaf certificates for an existing tunnel
// using the persisted CA material. Only the TLS secret files are updated;
// all other manifests remain untouched.
func RotateCertificates(cfg RotateConfig) (*TunnelMetadata, error) {
	// Read and parse tunnel metadata.
	metaPath := filepath.Join(cfg.TunnelDir, "tunnel.yaml")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read tunnel.yaml: %w", err)
	}
	var meta TunnelMetadata
	if err := yaml.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse tunnel.yaml: %w", err)
	}

	// Guard: detect cert-manager tunnels.
	cmFile := filepath.Join(cfg.TunnelDir, "source", "cert-manager-initiator-certificate.yaml")
	if _, err := os.Stat(cmFile); err == nil {
		return nil, fmt.Errorf("this tunnel uses cert-manager for certificate management; rotation is handled automatically by cert-manager")
	}

	// Guard: detect secret-ref tunnels.
	if meta.SecretRef != "" {
		return nil, fmt.Errorf("this tunnel uses an externally managed secret (%s); certificate rotation must be performed by the external secret provider", meta.SecretRef)
	}

	// Read CA material.
	caCert, caKey, err := readCAMaterial(filepath.Join(cfg.TunnelDir, "ca"))
	if err != nil {
		return nil, fmt.Errorf("failed to read CA material: %w", err)
	}

	// Determine certificate validity: flag > tunnel.yaml > default.
	validity := cfg.CertValidity
	if validity == 0 && meta.CertValidity != "" {
		validity, err = time.ParseDuration(meta.CertValidity)
		if err != nil {
			return nil, fmt.Errorf("failed to parse certValidity %q from tunnel.yaml: %w", meta.CertValidity, err)
		}
	}
	if validity == 0 {
		validity = certs.DefaultCertificateValidity
	}

	// Determine responder SANs: metadata > derive from endpoint.
	responderSANs := meta.ResponderSANs
	if len(responderSANs) == 0 {
		host, _, err := net.SplitHostPort(meta.ResponderEndpoint)
		if err != nil {
			// Try as bare IP/host.
			host = meta.ResponderEndpoint
		}
		if host != "" {
			responderSANs = []string{host}
		}
	}

	// Rotate leaf certificates.
	tunnelCerts, err := certs.RotateLeafCertificates(caCert, caKey, meta.TunnelName, responderSANs, validity)
	if err != nil {
		return nil, fmt.Errorf("failed to rotate certificates: %w", err)
	}

	// Rebuild and overwrite the TLS secret files.
	sourceSecret, err := buildSecret("portal-tunnel-tls", meta.Namespace, tunnelCerts.InitiatorCert, tunnelCerts.InitiatorKey, tunnelCerts.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to build source secret: %w", err)
	}
	destSecret, err := buildSecret("portal-tunnel-tls", meta.Namespace, tunnelCerts.ResponderCert, tunnelCerts.ResponderKey, tunnelCerts.CACert)
	if err != nil {
		return nil, fmt.Errorf("failed to build destination secret: %w", err)
	}

	sourcePath := filepath.Join(cfg.TunnelDir, "source", sourceSecret.Filename)
	if err := os.WriteFile(sourcePath, sourceSecret.Content, 0644); err != nil {
		return nil, fmt.Errorf("failed to write source secret: %w", err)
	}

	destPath := filepath.Join(cfg.TunnelDir, "destination", destSecret.Filename)
	if err := os.WriteFile(destPath, destSecret.Content, 0644); err != nil {
		return nil, fmt.Errorf("failed to write destination secret: %w", err)
	}

	// Update tunnel metadata.
	now := time.Now().UTC()
	meta.LastRotatedAt = &now
	meta.RotationCount++
	meta.CertValidity = validity.String()
	if len(meta.ResponderSANs) == 0 {
		meta.ResponderSANs = responderSANs
	}

	metaBytes, err = yaml.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tunnel metadata: %w", err)
	}
	if err := os.WriteFile(metaPath, metaBytes, 0644); err != nil {
		return nil, fmt.Errorf("failed to write tunnel.yaml: %w", err)
	}

	return &meta, nil
}

// readCAMaterial reads the CA certificate and key from the ca/ directory.
func readCAMaterial(caDir string) ([]byte, []byte, error) {
	caCert, err := os.ReadFile(filepath.Join(caDir, "ca.crt"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("CA material not found in %s; this tunnel was generated with an older version of portal that did not persist the CA key — please regenerate the tunnel with the current version", caDir)
		}
		return nil, nil, fmt.Errorf("failed to read ca.crt: %w", err)
	}

	caKey, err := os.ReadFile(filepath.Join(caDir, "ca.key"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("CA key not found in %s; this tunnel was generated with an older version of portal that did not persist the CA key — please regenerate the tunnel with the current version", caDir)
		}
		return nil, nil, fmt.Errorf("failed to read ca.key: %w", err)
	}

	return caCert, caKey, nil
}
