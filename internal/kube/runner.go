package kube

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// CommandRunner abstracts subprocess execution for testability.
type CommandRunner interface {
	// Run executes a command synchronously and returns stdout, stderr, and any error.
	Run(ctx context.Context, stdin []byte, name string, args ...string) (stdout, stderr []byte, err error)
	// Start launches a long-lived subprocess and returns a Process handle.
	Start(ctx context.Context, name string, args ...string) (*Process, error)
}

// Process wraps a running subprocess started via CommandRunner.Start.
type Process struct {
	cmd *exec.Cmd
}

// Stop terminates the subprocess.
func (p *Process) Stop() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("failed to kill process: %w", err)
	}
	return nil
}

// execRunner is the default CommandRunner using os/exec.
type execRunner struct{}

func (r *execRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	return stdoutBuf.Bytes(), stderrBuf.Bytes(), err
}

func (r *execRunner) Start(ctx context.Context, name string, args ...string) (*Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", name, err)
	}
	return &Process{cmd: cmd}, nil
}
