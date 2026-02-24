package devcontainer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Client wraps the devcontainer CLI.
type Client struct {
	WorkspaceFolder string
	ConfigPath      string // optional override

	// Stdin, Stdout, Stderr override the default os streams for ExecAttached.
	// When nil, the corresponding os.Stdin/os.Stdout/os.Stderr is used.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Up runs `devcontainer up`, removing any existing container so config
// changes (e.g. new bridge server address) take effect.
func (c *Client) Up(ctx context.Context) error {
	args := []string{"up", "--workspace-folder", c.WorkspaceFolder, "--remove-existing-container"}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	cmd := exec.CommandContext(ctx, "devcontainer", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("devcontainer up: %w\n%s", err, out)
	}
	return nil
}

// Exec runs `devcontainer exec` with the given command.
func (c *Client) Exec(ctx context.Context, cmdArgs []string) error {
	args := []string{"exec", "--workspace-folder", c.WorkspaceFolder}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	args = append(args, cmdArgs...)
	cmd := exec.CommandContext(ctx, "devcontainer", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("devcontainer exec: %w\n%s", err, out)
	}
	return nil
}

// ExecAttached runs `devcontainer exec` with stdin/stdout/stderr attached
// for an interactive session.
func (c *Client) ExecAttached(ctx context.Context, cmdArgs []string) error {
	args := []string{"exec", "--workspace-folder", c.WorkspaceFolder}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	args = append(args, cmdArgs...)
	cmd := exec.CommandContext(ctx, "devcontainer", args...)
	cmd.Stdin = c.stdinOrDefault()
	cmd.Stdout = c.stdoutOrDefault()
	cmd.Stderr = c.stderrOrDefault()
	return cmd.Run()
}

// ExecOutput runs `devcontainer exec` and returns combined stdout+stderr.
func (c *Client) ExecOutput(ctx context.Context, cmdArgs []string) (string, error) {
	args := []string{"exec", "--workspace-folder", c.WorkspaceFolder}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	args = append(args, cmdArgs...)
	cmd := exec.CommandContext(ctx, "devcontainer", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Stop stops the devcontainer by running `docker stop` on the container.
func (c *Client) Stop(ctx context.Context) error {
	// devcontainer CLI doesn't have a stop command; use docker stop
	// with the label devcontainer sets.
	label := fmt.Sprintf("devcontainer.local_folder=%s", c.WorkspaceFolder)
	cmd := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label="+label)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return fmt.Errorf("no running devcontainer found for workspace %s", c.WorkspaceFolder)
	}
	containerID := string(out[:len(out)-1]) // trim newline
	stop := exec.CommandContext(ctx, "docker", "stop", containerID)
	if stopOut, err := stop.CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop: %w\n%s", err, stopOut)
	}
	return nil
}

func (c *Client) stdinOrDefault() io.Reader {
	if c.Stdin != nil {
		return c.Stdin
	}
	return os.Stdin
}

func (c *Client) stdoutOrDefault() io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	return os.Stdout
}

func (c *Client) stderrOrDefault() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}
