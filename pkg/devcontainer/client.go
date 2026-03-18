package devcontainer

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Client is the interface for interacting with the devcontainer CLI.
type Client interface {
	// Up starts `devcontainer up` in the background and returns a BuildHandle.
	Up(ctx context.Context, opts UpOpts) (BuildHandle, error)
	// Exec runs `devcontainer exec` with stdin/stdout/stderr attached.
	Exec(ctx context.Context, cmdArgs []string) error
	// ExecOutput runs `devcontainer exec` and returns combined stdout+stderr.
	ExecOutput(ctx context.Context, cmdArgs []string) (string, error)
}

// CLIClient wraps the devcontainer CLI.
type CLIClient struct {
	WorkspaceFolder string
	ConfigPath      string // optional override

	// UpArgs are additional arguments passed to `devcontainer up`.
	UpArgs []string

	// Stdin, Stdout, Stderr override the default os streams for Exec.
	// When nil, the corresponding os.Stdin/os.Stdout/os.Stderr is used.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// LogFormat controls the devcontainer CLI log output format.
type LogFormat string

const (
	LogFormatText LogFormat = "text"
	LogFormatJSON LogFormat = "json"
)

// UpOpts configures a devcontainer up invocation.
type UpOpts struct {
	// LogFormat sets the --log-format flag. Defaults to LogFormatText.
	LogFormat LogFormat
}

// BuildHandle represents a running devcontainer build process.
type BuildHandle interface {
	// Output returns a reader for the combined stdout/stderr of the process.
	Output() io.Reader
	// Wait waits for the process to exit and returns any error.
	// The caller should drain Output() before calling Wait.
	Wait() error
}

// Up starts `devcontainer up` in the background and returns a BuildHandle.
// The caller should read from Output() until EOF, then call Wait() to
// collect the exit status.
func (c *CLIClient) Up(ctx context.Context, opts UpOpts) (BuildHandle, error) {
	args := []string{"up", "--workspace-folder", c.WorkspaceFolder, "--remove-existing-container"}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	logFmt := opts.LogFormat
	if logFmt == "" {
		logFmt = LogFormatText
	}
	args = append(args, "--log-format", string(logFmt))
	args = append(args, c.UpArgs...)
	cmd := exec.CommandContext(ctx, "devcontainer", args...)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("devcontainer up: %w", err)
	}

	h := &buildHandle{output: pr, done: make(chan struct{})}

	go func() {
		h.waitErr = cmd.Wait()
		pw.Close()
		close(h.done)
	}()

	return h, nil
}

type buildHandle struct {
	output  *io.PipeReader
	done    chan struct{}
	waitErr error
}

func (h *buildHandle) Output() io.Reader {
	return h.output
}

func (h *buildHandle) Wait() error {
	<-h.done
	return h.waitErr
}

// Exec runs `devcontainer exec` with stdin/stdout/stderr attached.
func (c *CLIClient) Exec(ctx context.Context, cmdArgs []string) error {
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
func (c *CLIClient) ExecOutput(ctx context.Context, cmdArgs []string) (string, error) {
	args := []string{"exec", "--workspace-folder", c.WorkspaceFolder}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	args = append(args, cmdArgs...)
	cmd := exec.CommandContext(ctx, "devcontainer", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *CLIClient) stdinOrDefault() io.Reader {
	if c.Stdin != nil {
		return c.Stdin
	}
	return os.Stdin
}

func (c *CLIClient) stdoutOrDefault() io.Writer {
	if c.Stdout != nil {
		return c.Stdout
	}
	return os.Stdout
}

func (c *CLIClient) stderrOrDefault() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}
