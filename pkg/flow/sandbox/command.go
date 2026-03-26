package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/url"
)

func (c *httpAPIClient) RunCommand(ctx context.Context, sandboxID string, opts RunCommandOpts) (*Command, error) {
	body := runCommandRequest{
		Command: opts.Command,
		Args:    opts.Args,
		CWD:     opts.CWD,
		Env:     opts.Env,
		Sudo:    opts.Sudo,
	}
	if body.Env == nil {
		body.Env = map[string]string{}
	}
	if body.Args == nil {
		body.Args = []string{}
	}

	var resp commandResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/cmd", nil, body, &resp); err != nil {
		return nil, fmt.Errorf("run command: %w", err)
	}
	return &resp.Command, nil
}

func (c *httpAPIClient) RunCommandAndWait(ctx context.Context, sandboxID string, opts RunCommandOpts) (*Command, error) {
	body := runCommandRequest{
		Command: opts.Command,
		Args:    opts.Args,
		CWD:     opts.CWD,
		Env:     opts.Env,
		Sudo:    opts.Sudo,
		Wait:    true,
	}
	if body.Env == nil {
		body.Env = map[string]string{}
	}
	if body.Args == nil {
		body.Args = []string{}
	}

	// The wait=true response is NDJSON: first line is started, last line is finished.
	var resp commandResponse
	if err := c.doNDJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/cmd", nil, body, &resp); err != nil {
		return nil, fmt.Errorf("run command (wait): %w", err)
	}
	return &resp.Command, nil
}

func (c *httpAPIClient) ListCommands(ctx context.Context, sandboxID string) ([]Command, error) {
	var resp listCommandsResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes/"+sandboxID+"/cmd", nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("list commands: %w", err)
	}
	return resp.Commands, nil
}

func (c *httpAPIClient) GetCommand(ctx context.Context, sandboxID string, cmdID string) (*Command, error) {
	var resp commandResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes/"+sandboxID+"/cmd/"+cmdID, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("get command: %w", err)
	}
	return &resp.Command, nil
}

func (c *httpAPIClient) WaitCommand(ctx context.Context, sandboxID string, cmdID string) (*Command, error) {
	params := url.Values{"wait": {"true"}}
	var resp commandResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes/"+sandboxID+"/cmd/"+cmdID, params, nil, &resp); err != nil {
		return nil, fmt.Errorf("wait command: %w", err)
	}
	return &resp.Command, nil
}

func (c *httpAPIClient) KillCommand(ctx context.Context, sandboxID string, cmdID string, signal int) (*Command, error) {
	body := killCommandRequest{Signal: signal}
	var resp commandResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/"+cmdID+"/kill", nil, body, &resp); err != nil {
		return nil, fmt.Errorf("kill command: %w", err)
	}
	return &resp.Command, nil
}

func (c *httpAPIClient) StreamLogs(ctx context.Context, sandboxID string, cmdID string) (io.ReadCloser, error) {
	rc, err := c.doStream(ctx, "GET", "/v1/sandboxes/"+sandboxID+"/cmd/"+cmdID+"/logs", nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("stream logs: %w", err)
	}
	return rc, nil
}

// Request/response types for JSON serialization.

type runCommandRequest struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	CWD     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env"`
	Sudo    bool              `json:"sudo"`
	Wait    bool              `json:"wait,omitempty"`
}

type killCommandRequest struct {
	Signal int `json:"signal"`
}

type commandResponse struct {
	Command Command `json:"command"`
}

type listCommandsResponse struct {
	Commands []Command `json:"commands"`
}
