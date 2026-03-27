package vercel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCreateDeployment(t *testing.T) {
	var gotAuth string
	var gotQuery string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v13/deployments", r.URL.Path)

		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(Deployment{
			ID:         "dpl_123",
			Name:       "dispatcher",
			ProjectID:  "prj_123",
			URL:        "dispatcher-test.vercel.app",
			ReadyState: DeploymentStateBuilding,
			Status:     DeploymentStateBuilding,
		}))
	}))
	defer server.Close()

	client := New(Config{
		Token:   "token-123",
		TeamID:  "team_123",
		BaseURL: server.URL,
	})

	buildCommand := ""
	deployment, err := client.CreateDeployment(context.Background(), CreateDeploymentOpts{
		Name:    "dispatcher",
		Project: "dispatcher",
		Target:  "preview",
		Env: map[string]string{
			"FOO": "bar",
		},
		GitSource: &GitSource{
			Type: "github",
			Org:  "vercel",
			Repo: "bridge",
			Ref:  "main",
			SHA:  "deadbeef",
		},
		Files: []File{
			{File: "index.html", Data: []byte("<h1>ok</h1>")},
		},
		ProjectSettings: &ProjectSettings{
			BuildCommand: &buildCommand,
		},
		ForceNew:       true,
		SkipAutoDetect: true,
	})
	require.NoError(t, err)
	require.Equal(t, "Bearer token-123", gotAuth)
	require.Contains(t, gotQuery, "teamId=team_123")
	require.Contains(t, gotQuery, "forceNew=1")
	require.Contains(t, gotQuery, "skipAutoDetectionConfirmation=1")
	require.Equal(t, "dispatcher", gotBody["name"])
	require.Equal(t, "dispatcher", gotBody["project"])
	require.Equal(t, map[string]any{"FOO": "bar"}, gotBody["env"])
	require.Equal(t, map[string]any{
		"type": "github",
		"org":  "vercel",
		"repo": "bridge",
		"ref":  "main",
		"sha":  "deadbeef",
	}, gotBody["gitSource"])

	files := gotBody["files"].([]any)
	require.Len(t, files, 1)
	file := files[0].(map[string]any)
	require.Equal(t, "index.html", file["file"])
	require.Equal(t, "<h1>ok</h1>", file["data"])

	require.Equal(t, "dpl_123", deployment.ID)
	require.Equal(t, DeploymentStateBuilding, deployment.ReadyState)
}

func TestWaitDeployment(t *testing.T) {
	var calls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v13/deployments/dpl_123", r.URL.Path)

		state := DeploymentStateBuilding
		if calls > 0 {
			state = DeploymentStateReady
		}
		calls++

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(Deployment{
			ID:         "dpl_123",
			ReadyState: state,
			Status:     state,
			URL:        "dispatcher-test.vercel.app",
		}))
	}))
	defer server.Close()

	client := New(Config{
		Token:   "token-123",
		TeamID:  "team_123",
		BaseURL: server.URL,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	deployment, err := client.WaitDeployment(ctx, "dpl_123", 10*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, DeploymentStateReady, deployment.ReadyState)
	require.GreaterOrEqual(t, calls, 2)
}
