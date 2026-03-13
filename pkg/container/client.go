package container

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// FindOpts are options for FindID.
type FindOpts struct {
	Labels map[string]string
}

// StopAllOpts are options for StopAll.
type StopAllOpts struct {
	Labels map[string]string
}

// ListOpts are options for List.
type ListOpts struct {
	Labels map[string]string
}

// Client provides container runtime operations.
type Client interface {
	// FindID returns the ID of the first running container matching the given
	// options. Returns an error if no match is found.
	FindID(ctx context.Context, opts FindOpts) (string, error)

	// StopAll stops and removes all containers (running or stopped) matching
	// the given options.
	StopAll(ctx context.Context, opts StopAllOpts)

	// Stop stops a container by ID.
	Stop(ctx context.Context, containerID string) error

	// Exec runs a command inside the container and returns combined output.
	Exec(ctx context.Context, containerID string, args ...string) (string, error)

	// ReadFile reads a file from inside the container. Returns empty string on error.
	ReadFile(ctx context.Context, containerID, path string) string

	// InspectLabel returns the value of a single label on a container.
	InspectLabel(ctx context.Context, containerID, label string) (string, error)

	// List returns formatted output and IDs for all running containers
	// matching the given options.
	List(ctx context.Context, opts ListOpts) (string, []string, error)
}

// dockerClient implements Client using the Docker CLI.
type dockerClient struct {
	runtime string
}

// NewDockerClient returns a Client backed by Docker.
func NewDockerClient() Client {
	return &dockerClient{runtime: "docker"}
}

func labelFilters(labels map[string]string) []string {
	var filters []string
	for k, v := range labels {
		if v == "" {
			filters = append(filters, "--filter", "label="+k)
		} else {
			filters = append(filters, "--filter", "label="+k+"="+v)
		}
	}
	return filters
}

func (c *dockerClient) FindID(ctx context.Context, opts FindOpts) (string, error) {
	args := append([]string{"ps", "-q"}, labelFilters(opts.Labels)...)
	cmd := exec.CommandContext(ctx, c.runtime, args...)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return "", fmt.Errorf("no running container found matching labels %v", opts.Labels)
	}
	id := strings.TrimSpace(string(out))
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	return id, nil
}

func (c *dockerClient) StopAll(ctx context.Context, opts StopAllOpts) {
	args := append([]string{"ps", "-aq"}, labelFilters(opts.Labels)...)
	cmd := exec.CommandContext(ctx, c.runtime, args...)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, id := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		slog.Debug("Stopping container", "id", id)
		_ = exec.CommandContext(ctx, c.runtime, "rm", "-f", id).Run()
	}
}

func (c *dockerClient) Stop(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, c.runtime, "stop", containerID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s stop: %w\n%s", c.runtime, err, out)
	}
	return nil
}

func (c *dockerClient) Exec(ctx context.Context, containerID string, args ...string) (string, error) {
	cmdArgs := append([]string{"exec", containerID}, args...)
	cmd := exec.CommandContext(ctx, c.runtime, cmdArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *dockerClient) ReadFile(ctx context.Context, containerID, path string) string {
	cmd := exec.CommandContext(ctx, c.runtime, "exec", containerID, "cat", path)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (c *dockerClient) InspectLabel(ctx context.Context, containerID, label string) (string, error) {
	tmpl := fmt.Sprintf(`{{index .Config.Labels %q}}`, label)
	cmd := exec.CommandContext(ctx, c.runtime, "inspect", "--format", tmpl, containerID)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s inspect: %w", c.runtime, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *dockerClient) List(ctx context.Context, opts ListOpts) (string, []string, error) {
	args := append([]string{"ps"}, labelFilters(opts.Labels)...)

	// Build --format with label values.
	fmtParts := []string{"{{.ID}}", "{{.Names}}", "{{.Status}}"}
	for k := range opts.Labels {
		fmtParts = append(fmtParts, fmt.Sprintf("{{.Label %q}}", k))
	}
	args = append(args, "--format", strings.Join(fmtParts, "\t"))

	cmd := exec.CommandContext(ctx, c.runtime, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("%s ps: %w", c.runtime, err)
	}
	output := strings.TrimSpace(string(out))
	if output == "" {
		return "", nil, nil
	}
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		if id, _, ok := strings.Cut(line, "\t"); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return output, ids, nil
}
