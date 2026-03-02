package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// kubectlClient implements Client by shelling out to kubectl.
type kubectlClient struct {
	kubeContext string
	namespace   string
	runner      CommandRunner
}

// baseArgs returns the common flags for every kubectl invocation.
func (c *kubectlClient) baseArgs() []string {
	return []string{"--context", c.kubeContext, "-n", c.namespace}
}

// Apply creates or updates resources from raw YAML manifests.
func (c *kubectlClient) Apply(ctx context.Context, yamls [][]byte) error {
	if len(yamls) == 0 {
		return nil
	}
	stdin := joinYAMLDocuments(yamls)
	args := append(c.baseArgs(), "apply", "-f", "-")
	_, stderr, err := c.runner.Run(ctx, stdin, "kubectl", args...)
	if err != nil {
		return &KubectlError{Command: "apply", Stderr: strings.TrimSpace(string(stderr)), Err: err}
	}
	return nil
}

// Delete removes resources described by raw YAML manifests.
func (c *kubectlClient) Delete(ctx context.Context, yamls [][]byte) error {
	if len(yamls) == 0 {
		return nil
	}
	stdin := joinYAMLDocuments(yamls)
	args := append(c.baseArgs(), "delete", "-f", "-", "--ignore-not-found")
	_, stderr, err := c.runner.Run(ctx, stdin, "kubectl", args...)
	if err != nil {
		return &KubectlError{Command: "delete", Stderr: strings.TrimSpace(string(stderr)), Err: err}
	}
	return nil
}

// WaitForDeployment blocks until the named deployment is Available or the timeout expires.
func (c *kubectlClient) WaitForDeployment(ctx context.Context, name string, timeout time.Duration) error {
	timeoutStr := fmt.Sprintf("%ds", int(timeout.Seconds()))
	args := append(c.baseArgs(), "wait", fmt.Sprintf("deployment/%s", name),
		"--for=condition=Available", fmt.Sprintf("--timeout=%s", timeoutStr))
	_, stderr, err := c.runner.Run(ctx, nil, "kubectl", args...)
	if err != nil {
		stderrStr := strings.TrimSpace(string(stderr))
		if strings.Contains(stderrStr, "timed out") {
			return &TimeoutError{Operation: fmt.Sprintf("waiting for deployment/%s", name), Duration: timeoutStr}
		}
		return &KubectlError{Command: "wait", Stderr: stderrStr, Err: err}
	}
	return nil
}

// WaitForServiceAddress polls until the named service has an external address.
func (c *kubectlClient) WaitForServiceAddress(ctx context.Context, name string, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		svc, err := c.GetService(ctx, name)
		if err != nil {
			// If not found, keep polling; other errors are fatal.
			if _, ok := err.(*NotFoundError); !ok {
				return "", fmt.Errorf("failed to get service %s: %w", name, err)
			}
		} else {
			for _, ing := range svc.LoadBalancerIngress {
				if addr := ing.Address(); addr != "" {
					return addr, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("context cancelled while waiting for service/%s address: %w", name, ctx.Err())
		case <-deadline:
			return "", &TimeoutError{
				Operation: fmt.Sprintf("waiting for service/%s address", name),
				Duration:  timeout.String(),
			}
		case <-ticker.C:
			// continue polling
		}
	}
}

// PortForward starts a kubectl port-forward subprocess and returns a session handle.
func (c *kubectlClient) PortForward(ctx context.Context, target string, localPort, remotePort int) (*PortForwardSession, error) {
	args := append(c.baseArgs(), "port-forward", target, fmt.Sprintf("%d:%d", localPort, remotePort))
	proc, err := c.runner.Start(ctx, "kubectl", args...)
	if err != nil {
		return nil, &KubectlError{Command: "port-forward", Err: err}
	}

	if err := waitForPort(localPort, 5*time.Second); err != nil {
		_ = proc.Stop()
		return nil, fmt.Errorf("port-forward readiness check failed: %w", err)
	}

	return &PortForwardSession{
		LocalPort:  localPort,
		RemotePort: remotePort,
		Target:     target,
		Namespace:  c.namespace,
		process:    proc,
	}, nil
}

// GetPods returns pods matching the given label selector.
func (c *kubectlClient) GetPods(ctx context.Context, labelSelector string) ([]PodInfo, error) {
	args := append(c.baseArgs(), "get", "pods", "-l", labelSelector, "-o", "json")
	stdout, stderr, err := c.runner.Run(ctx, nil, "kubectl", args...)
	if err != nil {
		return nil, &KubectlError{Command: "get pods", Stderr: strings.TrimSpace(string(stderr)), Err: err}
	}
	return parsePodListJSON(stdout)
}

// RolloutRestart triggers a rolling restart of the named deployment.
func (c *kubectlClient) RolloutRestart(ctx context.Context, deployment string) error {
	args := append(c.baseArgs(), "rollout", "restart", fmt.Sprintf("deployment/%s", deployment))
	_, stderr, err := c.runner.Run(ctx, nil, "kubectl", args...)
	if err != nil {
		return &KubectlError{Command: "rollout restart", Stderr: strings.TrimSpace(string(stderr)), Err: err}
	}
	return nil
}

// GetService returns information about the named service.
func (c *kubectlClient) GetService(ctx context.Context, name string) (*ServiceInfo, error) {
	args := append(c.baseArgs(), "get", "service", name, "-o", "json")
	stdout, stderr, err := c.runner.Run(ctx, nil, "kubectl", args...)
	if err != nil {
		stderrStr := strings.TrimSpace(string(stderr))
		if strings.Contains(stderrStr, "NotFound") || strings.Contains(stderrStr, "not found") {
			return nil, &NotFoundError{Resource: fmt.Sprintf("service/%s", name)}
		}
		return nil, &KubectlError{Command: "get service", Stderr: stderrStr, Err: err}
	}
	return parseServiceJSON(stdout)
}

// joinYAMLDocuments concatenates multiple YAML documents with --- separators.
func joinYAMLDocuments(docs [][]byte) []byte {
	var buf []byte
	for i, doc := range docs {
		if i > 0 {
			buf = append(buf, []byte("---\n")...)
		}
		buf = append(buf, doc...)
		if len(doc) > 0 && doc[len(doc)-1] != '\n' {
			buf = append(buf, '\n')
		}
	}
	return buf
}

// waitForPort TCP-dials localhost:port until it responds or the timeout expires.
func waitForPort(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %d not ready within %s", port, timeout)
}

// JSON structures for kubectl output parsing (no k8s.io/api dependency).

type podList struct {
	Items []podItem `json:"items"`
}

type podItem struct {
	Metadata struct {
		Name              string    `json:"name"`
		Namespace         string    `json:"namespace"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		Phase             string                `json:"phase"`
		PodIP             string                `json:"podIP"`
		ContainerStatuses []containerStatusJSON `json:"containerStatuses"`
	} `json:"status"`
}

type containerStatusJSON struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	Image        string `json:"image"`
}

func parsePodListJSON(data []byte) ([]PodInfo, error) {
	var pl podList
	if err := json.Unmarshal(data, &pl); err != nil {
		return nil, fmt.Errorf("parsing pod list JSON: %w", err)
	}
	pods := make([]PodInfo, 0, len(pl.Items))
	for _, item := range pl.Items {
		var totalRestarts int32
		allReady := len(item.Status.ContainerStatuses) > 0
		containers := make([]ContainerInfo, 0, len(item.Status.ContainerStatuses))
		for _, cs := range item.Status.ContainerStatuses {
			totalRestarts += cs.RestartCount
			if !cs.Ready {
				allReady = false
			}
			containers = append(containers, ContainerInfo{
				Name:     cs.Name,
				Ready:    cs.Ready,
				Restarts: cs.RestartCount,
				Image:    cs.Image,
			})
		}
		pods = append(pods, PodInfo{
			Name:       item.Metadata.Name,
			Namespace:  item.Metadata.Namespace,
			Phase:      PodPhase(item.Status.Phase),
			Ready:      allReady,
			Restarts:   totalRestarts,
			Age:        time.Since(item.Metadata.CreationTimestamp),
			IP:         item.Status.PodIP,
			NodeName:   item.Spec.NodeName,
			Containers: containers,
		})
	}
	return pods, nil
}

type serviceJSON struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Type        string            `json:"type"`
		ClusterIP   string            `json:"clusterIP"`
		ExternalIPs []string          `json:"externalIPs"`
		Ports       []servicePortJSON `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []lbIngressJSON `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type servicePortJSON struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Port       int32  `json:"port"`
	TargetPort any    `json:"targetPort"` // can be int or string
}

type lbIngressJSON struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
}

func parseServiceJSON(data []byte) (*ServiceInfo, error) {
	var svc serviceJSON
	if err := json.Unmarshal(data, &svc); err != nil {
		return nil, fmt.Errorf("parsing service JSON: %w", err)
	}

	ports := make([]ServicePort, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		tp := ""
		switch v := p.TargetPort.(type) {
		case float64:
			tp = fmt.Sprintf("%d", int(v))
		case string:
			tp = v
		}
		ports = append(ports, ServicePort{
			Name:       p.Name,
			Protocol:   p.Protocol,
			Port:       p.Port,
			TargetPort: tp,
		})
	}

	ingress := make([]LoadBalancerIngress, 0, len(svc.Status.LoadBalancer.Ingress))
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		ingress = append(ingress, LoadBalancerIngress(ing))
	}

	return &ServiceInfo{
		Name:                svc.Metadata.Name,
		Namespace:           svc.Metadata.Namespace,
		Type:                svc.Spec.Type,
		ClusterIP:           svc.Spec.ClusterIP,
		ExternalIPs:         svc.Spec.ExternalIPs,
		Ports:               ports,
		LoadBalancerIngress: ingress,
	}, nil
}
