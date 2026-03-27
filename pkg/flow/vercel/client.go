package vercel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// New returns a new Vercel API client.
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
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}
}

type httpAPIClient struct {
	token      string
	teamID     string
	baseURL    string
	httpClient *http.Client
}

type APIError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("vercel api: %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("vercel api: HTTP %d: %s", e.StatusCode, e.Message)
}

func (c *httpAPIClient) CreateDeployment(ctx context.Context, opts CreateDeploymentOpts) (*Deployment, error) {
	body := createDeploymentRequest{
		Name:            opts.Name,
		Project:         opts.Project,
		Target:          opts.Target,
		Meta:            opts.Meta,
		Env:             opts.Env,
		GitSource:       opts.GitSource,
		ProjectSettings: opts.ProjectSettings,
	}

	body.Files = make([]deploymentFile, 0, len(opts.Files))
	for _, file := range opts.Files {
		body.Files = append(body.Files, deploymentFile{
			File: file.File,
			Data: string(file.Data),
		})
	}

	params := url.Values{}
	if opts.ForceNew {
		params.Set("forceNew", "1")
	}
	if opts.SkipAutoDetect {
		params.Set("skipAutoDetectionConfirmation", "1")
	}

	var deployment Deployment
	if err := c.doJSON(ctx, http.MethodPost, "/v13/deployments", params, body, &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

func (c *httpAPIClient) GetDeployment(ctx context.Context, idOrURL string) (*Deployment, error) {
	var deployment Deployment
	path := "/v13/deployments/" + url.PathEscape(idOrURL)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &deployment); err != nil {
		return nil, err
	}
	return &deployment, nil
}

func (c *httpAPIClient) WaitDeployment(ctx context.Context, idOrURL string, pollInterval time.Duration) (*Deployment, error) {
	if pollInterval <= 0 {
		pollInterval = time.Second
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		deployment, err := c.GetDeployment(ctx, idOrURL)
		if err != nil {
			return nil, err
		}

		switch deployment.ReadyState {
		case DeploymentStateReady, DeploymentStateError, DeploymentStateCanceled:
			return deployment, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *httpAPIClient) buildURL(path string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	if c.teamID != "" {
		params.Set("teamId", c.teamID)
	}

	if encoded := params.Encode(); encoded != "" {
		return c.baseURL + path + "?" + encoded
	}
	return c.baseURL + path
}

func (c *httpAPIClient) newRequest(ctx context.Context, method, fullURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

func (c *httpAPIClient) doJSON(ctx context.Context, method, path string, params url.Values, reqBody any, dst any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("vercel api: marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := c.newRequest(ctx, method, c.buildURL(path, params), body)
	if err != nil {
		return fmt.Errorf("vercel api: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("vercel api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}

	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			return fmt.Errorf("vercel api: decode response: %w", err)
		}
	}
	return nil
}

func (c *httpAPIClient) parseError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}

	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err == nil && errBody.Error.Message != "" {
		apiErr.Code = errBody.Error.Code
		apiErr.Message = errBody.Error.Message
		return apiErr
	}

	apiErr.Message = http.StatusText(resp.StatusCode)
	return apiErr
}
