package kube

import "fmt"

// KubectlError wraps a failed kubectl command with context.
type KubectlError struct {
	Command string
	Stderr  string
	Err     error
}

// Error returns a human-readable description of the kubectl failure.
func (e *KubectlError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("kubectl %s: %s", e.Command, e.Stderr)
	}
	return fmt.Sprintf("kubectl %s: %v", e.Command, e.Err)
}

// Unwrap returns the underlying error for use with errors.Is/As.
func (e *KubectlError) Unwrap() error {
	return e.Err
}

// TimeoutError indicates an operation exceeded its deadline.
type TimeoutError struct {
	Operation string
	Duration  string
}

// Error returns a human-readable description of the timeout.
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.Operation, e.Duration)
}

// NotFoundError indicates a resource was not found.
type NotFoundError struct {
	Resource string
}

// Error returns a human-readable description of the missing resource.
func (e *NotFoundError) Error() string {
	return fmt.Sprintf("resource not found: %s", e.Resource)
}
