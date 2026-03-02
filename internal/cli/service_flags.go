package cli

import (
	"fmt"
	"strings"

	"github.com/johnlanda/portal/internal/manifest"
)

// parseServiceFlags parses --service and --service-local-port flag values into ServiceConfigs.
//
// Service format: "sni=host:port" (e.g., "backend=backend-svc.synapse-system.svc:8443")
// Local port format: "sni=port" (e.g., "backend=18443")
func parseServiceFlags(serviceFlags, localPortFlags []string) ([]manifest.ServiceConfig, error) {
	if len(serviceFlags) == 0 {
		return nil, nil
	}

	// Parse local port overrides into a map.
	localPorts := make(map[string]int)
	for _, lp := range localPortFlags {
		parts := strings.SplitN(lp, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --service-local-port format %q: expected sni=port", lp)
		}
		var port int
		if _, err := fmt.Sscanf(parts[1], "%d", &port); err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid --service-local-port value %q: port must be 1-65535", lp)
		}
		localPorts[parts[0]] = port
	}

	var configs []manifest.ServiceConfig
	seen := make(map[string]bool)

	for _, svc := range serviceFlags {
		parts := strings.SplitN(svc, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --service format %q: expected sni=host:port", svc)
		}
		sni := parts[0]
		if seen[sni] {
			return nil, fmt.Errorf("duplicate service SNI %q", sni)
		}
		seen[sni] = true

		hostPort := parts[1]
		lastColon := strings.LastIndex(hostPort, ":")
		if lastColon < 0 {
			return nil, fmt.Errorf("invalid --service format %q: expected host:port after '='", svc)
		}
		host := hostPort[:lastColon]
		var port int
		if _, err := fmt.Sscanf(hostPort[lastColon+1:], "%d", &port); err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid --service format %q: port must be 1-65535", svc)
		}

		cfg := manifest.ServiceConfig{
			SNI:         sni,
			BackendHost: host,
			BackendPort: port,
		}
		if lp, ok := localPorts[sni]; ok {
			cfg.LocalPort = lp
		}
		configs = append(configs, cfg)
	}

	return configs, nil
}
