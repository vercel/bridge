package sandbox

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

func (c *httpAPIClient) Create(ctx context.Context, opts CreateOpts) (*Sandbox, []Route, error) {
	body := createRequest{
		ProjectID:     opts.ProjectID,
		Ports:         opts.Ports,
		Source:        opts.Source,
		Resources:     opts.Resources,
		Runtime:       opts.Runtime,
		NetworkPolicy: opts.NetworkPolicy,
		Env:           opts.Env,
	}
	if opts.Timeout > 0 {
		ms := opts.Timeout.Milliseconds()
		body.Timeout = &ms
	}

	var resp sandboxWithRoutesResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes", nil, body, &resp); err != nil {
		return nil, nil, fmt.Errorf("create sandbox: %w", err)
	}
	return &resp.Sandbox, resp.Routes, nil
}

func (c *httpAPIClient) Get(ctx context.Context, sandboxID string) (*Sandbox, []Route, error) {
	var resp sandboxWithRoutesResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes/"+sandboxID, nil, nil, &resp); err != nil {
		return nil, nil, fmt.Errorf("get sandbox: %w", err)
	}
	return &resp.Sandbox, resp.Routes, nil
}

func (c *httpAPIClient) List(ctx context.Context, opts ListOpts) ([]Sandbox, Pagination, error) {
	params := url.Values{}
	if opts.ProjectID != "" {
		params.Set("project", opts.ProjectID)
	}
	if opts.Limit > 0 {
		params.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Since != nil {
		params.Set("since", strconv.FormatInt(opts.Since.UnixMilli(), 10))
	}
	if opts.Until != nil {
		params.Set("until", strconv.FormatInt(opts.Until.UnixMilli(), 10))
	}

	var resp listSandboxesResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes", params, nil, &resp); err != nil {
		return nil, Pagination{}, fmt.Errorf("list sandboxes: %w", err)
	}
	return resp.Sandboxes, resp.Pagination, nil
}

func (c *httpAPIClient) Stop(ctx context.Context, sandboxID string) (*Sandbox, error) {
	var resp sandboxResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/stop", nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("stop sandbox: %w", err)
	}
	return &resp.Sandbox, nil
}

func (c *httpAPIClient) ExtendTimeout(ctx context.Context, sandboxID string, duration time.Duration) (*Sandbox, error) {
	body := extendTimeoutRequest{Duration: duration.Milliseconds()}
	var resp sandboxResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/extend-timeout", nil, body, &resp); err != nil {
		return nil, fmt.Errorf("extend timeout: %w", err)
	}
	return &resp.Sandbox, nil
}

func (c *httpAPIClient) UpdateNetworkPolicy(ctx context.Context, sandboxID string, policy NetworkPolicy) (*Sandbox, error) {
	var resp sandboxResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/network-policy", nil, policy, &resp); err != nil {
		return nil, fmt.Errorf("update network policy: %w", err)
	}
	return &resp.Sandbox, nil
}

// Request/response types for JSON serialization.

type createRequest struct {
	ProjectID     string            `json:"projectId"`
	Ports         []int             `json:"ports,omitempty"`
	Source        *Source           `json:"source,omitempty"`
	Timeout       *int64            `json:"timeout,omitempty"`
	Resources     *Resources        `json:"resources,omitempty"`
	Runtime       string            `json:"runtime,omitempty"`
	NetworkPolicy *NetworkPolicy    `json:"networkPolicy,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

type extendTimeoutRequest struct {
	Duration int64 `json:"duration"`
}

type sandboxWithRoutesResponse struct {
	Sandbox Sandbox `json:"sandbox"`
	Routes  []Route `json:"routes"`
}

type sandboxResponse struct {
	Sandbox Sandbox `json:"sandbox"`
}

type listSandboxesResponse struct {
	Sandboxes  []Sandbox  `json:"sandboxes"`
	Pagination Pagination `json:"pagination"`
}
