package vercel

import (
	"context"
	"net/http"
	"time"
)

const defaultBaseURL = "https://api.vercel.com"

type Client interface {
	CreateDeployment(ctx context.Context, opts CreateDeploymentOpts) (*Deployment, error)
	GetDeployment(ctx context.Context, idOrURL string) (*Deployment, error)
	WaitDeployment(ctx context.Context, idOrURL string, pollInterval time.Duration) (*Deployment, error)
}

type Config struct {
	Token      string
	TeamID     string
	BaseURL    string
	HTTPClient *http.Client
}

type CreateDeploymentOpts struct {
	Name            string
	Project         string
	Target          string
	Meta            map[string]string
	Env             map[string]string
	Files           []File
	GitSource       *GitSource
	ProjectSettings *ProjectSettings
	ForceNew        bool
	SkipAutoDetect  bool
}

type File struct {
	File string
	Data []byte
}

type deploymentFile struct {
	File string `json:"file"`
	Data string `json:"data"`
}

type createDeploymentRequest struct {
	Name            string            `json:"name"`
	Project         string            `json:"project,omitempty"`
	Target          string            `json:"target,omitempty"`
	Meta            map[string]string `json:"meta,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Files           []deploymentFile  `json:"files,omitempty"`
	GitSource       *GitSource        `json:"gitSource,omitempty"`
	ProjectSettings *ProjectSettings  `json:"projectSettings,omitempty"`
}

type GitSource struct {
	Type   string `json:"type"`
	Org    string `json:"org,omitempty"`
	Repo   string `json:"repo,omitempty"`
	RepoID int64  `json:"repoId,omitempty"`
	Ref    string `json:"ref,omitempty"`
	SHA    string `json:"sha,omitempty"`
	PRID   int64  `json:"prId,omitempty"`
}

type ProjectSettings struct {
	Framework       *string `json:"framework,omitempty"`
	BuildCommand    *string `json:"buildCommand,omitempty"`
	InstallCommand  *string `json:"installCommand,omitempty"`
	OutputDirectory *string `json:"outputDirectory,omitempty"`
	RootDirectory   *string `json:"rootDirectory,omitempty"`
	NodeVersion     *string `json:"nodeVersion,omitempty"`
}

type Deployment struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	ProjectID    string            `json:"projectId"`
	URL          string            `json:"url"`
	InspectorURL string            `json:"inspectorUrl"`
	ReadyState   string            `json:"readyState"`
	Status       string            `json:"status"`
	ErrorCode    string            `json:"errorCode"`
	ErrorMessage string            `json:"errorMessage"`
	Alias        []string          `json:"alias"`
	Meta         map[string]string `json:"meta"`
	CreatedAt    int64             `json:"createdAt"`
	Ready        int64             `json:"ready"`
}

const (
	DeploymentStateQueued       = "QUEUED"
	DeploymentStateBuilding     = "BUILDING"
	DeploymentStateInitializing = "INITIALIZING"
	DeploymentStateReady        = "READY"
	DeploymentStateError        = "ERROR"
	DeploymentStateCanceled     = "CANCELED"
)
