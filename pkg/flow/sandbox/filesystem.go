package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

func (c *httpAPIClient) ReadFile(ctx context.Context, sandboxID string, path string) (io.ReadCloser, error) {
	body := readFileRequest{Path: path}
	rc, err := c.doStream(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/fs/read", nil, "application/json", marshalReader(body))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return rc, nil
}

func (c *httpAPIClient) WriteFiles(ctx context.Context, sandboxID string, cwd string, archive io.Reader) error {
	req, err := c.newRequest(ctx, "POST", c.buildURL("/v1/sandboxes/"+sandboxID+"/fs/write", nil), archive)
	if err != nil {
		return fmt.Errorf("write files: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	if cwd != "" {
		req.Header.Set("X-Cwd", cwd)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("write files: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}
	return nil
}

func (c *httpAPIClient) CreateDirectory(ctx context.Context, sandboxID string, path string) error {
	body := mkdirRequest{Path: path, Recursive: true}
	if err := c.doJSON(ctx, "POST", "/v1/sandboxes/"+sandboxID+"/fs/mkdir", nil, body, nil); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	return nil
}

// marshalReader is a helper that JSON-encodes v into a bytes.Reader.
func marshalReader(v any) io.Reader {
	data, _ := json.Marshal(v)
	return bytes.NewReader(data)
}

// Request types for JSON serialization.

type readFileRequest struct {
	Path string `json:"path"`
}

type mkdirRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}
