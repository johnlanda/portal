package kube

import "fmt"

// KubectlError wraps a failed kubectl command with context.
type KubectlError struct {
	Command string
	Stderr  string
	Err     error
}

func (e *KubectlError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("kubectl %s: %s", e.Command, e.Stderr)
	}
	return fmt.Sprintf("kubectl %s: %v", e.Command, e.Err)
}

func (e *KubectlError) Unwrap() error {
	return e.Err
}

// TimeoutError indicates an operation exceeded its deadline.
type TimeoutError struct {
	Operation string
	Duration  string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.Operation, e.Duration)
}

// NotFoundError indicates a resource was not found.
type NotFoundError struct {
	Resource string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("resource not found: %s", e.Resource)
}
