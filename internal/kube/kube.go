// Package kube provides a Kubernetes client abstraction built on kubectl.
//
// Rather than importing the k8s.io client libraries, kube shells out to kubectl
// so that it respects the user's kubeconfig, auth plugins, and context settings
// with zero additional dependencies. Every Client is scoped to a single
// kubeconfig context and namespace.
//
// The [CommandRunner] interface enables full unit testing without a live cluster.
package kube

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Client provides operations against a single Kubernetes cluster and namespace.
type Client interface {
	// Apply creates or updates resources from raw YAML manifests.
	Apply(ctx context.Context, yamls [][]byte) error
	// Delete removes resources described by raw YAML manifests.
	Delete(ctx context.Context, yamls [][]byte) error
	// WaitForDeployment blocks until the named deployment is Available or the timeout expires.
	WaitForDeployment(ctx context.Context, name string, timeout time.Duration) error
	// WaitForServiceAddress polls until the named service has an external address (LoadBalancer IP/hostname).
	WaitForServiceAddress(ctx context.Context, name string, timeout time.Duration) (string, error)
	// PortForward starts a kubectl port-forward subprocess and returns a session handle.
	PortForward(ctx context.Context, target string, localPort, remotePort int) (*PortForwardSession, error)
	// GetPods returns pods matching the given label selector.
	GetPods(ctx context.Context, labelSelector string) ([]PodInfo, error)
	// GetService returns information about the named service.
	GetService(ctx context.Context, name string) (*ServiceInfo, error)
	// RolloutRestart triggers a rolling restart of the named deployment.
	RolloutRestart(ctx context.Context, deployment string) error
}

// ClientOption configures a Client.
type ClientOption func(*kubectlClient)

// WithRunner sets the CommandRunner used to execute kubectl. Useful for testing.
func WithRunner(r CommandRunner) ClientOption {
	return func(c *kubectlClient) {
		c.runner = r
	}
}

// NewClient creates a Client scoped to the given kubeconfig context and namespace.
func NewClient(kubeContext, namespace string, opts ...ClientOption) Client {
	c := &kubectlClient{
		kubeContext: kubeContext,
		namespace:   namespace,
		runner:      &execRunner{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CheckKubectl verifies that kubectl is available on PATH.
func CheckKubectl() error {
	_, err := exec.LookPath("kubectl")
	if err != nil {
		return fmt.Errorf("kubectl not found in PATH: %w", err)
	}
	return nil
}

// CheckContext verifies that the given kubeconfig context exists.
func CheckContext(kubeContext string) error {
	cmd := exec.Command("kubectl", "config", "get-contexts", kubeContext, "--no-headers")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kube context %q not found in kubeconfig", kubeContext)
	}
	return nil
}
