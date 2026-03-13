// Package validate provides input validation functions for Portal.
package validate

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

const maxNameLen = 253

var (
	nameRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	validLogLevels = map[string]bool{
		"trace": true, "debug": true, "info": true,
		"warning": true, "error": true, "critical": true, "off": true,
	}
)

// Name validates that s is a safe identifier: alphanumeric, dash, underscore, dot;
// 1-253 characters, starting with an alphanumeric character.
func Name(s string) error {
	if s == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(s) > maxNameLen {
		return fmt.Errorf("name %q exceeds maximum length of %d characters", s, maxNameLen)
	}
	if !nameRe.MatchString(s) {
		return fmt.Errorf("name %q contains invalid characters; must match [a-zA-Z0-9][a-zA-Z0-9._-]*", s)
	}
	return nil
}

// DNSName validates that s is a valid DNS name (RFC 1123) or IP address.
func DNSName(s string) error {
	if s == "" {
		return fmt.Errorf("DNS name must not be empty")
	}
	if len(s) > maxNameLen {
		return fmt.Errorf("DNS name %q exceeds maximum length of %d characters", s, maxNameLen)
	}
	// Accept valid IP addresses.
	if net.ParseIP(s) != nil {
		return nil
	}
	// Validate as DNS name: each label must match RFC 1123.
	labels := strings.Split(strings.ToLower(s), ".")
	for _, label := range labels {
		if label == "" {
			return fmt.Errorf("DNS name %q contains empty label", s)
		}
		if len(label) > 63 {
			return fmt.Errorf("DNS name %q has label exceeding 63 characters", s)
		}
		if !dnsLabelRe.MatchString(label) {
			return fmt.Errorf("DNS name %q contains invalid label %q; must match [a-z0-9]([a-z0-9-]*[a-z0-9])?", s, label)
		}
	}
	return nil
}

// FilePath validates that s is a safe file path: no ".." components, no newlines.
func FilePath(s string) error {
	if s == "" {
		return fmt.Errorf("file path must not be empty")
	}
	if strings.ContainsAny(s, "\n\r\x00") {
		return fmt.Errorf("file path %q contains invalid characters", s)
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return fmt.Errorf("file path %q contains '..' traversal", s)
		}
	}
	return nil
}

// LogLevel validates that s is a known Envoy log level.
func LogLevel(s string) error {
	if !validLogLevels[s] {
		return fmt.Errorf("invalid log level %q; must be one of: trace, debug, info, warning, error, critical, off", s)
	}
	return nil
}

// DockerImage validates that s looks like a valid Docker image reference.
func DockerImage(s string) error {
	if s == "" {
		return fmt.Errorf("Docker image must not be empty")
	}
	if strings.ContainsAny(s, "\n\r\x00 ") {
		return fmt.Errorf("Docker image %q contains invalid characters", s)
	}
	return nil
}
