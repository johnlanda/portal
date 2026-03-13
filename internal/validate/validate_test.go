package validate

import (
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	valid := []string{
		"my-tunnel",
		"context1",
		"a",
		"abc.def",
		"a_b-c.d",
		"A1-B2_C3",
	}
	for _, v := range valid {
		if err := Name(v); err != nil {
			t.Errorf("Name(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"",
		"-starts-with-dash",
		".starts-with-dot",
		"_starts-with-underscore",
		"has space",
		"has\nnewline",
		"has;semicolon",
		"has'quote",
		strings.Repeat("a", 254),
	}
	for _, v := range invalid {
		if err := Name(v); err == nil {
			t.Errorf("Name(%q) = nil, want error", v)
		}
	}
}

func TestDNSName(t *testing.T) {
	valid := []string{
		"example.com",
		"a",
		"my-service",
		"backend.default.svc",
		"192.168.1.1",
		"::1",
		"10.0.0.1",
		"a1",
		"1a",
	}
	for _, v := range valid {
		if err := DNSName(v); err != nil {
			t.Errorf("DNSName(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"",
		"-starts-with-dash",
		"ends-with-dash-",
		"has space",
		"has_underscore",
		"has..double-dot",
		strings.Repeat("a", 254),
	}
	for _, v := range invalid {
		if err := DNSName(v); err == nil {
			t.Errorf("DNSName(%q) = nil, want error", v)
		}
	}
}

func TestFilePath(t *testing.T) {
	valid := []string{
		"/etc/portal",
		"/usr/bin/envoy",
		"envoy",
		"./relative/path",
	}
	for _, v := range valid {
		if err := FilePath(v); err != nil {
			t.Errorf("FilePath(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"",
		"/etc/../passwd",
		"path\nwith\nnewlines",
		"path\x00with\x00null",
	}
	for _, v := range invalid {
		if err := FilePath(v); err == nil {
			t.Errorf("FilePath(%q) = nil, want error", v)
		}
	}
}

func TestLogLevel(t *testing.T) {
	valid := []string{"trace", "debug", "info", "warning", "error", "critical", "off"}
	for _, v := range valid {
		if err := LogLevel(v); err != nil {
			t.Errorf("LogLevel(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{"", "INFO", "verbose", "warn", "fatal"}
	for _, v := range invalid {
		if err := LogLevel(v); err == nil {
			t.Errorf("LogLevel(%q) = nil, want error", v)
		}
	}
}

func TestDockerImage(t *testing.T) {
	valid := []string{
		"envoyproxy/envoy:v1.37-latest",
		"nginx",
		"registry.example.com/image:tag",
		"ghcr.io/org/image@sha256:abc123",
	}
	for _, v := range valid {
		if err := DockerImage(v); err != nil {
			t.Errorf("DockerImage(%q) = %v, want nil", v, err)
		}
	}

	invalid := []string{
		"",
		"image with spaces",
		"image\nwith\nnewlines",
	}
	for _, v := range invalid {
		if err := DockerImage(v); err == nil {
			t.Errorf("DockerImage(%q) = nil, want error", v)
		}
	}
}
