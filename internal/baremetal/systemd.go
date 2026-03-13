package baremetal

import (
	"bytes"
	"fmt"
	"text/template"
)

// systemdConfig holds parameters for rendering a systemd unit file.
type systemdConfig struct {
	Description   string
	UnitName      string
	EnvoyCommand  string
	ConfigPath    string
	EnvoyLogLevel string
	RunUser       string
}

const systemdTemplate = `[Unit]
Description={{.Description}}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{.RunUser}}
ExecStart={{.EnvoyCommand}} -c {{.ConfigPath}} --log-level {{.EnvoyLogLevel}}
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`

// renderSystemdUnit renders a systemd unit file from the given config.
func renderSystemdUnit(cfg systemdConfig) ([]byte, error) {
	tmpl, err := template.New("systemd").Parse(systemdTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse systemd template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return nil, fmt.Errorf("failed to render systemd template: %w", err)
	}
	return buf.Bytes(), nil
}
