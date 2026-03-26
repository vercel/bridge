package sandbox

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

func (c *httpAPIClient) CreateSnapshot(ctx context.Context, sandboxID string, opts CreateSnapshotOpts) (*Snapshot, *Sandbox, error) {
	var body any
	if opts.Expiration > 0 {
		body = createSnapshotRequest{Expiration: opts.Expiration.Milliseconds()}
	} else if opts.Expiration == 0 {
		// Omit body entirely to use server default.
	}

	var resp snapshotWithSandboxResponse
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/snapshot", nil, body, &resp); err != nil {
		return nil, nil, fmt.Errorf("create snapshot: %w", err)
	}
	return &resp.Snapshot, &resp.Sandbox, nil
}

func (c *httpAPIClient) GetSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error) {
	var resp snapshotResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes/snapshots/"+snapshotID, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("get snapshot: %w", err)
	}
	return &resp.Snapshot, nil
}

func (c *httpAPIClient) ListSnapshots(ctx context.Context, opts ListSnapshotsOpts) ([]Snapshot, Pagination, error) {
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

	var resp listSnapshotsResponse
	if err := c.doJSON(ctx, "GET", "/v1/sandboxes/snapshots", params, nil, &resp); err != nil {
		return nil, Pagination{}, fmt.Errorf("list snapshots: %w", err)
	}
	return resp.Snapshots, resp.Pagination, nil
}

func (c *httpAPIClient) DeleteSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error) {
	var resp snapshotResponse
	if err := c.doJSON(ctx, "DELETE", "/v1/sandboxes/snapshots/"+snapshotID, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("delete snapshot: %w", err)
	}
	return &resp.Snapshot, nil
}

// Request/response types for JSON serialization.

type createSnapshotRequest struct {
	Expiration int64 `json:"expiration"`
}

type snapshotWithSandboxResponse struct {
	Snapshot Snapshot `json:"snapshot"`
	Sandbox  Sandbox  `json:"sandbox"`
}

type snapshotResponse struct {
	Snapshot Snapshot `json:"snapshot"`
}

type listSnapshotsResponse struct {
	Snapshots  []Snapshot `json:"snapshots"`
	Pagination Pagination `json:"pagination"`
}
