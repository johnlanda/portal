package baremetal

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/johnlanda/portal/internal/manifest"
)

// composeConfig holds parameters for rendering a docker-compose file.
type composeConfig struct {
	ServiceName   string
	EnvoyImage    string
	EnvoyLogLevel string
	ConfigPath    string
	CertPath      string
	TunnelPort    int
	Services      []manifest.ServiceConfig
	IsInitiator   bool
}

const composeTemplate = `services:
  {{.ServiceName}}:
    image: {{.EnvoyImage}}
    command:
      - -c
      - /etc/envoy/envoy.yaml
      - --log-level
      - {{.EnvoyLogLevel}}
    restart: unless-stopped
    ports:
{{- if .IsInitiator}}
{{- if .Services}}
{{- range .Services}}
{{- $lp := .LocalPort}}
{{- if eq $lp 0}}{{$lp = .BackendPort}}{{end}}
      - "{{$lp}}:{{$lp}}"
{{- end}}
{{- else}}
      - "{{.TunnelPort}}:{{.TunnelPort}}"
{{- end}}
{{- else}}
      - "{{.TunnelPort}}:{{.TunnelPort}}"
{{- end}}
    volumes:
      - ./envoy.yaml:/etc/envoy/envoy.yaml:ro
      - ./certs:{{.CertPath}}:ro
`

// renderDockerCompose renders a docker-compose.yaml file from the given config.
func renderDockerCompose(cfg composeConfig) ([]byte, error) {
	tmpl, err := template.New("compose").Parse(composeTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse docker-compose template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return nil, fmt.Errorf("failed to render docker-compose template: %w", err)
	}
	return buf.Bytes(), nil
}
