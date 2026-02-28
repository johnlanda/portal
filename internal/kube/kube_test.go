package kube

import (
	"testing"
)

func TestCheckKubectl(t *testing.T) {
	err := CheckKubectl()
	if err != nil {
		t.Skip("kubectl not found on PATH, skipping")
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("my-context", "my-namespace")
	kc, ok := c.(*kubectlClient)
	if !ok {
		t.Fatal("expected *kubectlClient")
	}
	if kc.kubeContext != "my-context" {
		t.Errorf("kubeContext = %q, want %q", kc.kubeContext, "my-context")
	}
	if kc.namespace != "my-namespace" {
		t.Errorf("namespace = %q, want %q", kc.namespace, "my-namespace")
	}
	if kc.runner == nil {
		t.Error("runner should not be nil")
	}
}

func TestNewClientWithRunner(t *testing.T) {
	m := &mockRunner{}
	c := NewClient("ctx", "ns", WithRunner(m))
	kc := c.(*kubectlClient)
	if kc.runner != m {
		t.Error("expected custom runner to be set")
	}
}

func TestJoinYAMLDocuments(t *testing.T) {
	tests := []struct {
		name string
		docs [][]byte
		want string
	}{
		{
			name: "empty",
			docs: nil,
			want: "",
		},
		{
			name: "single document",
			docs: [][]byte{[]byte("apiVersion: v1\nkind: Pod\n")},
			want: "apiVersion: v1\nkind: Pod\n",
		},
		{
			name: "multiple documents",
			docs: [][]byte{
				[]byte("apiVersion: v1\nkind: Pod\n"),
				[]byte("apiVersion: v1\nkind: Service\n"),
			},
			want: "apiVersion: v1\nkind: Pod\n---\napiVersion: v1\nkind: Service\n",
		},
		{
			name: "missing trailing newline",
			docs: [][]byte{
				[]byte("apiVersion: v1"),
				[]byte("apiVersion: apps/v1"),
			},
			want: "apiVersion: v1\n---\napiVersion: apps/v1\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(joinYAMLDocuments(tt.docs))
			if got != tt.want {
				t.Errorf("joinYAMLDocuments() = %q, want %q", got, tt.want)
			}
		})
	}
}
