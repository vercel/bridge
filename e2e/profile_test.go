package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/e2e/testutil"
	"github.com/vercel/bridge/pkg/commands"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/mocks/bridgev1mocks"
	"github.com/vercel/bridge/pkg/profile"
)

type ProfileSuite struct {
	suite.Suite
}

func TestProfileSuite(t *testing.T) {
	suite.Run(t, new(ProfileSuite))
}

// TestFind verifies directory-walking discovery of .bridge/profile.json.
func (s *ProfileSuite) TestFind() {
	root := s.T().TempDir()
	bridgeDir := filepath.Join(root, ".bridge")
	s.Require().NoError(os.MkdirAll(bridgeDir, 0755))
	s.Require().NoError(os.WriteFile(filepath.Join(bridgeDir, "profile.json"), []byte(`{"create":[]}`), 0644))

	child := filepath.Join(root, "services", "api", "cmd")
	s.Require().NoError(os.MkdirAll(child, 0755))

	s.Equal(filepath.Join(bridgeDir, "profile.json"), profile.Find(child))
	s.Empty(profile.Find(s.T().TempDir()))
}

// TestLoad verifies parsing of a profile.json file.
func (s *ProfileSuite) TestLoad() {
	dir := s.T().TempDir()
	bridgeDir := filepath.Join(dir, ".bridge")
	s.Require().NoError(os.MkdirAll(bridgeDir, 0755))

	profileJSON := `{
  "create": [
    {
      "match": "payload.workload_name == 'api-feature-flags'",
      "command": {
        "server_facades": ["reactors/flags.json"],
        "namespace": "production"
      }
    }
  ]
}`
	s.Require().NoError(os.WriteFile(filepath.Join(bridgeDir, "profile.json"), []byte(profileJSON), 0644))

	p, err := profile.Load(filepath.Join(bridgeDir, "profile.json"))
	s.Require().NoError(err)
	s.Len(p.Create, 1)
	s.Equal("payload.workload_name == 'api-feature-flags'", p.Create[0].Match)
	s.Equal([]string{"reactors/flags.json"}, p.Create[0].GetCommand().ServerFacades)
	s.Equal("production", p.Create[0].GetCommand().Namespace)
}

// TestCreateInjectsFacades starts a mock administrator gRPC server,
// writes a .bridge/profile.json that matches a deployment name, and runs
// `bridge create`. It verifies that the CreateBridgeRequest sent to the
// administrator contains the server facades and namespace injected by the profile.
func (s *ProfileSuite) TestCreateInjectsFacades() {
	dir := s.T().TempDir()
	origDir, err := os.Getwd()
	s.Require().NoError(err)
	s.Require().NoError(os.Chdir(dir))
	s.T().Cleanup(func() { os.Chdir(origDir) })

	_, err = identity.EnsureDeviceID()
	s.Require().NoError(err)

	// Write server facade spec file.
	facadesDir := filepath.Join(dir, "facades")
	s.Require().NoError(os.MkdirAll(facadesDir, 0755))
	facadeJSON := `{
  "host": "flags.vercel.com",
  "routes": [{
    "match": {"cel": "request.method == 'POST'"},
    "action": {"http_response": {"status": 200, "body": {"mocked": true}}}
  }]
}`
	s.Require().NoError(os.WriteFile(filepath.Join(facadesDir, "flags.json"), []byte(facadeJSON), 0644))

	// Write .bridge/profile.json.
	bridgeDir := filepath.Join(dir, ".bridge")
	s.Require().NoError(os.MkdirAll(bridgeDir, 0755))
	profileJSON := `{
  "create": [
    {
      "match": "payload.workload_name == 'api-feature-flags'",
      "command": {
        "server_facades": ["facades/flags.json"],
        "namespace": "production"
      }
    }
  ]
}`
	s.Require().NoError(os.WriteFile(filepath.Join(bridgeDir, "profile.json"), []byte(profileJSON), 0644))

	// --- Start mock administrator gRPC server ---

	mockAdmin := bridgev1mocks.NewAdministratorServiceServer(s.T())

	mockAdmin.EXPECT().
		ListBridges(mock.Anything, mock.Anything).
		Return(&bridgev1.ListBridgesResponse{}, nil)

	sentinel := fmt.Errorf("mock: stop after create")
	mockAdmin.EXPECT().
		CreateBridge(mock.Anything, mock.MatchedBy(func(req *bridgev1.CreateBridgeRequest) bool {
			s.Equal("production", req.SourceNamespace, "profile should inject namespace")
			s.Require().Len(req.ServerFacades, 1, "profile should inject one server facade")
			s.Equal("flags.vercel.com", req.ServerFacades[0].Host)
			s.Require().Len(req.ServerFacades[0].Routes, 1)
			s.Equal("request.method == 'POST'", req.ServerFacades[0].Routes[0].Match.Cel)
			return true
		})).
		Return((*bridgev1.CreateBridgeResponse)(nil), sentinel)

	srv, err := testutil.StartMockAdminServer(mockAdmin)
	s.Require().NoError(err)
	s.T().Cleanup(srv.Stop)

	// --- Run `bridge create api-feature-flags` ---

	app := commands.NewApp()
	app.Reader = strings.NewReader("")
	app.Writer = io.Discard

	_ = app.Run(context.Background(), []string{
		"bridge", "create", "api-feature-flags",
		"--yes",
		"--admin-addr", srv.Addr,
	})
}
