// Package sandbox provides a client for the Vercel Sandbox API.
package sandbox

import "time"

// Sandbox represents a Vercel Sandbox instance.
type Sandbox struct {
	ID                  string           `json:"id"`
	Memory              int              `json:"memory"`
	VCPUs               int              `json:"vcpus"`
	Region              string           `json:"region"`
	Runtime             string           `json:"runtime"`
	Timeout             int64            `json:"timeout"`
	Status              string           `json:"status"`
	CWD                 string           `json:"cwd"`
	RequestedAt         int64            `json:"requestedAt"`
	StartedAt           *int64           `json:"startedAt,omitempty"`
	RequestedStopAt     *int64           `json:"requestedStopAt,omitempty"`
	StoppedAt           *int64           `json:"stoppedAt,omitempty"`
	AbortedAt           *int64           `json:"abortedAt,omitempty"`
	Duration            *int64           `json:"duration,omitempty"`
	SourceSnapshotID    *string          `json:"sourceSnapshotId,omitempty"`
	SnapshottedAt       *int64           `json:"snapshottedAt,omitempty"`
	CreatedAt           int64            `json:"createdAt"`
	UpdatedAt           int64            `json:"updatedAt"`
	InteractivePort     *int             `json:"interactivePort,omitempty"`
	NetworkPolicy       *NetworkPolicy   `json:"networkPolicy,omitempty"`
	ActiveCPUDurationMs *int64           `json:"activeCpuDurationMs,omitempty"`
	NetworkTransfer     *NetworkTransfer `json:"networkTransfer,omitempty"`
}

// Sandbox status constants.
const (
	StatusPending      = "pending"
	StatusRunning      = "running"
	StatusStopping     = "stopping"
	StatusStopped      = "stopped"
	StatusFailed       = "failed"
	StatusAborted      = "aborted"
	StatusSnapshotting = "snapshotting"
)

// Runtime constants.
const (
	RuntimeNode24    = "node24"
	RuntimeNode22    = "node22"
	RuntimePython313 = "python3.13"
)

// NetworkTransfer contains ingress/egress byte counts.
type NetworkTransfer struct {
	Ingress int64 `json:"ingress"`
	Egress  int64 `json:"egress"`
}

// Route represents a public route to a sandbox port.
type Route struct {
	URL       string `json:"url"`
	Subdomain string `json:"subdomain"`
	Port      int    `json:"port"`
	System    *bool  `json:"system,omitempty"`
}

// Command represents a command running or completed in a sandbox.
type Command struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Args      []string `json:"args"`
	CWD       string   `json:"cwd"`
	SandboxID string   `json:"sandboxId"`
	ExitCode  *int     `json:"exitCode"`
	StartedAt int64    `json:"startedAt"`
}

// Snapshot represents a saved sandbox state.
type Snapshot struct {
	ID              string `json:"id"`
	SourceSandboxID string `json:"sourceSandboxId"`
	Region          string `json:"region"`
	Status          string `json:"status"`
	SizeBytes       int64  `json:"sizeBytes"`
	ExpiresAt       *int64 `json:"expiresAt,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

// Snapshot status constants.
const (
	SnapshotStatusCreated = "created"
	SnapshotStatusDeleted = "deleted"
	SnapshotStatusFailed  = "failed"
)

// NetworkPolicy configures sandbox network access.
type NetworkPolicy struct {
	Mode           string          `json:"mode"`
	AllowedDomains []string        `json:"allowedDomains,omitempty"`
	AllowedCIDRs   []string        `json:"allowedCIDRs,omitempty"`
	DeniedCIDRs    []string        `json:"deniedCIDRs,omitempty"`
	InjectionRules []InjectionRule `json:"injectionRules,omitempty"`
}

// Network policy mode constants.
const (
	NetworkPolicyAllowAll = "allow-all"
	NetworkPolicyDenyAll  = "deny-all"
	NetworkPolicyCustom   = "custom"
)

// InjectionRule configures header injection for requests to a domain.
type InjectionRule struct {
	Domain      string            `json:"domain"`
	Headers     map[string]string `json:"headers,omitempty"`
	HeaderNames []string          `json:"headerNames,omitempty"`
}

// Pagination contains cursor-based pagination info.
type Pagination struct {
	Count int    `json:"count"`
	Next  *int64 `json:"next"`
	Prev  *int64 `json:"prev"`
}

// LogLine represents a single line from a command log stream.
type LogLine struct {
	Stream string `json:"stream"` // "stdout", "stderr", or "error"
	Data   any    `json:"data"`   // string for stdout/stderr, LogError for error
}

// LogError represents an error in a log stream.
type LogError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Source configures where to load sandbox contents from.
type Source struct {
	Type       string `json:"type"` // "git", "tarball", or "snapshot"
	URL        string `json:"url,omitempty"`
	Depth      *int   `json:"depth,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	SnapshotID string `json:"snapshotId,omitempty"`
}

// Resources configures sandbox compute resources.
type Resources struct {
	VCPUs int `json:"vcpus"` // 1, 2, 4, or 8
}

// CreateOpts are options for creating a sandbox.
type CreateOpts struct {
	ProjectID     string
	Ports         []int
	Source        *Source
	Timeout       time.Duration // 0 uses server default (5 min)
	Resources     *Resources
	Runtime       string
	NetworkPolicy *NetworkPolicy
	Env           map[string]string
}

// ListOpts are options for listing sandboxes.
type ListOpts struct {
	ProjectID string
	Limit     int
	Since     *time.Time
	Until     *time.Time
}

// RunCommandOpts are options for running a command in a sandbox.
type RunCommandOpts struct {
	Command string
	Args    []string
	CWD     string
	Env     map[string]string
	Sudo    bool
}

// CreateSnapshotOpts are options for creating a snapshot.
type CreateSnapshotOpts struct {
	Expiration time.Duration // 0 uses server default (30 days)
}

// ListSnapshotsOpts are options for listing snapshots.
type ListSnapshotsOpts struct {
	ProjectID string
	Limit     int
	Since     *time.Time
	Until     *time.Time
}
