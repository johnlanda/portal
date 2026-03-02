package cli

import (
	"testing"
)

func TestParseServiceFlagsEmpty(t *testing.T) {
	configs, err := parseServiceFlags(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if configs != nil {
		t.Errorf("expected nil configs, got %v", configs)
	}
}

func TestParseServiceFlagsSingle(t *testing.T) {
	configs, err := parseServiceFlags(
		[]string{"backend=backend-svc.synapse-system.svc:8443"},
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	c := configs[0]
	if c.SNI != "backend" {
		t.Errorf("SNI = %q, want %q", c.SNI, "backend")
	}
	if c.BackendHost != "backend-svc.synapse-system.svc" {
		t.Errorf("BackendHost = %q, want %q", c.BackendHost, "backend-svc.synapse-system.svc")
	}
	if c.BackendPort != 8443 {
		t.Errorf("BackendPort = %d, want %d", c.BackendPort, 8443)
	}
	if c.LocalPort != 0 {
		t.Errorf("LocalPort = %d, want 0", c.LocalPort)
	}
}

func TestParseServiceFlagsMultipleWithLocalPorts(t *testing.T) {
	configs, err := parseServiceFlags(
		[]string{
			"backend=backend-svc.synapse-system.svc:8443",
			"otel=otel-collector.synapse-system.svc:4317",
		},
		[]string{
			"backend=18443",
			"otel=14317",
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}

	// First service.
	if configs[0].SNI != "backend" {
		t.Errorf("configs[0].SNI = %q, want %q", configs[0].SNI, "backend")
	}
	if configs[0].LocalPort != 18443 {
		t.Errorf("configs[0].LocalPort = %d, want %d", configs[0].LocalPort, 18443)
	}

	// Second service.
	if configs[1].SNI != "otel" {
		t.Errorf("configs[1].SNI = %q, want %q", configs[1].SNI, "otel")
	}
	if configs[1].BackendPort != 4317 {
		t.Errorf("configs[1].BackendPort = %d, want %d", configs[1].BackendPort, 4317)
	}
	if configs[1].LocalPort != 14317 {
		t.Errorf("configs[1].LocalPort = %d, want %d", configs[1].LocalPort, 14317)
	}
}

func TestParseServiceFlagsDuplicateSNI(t *testing.T) {
	_, err := parseServiceFlags(
		[]string{
			"backend=host1:8443",
			"backend=host2:9443",
		},
		nil,
	)
	if err == nil {
		t.Fatal("expected error for duplicate SNI")
	}
}

func TestParseServiceFlagsInvalidFormat(t *testing.T) {
	tests := []struct {
		name    string
		service string
	}{
		{"missing equals", "backend"},
		{"missing port", "backend=host"},
		{"invalid port", "backend=host:abc"},
		{"port zero", "backend=host:0"},
		{"port too large", "backend=host:99999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseServiceFlags([]string{tt.service}, nil)
			if err == nil {
				t.Errorf("expected error for %q", tt.service)
			}
		})
	}
}

func TestParseServiceFlagsInvalidLocalPort(t *testing.T) {
	tests := []struct {
		name      string
		localPort string
	}{
		{"missing equals", "backend"},
		{"invalid port", "backend=abc"},
		{"port zero", "backend=0"},
		{"port too large", "backend=99999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseServiceFlags(
				[]string{"backend=host:8443"},
				[]string{tt.localPort},
			)
			if err == nil {
				t.Errorf("expected error for local port %q", tt.localPort)
			}
		})
	}
}
