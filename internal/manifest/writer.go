package manifest

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteToDisk writes a ManifestBundle to the specified output directory.
// It creates the directory structure:
//
//	<outputDir>/
//	├── source/
//	│   ├── *.yaml
//	│   └── kustomization.yaml
//	├── destination/
//	│   ├── *.yaml
//	│   └── kustomization.yaml
//	└── tunnel.yaml
func WriteToDisk(bundle *ManifestBundle, outputDir string) error {
	sourceDir := filepath.Join(outputDir, "source")
	destDir := filepath.Join(outputDir, "destination")

	// Write source resources.
	if err := writeResources(sourceDir, bundle.Source); err != nil {
		return fmt.Errorf("failed to write source manifests: %w", err)
	}

	// Write destination resources.
	if err := writeResources(destDir, bundle.Destination); err != nil {
		return fmt.Errorf("failed to write destination manifests: %w", err)
	}

	// Persist CA material when certs are present (not cert-manager mode).
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

// writeCAMaterial persists the CA certificate and key to a ca/ directory
// within the tunnel output directory. This enables certificate rotation
// without regenerating the entire PKI.
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

func writeResources(dir string, resources []Resource) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	var filenames []string
	for _, r := range resources {
		path := filepath.Join(dir, r.Filename)
		if err := os.WriteFile(path, r.Content, 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", r.Filename, err)
		}
		filenames = append(filenames, r.Filename)
	}

	// Write kustomization.yaml.
	kustomization := map[string]interface{}{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  filenames,
	}
	kustomBytes, err := yaml.Marshal(kustomization)
	if err != nil {
		return fmt.Errorf("failed to marshal kustomization.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), kustomBytes, 0644); err != nil {
		return fmt.Errorf("failed to write kustomization.yaml: %w", err)
	}

	return nil
}
