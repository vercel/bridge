package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const defaultBaseURL = "https://vercel.com/api"

// Client is the interface for the Vercel Sandbox API.
type Client interface {
	// Create creates a new sandbox.
	Create(ctx context.Context, opts CreateOpts) (*Sandbox, []Route, error)
	// Get returns a sandbox and its routes.
	Get(ctx context.Context, sandboxID string) (*Sandbox, []Route, error)
	// List returns sandboxes matching the given options.
	List(ctx context.Context, opts ListOpts) ([]Sandbox, Pagination, error)
	// Stop stops a running sandbox.
	Stop(ctx context.Context, sandboxID string) (*Sandbox, error)
	// ExtendTimeout extends a sandbox's timeout.
	ExtendTimeout(ctx context.Context, sandboxID string, duration time.Duration) (*Sandbox, error)
	// UpdateNetworkPolicy updates a sandbox's network policy.
	UpdateNetworkPolicy(ctx context.Context, sandboxID string, policy NetworkPolicy) (*Sandbox, error)

	// RunCommand starts a command in a sandbox and returns immediately.
	RunCommand(ctx context.Context, sandboxID string, opts RunCommandOpts) (*Command, error)
	// RunCommandAndWait starts a command and blocks until it completes.
	RunCommandAndWait(ctx context.Context, sandboxID string, opts RunCommandOpts) (*Command, error)
	// ListCommands lists all commands in a sandbox.
	ListCommands(ctx context.Context, sandboxID string) ([]Command, error)
	// GetCommand returns the current state of a command.
	GetCommand(ctx context.Context, sandboxID string, cmdID string) (*Command, error)
	// WaitCommand blocks until a command completes and returns it.
	WaitCommand(ctx context.Context, sandboxID string, cmdID string) (*Command, error)
	// KillCommand sends a signal to a running command.
	KillCommand(ctx context.Context, sandboxID string, cmdID string, signal int) (*Command, error)
	// StreamLogs returns a stream of command log output as newline-delimited JSON.
	StreamLogs(ctx context.Context, sandboxID string, cmdID string) (io.ReadCloser, error)

	// ReadFile reads a file from a sandbox.
	ReadFile(ctx context.Context, sandboxID string, path string) (io.ReadCloser, error)
	// WriteFiles writes a gzipped tar archive to a sandbox at the given directory.
	WriteFiles(ctx context.Context, sandboxID string, cwd string, archive io.Reader) error
	// CreateDirectory creates a directory (recursively) in a sandbox.
	CreateDirectory(ctx context.Context, sandboxID string, path string) error

	// CreateSnapshot creates a snapshot of a sandbox. The sandbox is stopped afterward.
	CreateSnapshot(ctx context.Context, sandboxID string, opts CreateSnapshotOpts) (*Snapshot, *Sandbox, error)
	// GetSnapshot returns a snapshot by ID.
	GetSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error)
	// ListSnapshots lists snapshots matching the given options.
	ListSnapshots(ctx context.Context, opts ListSnapshotsOpts) ([]Snapshot, Pagination, error)
	// DeleteSnapshot deletes a snapshot.
	DeleteSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error)
}

// Config configures a sandbox API client.
type Config struct {
	// Token is the Vercel API token or OIDC token.
	Token string
	// TeamID is the Vercel team ID.
	TeamID string
	// BaseURL overrides the default API base URL. Empty uses https://vercel.com/api.
	BaseURL string
	// HTTPClient overrides the default HTTP client. Nil uses a client with 30s timeout.
	HTTPClient *http.Client
}

// New returns a new sandbox API client.
func New(cfg Config) Client {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &httpAPIClient{
		token:      cfg.Token,
		teamID:     cfg.TeamID,
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// httpAPIClient implements Client using the Vercel REST API.
type httpAPIClient struct {
	token      string
	teamID     string
	baseURL    string
	httpClient *http.Client
}

// APIError represents an error response from the Vercel API.
type APIError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("sandbox api: %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("sandbox api: HTTP %d: %s", e.StatusCode, e.Message)
}

// buildURL constructs a full API URL with teamId and additional query params.
func (c *httpAPIClient) buildURL(path string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	params.Set("teamId", c.teamID)
	return c.baseURL + path + "?" + params.Encode()
}

// newRequest creates an authenticated HTTP request.
func (c *httpAPIClient) newRequest(ctx context.Context, method, fullURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// doJSON sends a request with a JSON body and decodes a JSON response into dst.
func (c *httpAPIClient) doJSON(ctx context.Context, method, path string, params url.Values, reqBody any, dst any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("sandbox api: marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := c.newRequest(ctx, method, c.buildURL(path, params), body)
	if err != nil {
		return fmt.Errorf("sandbox api: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}

	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			return fmt.Errorf("sandbox api: decode response: %w", err)
		}
	}
	return nil
}

// doStream sends a request and returns the response body without decoding.
// The caller is responsible for closing the returned ReadCloser.
func (c *httpAPIClient) doStream(ctx context.Context, method, path string, params url.Values, contentType string, body io.Reader) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, method, c.buildURL(path, params), body)
	if err != nil {
		return nil, fmt.Errorf("sandbox api: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox api: %s %s: %w", method, path, err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, c.parseError(resp)
	}

	return resp.Body, nil
}

// doNDJSON sends a request expecting an NDJSON response, reads all lines,
// and decodes the last line into dst.
func (c *httpAPIClient) doNDJSON(ctx context.Context, method, path string, params url.Values, reqBody any, dst any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("sandbox api: marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := c.newRequest(ctx, method, c.buildURL(path, params), body)
	if err != nil {
		return fmt.Errorf("sandbox api: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}

	// Read all NDJSON lines and decode the last one.
	var lastLine []byte
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 0 {
			lastLine = make([]byte, len(line))
			copy(lastLine, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sandbox api: read ndjson stream: %w", err)
	}
	if lastLine == nil {
		return fmt.Errorf("sandbox api: empty ndjson response")
	}
	if err := json.Unmarshal(lastLine, dst); err != nil {
		return fmt.Errorf("sandbox api: decode ndjson line: %w", err)
	}
	return nil
}

// parseError reads an error response body and returns an *APIError.
func (c *httpAPIClient) parseError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	// Try to decode a JSON error body.
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err == nil && errBody.Error.Message != "" {
		apiErr.Code = errBody.Error.Code
		apiErr.Message = errBody.Error.Message
	} else {
		apiErr.Message = http.StatusText(resp.StatusCode)
	}
	return apiErr
}
