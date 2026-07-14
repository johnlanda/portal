// Command testrender renders sample reverse tunnel bootstraps for validation
// against a real Envoy binary. Not shipped; used by developers and CI only.
package main

import (
	"fmt"
	"os"

	"github.com/johnlanda/portal/internal/envoy"
)

func main() {
	member, err := envoy.RenderMemberBootstrap(envoy.MemberConfig{
		MemberName: "acme-prod",
		HubName:    "synapse",
		HubHost:    "tunnel.corp.example",
		Published: []envoy.PublishedService{
			{Name: "inference", BackendHost: "inference.default.svc", BackendPort: 8080, Protocol: "grpc"},
			{Name: "admin", BackendHost: "admin.default.svc", BackendPort: 9000},
		},
		Forward: []envoy.ServiceListener{{Name: "telemetry", ListenPort: 4317}},
	})
	if err != nil {
		panic(err)
	}
	hub, err := envoy.RenderHubBootstrap(envoy.HubConfig{
		Members:  []string{"acme-prod", "globex-dev"},
		Services: []envoy.ServiceRoute{{SNI: "api.hub", BackendHost: "backend.portal.svc", BackendPort: 8443}},
	})
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[1]+"/member.yaml", member, 0o644); err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[1]+"/hub.yaml", hub, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("rendered")
}
