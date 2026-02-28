package kube

import "time"

// PodPhase represents the phase of a Kubernetes pod.
type PodPhase string

const (
	PodPending   PodPhase = "Pending"
	PodRunning   PodPhase = "Running"
	PodSucceeded PodPhase = "Succeeded"
	PodFailed    PodPhase = "Failed"
	PodUnknown   PodPhase = "Unknown"
)

// PodInfo contains summary information about a Kubernetes pod.
type PodInfo struct {
	Name       string
	Namespace  string
	Phase      PodPhase
	Ready      bool
	Restarts   int32
	Age        time.Duration
	IP         string
	NodeName   string
	Containers []ContainerInfo
}

// ContainerInfo contains summary information about a container within a pod.
type ContainerInfo struct {
	Name     string
	Ready    bool
	Restarts int32
	Image    string
}

// ServiceInfo contains summary information about a Kubernetes service.
type ServiceInfo struct {
	Name                string
	Namespace           string
	Type                string
	ClusterIP           string
	ExternalIPs         []string
	Ports               []ServicePort
	LoadBalancerIngress []LoadBalancerIngress
}

// ServicePort describes a port exposed by a service.
type ServicePort struct {
	Name       string
	Protocol   string
	Port       int32
	TargetPort string
}

// LoadBalancerIngress describes an ingress point for a load balancer.
type LoadBalancerIngress struct {
	IP       string
	Hostname string
}

// Address returns the IP if non-empty, otherwise the Hostname.
func (l LoadBalancerIngress) Address() string {
	if l.IP != "" {
		return l.IP
	}
	return l.Hostname
}

// PortForwardSession represents an active kubectl port-forward subprocess.
type PortForwardSession struct {
	LocalPort  int
	RemotePort int
	Target     string
	Namespace  string
	process    *Process
}

// Close stops the port-forward subprocess.
func (s *PortForwardSession) Close() error {
	if s.process == nil {
		return nil
	}
	return s.process.Stop()
}
