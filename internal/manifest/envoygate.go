package manifest

import (
	"fmt"
	"regexp"
	"strings"
)

// The reverse tunnel APIs (rc:// addresses, handshake headers, filter protos,
// stat names) are experimental upstream and carry no stability promise, so
// each Portal release renders v2 configs only for Envoy minors it was
// verified against. Unsupported minors are refused, not warned about —
// a rendered manifest in a GitOps repo that silently stops matching its
// proxy is worse than a hard error at render time.

// SupportedEnvoyMinors lists the Envoy minor versions this Portal release
// renders reverse tunnel configuration for.
var SupportedEnvoyMinors = []string{"1.37"}

var envoyImageVersionRe = regexp.MustCompile(`:v(\d+\.\d+)`)

// CheckEnvoyImage verifies that the image references a supported Envoy minor.
// allowUnsupported bypasses the gate (for testing newer Envoys) but never the
// parse requirement being reported.
func CheckEnvoyImage(image string, allowUnsupported bool) error {
	if allowUnsupported {
		return nil
	}
	m := envoyImageVersionRe.FindStringSubmatch(image)
	if m == nil {
		return fmt.Errorf("cannot determine Envoy version from image %q; reverse tunnel APIs are experimental and version-gated (supported: %s) — use a tag like v%s, or --allow-unsupported-envoy to bypass",
			image, strings.Join(SupportedEnvoyMinors, ", "), SupportedEnvoyMinors[0])
	}
	for _, minor := range SupportedEnvoyMinors {
		if m[1] == minor {
			return nil
		}
	}
	return fmt.Errorf("Envoy %s is not supported by this Portal release (supported: %s); the reverse tunnel APIs are experimental upstream and may have changed — upgrade Portal, or pass --allow-unsupported-envoy to bypass",
		m[1], strings.Join(SupportedEnvoyMinors, ", "))
}
