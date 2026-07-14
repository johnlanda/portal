package cli

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"

	"github.com/johnlanda/portal/internal/envoy"
)

// envoyDefaultHandshakeSNI aliases the envoy package default for readability
// at the CLI layer.
const envoyDefaultHandshakeSNI = envoy.DefaultHandshakeSNI

// credential is the single-file member credential minted by 'portal hub
// invite'. It contains the member's private key; the two-phase CSR flow is
// preferred for two-party enrollment.
type credential struct {
	Member       string `json:"member"`
	Hub          string `json:"hub"`
	HubAddr      string `json:"hub_addr"`
	HandshakeSNI string `json:"handshake_sni"`
	Cert         string `json:"cert"`
	Key          string `json:"key"`
	CA           string `json:"ca"`
}

func writeCredential(path string, c credential) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode credential: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write credential: %w", err)
	}
	return nil
}

func readCredential(path string) (*credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read credential: %w", err)
	}
	var c credential
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse credential: %w", err)
	}
	if c.Member == "" || c.HubAddr == "" || c.Cert == "" || c.Key == "" || c.CA == "" {
		return nil, fmt.Errorf("credential is missing required fields")
	}
	return &c, nil
}

// splitHostPort splits host:port, applying defaultPort when no port is given.
func splitHostPort(addr string, defaultPort int) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// No port present.
		return addr, defaultPort, nil //nolint:nilerr // fall back to default port
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

// parseSerial parses a decimal certificate serial.
func parseSerial(s string) (*big.Int, bool) {
	return new(big.Int).SetString(s, 10)
}
