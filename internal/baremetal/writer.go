package baremetal

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteToDisk writes a BareMetalBundle to the specified output directory.
// It creates the directory structure:
//
//	<outputDir>/
//	├── initiator/
//	│   ├── envoy.yaml
//	│   ├── certs/
//	│   │   ├── tls.crt
//	│   │   ├── tls.key
//	│   │   └── ca.crt
//	│   ├── portal-initiator.service
//	│   └── docker-compose.yaml
//	├── responder/
//	│   ├── envoy.yaml
//	│   ├── certs/
//	│   │   ├── tls.crt
//	│   │   ├── tls.key
//	│   │   └── ca.crt
//	│   ├── portal-responder.service
//	│   └── docker-compose.yaml
//	├── ca/
//	│   ├── ca.crt
//	│   ├── ca.key
//	│   └── .gitignore
//	└── tunnel.yaml
func WriteToDisk(bundle *BareMetalBundle, outputDir string) error {
	// Write initiator side.
	if err := writeSide(filepath.Join(outputDir, "initiator"), "portal-initiator", bundle.Initiator); err != nil {
		return fmt.Errorf("failed to write initiator artifacts: %w", err)
	}

	// Write responder side.
	if err := writeSide(filepath.Join(outputDir, "responder"), "portal-responder", bundle.Responder); err != nil {
		return fmt.Errorf("failed to write responder artifacts: %w", err)
	}

	// Write CA material when certs were generated (not external).
	if bundle.Certs != nil {
		if err := writeCAMaterial(outputDir, bundle.Certs.CACert, bundle.Certs.CAKey); err != nil {
			return fmt.Errorf("failed to write CA material: %w", err)
		}
	}

	// Write tunnel metadata.
	metadataBytes, err := yaml.Marshal(bundle.Metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal tunnel metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, "tunnel.yaml"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write tunnel.yaml: %w", err)
	}

	return nil
}

func writeSide(dir, unitName string, side BareMetalSide) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Write Envoy config.
	if err := os.WriteFile(filepath.Join(dir, "envoy.yaml"), side.EnvoyConfig, 0644); err != nil {
		return fmt.Errorf("failed to write envoy.yaml: %w", err)
	}

	// Write certificate files.
	certDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return fmt.Errorf("failed to create certs directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.crt"), side.CertFiles.Cert, 0644); err != nil {
		return fmt.Errorf("failed to write tls.crt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "tls.key"), side.CertFiles.Key, 0600); err != nil {
		return fmt.Errorf("failed to write tls.key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.crt"), side.CertFiles.CA, 0644); err != nil {
		return fmt.Errorf("failed to write ca.crt: %w", err)
	}

	// Write systemd unit.
	if err := os.WriteFile(filepath.Join(dir, unitName+".service"), side.SystemdUnit, 0644); err != nil {
		return fmt.Errorf("failed to write systemd unit: %w", err)
	}

	// Write docker-compose file.
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), side.DockerCompose, 0644); err != nil {
		return fmt.Errorf("failed to write docker-compose.yaml: %w", err)
	}

	return nil
}

func writeCAMaterial(outputDir string, caCert, caKey []byte) error {
	caDir := filepath.Join(outputDir, "ca")
	if err := os.MkdirAll(caDir, 0700); err != nil {
		return fmt.Errorf("failed to create CA directory: %w", err)
	}

	if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), caCert, 0644); err != nil {
		return fmt.Errorf("failed to write ca.crt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "ca.key"), caKey, 0600); err != nil {
		return fmt.Errorf("failed to write ca.key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(caDir, ".gitignore"), []byte("*\n"), 0644); err != nil {
		return fmt.Errorf("failed to write .gitignore: %w", err)
	}

	return nil
}
